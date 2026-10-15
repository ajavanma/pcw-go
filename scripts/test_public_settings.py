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
    def test_preview_makes_no_requests(self):
        for flags in ([], ["--include-python", "--require-codeql"]):
            with self.subTest(flags=flags), patch.object(settings, "api") as api:
                output = io.StringIO()
                with redirect_stdout(output):
                    settings.main(flags)
                preview = json.loads(output.getvalue())
                self.assertEqual(preview["mode"], "offline preview")
                self.assertEqual(preview["repository_id"], settings.REPOSITORY_ID)
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

    def test_codeql_does_not_restart_unchanged_setup_or_remove_python(self):
        configured = {"state": "configured", "languages": ["python", "go", "actions"],
                      "query_suite": "default", "updated_at": "2026-10-15T08:00:00Z"}
        with patch.object(settings, "api", return_value=configured) as api:
            setup, pending = settings.configure_codeql(False)
        self.assertEqual(setup, configured)
        self.assertFalse(pending)
        self.assertEqual(api.call_count, 1)

    def test_python_setup_change_is_reported_pending(self):
        configured = {"state": "configured", "languages": ["go", "actions"], "query_suite": "default"}
        with patch.object(settings, "api", side_effect=[configured, {"run_id": 42}]) as api, \
                redirect_stdout(io.StringIO()):
            _, pending = settings.configure_codeql(True)
        self.assertTrue(pending)
        self.assertEqual(api.call_args_list[1].args[0], "PATCH")
        self.assertEqual(api.call_args_list[1].args[2]["languages"], ["actions", "go", "python"])

    def test_codeql_run_requires_success_current_main_and_current_configuration(self):
        setup = {"state": "configured", "languages": ["actions", "go"],
                 "updated_at": "2026-10-15T08:03:00Z"}
        run = {
            "repository": {"id": settings.REPOSITORY_ID, "full_name": settings.REPOSITORY},
            "head_branch": "main", "head_sha": "current", "status": "completed",
            "conclusion": "success", "path": "dynamic/github-code-scanning/codeql",
            "created_at": "2026-10-15T08:01:00Z",
        }
        jobs = [{"name": f"Analyze ({language})", "status": "completed", "conclusion": "success"}
                for language in setup["languages"]]
        # A successful validation run can precede setup.updated_at.
        with patch.object(settings, "api", side_effect=[run, {"commit": {"sha": "current"}}, {"jobs": jobs}]):
            settings.verify_codeql_run(42, setup)
        for key, value in (
            ("head_sha", "stale"), ("conclusion", "failure"), ("status", "in_progress"),
            ("path", ".github/workflows/unrelated.yml"), ("head_branch", "other"),
            ("repository", {"id": 1, "full_name": settings.REPOSITORY}),
        ):
            with self.subTest(key=key), patch.object(settings, "api", side_effect=[
                {**run, key: value}, {"commit": {"sha": "current"}},
            ]):
                with self.assertRaises(RuntimeError):
                    settings.verify_codeql_run(42, setup)
        for invalid_jobs in (
            [], jobs[:1], [jobs[0], {**jobs[1], "conclusion": "failure"}],
            [jobs[0], {**jobs[1], "status": "in_progress"}],
        ):
            with self.subTest(jobs=invalid_jobs), patch.object(settings, "api", side_effect=[
                run, {"commit": {"sha": "current"}}, {"jobs": invalid_jobs},
            ]):
                with self.assertRaisesRegex(RuntimeError, "Missing or unsuccessful CodeQL analysis"):
                    settings.verify_codeql_run(42, setup)
        with patch.object(settings, "api", side_effect=[run, {"commit": {"sha": "current"}}, {"jobs": jobs}]):
            with self.assertRaisesRegex(RuntimeError, "analysis: python"):
                settings.verify_codeql_run(42, {**setup, "languages": ["actions", "go", "python"]})

    def test_codeql_run_checks_every_jobs_page(self):
        setup = {"state": "configured", "languages": ["actions", "go"]}
        run = {
            "repository": {"id": settings.REPOSITORY_ID, "full_name": settings.REPOSITORY},
            "head_branch": "main", "head_sha": "current", "status": "completed",
            "conclusion": "success", "path": "dynamic/github-code-scanning/codeql",
        }
        first_page = [{"name": "Analyze (actions)", "status": "completed", "conclusion": "success"}]
        first_page.extend({"name": f"Other {number}"} for number in range(99))
        second_page = [{"name": "Analyze (go)", "status": "completed", "conclusion": "success"}]
        with patch.object(settings, "api", side_effect=[
            run, {"commit": {"sha": "current"}}, {"jobs": first_page}, {"jobs": second_page},
        ]) as api:
            settings.verify_codeql_run(42, setup)
        self.assertEqual(api.call_args_list[-1].args, (
            "GET", f"{settings.BASE}/actions/runs/42/jobs?filter=latest&per_page=100&page=2",
        ))

    def test_codeql_run_refuses_unconfigured_or_incomplete_setup(self):
        for setup in (
            {"state": "not-configured", "languages": ["actions", "go"]},
            {"state": "configured", "languages": ["go"]},
            {"state": "configured"},
        ):
            with self.subTest(setup=setup), patch.object(settings, "api") as api:
                with self.assertRaises(RuntimeError):
                    settings.verify_codeql_run(42, setup)
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
