"""DFIR-IRIS actions (spec 10.1). The CaseSystem client below is selected by the
connection's api_version; only legacy_2_4 exists today. Instance-specific ids (IOC
types, TLP levels, asset types, users) are resolved by name on each call."""

import os
from datetime import datetime, timezone

import yaml

from soar_sdk import ActionError

HERE = os.path.dirname(os.path.abspath(__file__))
with open(os.path.join(HERE, "endpoints.yaml"), encoding="utf-8") as fh:
    ENDPOINTS = yaml.safe_load(fh)

# SOAR observable types -> IRIS IOC type names
IOC_TYPES = {"ip": "ip-any", "domain": "domain", "url": "url", "email": "email", "file_path": "filename",
             "host": "hostname", "user": "account"}
HASH_BY_LEN = {32: "md5", 40: "sha1", 64: "sha256"}
SEVERITY = {1: 2, 2: 3, 3: 4, 4: 5, 5: 6}  # SOAR 1-5 -> IRIS ids (Informational..Critical) *verify*


class CaseSystem:
    """Client for one IRIS API generation."""

    def __init__(self, ctx):
        version = ctx.connection.get("api_version", "legacy_2_4")
        if version not in ENDPOINTS:
            raise ActionError("unsupported", f"api_version {version!r} is not implemented (available: {', '.join(ENDPOINTS)})")
        self.ep = ENDPOINTS[version]
        self.http = ctx.http(auth="bearer")
        self.ctx = ctx
        self.customer = int(ctx.connection.get("customer_id", 1))

    def call(self, name, case_id=None, body=None, **path):
        e = self.ep[name]
        p = e["path"].format(**path)
        params = {"cid": case_id} if e.get("case_scoped") else None
        r = self.http.request(e["method"], p, params=params, json_body=body)
        data = r.json() or {}
        if data.get("status") not in (None, "success"):
            raise ActionError("iris_error", str(data.get("message") or data)[:500])
        return data.get("data", data)

    def lookup(self, name, key, want, id_key):
        items = self.call(name)
        if isinstance(items, dict):
            items = items.get(name) or list(items.values())
        for it in items or []:
            if str(it.get(key, "")).lower() == str(want).lower():
                return it[id_key]
        raise ActionError("not_found", f"IRIS has no {name.replace('_', ' ')} entry named {want!r}")

    def user_ids(self, names):
        users = self.call("users") or []
        by_name = {str(u.get("user_login", u.get("user_name", ""))).lower(): u.get("user_id") for u in users}
        ids = []
        for n in names or []:
            uid = by_name.get(str(n).lower())
            if uid is None:
                raise ActionError("not_found", f"IRIS has no user {n!r}")
            ids.append(uid)
        return ids


def connection_test(ctx):
    data = CaseSystem(ctx).call("ping")
    return {"ok": True, "ping": data}


def case_create(ctx):
    name = ctx.param("name", required=True)
    if ctx.dry_run:
        return {"dry_run": True, "would_create": name}
    iris = CaseSystem(ctx)
    body = {"case_name": name, "case_description": ctx.param("description", "") or name, "case_customer": iris.customer,
            "case_soc_id": ctx.param("soar_case_id", "") or ctx.idempotency_key[:60]}
    if ctx.param("classification_id"):
        body["classification_id"] = ctx.param("classification_id")
    data = iris.call("case_create", body=body)
    cid = data.get("case_id")
    if ctx.param("assignees"):
        ctx.log("assignees are applied through tasks; IRIS 2.4 cases have a single owner")
    base = ctx.connection.get("base_url", "").rstrip("/")
    return {"case_id": cid, "url": f"{base}/case?cid={cid}"}


def case_update(ctx):
    cid = ctx.param("case_id", required=True)
    body = {k2: ctx.param(k1) for k1, k2 in (("name", "case_name"), ("description", "case_description")) if ctx.param(k1) is not None}
    if ctx.param("tags"):
        body["case_tags"] = ",".join(ctx.param("tags"))
    if ctx.dry_run:
        return {"dry_run": True, "case_id": cid, "would_change": sorted(body)}
    iris = CaseSystem(ctx)
    if ctx.param("owner"):
        body["owner_id"] = iris.user_ids([ctx.param("owner")])[0]
    iris.call("case_update", body=body, case_id=cid)
    return {"case_id": cid, "updated": sorted(body)}


def case_close(ctx):
    cid = ctx.param("case_id", required=True)
    if ctx.dry_run:
        return {"dry_run": True, "would_close": cid}
    CaseSystem(ctx).call("case_close", case_id=cid)
    return {"case_id": cid, "closed": True}


def alert_create(ctx):
    title = ctx.param("title", required=True)
    if ctx.dry_run:
        return {"dry_run": True, "would_create": title}
    iris = CaseSystem(ctx)
    body = {"alert_title": title, "alert_description": ctx.param("description", ""), "alert_source": ctx.param("source", "SOAR"),
            "alert_source_ref": ctx.param("source_ref", ""), "alert_severity_id": SEVERITY.get(int(ctx.param("severity", 3)), 4),
            "alert_status_id": 2, "alert_customer_id": iris.customer, "alert_source_content": ctx.param("source_content", {}),
            "alert_tags": ",".join(ctx.param("tags", [])), "alert_source_event_time": datetime.now(timezone.utc).isoformat()}
    iocs = []
    for i in ctx.param("iocs", []) or []:
        t = _ioc_type(i.get("type", ""), i.get("value", ""))
        iocs.append({"ioc_value": i.get("value"), "ioc_type_id": iris.lookup("ioc_types", "type_name", t, "type_id"),
                     "ioc_tlp_id": iris.lookup("tlp_list", "tlp_name", i.get("tlp", "amber"), "tlp_id"), "ioc_description": i.get("description", "")})
    if iocs:
        body["alert_iocs"] = iocs
    data = iris.call("alert_create", body=body)
    return {"alert_id": data.get("alert_id"), "iocs": len(iocs)}


def alert_update(ctx):
    aid = ctx.param("alert_id", required=True)
    body = {}
    if ctx.param("status_id") is not None:
        body["alert_status_id"] = ctx.param("status_id")
    if ctx.param("severity") is not None:
        body["alert_severity_id"] = SEVERITY.get(int(ctx.param("severity")), 4)
    if ctx.dry_run:
        return {"dry_run": True, "alert_id": aid, "would_change": sorted(body)}
    iris = CaseSystem(ctx)
    if ctx.param("owner"):
        body["alert_owner_id"] = iris.user_ids([ctx.param("owner")])[0]
    iris.call("alert_update", body=body, alert_id=aid)
    return {"alert_id": aid, "updated": sorted(body)}


def alert_merge(ctx):
    aid, cid = ctx.param("alert_id", required=True), ctx.param("case_id", required=True)
    if ctx.dry_run:
        return {"dry_run": True, "would_merge": aid, "into": cid}
    CaseSystem(ctx).call("alert_merge", body={"target_case_id": cid, "iocs_import_list": [], "assets_import_list": [],
                                              "note": ctx.param("note", "merged by SOAR"), "import_as_event": False}, alert_id=aid)
    return {"alert_id": aid, "case_id": cid, "merged": True}


def _ioc_type(t, value):
    if t == "hash":
        return HASH_BY_LEN.get(len(value or ""), "other")
    return IOC_TYPES.get(t, t)


def ioc_add(ctx):
    cid = ctx.param("case_id", required=True)
    value = ctx.param("value", required=True)
    t = _ioc_type(ctx.param("type", required=True), value)
    if ctx.dry_run:
        return {"dry_run": True, "would_add": value, "type": t}
    iris = CaseSystem(ctx)
    body = {"ioc_value": value, "ioc_type_id": iris.lookup("ioc_types", "type_name", t, "type_id"),
            "ioc_tlp_id": iris.lookup("tlp_list", "tlp_name", ctx.param("tlp", "amber"), "tlp_id"),
            "ioc_description": ctx.param("description", "added by SOAR"), "ioc_tags": ",".join(ctx.param("tags", []))}
    data = iris.call("ioc_add", case_id=cid, body=body)
    return {"ioc_id": data.get("ioc_id"), "case_id": cid}


def ioc_list(ctx):
    data = CaseSystem(ctx).call("ioc_list", case_id=ctx.param("case_id", required=True))
    iocs = data.get("ioc", data) if isinstance(data, dict) else data
    return {"iocs": iocs or []}


def asset_add(ctx):
    cid = ctx.param("case_id", required=True)
    name = ctx.param("name", required=True)
    if ctx.dry_run:
        return {"dry_run": True, "would_add": name}
    iris = CaseSystem(ctx)
    body = {"asset_name": name, "asset_type_id": iris.lookup("asset_types", "asset_name", ctx.param("type", "Windows - Computer"), "asset_id"),
            "asset_ip": ctx.param("ip", ""), "asset_description": ctx.param("description", ""), "analysis_status_id": 1}
    data = iris.call("asset_add", case_id=cid, body=body)
    return {"asset_id": data.get("asset_id"), "case_id": cid}


def note_add(ctx):
    cid = ctx.param("case_id", required=True)
    if ctx.dry_run:
        return {"dry_run": True, "would_add_note": ctx.param("title")}
    iris = CaseSystem(ctx)
    group = ctx.param("group", "SOAR")
    groups = iris.call("note_group_list", case_id=cid) or []
    gid = next((g.get("group_id") for g in groups if g.get("group_title") == group), None)
    if gid is None:
        gid = iris.call("note_group_add", case_id=cid, body={"group_title": group}).get("group_id")
    data = iris.call("note_add", case_id=cid, body={"note_title": ctx.param("title", required=True), "note_content": ctx.param("content", required=True), "group_id": gid})
    return {"note_id": data.get("note_id"), "case_id": cid}


def timeline_add(ctx):
    cid = ctx.param("case_id", required=True)
    if ctx.dry_run:
        return {"dry_run": True, "would_add_event": ctx.param("title")}
    when = datetime.fromisoformat(ctx.param("date", required=True).replace("Z", "+00:00"))
    body = {"event_title": ctx.param("title", required=True), "event_content": ctx.param("content", ""), "event_raw": "",
            "event_date": when.strftime("%Y-%m-%dT%H:%M:%S.000"), "event_tz": "+00:00", "event_category_id": ctx.param("category_id", 1),
            "event_assets": [], "event_iocs": [], "event_in_summary": True, "event_in_graph": True, "event_source": "SOAR", "event_tags": ""}
    data = CaseSystem(ctx).call("timeline_add", case_id=cid, body=body)
    return {"event_id": data.get("event_id"), "case_id": cid}


def task_create(ctx):
    cid = ctx.param("case_id", required=True)
    if ctx.dry_run:
        return {"dry_run": True, "would_create_task": ctx.param("title")}
    iris = CaseSystem(ctx)
    body = {"task_title": ctx.param("title", required=True), "task_description": ctx.param("description", ""), "task_status_id": 1,
            "task_assignees_id": iris.user_ids(ctx.param("assignees", [])), "task_tags": "SOAR"}
    data = iris.call("task_add", case_id=cid, body=body)
    return {"task_id": data.get("id", data.get("task_id")), "case_id": cid}
