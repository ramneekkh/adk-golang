#!/bin/bash
set -eo pipefail

if [ -z "${VA_WEB_PORT}" ]; then
    if lsof -i :8080 >/dev/null 2>&1; then
        export VA_WEB_PORT=8082
    else
        export VA_WEB_PORT=8080
    fi
fi

fuser -k "${VA_WEB_PORT}/tcp" 8081/tcp 2>/dev/null || true
pkill -f '[.]bin/gateway' || true
pkill -f '[.]bin/web' || true
pkill -f '[c]md/gateway' || true
pkill -f '[c]md/web' || true
sleep 1

export VA_GATEWAY_PORT=8081
export VA_GATEWAY_URL="http://127.0.0.1:${VA_GATEWAY_PORT}/v1"
export VA_MODEL="${VA_MODEL:-gemini-2.5-flash}"
export VA_VERTEX_PROJECT="${VA_VERTEX_PROJECT:-ozone-511309}"

mkdir -p .bin
go build -o .bin/gateway ./cmd/gateway
go build -o .bin/web ./cmd/web

echo "Starting LiteLLM-compatible gateway on port ${VA_GATEWAY_PORT}..."
./.bin/gateway > gateway.log 2>&1 &
GATEWAY_PID=$!

echo "Starting ADK web UI and agents on port ${VA_WEB_PORT}..."
./.bin/web > web.log 2>&1 &
WEB_PID=$!

function cleanup {
    echo "Shutting down..."
    kill $GATEWAY_PID 2>/dev/null || true
    kill $WEB_PID 2>/dev/null || true
}
trap cleanup EXIT

echo "Services running:"
echo "  - ADK Web UI: http://rktop.c.googlers.com:${VA_WEB_PORT}/ui/"
echo "  - Gateway:    http://127.0.0.1:${VA_GATEWAY_PORT}/v1"
echo "Press Ctrl+C to stop."
wait
