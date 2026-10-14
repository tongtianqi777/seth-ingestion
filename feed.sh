#!/usr/bin/env bash
# Builds seth-feed if needed and runs it. seth-feed serves the recorded
# messages as a read-only REST API with JSON responses, for AI agents.
#   ./feed.sh            serve data/messages.db on http://127.0.0.1:8080
#   ./feed.sh -sim       serve the simulator's data-sim/ instead
#   ./feed.sh -h         all flags
set -euo pipefail

cd "$(dirname "$0")"

if ! command -v go >/dev/null 2>&1; then
  echo "Go is required (brew install go)." >&2
  exit 1
fi

# Rebuild when any source file is newer than the binary.
if [[ ! -x bin/seth-feed ]] || [[ -n "$(find . \( -name '*.go' -o -name '*.sql' -o -name 'go.*' \) -newer bin/seth-feed | head -1)" ]]; then
  echo "Building seth-feed..." >&2
  CGO_ENABLED=1 go build -o bin/seth-feed ./cmd/seth-feed
fi

exec ./bin/seth-feed "$@"
