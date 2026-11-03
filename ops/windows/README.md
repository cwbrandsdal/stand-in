# Windows hub operations

Scripts that run this fork as the always-on CLIProxyAPI hub for Claude Code, Codex and T3 Code
on a Windows machine, with every other machine on the LAN pointing at it.

| Script | Purpose | Elevation |
| --- | --- | --- |
| `Install-Hub.ps1` | Build, create missing keys in SecretStore, write `config.yaml`/`.env` once, register and start the logon task. Rerun after code changes to update. | No |
| `Start-Hub.ps1` | Supervisor run by the `CLIProxyAPI Hub` task; restarts the server when it exits. | No |
| `Add-HubFirewallRule.ps1` | Allow the listed LAN hosts on TCP 8317 and remove Windows' auto-created block rules for the binary. | Yes |
| `Add-HubClient.ps1` | Add a client machine: create its key in SecretStore, add it to `config.yaml` (hot-reloaded), name its IP on the dashboard and add the IP to the firewall rule. | Asks for the firewall step |
| `Connect-HubToLuna.ps1` | Create the hub's alert source in Luna, store its token in SecretStore, point the hub at it and restart the hub. | No |

## Layout

Runtime state lives outside the repository, in `D:\Knowledge\00-Control\services\cliproxyapi`:

| Path | Contents |
| --- | --- |
| `bin\cli-proxy-api.exe` | Installed build (the repository build output is never run directly). |
| `config.yaml` | Hub configuration. Created once; edit in place. The management key is stored as a bcrypt hash. |
| `.env` | Non-secret settings, such as `INSIGHTS_HOST_LABELS`, `INSIGHTS_RETENTION_DAYS` and `INSIGHTS_BACKUP_DIR`, plus `INSIGHTS_VIEWER_KEY_SHA256` (the hash of the viewer key, kept in sync by `Install-Hub.ps1`). |
| `auths\` | OAuth credential files, one per signed-in account. Never copy or commit these. |
| `data\insights.db` | SQLite usage and quota history (see below). |
| `logs\` | Server logs plus `supervisor.log`. |
| `static\management.html` | Upstream management panel, downloaded on first use. |

Secrets live in Windows SecretStore (vault `LocalSecrets`), never in the repository:

| Secret | Used by |
| --- | --- |
| `CLIProxyAPI.Hub.ManagementKey` | Management panel, `/insights`, T3 Code's hub connection. Full control, including downloading OAuth credentials. |
| `CLIProxyAPI.Hub.InsightsViewerKey` | `/insights` only: the read-only key for phones and remote access, and for Luna Core, which reads `/insights/overview` and `/insights/waste` with it. It cannot open any other management route. |
| `CLIProxyAPI.Hub.ClientKey.<machine>` | One client key per machine (`cpa-<machine>-<random>`); the dashboard attributes usage by `<machine>`. |
| `Luna.Alerts.Hub.Token` | Optional. The hub's alert source token in Luna (`Connect-HubToLuna.ps1`); `Start-Hub.ps1` passes it to the hub process as `INSIGHTS_LUNA_TOKEN`. Luna keeps only its hash. |

Read one when configuring a client: `Get-Secret -Name CLIProxyAPI.Hub.ClientKey.proart -Vault LocalSecrets -AsPlainText`.
Copy them into the password manager as the authoritative copy.

## First-time setup

1. Install and start: `pwsh -File ops\windows\Install-Hub.ps1 -HostLabels @{ '192.168.1.170' = 'ProArt'; '192.168.1.171' = 'Deathstar' }`
2. From an elevated PowerShell: `pwsh -File ops\windows\Add-HubFirewallRule.ps1 -RemoteAddress 192.168.1.170,192.168.1.171`
   If Windows shows an "allow access" prompt for `cli-proxy-api.exe`, cancel it; the script handles access.
3. Sign accounts in on the hub. The browser flow completes on this machine:

   ```powershell
   cd D:\Knowledge\00-Control\services\cliproxyapi
   .\bin\cli-proxy-api.exe -config .\config.yaml -claude-login
   .\bin\cli-proxy-api.exe -config .\config.yaml -codex-login      # or -codex-device-login
   ```

   Repeat per account. The running hub picks up new credential files without a restart. You can also use
   **OAuth Login** in `http://127.0.0.1:8317/management.html`.
4. For each Codex credential, enable the upstream websocket transport (otherwise Codex's websocket requests fall
   back to HTTP). Either set `"websockets": true` in its JSON file under `auths\`, or call
   `PATCH /v8/management/credentials/fields` with `{"name": "<file name>", "websockets": true}`. To bench an
   account, set `"disabled": true` the same way.

Check from another machine: `curl.exe -s http://192.168.1.134:8317/v1/models -H "Authorization: Bearer <client key>"`
returns a JSON model list once accounts are signed in.

## Pointing clients at the hub (manual; review first)

Use `http://127.0.0.1:8317` on the hub itself and `http://192.168.1.134:8317` elsewhere. Back up each file before editing.
Replace `<machine>` with `r2-d2`, `proart` or `deathstar`.

To add a machine later, run `Add-HubClient.ps1 -Name <machine> -Label '<Name>' -Address <ip> -CopyKey`.
The Mac mini (`mac-mini`) keeps its key in the macOS login Keychain instead of SecretStore and reads it with
`/usr/bin/security find-generic-password -s CLIProxyAPI.Hub.ClientKey.mac-mini -w`; see
`X:\10-Operations\runbooks\cliproxyapi-client-mac-mini.md`.

Both clients read the machine's client key from SecretStore with a command, so the key never sits in a config
file. On ProArt and Deathstar, store that machine's key locally first (copy it from the password manager when
prompted): `X:\60-Secrets\scripts\Set-RepoSecret.ps1 CLIProxyAPI.Hub.ClientKey.<machine> -SkipAzureKeyVault`.

Claude Code: `settings.json` in the Claude config directory (`CLAUDE_CONFIG_DIR`, or `~/.claude`). The helper
command has no inner quotes on purpose, so it works whether Claude Code runs it through cmd.exe or Git Bash:

```json
{
  "apiKeyHelper": "pwsh -NoProfile -NonInteractive -Command Get-Secret -Name CLIProxyAPI.Hub.ClientKey.<machine> -AsPlainText",
  "env": {
    "ANTHROPIC_BASE_URL": "http://192.168.1.134:8317"
  }
}
```

If that config directory holds a claude.ai login, run `/logout` there first, because the login conflicts with
the helper. To undo, remove both keys and run `/login`. To try the hub without touching the normal setup, use a
second directory, such as `$env:CLAUDE_CONFIG_DIR = "$HOME\.claude-hub"; claude`.

Codex (0.165 or later): `config.toml` in `CODEX_HOME` (or `~/.codex`). The `auth` table runs the command and
sends its output as the bearer token. Put the `model_provider` line among the top-level settings before the
first `[table]`, and the two tables at the end of the file. In TOML, every line after a table header belongs to
that table, so a top-level setting such as `notify` placed below `[model_providers.cliproxy.auth]` is silently
moved into it (Codex warns `model_providers.cliproxy.auth.notify is ignored`).

```toml
model_provider = "cliproxy"

[model_providers.cliproxy]
name = "CLIProxyAPI hub"
base_url = "http://192.168.1.134:8317/v1"
wire_api = "responses"
requires_openai_auth = false
supports_websockets = true

[model_providers.cliproxy.auth]
command = "pwsh"
args = ["-NoProfile", "-NonInteractive", "-Command", "Get-Secret -Name CLIProxyAPI.Hub.ClientKey.<machine> -AsPlainText"]
```

To undo, remove the `model_provider` line. The ChatGPT login in `auth.json` is still there.

Both snippets were tested against the hub on 2026-10-29 (Claude Code through `apiKeyHelper`; Codex through the
command-based auth over upstream websockets).

T3 Code runs providers with the environment machine's CLI configuration. Claude follows the Claude Code
settings above. For Codex, choose **Use existing CLI**; **Continue with ChatGPT** bypasses the hub. For pooled
limits, use **Settings → Providers → Usage providers → Add hub** with `http://192.168.1.134:8317` and the
management key.

House rules from the T3 Code guide: use the Claude subscriptions only through Claude Code, and never serve
other people's traffic through the hub.

## Insights dashboard and database

`http://<hub>:8317/insights` has these sections, top to bottom:

- **Alerts**: open alerts, most severe first (see [Alerts and Luna](#alerts-and-luna)).
- **Quota pools**: pooled quota per provider with recorded history, a projection at the current burn rate with
  each account's resets, drain rate, and every account's current windows (5-hour where the plan has one, weekly,
  model-specific). A window an account stopped reporting is dropped once its period has ended and the account's
  other windows have kept arriving for 6 hours; the account then shows "none reported since …". Anthropic removed
  the 5-hour window for verified Claude Ultra accounts from 30 October 2026 (Pro and Max keep it), so an Ultra
  account shows only weekly windows. The 5-hour option appears only while some account still reports one.
- **Usage**: tokens, requests, API value, cache hit rate and latency, by model, account, machine and tool, plus a
  weekday/hour heatmap.
- **Accounts**: each account's share of its provider's traffic, API value, monthly pace against the plan price,
  the window closest to its limit, a tokens-per-account chart per provider, and sessions that moved between
  accounts (failover after an error, or affinity expiry after an idle gap).
- **Waste**: threads whose first request wrote a very large prompt cache (Claude through the Codex harness),
  cache written again after an idle gap, fast-mode refusals per machine and tool, and account moves.
- **Threads**: agent threads (client sessions) by API value or recency, with harness, models and first cache
  write. Select one for its cumulative cost and every request.
- **Errors and latency**: failures by cause over time and by machine and tool, latency and time-to-first-token
  percentiles (p50/p90/p99) per model, and rejected client or management keys.
- **Session affinity** and **Recent requests**.
- **Data and settings**: alert settings, backups, price table, and the SQL console (management key only).

**API value** prices each request at the provider's public API list price (standard, priority/fast, ultrafast
and flex tiers, long-context rates, Anthropic's fast-mode multiplier). Subscriptions are not billed per token;
the figure shows what the same traffic would cost on the API, and the Accounts table compares each account's
monthly pace with its plan price. Codex plans are priced from the plan name (Plus $20, Pro $200, Pro Lite $100,
Pro Max $500); Claude credentials do not report a plan, so enter the price in the Plan price column (management
key). Prices start from a built-in table and refresh daily from LiteLLM's public price list
(`INSIGHTS_PRICES_URL`, or `off`); rows recorded before their model had a price are priced on the next refresh.

The dashboard asks for an access key and keeps it in the browser's local storage. Two keys work:

- The **viewer key** (`CLIProxyAPI.Hub.InsightsViewerKey`) opens only the insights endpoints
  (`/v8/management/insights/*`). Use it on phones and anywhere outside R2-D2; the page then shows
  "read-only key" and hides the Management link.
- The **management key** also works, but it controls the whole hub, so keep it to trusted computers. Only it
  can change alert settings and plan prices, run a backup, send a test alert to Luna or use the SQL console; the hub
  answers 403 to the viewer key for those.

To copy a key on R2-D2 without displaying it:
`Get-Secret -Name CLIProxyAPI.Hub.InsightsViewerKey -Vault LocalSecrets -AsPlainText | Set-Clipboard`.
To rotate the viewer key, remove that secret (`Remove-Secret -Name CLIProxyAPI.Hub.InsightsViewerKey -Vault LocalSecrets`)
and run `Install-Hub.ps1`, which creates a new one, updates the hash in `.env` and restarts the hub.

The page fits phone screens; tap a chart to see its values. It is dark by default; **Dark**, **Light** and
**Auto** (follow the device) at the top switch it, and the choice is kept in the browser.
After 5 wrong keys the hub blocks that IP address from all management endpoints for 30 minutes, even for the
right key, and the viewer key counts toward the same limit. The dashboard then shows "IP banned due to too many failed attempts". Wait it out, or restart the hub
to clear it, since the ban is kept in memory only.

Recording is passive. Request rows come from the proxy's usage pipeline. Quota points come from the rate-limit
signals already present on upstream responses, so the hub makes no extra calls to Anthropic or OpenAI.
Prompts and responses are never stored. Client keys are stored only as a 4-byte fingerprint and the
`<machine>` label.

`data\insights.db` is SQLite in WAL mode, safe to read while the hub runs. Timestamps are Unix milliseconds (UTC).
The hub upgrades older databases on start (`PRAGMA user_version` 1): it adds the new columns and classifies
earlier failures from their status code. A Claude 429 that the same session retried successfully within 15
seconds is taken to be a fast-mode refusal.

| Table | Rows |
| --- | --- |
| `requests` | One per upstream request: provider, model, account, machine IP, tool, client-key label, session, status, latency, TTFT, token buckets (uncached input, cache read, cache write, output, reasoning), `error_class` (why a failed request failed: `fast_mode`, `rate_limit`, `overloaded`, `upstream_auth`, `context_length`, `model_not_found`, `bad_request`, `client_cancelled`, `stream_truncated`, `upstream_error`, `other`) and `cost_usd` (API value; NULL for unknown models). |
| `quota_samples` | Quota window observations per account: `window_key` (`5h`, `7d`, `7d_oi` = Fable weekly, …), `used_pct`, `resets_at_ms`. |
| `accounts` | Latest label, email and plan per credential, and `monthly_usd` (the plan price set on the dashboard). |
| `alerts` | Raised alerts: kind, severity, title and text, Luna fingerprint, delivery to Luna (`push_status`), when it was marked seen, and when and how it resolved. |
| `access_failures` | Rejected client and management keys: kind, status, source address and route. The key itself is never stored. |
| `meta` | Alert settings and the last backup and alert check. |

```sql
-- Tokens and cache hit rate per day and account (local time)
SELECT date(ts_ms / 1000, 'unixepoch', 'localtime') AS day, account,
       SUM(total_tokens) AS tokens,
       ROUND(100.0 * SUM(cache_read_tokens) / NULLIF(SUM(input_tokens + cache_read_tokens + cache_write_tokens), 0), 1) AS cache_hit_pct
FROM requests GROUP BY day, account ORDER BY day DESC;
```

The **SQL console** on the dashboard (management key) runs one `SELECT`, `WITH` or `VALUES` statement at a
time on a separate read-only connection (`mode=ro`, `query_only`), returns at most 1,000 rows and stops after
20 seconds. Its Examples menu has queries for cost per machine, daily cost, the most expensive threads, failures,
cache hit rate, quota history and alerts.

### Alerts and Luna

Every minute the hub checks:

| Alert | Raised when | Severity | To Luna |
| --- | --- | --- | --- |
| Limit at N% | An account passes the threshold (default 80%) of a window; once per window period and threshold. | warning | Limits |
| Limit reached | A window reaches 100%. | critical | Limits |
| On pace to run out | A window that is at least 50% used would reach 100% before it resets, at the burn rate of the last hour (5-hour windows) or six hours (weekly windows). | warning | Limits |
| Sign-in failing | Requests to an account fail with an authentication error (repeats every 6 hours while it lasts). | critical | Limits |
| Fast mode refused | One machine and tool collect 5 fast-mode refusals in a clock hour. | warning | Waste |
| Large first cache write | A new thread's first request writes more than `INSIGHTS_WASTE_FIRST_WRITE` tokens (100K). | info or warning | no |
| Sessions moved between accounts | Sessions moved to another account of the same provider (one alert per provider and hour). | info | no |

Alerts always appear on the dashboard; **Seen** and **Mark all seen** close them. An alert also resolves by
itself: a limit alert when its window resets, a sign-in alert when the account serves a request again, and a
fast-mode alert after an hour without refusals.

[Luna](https://github.com/cwbrandsdal/luna) delivers them. The hub posts each alert to Luna's alert webhook as a
generic source, with one fingerprint per condition: on pace, 80% and 100% of one window period are one Luna alert
that escalates, and the hub sends "resolved" when the window resets. Luna speaks critical alerts at the next
pause in a call, or pushes them to the iPhone when no call is live; it batches warnings for five minutes and
keeps open ones in the drive brief. Quiet hours, mutes ("mute the hub for an hour") and Ack on Luna's Work screen
apply. Large first cache writes and account moves are not sent: Luna's brief reads them from the waste report.
If Luna is unreachable, the hub retries every minute for 24 hours. Which groups go to Luna (both by default) is
on the dashboard under **Data and settings**, with **Send test alert to Luna**.

Connect the hub to Luna once, on R2-D2:

```powershell
pwsh -File D:\Github\stand-in\ops\windows\Connect-HubToLuna.ps1
```

It runs Luna's `alert-source add 'CLIProxyAPI hub' --kind generic`, stores the token as `Luna.Alerts.Hub.Token` in
SecretStore without displaying it, writes `INSIGHTS_LUNA_ALERTS_URL=http://127.0.0.1:8787/v1/alerts/<sourceId>` to
`.env` and restarts the hub. `-Rotate` creates a new source and revokes the old one; `-Remove` revokes it and
disconnects. Set `INSIGHTS_DASHBOARD_URL` in `.env` to attach a dashboard link to each alert.

Luna reads the other direction too: its **AI subscriptions** screen and voice tool use
`GET /v8/management/insights/overview` with the viewer key. The overview lists each account's windows (used share,
reset and, when the recent burn rate gets there first, when it runs out), each provider's API value over 7 days
and monthly pace against its plan price, today's totals and three costliest threads, today's waste and the
number of open alerts.

### Backups

With `INSIGHTS_BACKUP_DIR` set (`Install-Hub.ps1` adds `D:\Knowledge\70-Backups\cliproxyapi`), the hub writes a
consistent copy of the database with `VACUUM INTO` once a day after 03:00 local time, as
`insights-YYYYMMDD.db`, and keeps the newest 14. The first copy is written a few minutes after the hub starts.
**Back up now** on the dashboard writes another copy. The copies are on the same disk as the hub, so they protect
against mistakes, not disk loss. To restore: stop the hub, replace `data\insights.db` with a copy (and delete
`insights.db-wal` and `insights.db-shm`), then start the hub.

### Environment settings

In `.env` in the runtime directory:

| Variable | Default | Meaning |
| --- | --- | --- |
| `INSIGHTS_HOST_LABELS` | | Machine names for client IPs: `ip=name,…`. |
| `INSIGHTS_RETENTION_DAYS` | keep everything | Delete rows (requests, quota samples, alerts, rejected keys) older than this. |
| `INSIGHTS_BACKUP_DIR` | off | Directory for nightly copies. |
| `INSIGHTS_BACKUP_HOUR`, `INSIGHTS_BACKUP_KEEP` | `3`, `14` | Local hour after which the daily copy runs; copies kept. |
| `INSIGHTS_PRICES_URL` | LiteLLM's price list | Price source refreshed daily (URL or file), or `off` for the built-in table. |
| `INSIGHTS_WASTE_FIRST_WRITE` | `100000` | First-request cache write that counts as large. |
| `INSIGHTS_WASTE_IDLE_MINUTES`, `INSIGHTS_WASTE_REWRITE_TOKENS` | `5`, `20000` | Idle gap and cache write that count as a cache rewrite. |
| `INSIGHTS_LUNA_ALERTS_URL` | | Luna's alert webhook for the hub's source (written by `Connect-HubToLuna.ps1`). |
| `INSIGHTS_DASHBOARD_URL` | | Dashboard link attached to alerts sent to Luna. |
| `INSIGHTS_DB_PATH` | `data\insights.db` | Database location. |
| `INSIGHTS_DISABLED` | | `1` turns recording off. |

`INSIGHTS_LUNA_TOKEN` comes from SecretStore through `Start-Hub.ps1`; do not put it in `.env`.

## Updating and troubleshooting

- Update after pulling or changing code: `pwsh -File ops\windows\Install-Hub.ps1`. It rebuilds, swaps the binary and restarts.
- Restart: `Stop-ScheduledTask 'CLIProxyAPI Hub'; Start-ScheduledTask 'CLIProxyAPI Hub'`. The supervisor stops any leftover server first.
- Stop completely: `Stop-ScheduledTask 'CLIProxyAPI Hub'; Get-Process cli-proxy-api | Stop-Process`. Stopping only the task leaves the server running.
- The task starts at logon, so the hub runs only while the user is signed in (the same model as Luna).
- Codex `token_invalidated` (401): sign that account in again with `-codex-login`.
- With a single account per provider, a limit hit has nowhere to fail over.
