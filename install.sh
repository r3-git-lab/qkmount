#!/usr/bin/env bash
# Install QKMount for the current user (no root required).
#
#   ./install.sh                 install to ~/.local (or $PREFIX)
#   PREFIX=/usr/local ./install.sh
#   ./install.sh --uninstall     remove installed files
#   ./install.sh --autostart     also enable "start at login"
set -euo pipefail

PREFIX="${PREFIX:-$HOME/.local}"
BIN_SRC="${QKMOUNT_BIN:-}"
AUTOSTART=0
UNINSTALL=0

for arg in "$@"; do
  case "$arg" in
    --uninstall) UNINSTALL=1 ;;
    --autostart) AUTOSTART=1 ;;
    *) echo "unknown option: $arg" >&2; exit 2 ;;
  esac
done

BIN_DST="$PREFIX/bin/qkmount"
ICON_DIR="$PREFIX/share/icons/hicolor/scalable/apps"
ICON_DST="$ICON_DIR/qkmount.svg"
APPS_DIR="$PREFIX/share/applications"
DESKTOP_DST="$APPS_DIR/qkmount.desktop"
AUTOSTART_DST="${XDG_CONFIG_HOME:-$HOME/.config}/autostart/qkmount.desktop"

say() { printf '  %s\n' "$*"; }

update_caches() {
  command -v update-desktop-database >/dev/null 2>&1 && \
    update-desktop-database "$APPS_DIR" >/dev/null 2>&1 || true
  command -v gtk-update-icon-cache >/dev/null 2>&1 && \
    gtk-update-icon-cache -q -t -f "$PREFIX/share/icons/hicolor" >/dev/null 2>&1 || true
}

if [ "$UNINSTALL" = 1 ]; then
  echo "Uninstalling QKMount from $PREFIX"
  rm -f "$BIN_DST" "$ICON_DST" "$DESKTOP_DST" "$AUTOSTART_DST"
  update_caches
  say "removed $BIN_DST"
  say "removed desktop + icon entries"
  say "removed autostart entry (if any)"
  exit 0
fi

# Locate the built binary.
if [ -z "$BIN_SRC" ]; then
  if [ -x "bin/qkmount" ]; then
    BIN_SRC="$(pwd)/bin/qkmount"
  else
    echo "error: no binary found. Run 'make build' first or set QKMOUNT_BIN." >&2
    exit 1
  fi
fi

echo "Installing QKMount"
echo "  prefix: $PREFIX"

mkdir -p "$PREFIX/bin" "$ICON_DIR" "$APPS_DIR"
install -m 0755 "$BIN_SRC" "$BIN_DST"
say "binary    -> $BIN_DST"

if [ -f "data/qkmount.svg" ]; then
  install -m 0644 "data/qkmount.svg" "$ICON_DST"
  say "icon      -> $ICON_DST"
fi

if [ -f "data/qkmount.desktop" ]; then
  install -m 0644 "data/qkmount.desktop" "$DESKTOP_DST"
  say "launcher  -> $DESKTOP_DST"
fi

if [ "$AUTOSTART" = 1 ]; then
  mkdir -p "$(dirname "$AUTOSTART_DST")"
  sed "s|^Exec=.*|Exec=$BIN_DST serve|" data/qkmount.desktop > "$AUTOSTART_DST"
  say "autostart -> $AUTOSTART_DST"
fi

update_caches

echo
echo "Done. If '$PREFIX/bin' is not on your PATH, add it:"
echo "  export PATH=\"$PREFIX/bin:\$PATH\""
echo
echo "Then run:   qkmount ui     (opens the app and starts the daemon)"
echo "Enable at login later with:   qkmount autostart on"
