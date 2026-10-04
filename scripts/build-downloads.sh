#!/usr/bin/env bash
# Cross-compiles the product CLI for every published platform into dist/:
# dist/tjuclaw-<os>-<arch>[.exe]. Static, path-free builds of this public
# module only; the version comes from package.json.
set -euo pipefail

cd "$(dirname -- "${BASH_SOURCE[0]}")/.."
version=$(node -p 'require("./package.json").version')
rm -rf dist
mkdir -p dist
for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64; do
  os=${target%/*}
  arch=${target#*/}
  out="dist/tjuclaw-$os-$arch"
  [[ $os == windows ]] && out="$out.exe"
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath \
    -ldflags="-s -w -buildid= -X main.version=$version" -o "$out" ./cmd/tjuclaw
  echo "built $out"
done
