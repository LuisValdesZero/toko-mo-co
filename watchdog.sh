#!/bin/bash
# Toko-Mo-Co watchdog — restarts the proxy if it goes down.
# Usage: ./watchdog.sh &

DIR="$(cd "$(dirname "$0")" && pwd)"
BINARY="$DIR/tokomoco"
PORT=8081
CHECK_INTERVAL=5
LOG="$DIR/watchdog.log"

echo "[WATCHDOG] started at $(date)" >> "$LOG"

while true; do
    if ! lsof -ti:$PORT > /dev/null 2>&1; then
        echo "[WATCHDOG] $(date) — port $PORT down, restarting tokomoco" >> "$LOG"
        cd "$DIR" && "$BINARY" >> "$DIR/tokomoco.log" 2>&1 &
        sleep 2
        if lsof -ti:$PORT > /dev/null 2>&1; then
            echo "[WATCHDOG] $(date) — tokomoco restarted successfully (PID $(lsof -ti:$PORT))" >> "$LOG"
        else
            echo "[WATCHDOG] $(date) — failed to restart tokomoco" >> "$LOG"
        fi
    fi
    sleep $CHECK_INTERVAL
done
