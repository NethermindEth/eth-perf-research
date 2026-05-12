FROM python:3.13-slim AS base
WORKDIR /app

RUN apt-get update && apt-get install -y --no-install-recommends \
    git protobuf-compiler ca-certificates && \
    rm -rf /var/lib/apt/lists/*

COPY pyworker/pyproject.toml /app/pyworker/pyproject.toml
COPY pyworker/builder_worker /app/pyworker/builder_worker
COPY proto /app/proto
COPY orchestrator-py /app/orchestrator-py

WORKDIR /app/pyworker
RUN make proto

RUN pip install --no-cache-dir protobuf>=4.25 /app/orchestrator-py /app/pyworker

USER 65532:65532
ENTRYPOINT ["python", "-m", "builder_worker"]
