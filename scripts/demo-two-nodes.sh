#!/usr/bin/env bash
# Два агента на 127.0.0.1: передача в обе стороны и задание watch.
# Запуск: make demo   (или bash scripts/demo-two-nodes.sh после make build)
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="${BIN:-$ROOT/bin/ferry}"
if [[ ! -x "$BIN" ]]; then
  echo "нет $BIN — выполните make build" >&2
  exit 1
fi

TMP="$(mktemp -d)"
PID_A=""
PID_B=""
cleanup() {
  if [[ -n "${PID_A}" ]]; then kill "$PID_A" 2>/dev/null || true; fi
  if [[ -n "${PID_B}" ]]; then kill "$PID_B" 2>/dev/null || true; fi
  if [[ -n "${PID_A}" ]]; then wait "$PID_A" 2>/dev/null || true; fi
  if [[ -n "${PID_B}" ]]; then wait "$PID_B" 2>/dev/null || true; fi
  rm -rf "$TMP"
}
trap cleanup EXIT

TOKEN="demo-token-not-for-production"
mkdir -p "$TMP/a/manual" "$TMP/a/outbox" "$TMP/a/sent" "$TMP/a/inbox" "$TMP/a/archive" \
  "$TMP/b/manual" "$TMP/b/outbox" "$TMP/b/sent" "$TMP/b/inbox" "$TMP/b/archive" \
  "$TMP/b/certs" "$TMP/a/certs"

"$BIN" cert --dir "$TMP/a/certs" --cn node-a >/dev/null
"$BIN" cert --dir "$TMP/b/certs" --cn node-b >/dev/null

cat >"$TMP/a/ferry.yaml" <<EOF
node:
  id: node-a
  listen: "127.0.0.1:17420"
  token: "${TOKEN}"
  data_dir: "${TMP}/a/data"
  scenarios: "${TMP}/a/scenarios.yaml"
  allowed_roots:
    - "${TMP}/a"
  inbound:
    - name: inbox
      path: "${TMP}/a/inbox"
    - name: archive
      path: "${TMP}/a/archive"
  tls:
    cert_file: "${TMP}/a/certs/cert.pem"
    key_file: "${TMP}/a/certs/key.pem"
peers:
  - id: node-b
    url: "https://127.0.0.1:17421"
    token: "${TOKEN}"
    tls_skip_verify: true
EOF

cat >"$TMP/a/scenarios.yaml" <<EOF
scenarios:
  - name: fanout-to-b
    watch: true
    settle: 300ms
    interval: 200ms
    sources:
      - path: "${TMP}/a/outbox"
        include: ["*"]
    destinations:
      - peer: node-b
        alias: inbox
      - peer: node-b
        alias: archive
    on_success: move
    success_policy: all
    archive_dir: "${TMP}/a/sent"
    retry:
      max_attempts: 3
      initial_backoff: 200ms
      max_backoff: 1s
EOF

cat >"$TMP/b/ferry.yaml" <<EOF
node:
  id: node-b
  listen: "127.0.0.1:17421"
  token: "${TOKEN}"
  data_dir: "${TMP}/b/data"
  scenarios: "${TMP}/b/scenarios.yaml"
  allowed_roots:
    - "${TMP}/b"
  inbound:
    - name: inbox
      path: "${TMP}/b/inbox"
    - name: archive
      path: "${TMP}/b/archive"
  tls:
    cert_file: "${TMP}/b/certs/cert.pem"
    key_file: "${TMP}/b/certs/key.pem"
peers:
  - id: node-a
    url: "https://127.0.0.1:17420"
    token: "${TOKEN}"
    tls_skip_verify: true
EOF

cat >"$TMP/b/scenarios.yaml" <<EOF
scenarios:
  - name: back-to-a
    watch: true
    settle: 300ms
    interval: 200ms
    sources:
      - path: "${TMP}/b/outbox"
        include: ["*"]
    destinations:
      - peer: node-a
        alias: inbox
    on_success: move
    success_policy: all
    archive_dir: "${TMP}/b/sent"
EOF

"$BIN" serve --config "$TMP/a/ferry.yaml" >"$TMP/a.log" 2>&1 &
PID_A=$!
"$BIN" serve --config "$TMP/b/ferry.yaml" >"$TMP/b.log" 2>&1 &
PID_B=$!

wait_ping() {
  local cfg="$1" peer="$2"
  for _ in $(seq 1 50); do
    if "$BIN" ping --config "$cfg" --peer "$peer" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.1
  done
  echo "узел $peer не ответил" >&2
  echo "--- log a ---" >&2
  cat "$TMP/a.log" >&2 || true
  echo "--- log b ---" >&2
  cat "$TMP/b.log" >&2 || true
  return 1
}

wait_ping "$TMP/a/ferry.yaml" node-b
wait_ping "$TMP/b/ferry.yaml" node-a

echo "from A" >"$TMP/a/manual/from-a.txt"
"$BIN" send --config "$TMP/a/ferry.yaml" --peer node-b \
  --src "$TMP/a/manual/from-a.txt" --dst "$TMP/b/inbox/from-a.txt"

echo "from B" >"$TMP/b/manual/from-b.txt"
"$BIN" send --config "$TMP/b/ferry.yaml" --peer node-a \
  --src "$TMP/b/manual/from-b.txt" --dst "$TMP/a/inbox/from-b.txt"

echo "watched" >"$TMP/a/outbox/auto.txt"
echo "back" >"$TMP/b/outbox/back.txt"
for _ in $(seq 1 50); do
  if [[ -f "$TMP/b/inbox/auto.txt" && -f "$TMP/b/archive/auto.txt" && -f "$TMP/a/sent/auto.txt" && ! -f "$TMP/a/outbox/auto.txt" \
    && -f "$TMP/a/inbox/back.txt" && -f "$TMP/b/sent/back.txt" ]]; then
    break
  fi
  sleep 0.1
done

fail=0
cmp -s "$TMP/a/manual/from-a.txt" "$TMP/b/inbox/from-a.txt" || { echo "A→B не совпало" >&2; fail=1; }
cmp -s "$TMP/b/manual/from-b.txt" "$TMP/a/inbox/from-b.txt" || { echo "B→A не совпало" >&2; fail=1; }
cmp -s <(printf 'watched\n') "$TMP/b/inbox/auto.txt" || { echo "watch не доставил файл в inbox" >&2; fail=1; }
cmp -s <(printf 'watched\n') "$TMP/b/archive/auto.txt" || { echo "watch не доставил файл в archive" >&2; fail=1; }
cmp -s <(printf 'back\n') "$TMP/a/inbox/back.txt" || { echo "обратный сценарий не доставил файл" >&2; fail=1; }
[[ -f "$TMP/a/sent/auto.txt" ]] || { echo "watch не перенёс исходник в sent" >&2; fail=1; }

if [[ "$fail" -ne 0 ]]; then
  echo "--- log a ---" >&2
  cat "$TMP/a.log" >&2 || true
  echo "--- log b ---" >&2
  cat "$TMP/b.log" >&2 || true
  exit 1
fi

echo "demo ok: A→B, B→A, fan-out inbox+archive, back to A"
