#!/bin/sh
# Installs the TJUClaw CLI (tjuclaw) on Linux or macOS.
#
#   curl -fsSL https://tjuclaw-release.zaixi.dev/cli/install.sh | sh
#
# Options (environment variables):
#   TJUCLAW_VERSION      install this version instead of the latest (e.g. 0.0.31)
#   TJUCLAW_INSTALL_DIR  install into this directory (default: ~/.local/bin)
#   TJUCLAW_DOWNLOAD_URL download base (default: https://tjuclaw-release.zaixi.dev/cli)
#
# The binary is checked against the release's SHA256SUMS before it replaces
# anything. Nothing is installed outside the chosen directory and no shell
# profile is edited.
set -eu

base=${TJUCLAW_DOWNLOAD_URL:-https://tjuclaw-release.zaixi.dev/cli}
dir=${TJUCLAW_INSTALL_DIR:-$HOME/.local/bin}

fail() { printf 'tjuclaw 安装失败：%s\n' "$1" >&2; exit 1; }
say() { printf '%s\n' "$1"; }

case $(uname -s) in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) fail "不支持的系统 $(uname -s)。Windows 请在 PowerShell 中运行：irm $base/install.ps1 | iex" ;;
esac
case $(uname -m) in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) fail "不支持的处理器架构 $(uname -m)" ;;
esac
# Rosetta reports x86_64 on Apple silicon; prefer the native build.
if [ "$os" = darwin ] && [ "$arch" = amd64 ] && [ "$(sysctl -n sysctl.proc_translated 2>/dev/null || echo 0)" = 1 ]; then
  arch=arm64
fi

if command -v curl >/dev/null 2>&1; then
  fetch() { curl -fsSL --retry 2 -o "$2" "$1"; }
elif command -v wget >/dev/null 2>&1; then
  fetch() { wget -q -O "$2" "$1"; }
else
  fail "需要 curl 或 wget"
fi
if command -v sha256sum >/dev/null 2>&1; then
  digest() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
  digest() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
  fail "需要 sha256sum 或 shasum 来校验下载"
fi

tmp=$(mktemp -d 2>/dev/null || mktemp -d -t tjuclaw)
trap 'rm -rf "$tmp"' EXIT INT TERM

version=${TJUCLAW_VERSION:-}
if [ -z "$version" ]; then
  fetch "$base/LATEST" "$tmp/LATEST" || fail "无法获取最新版本号（$base/LATEST）"
  version=$(tr -d ' \r\n' < "$tmp/LATEST")
fi
version=${version#v}
case $version in
  *[!0-9.]* | '' ) fail "版本号无效：$version" ;;
esac

name="tjuclaw-$os-$arch"
say "正在下载 tjuclaw $version（$os/$arch）…"
fetch "$base/v$version/$name" "$tmp/tjuclaw" || fail "下载失败：$base/v$version/$name"
fetch "$base/v$version/SHA256SUMS" "$tmp/SHA256SUMS" || fail "无法下载校验文件"
expected=$(awk -v n="$name" '$2 == n { print $1 }' "$tmp/SHA256SUMS")
[ -n "$expected" ] || fail "校验文件里没有 $name"
[ "$(digest "$tmp/tjuclaw")" = "$expected" ] || fail "校验失败，下载内容与发布不一致，已停止安装"

chmod 0755 "$tmp/tjuclaw"
mkdir -p "$dir" || fail "无法创建目录 $dir"
# Replace atomically: a running tjuclaw keeps its old file.
cp "$tmp/tjuclaw" "$dir/.tjuclaw.new" && mv -f "$dir/.tjuclaw.new" "$dir/tjuclaw" || fail "无法写入 $dir（可设置 TJUCLAW_INSTALL_DIR 换一个目录）"

say "已安装 tjuclaw $version 到 $dir/tjuclaw"
case ":$PATH:" in
  *":$dir:"*) say "运行 tjuclaw version 确认安装，然后参考文档连接这台电脑：https://tjuclaw.cloud/docs" ;;
  *)
    say ""
    say "$dir 不在 PATH 中。把下面这行加入 ~/.bashrc 或 ~/.zshrc 后重开终端："
    say "  export PATH=\"$dir:\$PATH\""
    ;;
esac
