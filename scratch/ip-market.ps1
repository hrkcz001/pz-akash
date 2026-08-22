# scratch/ip-market.ps1 — the real market for a dedicated IP, per provider.
#
# Why this exists: internal/akash/select.go filters on Console's *aggregate* stats,
# and both halves of that can be wrong in a way that costs us a 20-minute deploy:
#
#   * featEndpointIp is a self-reported flag. proxyua reports true to Console and its
#     own /status says leased_ip.capacity 0 — it cannot serve what it advertises.
#   * Aggregate free capacity is summed across nodes, but Akash must fit a service
#     onto ONE node. Digital Frontier shows ~100Gi free RAM and 0.245 free cores on
#     node1 and 16 free cores with 3.1Gi on node6, so it fits nothing our size while
#     looking enormous.
#
# So ask each provider directly. /status is public, needs no auth, and is served with
# a certificate that does not match its hostname (hence -SkipCertificateCheck, the
# same trade internal/akash/wait.go makes).
param(
    # The reservation we actually need to fit, from config.yaml server.resources.
    [int]$CPUMilli = 8000,
    [long]$MemoryBytes = 17179869184,
    [long]$StorageBytes = 32212254720
)

$ErrorActionPreference = "Stop"
$api = "https://console-api.akash.network"

$secrets = @{}
foreach ($line in [IO.File]::ReadAllLines("C:\Users\hrkcz001\.pz-akash\secrets.env")) {
    if ($line -match '^\s*(PZ_[A-Z0-9_]+)=(.*)$') { $secrets[$Matches[1]] = $Matches[2].Trim().Trim('"') }
}
$hdr = @{ "x-api-key" = $secrets["PZ_AKASH_API_KEY"] }

# Invoke-WebRequest, not Invoke-RestMethod: the providers array deserializes into
# objects with empty property names through the latter.
$r = Invoke-WebRequest -Uri "$api/v1/providers" -Headers $hdr -TimeoutSec 180
$body = $r.Content
if ($body -isnot [string]) { $body = [Text.Encoding]::UTF8.GetString([byte[]]$body) }
$all = $body | ConvertFrom-Json

$cands = @($all | Where-Object { $_.isOnline -and $_.featEndpointIp })
"online providers advertising featEndpointIp: $($cands.Count) of $($all.Count)"
""

$rows = foreach ($p in $cands) {
    $country = $p.ipCountryCode
    if (-not $country) { $country = $p.ipRegion }
    $row = [ordered]@{
        owner = $p.owner; country = $country; name = $p.name
        ipFree = "?"; ipCap = "?"; fitNodes = "?"; note = ""
    }
    $uri = $p.hostUri
    if (-not $uri) { $row.note = "publishes no hostUri"; [pscustomobject]$row; continue }
    if ($uri -notmatch "://") { $uri = "https://$uri" }
    try {
        $s = Invoke-WebRequest -Uri "$($uri.TrimEnd('/'))/status" -SkipCertificateCheck -TimeoutSec 25
        $st = $s.Content; if ($st -isnot [string]) { $st = [Text.Encoding]::UTF8.GetString([byte[]]$st) }
        $j = $st | ConvertFrom-Json
    } catch {
        $row.note = "/status unreachable: $($_.Exception.GetBaseException().Message)"
        [pscustomobject]$row; continue
    }

    # leased_ip lives at cluster.inventory.status.leased_ip on some builds and
    # elsewhere on others; find it by shape rather than by path.
    $flat = $j | ConvertTo-Json -Depth 12 -Compress
    if ($flat -match '"leased_ip":\{"allocatable":"?(\d+)"?,"allocated":"?(\d+)"?,"capacity":"?(\d+)"?\}') {
        $row.ipCap = [int]$Matches[3]
        $row.ipFree = [int]$Matches[3] - [int]$Matches[2]
    } else {
        $row.note = "reports no leased_ip block"
    }

    $nodes = @($j.cluster.inventory.available.nodes)
    if ($nodes.Count -eq 0) {
        $row.fitNodes = 0; $row.note += " no node inventory"
    } else {
        $row.fitNodes = @($nodes | Where-Object {
            $_.available.cpu -ge $CPUMilli -and
            $_.available.memory -ge $MemoryBytes -and
            $_.available.storage_ephemeral -ge $StorageBytes
        }).Count
    }
    [pscustomobject]$row
}

$rows | Format-Table -AutoSize owner, country, ipFree, ipCap, fitNodes, name, note
""
"--- can actually serve us right now (free IP AND a node that fits) ---"
$ok = @($rows | Where-Object { $_.ipFree -is [int] -and $_.ipFree -gt 0 -and $_.fitNodes -is [int] -and $_.fitNodes -gt 0 })
if ($ok.Count -eq 0) { "none" } else { $ok | Format-Table -AutoSize owner, country, ipFree, fitNodes, name }
