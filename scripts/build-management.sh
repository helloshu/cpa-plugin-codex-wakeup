#!/usr/bin/env bash
set -euo pipefail

plugin_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
panel_commit=c12997e1a544374336ea385d5e9a9bbabe1e4767
panel_root=$(mktemp -d)
trap 'rm -rf "$panel_root"' EXIT

git -C "$panel_root" init -q
git -C "$panel_root" fetch -q --depth 1 https://github.com/router-for-me/Cli-Proxy-API-Management-Center.git "$panel_commit"
git -C "$panel_root" checkout -q --detach FETCH_HEAD
test "$(git -C "$panel_root" rev-parse HEAD)" = "$panel_commit"
git -C "$panel_root" apply "$plugin_root/management-center/embedded-auth.patch"
version=$(sed -n 's/^[[:space:]]*pluginVersion = "\([^"]*\)"/\1/p' "$plugin_root/main.go")
(
  cd "$panel_root"
  bun install --frozen-lockfile
  VERSION="wakeup-v$version+${panel_commit:0:7}" bun run verify
)
mkdir -p "$plugin_root/dist/management"
cp "$panel_root/dist/index.html" "$plugin_root/dist/management/management.html"
cp "$panel_root/LICENSE" "$plugin_root/dist/management/management.LICENSE.txt"
echo "built $plugin_root/dist/management/management.html"
