"""Wazuh manager API actions (spec 10.1). Endpoints are marked *verify*."""

from soar_sdk import ActionError


def _client(ctx):
    cfg = ctx.connection
    http = ctx.http(auth="basic")
    # *verify*: 4.x issues a JWT for basic credentials; raw=true returns the bare token
    token = http.post("/security/user/authenticate", params={"raw": "true"}).text.strip()
    if not token:
        raise ActionError("auth_failed", "the Wazuh API returned no token")
    api = ctx.http(headers={"Authorization": "Bearer " + token})
    api.base_url = cfg.get("base_url", "").rstrip("/")
    return api


def _items(resp):
    body = resp.json() or {}
    data = body.get("data") or {}
    return data.get("affected_items") or [], int(data.get("total_affected_items") or 0)


def connection_test(ctx):
    body = _client(ctx).get("/").json() or {}
    data = body.get("data") or {}
    return {"ok": True, "api_version": data.get("api_version"), "hostname": data.get("hostname")}


def agent_get(ctx):
    agent_id = ctx.param("agent_id", required=True)
    items, _ = _items(_client(ctx).get("/agents", params={"agents_list": agent_id}))
    return {"agent": items[0] if items else {}, "found": bool(items)}


def agent_list(ctx):
    params = {"limit": ctx.param("limit", 500)}
    for k in ("status", "group", "name"):
        if ctx.param(k):
            params[k] = ctx.param(k)
    items, total = _items(_client(ctx).get("/agents", params=params))
    return {"agents": items, "total": total}


def agent_restart(ctx):
    agent_id = ctx.param("agent_id", required=True)
    if ctx.dry_run:
        return {"dry_run": True, "would_restart": agent_id}
    items, _ = _items(_client(ctx).put(f"/agents/{agent_id}/restart"))
    return {"restarted": [i if isinstance(i, str) else i.get("id") for i in items]}


def active_response(ctx):
    agents = ctx.param("agent_ids", required=True)
    command = ctx.param("command", required=True)
    body = {"command": command, "arguments": ctx.param("arguments", [])}
    if ctx.param("srcip"):
        body["alert"] = {"data": {"srcip": ctx.param("srcip")}}
    if ctx.dry_run:
        return {"dry_run": True, "would_send": body, "agents": agents}
    # *verify*: PUT /active-response?agents_list=001,002
    items, total = _items(_client(ctx).put("/active-response", params={"agents_list": ",".join(agents)}, json_body=body))
    return {"agents": items, "total": total}
