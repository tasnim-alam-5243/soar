import json
import os

import grpc
import pytest

from soar_runner import runner_pb2 as pb
from soar_runner import runner_pb2_grpc as pbg
from soar_runner.server import Catalog, RunnerService, serve

HERE = os.path.dirname(os.path.abspath(__file__))
INTEGRATIONS = os.path.join(HERE, "fixtures", "integrations")
SCRIPTS = os.path.join(HERE, "fixtures", "scripts")


def call(svc, action, params=None, secrets=None, timeout=10, memory=256, dry_run=False, cpu=0.0):
    req = pb.ExecuteRequest(action_id=action, params_json=json.dumps(params or {}).encode(), secrets=secrets or {},
                            limits=pb.Limits(timeout_s=timeout, memory_mb=memory, cpu=cpu), dry_run=dry_run,
                            connection_json=json.dumps({"base_url": "https://iris.example"}).encode(), attempt=2)

    class Ctx:
        def is_active(self):
            return True

    logs, final = [], None
    for ev in svc.Execute(req, Ctx()):
        kind = ev.WhichOneof("event")
        if kind == "log":
            logs.append(ev.log)
        else:
            final = ev
    return logs, final


@pytest.fixture(scope="module")
def svc():
    return RunnerService(Catalog(INTEGRATIONS), SCRIPTS)


def test_catalog_rejects_path_escape(svc):
    assert "demo.echo" in svc.catalog.actions
    assert "demo.escape" not in svc.catalog.actions


def test_echo(svc):
    logs, final = call(svc, "demo.echo", {"a": 1}, dry_run=True)
    out = json.loads(final.result.output_json)
    assert out == {"params": {"a": 1}, "dry_run": True, "conn": "https://iris.example", "attempt": 2}
    assert "hello from echo" in logs
    assert "printed output goes to the log" in logs


def test_action_error_keeps_code_and_retryable(svc):
    _, final = call(svc, "demo.fail")
    assert final.failure.code == "http_429" and final.failure.retryable


def test_exception_becomes_failure(svc):
    logs, final = call(svc, "demo.crash")
    assert final.failure.code == "action_error" and "boom" in final.failure.message
    assert not final.failure.retryable


def test_timeout_kills_child(svc):
    _, final = call(svc, "demo.sleep", timeout=1)
    assert final.failure.code == "timeout" and final.failure.retryable


def test_cpu_limit_is_a_cpu_time_budget(svc):
    # cpu 0.1 of a core over a 20 s timeout = 2 CPU seconds; a busy loop is stopped
    # long before the wall-clock timeout
    import time
    t0 = time.monotonic()
    _, final = call(svc, "demo.spin", timeout=20, cpu=0.1)
    took = time.monotonic() - t0
    assert final.failure.code == "cpu_limit" and not final.failure.retryable
    assert took < 10, f"the CPU budget must stop the action, not the 20 s timeout (took {took:.1f} s)"


def test_memory_limit(svc):
    _, final = call(svc, "demo.hog", memory=128, timeout=20)
    assert final.failure.code == "memory_limit", final


def test_memory_budget_excludes_the_interpreter(svc):
    # the smallest budget still runs a light action, on any host: the interpreter's own
    # address space (a 220 MB locale archive on RHEL) is not counted against it
    _, final = call(svc, "demo.echo", {"a": 1}, memory=64)
    assert final.WhichOneof("event") == "result", final


def test_secrets_are_scrubbed(svc):
    logs, final = call(svc, "demo.leak", secrets={"api_key": "s3cr3t-value"})
    assert all("s3cr3t-value" not in l for l in logs)
    assert "s3cr3t-value" not in final.failure.message and "***" in final.failure.message


def test_non_json_output(svc):
    _, final = call(svc, "demo.notjson")
    assert final.failure.code == "invalid_output"


def test_child_environment_is_empty(svc):
    os.environ["SOAR_MASTER_KEY"] = "must-not-leak"
    _, final = call(svc, "demo.env")
    keys = json.loads(final.result.output_json)["keys"]
    assert "SOAR_MASTER_KEY" not in keys


def test_unknown_action(svc):
    _, final = call(svc, "nope.nope")
    assert final.failure.code == "unknown_action"


def test_filter(svc):
    resp = svc.Filter(pb.FilterRequest(script="filters/odd.py", events_json=json.dumps([{"n": 1}, {"n": 2}]).encode()), None)
    assert list(resp.decisions) == ["drop", "keep"]
    bad = svc.Filter(pb.FilterRequest(script="../../etc/passwd", events_json=b"[]"), None)
    assert bad.failure.code == "unknown_script"


def test_grpc_round_trip():
    server, port = serve("127.0.0.1:0", INTEGRATIONS, SCRIPTS, 2)
    try:
        with grpc.insecure_channel(f"127.0.0.1:{port}") as ch:
            stub = pbg.RunnerStub(ch)
            h = stub.Health(pb.HealthRequest(), timeout=5)
            assert h.ready and "demo.echo" in h.actions
            events = list(stub.Execute(pb.ExecuteRequest(action_id="demo.echo", params_json=b'{"x": 1}',
                                                         limits=pb.Limits(timeout_s=10)), timeout=20))
            assert events[-1].WhichOneof("event") == "result"
            assert json.loads(events[-1].result.output_json)["params"] == {"x": 1}
    finally:
        server.stop(0)
