import unittest

from release_gate import verified_main_run


SHA = "a" * 40
REPO = "SawaMEN/boosty_downloader"


def run(**changes):
    result = {
        "head_sha": SHA, "head_branch": "main", "event": "push",
        "head_repository": {"full_name": REPO}, "path": ".github/workflows/ci.yml",
        "run_number": 1, "run_attempt": 1, "id": 100,
        "status": "completed", "conclusion": "success",
    }
    result.update(changes)
    return result


class ReleaseGateTests(unittest.TestCase):
    def test_exact_success(self):
        self.assertEqual(verified_main_run({"workflow_runs": [run()]}, SHA, REPO), 100)

    def test_rejects_unrelated_or_unsuccessful_runs(self):
        for change in (
            {"head_sha": "b" * 40}, {"head_branch": "feature"},
            {"event": "pull_request"}, {"head_repository": {"full_name": "fork/project"}},
            {"path": ".github/workflows/other.yml"}, {"status": "in_progress"},
            {"conclusion": "failure"}, {"conclusion": "cancelled"},
            {"head_repository": None},
        ):
            with self.subTest(change=change), self.assertRaises(ValueError):
                verified_main_run({"workflow_runs": [run(**change)]}, SHA, REPO)

    def test_newer_failure_blocks_old_success(self):
        with self.assertRaises(ValueError):
            verified_main_run({"workflow_runs": [
                run(), run(run_number=2, id=101, conclusion="failure"),
            ]}, SHA, REPO)

    def test_empty_runs(self):
        with self.assertRaises(ValueError):
            verified_main_run({"workflow_runs": []}, SHA, REPO)


if __name__ == "__main__":
    unittest.main()
