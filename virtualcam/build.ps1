[CmdletBinding()]
param(
    [ValidateSet("x64", "x86")]
    [string]$Arch = "x64"
)

$ErrorActionPreference = "Stop"

$vswhere = "C:\Program Files (x86)\Microsoft Visual Studio\Installer\vswhere.exe"
if (-not (Test-Path $vswhere)) {
    throw "vswhere.exe not found at $vswhere"
}
$vsPath = & $vswhere -latest -products * -requires Microsoft.VisualStudio.Component.VC.Tools.x86.x64 -property installationPath
if (-not $vsPath) {
    $vsPath = & $vswhere -all -products * -property installationPath | Select-Object -First 1
}
if (-not $vsPath) {
    throw "No Visual Studio installation found"
}
$vcvars = Join-Path $vsPath "VC\Auxiliary\Build\vcvarsall.bat"
if (-not (Test-Path $vcvars)) {
    throw "vcvarsall.bat not found at $vcvars"
}

$outDir = Join-Path $PSScriptRoot "build\$Arch"
New-Item -ItemType Directory -Force -Path $outDir | Out-Null

$srcDir = $PSScriptRoot
$cmd = @"
call "$vcvars" $Arch >nul
if errorlevel 1 exit /b 1
cl.exe /nologo /std:c++20 /O2 /EHsc /W4 /DUNICODE /D_UNICODE /LD ^
  "$srcDir\dllmain.cpp" "$srcDir\vcam_source.cpp" "$srcDir\shm_reader.cpp" ^
  /Fe:"$outDir\capview_vcam.dll" /Fo:"$outDir\\" ^
  /link /DEF:"$srcDir\capview_vcam.def" ^
  mfplat.lib mf.lib mfuuid.lib ole32.lib advapi32.lib
if errorlevel 1 exit /b 1
cl.exe /nologo /std:c++20 /O2 /EHsc /W4 /DUNICODE /D_UNICODE ^
  "$srcDir\vcam_loopback_test.cpp" ^
  /Fe:"$outDir\vcam_loopback_test.exe" /Fo:"$outDir\\" ^
  /link mfplat.lib mf.lib mfreadwrite.lib mfuuid.lib ole32.lib
if errorlevel 1 exit /b 1
"@

$batPath = Join-Path $outDir "do_build.cmd"
Set-Content -Path $batPath -Value $cmd -Encoding ascii
cmd.exe /c $batPath
if ($LASTEXITCODE -ne 0) {
    throw "MSVC build failed with exit code $LASTEXITCODE"
}
Write-Host "Built:"
Get-ChildItem "$outDir\*" -Include *.dll,*.exe | ForEach-Object { Write-Host "  $($_.FullName) ($($_.Length) bytes)" }
