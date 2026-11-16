import os
import time

from soar_sdk import ActionError


def echo(ctx):
    print("printed output goes to the log")
    ctx.log("hello from echo")
    return {"params": ctx.params, "dry_run": ctx.dry_run, "conn": ctx.connection.get("base_url"), "attempt": ctx.attempt}


def fail(ctx):
    raise ActionError("http_429", "slow down", retryable=True)


def crash(ctx):
    raise ValueError("boom")


def sleep(ctx):
    time.sleep(60)


def hog(ctx):
    data = []
    while True:
        data.append(bytearray(50 * 1024 * 1024))


def leak(ctx):
    token = ctx.secret("api_key")
    ctx.log("token is " + token)
    raise ActionError("http_401", "rejected token " + token)


def notjson(ctx):
    return {"x": object()}


def env(ctx):
    return {"keys": sorted(os.environ.keys())}


def spin(ctx):
    n = 0
    while True:  # pure CPU, never sleeps
        n += 1
