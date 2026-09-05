#!/usr/bin/env bash
set -euo pipefail

plugin_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
target_os=${GOOS:-$(go env GOOS)}
target_arch=${GOARCH:-$(go env GOARCH)}

case "$target_os" in
  linux) extension=so ;;
  darwin) extension=dylib ;;
  windows) extension=dll ;;
  *) echo "unsupported GOOS: $target_os" >&2; exit 1 ;;
esac

mkdir -p "$plugin_root/dist/$target_os/$target_arch"
CGO_ENABLED=1 GOOS="$target_os" GOARCH="$target_arch" \
  go build -buildvcs=false -buildmode=c-shared \
  -o "$plugin_root/dist/$target_os/$target_arch/codex-wakeup.$extension" \
  "$plugin_root"

rm -f "$plugin_root/dist/$target_os/$target_arch/codex-wakeup.h"
echo "built $plugin_root/dist/$target_os/$target_arch/codex-wakeup.$extension"
