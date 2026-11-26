# Leak check

This repository is public. The `Leak Check` workflow
(`.github/workflows/leakcheck.yml`) fails a pull request or a push to `main`
when it finds internal names or private network details in what the change
adds. The workflow runs `hack/leakcheck/leakcheck.py`, which you can also run
locally.

## What it scans

| Event | Range |
|---|---|
| `pull_request` (any base branch; also re-runs when the title or body is edited) | `base.sha...head.sha` |
| `push` to `main` | `before...after` (only the pushed commit if `before` is unknown) |

Within that range it scans:

- lines added in the final diff, with file and line number;
- lines added by each individual non-merge commit, so text that one commit adds and a later commit removes is still caught (it stays visible in the PR's commit list);
- the full content of added or modified binary files (up to 20 MiB);
- new file paths (added, renamed or copied files);
- every commit message (subject, body and trailers) and author/committer name and email;
- the PR title, PR body and branch name. The workflow passes these through environment variables, never through `${{ }}` inside shell.

Deleted lines and paths that already exist on the base branch are not scanned, so a PR that removes a leak passes.

## What it checks

**Denylist (private).** A newline-separated list of fixed strings, matched case-insensitively. Lines that are empty or start with `#` are ignored. Every entry is masked in the job log with `::add-mask::` (as written, lower case and upper case), and findings report only `denylist entry N` with the file and line, never the matched text. A path that contains a denylist entry is shown as `<redacted path #N>`.

**Generic (always on).**

- IPv4 addresses in 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16 (RFC 1918) and 100.64.0.0/10 (CGNAT). Each octet must be 0–255 and the address must not be part of a longer dotted number, so `1.10.2.3` (not private) and `10.1.2.3.4` (five parts) are not reported. The trade-off: a four-part version string that is also a valid private address, such as `10.1.2.3`, is reported. Add it to the allowlist if that happens.
- Hostnames under the `ts[.]net` domain (dot shown in brackets), in any case and with or without a trailing dot.

### Normalisation and decoding

Before matching, each scanned text is also checked in these forms (up to two nested decodings, for example base64 inside base64):

- Unicode NFKC normalisation and removal of zero-width and other format characters (U+200B, U+200D, U+FEFF, soft hyphen and so on);
- base64 and base64url tokens of 16 or more characters;
- URL percent-encoding, HTML entities, `\xNN` / `\uNNNN` / octal escapes;
- hex strings of 8 or more bytes, with or without `:`, `-` or space separators;
- for denylist entries of 8 or more letters and digits: the same text with all punctuation and whitespace removed, which catches `secret_cluster.xyz` for the entry `secret-cluster-xyz`;
- for denylist entries: two adjacent added lines joined together, and commit messages and the PR body with line breaks removed.

### Coverage

The test suite (`hack/leakcheck/test_leakcheck.py`) runs each of these attempts against a throwaway repository with synthetic entries.

| Attempt | Result |
|---|---|
| Entry in an added line, any case | caught |
| Entry in commit subject, body, trailer or author email | caught |
| Entry in PR title, PR body or branch name | caught |
| Entry base64-encoded (including double base64) | caught |
| Entry split across two adjacent lines, in a commit message, or in the PR body | caught |
| Entry in a new or renamed file path | caught |
| Entry URL-encoded, hex, backslash-escaped, with zero-width characters, or with changed separators | caught |
| Entry in a binary file (raw or UTF-16) | caught |
| Entry added in one commit and removed in a later commit of the same PR | caught |
| Private IP (100.64.x, 172.31.x, 192.168.x), also inside a CIDR, a commit message, the PR body, a file path or base64 | caught |
| `ts[.]net` hostname in upper case or with a trailing dot | caught |
| Committing `.leakcheck-denylist` | caught |
| Entry in a deleted line | not flagged (by design) |
| Entry split across three or more lines | missed |
| Short entry (under 12 characters) base64-encoded on its own, giving a token under 16 characters | missed |
| Entry inside a compressed or encrypted file (gzip, zip, images with text) | missed |
| ROT13 or other ciphers; look-alike letters from other scripts (Cyrillic `е`) | missed |
| IP written as a decimal or hex integer, or IPv6 addresses | missed |

Also not covered: files on the base branch that the change does not touch, binary files changed in intermediate commits only, git notes and tags, PR comments and review comments, and issues.

## Configure the denylist

1. In the repository, open **Settings → Secrets and variables → Actions**.
2. Create a repository secret named `LEAKCHECK_DENYLIST` with one term per line.
3. Optionally require the check: **Settings → Branches** (or **Rules → Rulesets**) for `main`, require the status check `Leak Check`.

If the secret is missing, the workflow falls back to an Actions variable named `LEAKCHECK_DENYLIST`. Variables are not secret: GitHub prints their values in the step's `env:` header in the public log before they can be masked. Use the variable only for terms that are safe to publish.

Secrets are not passed to workflows triggered from forks, and Dependabot PRs only see Dependabot secrets. In those runs the job logs a warning and runs only the generic checks. To cover Dependabot PRs, add the same value as a Dependabot secret named `LEAKCHECK_DENYLIST`.

## Run locally

Requires Python 3.8+ and git.

```sh
# One term per line; the file is git-ignored.
printf '%s\n' 'first-term' 'second-term' > .leakcheck-denylist

# Scan the current branch against origin/main.
hack/leakcheck/leakcheck.py

# Include a PR title and body, or a different range.
hack/leakcheck/leakcheck.py --base origin/main --head HEAD \
  --pr-title "feat: my change" --pr-body-file /tmp/pr-body.md --branch "$(git branch --show-current)"
```

Instead of the file you can set `LEAKCHECK_DENYLIST` in the environment. Exit status is 0 when clean, 1 when something is found and 2 on a usage or git error.

Run the tests with `python3 -m unittest discover -s hack/leakcheck -v`. Print the bypass table with `python3 hack/leakcheck/test_leakcheck.py --table`.

## Allowlist

`.leakcheck-allow` holds exceptions for the generic checks only; it never hides a denylist match. One entry per line:

```text
# Skip generic findings in matching files:
path:docs/examples/*
# Skip this exact IP or hostname everywhere:
match:192.168.0.1
```

Keep it short. Currently it lists only the leak check's own tests and this page, which contain example addresses.
