#!/bin/sh
# Publishes the packages produced by scripts/npm-build.sh to the npm registry.
# Platform packages go first so the main package never points at missing versions.
# Authentication: an npm token in NODE_AUTH_TOKEN or NPM_TOKEN (never stored in the repo).
# Usage: sh scripts/npm-publish.sh [output-dir]
set -eu

out=${1:-npm-dist}
token=${NODE_AUTH_TOKEN:-${NPM_TOKEN:-}}
[ -n "$token" ] || { echo "set NODE_AUTH_TOKEN or NPM_TOKEN" >&2; exit 1; }

npmrc=$(mktemp)
trap 'rm -f "$npmrc"' EXIT
# the token stays in the environment; the config file only references it
printf '//registry.npmjs.org/:_authToken=${GIT_PROTECT_NPM_TOKEN}\n' > "$npmrc"
export GIT_PROTECT_NPM_TOKEN="$token"

publish() {
    name=$(cd "$1" && node -p "require('./package.json').name")
    version=$(cd "$1" && node -p "require('./package.json').version")
    if npm view "$name@$version" version --userconfig "$npmrc" >/dev/null 2>&1; then
        echo "skip $name@$version (already published)"
        return
    fi
    echo "publish $name@$version"
    if ! log=$(cd "$1" && npm publish --access public --userconfig "$npmrc" 2>&1); then
        # the registry can lag behind right after a publish; treat an existing version as done
        case "$log" in
            *"previously published versions"*|*"publish over previously"*) echo "skip $name@$version (already published)"; return ;;
        esac
        printf '%s\n' "$log" >&2
        return 1
    fi
}

for dir in "$out"/git-protect-*/; do
    publish "${dir%/}"
done
publish "$out/git-protect"
