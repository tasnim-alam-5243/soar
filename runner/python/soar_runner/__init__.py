"""SOAR sandboxed Python runner (spec section 10).

The gRPC server (server.py) receives one action call at a time per request and runs
the integration code in a child process (child.py) with resource limits, an empty
environment and secrets passed through a pipe. Integration code uses soar_sdk.
"""

__version__ = "0.1.0"
