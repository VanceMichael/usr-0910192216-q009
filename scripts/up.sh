#!/usr/bin/env bash
# 启动整个账本栈：db（健康检查）-> migrate -> seed -> app。
set -euo pipefail
cd "$(dirname "$0")/.."

docker compose up -d --build --wait

echo "stack is up:"
docker compose ps --format 'table {{.Name}}\t{{.Status}}'
