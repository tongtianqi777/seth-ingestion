#!/usr/bin/env bash
# Builds seth-dashboard if needed, runs it, and opens it in the browser.
# The dashboard is a control panel for run.sh (WhatsApp ingestion) and
# feed.sh (the data feed): link WhatsApp with the QR code, start and stop
# both, and choose where the data lives. Press Ctrl+C to stop everything.
#   ./dashboard.sh             open the dashboard at http://127.0.0.1:8090
#   ./dashboard.sh -open=false don't open the browser
set -euo pipefail

cd "$(dirname "$0")"

if ! command -v go >/dev/null 2>&1; then
  echo "Go is required (brew install go)." >&2
  exit 1
fi

# Rebuild when any source file is newer than the binary.
if [[ ! -x bin/seth-dashboard ]] || [[ -n "$(find . \( -name '*.go' -o -name '*.sql' -o -name '*.html' -o -name 'go.*' \) -newer bin/seth-dashboard | head -1)" ]]; then
  echo "Building seth-dashboard..." >&2
  CGO_ENABLED=1 go build -o bin/seth-dashboard ./cmd/seth-dashboard
fi

exec ./bin/seth-dashboard "$@"
