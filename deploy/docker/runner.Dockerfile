# syntax=docker/dockerfile:1
# Sandboxed Python runner (spec section 10). Build context: repository root.
FROM python:3.12-slim-bookworm
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 PIP_NO_CACHE_DIR=1 PIP_DISABLE_PIP_VERSION_CHECK=1
WORKDIR /app
COPY runner/python/requirements.txt /app/requirements.txt
RUN pip install -r /app/requirements.txt && useradd --system --uid 10001 --home /nonexistent --shell /usr/sbin/nologin runner
COPY runner/python/soar_runner /app/soar_runner
COPY runner/python/soar_sdk /app/soar_sdk
# Integration packages: manifests and action code. Only the actions/ folders and manifests are needed.
COPY integrations /app/integrations
COPY config/filters /app/scripts/filters
USER 10001:10001
EXPOSE 50051
# Read-only root filesystem: the child processes only write to /tmp (mount a tmpfs there).
ENTRYPOINT ["python", "-m", "soar_runner.server", "--listen", "0.0.0.0:50051", "--integrations", "/app/integrations", "--scripts", "/app/scripts"]
