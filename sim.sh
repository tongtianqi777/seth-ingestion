#!/usr/bin/env bash
# Builds seth-sim if needed and runs it. seth-sim writes simulated Seth group
# messages to data-sim/, for testing clients without real WhatsApp messages.
#   ./sim.sh seed            a week of history
#   ./sim.sh live            new messages every few seconds, until Ctrl+C
#   ./sim.sh send -group Preschool -text "Pickup is at 3 today"
set -euo pipefail

cd "$(dirname "$0")"

if ! command -v go >/dev/null 2>&1; then
  echo "Go is required (brew install go)." >&2
  exit 1
fi

# Rebuild when any source file is newer than the binary.
if [[ ! -x bin/seth-sim ]] || [[ -n "$(find . \( -name '*.go' -o -name '*.sql' -o -name 'go.*' \) -newer bin/seth-sim | head -1)" ]]; then
  echo "Building seth-sim..." >&2
  CGO_ENABLED=1 go build -o bin/seth-sim ./cmd/seth-sim
fi

exec ./bin/seth-sim "$@"
