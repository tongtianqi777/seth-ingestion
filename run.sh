#!/usr/bin/env bash
# Entry point: builds seth-ingestion if needed and starts it.
# Any arguments are passed through, e.g. ./run.sh -log-level DEBUG
set -euo pipefail

cd "$(dirname "$0")"

if ! command -v go >/dev/null 2>&1; then
  echo "Go is required (brew install go)." >&2
  exit 1
fi

# Rebuild when any source file is newer than the binary.
if [[ ! -x bin/seth-ingestion ]] || [[ -n "$(find . \( -name '*.go' -o -name '*.sql' -o -name 'go.*' \) -newer bin/seth-ingestion | head -1)" ]]; then
  echo "Building seth-ingestion..."
  CGO_ENABLED=1 go build -o bin/seth-ingestion .
fi

exec ./bin/seth-ingestion "$@"
