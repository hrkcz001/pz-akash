# scratch/lease-restart.ps1 — restart one service in a live lease, in place.
#
# Why this exists: config.yaml is read from git at boot, not per-resolve, so
# changing it is "a restart, not a re-render" (internal/bootstrap's package doc).
# The obvious way to restart is to redeploy, but that mints a new dseq and a new
# external port, which means re-syncing both CNAMEs and the Cloudflare origin
# rule — a lot of moving parts to reload a YAML file.
#
# A provider-side restart keeps the lease, the IP, the port and the persistent
# volume, and costs about a minute of downtime. It also has a useful side effect:
# the driver's provider skip-list lives in memory (internal/akash/deploy.go), so
# a restart forgets it and a provider that failed us an hour ago is a candidate
# again.
#
# Same two safety rules as lease-logs.ps1, from the same past mistake: no secret
# is ever an argument, and the API key is read out of ~/.pz-akash/secrets.env by
# this script itself.
param(
    [Parameter(Mandatory = $true)][string]$DSeq,
    [Parameter(Mandatory = $true)][string]$Provider,
    [string]$Service = "controller",
    [int]$Gseq = 1,
    [int]$Oseq = 1
)

$ErrorActionPreference = "Stop"
$api = "https://console-api.akash.network"

$secrets = @{}
foreach ($line in [IO.File]::ReadAllLines("C:\Users\hrkcz001\.pz-akash\secrets.env")) {
    if ($line -match '^\s*(PZ_[A-Z0-9_]+)=(.*)$') { $secrets[$Matches[1]] = $Matches[2].Trim().Trim('"') }
}
$key = $secrets["PZ_AKASH_API_KEY"]
if (-not $key) { throw "PZ_AKASH_API_KEY is not in secrets.env" }
$hdr = @{ "x-api-key" = $key }

$p = Invoke-RestMethod -Uri "$api/v1/providers/$Provider" -Headers $hdr -TimeoutSec 60
$hostUri = @($p.data.hostUri, $p.data.host_uri, $p.hostUri | Where-Object { $_ })[0]
if (-not $hostUri) { throw "provider $Provider publishes no hostUri" }
$hostUri = $hostUri.TrimEnd("/")
if ($hostUri -notmatch "://") { $hostUri = "https://$hostUri" }
"hostUri  $hostUri"

# `restart` is its own scope in the provider's vocabulary — see lease-logs.ps1 for
# the full list and for why a mismatched scope reads as an auth failure.
$jwt = Invoke-RestMethod -Uri "$api/v1/create-jwt-token" -Headers $hdr -Method POST `
    -ContentType "application/json" -TimeoutSec 60 `
    -Body (@{ data = @{ ttl = 300; leases = @{ access = "scoped"; scope = @("restart") } } } | ConvertTo-Json -Depth 6)
$token = @($jwt.data.token, $jwt.token | Where-Object { $_ })[0]
if (-not $token) { throw "the API returned no restart token" }
"token minted: $($token.Length) chars"

# Provider lease certificates routinely do not match hostUri; internal/akash/wait.go
# retries insecurely for the same reason, and the token is scoped to restarting this
# one service and lives for minutes.
$url = "$hostUri/lease/$DSeq/$Gseq/$Oseq/service/$Service/restart"
"POST $url"
try {
    $r = Invoke-WebRequest -Uri $url -Method POST -Headers @{ Authorization = "Bearer $token" } `
        -SkipCertificateCheck -TimeoutSec 120
    "HTTP $($r.StatusCode) $($r.StatusDescription)"
    if ($r.Content) { [Text.Encoding]::UTF8.GetString($r.Content) }
} catch {
    $resp = $_.Exception.Response
    if ($resp) { "HTTP $([int]$resp.StatusCode) — $($_.Exception.Message)" }
    else { "failed: $($_.Exception.GetBaseException().Message)" }
    exit 1
}
