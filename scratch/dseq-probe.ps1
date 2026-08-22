# scratch/dseq-probe.ps1 — what the Akash Console API still remembers about a dseq.
#
# Written because the controller's own record of a failed deploy does not survive:
# onDeployResult writes the real reason into doc.LastError, and the close that follows
# overwrites it with "closed dseq <n>" before the branch is published. The branch is a
# force-pushed single orphan commit, so there is no earlier version to read. The API is
# the only remaining witness.
#
# Prints named fields only. A deployment detail response carries the manifest, and the
# manifest carries every env value we hand the container — so nothing here dumps a
# whole object. The key is read from the secrets file at runtime, never passed in.
param([Parameter(Mandatory = $true)][string[]]$DSeq)

$ErrorActionPreference = "Stop"
$secretsEnv = "C:\Users\hrkcz001\.pz-akash\secrets.env"
foreach ($line in [IO.File]::ReadAllLines($secretsEnv)) {
    if ($line -match '^\s*PZ_AKASH_API_KEY=(.*)$') { $key = $Matches[1].Trim().Trim('"') }
}
if (-not $key) { throw "PZ_AKASH_API_KEY is not in secrets.env" }
"key loaded: $($key.Length) chars"

foreach ($d in $DSeq) {
    "=== dseq $d ==="
    try {
        $r = Invoke-RestMethod -Uri "https://console-api.akash.network/v1/deployments/$d" `
            -Headers @{ "x-api-key" = $key } -TimeoutSec 60
    } catch {
        "  request failed: $($_.Exception.Message)"
        continue
    }
    $dep = $r.data
    "  state        $($dep.deployment.state)"
    "  created      $($dep.deployment.createdHeight)"
    "  denom        $($dep.deployment.denom)"
    "  balance      $($dep.deployment.balance)"
    "  transferred  $($dep.deployment.transferred)"
    foreach ($l in @($dep.leases)) {
        "  lease        provider=$($l.provider) state=$($l.state) gseq=$($l.gseq) oseq=$($l.oseq)"
        "               price=$($l.price.amount) $($l.price.denom)  withdrawn=$($l.withdrawn)"
        "               createdHeight=$($l.createdHeight) closedHeight=$($l.closedHeight)"
        # The interesting part: per-service replica counts and whatever reason the
        # provider gave. available=0 with a non-empty status is an image pull or an OOM.
        foreach ($s in @($l.status.services.PSObject.Properties)) {
            $v = $s.Value
            "               service $($s.Name): ready=$($v.ready_replicas)/$($v.total) available=$($v.available) status=$($v.status)"
            foreach ($u in @($v.uris)) { "                 uri $u" }
        }
        if ($l.status.error) { "               status error: $($l.status.error)" }
    }
    "  events:"
    foreach ($e in @($dep.events)) { "    $($e.time) $($e.type) $($e.description)" }
}
