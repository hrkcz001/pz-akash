# scratch/lease-logs.ps1 — read a live lease's container log off the provider.
#
# Why this exists: a failed deploy's reason does not survive in the state branch.
# onDeployResult writes it to doc.LastError and the close that follows overwrites it
# with "closed dseq <n>"; the branch is a force-pushed single orphan commit, so there
# is no previous version. The controller printed the reason to stdout, and the
# provider still holds that stdout for as long as the lease is open. So: mint a JWT
# scoped to `logs`, ask the provider, and read what the controller actually said.
#
# Two safety rules, both from the same past mistake:
#   * No secret is ever an argument. The API key and the redaction table are read out
#     of ~/.pz-akash/secrets.env by this script itself.
#   * Container stdout is not assumed safe to print. Every known secret value is
#     replaced with <redacted:NAME> before anything reaches the terminal, and the
#     unredacted copy is written to TEMP rather than into the repo.
param(
    [Parameter(Mandatory = $true)][string]$DSeq,
    [string]$Provider = "",
    # The provider serves logs per service, not per lease: /lease/../service/<name>/logs.
    # Our SDL names them "controller" and "pz-server".
    [string]$Service = "controller",
    # logs = container stdout; kubeevents = the scheduler's own account of why a pod is
    # not running (image pull, eviction, OOM).
    [ValidateSet("logs", "kubeevents")][string]$Endpoint = "logs",
    [int]$Tail = 3000,
    # Stream new output as it arrives instead of taking a snapshot and exiting.
    #
    # This is not a convenience for `logs`, it is the only way to read
    # `kubeevents` at all: kubeevents is a live Kubernetes watch, so with
    # follow=false the provider replays nothing and answers with an empty frame —
    # which reads exactly like "the pod is fine, no events", the opposite of what
    # it means. `logs` does honour tail, so a snapshot there is real.
    [switch]$Follow,
    # How long to keep streaming under -Follow before giving up and printing what
    # arrived. An image pull is the thing worth watching and it takes minutes.
    [int]$FollowSeconds = 180,
    # Regex; only matching lines are printed. Default keeps the controller's own
    # narration and drops nothing else of interest.
    [string]$Match = ""
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

# Redact longest-first, so a secret that contains another is not half-replaced.
function Redact([string]$text) {
    foreach ($n in ($secrets.Keys | Sort-Object { -$secrets[$_].Length })) {
        $v = $secrets[$n]
        if ($v.Length -ge 6) { $text = $text.Replace($v, "<redacted:$n>") }
    }
    return $text
}

if (-not $Provider) {
    $d = Invoke-RestMethod -Uri "$api/v1/deployments/$DSeq" -Headers $hdr -TimeoutSec 60
    $lease = @($d.data.leases)[0]
    if (-not $lease) { throw "dseq $DSeq has no lease" }
    if ($lease.state -ne "active") { "note: lease state is '$($lease.state)' — a closed lease keeps no logs" }
    $Provider = $lease.id.provider
    $gseq = $lease.id.gseq; $oseq = $lease.id.oseq
} else { $gseq = 1; $oseq = 1 }
"provider $Provider  gseq=$gseq oseq=$oseq"

$p = Invoke-RestMethod -Uri "$api/v1/providers/$Provider" -Headers $hdr -TimeoutSec 60
# @() around the pipeline, not just parentheses: when exactly one candidate survives
# Where-Object the result is a bare string, and [0] on a string is its first character.
$hostUri = @($p.data.hostUri, $p.data.host_uri, $p.hostUri | Where-Object { $_ })[0]
if (-not $hostUri) { throw "provider $Provider publishes no hostUri" }
$hostUri = $hostUri.TrimEnd("/")
if ($hostUri -notmatch "://") { $hostUri = "https://$hostUri" }
"hostUri  $hostUri"

# A token scoped to what this run actually reads and nothing else, minutes-lived:
# what an interceptor on an unverified connection could get is the right to read this
# container's stdout.
#
# The scope has to name the endpoint, and the scope vocabulary is not the URL
# vocabulary: /kubeevents is served under the scope `events`. Get it wrong in either
# direction and you get an error that points somewhere else — a scope of `logs` against
# /kubeevents fails at the WebSocket handshake as "status code '401' when '101' was
# expected" (reads as auth, is actually scope), and a scope of `kubeevents` is rejected
# by the token endpoint itself. The full vocabulary it accepts: send-manifest,
# get-manifest, logs, shell, events, status, restart, hostname-migrate, ip-migrate,
# attestation.
$scope = if ($Endpoint -eq "kubeevents") { "events" } else { $Endpoint }
$jwt = Invoke-RestMethod -Uri "$api/v1/create-jwt-token" -Headers $hdr -Method POST `
    -ContentType "application/json" -TimeoutSec 60 `
    -Body (@{ data = @{ ttl = 600; leases = @{ access = "scoped"; scope = @($scope) } } } | ConvertTo-Json -Depth 6)
$token = @($jwt.data.token, $jwt.token | Where-Object { $_ })[0]
if (-not $token) { throw "the API returned no logs token" }
"token minted: $($token.Length) chars"

# The provider gateway serves logs and kubeevents over a WebSocket, not as REST —
# /status is the only plain GET. A plain GET here answers 400 (a failed upgrade), which
# is what the first attempt at this script got. Query params: service, follow, tail.
#
# kubeevents is the interesting sibling: it is where "Failed to pull image" and
# "OOMKilled" appear, i.e. the reason a replica never becomes ready — which the state
# branch never records and the closed-lease API no longer remembers.
$scheme = $hostUri -replace '^https://', 'wss://' -replace '^http://', 'ws://'
$followFlag = if ($Follow) { "true" } else { "false" }
$url = "$scheme/lease/$DSeq/$gseq/$oseq/$Endpoint" +
       "?follow=$followFlag&tail=$Tail" + $(if ($Service) { "&service=$Service" } else { "" })
"connecting $url"

$ws = [System.Net.WebSockets.ClientWebSocket]::new()
$ws.Options.SetRequestHeader("Authorization", "Bearer $token")
# Provider lease certificates routinely do not match hostUri; internal/akash/wait.go
# retries insecurely for the same reason. The token is scoped to reading this one
# container's output and lives for minutes.
#
# A compiled delegate rather than a `{ $true }` scriptblock: the handshake runs the
# callback on a thread pool thread, where a scriptblock has no runspace and throws
# "There is no Runspace available to run scripts in this thread".
if (-not ("PzCertAny" -as [type])) {
    Add-Type -TypeDefinition @"
using System.Net.Security;
using System.Security.Cryptography.X509Certificates;
public static class PzCertAny {
    static bool Ok(object s, X509Certificate c, X509Chain ch, SslPolicyErrors e) { return true; }
    public static RemoteCertificateValidationCallback Cb = new RemoteCertificateValidationCallback(Ok);
}
"@
}
$ws.Options.RemoteCertificateValidationCallback = [PzCertAny]::Cb
$cts = [Threading.CancellationTokenSource]::new(
    [TimeSpan]::FromSeconds($(if ($Follow) { $FollowSeconds } else { 120 })))
try {
    $ws.ConnectAsync([Uri]$url, $cts.Token).GetAwaiter().GetResult()
} catch {
    "connect failed: $($_.Exception.GetBaseException().Message)"
    exit 1
}
"connected: $($ws.State)"

$sb = [Text.StringBuilder]::new()
$buf = [byte[]]::new(64KB)
while ($ws.State -eq 'Open') {
    try {
        $seg = [ArraySegment[byte]]::new($buf)
        $res = $ws.ReceiveAsync($seg, $cts.Token).GetAwaiter().GetResult()
    } catch { break }
    if ($res.MessageType -eq 'Close') { break }
    [void]$sb.Append([Text.Encoding]::UTF8.GetString($buf, 0, $res.Count))
}
$raw = $sb.ToString()
$dump = Join-Path ([IO.Path]::GetTempPath()) "lease-$DSeq.log"
[IO.File]::WriteAllText($dump, $raw)
"raw: $dump ($($raw.Length) bytes) — unredacted, outside the repo"

$lines = $raw -split "`r?`n"
if ($Match) { $lines = $lines | Where-Object { $_ -match $Match } }
"--- $($lines.Count) line(s) ---"
foreach ($l in $lines) { Redact $l }
