<#
.SYNOPSIS
    Installs capview_vcam.dll into C:\ProgramData\capview with LocalService ACLs
    and registers the COM Media Source CLSID in HKLM (or HKCU for spike tests).
#>
[CmdletBinding()]
param(
    [ValidateSet("Machine", "User")]
    [string]$Scope = "Machine",
    [string]$SourceDll = "",
    [string]$InstallDir = "C:\ProgramData\capview",
    [switch]$NoCopy
)

$ErrorActionPreference = "Stop"

if (-not $SourceDll) {
    $SourceDll = Join-Path $PSScriptRoot "build\x64\capview_vcam.dll"
}
if (-not (Test-Path $SourceDll)) {
    throw "capview_vcam.dll not found at $SourceDll. Run .\virtualcam\build.ps1 first."
}

$targetDll = $SourceDll
if (-not $NoCopy) {
    New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
    $targetDll = Join-Path $InstallDir "capview_vcam.dll"
    Copy-Item -Path $SourceDll -Destination $targetDll -Force
    Write-Host "Copied DLL to $targetDll"

    # Windows Camera Frame Server runs under NT AUTHORITY\LocalService (S-1-5-19).
    icacls $InstallDir /grant "*S-1-5-19:(OI)(CI)(M)" "*S-1-5-32-545:(OI)(CI)(RX)" | Out-Null
    Write-Host "Granted LocalService (S-1-5-19) Modify and Users Read&Execute on $InstallDir"
}

if ($Scope -eq "Machine") {
    $isAdmin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
    if (-not $isAdmin) {
        throw "HKLM COM registration requires an Administrator PowerShell prompt. Re-run elevated, or pass -Scope User for HKCU diagnostic testing."
    }
    $proc = Start-Process -FilePath "regsvr32.exe" -ArgumentList @("/s", "`"$targetDll`"") -Wait -PassThru
    if ($proc.ExitCode -ne 0) {
        throw "regsvr32 HKLM failed with exit code $($proc.ExitCode)"
    }
    Write-Host "Registered CLSID {7E4B8A12-3D5C-4F91-B2A7-9C6E1D4F8A03} in HKLM -> $targetDll"
} else {
    $proc = Start-Process -FilePath "regsvr32.exe" -ArgumentList @("/s", "/n", "/i:user", "`"$targetDll`"") -Wait -PassThru
    if ($proc.ExitCode -ne 0) {
        throw "regsvr32 /i:user HKCU failed with exit code $($proc.ExitCode)"
    }
    Write-Host "Registered CLSID {7E4B8A12-3D5C-4F91-B2A7-9C6E1D4F8A03} in HKCU -> $targetDll"
}
