<#
.SYNOPSIS
  LRM installer for Windows.

.DESCRIPTION
  Downloads the prebuilt lrm binary for this machine, verifies it against
  the release checksums, installs it under your user profile (no admin),
  puts it on PATH, and then runs it to prove it works.

  Nothing is written outside your user profile and no administrator rights
  are required.

.EXAMPLE
  irm https://raw.githubusercontent.com/hacvilke/lrm/main/scripts/install.ps1 | iex

.EXAMPLE
  .\install.ps1 -Dir C:\tools\lrm -Force

.NOTES
  There is an unrelated npm package also called "lrm". If you have ever
  run `npm i -g lrm`, a shim of that name may sit on your PATH ahead of
  this one; the installer checks for that and tells you how to fix it.
#>
[CmdletBinding()]
param(
    # Install directory. Default: %LOCALAPPDATA%\Programs\lrm
    [string]$Dir,
    # Release tag to install, e.g. v0.3.1. Default: latest.
    [string]$Version,
    # Install a local binary instead of downloading.
    [string]$From,
    # owner/repo to download from.
    [string]$Repo = 'hacvilke/lrm',
    # Overwrite an existing lrm.exe.
    [switch]$Force,
    # Print the plan and change nothing.
    [switch]$DryRun,
    # Skip SHA-256 verification (not recommended).
    [switch]$NoVerify,
    # Do not modify PATH.
    [switch]$NoPath
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version 2.0

function Say  { param([string]$m) Write-Host $m }
function Warn { param([string]$m) Write-Host "warning: $m" -ForegroundColor Yellow }
function Die  { param([string]$m) Write-Host "error: $m" -ForegroundColor Red; exit 1 }

# ----------------------------------------------------------- platform ---
# PowerShell runs on Linux and macOS too, where this installer is the
# wrong tool: install.sh handles those and does more (Android detection,
# source builds). Say so rather than producing a broken install.
$onWindows = $true
if (Test-Path Variable:\IsWindows) { $onWindows = $IsWindows }
if (-not $onWindows -and -not $env:LRM_PS_TEST) {
    Die @"
this is the Windows installer, and this is not Windows.
       Use the POSIX installer instead:
         curl -fsSL https://raw.githubusercontent.com/$Repo/main/scripts/install.sh | sh
"@
}

function Get-LrmArch {
    $a = $env:PROCESSOR_ARCHITECTURE
    if (-not $a) { $a = 'AMD64' }                 # non-Windows test runs
    if ($env:LRM_TEST_ARCH) { $a = $env:LRM_TEST_ARCH }
    switch -Regex ($a) {
        '^(AMD64|x86_64)$' { return 'amd64' }
        '^ARM64$'          { return 'arm64' }
        '^x86$'            { Die 'LRM does not ship a 32-bit Windows build. Use a 64-bit Windows, or build from source with Go.' }
        default            { Die "unsupported CPU architecture: $a" }
    }
}

$arch  = Get-LrmArch
$asset = "lrm_windows_$arch.exe"

# ------------------------------------------------------------- paths ---
if (-not $Dir) {
    if ($env:LRM_INSTALL_DIR) {
        $Dir = $env:LRM_INSTALL_DIR
    } elseif ($env:LOCALAPPDATA) {
        $Dir = Join-Path $env:LOCALAPPDATA 'Programs\lrm'
    } else {
        $Dir = Join-Path $HOME '.local/bin'
    }
}
$dest = Join-Path $Dir 'lrm.exe'

Say 'LRM installer (Windows)'
Say "  platform : windows/$arch"
Say "  asset    : $asset"
Say "  install  : $dest"
Say ''

if ($DryRun) {
    Say "dry run: would install $asset to $dest"
    exit 0
}

if ((Test-Path $dest) -and -not $Force) {
    Die "$dest already exists (use -Force to replace it)"
}

New-Item -ItemType Directory -Force -Path $Dir | Out-Null
$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("lrm-" + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Force -Path $tmp | Out-Null

try {
    if ($From) {
        if (-not (Test-Path $From)) { Die "no such file: $From" }
        $src = $From
        Say "using local binary: $From"
    }
    else {
        # TLS 1.2 for Windows PowerShell 5.1, whose default is older.
        try {
            [Net.ServicePointManager]::SecurityProtocol =
                [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
        } catch { }

        if (-not $Version) {
            Say 'resolving the latest release ...'
            try {
                $rel = Invoke-RestMethod -UseBasicParsing `
                    -Uri "https://api.github.com/repos/$Repo/releases/latest" `
                    -Headers @{ 'User-Agent' = 'lrm-installer' }
                $Version = $rel.tag_name
            } catch {
                Die "could not reach the GitHub API: $($_.Exception.Message)"
            }
        }
        if (-not $Version) { Die "no published release found for $Repo" }

        $base = "https://github.com/$Repo/releases/download/$Version"
        $src  = Join-Path $tmp $asset
        Say "downloading $asset ($Version) ..."
        try {
            Invoke-WebRequest -UseBasicParsing -Uri "$base/$asset" -OutFile $src
        } catch {
            Die @"
download failed: $base/$asset
       $($_.Exception.Message)
       If this is Windows on ARM, there may be no arm64 build yet -- install
       the amd64 build instead (Windows emulates it):
         .\install.ps1 -Version $Version -Force
       after setting `$env:LRM_TEST_ARCH='AMD64'
"@
        }

        if ($NoVerify) {
            Warn 'checksum verification skipped (-NoVerify)'
        }
        else {
            Say 'verifying SHA-256 ...'
            $sumsPath = Join-Path $tmp 'SHA256SUMS.txt'
            try {
                Invoke-WebRequest -UseBasicParsing -Uri "$base/SHA256SUMS.txt" -OutFile $sumsPath
            } catch {
                Die 'SHA256SUMS.txt could not be downloaded; refusing to install an unverified binary (use -NoVerify to override)'
            }
            $want = $null
            foreach ($line in Get-Content $sumsPath) {
                $parts = $line -split '\s+' | Where-Object { $_ -ne '' }
                if ($parts.Count -ge 2 -and ($parts[1] -replace '^\*','') -eq $asset) {
                    $want = $parts[0].ToLower()
                    break
                }
            }
            if (-not $want) { Die "no checksum listed for $asset in release $Version" }
            $got = (Get-FileHash -Algorithm SHA256 -Path $src).Hash.ToLower()
            if ($want -ne $got) {
                Die @"
checksum mismatch for $asset
       expected $want
       got      $got
"@
            }
            Say "  ok $got"
        }
    }

    Copy-Item -Path $src -Destination $dest -Force
    Say "installed: $dest"
}
finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}

# ------------------------------------------------- does it actually run ---
# A checksum proves we downloaded the bytes we meant to. It says nothing
# about whether this machine will run them. Check before claiming success.
Say ''
Say 'checking that it runs here ...'
# Exit status alone is not enough: a host that cannot execute the file may
# still hand it to some other handler that exits 0. Require the output to
# actually look like LRM.
$ran = $false
$out = $null
try {
    $out = & $dest version 2>&1
    $text = ($out | Out-String)
    if ($LASTEXITCODE -eq 0 -and $text -match '(?i)lrm') {
        Say ("  ok  " + (($text -split "`n")[0]).Trim())
        $ran = $true
    }
} catch { }
if (-not $ran) {
    try {
        $out = & $dest --help 2>&1
        $text = ($out | Out-String)
        if ($LASTEXITCODE -eq 0 -and $text -match '(?i)(usage|lrm)') {
            Say '  ok  lrm --help works'
            $ran = $true
        }
    } catch { }
}
if (-not $ran) {
    $detail = ''
    if ($out) { $detail = ($out | Out-String).Trim() }
    if (-not $detail) { $detail = 'it exited without output -- the file is probably not a valid executable for this machine.' }
    Die @"
the installed binary did not run:
       $detail
       The download was checksum-verified, so the file is intact; it is
       the wrong kind of binary for this machine. Check that you are on
       64-bit Windows, or build from source with Go.
"@
}

# ------------------------------------------------------------- PATH ---
$userPath = ''
if ($onWindows) {
    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    if (-not $userPath) { $userPath = '' }
}
$onPath = ($env:Path -split ';' | Where-Object { $_ } |
           ForEach-Object { $_.TrimEnd('\') } ) -contains $Dir.TrimEnd('\')

if ($onPath) {
    Say ''
    Say "PATH: ok -- $Dir is already on your PATH."
}
elseif ($NoPath -or -not $onWindows) {
    Say ''
    Say "PATH: add $Dir to your PATH to run lrm by name."
}
else {
    $new = if ($userPath -eq '') { $Dir } else { "$userPath;$Dir" }
    [Environment]::SetEnvironmentVariable('Path', $new, 'User')
    $env:Path = "$env:Path;$Dir"
    Say ''
    Say "PATH: added $Dir to your user PATH."
    Say '      Open a NEW terminal for it to take effect in other windows.'
}

# ------------------------------------------- the npm name collision ---
# There is an unrelated npm package called "lrm". A global install of it
# leaves a shim in %APPDATA%\npm which comes earlier on PATH than this
# install, so typing `lrm` runs node and fails with MODULE_NOT_FOUND.
$shadow = $null
try {
    $cmds = @(Get-Command lrm -All -ErrorAction SilentlyContinue)
    foreach ($c in $cmds) {
        $p = $null
        if ($c.PSObject.Properties.Name -contains 'Source') { $p = $c.Source }
        if (-not $p -and $c.PSObject.Properties.Name -contains 'Path') { $p = $c.Path }
        if ($p -and ($p -ne $dest)) { $shadow = $p; break }
    }
} catch { }

if ($shadow) {
    Say ''
    Warn @"
another command named 'lrm' comes first on your PATH:
           $shadow
         That is almost certainly the unrelated npm package of the same
         name. Typing 'lrm' will run that instead of this install.

         Remove it with:
           npm uninstall -g lrm

         If npm reports nothing to uninstall, delete the leftover shims:
           Remove-Item "`$env:APPDATA\npm\lrm", "`$env:APPDATA\npm\lrm.cmd", "`$env:APPDATA\npm\lrm.ps1" -ErrorAction SilentlyContinue
           Remove-Item -Recurse "`$env:APPDATA\npm\node_modules\lrm" -ErrorAction SilentlyContinue

         Until then, run this install by its full path:
           $dest
"@
}

Say ''
Say 'Next:'
Say '  lrm --help'
Say '  mkdir ~/my-project; cd ~/my-project'
Say '  lrm init --user you'
Say '  lrm daemon            # starts syncing with your other devices'
Say '  lrm dashboard         # opens the local view in your browser'
