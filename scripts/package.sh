#!/bin/sh
# Cross-compiles Git Protect for all supported platforms and packages release archives.
# Usage: sh scripts/package.sh <version> [output-dir]
# Produces <output-dir>/git-protect_<version>_<os>_<arch>.{zip,tar.gz} and SHA256SUMS.txt
set -eu

version=${1:?usage: package.sh <version> [output-dir]}
out=${2:-dist}
root=$(cd "$(dirname "$0")/.." && pwd)
targets="windows/amd64 windows/arm64 darwin/amd64 darwin/arm64 linux/amd64 linux/arm64"

rm -rf "$out"
mkdir -p "$out"
out=$(cd "$out" && pwd)

# zip is not available everywhere (e.g. Git Bash on Windows); fall back to Python
make_zip() { # <archive> <dir-name> (run inside parent dir)
    if command -v zip >/dev/null 2>&1; then
        zip -qr "$1" "$2"
    else
        for py in python3 python; do
            if "$py" -c "" >/dev/null 2>&1; then
                "$py" -m zipfile -c "$1" "$2"
                return
            fi
        done
        echo "need zip or python to create $1" >&2
        return 1
    fi
}

for t in $targets; do
    os=${t%/*}
    arch=${t#*/}
    name="git-protect_${version}_${os}_${arch}"
    stage="$out/$name"
    mkdir -p "$stage"
    exe=""
    [ "$os" = windows ] && exe=".exe"

    echo "building $name"
    (cd "$root" && CGO_ENABLED=0 GOOS=$os GOARCH=$arch \
        go build -trimpath -ldflags "-s -w -X main.version=$version" -o "$stage/git-protect$exe" .)
    # the same binary acts as the git remote helper under this name
    cp "$stage/git-protect$exe" "$stage/git-remote-protect$exe"
    cp "$root/README.md" "$stage/"

    if [ "$os" = windows ]; then
        cp "$root/scripts/install.ps1" "$stage/"
        (cd "$out" && make_zip "$name.zip" "$name")
    else
        cp "$root/scripts/install.sh" "$stage/"
        chmod 755 "$stage/git-protect" "$stage/git-remote-protect" "$stage/install.sh"
        (cd "$out" && tar -czf "$name.tar.gz" "$name")
    fi
    rm -rf "$stage"
done

(cd "$out" && sha256sum git-protect_* > SHA256SUMS.txt)
echo "done:"
ls -l "$out"
