#!/bin/sh
set -eu

APP_PORT="${APP_PORT:-8080}"
export APP_PORT
export CPA_BASE_URL="${CPA_BASE_URL:-https://pjpjq-daili.hf.space}"
# Cross-Space raw TCP/RESP is not reachable; force a fast failed TCP attempt so
# cpa-usage-keeper immediately falls back to CPA /v0/management/usage-queue.
export REDIS_QUEUE_ADDR="${REDIS_QUEUE_ADDR:-127.0.0.1:9}"
export WORK_DIR="${WORK_DIR:-/data}"
export TZ="${TZ:-Asia/Shanghai}"
export AUTH_ENABLED="${AUTH_ENABLED:-true}"
export KEEPER_HF_REPO_ID="${KEEPER_HF_REPO_ID:-pjpjq/daili-usage-state}"
export KEEPER_HF_REPO_TYPE="${KEEPER_HF_REPO_TYPE:-dataset}"
export KEEPER_HF_PATH="${KEEPER_HF_PATH:-usage-keeper/app.db}"
export KEEPER_HF_ROTATE_INTERVAL="${KEEPER_HF_ROTATE_INTERVAL:-60}"
export KEEPER_HF_ROTATE_KEEP="${KEEPER_HF_ROTATE_KEEP:-48}"
export KEEPER_HF_WRITE_LATEST="${KEEPER_HF_WRITE_LATEST:-0}"
export KEEPER_HF_UPLOAD_INTERVAL="${KEEPER_HF_UPLOAD_INTERVAL:-60}"

missing=""
[ -n "${CPA_MANAGEMENT_KEY:-}" ] || missing="$missing CPA_MANAGEMENT_KEY"
if [ "${AUTH_ENABLED}" = "true" ] && [ -z "${LOGIN_PASSWORD:-}" ]; then
  missing="$missing LOGIN_PASSWORD"
fi

if [ -n "$missing" ]; then
  mkdir -p /tmp/placeholder
  cat > /tmp/placeholder/index.html <<HTML
<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"/><meta name="viewport" content="width=device-width,initial-scale=1"/>
<title>Daili Usage Keeper</title>
<style>body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;margin:0;background:#0f172a;color:#e2e8f0;display:grid;place-items:center;min-height:100vh}main{max-width:760px;padding:32px;border:1px solid #334155;border-radius:16px;background:#111827;box-shadow:0 20px 60px #0008}code{background:#020617;border:1px solid #334155;border-radius:6px;padding:2px 6px;color:#93c5fd}</style>
</head><body><main><h1>Daili Usage Keeper is deployed</h1><p>Missing Space secret(s): <code>${missing}</code>.</p><p>Set the secret(s), then restart this Space.</p></main></body></html>
HTML
  exec busybox httpd -f -p "0.0.0.0:${APP_PORT}" -h /tmp/placeholder
fi

mkdir -p "$WORK_DIR"

restore_sqlite_snapshot() {
  if [ -z "${KEEPER_HF_TOKEN:-}" ] || [ -z "${KEEPER_HF_REPO_ID:-}" ] || [ -z "${KEEPER_HF_PATH:-}" ]; then
    echo "keeper hf snapshot restore disabled: missing token/repo/path"
    return 0
  fi
  python3 /app/hf_state_snapshot.py download "$WORK_DIR/app.db" || echo "keeper hf snapshot restore failed"
}

upload_sqlite_snapshot_loop() {
  if [ -z "${KEEPER_HF_TOKEN:-}" ] || [ -z "${KEEPER_HF_REPO_ID:-}" ] || [ -z "${KEEPER_HF_PATH:-}" ]; then
    echo "keeper hf snapshot upload disabled: missing token/repo/path"
    return 0
  fi
  while true; do
    if [ -f "$WORK_DIR/app.db" ] && [ -s "$WORK_DIR/app.db" ]; then
      python3 /app/hf_state_snapshot.py upload "$WORK_DIR/app.db" || echo "keeper hf snapshot upload failed"
    fi
    sleep "${KEEPER_HF_UPLOAD_INTERVAL}"
  done
}

restore_sqlite_snapshot
upload_sqlite_snapshot_loop &

exec /usr/local/bin/docker-entrypoint.sh "$@"
