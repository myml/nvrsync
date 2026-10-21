#!/usr/bin/env bash
# 校验「设置存 config.json、其余存 SQLite」的迁移结果
set -euo pipefail
ADDR=${ADDR:-http://127.0.0.1:8099}
DATA=${DATA:-/root/Src/nvr-sync/data}

echo "== 文件"
ls -lh "$DATA"/config.json "$DATA"/nvrsync.db 2>/dev/null || true
[ -f "$DATA/store.json.migrated" ] && echo "legacy: store.json.migrated"

echo "== config.json 只含设置"
python3 - "$DATA/config.json" <<'PY'
import json,sys
d=json.load(open(sys.argv[1]))
print("keys:", sorted(d.keys()))
assert "segments" not in d and "logs" not in d, "config.json 不应包含 segments/logs"
print("ok: settings only")
PY

echo "== SQLite 表"
python3 - "$DATA/nvrsync.db" <<'PY'
import sqlite3,sys
c=sqlite3.connect(sys.argv[1])
tabs=[r[0] for r in c.execute("select name from sqlite_master where type='table' order by name")]
print("tables:", tabs)
for t in tabs:
    print(" ", t, c.execute(f"select count(*) from {t}").fetchone()[0], "rows")
PY

echo "== API"
curl -s "$ADDR/api/health"; echo
curl -s "$ADDR/api/segments?limit=1" | python3 -c "import sys,json;d=json.load(sys.stdin)['data'];print('segments total:',d['total'])"
curl -s "$ADDR/api/days?ch=1" | python3 -c "import sys,json;print('days:',len(json.load(sys.stdin)['data']))"
curl -s "$ADDR/api/logs?limit=3" | python3 -c "import sys,json;print('logs:',len(json.load(sys.stdin)['data']))"
