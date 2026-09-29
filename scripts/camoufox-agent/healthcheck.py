import json
import os
import urllib.request


request = urllib.request.Request(
    "http://127.0.0.1:8089/health",
    headers={"X-Camoufox-Token": os.environ.get("ICLOUD_HME_CAMOUFOX_TOKEN", "")},
)
with urllib.request.urlopen(request, timeout=12) as response:
    payload = json.loads(response.read().decode("utf-8"))
if not payload.get("camoufox_ready"):
    raise SystemExit(1)
