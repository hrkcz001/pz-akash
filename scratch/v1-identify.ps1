# scratch/v1-identify.ps1 — which live dseq is the controller and which is the server.
#
# Needed because `pzctl akash leases` only ever reports the server: Adopt identifies a
# deployment by service name and deliberately never claims the controller's own, so a
# cutover that has to close both has to find the other one here.
#
# Order matters at close time. v1's controller reconciles desired_state=running by
# deploying a server, so closing the server first would make it fund a replacement.
# The controller goes first, which is why this script exists at all rather than
# closing both in whatever order the list came back in.
#
# Prints deployment ids, states and service names. Not the manifest: a v1 service's
# environment is where v1 kept its passwords, and this output goes to a terminal.
$ErrorActionPreference = "Stop"

foreach ($line in [IO.File]::ReadAllLines("C:\Users\hrkcz001\.pz-akash\secrets.env")) {
    if ($line -match '^(PZ_AKASH_API_KEY)=(.+)$') { Set-Item "env:$($Matches[1])" $Matches[2] }
}
if (-not $env:PZ_AKASH_API_KEY) { throw "PZ_AKASH_API_KEY is not in secrets.env" }
Write-Output ("PZ_AKASH_API_KEY loaded: {0} chars" -f $env:PZ_AKASH_API_KEY.Length)

# x-api-key, not Authorization: Bearer. Bearer is the provider header; the console
# rejects it with UnauthorizedError.
$headers = @{ "x-api-key" = $env:PZ_AKASH_API_KEY }
$base = "https://console-api.akash.network"

foreach ($dseq in 1787078661931, 1787103872228) {
    $d = Invoke-RestMethod -Uri "$base/v1/deployments/$dseq" -Headers $headers
    $svcs = @()
    foreach ($lease in $d.data.leases) {
        if ($lease.status -and $lease.status.services) {
            $svcs += $lease.status.services.PSObject.Properties.Name
        }
    }
    $names = ($svcs | Sort-Object -Unique) -join ", "
    if (-not $names) { $names = "(no service names in the lease status)" }
    Write-Output ("  dseq {0}  state={1}  services: {2}" -f $dseq, $d.data.deployment.state, $names)
}
