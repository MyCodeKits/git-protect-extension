#!/bin/sh
# genc installer for macOS / Linux (per-user).
# Usage: sh scripts/install.sh [path-to-genc-binary]
set -e
root=$(cd "$(dirname "$0")/.." && pwd)
src=${1:-$root/bin/genc}
if [ ! -x "$src" ]; then
    echo "Building genc..."
    (cd "$root" && go build -o "$src" .)
fi
dest=${GENC_INSTALL_DIR:-$HOME/.local/bin}
mkdir -p "$dest"
cp "$src" "$dest/genc"
# the same binary acts as the git remote helper when named git-remote-genc
cp "$src" "$dest/git-remote-genc"
chmod 755 "$dest/genc" "$dest/git-remote-genc"
case ":$PATH:" in
    *":$dest:"*) ;;
    *) echo "Add $dest to your PATH (e.g. in ~/.profile): export PATH=\"$dest:\$PATH\"" ;;
esac
echo "Installed genc to $dest. Restart your IDE, then run: genc doctor"
