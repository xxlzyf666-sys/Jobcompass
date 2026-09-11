#!/usr/bin/env bash
set -euo pipefail
task_arch="${1:-amd64}"
case "$task_arch" in amd64|arm64) ;; *) echo 'Supported architectures: amd64, arm64' >&2; exit 1 ;; esac
if [ ! -f internal/app/web/vendor/pdf.mjs ]; then
  echo 'Prepare PDF.js assets first: npm ci --ignore-scripts --omit=optional && npm run assets' >&2
  exit 1
fi
mkdir -p bin
CGO_ENABLED=0 GOOS=linux GOARCH="$task_arch" "${GO_BINARY:-go}" build -buildvcs=false -trimpath -ldflags='-s -w' -o "bin/jobcompass-linux-$task_arch" ./cmd/jobcompass
echo "Built bin/jobcompass-linux-$task_arch"
