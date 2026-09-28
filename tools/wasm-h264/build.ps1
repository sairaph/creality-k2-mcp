# Builds internal/camera/decode/h264dec.wasm on native Windows PowerShell,
# without requiring Git Bash. Equivalent to build.sh; see that file for the
# full explanation of what is being built and why.
#
# Pinned sources (see dev_docs/t0-decoder-spike.md for the licence and
# patent analysis of what this produces):
#   - wasi-sdk 34.0    https://github.com/WebAssembly/wasi-sdk/releases/tag/wasi-sdk-34
#   - OpenH264 v2.6.0  https://github.com/cisco/openh264, commit 652bdb7719f30b52b08e506645a7322ff1b2cc6f
#
# Requires: git, curl.exe (bundled with Windows 10/11), and PowerShell's
# own Expand-Archive is not used because the asset is a .tar.gz; tar.exe
# (bundled with Windows 10/11) is used instead.
#
# Usage: pwsh -File tools/wasm-h264/build.ps1

$ErrorActionPreference = "Stop"

$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$RepoRoot = Resolve-Path (Join-Path $ScriptDir "..\..")
$CacheDir = Join-Path $RepoRoot "tools\.cache"
$SrcDir = Join-Path $CacheDir "src"
$BuildDir = Join-Path $CacheDir "build"
$ObjDir = Join-Path $BuildDir "obj"
$OutWasm = Join-Path $RepoRoot "internal\camera\decode\h264dec.wasm"

$WasiSdkVersion = "34.0"
$WasiSdkTag = "wasi-sdk-34"
$OpenH264Commit = "652bdb7719f30b52b08e506645a7322ff1b2cc6f" # v2.6.0

# Host asset + checksum. This script only targets a Windows x86_64 host
# (the machine running clang, not the wasm output, which is architecture
# neutral); see build.sh for the other host platforms.
$Asset = "x86_64-windows.tar.gz"
$SdkChecksum = "cccb5c323a9b34f0349a9b09e8804a0a7632c68c3310f4b5f437ed57d7e71d8f"

New-Item -ItemType Directory -Force -Path $CacheDir, $SrcDir, $ObjDir | Out-Null

$SdkArchive = Join-Path $CacheDir "wasi-sdk-$WasiSdkVersion-$Asset"
$SdkDirName = "wasi-sdk-$WasiSdkVersion-" + $Asset.Substring(0, $Asset.Length - 7)
$SdkDir = Join-Path $CacheDir $SdkDirName

if (-not (Test-Path $SdkDir)) {
    if (-not (Test-Path $SdkArchive)) {
        Write-Host "Downloading wasi-sdk $WasiSdkVersion ($Asset)..."
        $url = "https://github.com/WebAssembly/wasi-sdk/releases/download/$WasiSdkTag/wasi-sdk-$WasiSdkVersion-$Asset"
        curl.exe -L -o $SdkArchive $url
    }
    Write-Host "Verifying checksum..."
    $got = (Get-FileHash -Algorithm SHA256 $SdkArchive).Hash.ToLower()
    if ($got -ne $SdkChecksum) {
        throw "checksum mismatch for $SdkArchive`: want $SdkChecksum, got $got"
    }
    Write-Host "Extracting wasi-sdk..."
    tar.exe xzf $SdkArchive -C $CacheDir
}

$CXX = Join-Path $SdkDir "bin\wasm32-wasip1-clang++.exe"
if (-not (Test-Path $CXX)) {
    throw "expected compiler not found at $CXX"
}

$OpenH264Dir = Join-Path $SrcDir "openh264"
if (-not (Test-Path $OpenH264Dir)) {
    Write-Host "Cloning OpenH264..."
    git clone https://github.com/cisco/openh264.git $OpenH264Dir
}
Push-Location $OpenH264Dir
try {
    git fetch --depth 1 origin $OpenH264Commit
    git checkout --quiet $OpenH264Commit

    $Patch = Join-Path $ScriptDir "patches\0001-wasi-processor-count.patch"
    git apply --check --reverse $Patch
    $alreadyApplied = ($LASTEXITCODE -eq 0)
    if ($alreadyApplied) {
        Write-Host "WASI patch already applied."
    } else {
        Write-Host "Applying WASI processor-count patch..."
        git apply $Patch
    }
} finally {
    Pop-Location
}

$Includes = @(
    "-I$OpenH264Dir\codec\api\wels"
    "-I$OpenH264Dir\codec\common\inc"
    "-I$OpenH264Dir\codec\decoder\core\inc"
    "-I$OpenH264Dir\codec\decoder\plus\inc"
)
$CxxFlags = @("-O2", "-std=gnu++11", "-D_WASI_EMULATED_SIGNAL", "-fno-exceptions", "-fno-rtti")

Write-Host "Compiling OpenH264 decoder (wasm32-wasip1)..."
$Sources = @()
$Sources += Get-ChildItem (Join-Path $OpenH264Dir "codec\decoder\core\src\*.cpp")
$Sources += Get-ChildItem (Join-Path $OpenH264Dir "codec\decoder\plus\src\*.cpp")
$Sources += Get-ChildItem (Join-Path $OpenH264Dir "codec\common\src\*.cpp")
$Sources += Get-Item (Join-Path $ScriptDir "src\shim.cpp")

foreach ($src in $Sources) {
    $objName = [System.IO.Path]::GetFileNameWithoutExtension($src.Name) + ".o"
    $objPath = Join-Path $ObjDir $objName
    & $CXX @Includes @CxxFlags -c $src.FullName -o $objPath
    if ($LASTEXITCODE -ne 0) { throw "compile failed: $($src.FullName)" }
}

Write-Host "Linking..."
$LinkedWasm = Join-Path $BuildDir "h264dec.wasm"
$Objs = Get-ChildItem (Join-Path $ObjDir "*.o") | ForEach-Object { $_.FullName }
& $CXX -O2 -mexec-model=reactor "-Wl,--no-entry" "-Wl,--gc-sections" -lwasi-emulated-signal @Objs -o $LinkedWasm
if ($LASTEXITCODE -ne 0) { throw "link failed" }

$Strip = Join-Path $SdkDir "bin\llvm-strip.exe"
& $Strip -o $OutWasm $LinkedWasm
if ($LASTEXITCODE -ne 0) { throw "strip failed" }

$size = (Get-Item $OutWasm).Length
Write-Host "Wrote $OutWasm ($size bytes)"
