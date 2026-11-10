param(
    [Parameter(Mandatory)][string]$Version,
    [Parameter(Mandatory)][string]$OutputDir
)
$ErrorActionPreference = 'Stop'
if ($Version -notmatch '^\d+\.\d+\.\d+(-test\.[0-9a-f]+)?$') { throw 'Invalid package version' }

New-Item -ItemType Directory -Force -Path dist/windows, $OutputDir | Out-Null
go build -trimpath -ldflags "-s -w -X main.version=$Version" -o dist/windows/mobd.exe ./cmd/mobd
if ($LASTEXITCODE -ne 0) { throw 'Go build failed' }
if ((& ./dist/windows/mobd.exe --version) -ne "mobd $Version" -or $LASTEXITCODE -ne 0) {
    throw 'Binary version check failed'
}
& ./dist/windows/mobd.exe --help
if ($LASTEXITCODE -ne 0) { throw 'Binary help check failed' }

Copy-Item LICENSE dist/windows/LICENSE
Copy-Item packaging/windows/README.md dist/windows/README.md
$archive = Join-Path $OutputDir "mobd_${Version}_windows_amd64.zip"
Compress-Archive -LiteralPath dist/windows/mobd.exe, dist/windows/LICENSE, dist/windows/README.md -DestinationPath $archive

# Check the actual package layout and extracted binary before uploading it.
$checkDir = Join-Path $OutputDir 'windows-package-check'
Expand-Archive -LiteralPath $archive -DestinationPath $checkDir
$names = (Get-ChildItem $checkDir | Sort-Object Name).Name -join ','
if ($names -ne 'LICENSE,mobd.exe,README.md') { throw "Unexpected ZIP contents: $names" }
if ((& (Join-Path $checkDir 'mobd.exe') --version) -ne "mobd $Version" -or $LASTEXITCODE -ne 0) {
    throw 'Packaged binary version check failed'
}
