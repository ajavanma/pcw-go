"""Preview or maintain the public pcw-go repository's GitHub controls.

The default preview is entirely offline. --apply never changes visibility or
publishes a release. Enable the CodeQL gate only after its setup run succeeds;
add --include-python after these maintenance scripts have landed on main.
"""

import argparse
import json
from pathlib import Path
import subprocess
import sys


REPOSITORY = "ajavanma/pcw-go"
REPOSITORY_ID = 1421181393
BASE = f"repos/{REPOSITORY}"
TAG_RULESET = {
    "name": "Immutable version tags",
    "target": "tag",
    "enforcement": "active",
    "bypass_actors": [],
    "conditions": {"ref_name": {"include": ["refs/tags/v*"], "exclude": []}},
    "rules": [{"type": "update"}, {"type": "deletion"}],
}
CODEQL_RULESET = {
    "name": "CodeQL merge gate",
    "target": "branch",
    "enforcement": "active",
    "bypass_actors": [],
    "conditions": {"ref_name": {"include": ["refs/heads/main"], "exclude": []}},
    "rules": [{"type": "code_scanning", "parameters": {
        "code_scanning_tools": [{
            "tool": "CodeQL", "alerts_threshold": "errors",
            "security_alerts_threshold": "high_or_higher",
        }],
    }}],
}
REPO_SETTINGS = {
    "allow_squash_merge": True,
    "allow_merge_commit": False,
    "allow_rebase_merge": False,
    "delete_branch_on_merge": True,
    "squash_merge_commit_title": "PR_TITLE",
    "squash_merge_commit_message": "BLANK",
    "security_and_analysis": {
        "secret_scanning": {"status": "enabled"},
        "secret_scanning_push_protection": {"status": "enabled"},
    },
}
WORKFLOW_SETTINGS = {
    "default_workflow_permissions": "read",
    "can_approve_pull_request_reviews": False,
}
APPROVAL_SETTINGS = {"approval_policy": "all_external_contributors"}


def api(method, endpoint, payload=None):
    command = [
        "gh", "api", "--hostname", "github.com", "--method", method, endpoint,
        "-H", "Accept: application/vnd.github+json",
        "-H", "X-GitHub-Api-Version: 2022-11-28",
    ]
    if payload is not None:
        command.extend(["--input", "-"])
    result = subprocess.run(
        command, input=None if payload is None else json.dumps(payload),
        text=True, capture_output=True, check=False,
    )
    if result.returncode:
        raise RuntimeError(f"{method} {endpoint}: {result.stderr.strip()}")
    return json.loads(result.stdout) if result.stdout.strip() else None


def matches(observed, expected):
    """Compare maintained fields, accepting server metadata and list ordering."""
    if isinstance(expected, dict):
        return isinstance(observed, dict) and all(
            key in observed and matches(observed[key], value)
            for key, value in expected.items()
        )
    if isinstance(expected, list):
        if not isinstance(observed, list) or len(observed) != len(expected):
            return False
        unmatched = list(observed)
        for value in expected:
            index = next((i for i, item in enumerate(unmatched) if matches(item, value)), None)
            if index is None:
                return False
            unmatched.pop(index)
        return True
    return type(observed) is type(expected) and observed == expected


def verify(observed, expected, label):
    if not matches(observed, expected):
        raise RuntimeError(f"Verification failed: {label}")


def guard_repository(repository):
    verify(repository, {
        "id": REPOSITORY_ID, "full_name": REPOSITORY, "private": False,
        "default_branch": "main", "permissions": {"admin": True},
        "archived": False,
    }, "expected public repository, main default branch, and admin permission")


def baseline_operations(protection):
    return [
        ("PUT", f"{BASE}/branches/main/protection", protection),
        ("PATCH", BASE, REPO_SETTINGS),
        ("PUT", f"{BASE}/vulnerability-alerts", None),
        ("PUT", f"{BASE}/automated-security-fixes", None),
        ("PUT", f"{BASE}/private-vulnerability-reporting", None),
        ("PUT", f"{BASE}/immutable-releases", None),
        ("PUT", f"{BASE}/actions/permissions/workflow", WORKFLOW_SETTINGS),
        ("PUT", f"{BASE}/actions/permissions/fork-pr-contributor-approval", APPROVAL_SETTINGS),
    ]


def verify_baseline(protection):
    expected = dict(protection)
    # GitHub may omit restrictions entirely when they are disabled.
    expected.pop("restrictions")
    for key in ("enforce_admins", "required_linear_history", "allow_force_pushes",
                "allow_deletions", "required_conversation_resolution"):
        expected[key] = {"enabled": protection[key]}
    observed = api("GET", f"{BASE}/branches/main/protection")
    verify(observed, expected, "main branch protection")
    if observed.get("restrictions") is not None:
        raise RuntimeError("Verification failed: unexpected branch restrictions")
    verify(api("GET", BASE), REPO_SETTINGS, "repository security and merge settings")
    api("GET", f"{BASE}/vulnerability-alerts")  # 204 means enabled; 404 fails.
    verify(api("GET", f"{BASE}/automated-security-fixes"), {"enabled": True}, "security fixes")
    verify(api("GET", f"{BASE}/private-vulnerability-reporting"), {"enabled": True}, "private reports")
    verify(api("GET", f"{BASE}/immutable-releases"), {"enabled": True}, "immutable releases")
    verify(api("GET", f"{BASE}/actions/permissions/workflow"), WORKFLOW_SETTINGS, "workflow token")
    verify(api("GET", f"{BASE}/actions/permissions/fork-pr-contributor-approval"),
           APPROVAL_SETTINGS, "external contributor approval")


def list_rulesets():
    rulesets = []
    page = 1
    while True:
        batch = api("GET", f"{BASE}/rulesets?includes_parents=false&per_page=100&page={page}")
        rulesets.extend(batch)
        if len(batch) < 100:
            return rulesets
        page += 1


def ensure_ruleset(desired):
    existing = [item for item in list_rulesets() if item["name"] == desired["name"]]
    if len(existing) > 1:
        raise RuntimeError(f"Duplicate ruleset name: {desired['name']}")
    if existing:
        endpoint = f"{BASE}/rulesets/{existing[0]['id']}"
        observed = api("GET", endpoint)
        if not matches(observed, desired):
            api("PUT", endpoint, desired)
    else:
        created = api("POST", f"{BASE}/rulesets", desired)
        endpoint = f"{BASE}/rulesets/{created['id']}"
    verify(api("GET", endpoint), desired, desired["name"])


def configure_codeql(include_python):
    endpoint = f"{BASE}/code-scanning/default-setup"
    observed = api("GET", endpoint)
    # Do not disable Python scanning on later invocations without the flag.
    languages = {"actions", "go"} | set(observed.get("languages", []))
    if include_python:
        languages.add("python")
    desired = {"state": "configured", "query_suite": "default", "languages": sorted(languages)}
    if matches(observed, desired):
        return observed, False
    result = api("PATCH", endpoint, desired)
    print(f"CodeQL setup requested: {json.dumps(result)}")
    print("Wait for the setup run to succeed before requiring CodeQL; rerun to verify configuration.")
    return desired, True


def verify_codeql_run(run_id, setup):
    verify(setup, {"state": "configured"}, "configured CodeQL default setup")
    languages = setup.get("languages")
    if not isinstance(languages, list) or not {"actions", "go"}.issubset(languages):
        raise RuntimeError("CodeQL setup must include GitHub Actions and Go")
    run = api("GET", f"{BASE}/actions/runs/{run_id}")
    main_sha = api("GET", f"{BASE}/branches/main")["commit"]["sha"]
    verify(run, {
        "repository": {"id": REPOSITORY_ID, "full_name": REPOSITORY},
        "head_branch": "main", "head_sha": main_sha,
        "status": "completed", "conclusion": "success",
    }, "successful CodeQL run on current main")
    if not run.get("path", "").startswith("dynamic/github-code-scanning/codeql"):
        raise RuntimeError("The supplied run must be GitHub's default CodeQL workflow")
    # Setup updated_at can be later than its successful validation run. Verify
    # actual language coverage instead of inferring it from those timestamps.
    jobs = []
    page = 1
    while True:
        batch = api("GET", f"{BASE}/actions/runs/{run_id}/jobs?filter=latest&per_page=100&page={page}")["jobs"]
        jobs.extend(batch)
        if len(batch) < 100:
            break
        page += 1
    for language in languages:
        analyses = [job for job in jobs if job.get("name") == f"Analyze ({language})"]
        if not analyses or any(
            job.get("status") != "completed" or job.get("conclusion") != "success"
            for job in analyses
        ):
            raise RuntimeError(f"Missing or unsuccessful CodeQL analysis: {language}")


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--apply", action="store_true", help="Apply to the already-public repository.")
    parser.add_argument("--include-python", action="store_true", help="Add Python scanning after scripts land on main.")
    parser.add_argument("--require-codeql", action="store_true", help="Enable the CodeQL merge gate after a successful baseline.")
    parser.add_argument("--codeql-run-id", type=int, help="Successful default CodeQL run on current main, used with --require-codeql.")
    arguments = parser.parse_args(argv)
    if arguments.codeql_run_id is not None and arguments.codeql_run_id <= 0:
        parser.error("--codeql-run-id must be positive")
    if arguments.apply and arguments.require_codeql and arguments.codeql_run_id is None:
        parser.error("--require-codeql with --apply requires --codeql-run-id")
    if arguments.codeql_run_id is not None and not arguments.require_codeql:
        parser.error("--codeql-run-id requires --require-codeql")
    protection = json.loads(Path(__file__).with_name("branch-protection.json").read_text())
    operations = baseline_operations(protection)
    if not arguments.apply:
        print(json.dumps({
            "repository": REPOSITORY, "repository_id": REPOSITORY_ID, "mode": "offline preview",
            "operations": [{"method": method, "endpoint": endpoint, "payload": payload}
                           for method, endpoint, payload in operations],
            "named_rulesets": [TAG_RULESET] + ([CODEQL_RULESET] if arguments.require_codeql else []),
            "codeql_default_setup": {
                "state": "configured", "query_suite": "default",
                "languages": ["actions", "go"] + (["python"] if arguments.include_python else []),
                "preserve_existing_languages": True,
            },
            "codeql_gate": "Requires a successful run on current main after default setup completes.",
        }, indent=2))
        return

    guard_repository(api("GET", BASE))
    for method, endpoint, payload in operations:
        api(method, endpoint, payload)
        print(f"Applied {method} {endpoint}")
    verify_baseline(protection)
    ensure_ruleset(TAG_RULESET)
    setup, pending = configure_codeql(arguments.include_python)
    if arguments.require_codeql:
        if pending:
            raise RuntimeError("CodeQL setup changed; wait for its run before enabling the gate")
        verify_codeql_run(arguments.codeql_run_id, setup)
        ensure_ruleset(CODEQL_RULESET)
    print("Verified baseline repository controls and immutable version tags.")
    if not pending:
        print("Verified CodeQL default setup." + (" Verified CodeQL merge gate." if arguments.require_codeql else ""))


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, RuntimeError, KeyError) as error:
        print(f"public settings: {error}", file=sys.stderr)
        raise SystemExit(1)
