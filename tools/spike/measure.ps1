<#
.SYNOPSIS
    Measures capview D3D11 vs Vulkan presentation behavior using PresentMon Console and capview stdout telemetry.

.DESCRIPTION
    Launches capview-cli.exe with the specified renderer/mode/source, attaches PresentMon-2.6.0-x64.exe
    by PID to capture ETW present/display events to a CSV, and prints a summary of PresentMode,
    AllowsTearing, SyncInterval, MsBetweenPresents (p50/p99), and MsUntilDisplayed (p50/p99),
    alongside capview's stdout telemetry.

    NOTE ON PERMISSIONS:
    ETW capture with PresentMon typically requires an elevated Administrator PowerShell prompt
    (or membership in the 'Performance Log Users' group with ETW session permissions).

.EXAMPLE
    .\tools\spike\measure.ps1 -Renderer d3d11 -Seconds 20
    .\tools\spike\measure.ps1 -Renderer vulkan -Fullscreen -Seconds 20
    .\tools\spike\measure.ps1 -Renderer vulkan -Fullscreen -VkFse allowed -Seconds 20
#>
[CmdletBinding()]
param(
    [ValidateSet("d3d11", "vulkan")]
    [string]$Renderer = "d3d11",
    [switch]$Fullscreen,
    [switch]$VSync,
    [int]$Seconds = 20,
    [int]$WarmupSeconds = 2,
    [ValidateSet("synthetic", "mf")]
    [string]$Source = "synthetic",
    [string]$CapviewExe = "",
    [string]$PresentMonExe = "C:\Users\virgil\tools\presentmon\PresentMon-2.6.0-x64.exe",
    [string]$OutDir = "",
    [string]$VkFse = "",
    [string]$Display = ""
)

$ErrorActionPreference = "Stop"

$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot "..\..")).Path
if (-not $CapviewExe) {
    $CapviewExe = Join-Path $repoRoot "capview-cli.exe"
}
if (-not (Test-Path $CapviewExe)) {
    throw "capview-cli executable not found at $CapviewExe. Build with: go build -ldflags=`"-s -w`" -o capview-cli.exe ./cmd/capview"
}
if (-not (Test-Path $PresentMonExe)) {
    throw "PresentMon executable not found at $PresentMonExe"
}
if (-not $OutDir) {
    $OutDir = Join-Path $env:TEMP "capview-spike-measure"
}
New-Item -ItemType Directory -Force -Path $OutDir | Out-Null

$modeTag = if ($Fullscreen) { "fullscreen" } else { "windowed" }
if ($VSync) { $modeTag += "-vsync" }
$stamp = Get-Date -Format "yyyyMMdd-HHmmss"
$baseName = "$Renderer-$modeTag-$stamp"
$csvPath = Join-Path $OutDir "$baseName.csv"
$stdoutLog = Join-Path $OutDir "$baseName.stdout.log"
$stderrLog = Join-Path $OutDir "$baseName.stderr.log"
$pmStdout = Join-Path $OutDir "$baseName.pm.stdout.log"
$pmStderr = Join-Path $OutDir "$baseName.pm.stderr.log"

$totalDur = $WarmupSeconds + $Seconds + 2
$cvArgs = @(
    "-renderer", $Renderer,
    "-source", $Source,
    "-no-audio",
    "-stats=true",
    "-fullscreen=$($Fullscreen.IsPresent.ToString().ToLower())",
    "-vsync=$($VSync.IsPresent.ToString().ToLower())",
    "-duration", "${totalDur}s"
)
if ($Display) {
    $cvArgs += @("-display", $Display)
}

$prevFse = $env:CAPVIEW_VK_FSE
if ($VkFse) {
    $env:CAPVIEW_VK_FSE = $VkFse
}

Write-Host "=== Measuring $Renderer ($modeTag) for ${Seconds}s (warmup ${WarmupSeconds}s) ==="
$cvProc = $null
try {
    $cvProc = Start-Process -FilePath $CapviewExe -ArgumentList $cvArgs `
        -RedirectStandardOutput $stdoutLog -RedirectStandardError $stderrLog -PassThru

    Start-Sleep -Milliseconds 700
    if ($cvProc.HasExited) {
        $errTxt = if (Test-Path $stderrLog) { Get-Content $stderrLog -Raw } else { "" }
        throw "capview exited early (code $($cvProc.ExitCode)): $errTxt"
    }

    $pmArgs = @(
        "--process_id", $cvProc.Id,
        "--output_file", $csvPath,
        "--delay", $WarmupSeconds,
        "--timed", $Seconds,
        "--terminate_after_timed",
        "--terminate_on_proc_exit",
        "--no_console_stats",
        "--stop_existing_session",
        "--session_name", "capview_spike"
    )
    $pmProc = Start-Process -FilePath $PresentMonExe -ArgumentList $pmArgs `
        -RedirectStandardOutput $pmStdout -RedirectStandardError $pmStderr -Wait -PassThru

    if (-not $cvProc.HasExited) {
        $cvProc.WaitForExit(10000) | Out-Null
        if (-not $cvProc.HasExited) {
            Stop-Process -Id $cvProc.Id -Force -ErrorAction SilentlyContinue
        }
    }

    if ($pmProc.ExitCode -ne 0) {
        $pmErr = if (Test-Path $pmStderr) { Get-Content $pmStderr -Raw } else { "" }
        $pmOut = if (Test-Path $pmStdout) { Get-Content $pmStdout -Raw } else { "" }
        Write-Warning "PresentMon exited with code $($pmProc.ExitCode).`nSTDOUT: $pmOut`nSTDERR: $pmErr"
    }
} finally {
    if ($VkFse) {
        if ($null -eq $prevFse) { Remove-Item Env:CAPVIEW_VK_FSE -ErrorAction SilentlyContinue }
        else { $env:CAPVIEW_VK_FSE = $prevFse }
    }
    if ($cvProc -and -not $cvProc.HasExited) {
        Stop-Process -Id $cvProc.Id -Force -ErrorAction SilentlyContinue
    }
}

function Get-Percentile {
    param([double[]]$Sorted, [double]$P)
    if ($Sorted.Count -eq 0) { return [double]::NaN }
    $idx = [int][Math]::Floor(($Sorted.Count - 1) * $P)
    if ($idx -lt 0) { $idx = 0 }
    if ($idx -ge $Sorted.Count) { $idx = $Sorted.Count - 1 }
    return $Sorted[$idx]
}

Write-Host "`n--- capview stdout ($stdoutLog) ---"
if (Test-Path $stdoutLog) {
    Get-Content $stdoutLog
}

Write-Host "`n--- PresentMon CSV summary ($csvPath) ---"
if (-not (Test-Path $csvPath)) {
    Write-Warning "PresentMon CSV not created."
    exit 1
}

$rows = Import-Csv $csvPath
$total = $rows.Count
Write-Host "Total CSV rows: $total"
if ($total -eq 0) {
    if (Test-Path $pmStderr) { Get-Content $pmStderr }
    exit 1
}

Write-Host "`nPresentMode distribution:"
$rows | Group-Object PresentMode | Sort-Object Count -Descending | ForEach-Object {
    $pct = ($_.Count * 100.0) / $total
    Write-Host ("  {0,-42} {1,5} ({2:N1}%)" -f $_.Name, $_.Count, $pct)
}

Write-Host "`nAllowsTearing distribution:"
$rows | Group-Object AllowsTearing | Sort-Object Name | ForEach-Object {
    Write-Host ("  AllowsTearing={0,-6} {1,5}" -f $_.Name, $_.Count)
}

Write-Host "`nSyncInterval distribution:"
$rows | Group-Object SyncInterval | Sort-Object Name | ForEach-Object {
    Write-Host ("  SyncInterval={0,-7} {1,5}" -f $_.Name, $_.Count)
}

$between = New-Object System.Collections.Generic.List[double]
$untilDisp = New-Object System.Collections.Generic.List[double]
$dispLat = New-Object System.Collections.Generic.List[double]
$inPresent = New-Object System.Collections.Generic.List[double]

foreach ($r in $rows) {
    $v = 0.0
    if ($r.MsBetweenPresents -and [double]::TryParse($r.MsBetweenPresents, [System.Globalization.NumberStyles]::Float, [System.Globalization.CultureInfo]::InvariantCulture, [ref]$v)) {
        $between.Add($v)
    }
    if ($r.MsUntilDisplayed -and [double]::TryParse($r.MsUntilDisplayed, [System.Globalization.NumberStyles]::Float, [System.Globalization.CultureInfo]::InvariantCulture, [ref]$v)) {
        $untilDisp.Add($v)
    }
    if ($r.DisplayLatency -and [double]::TryParse($r.DisplayLatency, [System.Globalization.NumberStyles]::Float, [System.Globalization.CultureInfo]::InvariantCulture, [ref]$v)) {
        $dispLat.Add($v)
    }
    if ($r.MsInPresentAPI -and [double]::TryParse($r.MsInPresentAPI, [System.Globalization.NumberStyles]::Float, [System.Globalization.CultureInfo]::InvariantCulture, [ref]$v)) {
        $inPresent.Add($v)
    }
}

$betweenArr = $between.ToArray(); [Array]::Sort($betweenArr)
$untilArr = $untilDisp.ToArray(); [Array]::Sort($untilArr)
$dispLatArr = $dispLat.ToArray(); [Array]::Sort($dispLatArr)
$inPresArr = $inPresent.ToArray(); [Array]::Sort($inPresArr)

Write-Host "`nLatency metrics (ms):"
if ($betweenArr.Count -gt 0) {
    Write-Host ("  MsBetweenPresents (n={0}): p50={1:N3} ms, p99={2:N3} ms" -f $betweenArr.Count, (Get-Percentile $betweenArr 0.50), (Get-Percentile $betweenArr 0.99))
}
if ($untilArr.Count -gt 0) {
    Write-Host ("  MsUntilDisplayed  (n={0}): p50={1:N3} ms, p99={2:N3} ms" -f $untilArr.Count, (Get-Percentile $untilArr 0.50), (Get-Percentile $untilArr 0.99))
} else {
    Write-Host "  MsUntilDisplayed  (n=0): NA (not available / no displayed frames)"
}
if ($dispLatArr.Count -gt 0) {
    Write-Host ("  DisplayLatency    (n={0}): p50={1:N3} ms, p99={2:N3} ms" -f $dispLatArr.Count, (Get-Percentile $dispLatArr 0.50), (Get-Percentile $dispLatArr 0.99))
}
if ($inPresArr.Count -gt 0) {
    Write-Host ("  MsInPresentAPI    (n={0}): p50={1:N3} ms, p99={2:N3} ms" -f $inPresArr.Count, (Get-Percentile $inPresArr 0.50), (Get-Percentile $inPresArr 0.99))
}
