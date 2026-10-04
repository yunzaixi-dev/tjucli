# Installs the TJUClaw CLI (tjuclaw.exe) on Windows.
#
#   irm https://tjuclaw-release.zaixi.dev/cli/install.ps1 | iex
#
# Options (environment variables):
#   TJUCLAW_VERSION      install this version instead of the latest (e.g. 0.0.31)
#   TJUCLAW_INSTALL_DIR  install into this directory (default: %LOCALAPPDATA%\Programs\tjuclaw)
#   TJUCLAW_DOWNLOAD_URL download base (default: https://tjuclaw-release.zaixi.dev/cli)
#
# The binary is checked against the release's SHA256SUMS before it replaces
# anything. The install directory is added to the user PATH only.
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

function Fail($message) { throw "tjuclaw 安装失败：$message" }

$base = if ($env:TJUCLAW_DOWNLOAD_URL) { $env:TJUCLAW_DOWNLOAD_URL.TrimEnd('/') } else { 'https://tjuclaw-release.zaixi.dev/cli' }
$dir = if ($env:TJUCLAW_INSTALL_DIR) { $env:TJUCLAW_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA 'Programs\tjuclaw' }

$machine = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
$arch = switch ($machine) {
  'AMD64' { 'amd64' }
  'ARM64' { 'arm64' }
  default { Fail "不支持的处理器架构 $machine" }
}

$tmp = Join-Path ([IO.Path]::GetTempPath()) ("tjuclaw-" + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
  $version = $env:TJUCLAW_VERSION
  if (-not $version) {
    try { $version = (Invoke-WebRequest -UseBasicParsing "$base/LATEST").Content } catch { Fail "无法获取最新版本号（$base/LATEST）" }
    if ($version -is [byte[]]) { $version = [Text.Encoding]::UTF8.GetString($version) }
  }
  $version = $version.Trim().TrimStart('v')
  if ($version -notmatch '^[0-9][0-9.]*$') { Fail "版本号无效：$version" }

  $name = "tjuclaw-windows-$arch.exe"
  Write-Host "正在下载 tjuclaw $version（windows/$arch）…"
  $exe = Join-Path $tmp 'tjuclaw.exe'
  $sums = Join-Path $tmp 'SHA256SUMS'
  try { Invoke-WebRequest -UseBasicParsing "$base/v$version/$name" -OutFile $exe } catch { Fail "下载失败：$base/v$version/$name" }
  try { Invoke-WebRequest -UseBasicParsing "$base/v$version/SHA256SUMS" -OutFile $sums } catch { Fail '无法下载校验文件' }

  $expected = $null
  foreach ($line in Get-Content $sums) {
    $parts = $line -split '\s+'
    # Accept both checksum line styles: "<hash>  name" and "<hash> *name".
    if ($parts.Count -ge 2 -and $parts[1].TrimStart('*') -eq $name) { $expected = $parts[0].ToLowerInvariant() }
  }
  if (-not $expected) { Fail "校验文件里没有 $name" }
  $actual = (Get-FileHash -Algorithm SHA256 $exe).Hash.ToLowerInvariant()
  if ($actual -ne $expected) { Fail '校验失败，下载内容与发布不一致，已停止安装' }

  New-Item -ItemType Directory -Force -Path $dir | Out-Null
  $target = Join-Path $dir 'tjuclaw.exe'
  # A running tjuclaw.exe cannot be overwritten; move it aside first.
  if (Test-Path $target) {
    $old = "$target.old"
    Remove-Item -Force $old -ErrorAction SilentlyContinue
    try { Move-Item -Force $target $old } catch { Fail "无法替换 $target，请先关闭正在运行的 tjuclaw" }
  }
  Copy-Item $exe $target
  Remove-Item -Force "$target.old" -ErrorAction SilentlyContinue

  Write-Host "已安装 tjuclaw $version 到 $target"
  $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
  $entries = @($userPath -split ';' | Where-Object { $_ })
  if ($entries -notcontains $dir) {
    [Environment]::SetEnvironmentVariable('Path', (($entries + $dir) -join ';'), 'User')
    $env:Path = "$env:Path;$dir"
    Write-Host "已把 $dir 加入用户 PATH；已打开的其他终端需要重开才能使用 tjuclaw。"
  }
  Write-Host '运行 tjuclaw version 确认安装，然后参考文档连接这台电脑：https://tjuclaw.cloud/docs'
} finally {
  Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}
