<#
.SYNOPSIS
    Unregisters the capview Virtual Camera COM Media Source CLSID.
#>
[CmdletBinding()]
param(
    [ValidateSet("Machine", "User", "Both")]
    [string]$Scope = "Machine",
    [string]$DllPath = "C:\ProgramData\capview\capview_vcam.dll"
)

$ErrorActionPreference = "Stop"

if (-not (Test-Path $DllPath)) {
    $fallback = Join-Path $PSScriptRoot "build\x64\capview_vcam.dll"
    if (Test-Path $fallback) {
        $DllPath = $fallback
    } else {
        throw "capview_vcam.dll not found at $DllPath"
    }
}

if ($Scope -eq "Machine" -or $Scope -eq "Both") {
    $isAdmin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
    if (-not $isAdmin) {
        throw "HKLM COM unregistration requires an Administrator PowerShell prompt."
    }
    $proc = Start-Process -FilePath "regsvr32.exe" -ArgumentList @("/s", "/u", "`"$DllPath`"") -Wait -PassThru
    if ($proc.ExitCode -ne 0) {
        throw "regsvr32 /u HKLM failed with exit code $($proc.ExitCode)"
    }
    Write-Host "Unregistered HKLM CLSID {7E4B8A12-3D5C-4F91-B2A7-9C6E1D4F8A03}"
}

if ($Scope -eq "User" -or $Scope -eq "Both") {
    $proc = Start-Process -FilePath "regsvr32.exe" -ArgumentList @("/s", "/u", "/n", "/i:user", "`"$DllPath`"") -Wait -PassThru
    if ($proc.ExitCode -ne 0) {
        throw "regsvr32 /u /i:user HKCU failed with exit code $($proc.ExitCode)"
    }
    Write-Host "Unregistered HKCU CLSID {7E4B8A12-3D5C-4F91-B2A7-9C6E1D4F8A03}"
}
