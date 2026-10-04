#!/usr/bin/env bash
# End-to-end test of install/install.sh against a local copy of the download
# layout: latest install, pinned version, tampered binary, and (when Docker is
# available) busybox wget on Alpine and a curl-less Debian.
set -euo pipefail

cd "$(dirname -- "${BASH_SOURCE[0]}")/.."
[[ -f dist/tjuclaw-linux-amd64 ]] || bash scripts/build-downloads.sh >/dev/null
version=$(node -p 'require("./package.json").version')
work=$(mktemp -d)
trap 'kill "${server:-0}" 2>/dev/null || true; rm -rf "$work"' EXIT

site="$work/site/cli"
mkdir -p "$site/v$version" "$site/v0.0.1"
cp dist/tjuclaw-* "$site/v$version/"
(cd "$site/v$version" && sha256sum tjuclaw-* > SHA256SUMS)
echo "$version" > "$site/LATEST"
# An older "release" whose binary does not match its checksum.
cp dist/tjuclaw-linux-amd64 "$site/v0.0.1/tjuclaw-linux-amd64"
echo "0000000000000000000000000000000000000000000000000000000000000000  tjuclaw-linux-amd64" > "$site/v0.0.1/SHA256SUMS"
cp install/install.sh install/install.ps1 "$site/"

port=$((20000 + RANDOM % 20000))
python3 -m http.server "$port" --bind 0.0.0.0 --directory "$work/site" >/dev/null 2>&1 &
server=$!
for _ in $(seq 50); do curl -fs "http://127.0.0.1:$port/cli/LATEST" >/dev/null && break; sleep 0.1; done
base="http://127.0.0.1:$port/cli"

check() { printf '  %-58s' "$1"; }
ok() { echo ok; }

check "latest install into a fresh directory"
out=$(TJUCLAW_DOWNLOAD_URL=$base TJUCLAW_INSTALL_DIR="$work/bin" sh "$site/install.sh")
"$work/bin/tjuclaw" version | grep -q "\"version\":\"$version\"" && grep -q "不在 PATH" <<<"$out" && ok

check "pinned version replaces the installed binary"
TJUCLAW_VERSION="v$version" TJUCLAW_DOWNLOAD_URL=$base TJUCLAW_INSTALL_DIR="$work/bin" sh "$site/install.sh" >/dev/null && ok

check "a checksum mismatch installs nothing"
if TJUCLAW_VERSION=0.0.1 TJUCLAW_DOWNLOAD_URL=$base TJUCLAW_INSTALL_DIR="$work/bin2" sh "$site/install.sh" >/dev/null 2>"$work/err"; then
  echo "FAILED: installed despite mismatch"; exit 1
fi
grep -q "校验失败" "$work/err" && [[ ! -e "$work/bin2/tjuclaw" ]] && ok

check "a missing version fails clearly"
if TJUCLAW_VERSION=9.9.9 TJUCLAW_DOWNLOAD_URL=$base TJUCLAW_INSTALL_DIR="$work/bin3" sh "$site/install.sh" >/dev/null 2>"$work/err"; then
  echo "FAILED"; exit 1
fi
grep -q "下载失败" "$work/err" && ok

if command -v docker >/dev/null && docker info >/dev/null 2>&1; then
  check "Alpine: busybox sh, wget and sha256sum"
  docker run --rm --network host -e TJUCLAW_DOWNLOAD_URL=$base alpine:3.22 sh -c \
    "wget -qO- $base/install.sh | sh >/dev/null && ~/.local/bin/tjuclaw version" | grep -q "\"version\":\"$version\"" && ok
  check "Debian slim (dash) without curl or wget says what is missing"
  if docker run --rm --network host -v "$site/install.sh:/install.sh:ro" debian:bookworm-slim sh /install.sh 2>"$work/err"; then
    echo "FAILED"; exit 1
  fi
  grep -q "需要 curl 或 wget" "$work/err" && ok
fi
echo "install.sh: all checks passed"
