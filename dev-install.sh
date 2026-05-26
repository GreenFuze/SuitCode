#!/usr/bin/env bash
# dev-install.sh — build and install SuitCode from local source.
#
# Use this during development instead of `go install ./...` so the correct
# build flags are always applied per platform.
#
# Usage:
#   ./dev-install.sh             build + go install all three binaries
#   ./dev-install.sh --restart   also kill any running coordinator and restart it
#   ./dev-install.sh --ci        CGo-free, no tray (headless / CI servers)
#
# What each binary needs:
#   suitcode      — plain build, console subsystem (CLI tool)
#   coordinator   — -tags systray                        (macOS/Linux)
#                   -tags systray -ldflags "-H windowsgui" (Windows: no console)
#   investigator  — plain build

set -euo pipefail

RESTART=0
CI=0
OS="$(uname -s)"

for arg in "$@"; do
  case "$arg" in
    --restart) RESTART=1 ;;
    --ci)      CI=1 ;;
  esac
done

if ! command -v go >/dev/null 2>&1; then
  echo "error: 'go' not found in PATH" >&2
  exit 1
fi

echo "Building and installing from local source..."
echo ""

if [ "$CI" -eq 1 ]; then
  echo "  [headless mode: no tray icon]"
  CGO_ENABLED=0 go install ./suitcode/
  CGO_ENABLED=0 go install ./coordinator/
  CGO_ENABLED=0 go install ./investigator/
else
  # suitcode — plain console binary.
  echo "  go install ./suitcode/"
  go install ./suitcode/

  # coordinator — must include the systray tag.
  # On Windows also needs -H windowsgui to suppress the console window.
  # Without it, launching from Explorer or auto-start opens a terminal.
  if [ "$OS" = "MINGW64_NT"* ] || [ "$OS" = "MSYS_NT"* ] || \
     echo "$OS" | grep -qi "mingw\|msys\|cygwin\|windows"; then
    echo "  go install -tags systray -ldflags \"-H windowsgui\" ./coordinator/"
    go install -tags systray -ldflags "-H windowsgui" ./coordinator/
  else
    echo "  go install -tags systray ./coordinator/"
    go install -tags systray ./coordinator/
  fi

  # investigator — plain console binary.
  echo "  go install ./investigator/"
  go install ./investigator/
fi

GOBIN="${GOPATH:-$HOME/go}/bin"

echo ""
echo "  suitcode       ✓"
if [ "$CI" -eq 1 ]; then
  echo "  coordinator    ✓  (headless — no tray icon)"
else
  echo "  coordinator    ✓  (tray icon included)"
fi
echo "  investigator   ✓"
echo ""

if [ "$RESTART" -eq 1 ]; then
  echo "Restarting coordinator..."

  # Kill any running SuitCode processes. On Windows (Git Bash / MSYS) pkill
  # does not exist or silently does nothing — use taskkill.exe instead.
  if echo "$OS" | grep -qi "mingw\|msys\|cygwin\|windows"; then
    taskkill.exe /F /IM suitcode.exe    2>/dev/null || true
    taskkill.exe /F /IM coordinator.exe 2>/dev/null || true
    taskkill.exe /F /IM investigator.exe 2>/dev/null || true
  else
    pkill -x suitcode    2>/dev/null || true
    pkill -x coordinator 2>/dev/null || true
    pkill -x investigator 2>/dev/null || true
  fi

  sleep 0.5
  "$GOBIN/coordinator" &
  echo "  coordinator restarted from $GOBIN/coordinator"
  echo ""
fi

echo "Done! Binaries are in: $GOBIN"
