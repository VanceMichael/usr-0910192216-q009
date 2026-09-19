#!/usr/bin/env bash
# 故障注入与恢复验证：崩溃结算确认事务，验证回滚、幂等重试与审计序号连续性。
set -euo pipefail
cd "$(dirname "$0")/.."

"$(dirname "$0")/up.sh"

for i in $(seq 1 60); do
  if curl -fsS -o /dev/null http://localhost:8080/readyz 2>/dev/null; then
    break
  fi
  sleep 1
done

python3 "$(dirname "$0")/fault_inject.py"
