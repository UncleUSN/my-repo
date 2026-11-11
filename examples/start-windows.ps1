# Запуск агента в окне PowerShell. Остановка — Ctrl+C.
# Каталог скрипта должен содержать ferry.exe и ferry.yaml.
$ErrorActionPreference = "Stop"
Set-Location $PSScriptRoot
& .\ferry.exe serve --config .\ferry.yaml
