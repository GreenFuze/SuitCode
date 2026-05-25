#!/usr/bin/env bash
# install.sh — install SuitCode binaries from the Go module registry
#
# Usage:
#   ./install.sh           install suitcode, coordinator (with tray), investigator
#   ./install.sh --ci      CGo-free build (no tray icon, safe for headless servers)
#
# The coordinator binary includes the system-tray icon by default.
# Use --ci only for servers or build agents where no desktop is available.
#
# Linux prerequisite for the tray icon:
#   sudo apt install libayatana-appindicator3-dev   # Debian/Ubuntu
#   sudo dnf install libayatana-appindicator3-devel # Fedora
#
# One-liner (no clone needed):
#   curl -sSfL https://raw.githubusercontent.com/GreenFuze/SuitCode/main/install.sh | sh

set -euo pipefail

REPO="github.com/GreenFuze/SuitCode"
CI=0

for arg in "$@"; do
  case "$arg" in
    --ci)   CI=1 ;;
    --tray)
      echo "Note: --tray is no longer needed. The coordinator binary now includes"
      echo "      the system-tray icon. Continuing with default install."
      ;;
  esac
done

# Verify Go is available.
if ! command -v go >/dev/null 2>&1; then
  echo "error: 'go' not found in PATH — install Go 1.21+ from https://go.dev/dl/" >&2
  exit 1
fi

GO_VER=$(go version | awk '{print $3}' | sed 's/go//')
echo "Using Go $GO_VER"

# Core binaries.
echo ""
echo "Installing core binaries..."

if [ "$CI" -eq 1 ]; then
  echo "  [headless mode: no tray icon]"
  CGO_ENABLED=0 go install "${REPO}/suitcode@latest"
  CGO_ENABLED=0 go install "${REPO}/coordinator@latest"
  CGO_ENABLED=0 go install "${REPO}/investigator@latest"
else
  # Linux requires AppIndicator for the systray build — warn but don't abort
  # so users without the library get a clear message.
  if [ "$(uname -s)" = "Linux" ]; then
    echo "  Note (Linux): the tray icon requires libayatana-appindicator3."
    echo "  If this build fails, run first:"
    echo "    sudo apt install libayatana-appindicator3-dev   # Debian/Ubuntu"
    echo "    sudo dnf install libayatana-appindicator3-devel # Fedora"
    echo "  Then re-run this script. Or use --ci for a headless build."
    echo ""
  fi

  go install "${REPO}/suitcode@latest"
  go install -tags systray "${REPO}/coordinator@latest"
  go install "${REPO}/investigator@latest"
fi

echo "  suitcode       ✓"
echo "  coordinator    ✓  (tray icon included)"
echo "  investigator   ✓"

# PATH reminder.
GOBIN="${GOPATH:-$HOME/go}/bin"
echo ""
echo "Done! Binaries are in: $GOBIN"
if ! echo "$PATH" | tr ':' '\n' | grep -qx "$GOBIN"; then
  echo ""
  echo "  ⚠  $GOBIN is not in your PATH."
  echo "  Add this to your shell profile (~/.bashrc, ~/.zshrc, etc.):"
  echo "    export PATH=\"\$PATH:$GOBIN\""
fi
echo ""
echo "Get started:"
echo "  coordinator                       # start the coordinator (shows tray icon)"
echo "  suitcode . warmup"
echo "  suitcode . context --files <file> --budget 8000"
