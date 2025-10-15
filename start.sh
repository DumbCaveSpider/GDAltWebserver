#!/bin/sh
# start.sh — load .env (if present) and run the Go server
# POSIX-compatible shell script.

set -eu

# Load .env into the environment for the duration of this script
if [ -f .env ]; then
  # export variables defined in .env (simple KEY=VALUE lines)
  set -a
  # shellcheck disable=SC1091
  . ./.env
  set +a
fi

echo "Starting Go server (go run .)"
exec go run .
