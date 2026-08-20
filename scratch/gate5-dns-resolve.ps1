# scratch/gate5-dns-resolve.ps1 — the part of the DNS gate the first run could not
# answer: does the record actually resolve for a player, not merely exist in
# Cloudflare's API.
#
# The first attempt asked 8.8.8.8 about two seconds after the create and got
# NXDOMAIN, which proves nothing either way — so this asks the zone's own
# authoritative nameservers (ground truth, no caching in the way) as well as two
# public resolvers, and retries for up to two minutes.
#
# Same safety as gate5-dns.ps1: only --game, so the apex and www CNAMEs pointing at
# the live v1 controller are never touched. The record is removed at the end.
$ErrorActionPreference = "Stop"
$root = "C:\Users\hrkcz001\zomboid-akash"
$cfg = "$root\pzctl\config.yaml"
$pzctl = "$root\scratch\pzctl.exe"
$ip = "213.58.173.240"
$name = "pz.vsrania.online"

$yaml = Get-Content "$root\pz-controller\deployment.yaml" -Raw
if ($yaml -notmatch "(?m)^\s*-\s*CLOUDFLARE_API_TOKEN=(.+)$") {
    throw "CLOUDFLARE_API_TOKEN not found in pz-controller/deployment.yaml"
}
$env:PZ_CLOUDFLARE_API_TOKEN = $Matches[1].Trim()
Write-Output ("PZ_CLOUDFLARE_API_TOKEN loaded: {0} chars" -f $env:PZ_CLOUDFLARE_API_TOKEN.Length)

# The authoritative servers for the zone. Whatever these say is the truth; a public
# resolver disagreeing with them is a caching question, not a correctness one.
$ns = (Resolve-DnsName -Name "vsrania.online" -Type NS -Server 8.8.8.8).NameHost | Sort-Object -Unique
Write-Output ("authoritative: {0}" -f ($ns -join ", "))

function Ask($server) {
    try {
        $a = Resolve-DnsName -Name $name -Type A -Server $server -DnsOnly -ErrorAction Stop |
             Where-Object { $_.Type -eq "A" }
        if ($a) { return ($a.IPAddress -join ",") }
        return "no-A"
    } catch { return "NXDOMAIN" }
}

Write-Output "`n=== before: nothing should be there ==="
foreach ($s in @($ns[0], "8.8.8.8", "1.1.1.1")) { Write-Output ("  {0,-24} {1}" -f $s, (Ask $s)) }

Write-Output "`n=== create ==="
& $pzctl dns sync -c $cfg --game $ip
if ($LASTEXITCODE -ne 0) { throw "sync failed" }

try {
    Write-Output "`n=== poll until the authoritative servers answer (up to 120s) ==="
    $sw = [Diagnostics.Stopwatch]::StartNew()
    $ok = $false
    while ($sw.Elapsed.TotalSeconds -lt 120) {
        $auth = Ask $ns[0]
        Write-Output ("  {0,5:n1}s  {1,-24} {2}" -f $sw.Elapsed.TotalSeconds, $ns[0], $auth)
        if ($auth -eq $ip) { $ok = $true; break }
        Start-Sleep -Seconds 5
    }
    if (-not $ok) { Write-Output "  NOT RESOLVED by the authoritative server within 120s" }

    Write-Output "`n=== and from public resolvers ==="
    foreach ($s in @($ns[0], $ns[1], "8.8.8.8", "1.1.1.1", "9.9.9.9")) {
        Write-Output ("  {0,-24} {1}" -f $s, (Ask $s))
    }

    # What a player's client is actually given. The A record carries no port, so this
    # is the whole of what DNS contributes to "pz.vsrania.online:16261".
    Write-Output "`n=== the address a player would use ==="
    Write-Output ("  {0}:{1}" -f $name, 16261)
}
finally {
    Write-Output "`n=== remove ==="
    & $pzctl dns clear-game -c $cfg
    Write-Output "  exit $LASTEXITCODE"
    Write-Output ("  authoritative now: {0}" -f (Ask $ns[0]))
}
