"""SDK for SOAR integration actions.

An action is a function ``def run(ctx: ActionContext) -> dict`` (any JSON value).
Raise ``ActionError(code, message, retryable=...)`` for expected failures. The
platform handles retries, timeouts, idempotency and auth secrets; connector code
only maps params to the remote API and returns normalized output.
"""

from __future__ import annotations

import base64
import fnmatch
import json
import socket
import ssl
import time
import urllib.error
import urllib.parse
import urllib.request
from typing import Any, Callable, Dict, Iterable, List, Optional

__all__ = ["ActionError", "ActionContext", "Response", "HTTP"]


class ActionError(Exception):
    """An expected action failure. ``code`` follows the platform's error codes
    (network, timeout, http_429, http_5xx, http_404, invalid_params, not_found, ...)."""

    def __init__(self, code: str, message: str = "", retryable: bool = False):
        super().__init__(message or code)
        self.code = code
        self.message = message or code
        self.retryable = retryable


class Response:
    def __init__(self, status: int, headers: Dict[str, str], body: bytes):
        self.status = status
        self.headers = headers
        self.body = body

    def json(self) -> Any:
        if not self.body:
            return None
        return json.loads(self.body.decode("utf-8"))

    @property
    def text(self) -> str:
        return self.body.decode("utf-8", errors="replace")


class HTTP:
    """Small HTTP client with egress allowlist, timeouts, platform error mapping and
    optional custom CA. Built on urllib so integrations need no third-party packages."""

    def __init__(self, base_url: str = "", headers: Optional[Dict[str, str]] = None,
                 egress: Optional[List[str]] = None, timeout: float = 30.0,
                 ca_pem: Optional[str] = None,
                 log: Optional[Callable[[str], None]] = None,
                 verify_tls: bool = True):
        self.base_url = base_url.rstrip("/")
        self.headers = dict(headers or {})
        self.egress = egress or ["*"]
        self.timeout = timeout
        self.log = log or (lambda _m: None)
        self.verify_tls = verify_tls
        # Certificates are verified unless the connection says verify_tls: false (lab use: a
        # self-signed server certificate); a private CA is supplied as ca_pem.
        self._ssl = ssl.create_default_context(cadata=ca_pem) if ca_pem else ssl.create_default_context()
        if not verify_tls:
            self._ssl.check_hostname = False
            self._ssl.verify_mode = ssl.CERT_NONE

    def _allowed(self, host: str) -> bool:
        return any(p == "*" or fnmatch.fnmatch(host.lower(), p.lower()) for p in self.egress)

    def request(self, method: str, path: str, *, params: Optional[Dict[str, Any]] = None,
                json_body: Any = None, data: Optional[bytes] = None,
                headers: Optional[Dict[str, str]] = None, expected: Iterable[int] = (),
                raise_for_status: bool = True) -> Response:
        url = path if path.startswith(("http://", "https://")) else self.base_url + "/" + path.lstrip("/")
        if params:
            sep = "&" if "?" in url else "?"
            url += sep + urllib.parse.urlencode({k: v for k, v in params.items() if v is not None}, doseq=True)
        host = urllib.parse.urlparse(url).hostname or ""
        if not self._allowed(host):
            raise ActionError("egress_denied", f"{host} is not in this action's egress allowlist")
        if not self.verify_tls and url.startswith("https://"):
            self.log(f"WARNING: TLS certificate of {host} is not verified (connection verify_tls: false)")
        h = dict(self.headers)
        h.update(headers or {})
        body = data
        if json_body is not None:
            body = json.dumps(json_body).encode("utf-8")
            h.setdefault("Content-Type", "application/json")
        req = urllib.request.Request(url, data=body, method=method.upper(), headers=h)
        try:
            with urllib.request.urlopen(req, timeout=self.timeout, context=self._ssl) as r:
                resp = Response(r.status, dict(r.headers.items()), r.read())
        except urllib.error.HTTPError as e:
            resp = Response(e.code, dict(e.headers.items()) if e.headers else {}, e.read() if e.fp else b"")
        except socket.timeout:
            raise ActionError("timeout", f"{method} {host} timed out", retryable=True)
        except urllib.error.URLError as e:
            if isinstance(e.reason, socket.timeout):
                raise ActionError("timeout", f"{method} {host} timed out", retryable=True)
            raise ActionError("network", f"{method} {host}: {e.reason}", retryable=True)
        except (ConnectionError, OSError) as e:
            raise ActionError("network", f"{method} {host}: {e}", retryable=True)
        if raise_for_status and resp.status not in expected:
            if resp.status == 429:
                raise ActionError("http_429", f"rate limited by {host} (Retry-After: {resp.headers.get('Retry-After', '')})", retryable=True)
            if resp.status >= 500:
                raise ActionError("http_5xx", f"{host} answered HTTP {resp.status}", retryable=True)
            if resp.status >= 400:
                snippet = resp.text[:300]
                raise ActionError(f"http_{resp.status}", f"{host} answered HTTP {resp.status}: {snippet}")
        return resp

    def get(self, path: str, **kw: Any) -> Response:
        return self.request("GET", path, **kw)

    def post(self, path: str, **kw: Any) -> Response:
        return self.request("POST", path, **kw)

    def put(self, path: str, **kw: Any) -> Response:
        return self.request("PUT", path, **kw)

    def patch(self, path: str, **kw: Any) -> Response:
        return self.request("PATCH", path, **kw)

    def delete(self, path: str, **kw: Any) -> Response:
        return self.request("DELETE", path, **kw)


class ActionContext:
    """What an action receives. Secrets are available only through ``secret()``."""

    def __init__(self, req: Dict[str, Any], log: Callable[[str], None]):
        self.action_id: str = req.get("action_id", "")
        self.params: Dict[str, Any] = req.get("params") or {}
        self.connection: Dict[str, Any] = req.get("connection") or {}
        self.dry_run: bool = bool(req.get("dry_run"))
        self.idempotency_key: str = req.get("idempotency_key", "")
        self.execution_id: str = req.get("execution_id", "")
        self.node_id: str = req.get("node_id", "")
        self.attempt: int = int(req.get("attempt") or 1)
        self.egress: List[str] = (req.get("limits") or {}).get("egress") or ["*"]
        self.timeout_s: float = float((req.get("limits") or {}).get("timeout_s") or 60)
        self._secrets: Dict[str, str] = req.get("secrets") or {}
        self._log = log
        self.started = time.monotonic()

    def log(self, message: str) -> None:
        self._log(str(message))

    def secret(self, name: str, required: bool = True) -> str:
        v = self._secrets.get(name, "")
        if required and not v:
            raise ActionError("missing_secret", f"the connection has no secret named {name!r}")
        return v

    def param(self, name: str, default: Any = None, required: bool = False) -> Any:
        if name in self.params and self.params[name] is not None:
            return self.params[name]
        if required:
            raise ActionError("invalid_params", f"parameter {name!r} is required")
        return default

    def remaining(self) -> float:
        return max(1.0, self.timeout_s - (time.monotonic() - self.started) - 1.0)

    def http(self, base_url: Optional[str] = None, headers: Optional[Dict[str, str]] = None,
             auth: Optional[str] = None) -> HTTP:
        """An HTTP client for the connection. ``auth`` is one of bearer, basic, api_key_header,
        or None (no auth header). Base URL, CA and auth come from the connection config."""
        cfg = self.connection
        h = dict(headers or {})
        if auth == "bearer":
            h["Authorization"] = "Bearer " + self.secret(cfg.get("token_secret", "api_key"))
        elif auth == "basic":
            user = cfg.get("username") or self.secret("username", required=False)
            pw = self.secret(cfg.get("password_secret", "password"))
            h["Authorization"] = "Basic " + base64.b64encode(f"{user}:{pw}".encode()).decode()
        elif auth == "api_key_header":
            h[cfg.get("api_key_header", "X-API-Key")] = self.secret(cfg.get("token_secret", "api_key"))
        verify = cfg.get("verify_tls", True)
        if isinstance(verify, str):
            verify = verify.strip().lower() not in ("false", "0", "no", "off")
        return HTTP(base_url or cfg.get("base_url", ""), headers=h, egress=self.egress,
                    timeout=min(self.remaining(), float(cfg.get("timeout_s", 30))),
                    ca_pem=cfg.get("ca_pem"), log=self._log, verify_tls=bool(verify))
