#requires -Version 7.0
<#
.SYNOPSIS
    Runs the CLIProxyAPI hub under a small supervisor that restarts it when it exits.

.DESCRIPTION
    Started at logon by the "CLIProxyAPI Hub" scheduled task that Install-Hub.ps1 registers.
    The working directory is the runtime directory, so the server loads its .env from there.
    Supervisor events go to <RuntimeDir>\logs\supervisor.log.

    The optional Luna alert source token (secret Luna.Alerts.Hub.Token, see "Alerts and Luna" in README.md)
    is read from SecretStore before every start and passed to the hub process only, as INSIGHTS_LUNA_TOKEN.
    It is never written to .env.

.EXAMPLE
    ./ops/windows/Start-Hub.ps1
#>
[CmdletBinding()]
param(
    [string]$RuntimeDir = 'D:\Knowledge\00-Control\services\cliproxyapi',
    [string]$Vault = 'LocalSecrets'
)

$ErrorActionPreference = 'Stop'
# Task Scheduler starts tasks below normal priority and children inherit it; the proxy sits in every agent request.
$self = Get-Process -Id $PID
if ($self.PriorityClass -in 'BelowNormal', 'Idle') { $self.PriorityClass = 'Normal' }

$RuntimeDir = [IO.Path]::GetFullPath($RuntimeDir)
$exe = Join-Path $RuntimeDir 'bin\cli-proxy-api.exe'
$config = Join-Path $RuntimeDir 'config.yaml'
$logDir = Join-Path $RuntimeDir 'logs'
New-Item -ItemType Directory -Force -Path $logDir | Out-Null
$supervisorLog = Join-Path $logDir 'supervisor.log'

function Write-SupervisorLog([string]$Message) {
    Add-Content -LiteralPath $supervisorLog -Value ('{0:yyyy-MM-dd HH:mm:ss} {1}' -f (Get-Date), $Message)
}

foreach ($path in $exe, $config) {
    if (-not (Test-Path -LiteralPath $path)) {
        Write-SupervisorLog "missing $path; run ops\windows\Install-Hub.ps1"
        throw "Missing $path. Run ops\windows\Install-Hub.ps1 first."
    }
}

# Stopping the scheduled task ends this supervisor but not its child; clear such orphans before starting.
Get-Process -Name 'cli-proxy-api' -ErrorAction SilentlyContinue |
    Where-Object { $_.Path -and [IO.Path]::GetFullPath($_.Path) -eq $exe } |
    ForEach-Object {
        Write-SupervisorLog "stopping orphaned hub process $($_.Id)"
        $_ | Stop-Process -Force
        $_.WaitForExit(10000) | Out-Null
    }

# Loads optional secrets into this process's environment, which the hub inherits.
function Import-HubSecrets {
    $map = [ordered]@{ 'Luna.Alerts.Hub.Token' = 'INSIGHTS_LUNA_TOKEN' }
    try {
        Import-Module Microsoft.PowerShell.SecretManagement -ErrorAction Stop
        foreach ($entry in $map.GetEnumerator()) {
            if (Get-SecretInfo -Name $entry.Key -Vault $Vault -ErrorAction SilentlyContinue) {
                Set-Item -LiteralPath "Env:$($entry.Value)" -Value (Get-Secret -Name $entry.Key -Vault $Vault -AsPlainText -ErrorAction Stop)
            } else {
                Remove-Item -LiteralPath "Env:$($entry.Value)" -ErrorAction SilentlyContinue
            }
        }
    } catch {
        Write-SupervisorLog "optional secrets not loaded: $($_.Exception.Message)"
    }
}

$delaySeconds = 2
while ($true) {
    Import-HubSecrets
    $started = Get-Date
    Write-SupervisorLog "starting $exe"
    $process = Start-Process -FilePath $exe -ArgumentList @('-config', $config) -WorkingDirectory $RuntimeDir `
        -NoNewWindow -PassThru `
        -RedirectStandardOutput (Join-Path $logDir 'stdout.log') `
        -RedirectStandardError (Join-Path $logDir 'stderr.log')
    $process.WaitForExit()
    Write-SupervisorLog "exited with code $($process.ExitCode)"
    # Back off on crash loops; reset after a healthy run.
    if (((Get-Date) - $started).TotalMinutes -ge 10) { $delaySeconds = 2 } else { $delaySeconds = [Math]::Min($delaySeconds * 2, 300) }
    Start-Sleep -Seconds $delaySeconds
}
