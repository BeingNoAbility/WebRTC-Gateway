#!/bin/bash
NODE_ID="$1"
SIGNAL_URL="$2"
TARGET_PORT="${3:-8089}"

pkill -f "python3 -m http.server" 2>/dev/null || true
pkill -x agent 2>/dev/null || true

nohup python3 -m http.server "$TARGET_PORT" </dev/null >/dev/null 2>&1 &
sleep 0.3

nohup /root/webrtc-testbed/agent -node-id "$NODE_ID" -signal "$SIGNAL_URL" -target "http://127.0.0.1:$TARGET_PORT" </dev/null >/tmp/cf-agent.log 2>&1 &
sleep 0.8

echo READY
exit 0
