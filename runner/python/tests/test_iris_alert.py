"""iris.alert.create against a fake DFIR-IRIS 2.4 over HTTPS with a self-signed certificate
for 127.0.0.1 (what a default IRIS docker install presents).

Checks the exact API calls: certificate verification on by default, verify_tls: false for a
lab instance (with a warning), the /alerts/add body (IOCs, assets, event time, tags), id
lists fetched once, and unknown IOC or asset types skipped and reported instead of failing.
"""
import json
import os
import shutil
import ssl
import subprocess
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.dirname(HERE))  # soar_sdk
sys.path.insert(0, HERE)

from soar_sdk import ActionContext, ActionError  # noqa: E402
from test_integrations import ROOT, load  # noqa: E402

IRIS = load(os.path.join(ROOT, "dfir_iris", "actions", "iris.py"))

LISTS = {
    "/manage/ioc-types/list": [{"type_id": 76, "type_name": "ip-any"}, {"type_id": 3, "type_name": "account"},
                               {"type_id": 113, "type_name": "sha256"}],  # no "hostname": skipped
    "/manage/tlp/list": [{"tlp_id": 1, "tlp_name": "red"}, {"tlp_id": 2, "tlp_name": "amber"}],
    "/manage/asset-type/list": [{"asset_id": 9, "asset_name": "Windows - Computer"}, {"asset_id": 3, "asset_name": "Linux - Server"}],
}


class FakeIris(BaseHTTPRequestHandler):
    calls = []

    def log_message(self, *_):
        pass

    def _reply(self, data, status=200):
        body = json.dumps({"status": "success", "message": "", "data": data}).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        FakeIris.calls.append(("GET", self.path, self.headers.get("Authorization"), None))
        if self.path in LISTS:
            return self._reply(LISTS[self.path])
        self._reply({}, 404)

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))) or b"{}")
        FakeIris.calls.append(("POST", self.path, self.headers.get("Authorization"), body))
        if self.path == "/alerts/add":
            return self._reply({"alert_id": 42})
        self._reply({}, 404)


@pytest.fixture(scope="module")
def iris(tmp_path_factory):
    if not shutil.which("openssl"):
        pytest.skip("openssl is needed to make the test certificate")
    d = tmp_path_factory.mktemp("iris")
    cert, key = str(d / "cert.pem"), str(d / "key.pem")
    subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-keyout", key, "-out", cert,
                    "-days", "1", "-subj", "/CN=127.0.0.1"], check=True, capture_output=True)
    srv = ThreadingHTTPServer(("127.0.0.1", 0), FakeIris)
    tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    tls.load_cert_chain(cert, key)
    srv.socket = tls.wrap_socket(srv.socket, server_side=True)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    yield "https://127.0.0.1:%d" % srv.server_address[1]
    srv.shutdown()


def ctx(base_url, params, verify=None, logs=None):
    conn = {"base_url": base_url, "customer_id": 3}
    if verify is not None:
        conn["verify_tls"] = verify
    return ActionContext({"action_id": "iris.alert.create", "params": params, "connection": conn,
                          "secrets": {"api_key": "iris-key"}, "limits": {"timeout_s": 10}},
                         (logs if logs is not None else []).append)


WAZUH_PARAMS = {
    "title": "[Wazuh] sshd: brute force trying to get access to the system",
    "description": "Rule 5763 level 10 on agent web01",
    "severity": 4, "source": "Wazuh", "source_ref": "1731400000.12345",
    "source_content": {"rule": {"id": "5763", "level": 10}},
    "event_time": "2026-11-12T10:00:00.000Z",
    "tags": ["sshd", "authentication_failures", "T1110", "sshd"],
    "iocs": [{"type": "ip", "value": "203.0.113.7", "tlp": "amber", "description": "source"},
             {"type": "user", "value": "root"},
             {"type": "host", "value": "web01"},          # IRIS here has no hostname type: skipped
             {"type": "ip", "value": ""}],                 # empty: ignored
    "assets": [{"name": "web01", "type": "Linux - Server", "ip": "10.10.192.50", "description": "Wazuh agent 001"},
               {"name": "db01", "type": "Mainframe"}],     # unknown asset type: skipped
}


def test_certificate_is_verified_by_default(iris):
    with pytest.raises(ActionError) as e:
        IRIS.alert_create(ctx(iris, {"title": "x"}))
    assert e.value.code == "network" and "CERTIFICATE_VERIFY_FAILED" in e.value.message


def test_alert_create_sends_the_wazuh_alert(iris):
    FakeIris.calls.clear()
    logs = []
    out = IRIS.alert_create(ctx(iris, WAZUH_PARAMS, verify=False, logs=logs))
    assert out["alert_id"] == 42 and out["iocs"] == 2 and out["assets"] == 1
    assert len(out["skipped"]) == 2 and any("web01" in s for s in out["skipped"]) and any("db01" in s for s in out["skipped"])
    assert any("not verified" in line for line in logs), logs

    gets = [c[1] for c in FakeIris.calls if c[0] == "GET"]
    assert sorted(gets) == sorted(set(gets)), "each id list is fetched once"
    posts = [c for c in FakeIris.calls if c[0] == "POST"]
    assert len(posts) == 1 and posts[0][1] == "/alerts/add" and posts[0][2] == "Bearer iris-key"
    body = posts[0][3]
    assert body["alert_title"] == WAZUH_PARAMS["title"]
    assert body["alert_severity_id"] == 5  # SOAR 4 (high) -> IRIS 5
    assert body["alert_customer_id"] == 3 and body["alert_status_id"] == 2
    assert body["alert_source"] == "Wazuh" and body["alert_source_ref"] == "1731400000.12345"
    assert body["alert_source_event_time"] == "2026-11-12T10:00:00.000Z"
    assert body["alert_source_content"] == {"rule": {"id": "5763", "level": 10}}
    assert body["alert_tags"] == "sshd,authentication_failures,T1110"
    assert body["alert_iocs"] == [
        {"ioc_value": "203.0.113.7", "ioc_type_id": 76, "ioc_tlp_id": 2, "ioc_description": "source", "ioc_tags": ""},
        {"ioc_value": "root", "ioc_type_id": 3, "ioc_tlp_id": 2, "ioc_description": "", "ioc_tags": ""},
    ]
    assert body["alert_assets"] == [
        {"asset_name": "web01", "asset_type_id": 3, "asset_ip": "10.10.192.50", "asset_description": "Wazuh agent 001", "asset_tags": ""},
    ]


def test_verify_tls_accepts_a_string(iris):
    out = IRIS.alert_create(ctx(iris, {"title": "x"}, verify="false"))
    assert out["alert_id"] == 42


def test_dry_run_calls_nothing(iris):
    FakeIris.calls.clear()
    c = ctx(iris, {"title": "x"}, verify=False)
    c.dry_run = True
    assert IRIS.alert_create(c) == {"dry_run": True, "would_create": "x"}
    assert FakeIris.calls == []
