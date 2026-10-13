#!/bin/sh
# Builds the npm packages for Git Protect: one package per platform holding the
# native binary, plus the main "git-protect" package that depends on them optionally.
# Usage: sh scripts/npm-build.sh <version> [output-dir]
#   version: e.g. v0.1.0 or 0.1.0
# Publish afterwards with: sh scripts/npm-publish.sh [output-dir]
set -eu

version=${1:?usage: npm-build.sh <version> [output-dir]}
version=${version#v}
out=${2:-npm-dist}
root=$(cd "$(dirname "$0")/.." && pwd)
repo="https://github.com/MyCodeKits/git-protect-extension"

rm -rf "$out"
mkdir -p "$out"
out=$(cd "$out" && pwd)

# Go target -> npm platform/arch
targets="windows/amd64:win32/x64 darwin/amd64:darwin/x64 darwin/arm64:darwin/arm64 linux/amd64:linux/x64 linux/arm64:linux/arm64"

optional=""
for t in $targets; do
    goos=${t%%/*}; rest=${t#*/}; goarch=${rest%%:*}
    np=${t#*:}; nos=${np%/*}; narch=${np#*/}
    name="git-protect-$nos-$narch"
    dir="$out/$name"
    exe=""
    [ "$goos" = windows ] && exe=".exe"
    mkdir -p "$dir/bin"
    echo "building $name"
    (cd "$root" && CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch \
        go build -trimpath -ldflags "-s -w -X main.version=v$version" -o "$dir/bin/git-protect$exe" .)
    chmod 755 "$dir/bin/git-protect$exe"
    cat > "$dir/package.json" <<EOF
{
  "name": "$name",
  "version": "$version",
  "description": "Git Protect native binary for $nos-$narch. Install the \"git-protect\" package instead.",
  "repository": { "type": "git", "url": "git+$repo.git" },
  "license": "UNLICENSED",
  "os": ["$nos"],
  "cpu": ["$narch"],
  "files": ["bin/"],
  "preferUnplugged": true
}
EOF
    printf 'This package contains the %s binary of Git Protect. Install `git-protect` instead:\n\n    npm install -g git-protect\n' "$nos-$narch" > "$dir/README.md"
    optional="$optional    \"$name\": \"$version\",
"
done
optional=$(printf '%s' "$optional" | sed '$ s/,$//')

main="$out/git-protect"
mkdir -p "$main"
cp -r "$root/npm/git-protect/bin" "$root/npm/git-protect/lib" "$main/"
cp "$root/README.md" "$main/README.md"
cat > "$main/package.json" <<EOF
{
  "name": "git-protect",
  "version": "$version",
  "description": "End-to-end encryption for Git repositories, built on Git's native remote helper extension mechanism",
  "keywords": ["git", "encryption", "remote-helper", "security", "e2ee"],
  "homepage": "$repo",
  "repository": { "type": "git", "url": "git+$repo.git" },
  "license": "UNLICENSED",
  "bin": {
    "git-protect": "bin/git-protect.js",
    "git-remote-protect": "bin/git-remote-protect.js"
  },
  "files": ["bin/", "lib/", "README.md"],
  "engines": { "node": ">=16" },
  "optionalDependencies": {
$optional
  }
}
EOF
echo "packages in $out:"
ls "$out"
