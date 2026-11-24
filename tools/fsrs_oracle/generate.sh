#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
image="kelma-fsrs-oracle:py-fsrs-6.3.1"
platform="linux/amd64"
output_dir="$repo_root/internal/fsrs/testdata"

mkdir -p "$output_dir"
docker build --platform "$platform" --tag "$image" "$repo_root/tools/fsrs_oracle"
docker run --rm --platform "$platform" --volume "$output_dir:/output" "$image"
shasum -a 256 "$output_dir/py-fsrs-6.3.1-vectors.json"
