# scratch/v1-verify-closed.ps1 — assert the wallet has nothing open left.
#
# Not a formality. The close is what unblocks the repository renames, and the reason
# the renames have to wait is that a live v1 controller pushes to pz-saves: GitHub
# redirects pushes for a renamed repository, so a controller that survived this would
# push v1's bus files into whatever repository next occupies the old name — which is
# the clean v2 one. An empty list here is the precondition for the next step.
$ErrorActionPreference = "Stop"

foreach ($line in [IO.File]::ReadAllLines("C:\Users\hrkcz001\.pz-akash\secrets.env")) {
    if ($line -match '^(PZ_AKASH_API_KEY)=(.+)$') { Set-Item "env:$($Matches[1])" $Matches[2] }
}
$headers = @{ "x-api-key" = $env:PZ_AKASH_API_KEY }
$list = Invoke-RestMethod -Uri "https://console-api.akash.network/v1/deployments?limit=1000" -Headers $headers

$open = @($list.data.deployments | Where-Object { $_.deployment.state -in "active", "open" })
Write-Output ("deployments: {0} total, {1} still open" -f @($list.data.deployments).Count, $open.Count)
foreach ($d in $open) {
    Write-Output ("  STILL OPEN dseq {0}  state={1}" -f $d.deployment.id.dseq, $d.deployment.state)
}
if ($open.Count -eq 0) { Write-Output "ok: nothing is billing; the renames are unblocked" }
