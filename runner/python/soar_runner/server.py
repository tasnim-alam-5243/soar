"""gRPC server of the sandboxed runner.

Run: python -m soar_runner.server --listen 0.0.0.0:50051 --integrations /app/integrations

The runner trusts its own manifests, not the caller: the entrypoint of an action is
taken from the integration package on disk, and paths can never leave the package.
"""

from __future__ import annotations

import argparse
import json
import logging
import os
import queue
import signal
import subprocess
import sys
import threading
import time
from concurrent import futures
from typing import Dict, Iterator, List, Optional, Tuple

import grpc

try:
    import yaml  # PyYAML
except ImportError:  # pragma: no cover
    yaml = None

from . import __version__
from . import runner_pb2 as pb
from . import runner_pb2_grpc as pbg

log = logging.getLogger("soar.runner")
CHILD = os.path.join(os.path.dirname(os.path.abspath(__file__)), "child.py")


class Catalog:
    """Actions loaded from <integrations>/<package>/manifest.yaml."""

    def __init__(self, root: str):
        self.root = os.path.abspath(root)
        self.actions: Dict[str, Tuple[str, str, str]] = {}  # id -> (package dir, file, function)
        self.load()

    def load(self) -> None:
        found: Dict[str, Tuple[str, str, str]] = {}
        if not os.path.isdir(self.root) or yaml is None:
            self.actions = found
            return
        for name in sorted(os.listdir(self.root)):
            pkg = os.path.join(self.root, name)
            mpath = os.path.join(pkg, "manifest.yaml")
            if not os.path.isfile(mpath):
                continue
            with open(mpath, encoding="utf-8") as fh:
                m = yaml.safe_load(fh) or {}
            for a in m.get("actions") or []:
                if (a.get("class") or "python") != "python":
                    continue
                entry = a.get("entrypoint") or ""
                file, _, func = entry.partition(":")
                path = safe_join(pkg, file)
                if path and func:
                    found[a["id"]] = (pkg, path, func)
                else:
                    log.warning("action %s has an invalid entrypoint %r", a.get("id"), entry)
        self.actions = found


def safe_join(root: str, rel: str) -> Optional[str]:
    if not rel or os.path.isabs(rel):
        return None
    p = os.path.realpath(os.path.join(root, rel))
    if not p.startswith(os.path.realpath(root) + os.sep) or not os.path.isfile(p):
        return None
    return p


def scrub(text: str, secrets: Dict[str, str]) -> str:
    for v in secrets.values():
        if v and len(v) >= 4:
            text = text.replace(v, "***")
    return text


def child_env() -> Dict[str, str]:
    env = {"PATH": "/usr/local/bin:/usr/bin:/bin", "LANG": "C.UTF-8", "HOME": "/tmp", "PYTHONDONTWRITEBYTECODE": "1"}
    for k in ("HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "SSL_CERT_FILE"):  # egress proxy, if the deployment uses one
        if os.environ.get(k):
            env[k] = os.environ[k]
    return env


def run_child(request: dict, timeout_s: float, secrets: Dict[str, str]) -> Iterator[Tuple[str, object]]:
    """Run child.py and yield ("log", str), then one ("result", value) or ("failure", dict)."""
    proc = subprocess.Popen(
        [sys.executable, "-I", CHILD], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        env=child_env(), cwd="/tmp" if os.path.isdir("/tmp") else None, start_new_session=True, text=True,
    )
    lines: "queue.Queue[Optional[str]]" = queue.Queue()

    def pump():
        assert proc.stdout is not None
        for line in proc.stdout:
            lines.put(line)
        lines.put(None)

    t = threading.Thread(target=pump, daemon=True)
    t.start()
    try:
        assert proc.stdin is not None
        proc.stdin.write(json.dumps(request))
        proc.stdin.close()
    except BrokenPipeError:
        pass

    deadline = time.monotonic() + timeout_s + 2
    final = None
    while True:
        left = deadline - time.monotonic()
        if left <= 0:
            kill(proc)
            yield "failure", {"code": "timeout", "message": f"the action did not finish within {int(timeout_s)} s", "retryable": True}
            return
        try:
            line = lines.get(timeout=min(left, 0.5))
        except queue.Empty:
            continue
        if line is None:
            break
        try:
            msg = json.loads(line)
        except ValueError:
            yield "log", scrub(line.rstrip(), secrets)
            continue
        if "log" in msg:
            yield "log", scrub(str(msg["log"]), secrets)
        elif "result" in msg or "failure" in msg:
            final = msg
    rc = proc.wait(timeout=5)
    if final is not None:
        if "failure" in final:
            f = dict(final["failure"])
            f["message"] = scrub(str(f.get("message", "")), secrets)
            yield "failure", f
        else:
            yield "result", final["result"]
        return
    err = scrub((proc.stderr.read() if proc.stderr else "")[-2000:], secrets)
    if rc in (-signal.SIGXCPU, -signal.SIGKILL):
        yield "failure", {"code": "cpu_limit", "message": "the action exceeded its CPU limit", "retryable": False}
    else:
        yield "failure", {"code": "runner_crash", "message": f"the action process exited with code {rc} without a result. {err}".strip(), "retryable": False}


def kill(proc: subprocess.Popen) -> None:
    try:
        os.killpg(proc.pid, signal.SIGKILL)
    except (ProcessLookupError, PermissionError):
        proc.kill()
    try:
        proc.wait(timeout=5)
    except subprocess.TimeoutExpired:
        pass


class RunnerService(pbg.RunnerServicer):
    def __init__(self, catalog: Catalog, scripts_root: str):
        self.catalog = catalog
        self.scripts_root = os.path.abspath(scripts_root)

    def Execute(self, request: pb.ExecuteRequest, context) -> Iterator[pb.ExecuteEvent]:
        entry = self.catalog.actions.get(request.action_id)
        if entry is None:
            yield pb.ExecuteEvent(failure=pb.Failure(code="unknown_action", message=f"the runner has no action {request.action_id!r}", retryable=False))
            return
        _pkg, path, func = entry
        secrets = dict(request.secrets)
        timeout_s = float(request.limits.timeout_s or 60)
        try:
            params = json.loads(request.params_json or b"{}")
            connection = json.loads(request.connection_json or b"{}")
        except ValueError as e:
            yield pb.ExecuteEvent(failure=pb.Failure(code="invalid_params", message=str(e)))
            return
        child_req = {
            "mode": "action", "path": path, "function": func, "action_id": request.action_id,
            "params": params, "connection": connection, "secrets": secrets, "dry_run": request.dry_run,
            "idempotency_key": request.idempotency_key, "execution_id": request.execution_id, "node_id": request.node_id,
            "attempt": request.attempt or 1,
            "limits": {"memory_mb": request.limits.memory_mb or 256, "timeout_s": int(timeout_s), "cpu": float(request.limits.cpu or 0),
                       "egress": list(request.limits.egress) or ["*"]},
        }
        log.info("execute %s execution=%s node=%s attempt=%s", request.action_id, request.execution_id, request.node_id, request.attempt)
        for kind, value in run_child(child_req, timeout_s, secrets):
            if not context.is_active():
                return
            if kind == "log":
                yield pb.ExecuteEvent(log=str(value))
            elif kind == "result":
                yield pb.ExecuteEvent(result=pb.Result(output_json=json.dumps(value).encode()))
            else:
                f = value  # type: ignore[assignment]
                yield pb.ExecuteEvent(failure=pb.Failure(code=str(f.get("code", "action_error")), message=str(f.get("message", "")), retryable=bool(f.get("retryable"))))

    def Filter(self, request: pb.FilterRequest, context) -> pb.FilterResponse:
        path = safe_join(self.scripts_root, request.script)
        if path is None:
            return pb.FilterResponse(failure=pb.Failure(code="unknown_script", message=f"no script {request.script!r} under the scripts root"))
        try:
            events = json.loads(request.events_json or b"[]")
        except ValueError as e:
            return pb.FilterResponse(failure=pb.Failure(code="invalid_events", message=str(e)))
        if len(events) > 256:
            return pb.FilterResponse(failure=pb.Failure(code="batch_too_large", message="at most 256 events per call"))
        timeout_s = float(request.limits.timeout_s or 10)
        child_req = {"mode": "filter", "path": path, "function": "decide", "events": events,
                     "limits": {"memory_mb": request.limits.memory_mb or 256, "timeout_s": int(timeout_s)}}
        for kind, value in run_child(child_req, timeout_s, {}):
            if kind == "result":
                return pb.FilterResponse(decisions=[str(d) for d in value])  # type: ignore[union-attr]
            if kind == "failure":
                f = value  # type: ignore[assignment]
                return pb.FilterResponse(failure=pb.Failure(code=str(f.get("code")), message=str(f.get("message")), retryable=bool(f.get("retryable"))))
        return pb.FilterResponse(failure=pb.Failure(code="runner_crash", message="no result"))

    def Health(self, request: pb.HealthRequest, context) -> pb.HealthResponse:
        return pb.HealthResponse(ready=True, actions=sorted(self.catalog.actions), version=__version__)


def serve(listen: str, integrations: str, scripts: str, workers: int) -> Tuple[grpc.Server, int]:
    server = grpc.server(futures.ThreadPoolExecutor(max_workers=workers), options=[
        ("grpc.max_receive_message_length", 8 * 1024 * 1024), ("grpc.max_send_message_length", 8 * 1024 * 1024)])
    catalog = Catalog(integrations)
    pbg.add_RunnerServicer_to_server(RunnerService(catalog, scripts), server)
    port = server.add_insecure_port(listen)
    if port == 0:
        raise SystemExit(f"cannot listen on {listen}")
    server.start()
    log.info("runner %s listening on %s with %d actions", __version__, listen.rsplit(":", 1)[0] + f":{port}", len(catalog.actions))
    print(f"READY {port}", flush=True)
    return server, port


def main(argv: Optional[List[str]] = None) -> None:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--listen", default=os.environ.get("SOAR_RUNNER_LISTEN", "0.0.0.0:50051"))
    ap.add_argument("--integrations", default=os.environ.get("SOAR_RUNNER_INTEGRATIONS", "/app/integrations"))
    ap.add_argument("--scripts", default=os.environ.get("SOAR_RUNNER_SCRIPTS", "/app/scripts"))
    ap.add_argument("--workers", type=int, default=int(os.environ.get("SOAR_RUNNER_WORKERS", "16")))
    args = ap.parse_args(argv)
    logging.basicConfig(level=logging.INFO, format='{"time":"%(asctime)s","level":"%(levelname)s","msg":"%(message)s","role":"runner"}')
    server, _ = serve(args.listen, args.integrations, args.scripts, args.workers)
    stop = threading.Event()
    signal.signal(signal.SIGTERM, lambda *_: stop.set())
    signal.signal(signal.SIGINT, lambda *_: stop.set())
    stop.wait()
    server.stop(grace=10).wait()


if __name__ == "__main__":
    main()
