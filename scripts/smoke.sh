#!/usr/bin/env bash
# 本地冒烟测试：起服务 → 配置 NVR → 同步一小段 → 校验 API/文件
set -euo pipefail
BIN=${BIN:-./nvrsync}
ADDR=${ADDR:-http://127.0.0.1:18099}
DATA=${DATA:-/tmp/nvrsync-smoke}
NVR=${NVR:-192.168.0.49:8000}
: "${PASS:?PASS must be set to the NVR administrator password}"
CH=${CH:-1}
DURATION=${DURATION:-120}

rm -rf "$DATA"; mkdir -p "$DATA"
"$BIN" -listen :18099 -data "$DATA" >/tmp/nvrsync-smoke.log 2>&1 &
PID=$!
trap 'kill $PID 2>/dev/null || true' EXIT
sleep 1

j() { curl -sS -H 'Content-Type: application/json' "$@"; }

echo "== health"; j "$ADDR/api/health"; echo
echo "== settings"
j -X PUT "$ADDR/api/settings" -d "{\"nvr_host\":\"$NVR\",\"nvr_user\":\"admin\",\"nvr_pass\":\"$PASS\",\"storage_dir\":\"$DATA\",\"channels\":[$CH],\"auto_minutes\":0,\"backfill_hours\":1}" ; echo
echo "== nvr test"; j -X POST "$ADDR/api/nvr/test" -d '{}'; echo

NOW=$(date +%s); FROM=$((NOW-DURATION-300)); TO=$((NOW-60))
echo "== sync start $FROM..$TO"
j -X POST "$ADDR/api/sync/start" -d "{\"channels\":[$CH],\"from\":$FROM,\"to\":$TO}" ; echo

for i in $(seq 1 60); do
  S=$(j "$ADDR/api/sync/status")
  echo "  $S"
  echo "$S" | grep -q '"running":false' && break
  sleep 2
done

echo "== segments"; j "$ADDR/api/segments?ch=$CH&limit=5" ; echo
echo "== logs"; j "$ADDR/api/logs?limit=5" ; echo
echo "== days"; j "$ADDR/api/days?ch=$CH"; echo
echo "== files"; find "$DATA" -type f | head -10

DAY=$(date +%F)
echo "== playlist"; curl -sS "$ADDR/api/playlist?ch=$CH&date=$DAY" | head -10 || true
