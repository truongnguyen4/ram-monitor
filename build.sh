#!/usr/bin/env bash
# Build ram-monitor for Linux and Windows (amd64) into dist/.
# Uses the Go installed on the host: $GO, else `go` in PATH, else ~/.local/go/bin/go.
set -euo pipefail
cd "$(dirname "$0")"

GO="${GO:-$(command -v go || true)}"
[[ -n $GO ]] || GO="$HOME/.local/go/bin/go"
if [[ ! -x $GO ]]; then
    echo "Go not found: install it (https://go.dev/dl/) into ~/.local/go or add it to PATH, or set GO=/path/to/go" >&2
    exit 1
fi
echo "Using $("$GO" version) ($GO)"

VERSION="${VERSION:-$(date +%Y%m%d)}"
mkdir -p dist
export CGO_ENABLED=0

GOOS=linux   GOARCH=amd64 "$GO" build -trimpath -ldflags "-s -w" -o "dist/ram-monitor-linux-amd64" .
GOOS=windows GOARCH=amd64 "$GO" build -trimpath -ldflags "-s -w" -o "dist/ram-monitor-windows-amd64.exe" .

ls -lh dist
echo "Build $VERSION done."
