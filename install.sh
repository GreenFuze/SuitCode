#!/usr/bin/env bash
# install.sh — install SuitCode binaries from the Go module registry
#
# Usage:
#   ./install.sh           install suitcode, coordinator, investigator
#   ./install.sh --tray    also install the desktop tray icon
#   ./install.sh --ci      CGo-free build (no tray, safe for headless servers)
#
# One-liner (no clone needed):
#   curl -sSfL https://raw.githubusercontent.com/GreenFuze/SuitCode/main/install.sh | sh
#   curl -sSfL https://raw.githubusercontent.com/GreenFuze/SuitCode/main/install.sh | sh -s -- --tray

set -euo pipefail

REPO="github.com/GreenFuze/SuitCode"
TRAY=0
CI=0

for arg in "$@"; do
  case "$arg" in
    --tray) TRAY=1 ;;
    --ci)   CI=1   ;;
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
  CGO_ENABLED=0 go install "${REPO}/suitcode@latest"
  CGO_ENABLED=0 go install "${REPO}/coordinator@latest"
  CGO_ENABLED=0 go install "${REPO}/investigator@latest"
else
  go install "${REPO}/suitcode@latest"
  go install "${REPO}/coordinator@latest"
  go install "${REPO}/investigator@latest"
fi

echo "  suitcode       ✓"
echo "  coordinator    ✓"
echo "  investigator   ✓"

# Optional tray icon.
if [ "$TRAY" -eq 1 ]; then
  echo ""
  echo "Installing desktop tray icon..."

  # Linux requires AppIndicator — warn but don't abort.
  if [ "$(uname -s)" = "Linux" ]; then
    echo "  Note (Linux): AppIndicator is required."
    echo "  If the build fails, run first:"
    echo "    sudo apt install libayatana-appindicator3-dev   # Debian/Ubuntu"
    echo "    sudo dnf install libayatana-appindicator3-devel # Fedora"
  fi

  go install -tags systray "${REPO}/tray@latest"
  echo "  suitcode-tray  ✓"
fi

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
echo "  suitcode . warmup          # pre-warm for your current project"
echo "  suitcode . context --files <file> --budget 8000"
