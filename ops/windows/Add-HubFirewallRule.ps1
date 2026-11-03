#requires -Version 5.1
#requires -RunAsAdministrator
<#
.SYNOPSIS
    Allows the listed LAN hosts to reach the CLIProxyAPI hub port. Run once from an elevated PowerShell.

.DESCRIPTION
    Creates or updates one inbound allow rule for TCP <Port> scoped to -RemoteAddress, on all network
    profiles (R2-D2's Ethernet profile is Public). It also removes the per-program block rules that
    Windows adds when its "allow access" prompt for cli-proxy-api.exe is dismissed, because block rules
    override allow rules. Other rules are left untouched.

.EXAMPLE
    .\Add-HubFirewallRule.ps1 -RemoteAddress 192.168.1.170, 192.168.1.171
#>
[CmdletBinding(SupportsShouldProcess)]
param(
    [Parameter(Mandatory)]
    [string[]]$RemoteAddress,
    [int]$Port = 8317,
    [string]$Program = 'D:\Knowledge\00-Control\services\cliproxyapi\bin\cli-proxy-api.exe',
    [string]$DisplayName = 'CLIProxyAPI Hub (LAN clients)'
)

$ErrorActionPreference = 'Stop'

# `pwsh -File` passes "a,b" as one string instead of an array, so split comma-joined values here.
$RemoteAddress = @($RemoteAddress -split '[,\s]+' | Where-Object { $_ })

$autoBlocks = Get-NetFirewallApplicationFilter -Program $Program -ErrorAction SilentlyContinue |
    Get-NetFirewallRule | Where-Object { $_.Direction -eq 'Inbound' -and $_.Action -eq 'Block' }
foreach ($rule in $autoBlocks) {
    if ($PSCmdlet.ShouldProcess($rule.DisplayName, 'Remove auto-created inbound block rule')) {
        Remove-NetFirewallRule -Name $rule.Name
        Write-Host "Removed block rule '$($rule.DisplayName)'"
    }
}

$existing = Get-NetFirewallRule -DisplayName $DisplayName -ErrorAction SilentlyContinue
if ($existing) {
    if ($PSCmdlet.ShouldProcess($DisplayName, 'Update remote addresses')) {
        $existing | Set-NetFirewallRule -RemoteAddress $RemoteAddress -LocalPort $Port -Protocol TCP -Profile Any -Action Allow
        Write-Host "Updated '$DisplayName': TCP $Port from $($RemoteAddress -join ', ')"
    }
} elseif ($PSCmdlet.ShouldProcess($DisplayName, 'Create inbound allow rule')) {
    New-NetFirewallRule -DisplayName $DisplayName -Direction Inbound -Action Allow -Protocol TCP -LocalPort $Port `
        -RemoteAddress $RemoteAddress -Profile Any -Description 'CLIProxyAPI hub API, management and insights for LAN clients.' | Out-Null
    Write-Host "Created '$DisplayName': TCP $Port from $($RemoteAddress -join ', ')"
}
