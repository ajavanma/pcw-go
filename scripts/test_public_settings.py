"""Offline safety tests for the public repository maintenance script."""

from contextlib import redirect_stdout
from copy import deepcopy
import importlib.util
import io
import json
from pathlib import Path
import unittest
from unittest.mock import patch


SPEC = importlib.util.spec_from_file_location(
    "public_settings", Path(__file__).with_name("apply-public-settings.py")
)
settings = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(settings)


class PublicSettingsTest(unittest.TestCase):
    def setUp(self):
        self.repository = {
            "id": settings.REPOSITORY_ID, "full_name": settings.REPOSITORY,
            "private": False, "default_branch": "main",
            "permissions": {"admin": True}, "archived": False,
        }
        self.workflow = {"id": 7, "path": settings.CODEQL_WORKFLOW_PATH, "state": "active"}
        self.run = {
            "repository": {"id": settings.REPOSITORY_ID, "full_name": settings.REPOSITORY},
            "workflow_id": 7, "path": settings.CODEQL_WORKFLOW_PATH,
            "head_branch": "main", "head_sha": "current", "status": "completed",
            "conclusion": "success",
        }
        self.jobs = [
            {"name": f"Analyze ({language})", "status": "completed", "conclusion": "success"}
            for language in ("actions", "go", "python")
        ]

    def test_preview_makes_no_requests(self):
        for flags in ([], ["--include-python"], ["--require-codeql"],
                      ["--include-python", "--require-codeql"]):
            with self.subTest(flags=flags), patch.object(settings, "api") as api:
                output = io.StringIO()
                with redirect_stdout(output):
                    settings.main(flags)
                preview = json.loads(output.getvalue())
                self.assertEqual(preview["mode"], "offline preview")
                self.assertEqual(preview["repository_id"], settings.REPOSITORY_ID)
                self.assertEqual(preview["codeql"], {
                    "mode": "advanced", "workflow_path": ".github/workflows/codeql.yml",
                    "workflow_state": "active", "default_setup_state": "not-configured",
                    "languages": ["actions", "go", "python"], "configuration_action": "verify only",
                })
                api.assert_not_called()

    def test_guard_rejects_wrong_identity_or_access_before_mutations(self):
        valid = {
            "id": settings.REPOSITORY_ID, "full_name": settings.REPOSITORY,
            "private": False, "default_branch": "main",
            "permissions": {"admin": True}, "archived": False,
        }
        settings.guard_repository(valid)
        for key, value in (
            ("id", 1), ("full_name", "elsewhere/pcw-go"), ("private", True),
            ("default_branch", "other"), ("permissions", {"admin": False}),
            ("archived", True),
        ):
            with self.subTest(key=key), patch.object(settings, "api", return_value={**valid, key: value}) as api:
                with self.assertRaises(RuntimeError):
                    settings.main(["--apply"])
                self.assertEqual(api.call_args_list, [unittest.mock.call("GET", settings.BASE)])

    def test_comparison_detects_security_drift(self):
        desired = settings.TAG_RULESET
        observed = {**deepcopy(desired), "id": 12, "_links": {}}
        observed["rules"].reverse()
        self.assertTrue(settings.matches(observed, desired))
        for key, value in (
            ("bypass_actors", [{"actor_id": 1, "actor_type": "RepositoryRole"}]),
            ("rules", [{"type": "update"}]), ("enforcement", "disabled"),
        ):
            self.assertFalse(settings.matches({**observed, key: value}, desired))
        self.assertFalse(settings.matches({"enabled": 1}, {"enabled": True}))
        self.assertFalse(settings.matches({"checks": ["a", "a"]}, {"checks": ["a", "b"]}))

    def test_existing_ruleset_is_idempotent_and_unrelated_rules_untouched(self):
        observed = {**settings.TAG_RULESET, "id": 12}
        with patch.object(settings, "list_rulesets", return_value=[
            {"id": 99, "name": "Unrelated"}, {"id": 12, "name": observed["name"]},
        ]), patch.object(settings, "api", return_value=observed) as api:
            settings.ensure_ruleset(settings.TAG_RULESET)
        self.assertEqual(len(api.call_args_list), 2)
        for call in api.call_args_list:
            self.assertEqual(call.args, ("GET", f"{settings.BASE}/rulesets/12"))

    def test_ruleset_updates_drift_and_checks_result(self):
        desired = settings.CODEQL_RULESET
        stale = {**desired, "bypass_actors": [{"actor_id": 5}]}
        with patch.object(settings, "list_rulesets", return_value=[{"id": 12, "name": desired["name"]}]), \
                patch.object(settings, "api", side_effect=[stale, desired, desired]) as api:
            settings.ensure_ruleset(desired)
        self.assertEqual(api.call_args_list[1].args, ("PUT", f"{settings.BASE}/rulesets/12", desired))

    def test_duplicate_ruleset_names_fail_without_mutating(self):
        with patch.object(settings, "list_rulesets", return_value=[
            {"id": 1, "name": settings.TAG_RULESET["name"]},
            {"id": 2, "name": settings.TAG_RULESET["name"]},
        ]), patch.object(settings, "api") as api:
            with self.assertRaises(RuntimeError):
                settings.ensure_ruleset(settings.TAG_RULESET)
            api.assert_not_called()

    def test_advanced_codeql_verification_is_read_only(self):
        with patch.object(settings, "api", side_effect=[
            {"state": "not-configured"}, self.workflow,
        ]) as api:
            self.assertEqual(settings.verify_advanced_codeql(), self.workflow)
        self.assertEqual(api.call_args_list, [
            unittest.mock.call("GET", f"{settings.BASE}/code-scanning/default-setup"),
            unittest.mock.call("GET", f"{settings.BASE}/actions/workflows/codeql.yml"),
        ])

    def test_configured_default_setup_is_refused_before_any_mutations(self):
        for flags in (["--apply"], ["--apply", "--include-python"],
                      ["--apply", "--require-codeql", "--codeql-run-id", "42"]):
            with self.subTest(flags=flags), patch.object(settings, "api", side_effect=[
                self.repository, {"state": "configured", "languages": ["actions", "go", "python"]},
            ]) as api:
                with self.assertRaisesRegex(RuntimeError, "default setup disabled"):
                    settings.main(flags)
                self.assertEqual(api.call_args_list, [
                    unittest.mock.call("GET", settings.BASE),
                    unittest.mock.call("GET", f"{settings.BASE}/code-scanning/default-setup"),
                ])

    def test_inactive_or_wrong_workflow_is_refused_before_any_mutations(self):
        for key, value in (("state", "disabled_manually"), ("path", ".github/workflows/other.yml")):
            with self.subTest(key=key), patch.object(settings, "api", side_effect=[
                self.repository, {"state": "not-configured"}, {**self.workflow, key: value},
            ]) as api:
                with self.assertRaisesRegex(RuntimeError, "active checked-in CodeQL workflow"):
                    settings.main(["--apply"])
                self.assertTrue(all(call.args[0] == "GET" for call in api.call_args_list))

    def test_apply_never_reconfigures_default_setup(self):
        protection = json.loads(Path(settings.__file__).with_name("branch-protection.json").read_text())
        operations = settings.baseline_operations(protection)
        for flags in (["--apply"], ["--apply", "--include-python"]):
            with self.subTest(flags=flags), patch.object(settings, "api", side_effect=[
                self.repository, {"state": "not-configured"}, self.workflow,
                *([None] * len(operations)),
            ]) as api, patch.object(settings, "verify_baseline"), \
                    patch.object(settings, "ensure_ruleset") as ruleset, redirect_stdout(io.StringIO()):
                settings.main(flags)
                self.assertEqual(api.call_args_list[3:], [
                    unittest.mock.call(method, endpoint, payload)
                    for method, endpoint, payload in operations
                ])
                ruleset.assert_called_once_with(settings.TAG_RULESET)

    def test_codeql_run_requires_success_current_main_and_advanced_workflow(self):
        with patch.object(settings, "api", side_effect=[
            self.run, {"commit": {"sha": "current"}}, {"jobs": self.jobs},
        ]):
            settings.verify_codeql_run(42, self.workflow)
        for key, value in (
            ("head_sha", "stale"), ("conclusion", "failure"), ("status", "in_progress"),
            ("path", ".github/workflows/unrelated.yml"), ("head_branch", "other"),
            ("path", "dynamic/github-code-scanning/codeql"), ("workflow_id", 8),
            ("repository", {"id": 1, "full_name": settings.REPOSITORY}),
            ("repository", {"id": settings.REPOSITORY_ID, "full_name": "elsewhere/pcw-go"}),
        ):
            with self.subTest(key=key), patch.object(settings, "api", side_effect=[
                {**self.run, key: value}, {"commit": {"sha": "current"}},
            ]):
                with self.assertRaises(RuntimeError):
                    settings.verify_codeql_run(42, self.workflow)

    def test_codeql_run_requires_all_three_successful_language_jobs(self):
        invalid_job_sets = [[]]
        for index in range(3):
            invalid_job_sets.append(self.jobs[:index] + self.jobs[index + 1:])
            for key, value in (("conclusion", "failure"), ("conclusion", "skipped"),
                               ("status", "in_progress")):
                invalid_job_sets.append([
                    {**job, key: value} if number == index else job
                    for number, job in enumerate(self.jobs)
                ])
        invalid_job_sets.append(self.jobs + [{**self.jobs[0], "conclusion": "failure"}])
        for invalid_jobs in invalid_job_sets:
            with self.subTest(jobs=invalid_jobs), patch.object(settings, "api", side_effect=[
                self.run, {"commit": {"sha": "current"}}, {"jobs": invalid_jobs},
            ]):
                with self.assertRaisesRegex(RuntimeError, "Missing or unsuccessful CodeQL analysis"):
                    settings.verify_codeql_run(42, self.workflow)

    def test_invalid_baseline_run_is_refused_before_any_mutations(self):
        with patch.object(settings, "api", side_effect=[
            self.repository, {"state": "not-configured"}, self.workflow,
            {**self.run, "head_sha": "stale"}, {"commit": {"sha": "current"}},
        ]) as api:
            with self.assertRaisesRegex(RuntimeError, "successful CodeQL run on current main"):
                settings.main(["--apply", "--require-codeql", "--codeql-run-id", "42"])
            self.assertTrue(all(call.args[0] == "GET" for call in api.call_args_list))

    def test_codeql_run_checks_every_jobs_page(self):
        first_page = [self.jobs[0]]
        first_page.extend({"name": f"Other {number}"} for number in range(99))
        second_page = self.jobs[1:]
        with patch.object(settings, "api", side_effect=[
            self.run, {"commit": {"sha": "current"}}, {"jobs": first_page}, {"jobs": second_page},
        ]) as api:
            settings.verify_codeql_run(42, self.workflow)
        self.assertEqual(api.call_args_list[-1].args, (
            "GET", f"{settings.BASE}/actions/runs/42/jobs?filter=latest&per_page=100&page=2",
        ))

    def test_codeql_run_refuses_inactive_or_wrong_workflow(self):
        for workflow in (
            {**self.workflow, "state": "disabled_manually"},
            {**self.workflow, "path": ".github/workflows/other.yml"},
            {},
        ):
            with self.subTest(workflow=workflow), patch.object(settings, "api") as api:
                with self.assertRaises(RuntimeError):
                    settings.verify_codeql_run(42, workflow)
                api.assert_not_called()

    def test_baseline_verification_detects_branch_drift(self):
        protection = json.loads(Path(settings.__file__).with_name("branch-protection.json").read_text())
        observed = deepcopy(protection)
        for key in ("enforce_admins", "required_linear_history", "allow_force_pushes",
                    "allow_deletions", "required_conversation_resolution"):
            observed[key] = {"enabled": protection[key]}
        observed["required_status_checks"]["strict"] = False
        with patch.object(settings, "api", return_value=observed):
            with self.assertRaisesRegex(RuntimeError, "main branch protection"):
                settings.verify_baseline(protection)


if __name__ == "__main__":
    unittest.main()
