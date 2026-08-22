# scratch/dseq-keys.ps1 — the shape of a deployment detail, without its contents.
#
# The response embeds the manifest, and the manifest embeds every env value handed to
# the container, so this prints paths and type names only. Values appear for numbers
# and booleans, and for strings only when the path is on the allow-list below —
# enough to read a provider's verdict, never enough to leak a password.
param([Parameter(Mandatory = $true)][string]$DSeq)

$ErrorActionPreference = "Stop"
foreach ($line in [IO.File]::ReadAllLines("C:\Users\hrkcz001\.pz-akash\secrets.env")) {
    if ($line -match '^\s*PZ_AKASH_API_KEY=(.*)$') { $key = $Matches[1].Trim().Trim('"') }
}
$raw = Invoke-RestMethod -Uri "https://console-api.akash.network/v1/deployments/$DSeq" `
    -Headers @{ "x-api-key" = $key } -TimeoutSec 60

# Paths whose string values are safe and interesting: state machines, timestamps,
# provider identities, and anything a provider says about why a pod is not running.
$safe = 'state|status|reason|message|error|time|date|height|provider|owner|dseq|gseq|oseq|denom|amount|balance|available|replicas|observed|name$|uri'

function Walk($node, $path, $depth) {
    if ($depth -gt 6) { return }
    if ($null -eq $node) { return }
    if ($node -is [string]) {
        if ($path -match $safe -and $node.Length -lt 200) { "  $path = $node" }
        else { "  $path : string($($node.Length))" }
        return
    }
    if ($node -is [bool] -or $node -is [int] -or $node -is [long] -or $node -is [double] -or $node -is [decimal]) {
        "  $path = $node"; return
    }
    if ($node -is [object[]]) {
        "  $path : array($($node.Count))"
        for ($i = 0; $i -lt [Math]::Min($node.Count, 4); $i++) { Walk $node[$i] "$path[$i]" ($depth + 1) }
        return
    }
    foreach ($p in $node.PSObject.Properties) {
        # The manifest is the one subtree that is all secrets and no answers.
        if ($p.Name -match '^(manifest|env|sdl|command|args)$') { "  $path.$($p.Name) : SKIPPED"; continue }
        Walk $p.Value "$path.$($p.Name)" ($depth + 1)
    }
}
"=== dseq $DSeq ==="
Walk $raw.data "data" 0
