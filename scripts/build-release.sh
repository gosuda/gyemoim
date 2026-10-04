#!/usr/bin/env bash
set -euo pipefail

script_dir="$(CDPATH= cd "$(dirname "$BASH_SOURCE")" >/dev/null && pwd -P)"
repo_root="$(CDPATH= cd "$script_dir/.." >/dev/null && pwd -P)"

if [[ "$#" -gt 1 ]]; then
  printf 'Usage: %s [OUTPUT_DIRECTORY]\n' "$0" >&2
  exit 2
fi

if [[ "$#" -eq 1 ]]; then
  output_dir=$1
else
  output_dir="$repo_root/dist"
fi

if [[ "$output_dir" != /* ]]; then
  output_dir="$PWD/$output_dir"
fi

mkdir -p "$output_dir"
output_dir="$(CDPATH= cd "$output_dir" >/dev/null && pwd -P)"

check_output_target() {
  target="$output_dir/$1"
  if [[ -L "$target" ]]; then
    printf 'Refusing to replace symlink: %s\n' "$target" >&2
    exit 1
  fi
  if [[ -e "$target" && ! -f "$target" ]]; then
    printf 'Refusing to replace non-file build output: %s\n' "$target" >&2
    exit 1
  fi
}

for artifact in \
  gyemoim-linux-amd64 \
  gyemoim-linux-arm64 \
  gyemoim-darwin-amd64 \
  gyemoim-darwin-arm64
do
  check_output_target "$artifact"
done

if ! command -v go >/dev/null 2>&1; then
  printf 'Go 1.25 or newer is required to build Gyemoim.\n' >&2
  exit 1
fi

stage_dir="$(mktemp -d "$output_dir/.gyemoim-build.XXXXXX")"
cleanup() {
  for artifact in \
    gyemoim-linux-amd64 \
    gyemoim-linux-arm64 \
    gyemoim-darwin-amd64 \
    gyemoim-darwin-arm64
  do
    if [[ -e "$stage_dir/$artifact" || -L "$stage_dir/$artifact" ]]; then
      rm -f "$stage_dir/$artifact" || true
    fi
  done
  rmdir "$stage_dir" 2>/dev/null || true
}
trap cleanup EXIT

build_one() {
  target_os=$1
  target_arch=$2
  artifact=$3
  printf 'Building %s/%s -> %s\n' "$target_os" "$target_arch" "$artifact"
  (
    cd "$repo_root"
    CGO_ENABLED=0 GOOS="$target_os" GOARCH="$target_arch" \
      go build -trimpath -o "$stage_dir/$artifact" ./cmd/gyemoim
  )
}

build_one linux amd64 gyemoim-linux-amd64
build_one linux arm64 gyemoim-linux-arm64
build_one darwin amd64 gyemoim-darwin-amd64
build_one darwin arm64 gyemoim-darwin-arm64

for artifact in \
  gyemoim-linux-amd64 \
  gyemoim-linux-arm64 \
  gyemoim-darwin-amd64 \
  gyemoim-darwin-arm64
do
  check_output_target "$artifact"
  mv -f "$stage_dir/$artifact" "$output_dir/$artifact"
  printf 'Created %s\n' "$output_dir/$artifact"
done

