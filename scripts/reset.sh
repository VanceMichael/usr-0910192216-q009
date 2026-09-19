#!/usr/bin/env bash
# 重置整个栈并清空持久卷（演示环境回到初始状态）。
set -euo pipefail
cd "$(dirname "$0")/.."

docker compose --profile audit down -v
echo "stack stopped, volume postgres_data removed"
