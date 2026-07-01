#!/usr/bin/env bash
# Builds the four release binaries into dist/ plus a SHA256SUMS manifest.
# Static, stripped, reproducible (trimpath, no cgo).
set -euo pipefail

cd "$(dirname "$0")/.."
mkdir -p dist

for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
  os="${target%/*}"; arch="${target#*/}"
  out="dist/onepilot-bridge-${os}-${arch}"
  echo "build ${out}"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
    go build -trimpath -ldflags "-s -w -buildid=" -o "$out" ./cmd/onepilot-bridge
done

cd dist
shasum -a 256 onepilot-bridge-* > SHA256SUMS
cat SHA256SUMS
