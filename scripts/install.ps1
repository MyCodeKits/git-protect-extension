# git-protect installer for Windows (per-user, no administrator rights needed).
# Usage: powershell -ExecutionPolicy Bypass -File scripts\install.ps1 [-Source <path to git-protect.exe>]
param([string]$Source = "")

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
if ($Source -eq "") {
    $Source = Join-Path $root "bin\git-protect.exe"
    if (-not (Test-Path $Source)) {
        Write-Host "Building git-protect..."
        Push-Location $root
        go build -o $Source .
        Pop-Location
    }
}

$dest = Join-Path $env:LOCALAPPDATA "git-protect"
New-Item -ItemType Directory -Force $dest | Out-Null
Copy-Item $Source (Join-Path $dest "git-protect.exe") -Force
# the same binary acts as the git remote helper when named git-remote-protect
Copy-Item $Source (Join-Path $dest "git-remote-protect.exe") -Force

$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
if (($userPath -split ";") -notcontains $dest) {
    [Environment]::SetEnvironmentVariable("Path", ($userPath.TrimEnd(";") + ";" + $dest), "User")
    Write-Host "Added $dest to your user PATH."
}

# Start menu shortcut for the management panel
$lnk = Join-Path ([Environment]::GetFolderPath("Programs")) "Git Protect Manager.lnk"
$shell = New-Object -ComObject WScript.Shell
$s = $shell.CreateShortcut($lnk)
$s.TargetPath = Join-Path $dest "git-protect.exe"
$s.Arguments = "ui"
$s.WorkingDirectory = [Environment]::GetFolderPath("UserProfile")
$s.Save()

Write-Host "Installed git-protect to $dest"
Write-Host "Restart your terminal and IDE, then run: git protect doctor"
