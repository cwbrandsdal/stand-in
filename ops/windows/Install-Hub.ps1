#requires -Version 7.0
<#
.SYNOPSIS
    Builds this fork and installs or updates it as the always-on CLIProxyAPI hub on this Windows machine.

.DESCRIPTION
    Idempotent. Each run:
      1. Builds cmd/server into <RuntimeDir>\bin (stops the running hub only for the binary swap).
      2. Creates missing client keys, the management key and the read-only insights viewer key in Windows
         SecretStore (vault LocalSecrets). Existing secrets are reused; values are never printed.
      3. Writes config.yaml and .env only when they do not exist yet, so later edits are preserved. Two
         exceptions: the INSIGHTS_VIEWER_KEY_SHA256 line in .env is kept in sync with the viewer key, and an
         INSIGHTS_BACKUP_DIR line is added when .env has none (comment it out to turn nightly backups off).
      4. Registers the "CLIProxyAPI Hub" logon task running Start-Hub.ps1, then starts it.

    Client keys look like cpa-<name>-<random>. The insights dashboard shows <name> per request, so give
    every machine its own key. The management key is hashed into config.yaml on first start. The viewer key
    opens only the insights dashboard and its read-only endpoints; .env holds its SHA-256, never the key.

    Not done here (needs elevation): the inbound firewall rule. Run Add-HubFirewallRule.ps1 as administrator.

.EXAMPLE
    ./ops/windows/Install-Hub.ps1 -HostLabels @{ '192.168.1.170' = 'ProArt'; '192.168.1.171' = 'Deathstar' }

.EXAMPLE
    ./ops/windows/Install-Hub.ps1 -SkipBuild    # re-register the task or regenerate missing files only
#>
[CmdletBinding()]
param(
    [string]$RuntimeDir = 'D:\Knowledge\00-Control\services\cliproxyapi',
    [string[]]$ClientNames = @('r2-d2', 'proart', 'deathstar'),
    [hashtable]$HostLabels = @{},
    [int]$Port = 8317,
    [string]$Vault = 'LocalSecrets',
    [string]$TaskName = 'CLIProxyAPI Hub',
    [string]$BackupDir = 'D:\Knowledge\70-Backups\cliproxyapi',
    [switch]$SkipBuild,
    [switch]$NoStart
)

$ErrorActionPreference = 'Stop'
$repo = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..\..'))
$RuntimeDir = [IO.Path]::GetFullPath($RuntimeDir)
$binDir = Join-Path $RuntimeDir 'bin'
$exe = Join-Path $binDir 'cli-proxy-api.exe'
$configPath = Join-Path $RuntimeDir 'config.yaml'
$envPath = Join-Path $RuntimeDir '.env'

foreach ($name in $ClientNames) {
    if ($name -cnotmatch '^[a-z0-9][a-z0-9-]{0,30}[a-z0-9]$') { throw "Client name '$name' must be lowercase letters, digits and dashes." }
}
foreach ($dir in $RuntimeDir, $binDir, (Join-Path $RuntimeDir 'auths'), (Join-Path $RuntimeDir 'data'), (Join-Path $RuntimeDir 'logs')) {
    New-Item -ItemType Directory -Force -Path $dir | Out-Null
}

function Stop-Hub {
    if (Get-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue) { Stop-ScheduledTask -TaskName $TaskName }
    Get-Process -Name 'cli-proxy-api' -ErrorAction SilentlyContinue |
        Where-Object { $_.Path -and [IO.Path]::GetFullPath($_.Path) -eq $exe } |
        ForEach-Object { $_ | Stop-Process -Force; $_.WaitForExit(10000) | Out-Null }
}

# 1. Build
if (-not $SkipBuild) {
    $go = (Get-Command go -ErrorAction SilentlyContinue).Source
    if (-not $go) { $go = Join-Path $env:LOCALAPPDATA 'Programs\go\bin\go.exe' }
    if (-not (Test-Path -LiteralPath $go)) { throw 'Go toolchain not found. Install Go 1.26+ first.' }
    $commit = (git -C $repo rev-parse --short HEAD).Trim()
    $dirty = if (git -C $repo status --porcelain) { '-dirty' } else { '' }
    $branch = (git -C $repo rev-parse --abbrev-ref HEAD).Trim()
    $buildDate = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')
    $staged = Join-Path $binDir 'cli-proxy-api.new.exe'
    $ldflags = "-s -w -X 'main.Version=$branch' -X 'main.Commit=$commit$dirty' -X 'main.BuildDate=$buildDate'"
    Push-Location $repo
    try {
        & $go build -ldflags $ldflags -o $staged ./cmd/server
        if ($LASTEXITCODE -ne 0) { throw "go build failed with exit code $LASTEXITCODE" }
    } finally { Pop-Location }
    Stop-Hub
    Move-Item -LiteralPath $staged -Destination $exe -Force
    Write-Host "Installed $exe ($branch $commit$dirty)"
}

# 2. Secrets
Import-Module Microsoft.PowerShell.SecretManagement -ErrorAction Stop
function New-RandomToken([int]$Length) {
    $alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789'
    $chars = [System.Text.StringBuilder]::new()
    $buffer = [byte[]]::new(64)
    while ($chars.Length -lt $Length) {
        [System.Security.Cryptography.RandomNumberGenerator]::Fill($buffer)
        foreach ($b in $buffer) {
            # Rejection sampling keeps the alphabet uniform.
            if ($b -lt 248 -and $chars.Length -lt $Length) { [void]$chars.Append($alphabet[$b % 62]) }
        }
    }
    $chars.ToString()
}
function Get-OrCreateSecret([string]$SecretName, [string]$NewValue) {
    if (Get-SecretInfo -Name $SecretName -Vault $Vault -ErrorAction SilentlyContinue) {
        return Get-Secret -Name $SecretName -Vault $Vault -AsPlainText
    }
    Set-Secret -Name $SecretName -Vault $Vault -Secret (ConvertTo-SecureString $NewValue -AsPlainText -Force)
    Write-Host "Created secret $Vault/$SecretName"
    $NewValue
}
$managementKey = Get-OrCreateSecret 'CLIProxyAPI.Hub.ManagementKey' (New-RandomToken 40)
$clientKeys = foreach ($clientName in $ClientNames) {
    $clientKey = Get-OrCreateSecret "CLIProxyAPI.Hub.ClientKey.$clientName" "cpa-$clientName-$(New-RandomToken 32)"
    if ($clientKey -cnotmatch "^cpa-$([regex]::Escape($clientName))-[A-Za-z0-9]{32}$") {
        throw "Secret CLIProxyAPI.Hub.ClientKey.$clientName has an unexpected format; remove it and rerun."
    }
    $clientKey
}

# 3. Config and .env (created once; edit them in place afterwards)
if (-not (Test-Path -LiteralPath $configPath)) {
    $authDir = (Join-Path $RuntimeDir 'auths').Replace('\', '/')
    $keyLines = ($clientKeys | ForEach-Object { "    - `"$_`"" }) -join "`n"
    $yaml = @"
# CLIProxyAPI hub configuration (v8 layout). Created by ops/windows/Install-Hub.ps1.
# See config.example.yaml in the repository for every option.
config-version: 8

server:
  # Listen on all interfaces; the Windows Firewall rule limits which LAN hosts can connect.
  host: ""
  port: $Port
  discovery:
    enabled: false

management:
  # Remote management lets T3 Code on other machines read pooled limits with the management key.
  allow-remote: true
  # Replaced by its bcrypt hash on first start.
  secret-key: "$managementKey"
  disable-control-panel: false

access:
  # One key per client machine: cpa-<machine>-<secret>.
  api-keys:
$keyLines

routing:
  strategy: "round-robin"
  # Keep each session on one account so its prompt cache is reused.
  session-affinity: true
  session-affinity-ttl: "1h"
  # Transient errors fail over for one request without moving the session; quota/auth errors rebind.
  session-affinity-rebind-on: "quota"
  retry:
    request-retry: 3
    max-retry-interval: 30

oauth:
  auth-dir: "$authDir"

observability:
  logs:
    logging-to-file: true
    logs-max-total-size-mb: 200
    request-log: false
"@
    Set-Content -LiteralPath $configPath -Value $yaml -Encoding utf8NoBOM
    Write-Host "Wrote $configPath"
} else {
    Write-Host "Kept existing $configPath"
}
$managementKey = $null
$clientKeys = $null

if (-not (Test-Path -LiteralPath $envPath)) {
    $labels = @{ '127.0.0.1' = $env:COMPUTERNAME; '::1' = $env:COMPUTERNAME }
    foreach ($entry in $HostLabels.GetEnumerator()) { $labels[[string]$entry.Key] = [string]$entry.Value }
    $pairs = ($labels.GetEnumerator() | Sort-Object Name | ForEach-Object { "$($_.Name)=$($_.Value)" }) -join ','
    @(
        '# Non-secret process settings for the CLIProxyAPI hub (loaded from the working directory).'
        '# Names client IPs on the insights dashboard.'
        "INSIGHTS_HOST_LABELS=$pairs"
        '# Optional: delete insights rows older than N days (default keeps everything).'
        '# INSIGHTS_RETENTION_DAYS=365'
    ) | Set-Content -LiteralPath $envPath -Encoding utf8NoBOM
    Write-Host "Wrote $envPath"
}

# The read-only viewer key opens only /v8/management/insights/*, so it is the key for phones and remote access.
$viewerKey = Get-OrCreateSecret 'CLIProxyAPI.Hub.InsightsViewerKey' (New-RandomToken 40)
$viewerHash = [Convert]::ToHexString([System.Security.Cryptography.SHA256]::HashData([Text.Encoding]::UTF8.GetBytes($viewerKey))).ToLowerInvariant()
$viewerKey = $null
$viewerLine = "INSIGHTS_VIEWER_KEY_SHA256=$viewerHash"
$envLines = [Collections.Generic.List[string]]::new([string[]]@(Get-Content -LiteralPath $envPath))
$viewerIndex = $envLines.FindIndex({ param($line) $line -match '^\s*INSIGHTS_VIEWER_KEY_SHA256\s*=' })
$envChanges = [Collections.Generic.List[string]]::new()
if ($viewerIndex -lt 0) {
    $envLines.Add('# SHA-256 of the read-only insights viewer key (secret CLIProxyAPI.Hub.InsightsViewerKey). Managed by Install-Hub.ps1.')
    $envLines.Add($viewerLine)
    $envChanges.Add('INSIGHTS_VIEWER_KEY_SHA256')
} elseif ($envLines[$viewerIndex] -ne $viewerLine) {
    $envLines[$viewerIndex] = $viewerLine
    $envChanges.Add('INSIGHTS_VIEWER_KEY_SHA256')
}
# Nightly database backups (VACUUM INTO); a commented-out line counts as a deliberate choice and is kept.
if ($BackupDir -and -not ($envLines | Where-Object { $_ -match '^\s*#?\s*INSIGHTS_BACKUP_DIR\s*=' })) {
    $envLines.Add('# Nightly copy of insights.db after 03:00 local time, keeping the newest 14 (INSIGHTS_BACKUP_HOUR, INSIGHTS_BACKUP_KEEP).')
    $envLines.Add("INSIGHTS_BACKUP_DIR=$([IO.Path]::GetFullPath($BackupDir))")
    $envChanges.Add('INSIGHTS_BACKUP_DIR')
}
if ($envChanges.Count) {
    Set-Content -LiteralPath $envPath -Value $envLines -Encoding utf8NoBOM
    Write-Host "Updated $($envChanges -join ', ') in $envPath (takes effect when the hub restarts)"
}

# 4. Logon task
$pwsh = (Get-Command pwsh -ErrorAction Stop).Source
$startScript = Join-Path $PSScriptRoot 'Start-Hub.ps1'
$action = New-ScheduledTaskAction -Execute $pwsh `
    -Argument "-NoProfile -WindowStyle Hidden -ExecutionPolicy Bypass -File `"$startScript`" -RuntimeDir `"$RuntimeDir`""
$trigger = New-ScheduledTaskTrigger -AtLogOn -User "$env:USERDOMAIN\$env:USERNAME"
$settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -StartWhenAvailable `
    -ExecutionTimeLimit ([TimeSpan]::Zero) -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1) -MultipleInstances IgnoreNew
$principal = New-ScheduledTaskPrincipal -UserId "$env:USERDOMAIN\$env:USERNAME" -LogonType Interactive -RunLevel Limited
Register-ScheduledTask -TaskName $TaskName -Action $action -Trigger $trigger -Settings $settings -Principal $principal `
    -Description 'CLIProxyAPI hub for Claude Code, Codex and T3 Code (ops/windows/Start-Hub.ps1).' -Force | Out-Null
Write-Host "Registered scheduled task '$TaskName'"

if (-not $NoStart) {
    Start-ScheduledTask -TaskName $TaskName
    $deadline = (Get-Date).AddSeconds(30)
    do {
        Start-Sleep -Milliseconds 500
        try { $health = Invoke-RestMethod -Uri "http://127.0.0.1:$Port/healthz" -TimeoutSec 2 } catch { $health = $null }
    } until ($health -or (Get-Date) -gt $deadline)
    if ($health) { Write-Host "Hub is healthy on port $Port" } else { Write-Warning "Hub did not answer /healthz within 30s; see $RuntimeDir\logs" }
}
