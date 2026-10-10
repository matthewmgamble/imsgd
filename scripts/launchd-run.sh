#!/usr/bin/env bash
# Runs the installed imsgd as a one-shot launchd job and prints its output.
#
# Processes launched by launchd are their own "responsible process" for macOS
# privacy (TCC) checks, so this exercises the permissions granted to imsgd
# itself rather than to whatever terminal or tool invoked this script.
#
# Usage: scripts/launchd-run.sh doctor -automation
set -euo pipefail

BIN="${IMSGD_BIN:-$HOME/.local/bin/imsgd}"
LABEL="com.mgamble.imsgd.oneshot.$$"
DOMAIN="gui/$(id -u)"
# launchd must be able to write the log; a mktemp dir from a sandboxed
# caller may not be usable, so use ~/Library/Logs.
dir="$HOME/Library/Logs/imsgd/oneshot"
mkdir -p "$dir"
rm -f "$dir/out.log"
plist="$dir/oneshot.plist"
trap 'launchctl bootout "$DOMAIN/$LABEL" 2>/dev/null || true' EXIT

args="<string>$BIN</string>"
for a in "$@"; do
  # sed rather than ${a//&/...}: in bash 5.2 "&" in a replacement means the match.
  a="$(printf '%s' "$a" | sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g')"
  args+="<string>$a</string>"
done

cat >"$plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>$LABEL</string>
  <key>ProgramArguments</key><array>$args</array>
  <key>WorkingDirectory</key><string>$PWD</string>
  <key>StandardOutPath</key><string>$dir/out.log</string>
  <key>StandardErrorPath</key><string>$dir/out.log</string>
  <key>RunAtLoad</key><true/>
</dict></plist>
PLIST

launchctl bootout "$DOMAIN/$LABEL" 2>/dev/null || true
launchctl bootstrap "$DOMAIN" "$plist"

# Wait until the job has run once and is no longer running (up to 5 minutes).
code=""
for _ in $(seq 1 600); do
  info="$(launchctl print "$DOMAIN/$LABEL" 2>/dev/null || true)"
  runs="$(awk '/^\truns = /{print $3}' <<<"$info")"
  state="$(awk '/^\tstate = /{print $3}' <<<"$info")"
  # While spawning, state is "xpcproxy" and the exit code reads "(never exited)".
  code="$(awk '/^\tlast exit code = /{print $5}' <<<"$info")"
  if [[ "${runs:-0}" -ge 1 && "$state" == "not" && "$code" =~ ^-?[0-9]+$ ]]; then
    break
  fi
  sleep 0.5
done
cat "$dir/out.log" 2>/dev/null || true
[[ "$code" =~ ^-?[0-9]+$ ]] || { echo "launchd-run: job did not finish (exit code: ${code:-unknown})" >&2; exit 1; }
exit "$code"
