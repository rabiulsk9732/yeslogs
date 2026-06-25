#!/usr/bin/env bash
# Build the collector binary into ./bin/natflow-collector.
set -euo pipefail

cd "$(dirname "$0")/.."
GO=${GO:-go}

mkdir -p bin
echo "==> building natflow-collector"
CGO_ENABLED=0 "$GO" build -trimpath -ldflags="-s -w" -o bin/natflow-collector ./cmd/collector
echo "==> built: $(pwd)/bin/natflow-collector"
"$GO" version
