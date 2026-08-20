# scratch/gate5-dns.ps1 — the step-5 live gate, part 2: write the game record into
# the real Cloudflare zone, read it back, and remove it again.
#
# Two things make this safe to run against the live zone. `dns sync` is given only
# --game, so it touches dns.game_record and nothing else — the apex and www CNAMEs
# that point at the live v1 controller are never read, let alone written. And
# pz.vsrania.online is a name v2 invents: v1 published the game address to the
# dashboard for players to read, so nothing resolves this name today.
#
# The address is the one gate run 3 was actually assigned, on a lease that has since
# been closed. It is written for about a minute and then removed.
#
# CLOUDFLARE_API_TOKEN is the real credential — a record cannot be written with a
# throwaway. Every other PZ_* is a throwaway: nothing here renders an SDL.
$ErrorActionPreference = "Stop"
$root = "C:\Users\hrkcz001\zomboid-akash"
$cfg = "$root\pzctl\config.yaml"
$ip = "213.58.173.240"

$yaml = Get-Content "$root\pz-controller\deployment.yaml" -Raw
if ($yaml -match "(?m)^\s*-\s*CLOUDFLARE_API_TOKEN=(.+)$") {
    $env:PZ_CLOUDFLARE_API_TOKEN = $Matches[1].Trim()
} else {
    throw "CLOUDFLARE_API_TOKEN not found in pz-controller/deployment.yaml"
}
Write-Output ("PZ_CLOUDFLARE_API_TOKEN loaded: {0} chars" -f $env:PZ_CLOUDFLARE_API_TOKEN.Length)

$env:PZ_AKASH_API_KEY = "gate-throwaway"
$env:PZ_DEPLOY_KEY_B64 = [Convert]::ToBase64String([Text.Encoding]::ASCII.GetBytes("gate-throwaway-not-a-key"))
$env:PZ_WEBHOOK_SECRET = "gate-throwaway"
$env:PZ_STORAGE_PASSWORD = "gate-throwaway"
$env:PZ_SERVER_FILES_PASSWORD = "gate-throwaway"
$env:PZ_BACKUPS_PASSWORD = "gate-throwaway"
$env:PZ_RCON_PASSWORD = "gate-throwaway"
$env:PZ_ADMIN_PASSWORD = "gate-throwaway"
$env:PZ_JOIN_PASSWORD = "gate-throwaway"

$pzctl = "$root\scratch\pzctl.exe"

Write-Output "`n=== 1. the zone as it stands ==="
& $pzctl dns zones -c $cfg

Write-Output "`n=== 2. dry run: what --game $ip would change ==="
& $pzctl dns sync -c $cfg --game $ip --dry-run

Write-Output "`n=== 3. for real ==="
& $pzctl dns sync -c $cfg --game $ip
Write-Output "EXIT $LASTEXITCODE"

Write-Output "`n=== 4. read back ==="
& $pzctl dns zones -c $cfg

Write-Output "`n=== 5. idempotence: the same sync again should change nothing ==="
& $pzctl dns sync -c $cfg --game $ip

Write-Output "`n=== 6. resolve it, from a resolver that is not Cloudflare's API ==="
try { Resolve-DnsName -Name "pz.vsrania.online" -Type A -Server 8.8.8.8 -ErrorAction Stop | Format-Table -AutoSize | Out-String }
catch { Write-Output "resolve failed: $_" }

Write-Output "`n=== 7. remove it ==="
& $pzctl dns clear-game -c $cfg
Write-Output "EXIT $LASTEXITCODE"

Write-Output "`n=== 8. gone? ==="
& $pzctl dns zones -c $cfg
