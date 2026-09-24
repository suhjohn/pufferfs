"""Assert observability of the stale-candidate workflow from API process logs."""
import re
import sys

verified = False
for line in sys.stdin:
    if "search_metrics " not in line:
        continue
    metrics = {key: int(value) for key, value in re.findall(r"(provider_calls|publication_passes|publication_retries|rejected_candidates)=(\d+)", line)}
    if (metrics.get("publication_passes") == 2 and metrics.get("publication_retries") == 1
            and metrics.get("provider_calls", 0) > 1 and metrics.get("rejected_candidates", 0) > 0
            and "failed=false" in line):
        verified = True
assert verified, "stale-candidate search did not report its retry and rejected candidates"
print("API logs measured the successful stale-candidate retry and rejection count.")
