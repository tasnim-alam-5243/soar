#!/usr/bin/env python3
"""Wazuh Integrator script: POST one alert (alert_format json) to the SOAR ingest endpoint.

Usage by Wazuh: custom-soar.py <alert_file> <api_key> <hook_url>
The api_key is the source token (SOAR_SOURCE_TOKEN_WAZUH_PROD). Exit code 0 on success; a
non-zero code makes Wazuh log the failure in integrations.log. Wazuh does not retry a failed
alert, so keep the SOAR ingest endpoint highly available.
"""
import json
import sys
import urllib.error
import urllib.request


def main() -> int:
    if len(sys.argv) < 4:
        print("usage: custom-soar.py <alert_file> <api_key> <hook_url>", file=sys.stderr)
        return 2
    alert_file, api_key, hook_url = sys.argv[1:4]
    with open(alert_file, "r", encoding="utf-8") as f:
        alert = json.load(f)
    req = urllib.request.Request(
        hook_url,
        data=json.dumps(alert).encode("utf-8"),
        headers={"Content-Type": "application/json", "Authorization": "Bearer " + api_key},
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            return 0 if resp.status == 202 else 1
    except urllib.error.HTTPError as e:
        print("soar ingest answered %d: %s" % (e.code, e.read(300)), file=sys.stderr)
        return 1
    except Exception as e:  # network errors
        print("soar ingest unreachable: %s" % e, file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
