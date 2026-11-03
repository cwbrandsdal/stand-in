#requires -Version 7.0
<#
.SYNOPSIS
    Connects the hub's insights alerts to Luna's operations sentinel.

.DESCRIPTION
    Runs Luna's CLI to create a generic alert source named 'CLIProxyAPI hub' (against Luna's own config and
    database, like the running Core), stores the source token in SecretStore as Luna.Alerts.Hub.Token without
    displaying it, writes INSIGHTS_LUNA_ALERTS_URL to the hub's .env and restarts the hub. Start-Hub.ps1 passes
    the token to the hub process as INSIGHTS_LUNA_TOKEN. Idempotent: an existing connection is kept.

    Luna keeps only the token's hash. Alert text goes to Luna Core on this machine, which may speak it or push it
    to the paired iPhone.

.EXAMPLE
    ./ops/windows/Connect-HubToLuna.ps1            # connect (or report the existing connection)

.EXAMPLE
    ./ops/windows/Connect-HubToLuna.ps1 -Rotate    # new source and token; the old source is revoked

.EXAMPLE
    ./ops/windows/Connect-HubToLuna.ps1 -Remove    # revoke the source and disconnect
#>
[CmdletBinding(DefaultParameterSetName = 'Connect')]
param(
    [string]$LunaRepo = 'D:\Github\luna',
    [string]$LunaUrl = 'http://127.0.0.1:8787',
    [string]$SourceName = 'CLIProxyAPI hub',
    [string]$RuntimeDir = 'D:\Knowledge\00-Control\services\cliproxyapi',
    [string]$Vault = 'LocalSecrets',
    [string]$TaskName = 'CLIProxyAPI Hub',
    [Parameter(ParameterSetName = 'Connect')][switch]$Rotate,
    [Parameter(ParameterSetName = 'Remove')][switch]$Remove,
    [switch]$NoRestart
)

$ErrorActionPreference = 'Stop'
Import-Module Microsoft.PowerShell.SecretManagement -ErrorAction Stop
$secretName = 'Luna.Alerts.Hub.Token'
$envPath = Join-Path ([IO.Path]::GetFullPath($RuntimeDir)) '.env'
if (-not (Test-Path -LiteralPath $envPath)) { throw "Missing $envPath. Run Install-Hub.ps1 first." }
if (-not (Test-Path -LiteralPath (Join-Path $LunaRepo 'apps\core\src\cli.ts'))) { throw "Luna's CLI not found under $LunaRepo." }
$envLines = [Collections.Generic.List[string]]::new([string[]]@(Get-Content -LiteralPath $envPath))
$urlIndex = $envLines.FindIndex({ param($line) $line -match '^\s*INSIGHTS_LUNA_ALERTS_URL\s*=' })
$oldSource = if ($urlIndex -ge 0 -and $envLines[$urlIndex] -match '/v1/alerts/([^/\s]+)\s*$') { $Matches[1] } else { $null }
$hasSecret = [bool](Get-SecretInfo -Name $secretName -Vault $Vault -ErrorAction SilentlyContinue)

# Runs Luna's CLI from its repository; output is returned, never printed, because 'add' prints the token.
function Invoke-LunaCli([string[]]$Arguments) {
    Push-Location $LunaRepo
    try {
        $output = & pnpm --silent --filter @luna/core cli @Arguments 2>&1 | ForEach-Object { "$_" }
        if ($LASTEXITCODE -ne 0) { throw "Luna CLI failed (exit $LASTEXITCODE): $(($output | Where-Object { $_ -notmatch 'Token|Bearer' }) -join ' ')" }
        $output
    } finally { Pop-Location }
}
function Restart-Hub {
    if ($NoRestart) { Write-Host 'Restart the hub to apply the change.'; return }
    if (-not (Get-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue)) { Write-Warning "Scheduled task '$TaskName' not found; start the hub with Start-Hub.ps1."; return }
    # Stopping the task ends the supervisor; Start-Hub.ps1 clears the orphaned hub process and reads the secret again.
    Stop-ScheduledTask -TaskName $TaskName
    Start-ScheduledTask -TaskName $TaskName
    Write-Host "Restarted '$TaskName'."
}
function Save-Env {
    Set-Content -LiteralPath $envPath -Value $envLines -Encoding utf8NoBOM
}

if ($Remove) {
    if ($oldSource) { Invoke-LunaCli @('alert-source', 'revoke', $oldSource) | Out-Null; Write-Host "Revoked Luna alert source $oldSource." }
    if ($hasSecret) { Remove-Secret -Name $secretName -Vault $Vault; Write-Host "Removed $Vault/$secretName." }
    if ($urlIndex -ge 0) { $envLines.RemoveAt($urlIndex); Save-Env; Write-Host "Removed INSIGHTS_LUNA_ALERTS_URL from $envPath." }
    Restart-Hub
    return
}

if ($oldSource -and $hasSecret -and -not $Rotate) {
    Write-Host "Already connected to Luna (source $oldSource). Use -Rotate for a new token or -Remove to disconnect."
    return
}

$output = Invoke-LunaCli @('alert-source', 'add', $SourceName, '--kind', 'generic')
$sourceId = ($output | Select-String -Pattern '^Id:\s*(\S+)' | Select-Object -First 1).Matches.Groups[1].Value
$token = ($output | Select-String -Pattern '^Token \(shown once\):\s*(\S+)' | Select-Object -First 1).Matches.Groups[1].Value
$output = $null
if (-not $sourceId -or -not $token) { $token = $null; throw "Could not read the new source from Luna's CLI output." }
Set-Secret -Name $secretName -Vault $Vault -Secret (ConvertTo-SecureString $token -AsPlainText -Force)
$token = $null
Write-Host "Created Luna alert source $sourceId and stored its token as $Vault/$secretName."

$line = "INSIGHTS_LUNA_ALERTS_URL=$($LunaUrl.TrimEnd('/'))/v1/alerts/$sourceId"
if ($urlIndex -ge 0) { $envLines[$urlIndex] = $line } else {
    $envLines.Add("# Luna's alert webhook for the hub's source; the token is Luna.Alerts.Hub.Token in SecretStore. Managed by Connect-HubToLuna.ps1.")
    $envLines.Add($line)
}
Save-Env
Write-Host "Wrote INSIGHTS_LUNA_ALERTS_URL to $envPath."

if ($oldSource -and $oldSource -ne $sourceId) {
    Invoke-LunaCli @('alert-source', 'revoke', $oldSource) | Out-Null
    Write-Host "Revoked the previous Luna alert source $oldSource."
}
Restart-Hub
Write-Host 'Use "Send test alert to Luna" on the dashboard to check the connection.'
