"""Microsoft Graph mail actions (spec 10.1).

Every action names a mailbox, which must pass the connection's mailbox_scope. Write and
destructive actions honour dry_run. Graph throttling (429) and 5xx errors are retryable
through the platform's retry layer.
"""

import base64
import hashlib
import json
import time
import uuid
from urllib.parse import quote

from soar_sdk import ActionError, HTTP

GRAPH = "https://graph.microsoft.com"


# ---- auth -------------------------------------------------------------------------

def _token(ctx):
    cfg = ctx.connection
    tenant, client_id = cfg.get("tenant_id"), cfg.get("client_id")
    if not tenant or not client_id:
        raise ActionError("invalid_connection", "the connection needs tenant_id and client_id")
    authority = cfg.get("authority_url", "https://login.microsoftonline.com").rstrip("/")
    url = f"{authority}/{quote(tenant)}/oauth2/v2.0/token"
    form = {"grant_type": "client_credentials", "client_id": client_id, "scope": "https://graph.microsoft.com/.default"}
    cert, key = ctx.secret("certificate_pem", required=False), ctx.secret("private_key_pem", required=False)
    if cert and key:
        form["client_assertion_type"] = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"
        form["client_assertion"] = _assertion(url, client_id, cert, key)
    else:
        form["client_secret"] = ctx.secret("client_secret")
    from urllib.parse import urlencode
    http = HTTP(egress=ctx.egress, timeout=30)
    r = http.post(url, data=urlencode(form).encode(), headers={"Content-Type": "application/x-www-form-urlencoded"}, raise_for_status=False)
    body = r.json() if r.body else {}
    if r.status != 200 or not body.get("access_token"):
        raise ActionError("auth_failed", f"token request failed: HTTP {r.status} {body.get('error', '')}", retryable=r.status >= 500)
    return body["access_token"]


def _assertion(url, client_id, cert_pem, key_pem):
    try:
        from cryptography import x509
        from cryptography.hazmat.primitives import hashes, serialization
        from cryptography.hazmat.primitives.asymmetric import padding
    except ImportError:
        raise ActionError("unsupported", "certificate credentials need the cryptography package in the runner image")
    cert = x509.load_pem_x509_certificate(cert_pem.encode())
    key = serialization.load_pem_private_key(key_pem.encode(), password=None)
    b64 = lambda b: base64.urlsafe_b64encode(b).rstrip(b"=").decode()
    thumb = cert.fingerprint(hashes.SHA1())
    now = int(time.time())
    header = b64(json.dumps({"alg": "RS256", "typ": "JWT", "x5t": b64(thumb)}).encode())
    claims = b64(json.dumps({"aud": url, "iss": client_id, "sub": client_id, "jti": str(uuid.uuid4()), "nbf": now, "exp": now + 600}).encode())
    sig = key.sign(f"{header}.{claims}".encode(), padding.PKCS1v15(), hashes.SHA256())
    return f"{header}.{claims}.{b64(sig)}"


def _graph(ctx):
    base = ctx.connection.get("graph_url", GRAPH).rstrip("/") + "/v1.0"
    return HTTP(base, headers={"Authorization": "Bearer " + _token(ctx)}, egress=ctx.egress, timeout=min(60, ctx.remaining()))


# ---- mailbox scope ----------------------------------------------------------------

def _allowed(ctx, mailbox):
    if not mailbox or "@" not in mailbox and len(mailbox) < 8:
        raise ActionError("invalid_params", f"{mailbox!r} is not a mailbox (UPN or id)")
    scope = ctx.connection.get("mailbox_scope") or {}
    m = mailbox.lower()
    for d in scope.get("deny") or []:
        d = str(d).lower()
        if m == d or (d.startswith("@") and m.endswith(d)):
            raise ActionError("mailbox_denied", f"{mailbox} is outside this connection's mailbox scope (deny list)")
    allow = scope.get("allow", "all")
    if allow != "all" and isinstance(allow, list):
        if not any(m == str(a).lower() or (str(a).startswith("@") and m.endswith(str(a).lower())) for a in allow):
            raise ActionError("mailbox_denied", f"{mailbox} is not in this connection's allow list")
    return quote(mailbox)


def _mailbox(ctx):
    return ctx.param("mailbox") or ctx.connection.get("default_mailbox") or ""


# ---- actions ----------------------------------------------------------------------

def connection_test(ctx):
    mb = ctx.connection.get("default_mailbox")
    g = _graph(ctx)
    if not mb:
        return {"ok": True, "token": "acquired", "note": "set default_mailbox to also test mailbox access"}
    r = g.get(f"/users/{_allowed(ctx, mb)}/mailFolders/inbox", params={"$select": "displayName,totalItemCount"}).json()
    return {"ok": True, "mailbox": mb, "inbox_items": r.get("totalItemCount")}


SELECT = "id,internetMessageId,subject,from,sender,toRecipients,receivedDateTime,hasAttachments,parentFolderId,webLink,bodyPreview"


def mail_get(ctx):
    mb = _allowed(ctx, ctx.param("mailbox", required=True))
    mid = quote(ctx.param("message_id", required=True))
    sel = SELECT + ",body" + (",internetMessageHeaders" if ctx.param("include_headers", True) else "")
    g = _graph(ctx)
    msg = g.get(f"/users/{mb}/messages/{mid}", params={"$select": sel}).json()
    if msg.get("hasAttachments"):
        atts = g.get(f"/users/{mb}/messages/{mid}/attachments", params={"$select": "id,name,contentType,size,isInline"}).json()
        msg["attachments"] = atts.get("value", [])
    return msg


def mail_search(ctx):
    boxes = list(ctx.param("mailboxes", []) or [])
    if ctx.param("mailbox"):
        boxes.append(ctx.param("mailbox"))
    if not boxes:
        raise ActionError("invalid_params", "give mailbox or mailboxes")
    cap = int(ctx.connection.get("max_mailboxes_per_run", 500))
    if len(boxes) > cap:
        raise ActionError("limit_exceeded", f"{len(boxes)} mailboxes exceeds the per-run cap of {cap}")
    params = {"$select": SELECT, "$top": ctx.param("top", 50)}
    filters = []
    if ctx.param("from"):
        filters.append(f"from/emailAddress/address eq '{_q(ctx.param('from'))}'")
    if ctx.param("internet_message_id"):
        filters.append(f"internetMessageId eq '{_q(ctx.param('internet_message_id'))}'")
    if ctx.param("received_after"):
        filters.append(f"receivedDateTime ge {ctx.param('received_after')}")
    if ctx.param("subject"):
        filters.append(f"contains(subject, '{_q(ctx.param('subject'))}')")
    if ctx.param("query"):
        params["$search"] = '"' + ctx.param("query").replace('"', "") + '"'
    elif filters:
        params["$filter"] = " and ".join(filters)
    g = _graph(ctx)
    found, errors = [], []
    for b in boxes:
        try:
            r = g.get(f"/users/{_allowed(ctx, b)}/messages", params=params).json()
            for m in r.get("value", []):
                m["mailbox"] = b
                found.append(m)
        except ActionError as e:
            if e.retryable:
                raise  # throttling: retry the whole action later
            errors.append({"mailbox": b, "code": e.code, "message": e.message})
    return {"messages": found, "mailboxes_searched": len(boxes), "errors": errors}


def _q(s):
    return str(s).replace("'", "''")


def get_attachment(ctx):
    mb = _allowed(ctx, ctx.param("mailbox", required=True))
    a = _graph(ctx).get(f"/users/{mb}/messages/{quote(ctx.param('message_id', required=True))}/attachments/{quote(ctx.param('attachment_id', required=True))}").json()
    data = base64.b64decode(a.get("contentBytes") or "")
    return {"name": a.get("name"), "content_type": a.get("contentType"), "size": len(data), "sha256": hashlib.sha256(data).hexdigest(),
            "content_base64": a.get("contentBytes", "")}


def mark_read(ctx):
    mb = _allowed(ctx, ctx.param("mailbox", required=True))
    read = bool(ctx.param("read", True))
    if ctx.dry_run:
        return {"dry_run": True, "would_mark_read": read}
    _graph(ctx).patch(f"/users/{mb}/messages/{quote(ctx.param('message_id', required=True))}", json_body={"isRead": read})
    return {"is_read": read}


def move(ctx):
    mb = _allowed(ctx, ctx.param("mailbox", required=True))
    folder = ctx.param("folder", required=True)
    if ctx.dry_run:
        return {"dry_run": True, "would_move_to": folder}
    r = _graph(ctx).post(f"/users/{mb}/messages/{quote(ctx.param('message_id', required=True))}/move", json_body={"destinationId": folder}).json()
    return {"moved": True, "new_id": r.get("id"), "folder": folder}


def reply(ctx):
    mb = _allowed(ctx, ctx.param("mailbox", required=True))
    if ctx.dry_run:
        return {"dry_run": True, "would_reply": True}
    _graph(ctx).post(f"/users/{mb}/messages/{quote(ctx.param('message_id', required=True))}/reply", json_body={"comment": ctx.param("comment", required=True)})
    return {"replied": True}


def send(ctx):
    mailbox = _mailbox(ctx)
    if not mailbox:
        raise ActionError("invalid_params", "give mailbox or set default_mailbox on the connection")
    mb = _allowed(ctx, mailbox)
    to, cc, bcc = ctx.param("to", []), ctx.param("cc", []), ctx.param("bcc", [])
    if not (to or cc or bcc):
        raise ActionError("invalid_params", "at least one recipient is required")
    rcpt = lambda lst: [{"emailAddress": {"address": a}} for a in lst]
    size = int(ctx.param("batch_size", 100))
    bccs = [bcc[i:i + size] for i in range(0, len(bcc), size)] or [[]]
    total = len(to) + len(cc) + len(bcc)
    if ctx.dry_run:
        return {"dry_run": True, "would_send": len(bccs), "recipients": total}
    g = _graph(ctx)
    for chunk in bccs:
        msg = {"subject": ctx.param("subject", required=True),
               "body": {"contentType": "HTML" if ctx.param("html") else "Text", "content": ctx.param("body", required=True)},
               "toRecipients": rcpt(to), "ccRecipients": rcpt(cc), "bccRecipients": rcpt(chunk)}
        g.post(f"/users/{mb}/sendMail", json_body={"message": msg, "saveToSentItems": True}, expected=(202,))
    return {"sent": True, "messages": len(bccs), "recipients": total, "from": mailbox}


def delete(ctx):
    mb = _allowed(ctx, ctx.param("mailbox", required=True))
    mid = ctx.param("message_id", required=True)
    if ctx.dry_run:
        return {"dry_run": True, "would_delete": mid}
    r = _graph(ctx).post(f"/users/{mb}/messages/{quote(mid)}/move", json_body={"destinationId": "deleteditems"}).json()
    return {"deleted": True, "new_id": r.get("id")}


def purge(ctx):
    mb = _allowed(ctx, ctx.param("mailbox", required=True))
    mid = ctx.param("message_id", required=True)
    if ctx.dry_run:
        return {"dry_run": True, "would_purge": mid}
    # *verify*: permanentDelete is available on messages in Graph v1.0 for recent tenants
    _graph(ctx).post(f"/users/{mb}/messages/{quote(mid)}/permanentDelete", expected=(204,))
    return {"purged": True}
