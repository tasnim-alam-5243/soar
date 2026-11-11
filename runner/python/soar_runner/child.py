"""Sandboxed child process: runs exactly one action call or one filter batch.

Started by server.py as ``python -I child.py`` with an empty environment. It reads
one JSON request from stdin, applies resource limits *before* importing any
integration code, runs the function and writes JSON lines to the original stdout:
``{"log": ...}`` while running and one final ``{"result": ...}`` or ``{"failure": ...}``.
Anything the integration prints goes to the log, never into the protocol stream.
"""

import importlib.util
import io
import json
import math
import os
import sys
import traceback

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.dirname(HERE))  # runner root: soar_sdk

_proto = os.fdopen(os.dup(1), "w", buffering=1, encoding="utf-8")


def emit(obj):
    _proto.write(json.dumps(obj, default=str) + "\n")
    _proto.flush()


class _LogWriter(io.TextIOBase):
    def __init__(self):
        self._buf = ""

    def write(self, s):
        self._buf += s
        while "\n" in self._buf:
            line, self._buf = self._buf.split("\n", 1)
            if line.strip():
                emit({"log": line[:4000]})
        return len(s)


def apply_limits(limits):
    try:
        import resource
    except ImportError:  # not on Linux: limits come from the container only
        return
    mem = int(limits.get("memory_mb") or 256)
    mem = max(mem, 64) * 1024 * 1024
    timeout = int(limits.get("timeout_s") or 60)
    # limits.cpu is a share of one core (manifest "cpu: 0.5"). A per-process core share
    # needs cgroups, which the runner container does not have, so it is enforced as a
    # CPU-time budget: an action may burn at most cpu x timeout CPU seconds, then the
    # kernel stops it (SIGXCPU -> "cpu_limit"). Without cpu, the budget is the timeout.
    cpu = float(limits.get("cpu") or 0)
    cpu_seconds = timeout + 1 if cpu <= 0 else max(1, int(math.ceil(cpu * timeout)))
    for lim, val in (
        (resource.RLIMIT_AS, mem),
        (resource.RLIMIT_CPU, cpu_seconds),
        (resource.RLIMIT_CORE, 0),
        (resource.RLIMIT_FSIZE, 64 * 1024 * 1024),
        (resource.RLIMIT_NOFILE, 256),
    ):
        try:
            soft, hard = resource.getrlimit(lim)
            if hard != resource.RLIM_INFINITY and val > hard:
                val = hard
            resource.setrlimit(lim, (val, hard if hard != resource.RLIM_INFINITY else val))
        except (ValueError, OSError):
            pass


def load_function(path, func):
    spec = importlib.util.spec_from_file_location("soar_action_" + os.path.basename(path).replace(".", "_"), path)
    if spec is None or spec.loader is None:
        raise RuntimeError(f"cannot load {path}")
    mod = importlib.util.module_from_spec(spec)
    sys.path.insert(0, os.path.dirname(path))  # sibling helper modules of the package
    spec.loader.exec_module(mod)
    fn = getattr(mod, func, None)
    if not callable(fn):
        raise RuntimeError(f"{path} has no function {func}()")
    return fn


def main():
    req = json.loads(sys.stdin.read())
    apply_limits(req.get("limits") or {})
    sys.stdout = _LogWriter()
    sys.stderr = sys.stdout

    from soar_sdk import ActionContext, ActionError

    try:
        fn = load_function(req["path"], req["function"])
        if req.get("mode") == "filter":
            out = fn(req.get("events") or [])
            if not isinstance(out, list) or len(out) != len(req.get("events") or []):
                raise ActionError("invalid_output", "a filter must return one decision per event")
            emit({"result": [str(d) for d in out]})
            return
        ctx = ActionContext(req, lambda m: emit({"log": str(m)[:4000]}))
        out = fn(ctx)
    except ActionError as e:
        emit({"failure": {"code": e.code, "message": e.message, "retryable": bool(e.retryable)}})
        return
    except MemoryError:
        emit({"failure": {"code": "memory_limit", "message": "the action exceeded its memory limit", "retryable": False}})
        return
    except Exception as e:  # noqa: BLE001 - every error must become a failure, never a hang
        tb = traceback.format_exc(limit=5)
        emit({"log": tb[-3000:]})
        emit({"failure": {"code": "action_error", "message": f"{type(e).__name__}: {e}"[:2000], "retryable": False}})
        return
    try:
        json.dumps(out)
    except (TypeError, ValueError) as e:
        emit({"failure": {"code": "invalid_output", "message": f"output is not JSON-serialisable: {e}", "retryable": False}})
        return
    emit({"result": out})


if __name__ == "__main__":
    main()
