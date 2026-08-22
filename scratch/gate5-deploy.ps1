# scratch/gate5-deploy.ps1 — the step-5 live gate, part 1: create one throwaway
# deployment and print what the network gave back.
#
# Every secret baked into the manifest is a throwaway. The manifest travels to a
# provider that will host nginx for a few minutes and then be closed, and the real
# deploy key has no business on it; the SDL's shape is what is under test, not the
# opacity of its values. PZ_AKASH_API_KEY is the exception — it is the credential the
# call is made with.
$ErrorActionPreference = "Stop"
$root = "C:\Users\hrkcz001\zomboid-akash"

# From secrets.env, not v1's manifest: the cutover deleted pz-controller/. The Akash
# key is the one credential here that has to be real — everything below is a throwaway
# because this gate only renders and submits an SDL.
foreach ($line in [IO.File]::ReadAllLines("C:\Users\hrkcz001\.pz-akash\secrets.env")) {
    if ($line -match '^(PZ_AKASH_API_KEY)=(.+)$') { Set-Item "env:$($Matches[1])" $Matches[2] }
}
if (-not $env:PZ_AKASH_API_KEY) { throw "PZ_AKASH_API_KEY is not in secrets.env" }

$env:PZ_DEPLOY_KEY_B64 = [Convert]::ToBase64String([Text.Encoding]::ASCII.GetBytes("gate-throwaway-not-a-key"))
$env:PZ_WEBHOOK_SECRET = "gate-throwaway"
$env:PZ_CLOUDFLARE_API_TOKEN = "gate-throwaway"
$env:PZ_STORAGE_PASSWORD = "gate-throwaway"
$env:PZ_SERVER_FILES_PASSWORD = "gate-throwaway"
$env:PZ_BACKUPS_PASSWORD = "gate-throwaway"
$env:PZ_RCON_PASSWORD = "gate-throwaway"
$env:PZ_ADMIN_PASSWORD = "gate-throwaway"
$env:PZ_JOIN_PASSWORD = "gate-throwaway"

& "$root\scratch\pzctl.exe" akash deploy -c "$root\scratch\gate-config.yaml" --role server
"EXIT $LASTEXITCODE"
