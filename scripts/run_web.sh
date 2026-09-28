#!/bin/bash
set -eo pipefail

fuser -k 8080/tcp 8081/tcp 8082/tcp 2>/dev/null || true
pkill -f '[.]bin/gateway' || true
pkill -f '[.]bin/web' || true
pkill -f '[c]md/gateway' || true
pkill -f '[c]md/web' || true
sleep 1

export VA_GATEWAY_PORT=8081
export VA_GATEWAY_URL="http://127.0.0.1:${VA_GATEWAY_PORT}/v1"
export VA_MODEL="${VA_MODEL:-gemini-2.5-flash}"

mkdir -p .bin
go build -o .bin/gateway ./cmd/gateway
go build -o .bin/web ./cmd/web

echo "Starting LiteLLM-compatible gateway on port ${VA_GATEWAY_PORT}..."
./.bin/gateway > gateway.log 2>&1 &
GATEWAY_PID=$!

echo "Starting ADK web UI and agents on port 8080..."
./.bin/web > web.log 2>&1 &
WEB_PID=$!

function cleanup {
    echo "Shutting down..."
    kill $GATEWAY_PID 2>/dev/null || true
    kill $WEB_PID 2>/dev/null || true
}
trap cleanup EXIT

echo "Services running:"
echo "  - ADK Web UI: http://rktop.c.googlers.com:8080/ui/"
echo "  - Gateway:    http://127.0.0.1:${VA_GATEWAY_PORT}/v1"
echo "Press Ctrl+C to stop."
wait
