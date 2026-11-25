#!/bin/sh
# remote-client installer for macOS and Linux. Downloads the
# release binary for this platform, checks it against the release's
# checksums.txt and installs it as ~/.local/bin/remote-client.
#
#   curl -fsSL https://raw.githubusercontent.com/dzunic-systems/remote.network-client/main/scripts/install.sh | sh
#
# REMOTE_CLIENT_VERSION pins a release (e.g. v0.1.0; default: the latest).
# REMOTE_BIN_DIR installs somewhere else (default: ~/.local/bin).
# Releases: https://github.com/dzunic-systems/remote.network-client/releases
set -eu

REPO=https://github.com/dzunic-systems/remote.network-client
if [ -n "${REMOTE_CLIENT_VERSION:-}" ]; then
  BASE="$REPO/releases/download/$REMOTE_CLIENT_VERSION"
else
  BASE="$REPO/releases/latest/download"
fi
BIN_DIR="${REMOTE_BIN_DIR:-$HOME/.local/bin}"

OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$OS" in darwin|linux) ;; *) echo "unsupported OS: $OS (Windows: download remote-client-windows-<arch>.exe from the releases page)" >&2; exit 1 ;; esac
case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac
# latest/download serves the plain names (remote-client-<os>-<arch>) next to the
# version-pinned ones; a pinned release has both too.
ASSET="remote-client-$OS-$ARCH"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
echo "downloading $ASSET from $BASE"
curl -fsSL "$BASE/$ASSET" -o "$TMP/$ASSET"
curl -fsSL "$BASE/checksums.txt" -o "$TMP/checksums.txt"

# Compared by hand: sha256sum -c is missing on macOS and shasum's list
# format differs between versions.
WANT="$(awk -v a="$ASSET" '$2 == a || $2 == "*" a {print $1}' "$TMP/checksums.txt")"
if command -v sha256sum >/dev/null 2>&1; then
  GOT="$(sha256sum "$TMP/$ASSET" | cut -d' ' -f1)"
else
  GOT="$(shasum -a 256 "$TMP/$ASSET" | cut -d' ' -f1)"
fi
if [ -z "$WANT" ] || [ "$WANT" != "$GOT" ]; then
  echo "checksum mismatch for $ASSET: want '$WANT', got '$GOT' — not installed" >&2
  exit 1
fi
echo "checksum OK"

mkdir -p "$BIN_DIR"
install -m 0755 "$TMP/$ASSET" "$BIN_DIR/remote-client"
# A binary fetched by curl carries no quarantine flag, but one copied in
# from a browser download does; clearing it is harmless either way.
[ "$OS" = darwin ] && xattr -d com.apple.quarantine "$BIN_DIR/remote-client" 2>/dev/null || true
echo "installed: $BIN_DIR/remote-client ($("$BIN_DIR/remote-client" --version 2>/dev/null || echo 'version unknown'))"

case ":$PATH:" in
  *":$BIN_DIR:"*) ;;
  *) echo
     echo "note: $BIN_DIR is not on your PATH. Add it, e.g.:"
     echo "  echo 'export PATH=\"$BIN_DIR:\$PATH\"' >> ~/.profile   # then open a new shell" ;;
esac

cat <<'NEXT'

next:
  1. create an API key at https://spine.remote.network (User Settings -> API Keys)
  2. remote-client login       # stores it in ~/.remote/client-api-key (0600)
  3. remote-client serve       # then point your tool at http://127.0.0.1:4141/v1
guide: https://github.com/dzunic-systems/remote.network-client#readme
NEXT
