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
```

The helper checks repository ID `1421181393` and `ajavanma/pcw-go`. It refuses an
unintended repository, private visibility, a different default branch or missing
admin permission. It does not change visibility or
publish software. It preserves unrelated rulesets and reads applied settings
back to detect drift.

### CodeQL advanced workflow

The checked-in [CodeQL workflow](../.github/workflows/codeql.yml) defines analysis
for GitHub Actions, Go and Python. GitHub default setup must be disabled before
the advanced workflow uploads results; enabling both blocks advanced uploads.
The helper verifies default setup is `not-configured` and the workflow at
`.github/workflows/codeql.yml` is active before changing any repository settings.
It does not enable or disable either CodeQL setup mode.

For the migration, prepare and review the advanced workflow through a PR, then
have a maintainer disable default setup once in the repository's code security
settings before advanced analysis runs. Preserve the existing merge gate,
required CI checks and read-only workflow token default. After the workflow
lands, run its analysis on current main and verify successful `Analyze (actions)`,
`Analyze (go)` and `Analyze (python)` jobs.

Once those prerequisites hold, maintain the baseline controls with `--apply`.
To also verify and maintain the CodeQL merge gate, supply the successful advanced
workflow run on current main:

```sh
python3 scripts/apply-public-settings.py --apply
python3 scripts/apply-public-settings.py --apply --require-codeql --codeql-run-id RUN_ID
```

Replace `RUN_ID` with that run's numeric ID. The helper rejects default-setup or
unrelated workflow runs, another repository, a stale main commit, and missing or
unsuccessful analysis jobs for any of the three languages. `--include-python`
remains accepted for older commands; Python is now always included in the
expected coverage, whether the flag is present or absent.

Source visibility, enforced settings, completed CI and a versioned release are
separate facts. A successful Go module fetch from a commit does not constitute a
tagged release or a promise of a stable API.
