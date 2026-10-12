#!/bin/sh
# git-protect installer for macOS / Linux (per-user).
# Usage: sh scripts/install.sh [path-to-git-protect-binary]
set -e
root=$(cd "$(dirname "$0")/.." && pwd)
src=${1:-$root/bin/git-protect}
if [ ! -x "$src" ]; then
    echo "Building git-protect..."
    (cd "$root" && go build -o "$src" .)
fi
dest=${GIT_PROTECT_INSTALL_DIR:-$HOME/.local/bin}
mkdir -p "$dest"
cp "$src" "$dest/git-protect"
# the same binary acts as the git remote helper when named git-remote-protect
cp "$src" "$dest/git-remote-protect"
chmod 755 "$dest/git-protect" "$dest/git-remote-protect"
case ":$PATH:" in
    *":$dest:"*) ;;
    *) echo "Add $dest to your PATH (e.g. in ~/.profile): export PATH=\"$dest:\$PATH\"" ;;
esac
echo "Installed git-protect to $dest. Restart your IDE, then run: git protect doctor"
