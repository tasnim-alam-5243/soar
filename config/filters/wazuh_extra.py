"""Example script filter for the Wazuh source (spec 9.5.1).

Drops successful logins from a monitoring host that runs every minute. Adjust or
replace; filter changes apply without touching Wazuh.
"""

MONITOR_IPS = {"10.10.192.250"}


def decide(events):
    out = []
    for e in events:
        rule = (e.get("rule") or {}).get("id")
        src = (e.get("data") or {}).get("srcip")
        out.append("drop" if rule == "5715" and src in MONITOR_IPS else "keep")
    return out
