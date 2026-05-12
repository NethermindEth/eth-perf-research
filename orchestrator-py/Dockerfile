# Multi-stage build. Builder resolves the wheel + lock; runtime is non-root + slim.
FROM python:3.12-slim AS builder

# git is required for uv to fetch eels-spamoor-builders (a git+https dep).
RUN apt-get update \
    && apt-get install -y --no-install-recommends git \
    && rm -rf /var/lib/apt/lists/* \
    && pip install --no-cache-dir uv

WORKDIR /app
COPY pyproject.toml uv.lock* ./
# Copy source before sync so hatchling can resolve the package during lock.
COPY src/ ./src/
COPY README.md ./
COPY reference_f/ ./reference_f/
RUN uv sync --no-dev --frozen || uv sync --no-dev


FROM python:3.12-slim AS runtime

# Non-root account (fixed UID for predictable file ownership across hosts).
RUN useradd --uid 10000 --no-create-home --shell /sbin/nologin orchestrator \
    && mkdir -p /app/state \
    && chown -R orchestrator:orchestrator /app

WORKDIR /app
COPY --from=builder --chown=orchestrator:orchestrator /app/.venv /app/.venv
COPY --chown=orchestrator:orchestrator src/ /app/src/
COPY --chown=orchestrator:orchestrator reference_f/ /app/reference_f/
COPY --chown=orchestrator:orchestrator target.yaml.example /app/target.yaml.example

ENV PATH="/app/.venv/bin:$PATH" \
    PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    STATE_DIR=/app/state

USER orchestrator

VOLUME ["/app/state", "/run/jwt"]

ENTRYPOINT ["orchestrator"]
