#!/usr/bin/env bash
set -euo pipefail

version=${1:-}
if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "usage: $0 <major.minor.patch>" >&2
  exit 2
fi

plugin_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
release_dir="$plugin_root/dist/release"
mkdir -p "$release_dir"

temporary_root=$(mktemp -d)
trap 'rm -rf -- "$temporary_root"' EXIT

archives=()
for arch in amd64 arm64; do
  source_file="$plugin_root/dist/linux/$arch/codex-wakeup.so"
  if [[ ! -f "$source_file" ]]; then
    echo "missing build output: $source_file" >&2
    exit 1
  fi
  package_dir="$temporary_root/$arch"
  mkdir -p "$package_dir"
  install -m 0644 "$source_file" "$package_dir/codex-wakeup.so"
  archive_name="codex-wakeup_${version}_linux_${arch}.zip"
  archive_path="$release_dir/$archive_name"
  rm -f -- "$archive_path"
  (cd "$package_dir" && zip -q "$archive_path" codex-wakeup.so)
  archives+=("$archive_name")
done

(
  cd "$release_dir"
  sha256sum "${archives[@]}" > checksums.txt
)

printf 'packaged %s\n' "${archives[@]}" "checksums.txt"
