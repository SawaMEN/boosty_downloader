"""Fail closed unless the latest exact main push CI completed successfully."""
import json
import re
import sys


def verified_main_run(payload, sha, repository):
    if not re.fullmatch(r"[0-9a-f]{40}", sha):
        raise ValueError("invalid commit SHA")
    runs = payload["workflow_runs"]
    if not isinstance(runs, list):
        raise ValueError("invalid workflow response")
    matching = [
        run for run in runs
        if run.get("head_sha") == sha
        and run.get("head_branch") == "main"
        and run.get("event") == "push"
        and (run.get("head_repository") or {}).get("full_name") == repository
        and run.get("path") == ".github/workflows/ci.yml"
    ]
    if not matching:
        raise ValueError("no matching main push CI")
    latest = max(matching, key=lambda run: (run["run_number"], run["run_attempt"], run["id"]))
    if latest.get("status") != "completed" or latest.get("conclusion") != "success":
        raise ValueError("latest matching CI has not succeeded")
    return latest["id"]


if __name__ == "__main__":
    try:
        with open(sys.argv[1], encoding="utf-8") as stream:
            run_id = verified_main_run(json.load(stream), sys.argv[2], sys.argv[3])
        print(f"Verified main CI run {run_id}")
    except (OSError, ValueError, KeyError, TypeError, AttributeError, IndexError):
        sys.exit("Stable release refused: no verified successful main CI for this commit")
