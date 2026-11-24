<#
.SYNOPSIS
    Runs >=60s On vs Off latency benchmark (arr->pres & arr->vbl p50/p99/max)
    for capview Virtual Camera impact analysis.
#>
[CmdletBinding()]
param(
    [int]$Seconds = 60,
    [ValidateSet("mf", "synthetic")]
    [string]$Source = "mf",
    [string]$Device = "auto",
    [string]$CapviewExe = "",
    [string]$OutDir = "",
    [switch]$BaselineOnly
)

$ErrorActionPreference = "Stop"
$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot "..\..")).Path
if (-not $CapviewExe) {
    $CapviewExe = Join-Path $repoRoot "dist\capview-cli-windows-amd64.exe"
}
if (-not (Test-Path $CapviewExe)) {
    throw "capview-cli executable not found at $CapviewExe"
}
if (-not $OutDir) {
    $OutDir = Join-Path $env:TEMP "capview-vcam-bench"
}
New-Item -ItemType Directory -Force -Path $OutDir | Out-Null

function Invoke-CapviewBench {
    param([bool]$VcamOn)
    $tag = if ($VcamOn) { "vcam-on" } else { "vcam-off" }
    $log = Join-Path $OutDir "$Source-$tag.stdout.log"
    $errLog = Join-Path $OutDir "$Source-$tag.stderr.log"
    $args = @(
        "-renderer", "d3d11",
        "-source", $Source,
        "-no-audio",
        "-stats=true",
        "-virtual-cam=$($VcamOn.ToString().ToLower())",
        "-duration", "${Seconds}s"
    )
    if ($Source -eq "mf") {
        $args += @("-device", $Device)
    }
    Write-Host "Running $tag ($Source, ${Seconds}s)..."
    $proc = Start-Process -FilePath $CapviewExe -ArgumentList $args `
        -RedirectStandardOutput $log -RedirectStandardError $errLog -Wait -PassThru
    if ($proc.ExitCode -ne 0) {
        $errTxt = if (Test-Path $errLog) { Get-Content $errLog -Raw } else { "" }
        throw "$tag exited with code $($proc.ExitCode): $errTxt"
    }
    $sessionLine = Get-Content $log | Where-Object { $_ -like "session |*" } | Select-Object -Last 1
    if (-not $sessionLine) {
        $sessionLine = Get-Content $log | Select-Object -Last 1
    }
    [pscustomobject]@{
        Case    = $tag
        LogPath = $log
        Summary = $sessionLine
    }
}

$results = @()
$results += Invoke-CapviewBench -VcamOn $false
if (-not $BaselineOnly) {
    $results += Invoke-CapviewBench -VcamOn $true
}

Write-Host "`n=== Benchmark Results ==="
$results | Format-Table -AutoSize -Wrap
