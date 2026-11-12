#!/usr/bin/env python3
"""Wazuh Integrator script (Wazuh 4.x): POST one alert to the SOAR ingest endpoint.

Wazuh runs it as:  custom-soar <alert_file> <api_key> <hook_url> [<options_file>] [debug]
(the shell wrapper custom-soar starts this file with Wazuh's own Python). Install both files
in /var/ossec/integrations (owner root:wazuh, mode 750) and add to /var/ossec/etc/ossec.conf:

  <integration>
    <name>custom-soar</name>
    <hook_url>http://SOAR_HOST:8081/ingest/wazuh-prod</hook_url>
    <api_key>SOAR_SOURCE_TOKEN_WAZUH_PROD</api_key>
    <level>5</level>
    <alert_format>json</alert_format>
    <options>{"timeout": 5, "retries": 2}</options>
  </integration>

api_key is the SOAR source token (SOAR_SOURCE_TOKEN_WAZUH_PROD). <options> is optional JSON:
  timeout   seconds per attempt (default 5)
  retries   extra attempts after a 429/5xx answer or a network error (default 2; Wazuh itself
            never retries, so a failed alert is lost: keep SOAR ingest available)
  backoff   seconds before the first retry, doubled each time (default 1)
  ca_file   CA certificate (PEM) that signed SOAR's certificate when hook_url is https://

Results go to /var/ossec/logs/integrations.log; the token is never logged. Exit code 0 means
SOAR accepted the alert (HTTP 202).
"""
import json
import os
import ssl
import sys
import time
import urllib.error
import urllib.request
from datetime import datetime

NAME = "custom-soar"
WAZUH_DIR = os.path.dirname(os.path.dirname(os.path.realpath(__file__)))
LOG_FILE = os.path.join(WAZUH_DIR, "logs", "integrations.log")
DEFAULTS = {"timeout": 5.0, "retries": 2, "backoff": 1.0, "ca_file": ""}


def log(msg: str) -> None:
    line = "%s %s: %s\n" % (datetime.now().strftime("%Y/%m/%d %H:%M:%S"), NAME, msg)
    try:
        with open(LOG_FILE, "a", encoding="utf-8") as f:
            f.write(line)
    except OSError:
        sys.stderr.write(line)


def read_options(args):
    opts = dict(DEFAULTS)
    for a in args:
        if a.endswith("options") and os.path.isfile(a):  # Wazuh names the file <...>.options
            try:
                with open(a, "r", encoding="utf-8") as f:
                    given = json.load(f)
            except (OSError, ValueError) as e:
                log("ignoring unreadable options file: %s" % e)
                break
            if isinstance(given, dict):
                for k in DEFAULTS:
                    if k in given:
                        opts[k] = given[k]
            break
    return opts


def post(hook_url, api_key, body, opts):
    """One attempt: returns (status, detail); status 0 means the request did not complete."""
    req = urllib.request.Request(hook_url, data=body, method="POST", headers={
        "Content-Type": "application/json", "Authorization": "Bearer " + api_key, "User-Agent": "wazuh-" + NAME})
    context = None
    if hook_url.startswith("https://"):
        context = ssl.create_default_context(cafile=opts["ca_file"] or None)
    try:
        with urllib.request.urlopen(req, timeout=float(opts["timeout"]), context=context) as resp:
            return resp.status, ""
    except urllib.error.HTTPError as e:
        return e.code, e.read(300).decode("utf-8", "replace")
    except Exception as e:  # network errors, timeouts, TLS
        return 0, str(e)


def main(argv) -> int:
    if len(argv) < 4:
        log("usage: %s <alert_file> <api_key> <hook_url> [options_file] [debug]" % NAME)
        return 2
    alert_file, api_key, hook_url = argv[1:4]
    debug = "debug" in argv[4:]
    opts = read_options(argv[4:])
    try:
        with open(alert_file, "r", encoding="utf-8") as f:
            alert = json.load(f)
    except (OSError, ValueError) as e:
        log("cannot read the alert file %s: %s" % (alert_file, e))
        return 1
    rule = alert.get("rule") or {}
    what = "alert %s (rule %s, level %s)" % (alert.get("id", "?"), rule.get("id", "?"), rule.get("level", "?"))
    body = json.dumps(alert).encode("utf-8")
    attempts = 1 + max(0, int(opts["retries"]))
    wait = float(opts["backoff"])
    for attempt in range(1, attempts + 1):
        status, detail = post(hook_url, api_key, body, opts)
        if status == 202 or 200 <= status < 300:
            if debug:
                log("%s accepted by SOAR (attempt %d)" % (what, attempt))
            return 0
        retryable = status == 0 or status == 429 or status >= 500
        reason = ("HTTP %d %s" % (status, detail.strip()[:200])) if status else detail[:200]
        if not retryable or attempt == attempts:
            hint = " (check api_key against SOAR_SOURCE_TOKEN_WAZUH_PROD)" if status == 401 else ""
            log("%s NOT delivered to %s after %d attempt(s): %s%s" % (what, hook_url, attempt, reason, hint))
            return 1
        if debug:
            log("%s attempt %d failed (%s); retrying in %.1fs" % (what, attempt, reason, wait))
        time.sleep(wait)
        wait *= 2
    return 1


if __name__ == "__main__":
    sys.exit(main(sys.argv))
