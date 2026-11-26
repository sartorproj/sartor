#!/usr/bin/env python3
"""Scan a git range for leaked internal terms, private IPs and private hostnames.

Scope of one run:
  * lines added in the diff BASE...HEAD (text files) and the full content of
    added/modified binary files,
  * new file paths (added, renamed, copied),
  * every commit message (subject, body, trailers) and author/committer
    identity in BASE..HEAD,
  * optionally the PR title, PR body and branch name.

Checks:
  * denylist: newline-separated fixed strings, matched case-insensitively,
    loaded from .leakcheck-denylist (or $LEAKCHECK_DENYLIST). Matched text is
    never printed; findings name the entry number only.
  * generic (always on): IPv4 addresses in 10/8, 172.16/12, 192.168/16 and
    100.64/10, and hostnames under ts[.]net.

Exit status: 0 clean, 1 findings, 2 usage or git error.
See docs/leakcheck.md for what is and is not covered.
"""

import argparse
import base64
import binascii
import fnmatch
import html
import os
import re
import subprocess
import sys
import unicodedata
import urllib.parse

EMPTY_TREE = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
DENYLIST_FILE = ".leakcheck-denylist"
ALLOWLIST_FILE = ".leakcheck-allow"
MAX_BLOB_BYTES = 20 * 1024 * 1024
MAX_VARIANTS = 64
# Entries shorter than this are only matched literally, not with separators
# stripped, to keep the squashed match from firing across unrelated words.
SQUASH_MIN_LEN = 8

IPV4_RE = re.compile(
    r"(?<![0-9.])([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})(?![0-9]|\.[0-9])"
)
PRIVATE_HOST_RE = re.compile(r"(?<![a-z0-9._-])((?:[a-z0-9_-]+\.)+ts\.net)(?!\.?[a-z0-9_-])")
BASE64_RE = re.compile(r"[A-Za-z0-9+/_-]{16,}={0,2}")
HEX_RE = re.compile(r"(?:0x)?((?:[0-9a-fA-F]{2}){8,})")
SEP_HEX_RE = re.compile(r"(?:[0-9a-fA-F]{2}[:\s-]){7,}[0-9a-fA-F]{2}")
ESCAPE_RE = re.compile(r"\\(?:x([0-9a-fA-F]{2})|u([0-9a-fA-F]{4})|U([0-9a-fA-F]{8})|([0-7]{1,3}))")
SQUASH_RE = re.compile(r"[\W_]+")


# --------------------------------------------------------------------------
# Normalisation and decoding


def strip_format_chars(s):
    """Drop zero-width and other Unicode format (Cf) characters."""
    if s.isascii():
        return s
    return "".join(ch for ch in s if unicodedata.category(ch) != "Cf")


def normalize(s):
    return unicodedata.normalize("NFKC", strip_format_chars(s))


def squash(s):
    return SQUASH_RE.sub("", s)


def bytes_to_text(b):
    out = [b.decode("utf-8", errors="replace")]
    if b"\x00" in b:
        out.append(b.replace(b"\x00", b"").decode("utf-8", errors="replace"))
    return out


def _decode_base64_token(token):
    s = token.rstrip("=").replace("-", "+").replace("_", "/")
    results = []
    # Try each alignment so a base64 run glued to a short identifier prefix
    # still decodes.
    for k in range(4):
        t = s[k:]
        if len(t) % 4 == 1:
            t = t[:-1]
        if len(t) < 12:
            break
        try:
            results.extend(bytes_to_text(base64.b64decode(t + "=" * (-len(t) % 4), validate=True)))
        except (binascii.Error, ValueError):
            continue
    return results


def _unescape(s):
    def repl(m):
        hexval = m.group(1) or m.group(2) or m.group(3)
        try:
            cp = int(hexval, 16) if hexval else int(m.group(4), 8)
            return chr(cp)
        except (ValueError, OverflowError):
            return m.group(0)

    return ESCAPE_RE.sub(repl, s)


def _decoders(s):
    """Yield (label, decoded) pairs for every encoding detected in s."""
    if "%" in s:
        dec = urllib.parse.unquote(s, errors="replace")
        if dec != s:
            yield "url-decoded", dec
    if "&" in s:
        dec = html.unescape(s)
        if dec != s:
            yield "html-unescaped", dec
    if "\\" in s:
        dec = _unescape(s)
        if dec != s:
            yield "unescaped", dec
    for m in HEX_RE.finditer(s):
        for dec in bytes_to_text(bytes.fromhex(m.group(1))):
            yield "hex-decoded", dec
    for m in SEP_HEX_RE.finditer(s):
        raw = re.sub(r"[:\s-]", "", m.group(0))
        for dec in bytes_to_text(bytes.fromhex(raw)):
            yield "hex-decoded", dec
    for m in BASE64_RE.finditer(s):
        for dec in _decode_base64_token(m.group(0)):
            yield "base64-decoded", dec


def variants(text):
    """Return [(label, text)] for the raw, normalised and decoded forms."""
    seen = {text}
    out = [("raw", text)]
    norm = normalize(text)
    if norm not in seen:
        seen.add(norm)
        out.append(("normalized", norm))
    frontier = list(out)
    for _ in range(2):
        nxt = []
        for label, v in frontier:
            for dlabel, dec in _decoders(v):
                dec = normalize(dec)
                if dec in seen or len(out) >= MAX_VARIANTS:
                    continue
                seen.add(dec)
                lab = dlabel if label in ("raw", "normalized") else label + "+" + dlabel
                out.append((lab, dec))
                nxt.append((lab, dec))
        frontier = nxt
    return out


# --------------------------------------------------------------------------
# Matchers


def is_private_ipv4(octets):
    a, b, _, _ = octets
    return a == 10 or (a == 172 and 16 <= b <= 31) or (a == 192 and b == 168) or (a == 100 and 64 <= b <= 127)


def find_private_ipv4(text):
    hits = []
    for m in IPV4_RE.finditer(text):
        octets = [int(g) for g in m.groups()]
        if all(o <= 255 for o in octets) and is_private_ipv4(octets):
            hits.append(".".join(str(o) for o in octets))
    return hits


def find_private_hosts(text):
    return [m.group(1) for m in PRIVATE_HOST_RE.finditer(text.casefold())]


class Denylist:
    def __init__(self, entries):
        self.raw = []
        self.terms = []
        for line in entries:
            e = line.strip()
            if not e or e.startswith("#"):
                continue
            self.raw.append(e)
            t = normalize(e).casefold()
            self.terms.append((t, squash(t) if len(squash(t)) >= SQUASH_MIN_LEN else None))

    def __len__(self):
        return len(self.terms)

    def match(self, text):
        """Return {1-based entry number: variant label} for entries found in text."""
        return self.match_variants(variants(text)) if self.terms else {}

    def match_variants(self, vs):
        found = {}
        if not self.terms:
            return found
        for label, v in vs:
            vf = v.casefold()
            vs = None
            for i, (term, sq) in enumerate(self.terms, 1):
                if i in found:
                    continue
                if term in vf:
                    found[i] = label
                elif sq:
                    if vs is None:
                        vs = squash(vf)
                    if sq in vs:
                        found[i] = label + "+separators-ignored"
        return found


class Allowlist:
    """Committed exceptions for generic findings. Never applies to the denylist."""

    def __init__(self, lines):
        self.paths = [ALLOWLIST_FILE]
        self.matches = set()
        for n, line in enumerate(lines, 1):
            line = line.strip()
            if not line or line.startswith("#"):
                continue
            kind, sep, value = line.partition(":")
            value = value.strip()
            if not sep or not value or kind not in ("path", "match"):
                raise ValueError("%s:%d: expected 'path:<glob>' or 'match:<string>'" % (ALLOWLIST_FILE, n))
            if kind == "path":
                self.paths.append(value)
            else:
                self.matches.add(value.casefold().rstrip("."))

    def allows(self, path, value):
        if value.casefold().rstrip(".") in self.matches:
            return True
        return path is not None and any(fnmatch.fnmatchcase(path, g) for g in self.paths)


# --------------------------------------------------------------------------
# Findings and output


def _esc_data(s):
    return s.replace("%", "%25").replace("\r", "%0D").replace("\n", "%0A")


def _esc_prop(s):
    return _esc_data(s).replace(":", "%3A").replace(",", "%2C")


class Reporter:
    def __init__(self, denylist, allowlist, actions):
        self.denylist = denylist
        self.allowlist = allowlist
        self.actions = actions
        self.findings = []
        self._seen = set()
        self._redacted = {}

    def display_path(self, path):
        if path is None:
            return None
        if self.denylist.match(path):
            if path not in self._redacted:
                self._redacted[path] = "<redacted path #%d>" % (len(self._redacted) + 1)
            return self._redacted[path]
        return path

    def add(self, where, path, line, kind, message, key=None):
        key = (where, path, line, kind, key or message)
        if key in self._seen:
            return
        self._seen.add(key)
        self.findings.append((where, self.display_path(path), line, message))

    def scan(self, where, text, path=None, line=None, generic=True):
        vs = variants(text)
        for entry, label in sorted(self.denylist.match_variants(vs).items()):
            self.add(where, path, line, "deny", "denylist entry %d (%s)" % (entry, label), key=entry)
        if not generic:
            return
        for label, v in vs:
            suffix = "" if label in ("raw", "normalized") else " (%s)" % label
            for ip in find_private_ipv4(v):
                if not self.allowlist.allows(path, ip):
                    self.add(where, path, line, "ip", "private/CGNAT IPv4 %s%s" % (ip, suffix))
            for host in find_private_hosts(v):
                if not self.allowlist.allows(path, host):
                    self.add(where, path, line, "ts", "private hostname %s%s" % (host, suffix))

    def scan_joined(self, where, first, second, path=None, line=None):
        """Catch a denylist term split across two adjacent lines."""
        single = set(self.denylist.match(first)) | set(self.denylist.match(second))
        for entry, label in sorted(self.denylist.match(first.rstrip() + second.lstrip()).items()):
            if entry not in single:
                self.add(where, path, line, "deny", "denylist entry %d (%s, split across lines)" % (entry, label), key=entry)

    def emit(self):
        for where, path, line, message in self.findings:
            loc = where
            if path is not None:
                loc = "%s %s" % (where, path) + (":%d" % line if line else "")
            text = "leakcheck: %s: %s" % (loc, message)
            if self.actions:
                props = ""
                if path is not None and not path.startswith("<redacted"):
                    props = " file=%s" % _esc_prop(path) + (",line=%d" % line if line else "")
                print("::error%s::%s" % (props, _esc_data(text)))
            else:
                print(text)


# --------------------------------------------------------------------------
# Git plumbing


class GitError(Exception):
    pass


def git(*args, check=True):
    cmd = ["git", "-c", "core.quotepath=off", "-c", "color.ui=never"] + list(args)
    p = subprocess.run(cmd, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    if check and p.returncode != 0:
        raise GitError("%s: %s" % (" ".join(args[:3]), p.stderr.decode(errors="replace").strip()))
    return p.stdout


def is_commit(ref):
    if not ref or set(ref) == {"0"}:
        return False
    return subprocess.run(
        ["git", "cat-file", "-e", ref + "^{commit}"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL
    ).returncode == 0


def c_unquote(s):
    """Undo git's C-style quoting of a path ("a\\tb" -> a<TAB>b)."""
    if not (len(s) >= 2 and s[0] == '"' and s[-1] == '"'):
        return s
    body = s[1:-1].encode("utf-8")
    out = bytearray()
    i = 0
    simple = {ord("n"): 10, ord("t"): 9, ord('"'): 34, ord("\\"): 92, ord("a"): 7, ord("b"): 8, ord("f"): 12, ord("r"): 13, ord("v"): 11}
    while i < len(body):
        c = body[i]
        if c == 92 and i + 1 < len(body):
            n = body[i + 1]
            if n in simple:
                out.append(simple[n])
                i += 2
                continue
            if re.fullmatch(rb"[0-7]{3}", body[i + 1 : i + 4]):
                out.append(int(body[i + 1 : i + 4], 8))
                i += 4
                continue
        out.append(c)
        i += 1
    return out.decode("utf-8", errors="replace")


def changed_files(diff_args):
    """Return (new_paths, binary_paths, deny_file_committed)."""
    raw = git("diff", "--name-status", "-z", "-M", "--no-ext-diff", *diff_args).split(b"\0")
    new_paths, live = [], set()
    i = 0
    while i < len(raw) and raw[i]:
        status = raw[i].decode()
        if status[0] in "RC":
            path = raw[i + 2].decode("utf-8", errors="replace")
            i += 3
        else:
            path = raw[i + 1].decode("utf-8", errors="replace")
            i += 2
        if status[0] != "D":
            live.add(path)
        if status[0] in "ARC":
            new_paths.append(path)

    binary = []
    raw = git("diff", "--numstat", "-z", "-M", "--no-ext-diff", *diff_args).split(b"\0")
    i = 0
    while i < len(raw) and raw[i]:
        added, deleted, path = raw[i].split(b"\t", 2)
        if path == b"":
            path = raw[i + 2]
            i += 3
        else:
            i += 1
        p = path.decode("utf-8", errors="replace")
        if added == b"-" and deleted == b"-" and p in live:
            binary.append(p)
    return new_paths, binary, any(os.path.basename(p) == DENYLIST_FILE for p in live)


HUNK_RE = re.compile(r"^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@")


def added_lines(diff_args):
    """Yield (path, line_number, text) for every added line in text files."""
    out = git(
        "diff", "--no-color", "--no-ext-diff", "--no-textconv", "-U0", "-M",
        "--src-prefix=a/", "--dst-prefix=b/", *diff_args,
    )
    path, lineno, in_hunk = None, 0, False
    for raw in out.split(b"\n"):
        line = raw.decode("utf-8", errors="replace")
        if line.startswith("diff --git "):
            path, in_hunk = None, False
            continue
        if not in_hunk:
            if line.startswith("+++ "):
                target = c_unquote(line[4:])
                path = target[2:] if target.startswith("b/") else None
            elif line.startswith("@@"):
                m = HUNK_RE.match(line)
                if m:
                    lineno, in_hunk = int(m.group(1)), True
            continue
        if line.startswith("@@"):
            m = HUNK_RE.match(line)
            if m:
                lineno = int(m.group(1))
            continue
        if line.startswith("+"):
            if path is not None:
                yield path, lineno, line[1:]
            lineno += 1
        elif line.startswith(" "):
            lineno += 1


def commits(log_args):
    out = git("log", "-z", "--format=%H %P%n%an <%ae>%n%cn <%ce>%n%B", *log_args)
    for chunk in out.split(b"\0"):
        if not chunk.strip():
            continue
        text = chunk.decode("utf-8", errors="replace")
        ids, author, committer, message = (text.split("\n", 3) + ["", "", ""])[:4]
        sha, *parents = ids.split()
        yield sha, author, committer, message, parents


# --------------------------------------------------------------------------
# Main


def load_denylist(path):
    if path and os.path.isfile(path):
        with open(path, encoding="utf-8", errors="replace") as f:
            return Denylist(f.read().splitlines()), path
    env = os.environ.get("LEAKCHECK_DENYLIST", "")
    if env.strip():
        return Denylist(env.splitlines()), "$LEAKCHECK_DENYLIST"
    return Denylist([]), None


def register_masks(denylist):
    for e in denylist.raw:
        for form in {e, e.lower(), e.upper()}:
            print("::add-mask::%s" % _esc_data(form))


def warn(msg, actions):
    if actions:
        print("::warning::%s" % _esc_data(msg))
    else:
        print("leakcheck: warning: %s" % msg, file=sys.stderr)


def parse_args(argv):
    env = os.environ.get
    p = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    p.add_argument("--base", default=env("LEAKCHECK_BASE") or "origin/main", help="base commit (default: origin/main)")
    p.add_argument("--head", default=env("LEAKCHECK_HEAD") or "HEAD", help="head commit (default: HEAD)")
    p.add_argument("--denylist", help="denylist file (default: <repo>/%s, then $LEAKCHECK_DENYLIST)" % DENYLIST_FILE)
    p.add_argument("--allowlist", help="allowlist file (default: <repo>/%s)" % ALLOWLIST_FILE)
    p.add_argument("--pr-title", default=env("LEAKCHECK_PR_TITLE"))
    p.add_argument("--pr-body-file", help="file containing the PR body (default: $LEAKCHECK_PR_BODY)")
    p.add_argument("--branch", default=env("LEAKCHECK_BRANCH"), help="branch name to scan")
    p.add_argument("--register-masks", action="store_true", help="only emit ::add-mask:: for each denylist entry")
    return p.parse_args(argv)


def run(argv=None):
    args = parse_args(argv)
    actions = os.environ.get("GITHUB_ACTIONS") == "true"
    try:
        root = git("rev-parse", "--show-toplevel").decode().strip()
    except GitError as e:
        print("leakcheck: %s" % e, file=sys.stderr)
        return 2

    denylist, deny_src = load_denylist(args.denylist or os.path.join(root, DENYLIST_FILE))
    if actions or args.register_masks:
        register_masks(denylist)
    if args.register_masks:
        return 0
    if deny_src is None:
        warn("no denylist configured (LEAKCHECK_DENYLIST unset or unavailable, e.g. on fork PRs); running generic checks only", actions)

    allow_path = args.allowlist or os.path.join(root, ALLOWLIST_FILE)
    try:
        if os.path.isfile(allow_path):
            with open(allow_path, encoding="utf-8") as f:
                allowlist = Allowlist(f.read().splitlines())
        else:
            allowlist = Allowlist([])
    except ValueError as e:
        print("leakcheck: %s" % e, file=sys.stderr)
        return 2

    if not is_commit(args.head):
        print("leakcheck: head %r is not a commit" % args.head, file=sys.stderr)
        return 2
    head = git("rev-parse", args.head + "^{commit}").decode().strip()
    if is_commit(args.base):
        base = git("rev-parse", args.base + "^{commit}").decode().strip()
        diff_args, log_args = ["%s...%s" % (base, head)], ["%s..%s" % (base, head)]
    else:
        # New-branch push or a before-SHA that is no longer reachable.
        warn("base %r is not a known commit; scanning only %s" % (args.base, head[:12]), actions)
        parent = head + "^" if is_commit(head + "^") else EMPTY_TREE
        diff_args, log_args = [parent, head], ["-n", "1", head]

    rep = Reporter(denylist, allowlist, actions)
    try:
        new_paths, binary, deny_committed = changed_files(diff_args)
        if deny_committed:
            rep.add("changed paths", None, None, "deny", "%s must never be committed" % DENYLIST_FILE)
        for path in new_paths:
            rep.scan("new path", path, path=path)

        n_lines = 0
        prev = None
        for path, lineno, text in added_lines(diff_args):
            n_lines += 1
            rep.scan("added line", text, path=path, line=lineno)
            if prev and prev[0] == path and prev[1] == lineno - 1:
                rep.scan_joined("added line", prev[2], text, path=path, line=prev[1])
            prev = (path, lineno, text)

        for path in binary:
            size = int(git("cat-file", "-s", "%s:%s" % (head, path)).decode().strip() or 0)
            if size > MAX_BLOB_BYTES:
                warn("binary file %s is larger than %d bytes; not scanned" % (rep.display_path(path), MAX_BLOB_BYTES), actions)
                continue
            blob = git("cat-file", "blob", "%s:%s" % (head, path))
            for text in [blob.decode("latin-1")] + bytes_to_text(blob)[1:]:
                rep.scan("binary file", text, path=path)

        final = {(f[1], f[3]) for f in rep.findings}
        n_commits = 0
        for sha, author, committer, message, parents in commits(log_args):
            n_commits += 1
            where = "commit %s" % sha[:12]
            rep.scan(where + " message", message)
            rep.scan(where + " message", message.replace("\n", ""), generic=False)
            rep.scan(where + " author/committer", author + "\n" + committer)
            # Lines added and later removed inside the range stay public in the
            # PR's commit list even though the final diff no longer shows them.
            if len(parents) == 1 and log_args[0] != "-n":
                sub = Reporter(denylist, allowlist, actions)
                for path, lineno, text in added_lines([parents[0], sha]):
                    sub.scan(where + " added line", text, path=path, line=lineno)
                for f in sub.findings:
                    if (f[1], f[3]) not in final:
                        rep.findings.append(f)
    except GitError as e:
        print("leakcheck: git error: %s" % e, file=sys.stderr)
        return 2

    body = None
    if args.pr_body_file:
        with open(args.pr_body_file, encoding="utf-8", errors="replace") as f:
            body = f.read()
    elif os.environ.get("LEAKCHECK_PR_BODY"):
        body = os.environ["LEAKCHECK_PR_BODY"]
    if args.pr_title:
        rep.scan("PR title", args.pr_title)
    if body:
        rep.scan("PR body", body)
        rep.scan("PR body", body.replace("\r", "").replace("\n", ""), generic=False)
    if args.branch:
        rep.scan("branch name", args.branch)

    rep.emit()
    summary = "leakcheck: scanned %d added lines, %d new paths, %d binary files, %d commits%s%s; denylist: %s" % (
        n_lines, len(new_paths), len(binary), n_commits,
        ", PR title" if args.pr_title else "", ", PR body" if body else "",
        "%d entries" % len(denylist) if deny_src else "not configured",
    )
    print(summary)
    step_summary = os.environ.get("GITHUB_STEP_SUMMARY")
    if actions and step_summary:
        with open(step_summary, "a", encoding="utf-8") as f:
            f.write("### Leak check\n\n%s\n\n%s\n" % (
                summary.replace("leakcheck: ", ""),
                "%d finding(s); see the job log." % len(rep.findings) if rep.findings else "No findings.",
            ))
    if rep.findings:
        print("leakcheck: FAILED with %d finding(s)" % len(rep.findings))
        return 1
    print("leakcheck: clean")
    return 0


if __name__ == "__main__":
    sys.exit(run())
