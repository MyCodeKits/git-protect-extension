#!/bin/sh
# git-protect installer for macOS / Linux (per-user).
# Release package: sh install.sh
# Source checkout: sh scripts/install.sh [path-to-git-protect-binary]
set -e
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/.." && pwd)
src=${1:-}
if [ -z "$src" ]; then
    if [ -x "$here/git-protect" ]; then
        src="$here/git-protect"        # release package: binary next to this script
    else
        src="$root/bin/git-protect"
        if [ ! -x "$src" ]; then
            echo "Building git-protect..."
            (cd "$root" && go build -o "$src" .)
        fi
    fi
fi
dest=${GIT_PROTECT_INSTALL_DIR:-$HOME/.local/bin}
mkdir -p "$dest"
cp "$src" "$dest/git-protect"
# the same binary acts as the git remote helper when named git-remote-protect
cp "$src" "$dest/git-remote-protect"
chmod 755 "$dest/git-protect" "$dest/git-remote-protect"
# macOS blocks unsigned binaries downloaded with a browser; clear the quarantine flag
if command -v xattr >/dev/null 2>&1; then
    xattr -d com.apple.quarantine "$dest/git-protect" "$dest/git-remote-protect" 2>/dev/null || true
fi
case ":$PATH:" in
    *":$dest:"*) ;;
    *) echo "Add $dest to your PATH (e.g. in ~/.profile): export PATH=\"$dest:\$PATH\"" ;;
esac
echo "Installed git-protect to $dest. Restart your IDE, then run: git protect doctor"
