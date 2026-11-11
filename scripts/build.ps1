# Сборка статических бинарников на Windows (нужен установленный Go).
# Запуск из PowerShell:  .\scripts\build.ps1
$ErrorActionPreference = "Stop"
$Root = Split-Path -Parent $PSScriptRoot
Set-Location $Root
New-Item -ItemType Directory -Force -Path dist | Out-Null
$env:CGO_ENABLED = "0"
$ldflags = "-s -w -X main.version=0.1.0"

$env:GOOS = "windows"
$env:GOARCH = "amd64"
go build -ldflags $ldflags -o dist\ferry-windows-amd64.exe .\cmd\ferry

$env:GOOS = "linux"
$env:GOARCH = "amd64"
go build -ldflags $ldflags -o dist\ferry-linux-amd64 .\cmd\ferry

$env:GOOS = "linux"
$env:GOARCH = "arm64"
go build -ldflags $ldflags -o dist\ferry-linux-arm64 .\cmd\ferry

Remove-Item Env:GOOS
Remove-Item Env:GOARCH
Write-Host "ready: dist\"
