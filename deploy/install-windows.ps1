# Пример установки службы Windows. PowerShell от имени администратора.
# Обычный sc.exe create для этого бинарника не подходит: Windows останавливает
# процесс, который не отвечает диспетчеру служб (ошибка 1053). Команда
# "ferry service install" регистрирует службу правильно.
#Requires -RunAsAdministrator
$ErrorActionPreference = "Stop"

$Exe = if ($env:FERRY_EXE) { $env:FERRY_EXE } else { "C:\ferry\ferry.exe" }
$Config = if ($env:FERRY_CONFIG) { $env:FERRY_CONFIG } else { "C:\ferry\ferry.yaml" }

if (-not (Test-Path $Exe)) { throw "no binary: $Exe" }
if (-not (Test-Path $Config)) { throw "no config: $Config" }

& $Exe service install --config $Config
& $Exe service start
Write-Host "service ferry started"
