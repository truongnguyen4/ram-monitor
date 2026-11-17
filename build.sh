#!/usr/bin/env bash
# Build ram-monitor for Linux and Windows (amd64) into dist/.
set -euo pipefail
cd "$(dirname "$0")"

VERSION="${VERSION:-$(date +%Y%m%d)}"
mkdir -p dist
export CGO_ENABLED=0

GOOS=linux   GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o "dist/ram-monitor-linux-amd64" .
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o "dist/ram-monitor-windows-amd64.exe" .

ls -lh dist
echo "Build $VERSION done."
