# scratch/kill-v1.ps1 — close both v1 deployments.
#
# The user's cutover decision: "just kill old deployments and deploy new", and, when
# asked directly about the live vsrania world, "Kill it, start a fresh world". So this
# discards a running world with players' progress in it, deliberately, and the escrow
# each deployment still holds settles back to the wallet — which is where the budget
# for v2's first deployment comes from.
#
# The controller is closed FIRST. v1's controller reconciles desired_state=running by
# creating a server deployment, so closing the server while the controller is alive
# would spend the refund on a replacement server. Identified by service name in
# scratch/v1-identify.ps1, not by creation order.
#
# The escrow read is before the close on purpose: after the close there is nothing to
# read, and the numbers are the only record of what the cutover cost or returned.
$ErrorActionPreference = "Stop"

foreach ($line in [IO.File]::ReadAllLines("C:\Users\hrkcz001\.pz-akash\secrets.env")) {
    if ($line -match '^(PZ_AKASH_API_KEY)=(.+)$') { Set-Item "env:$($Matches[1])" $Matches[2] }
}
if (-not $env:PZ_AKASH_API_KEY) { throw "PZ_AKASH_API_KEY is not in secrets.env" }
Write-Output ("PZ_AKASH_API_KEY loaded: {0} chars" -f $env:PZ_AKASH_API_KEY.Length)

$cfg = "C:\Users\hrkcz001\pz-saves\config.yaml"
Set-Location "C:\Users\hrkcz001\zomboid-akash\pzctl"

# controller then server. Not a list to iterate: the order is the whole point, and a
# loop over an array invites someone to sort it.
foreach ($step in @(
        @{ dseq = "1787078661931"; what = "the v1 controller" },
        @{ dseq = "1787103872228"; what = "the v1 game server (the live world)" })) {
    Write-Output ""
    Write-Output ("=== {0} — dseq {1}" -f $step.what, $step.dseq)
    go run ./cmd/pzctl akash escrow -c $cfg -dseq $step.dseq
    go run ./cmd/pzctl akash close -c $cfg -dseq $step.dseq
    if ($LASTEXITCODE -ne 0) { throw "close failed for dseq $($step.dseq)" }
}
