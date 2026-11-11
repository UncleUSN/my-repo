#!/usr/bin/env bash
# Пример установки агента службой systemd. Запускать от root на сервере, не в WSL-демо.
# Перед запуском положите конфиг в /etc/ferry/ferry.yaml и сертификаты рядом.
set -euo pipefail

PREFIX="${PREFIX:-/usr/local/bin}"
CONFIG_DIR="${CONFIG_DIR:-/etc/ferry}"
DATA_DIR="${DATA_DIR:-/var/lib/ferry}"
BIN_SRC="${BIN_SRC:-dist/ferry-linux-amd64}"

if [[ "$(id -u)" -ne 0 ]]; then
  echo "нужен root" >&2
  exit 1
fi
if [[ ! -f "$BIN_SRC" ]]; then
  echo "нет файла $BIN_SRC — сначала make cross" >&2
  exit 1
fi

install -d -m 0755 "$PREFIX" "$CONFIG_DIR"
install -m 0755 "$BIN_SRC" "$PREFIX/ferry"
if ! id ferry >/dev/null 2>&1; then
  useradd --system --home "$DATA_DIR" --shell /usr/sbin/nologin ferry
fi
install -d -m 0750 -o ferry -g ferry "$DATA_DIR"
if [[ ! -f "$CONFIG_DIR/ferry.yaml" ]]; then
  echo "Положите конфиг в $CONFIG_DIR/ferry.yaml и сертификаты, затем: systemctl restart ferry"
fi

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
install -m 0644 "$SCRIPT_DIR/../examples/ferry.service" /etc/systemd/system/ferry.service
systemctl daemon-reload
systemctl enable ferry
echo "Дальше: systemctl start ferry && systemctl status ferry"
