#!/bin/sh
set -eu

APP_PORT="${APP_PORT:-8080}"
export APP_PORT
export CPA_BASE_URL="${CPA_BASE_URL:-https://pjpjq-daili.hf.space}"
export REDIS_QUEUE_ADDR="${REDIS_QUEUE_ADDR:-127.0.0.1:9}"
export WORK_DIR="${WORK_DIR:-/data}"
export TZ="${TZ:-Asia/Shanghai}"
export AUTH_ENABLED="${AUTH_ENABLED:-false}"

if [ -z "${CPA_MANAGEMENT_KEY:-}" ] || [ "${CPA_MANAGEMENT_KEY:-}" = "CHANGE_ME" ]; then
  mkdir -p /tmp/placeholder
  cat > /tmp/placeholder/index.html <<'HTML'
<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width,initial-scale=1" />
  <title>Daili Usage Keeper</title>
  <style>
    body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;margin:0;background:#0f172a;color:#e2e8f0;display:grid;place-items:center;min-height:100vh}
    main{max-width:760px;padding:32px;border:1px solid #334155;border-radius:16px;background:#111827;box-shadow:0 20px 60px #0008}
    code{background:#020617;border:1px solid #334155;border-radius:6px;padding:2px 6px;color:#93c5fd}
    a{color:#93c5fd}
    li{margin:.45rem 0}
  </style>
</head>
<body>
  <main>
    <h1>Daili Usage Keeper is deployed</h1>
    <p>The real dashboard is intentionally not started yet because <code>CPA_MANAGEMENT_KEY</code> is not configured.</p>
    <ol>
      <li>Add a Hugging Face Space secret named <code>CPA_MANAGEMENT_KEY</code> with the management key for <code>https://pjpjq-daili.hf.space</code>.</li>
      <li>Restart this Space.</li>
      <li>Keeper will then connect to CPA through <code>/v0/management/usage-queue</code> and store data under <code>/data</code>.</li>
    </ol>
    <p>This placeholder avoids hammering or banning the CPA management endpoint with a wrong key.</p>
  </main>
</body>
</html>
HTML
  exec busybox httpd -f -p "0.0.0.0:${APP_PORT}" -h /tmp/placeholder
fi

exec /usr/local/bin/docker-entrypoint.sh "$@"
