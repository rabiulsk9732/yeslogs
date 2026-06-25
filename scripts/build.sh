#!/usr/bin/env bash
# Build all NATLog binaries into ./bin (static, CGO-free).
set -euo pipefail

cd "$(dirname "$0")/.."
GO=${GO:-go}
mkdir -p bin

# natlog = unified service (dataplane + control plane); the rest are split-mode
# and operator tools.
for cmd in natlog collector director benchgen pcapreplay pcapsanitize; do
  echo "==> building $cmd"
  CGO_ENABLED=0 "$GO" build -trimpath -ldflags="-s -w" -o "bin/$cmd" "./cmd/$cmd"
done
echo "==> built into $(pwd)/bin"
"$GO" version
