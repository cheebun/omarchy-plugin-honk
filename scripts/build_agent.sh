#!/usr/bin/env bash
# Build honk-agent into bin/, where BarWidget.qml looks for it.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root/agent"
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$root/bin/honk-agent" .
