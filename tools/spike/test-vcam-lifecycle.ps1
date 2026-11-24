<#
.SYNOPSIS
    Tests Windows 11 Virtual Camera lifetime behavior and TerminateProcess crash recovery.
#>
[CmdletBinding()]
param(
    [string]$CapviewExe = "",
    [switch]$TestSystemLifetime
)

$ErrorActionPreference = "Stop"
$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot "..\..")).Path
if (-not $CapviewExe) {
    $CapviewExe = Join-Path $repoRoot "dist\capview-cli-windows-amd64.exe"
}
if (-not (Test-Path $CapviewExe)) {
    throw "capview-cli executable not found at $CapviewExe"
}

Write-Host "=== 1. Devices before start ==="
& $CapviewExe list

Write-Host "`n=== 2. Launching Session-lifetime probe (-hold 30s) ==="
$outLog = Join-Path $env:TEMP "vcam-crash-test.stdout.log"
$errLog = Join-Path $env:TEMP "vcam-crash-test.stderr.log"
$proc = Start-Process -FilePath $CapviewExe `
    -ArgumentList @("vcam-probe", "-lifetime", "session", "-hold", "30s") `
    -RedirectStandardOutput $outLog -RedirectStandardError $errLog -PassThru

Start-Sleep -Seconds 3
if ($proc.HasExited) {
    Write-Host "Probe exited early with code $($proc.ExitCode):"
    if (Test-Path $outLog) { Get-Content $outLog }
    if (Test-Path $errLog) { Get-Content $errLog }
    exit 1
}

Write-Host "Probe alive (PID=$($proc.Id)). Devices while active:"
& $CapviewExe list

Write-Host "`n=== 3. Simulating hard crash via TerminateProcess (Stop-Process -Force) ==="
Stop-Process -Id $proc.Id -Force
$proc.WaitForExit(5000) | Out-Null
Start-Sleep -Seconds 2

Write-Host "Devices 2s after TerminateProcess:"
& $CapviewExe list

Write-Host "`n=== 4. Relaunching immediately after crash ==="
& $CapviewExe vcam-probe -lifetime session -hold 3s
if ($LASTEXITCODE -ne 0) {
    throw "Relaunch after TerminateProcess failed with exit code $LASTEXITCODE"
}

if ($TestSystemLifetime) {
    Write-Host "`n=== 5. Testing System-lifetime persistence + Remove() ==="
    $p2 = Start-Process -FilePath $CapviewExe `
        -ArgumentList @("vcam-probe", "-lifetime", "system", "-no-remove", "-hold", "2s") `
        -Wait -PassThru
    Write-Host "System-lifetime probe (no-remove) exit=$($p2.ExitCode). Devices after process exit:"
    & $CapviewExe list
    Write-Host "Cleaning up System-lifetime entry via normal probe (Stop+Remove):"
    & $CapviewExe vcam-probe -lifetime system -hold 1s
}
