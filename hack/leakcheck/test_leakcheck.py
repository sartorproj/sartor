#!/usr/bin/env python3
"""Tests for leakcheck.py. Only synthetic terms are used.

Run: python3 -m unittest discover -s hack/leakcheck -v
Print the bypass matrix as Markdown: python3 hack/leakcheck/test_leakcheck.py --table
"""

import base64
import gzip
import importlib.util
import os
import subprocess
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
SCRIPT = os.path.join(HERE, "leakcheck.py")
_spec = importlib.util.spec_from_file_location("leakcheck", SCRIPT)
lc = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(lc)

TERM = "secret-cluster-xyz"
SHORT = "zork1"
DOMAIN = ".".join(["ts", "net"])  # spelled out at runtime only
DENYLIST = "# synthetic test entries\n%s\n%s\n" % (TERM, SHORT)


def b64(s):
    return base64.b64encode(s.encode()).decode()


class TestPrivateIPv4(unittest.TestCase):
    CASES = [
        ("10.0.0.1", ["10.0.0.1"]),
        ("10.255.255.255", ["10.255.255.255"]),
        ("9.255.255.255", []),
        ("11.0.0.0", []),
        ("172.15.255.255", []),
        ("172.16.0.0", ["172.16.0.0"]),
        ("172.31.255.254", ["172.31.255.254"]),
        ("172.32.0.1", []),
        ("192.168.1.1", ["192.168.1.1"]),
        ("192.169.0.1", []),
        ("100.63.255.255", []),
        ("100.64.0.1", ["100.64.0.1"]),
        ("100.127.255.255", ["100.127.255.255"]),
        ("100.128.0.1", []),
        ("10.0.0.256", []),
        ("10.300.0.1", []),
        ("subnet 10.20.0.0/16", ["10.20.0.0"]),
        ("http://192.168.0.10:8080/x", ["192.168.0.10"]),
        ("::ffff:192.168.1.1", ["192.168.1.1"]),
        ("8.8.8.8", []),
        ("version 1.10.2.3", []),
        ("v1.10.2.3", []),
        ("10.1.2.3.4", []),
        ("1.10.0.0.1", []),
        ("go 1.24.0", []),
        ("a=10.0.0.1,b=172.20.1.1", ["10.0.0.1", "172.20.1.1"]),
    ]

    def test_table(self):
        for text, want in self.CASES:
            with self.subTest(text=text):
                self.assertEqual(lc.find_private_ipv4(text), want)


class TestPrivateHosts(unittest.TestCase):
    CASES = [
        ("foo." + DOMAIN, ["foo." + DOMAIN]),
        ("FOO." + DOMAIN.upper(), ["foo." + DOMAIN]),
        ("ssh foo.%s." % DOMAIN, ["foo." + DOMAIN]),
        ("https://box.example1234.%s/x" % DOMAIN, ["box.example1234." + DOMAIN]),
        ("foo.%swork" % DOMAIN, []),
        ("foo.%s.example.com" % DOMAIN, []),
        (DOMAIN, []),
        ("not" + DOMAIN, []),
    ]

    def test_table(self):
        for text, want in self.CASES:
            with self.subTest(text=text):
                self.assertEqual(lc.find_private_hosts(text), want)


class TestDenylistMatch(unittest.TestCase):
    CASES = [
        ("plain", "x %s y" % TERM, True),
        ("upper", TERM.upper(), True),
        ("mixed case", "Secret-CLUSTER-xyz", True),
        ("base64", b64(TERM), True),
        ("base64 urlsafe", base64.urlsafe_b64encode(("??" + TERM).encode()).decode(), True),
        ("base64 glued to prefix", "tok" + b64("hello " + TERM), True),
        ("double base64", b64(b64(TERM)), True),
        ("url-encoded", "secret%2Dcluster%2Dxyz", True),
        ("url-encoded all", "".join("%%%02X" % ord(c) for c in TERM), True),
        ("hex", TERM.encode().hex(), True),
        ("hex 0x", "0x" + TERM.encode().hex().upper(), True),
        ("hex separated", ":".join("%02x" % ord(c) for c in TERM), True),
        ("backslash-x", "".join("\\x%02x" % ord(c) for c in TERM), True),
        ("unicode escape", "".join("\\u%04x" % ord(c) for c in TERM), True),
        ("html entities", "&#115;ecret-cluster-&#x78;yz", True),
        ("zero-width", "secret\u200b-clus\u200dter-\ufeffxyz", True),
        ("soft hyphen", "secret-clus\u00adter-xyz", True),
        ("fullwidth", "ｓｅｃｒｅｔ-ｃｌｕｓｔｅｒ-ｘｙｚ", True),
        ("separators changed", "secret_cluster.xyz", True),
        ("spaces", "secret cluster xyz", True),
        ("partial", "secret-cluster", False),
        ("rot13", "frperg-pyhfgre-klm", False),
        ("cyrillic e", "s\u0435cret-cluster-xyz", False),
        ("short base64", b64(SHORT), False),
        ("short with separators", "zor k1", False),
    ]

    def test_table(self):
        d = lc.Denylist(DENYLIST.splitlines())
        self.assertEqual(len(d), 2)
        for name, text, want in self.CASES:
            with self.subTest(name=name):
                self.assertEqual(bool(d.match(text)), want, d.match(text))

    def test_empty(self):
        self.assertEqual(lc.Denylist(["", "  ", "# c"]).match(TERM), {})


class TestAllowlist(unittest.TestCase):
    def test_parse_and_allow(self):
        a = lc.Allowlist(["# c", "path:docs/*.md", "match:Foo.%s." % DOMAIN.upper()])
        self.assertTrue(a.allows("docs/x.md", "10.0.0.1"))
        self.assertTrue(a.allows(".leakcheck-allow", "10.0.0.1"))
        self.assertTrue(a.allows(None, "foo." + DOMAIN))
        self.assertFalse(a.allows("src/x.md", "10.0.0.1"))
        self.assertFalse(a.allows(None, "10.0.0.1"))

    def test_invalid(self):
        for line in ["docs/*.md", "path:", "glob:x"]:
            with self.subTest(line=line):
                with self.assertRaises(ValueError):
                    lc.Allowlist([line])


class TestUnquote(unittest.TestCase):
    CASES = [
        ("plain", "plain"),
        ('"a\\tb"', "a\tb"),
        ('"q\\"x"', 'q"x'),
        ('"\\303\\251t\\303\\251"', "été"),
    ]

    def test_table(self):
        for raw, want in self.CASES:
            with self.subTest(raw=raw):
                self.assertEqual(lc.c_unquote(raw), want)


# --------------------------------------------------------------------------
# End-to-end runs against throwaway repositories.


class Repo:
    def __init__(self, path):
        self.path = path
        self.env = dict(
            os.environ,
            GIT_CONFIG_GLOBAL=os.devnull,
            GIT_CONFIG_NOSYSTEM="1",
            GIT_AUTHOR_NAME="Test",
            GIT_AUTHOR_EMAIL="test@example.com",
            GIT_COMMITTER_NAME="Test",
            GIT_COMMITTER_EMAIL="test@example.com",
        )

    def git(self, *args, env=None):
        return subprocess.run(
            ["git"] + list(args), cwd=self.path, env=env or self.env, check=True,
            stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        ).stdout.decode()

    def write(self, rel, content):
        full = os.path.join(self.path, rel)
        os.makedirs(os.path.dirname(full), exist_ok=True)
        with open(full, "wb") as f:
            f.write(content.encode() if isinstance(content, str) else content)
        self.git("add", "--", rel)

    def commit(self, msg="change", author_email=None):
        env = dict(self.env)
        if author_email:
            env["GIT_AUTHOR_EMAIL"] = author_email
        self.git("commit", "-q", "--allow-empty", "-m", msg, env=env)


def make_repo(tmp):
    r = Repo(tmp)
    r.git("init", "-q", "-b", "main")
    r.write("README.txt", "hello\n")
    r.write("docs/plain.txt", "plain\n")
    r.write("legacy.txt", "keep\nold %s line\nkeep\n" % TERM)
    r.commit("base")
    r.git("checkout", "-q", "-b", "topic")
    return r


def run_scan(repo, args=(), denylist=DENYLIST, actions=False, extra_env=None):
    env = {k: v for k, v in repo.env.items() if not k.startswith(("LEAKCHECK_", "GITHUB_"))}
    if actions:
        env["GITHUB_ACTIONS"] = "true"
    env.update(extra_env or {})
    cmd = [sys.executable, SCRIPT, "--base", "main", "--head", "HEAD"] + list(args)
    if denylist is not None:
        dfile = os.path.join(repo.path, ".git", "denylist-for-test")
        with open(dfile, "w") as f:
            f.write(denylist)
        cmd += ["--denylist", dfile]
    p = subprocess.run(cmd, cwd=repo.path, env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    return p.returncode, p.stdout.decode()


def _added(path, content):
    def f(r):
        r.write(path, content)
        r.commit()
    return f


def _commit_msg(msg, **kw):
    def f(r):
        r.write("README.txt", "hello\nmore\n")
        r.commit(msg, **kw)
    return f


def _rename(new):
    def f(r):
        r.git("mv", "docs/plain.txt", new)
        r.commit()
    return f


def _deleted_line(r):
    r.write("legacy.txt", "keep\nkeep\n")
    r.commit()


def _added_then_removed(r):
    r.write("notes.txt", "x %s\n" % TERM)
    r.commit("wip")
    r.write("notes.txt", "x\n")
    r.commit("cleanup")


def _noop(r):
    r.write("README.txt", "hello\nworld\n")
    r.commit("docs: clean change")


def _with_allowlist(allow, path, content):
    def f(r):
        r.write(".leakcheck-allow", allow)
        r.write(path, content)
        r.commit()
    return f


IP = "192.168.10.20"
# (case, setup, extra CLI args, expected: True = check fails / leak caught)
BYPASS_CASES = [
    ("control: clean change", _noop, [], False),
    ("term in added line", _added("a.txt", "x %s\n" % TERM), [], True),
    ("different case", _added("a.txt", "x SECRET-Cluster-XYZ\n"), [], True),
    ("commit subject", _commit_msg("fix %s" % TERM), [], True),
    ("commit body", _commit_msg("fix\n\nmore about %s\n" % TERM), [], True),
    ("commit trailer", _commit_msg("fix\n\nReviewed-by: A <%s@example.com>\n" % TERM), [], True),
    ("author email", _commit_msg("fix", author_email="%s@example.com" % TERM), [], True),
    ("PR title", _noop, ["--pr-title", "feat: %s" % TERM.upper()], True),
    ("PR body", _noop, ["--pr-body-file", "@body:see %s" % TERM], True),
    ("branch name", _noop, ["--branch", "feat/%s" % TERM], True),
    ("base64-encoded", _added("a.txt", "k: %s\n" % b64(TERM)), [], True),
    ("base64 in commit message", _commit_msg("fix %s" % b64(TERM)), [], True),
    ("double base64", _added("a.txt", b64(b64(TERM)) + "\n"), [], True),
    ("short term base64 (<16 chars)", _added("a.txt", b64(SHORT) + "\n"), [], False),
    ("split across 2 lines", _added("a.txt", "secret-clus\nter-xyz\n"), [], True),
    ("split across 3 lines", _added("a.txt", "secret-\ncluster\n-xyz\n"), [], False),
    ("split across lines in commit msg", _commit_msg("fix\n\nsecret-clus\nter-xyz\n"), [], True),
    ("split across lines in PR body", _noop, ["--pr-body-file", "@body:a secret-clus\nter-xyz b"], True),
    ("renamed file path", _rename("docs/%s.txt" % TERM), [], True),
    ("new path, upper case", _added("%s/f.txt" % TERM.upper(), "x\n"), [], True),
    ("URL-encoded", _added("a.txt", "".join("%%%02x" % ord(c) for c in TERM) + "\n"), [], True),
    ("hex", _added("a.txt", TERM.encode().hex() + "\n"), [], True),
    ("backslash-escaped", _added("a.txt", "".join("\\x%02x" % ord(c) for c in TERM) + "\n"), [], True),
    ("zero-width characters", _added("a.txt", "secret\u200b-cluster\u200c-xyz\n"), [], True),
    ("separators changed", _added("a.txt", "secret_cluster.xyz\n"), [], True),
    ("binary file", _added("img.bin", b"\x89PNG\x00\x01" + TERM.encode() + b"\x00\xff"), [], True),
    ("binary file, UTF-16", _added("img.bin", b"\xff\xfe" + TERM.encode("utf-16-le")), [], True),
    ("binary file, gzip-compressed", _added("blob.gz", gzip.compress(TERM.encode() * 3)), [], False),
    ("deleted line (by design)", _deleted_line, [], False),
    ("added then removed in a later commit", _added_then_removed, [], True),
    ("rot13", _added("a.txt", "frperg-pyhfgre-klm\n"), [], False),
    ("cyrillic homoglyph", _added("a.txt", "s\u0435cret-cluster-xyz\n"), [], False),
    ("IP 100.64.x", _added("a.txt", "addr 100.64.12.7\n"), [], True),
    ("IP 172.31.x", _added("a.txt", "addr 172.31.255.254\n"), [], True),
    ("IP 192.168.x", _added("a.txt", "addr %s\n" % IP), [], True),
    ("IP inside CIDR", _added("a.txt", "cidr: 10.20.0.0/16\n"), [], True),
    ("IP in commit message", _commit_msg("route via %s" % IP), [], True),
    ("IP in PR body", _noop, ["--pr-body-file", "@body:host %s" % IP], True),
    ("IP in new file path", _added("hosts/%s.yaml" % IP, "x\n"), [], True),
    ("IP base64-encoded", _added("a.txt", b64("host=%s" % IP) + "\n"), [], True),
    ("IP as decimal integer", _added("a.txt", "3232238100\n"), [], False),
    ("IP as hex integer", _added("a.txt", "0xC0A80A14\n"), [], False),
    ("private host uppercase", _added("a.txt", "ssh FOO.%s\n" % DOMAIN.upper()), [], True),
    ("private host trailing dot", _added("a.txt", "dig foo.%s.\n" % DOMAIN), [], True),
    ("control: public IP / version", _added("a.txt", "8.8.8.8 v1.10.2.3\n"), [], False),
    ("allowlist path suppresses IP", _with_allowlist("path:examples/*\n", "examples/a.txt", IP + "\n"), [], False),
    ("allowlist match suppresses IP", _with_allowlist("match:%s\n" % IP, "a.txt", IP + "\n"), [], False),
    ("allowlist never hides denylist", _with_allowlist("path:examples/*\n", "examples/a.txt", TERM + "\n"), [], True),
    ("committed .leakcheck-denylist", _added(".leakcheck-denylist", "x\n"), [], True),
]


def _materialize(repo, args):
    out = []
    for a in args:
        if a.startswith("@body:"):
            p = os.path.join(repo.path, ".git", "pr-body")
            with open(p, "w") as f:
                f.write(a[len("@body:"):])
            a = p
        out.append(a)
    return out


def run_case(setup, args):
    with tempfile.TemporaryDirectory() as tmp:
        repo = make_repo(tmp)
        setup(repo)
        return run_scan(repo, _materialize(repo, args))


class TestBypassMatrix(unittest.TestCase):
    def test_cases(self):
        for name, setup, args, caught in BYPASS_CASES:
            with self.subTest(case=name):
                code, out = run_case(setup, args)
                self.assertEqual(code, 1 if caught else 0, out)
                self.assertNotIn(TERM, out.casefold(), "denylist text leaked into output")
                self.assertNotIn("cluster", out.casefold(), "denylist text leaked into output")


class TestRunModes(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.repo = make_repo(self._tmp.name)

    def tearDown(self):
        self._tmp.cleanup()

    def test_redacts_sensitive_path_in_annotations(self):
        _added("%s/a.txt" % TERM, "x %s\nip 10.1.2.3\n" % TERM)(self.repo)
        code, out = run_scan(self.repo, actions=True)
        self.assertEqual(code, 1)
        self.assertIn("<redacted path #1>", out)
        self.assertIn("10.1.2.3", out)
        for line in out.splitlines():
            if "cluster" in line.casefold():
                self.assertTrue(line.startswith("::add-mask::"), line)

    def test_annotations_for_generic_hit(self):
        _added("a.txt", "ok\nhost 10.1.2.3\n")(self.repo)
        code, out = run_scan(self.repo, actions=True)
        self.assertEqual(code, 1)
        self.assertIn("::error file=a.txt,line=2::", out)

    def test_no_denylist_runs_generic(self):
        _added("a.txt", "x %s\n" % TERM)(self.repo)
        code, out = run_scan(self.repo, denylist=None)
        self.assertEqual(code, 0, out)
        self.assertIn("no denylist configured", out)
        _added("b.txt", "10.9.8.7\n")(self.repo)
        code, out = run_scan(self.repo, denylist=None)
        self.assertEqual(code, 1, out)

    def test_denylist_from_env(self):
        _added("a.txt", "x %s\n" % TERM)(self.repo)
        code, out = run_scan(self.repo, denylist=None, extra_env={"LEAKCHECK_DENYLIST": TERM})
        self.assertEqual(code, 1, out)

    def test_pr_metadata_from_env(self):
        _noop(self.repo)
        code, out = run_scan(self.repo, extra_env={"LEAKCHECK_PR_BODY": "body " + TERM})
        self.assertEqual(code, 1, out)
        self.assertIn("PR body", out)

    def test_register_masks(self):
        code, out = run_scan(self.repo, ["--register-masks"])
        self.assertEqual(code, 0)
        self.assertIn("::add-mask::%s" % TERM, out)
        self.assertIn("::add-mask::%s" % TERM.upper(), out)

    def test_unknown_base_scans_head_commit(self):
        _added("a.txt", "x %s\n" % TERM)(self.repo)
        code, out = run_scan(self.repo, ["--base", "0" * 40])
        self.assertEqual(code, 1, out)
        self.assertIn("not a known commit", out)

    def test_bad_head(self):
        code, out = run_scan(self.repo, ["--head", "nope"])
        self.assertEqual(code, 2, out)

    def test_bad_allowlist(self):
        self.repo.write(".leakcheck-allow", "docs/*\n")
        code, out = run_scan(self.repo)
        self.assertEqual(code, 2, out)


def print_table():
    print("| Attempt | Expected | Result |")
    print("|---|---|---|")
    for name, setup, args, caught in BYPASS_CASES:
        code, _ = run_case(setup, args)
        got = {0: "not flagged", 1: "caught"}.get(code, "error %d" % code)
        want = "caught" if caught else "not flagged"
        print("| %s | %s | %s |" % (name, want, got))


if __name__ == "__main__":
    if "--table" in sys.argv:
        print_table()
    else:
        unittest.main()
