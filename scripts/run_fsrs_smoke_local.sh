#!/usr/bin/env bash
# Run the same FSRS acceptance test used after deployment against a local server.
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
port="${E2E_PORT:-18083}"
database_url="${DATABASE_URL:-postgres://kelma:kelma@localhost:5433/kelma_sync_test?sslmode=disable}"
database_name="$(python3 - "$database_url" <<'PY'
import sys
import urllib.parse
print(urllib.parse.urlparse(sys.argv[1]).path.lstrip("/"))
PY
)"
if [[ "$database_name" != *_test && "${ALLOW_FSRS_SMOKE_DATABASE:-}" != 1 ]]; then
  echo "Refusing to run against non-test database '$database_name'." >&2
  echo "Set ALLOW_FSRS_SMOKE_DATABASE=1 only for an intentional disposable database." >&2
  exit 1
fi
tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/kelma-fsrs-smoke.XXXXXX")"
server_pid=""

cleanup() {
  if [[ -n "$server_pid" ]] && kill -0 "$server_pid" 2>/dev/null; then
    kill "$server_pid" 2>/dev/null || true
    wait "$server_pid" 2>/dev/null || true
  fi
  rm -rf "$tmp_dir"
}
trap cleanup EXIT

cd "$repo_root"
go build -o "$tmp_dir/kelma-sync2" ./cmd/server
PORT="$port" \
DATABASE_URL="$database_url" \
MIGRATIONS_DIR="$repo_root/migrations" \
KELMA_AUTH_MODE=local \
KELMA_INTERNAL_SECRET=local-fsrs-smoke-secret \
MEDIA_DIR="$tmp_dir/media" \
  "$tmp_dir/kelma-sync2" >"$tmp_dir/server.log" 2>&1 &
server_pid=$!

ready=0
for _ in $(seq 1 100); do
  if python3 - "$port" <<'PY'
import sys
import urllib.request
try:
    with urllib.request.urlopen(f"http://127.0.0.1:{sys.argv[1]}/health", timeout=0.5) as response:
        raise SystemExit(0 if response.status == 200 else 1)
except Exception:
    raise SystemExit(1)
PY
  then
    ready=1
    break
  fi
  if ! kill -0 "$server_pid" 2>/dev/null; then
    echo "Local FSRS smoke server exited before becoming ready." >&2
    tail -40 "$tmp_dir/server.log" >&2
    exit 1
  fi
  sleep 0.2
done
if [[ "$ready" != 1 ]]; then
  echo "Local FSRS smoke server did not become ready." >&2
  tail -40 "$tmp_dir/server.log" >&2
  exit 1
fi

KELMA_SYNC_SMOKE_URL="http://127.0.0.1:$port" \
KELMA_SYNC_INTERNAL_SECRET=local-fsrs-smoke-secret \
DEPLOY_REVISION=local \
  python3 "$repo_root/scripts/fsrs_postdeploy_smoke.py"

KELMA_SYNC_MONITOR_URL="http://127.0.0.1:$port" \
KELMA_SYNC_INTERNAL_SECRET=local-fsrs-smoke-secret \
  python3 "$repo_root/scripts/check_projection_health.py"
