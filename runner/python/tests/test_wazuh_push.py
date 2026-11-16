"""The Wazuh Integrator script (integrations/wazuh/push/custom-soar.py), run the way
wazuh-integratord runs it: <alert_file> <api_key> <hook_url> [<options_file>] [debug],
from an integrations/ directory next to logs/integrations.log.
"""
import json
import os
import shutil
import subprocess
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest

HERE = os.path.dirname(os.path.abspath(__file__))
SCRIPT = os.path.abspath(os.path.join(HERE, "..", "..", "..", "integrations", "wazuh", "push", "custom-soar.py"))
SAMPLE = os.path.abspath(os.path.join(HERE, "..", "..", "..", "integrations", "wazuh", "testdata", "05_sshd_auth_success.json"))


class Ingest(BaseHTTPRequestHandler):
    answers = []  # status codes to give, in order; then 202
    seen = []

    def log_message(self, *_):
        pass

    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        Ingest.seen.append((self.path, self.headers.get("Authorization"), self.headers.get("Content-Type"), body))
        code = Ingest.answers.pop(0) if Ingest.answers else 202
        self.send_response(code)
        self.end_headers()
        self.wfile.write(b'{"detail":"x"}' if code != 202 else b"{}")


@pytest.fixture()
def ingest():
    Ingest.answers, Ingest.seen = [], []
    srv = ThreadingHTTPServer(("127.0.0.1", 0), Ingest)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    yield "http://127.0.0.1:%d/ingest/wazuh-prod" % srv.server_address[1]
    srv.shutdown()


@pytest.fixture()
def wazuh(tmp_path):
    """A /var/ossec look-alike: integrations/custom-soar.py and logs/."""
    (tmp_path / "integrations").mkdir()
    (tmp_path / "logs").mkdir()
    shutil.copy(SCRIPT, tmp_path / "integrations" / "custom-soar.py")
    alert = tmp_path / "1731400000-123.alert"
    shutil.copy(SAMPLE, alert)
    return tmp_path, str(alert)


def run(wazuh, url, *extra, options=None):
    root, alert = wazuh
    args = [sys.executable, str(root / "integrations" / "custom-soar.py"), alert, "tok-secret-123", url]
    if options is not None:
        opt = root / "1731400000-123.options"
        opt.write_text(json.dumps(options))
        args.append(str(opt))
    args += list(extra)
    p = subprocess.run(args, capture_output=True, text=True, timeout=60)
    log_file = root / "logs" / "integrations.log"
    return p.returncode, (log_file.read_text() if log_file.exists() else "")


def test_delivers_the_alert_with_the_token(wazuh, ingest):
    code, log = run(wazuh, ingest)
    assert code == 0, log
    path, auth, ctype, body = Ingest.seen[0]
    assert path == "/ingest/wazuh-prod" and auth == "Bearer tok-secret-123" and ctype == "application/json"
    with open(SAMPLE, encoding="utf-8") as f:
        assert json.loads(body) == json.load(f)
    assert log == ""  # quiet on success unless debug


def test_retries_queue_full_then_succeeds(wazuh, ingest):
    Ingest.answers = [503, 503]
    code, log = run(wazuh, ingest, "debug", options={"backoff": 0.05, "retries": 2})
    assert code == 0, log
    assert len(Ingest.seen) == 3
    assert "retrying" in log and "accepted by SOAR (attempt 3)" in log


def test_bad_token_fails_at_once_and_never_logs_it(wazuh, ingest):
    Ingest.answers = [401]
    code, log = run(wazuh, ingest, options={"backoff": 0.05})
    assert code == 1 and len(Ingest.seen) == 1
    assert "NOT delivered" in log and "HTTP 401" in log and "SOAR_SOURCE_TOKEN_WAZUH_PROD" in log
    assert "tok-secret-123" not in log


def test_unreachable_soar_is_logged(wazuh):
    code, log = run(wazuh, "http://127.0.0.1:1/ingest/wazuh-prod", options={"backoff": 0.01, "retries": 1, "timeout": 2})
    assert code == 1 and "NOT delivered" in log and "after 2 attempt(s)" in log
