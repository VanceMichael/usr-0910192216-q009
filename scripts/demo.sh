#!/usr/bin/env bash
# 端到端演示：先确保栈已启动，再执行 fixtures/分账事件.json 中的完整场景。
set -euo pipefail
cd "$(dirname "$0")/.."

"$(dirname "$0")/up.sh"

# 等待应用可服务
for i in $(seq 1 60); do
  if curl -fsS -o /dev/null http://localhost:8080/readyz 2>/dev/null; then
    break
  fi
  sleep 1
done

python3 "$(dirname "$0")/demo.py"
