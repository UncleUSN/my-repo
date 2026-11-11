#!/usr/bin/env bash
# Запуск агента в WSL. Подставляет IP Windows-хоста в переменную WIN_HOST,
# которую читает ferry.wsl.yaml (https://${WIN_HOST}:7420).
set -euo pipefail
cd "$(dirname "$0")"

if [[ -z "${WIN_HOST:-}" ]]; then
  WIN_HOST="$(ip route show default 2>/dev/null | awk '{print $3; exit}')"
fi
if [[ -z "${WIN_HOST:-}" ]]; then
  echo "Не удалось определить IP Windows. Задайте его вручную: WIN_HOST=172.x.x.x $0" >&2
  exit 1
fi
export WIN_HOST
echo "WIN_HOST=${WIN_HOST} (если ping до Windows не проходит, а в .wslconfig включён mirrored, попробуйте WIN_HOST=127.0.0.1)"
exec ./ferry serve --config "${1:-./ferry.yaml}"
