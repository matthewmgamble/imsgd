#!/usr/bin/env bash
# Runs `imsgd run` as a launchd agent for development, so it gets the
# permissions granted to the installed binary: macOS applies Full Disk Access
# and Automation to the program launchd started, not to the calling terminal.
#
# Usage: scripts/dev-agent.sh start|stop|restart|status|logs
set -euo pipefail

BIN="${IMSGD_BIN:-$HOME/.local/bin/imsgd}"
LABEL="com.mgamble.imsgd.dev"
DOMAIN="gui/$(id -u)"
LOGDIR="$HOME/Library/Logs/imsgd"
PLIST="$LOGDIR/$LABEL.plist"
LOG="$LOGDIR/dev.log"

start() {
  mkdir -p "$LOGDIR"
  cat >"$PLIST" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>$LABEL</string>
  <key>ProgramArguments</key><array><string>$BIN</string><string>run</string></array>
  <key>StandardOutPath</key><string>$LOG</string>
  <key>StandardErrorPath</key><string>$LOG</string>
  <key>RunAtLoad</key><true/>
</dict></plist>
PLIST
  launchctl bootstrap "$DOMAIN" "$PLIST"
  echo "started; logs: $LOG"
}

stop() { launchctl bootout "$DOMAIN/$LABEL" 2>/dev/null && echo stopped || echo "not running"; }

case "${1:-}" in
  start) start ;;
  stop) stop ;;
  restart) stop; sleep 1; start ;;
  status) launchctl print "$DOMAIN/$LABEL" 2>/dev/null | grep -E '^\s+(state|pid|last exit code) =' || echo "not loaded" ;;
  logs) tail -n "${2:-50}" "$LOG" ;;
  *) echo "usage: $0 start|stop|restart|status|logs [n]" >&2; exit 2 ;;
esac
