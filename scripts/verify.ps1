<#
.SYNOPSIS
  Download the latest route-prism operator binary and run `route-prism verify`.

.EXAMPLE
  iwr https://raw.githubusercontent.com/egoavara/route-prism/main/scripts/verify.ps1 -UseBasicParsing | iex

.EXAMPLE
  # With explicit args (non-interactive)
  & ([scriptblock]::Create((iwr https://raw.githubusercontent.com/egoavara/route-prism/main/scripts/verify.ps1 -UseBasicParsing).Content)) --context my-cluster

.NOTES
  Environment overrides:
    $env:ROUTE_PRISM_VERSION   pin a release tag (default: latest)
    $env:ROUTE_PRISM_BIN_DIR   cache directory (default: $env:LOCALAPPDATA\route-prism)
#>

[CmdletBinding()]
param(
    [Parameter(ValueFromRemainingArguments = $true)]
    [string[]]$Args
)

$ErrorActionPreference = 'Stop'

$repo = 'egoavara/route-prism'
$version = $env:ROUTE_PRISM_VERSION
$binDir = $env:ROUTE_PRISM_BIN_DIR
if (-not $binDir) { $binDir = Join-Path $env:LOCALAPPDATA 'route-prism' }

# --- detect arch (windows-only here) ---------------------------------------
$arch = switch ($env:PROCESSOR_ARCHITECTURE) {
    'AMD64' { 'amd64' }
    'ARM64' { 'arm64' }
    default { Write-Error "verify.ps1: unsupported processor architecture '$env:PROCESSOR_ARCHITECTURE'"; return }
}
if ($arch -ne 'amd64') {
    Write-Error "verify.ps1: only windows/amd64 binaries are published today (got $arch)"
    return
}

# --- resolve version --------------------------------------------------------
if (-not $version) {
    $resp = Invoke-RestMethod -Uri "https://api.github.com/repos/$repo/releases/latest" -UseBasicParsing
    $version = $resp.tag_name
    if (-not $version) {
        Write-Error 'verify.ps1: failed to resolve latest release tag'
        return
    }
}
$verNoV = $version.TrimStart('v')

# --- fetch binary -----------------------------------------------------------
$asset = "route-prism-operator_${verNoV}_windows_${arch}.exe"
$url   = "https://github.com/$repo/releases/download/$version/$asset"
New-Item -ItemType Directory -Force -Path $binDir | Out-Null
$bin = Join-Path $binDir $asset

if (-not (Test-Path $bin)) {
    Write-Host "verify.ps1: downloading $asset ($version) ..." -ForegroundColor DarkGray
    Invoke-WebRequest -Uri $url -OutFile $bin -UseBasicParsing
}

# --- run --------------------------------------------------------------------
& $bin verify @Args
exit $LASTEXITCODE
