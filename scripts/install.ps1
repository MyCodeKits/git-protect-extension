# genc installer for Windows (per-user, no administrator rights needed).
# Usage: powershell -ExecutionPolicy Bypass -File scripts\install.ps1 [-Source <path to genc.exe>]
param([string]$Source = "")

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
if ($Source -eq "") {
    $Source = Join-Path $root "bin\genc.exe"
    if (-not (Test-Path $Source)) {
        Write-Host "Building genc..."
        Push-Location $root
        go build -o $Source .
        Pop-Location
    }
}

$dest = Join-Path $env:LOCALAPPDATA "genc"
New-Item -ItemType Directory -Force $dest | Out-Null
Copy-Item $Source (Join-Path $dest "genc.exe") -Force
# the same binary acts as the git remote helper when named git-remote-genc
Copy-Item $Source (Join-Path $dest "git-remote-genc.exe") -Force

$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
if (($userPath -split ";") -notcontains $dest) {
    [Environment]::SetEnvironmentVariable("Path", ($userPath.TrimEnd(";") + ";" + $dest), "User")
    Write-Host "Added $dest to your user PATH."
}

# Start menu shortcut for the management panel
$lnk = Join-Path ([Environment]::GetFolderPath("Programs")) "genc Manager.lnk"
$shell = New-Object -ComObject WScript.Shell
$s = $shell.CreateShortcut($lnk)
$s.TargetPath = Join-Path $dest "genc.exe"
$s.Arguments = "ui"
$s.WorkingDirectory = [Environment]::GetFolderPath("UserProfile")
$s.Save()

Write-Host "Installed genc to $dest"
Write-Host "Restart your terminal and IDE, then run: genc doctor"
