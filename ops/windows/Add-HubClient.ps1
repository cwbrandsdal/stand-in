#requires -Version 7.0
<#
.SYNOPSIS
    Adds a client machine to the CLIProxyAPI hub: its own client key, dashboard name and firewall access.

.DESCRIPTION
    Idempotent. Each run:
      1. Creates CLIProxyAPI.Hub.ClientKey.<Name> (cpa-<Name>-<32 random characters>) in SecretStore, or reuses it.
         The value is never printed.
      2. Adds the key to access.api-keys in config.yaml (backed up first). The hub reloads config.yaml by itself,
         so no restart is needed; the script waits until the hub accepts the key.
      3. With -Address: names the address in INSIGHTS_HOST_LABELS in .env (shown after the hub's next restart, and
         then for earlier requests too) and adds it to the 'CLIProxyAPI Hub (LAN clients)' firewall rule, keeping
         the addresses already there. Changing the rule needs elevation; Windows asks for it.
         -SkipFirewall leaves the firewall alone.
      4. With -CopyKey: copies the key to the clipboard so you can save it in the password manager, waits for
         Enter, then clears the clipboard.

    Run it as the user who owns the hub, not elevated.

.EXAMPLE
    ./ops/windows/Add-HubClient.ps1 -Name mac-mini -Label 'Mac mini' -Address 192.168.1.172 -CopyKey
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$Name,
    [string]$Label,
    [string]$Address,
    [switch]$CopyKey,
    [switch]$RestartHub,
    [switch]$SkipFirewall,
    [string]$RuntimeDir = 'D:\Knowledge\00-Control\services\cliproxyapi',
    [string]$Vault = 'LocalSecrets',
    [int]$Port = 8317,
    [string]$TaskName = 'CLIProxyAPI Hub',
    [string]$FirewallRule = 'CLIProxyAPI Hub (LAN clients)'
)

$ErrorActionPreference = 'Stop'
if ($Name -cnotmatch '^[a-z0-9][a-z0-9-]{0,30}[a-z0-9]$') { throw "Name '$Name' must be lowercase letters, digits and dashes (2-32 characters)." }
if ($Address -and -not ($Address -as [ipaddress])) { throw "Address '$Address' is not an IP address." }
if (-not $Label) { $Label = $Name }
if ($Label -match '[,=\r\n]') { throw 'Label must not contain commas, equals signs or line breaks.' }
$RuntimeDir = [IO.Path]::GetFullPath($RuntimeDir)
$configPath = Join-Path $RuntimeDir 'config.yaml'
$envPath = Join-Path $RuntimeDir '.env'
foreach ($path in $configPath, $envPath) { if (-not (Test-Path -LiteralPath $path)) { throw "Missing $path. Run Install-Hub.ps1 first." } }
$stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
$utf8 = [Text.UTF8Encoding]::new($false)

# 1. Key
Import-Module Microsoft.PowerShell.SecretManagement -ErrorAction Stop
$secretName = "CLIProxyAPI.Hub.ClientKey.$Name"
if (Get-SecretInfo -Name $secretName -Vault $Vault -ErrorAction SilentlyContinue) {
    Write-Host "Using the existing secret $Vault/$secretName"
} else {
    $alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789'
    $chars = [Text.StringBuilder]::new()
    $buffer = [byte[]]::new(64)
    while ($chars.Length -lt 32) {
        [Security.Cryptography.RandomNumberGenerator]::Fill($buffer)
        # Rejection sampling keeps the alphabet uniform.
        foreach ($b in $buffer) { if ($b -lt 248 -and $chars.Length -lt 32) { [void]$chars.Append($alphabet[$b % 62]) } }
    }
    Set-Secret -Name $secretName -Vault $Vault -Secret (ConvertTo-SecureString "cpa-$Name-$chars" -AsPlainText -Force)
    $chars = $null
    Write-Host "Created secret $Vault/$secretName"
}
$key = Get-Secret -Name $secretName -Vault $Vault -AsPlainText
if ($key -cnotmatch "^cpa-$([regex]::Escape($Name))-[A-Za-z0-9]{32}$") { throw "Secret $secretName has an unexpected format; remove it and rerun." }

try {
    # 2. config.yaml: append the key to access.api-keys
    $lines = [Collections.Generic.List[string]]::new([string[]][IO.File]::ReadAllLines($configPath))
    $start = $lines.FindIndex({ param($line) $line -match '^\s+api-keys:\s*$' })
    if ($start -lt 0) { throw "No access.api-keys list in $configPath; add the key by hand." }
    $indent = $null; $last = $start; $present = $false
    for ($i = $start + 1; $i -lt $lines.Count; $i++) {
        if ($lines[$i] -match '^(\s+)-\s*"?([^"\s]+)"?\s*$') {
            $indent = $Matches[1]; $last = $i
            if ($Matches[2] -ceq $key) { $present = $true }
        } elseif ($lines[$i].Trim() -and -not $lines[$i].TrimStart().StartsWith('#')) { break }
    }
    if ($present) {
        Write-Host "config.yaml already accepts the $Name key"
    } else {
        if (-not $indent) { $indent = (([regex]::Match($lines[$start], '^\s*')).Value) + '  ' }
        Copy-Item -LiteralPath $configPath -Destination "$configPath.bak-$stamp"
        $lines.Insert($last + 1, "$indent- `"$key`"")
        [IO.File]::WriteAllLines($configPath, [string[]]$lines, $utf8)
        Write-Host "Added the $Name key to access.api-keys (backup: config.yaml.bak-$stamp)"
    }
    $lines = $null

    # The hub reloads config.yaml on change; wait until it accepts the key.
    $accepted = $false
    $deadline = (Get-Date).AddSeconds(30)
    while (-not $accepted -and (Get-Date) -lt $deadline) {
        try {
            $models = Invoke-RestMethod "http://127.0.0.1:$Port/v1/models" -Headers @{ Authorization = "Bearer $key" } -TimeoutSec 3
            $accepted = $true
        } catch {
            $status = $_.Exception.Response.StatusCode.value__
            if ($status -and $status -ne 401) { throw "The hub answered HTTP $status to the new key." }
            Start-Sleep -Seconds 1
        }
    }
    if ($accepted) { Write-Host "The hub accepts the $Name key ($(@($models.data).Count) models)" }
    else { Write-Warning "The hub did not accept the $Name key within 30 seconds. Is it running? Restart it: Stop-ScheduledTask '$TaskName'; Start-ScheduledTask '$TaskName'" }

    # 3. Dashboard name and firewall
    if ($Address) {
        $envLines = [Collections.Generic.List[string]]::new([string[]][IO.File]::ReadAllLines($envPath))
        $labelIndex = $envLines.FindIndex({ param($line) $line -match '^\s*INSIGHTS_HOST_LABELS\s*=' })
        $pairs = [ordered]@{}
        if ($labelIndex -ge 0) {
            foreach ($pair in (($envLines[$labelIndex] -split '=', 2)[1] -split ',')) {
                $parts = $pair.Trim() -split '=', 2
                if ($parts.Count -eq 2 -and $parts[0]) { $pairs[$parts[0].Trim()] = $parts[1].Trim() }
            }
        }
        if ($pairs[$Address] -ceq $Label) {
            Write-Host "INSIGHTS_HOST_LABELS already names $Address '$Label'"
        } else {
            $pairs[$Address] = $Label
            $line = 'INSIGHTS_HOST_LABELS=' + (($pairs.GetEnumerator() | ForEach-Object { "$($_.Key)=$($_.Value)" }) -join ',')
            if ($labelIndex -ge 0) { $envLines[$labelIndex] = $line } else { $envLines.Add($line) }
            Copy-Item -LiteralPath $envPath -Destination "$envPath.bak-$stamp"
            [IO.File]::WriteAllLines($envPath, [string[]]$envLines, $utf8)
            Write-Host "Named $Address '$Label' in INSIGHTS_HOST_LABELS (shown after the hub restarts)"
        }

        $rule = Get-NetFirewallRule -DisplayName $FirewallRule -ErrorAction SilentlyContinue
        $current = if ($rule) { @(($rule | Get-NetFirewallAddressFilter).RemoteAddress) } else { @() }
        if ($SkipFirewall) {
            Write-Host 'Firewall left unchanged (-SkipFirewall)'
        } elseif ($current -contains 'Any') {
            Write-Host "The firewall rule already allows any address"
        } elseif ($current -contains $Address) {
            Write-Host "The firewall rule already allows $Address"
        } else {
            $wanted = @($current) + $Address
            $firewallScript = Join-Path $PSScriptRoot 'Add-HubFirewallRule.ps1'
            Write-Host "Allowing $Address through the firewall. Approve the Windows prompt."
            try {
                Start-Process -FilePath (Get-Command pwsh).Source -Verb RunAs -Wait -WindowStyle Hidden -ArgumentList @(
                    '-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', "`"$firewallScript`"", '-RemoteAddress', ($wanted -join ','))
            } catch { Write-Warning "Firewall not changed ($($_.Exception.Message)). From an elevated PowerShell run: & '$firewallScript' -RemoteAddress $($wanted -join ',')" }
            $after = @((Get-NetFirewallRule -DisplayName $FirewallRule -ErrorAction SilentlyContinue | Get-NetFirewallAddressFilter).RemoteAddress)
            if ($after -contains $Address) { Write-Host "Firewall allows TCP $Port from $($after -join ', ')" }
            else { Write-Warning "The firewall rule does not allow $Address yet." }
        }
    }

    # 4. Password manager copy
    if ($CopyKey) {
        Set-Clipboard -Value $key
        Read-Host "The $Name key is on the clipboard. Save it in the password manager as $secretName, then press Enter to clear the clipboard" | Out-Null
        Set-Clipboard -Value ' '
        Write-Host 'Clipboard cleared.'
    }
} finally {
    $key = $null
}

if ($RestartHub) {
    Stop-ScheduledTask -TaskName $TaskName
    Start-ScheduledTask -TaskName $TaskName
    Write-Host "Restarted '$TaskName'"
}
