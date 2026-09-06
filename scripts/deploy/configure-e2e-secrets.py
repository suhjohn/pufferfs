#!/usr/bin/env python3
"""Configure approval-gated E2E credentials; never print or pass secrets in argv."""

import argparse
import json
import os
import subprocess


def gh(*args, body=None):
    result = subprocess.run(["gh", *args], input=body, text=True, capture_output=True)
    if result.returncode:
        raise RuntimeError("GitHub configuration operation failed (credentials were not logged)")
    return result.stdout


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repo", required=True, help="Explicit OWNER/REPO target")
    parser.add_argument("--environment", default="e2e")
    args = parser.parse_args()
    names = ("GEMINI_API_KEY", "TURBOPUFFER_API_KEY")
    if any(not os.environ.get(name, "").strip() for name in names):
        raise RuntimeError("Both real provider credentials must be set in the environment")
    repo = json.loads(gh("api", f"repos/{args.repo}"))
    if not repo.get("permissions", {}).get("admin"):
        raise RuntimeError("Repository admin permission is required to protect E2E credentials")
    environments = json.loads(gh("api", f"repos/{args.repo}/environments", "--paginate", "--slurp"))
    existing = next((env for page in environments for env in page["environments"]
                     if env["name"] == args.environment), None)
    if existing is None:
        user = json.loads(gh("api", "user"))
        rules = {"reviewers": [{"type": "User", "id": user["id"]}],
                 "prevent_self_review": False, "wait_timer": 0}
        gh("api", "--method", "PUT", f"repos/{args.repo}/environments/{args.environment}",
           "--input", "-", body=json.dumps(rules))
    actual = json.loads(gh("api", f"repos/{args.repo}/environments/{args.environment}"))
    if not any(rule["type"] == "required_reviewers" and rule.get("reviewers")
               for rule in actual["protection_rules"]):
        raise RuntimeError("Environment has no required reviewer; refusing to upload provider keys")
    for name in names:
        gh("secret", "set", name, "--repo", args.repo, "--env", args.environment,
           body=os.environ[name])
    stored = json.loads(gh("secret", "list", "--repo", args.repo, "--env", args.environment,
                           "--json", "name"))
    if not set(names).issubset(item["name"] for item in stored):
        raise RuntimeError("Could not verify both E2E secret names")
    print("E2E environment requires manual approval; both provider secret names verified.")
    print("No workflow was dispatched or approved. Existing review rules were preserved.")


if __name__ == "__main__":
    main()
