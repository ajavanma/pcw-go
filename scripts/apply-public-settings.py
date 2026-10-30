"""Preview or maintain the public pcw-go repository's GitHub controls.

The default preview is entirely offline. --apply never changes visibility or
publishes a release. CodeQL uses the checked-in advanced workflow for Actions,
Go and Python. Disable default setup separately before applying these controls.
"""

import argparse
import json
from pathlib import Path
import subprocess
import sys


REPOSITORY = "ajavanma/pcw-go"
REPOSITORY_ID = 1421181393
BASE = f"repos/{REPOSITORY}"
CODEQL_WORKFLOW_PATH = ".github/workflows/codeql.yml"
CODEQL_LANGUAGES = ["actions", "go", "python"]
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


def verify_advanced_codeql():
    setup = api("GET", f"{BASE}/code-scanning/default-setup")
    verify(setup, {"state": "not-configured"},
           "CodeQL default setup disabled before using the advanced workflow")
    workflow = api("GET", f"{BASE}/actions/workflows/codeql.yml")
    verify(workflow, {"path": CODEQL_WORKFLOW_PATH, "state": "active"},
           "active checked-in CodeQL workflow")
    return workflow


def verify_codeql_run(run_id, workflow):
    verify(workflow, {"path": CODEQL_WORKFLOW_PATH, "state": "active"},
           "active checked-in CodeQL workflow")
    run = api("GET", f"{BASE}/actions/runs/{run_id}")
    main_sha = api("GET", f"{BASE}/branches/main")["commit"]["sha"]
    verify(run, {
        "repository": {"id": REPOSITORY_ID, "full_name": REPOSITORY},
        "workflow_id": workflow["id"], "path": CODEQL_WORKFLOW_PATH,
        "head_branch": "main", "head_sha": main_sha,
        "status": "completed", "conclusion": "success",
    }, "successful CodeQL run on current main")
    # Verify actual language coverage, including every page of matrix jobs.
    jobs = []
    page = 1
    while True:
        batch = api("GET", f"{BASE}/actions/runs/{run_id}/jobs?filter=latest&per_page=100&page={page}")["jobs"]
        jobs.extend(batch)
        if len(batch) < 100:
            break
        page += 1
    for language in CODEQL_LANGUAGES:
        analyses = [job for job in jobs if job.get("name") == f"Analyze ({language})"]
        if not analyses or any(
            job.get("status") != "completed" or job.get("conclusion") != "success"
            for job in analyses
        ):
            raise RuntimeError(f"Missing or unsuccessful CodeQL analysis: {language}")


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--apply", action="store_true", help="Apply to the already-public repository.")
    parser.add_argument("--include-python", action="store_true", help="Accepted for compatibility; Actions, Go and Python are always required.")
    parser.add_argument("--require-codeql", action="store_true", help="Enable the CodeQL merge gate after a successful baseline.")
    parser.add_argument("--codeql-run-id", type=int, help="Successful advanced CodeQL run on current main, used with --require-codeql.")
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
            "codeql": {
                "mode": "advanced", "workflow_path": CODEQL_WORKFLOW_PATH,
                "workflow_state": "active", "default_setup_state": "not-configured",
                "languages": CODEQL_LANGUAGES, "configuration_action": "verify only",
            },
            "codeql_gate": "Requires a successful advanced workflow run on current main covering all three languages.",
        }, indent=2))
        return

    guard_repository(api("GET", BASE))
    workflow = verify_advanced_codeql()
    if arguments.require_codeql:
        verify_codeql_run(arguments.codeql_run_id, workflow)
    for method, endpoint, payload in operations:
        api(method, endpoint, payload)
        print(f"Applied {method} {endpoint}")
    verify_baseline(protection)
    ensure_ruleset(TAG_RULESET)
    if arguments.require_codeql:
        ensure_ruleset(CODEQL_RULESET)
    print("Verified baseline repository controls and immutable version tags.")
    print("Verified active CodeQL advanced workflow and disabled default setup."
          + (" Verified CodeQL merge gate." if arguments.require_codeql else ""))


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, RuntimeError, KeyError) as error:
        print(f"public settings: {error}", file=sys.stderr)
        raise SystemExit(1)
