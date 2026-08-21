# scratch/set-github-secrets.ps1 — move v1's secrets out of git and into GitHub.
#
# This implements the locked decision "reuse everything, just move it out of git":
# no rotation, the same values, sourced from where v1 kept them and written to the
# repository's Actions secrets so step 8's CI can render an SDL without any of them
# living in a committed file.
#
# Sources:
#   pz-controller/deployment.yaml   eight values, including the deploy key
#   pz-saves/server/Server/vsrania.ini   the join password (ini `Password=`)
#
# PZ_ADMIN_PASSWORD is deliberately absent: v1 never set one. sdl.template carries
# the placeholder, but deployment.yaml never filled it, so entrypoint.sh's
# ADMIN_FLAG stayed empty and the live server runs with no admin password at all.
# There is nothing to reuse, so that one value has to be chosen rather than moved.
#
# No secret value is ever printed. Each is written to a temp file with no trailing
# newline and fed to gh over stdin — not --body, which would put it in the process
# command line, and not a PowerShell pipe, which would append a newline that a
# rendered SDL would carry into the manifest.
$ErrorActionPreference = "Stop"
$root = "C:\Users\hrkcz001\zomboid-akash"
$repo = "hrkcz001/pz-akash"

$manifest = Get-Content "$root\pz-controller\deployment.yaml" -Raw
$ini = Get-Content "C:\Users\hrkcz001\pz-saves\server\Server\vsrania.ini" -Raw

function FromManifest($name) {
    if ($manifest -match "(?m)^\s*-\s*$name=(.+)$") { return $Matches[1].Trim() }
    throw "$name not found in pz-controller/deployment.yaml"
}
function FromIni($key) {
    if ($ini -match "(?m)^\s*$key\s*=(.*)$") { return $Matches[1].Trim() }
    throw "$key not found in vsrania.ini"
}

# Ordered so the report reads like internal/secrets' own registry.
$map = [ordered]@{
    PZ_DEPLOY_KEY_B64        = FromManifest "SSH_PRIVATE_KEY_BASE64"
    PZ_AKASH_API_KEY         = FromManifest "AKASH_API_KEY"
    PZ_WEBHOOK_SECRET        = FromManifest "WEBHOOK_SECRET"
    PZ_CLOUDFLARE_API_TOKEN  = FromManifest "CLOUDFLARE_API_TOKEN"
    PZ_STORAGE_PASSWORD      = FromManifest "STORAGE_PASSWORD"
    PZ_SERVER_FILES_PASSWORD = FromManifest "SERVER_FILES_PASSWORD"
    PZ_BACKUPS_PASSWORD      = FromManifest "BACKUPS_PASSWORD"
    PZ_RCON_PASSWORD         = FromManifest "RCON_PASSWORD"
    PZ_JOIN_PASSWORD         = FromIni "Password"
}

Write-Output "repo: $repo`n"
foreach ($name in $map.Keys) {
    $v = $map[$name]
    if ([string]::IsNullOrWhiteSpace($v)) { throw "$name resolved to an empty value" }
    $tmp = [IO.Path]::Combine([IO.Path]::GetTempPath(), [Guid]::NewGuid().ToString("N"))
    try {
        [IO.File]::WriteAllText($tmp, $v, (New-Object Text.UTF8Encoding $false))
        cmd /c "gh secret set $name --repo $repo < ""$tmp""" 2>&1 | Out-Null
        $ok = ($LASTEXITCODE -eq 0)
    } finally {
        Remove-Item $tmp -Force -ErrorAction SilentlyContinue
    }
    $status = "FAILED"
    if ($ok) { $status = "set" }
    Write-Output ("  {0,-26} {1,4} chars  {2}" -f $name, $v.Length, $status)
}

Write-Output "`n--- repo secrets now ---"
gh secret list --repo $repo
