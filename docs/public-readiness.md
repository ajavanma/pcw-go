# Public repository controls

The initial Go runner was developed privately. The maintainer authorized public
source access on 2026-10-15. The repository retains its fresh history and MIT
license; the API remains experimental, with no version tag or module release.

The pre-publication review covered all three reachable commits and 24 unique
UTF-8 file blobs, including synthetic fixture provenance, author metadata and
GitHub Actions permissions. No credential/private-data blocker or binary
artifact was found. This was a scoped pattern and manual review; Gitleaks was
not installed. Existing fixtures use normalized `/fixture/pcw` paths and contain
responses from generated synthetic data, not research artifacts.

## Main and workflow protection

- Require PRs and all five named CI checks in
  [branch-protection.json](../scripts/branch-protection.json), with branches
  current against main. Apply these protections to administrators.
- Require linear history and resolved conversations; prohibit force pushes and
  branch deletion. The solo-maintainer policy requires zero additional approving
  reviewers, so it does not pretend to provide independent human approval.
- Require CodeQL analysis, blocking error-level and high/critical security
  findings. The code-scanning rule has no bypass actors.
- Use read-only workflow tokens and prevent Actions from approving PRs. Require
  approval for all external contributors before their workflows execute.
- Pin workflow actions to immutable revisions. Dependabot tracks Go modules and
  Actions; compatibility CI pins the Python worker by commit and archive hash.
- Use squash-only merges with short PR titles and blank commit bodies.

## Security and release controls

Secret scanning and push protection are enabled, together with dependency
alerts, automatic security fixes, and private vulnerability reporting. The
[security policy](../SECURITY.md) links directly to GitHub's private report form.

An active `v*` tag ruleset prevents tag updates and deletion without bypass
actors. It permits creating a reviewed version tag in a future release. No tag
or release is created by this public-source transition.
GitHub immutable releases are also enabled to protect future published release
assets and their associated tags.

## Maintain and verify

The settings helper requires Python and an authenticated `gh` administrator.
Without `--apply`, it prints an offline preview and sends no requests:

```sh
python3 scripts/apply-public-settings.py
python3 scripts/apply-public-settings.py --apply
```

The helper checks repository ID `1421181393` and `ajavanma/pcw-go`. It refuses an
unintended repository, private visibility, a different default branch or missing
admin permission. It does not change visibility or
publish software. It preserves unrelated rulesets and reads applied settings
back to detect drift.

After the initial CodeQL setup completes successfully, enforce its merge gate
with `--apply --require-codeql --codeql-run-id RUN_ID`, replacing `RUN_ID` with
the successful default CodeQL run on current main. Once the Python maintenance
script is on main, use `--apply --include-python` to cover Go, GitHub Actions and Python. Verify that
the resulting analysis completes before treating the new language as covered.

Source visibility, enforced settings, completed CI and a versioned release are
separate facts. A successful Go module fetch from a commit does not constitute a
tagged release or a promise of a stable API.
