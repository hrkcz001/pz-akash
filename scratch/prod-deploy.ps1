# scratch/prod-deploy.ps1 — the production bring-up, one phase per run.
#
# This is the first script here that spends real money and carries real secrets, so
# two rules from earlier steps are load-bearing rather than stylistic:
#
#   1. Secrets are read from ~/.pz-akash/secrets.env into the environment. They are
#      never passed as arguments. A PowerShell parameter-binding error quotes the
#      offending argument verbatim, which is how two live passwords once ended up on
#      screen. Nothing below takes a secret as a parameter.
#   2. Every phase prints the dseq before it can fail. An Akash deploy funds an
#      escrow on submission, so a run that dies halfway has already started billing;
#      the dseq is the only handle `pzctl akash close` needs.
#
# Usage:
#   pwsh scratch/prod-deploy.ps1 -Phase controller   # deploy the controller, print its URL
#   pwsh scratch/prod-deploy.ps1 -Phase dns          # point the apex at the controller
#   pwsh scratch/prod-deploy.ps1 -Phase webhook      # install the pz-saves push hook
#   pwsh scratch/prod-deploy.ps1 -Phase start        # push triggers/start (fresh world)
#   pwsh scratch/prod-deploy.ps1 -Phase status       # leases, state branches, dashboard

param(
    [Parameter(Mandatory = $true)]
    [ValidateSet("controller", "dns", "webhook", "start", "status")]
    [string]$Phase,

    # Set by -Phase dns and -Phase webhook when the controller URL is already known;
    # otherwise both read it from scratch/prod-controller.json, written by -Phase
    # controller. Not a secret.
    [string]$ControllerUrl = ""
)

$ErrorActionPreference = "Stop"
$root       = "C:\Users\hrkcz001\zomboid-akash"
$saves      = "C:\Users\hrkcz001\pz-saves"
$config     = "$saves\config.yaml"          # the shipped config, not pzctl/config.yaml
$pzctl      = "$root\scratch\pzctl.exe"
$stateFile  = "$root\scratch\prod-controller.json"
$secretsEnv = "C:\Users\hrkcz001\.pz-akash\secrets.env"

# ---- secrets ---------------------------------------------------------------
# All ten, because the controller manifest carries all ten: the game passwords reach
# the .ini through the server deploy the controller itself makes, so the controller
# has to be handed them at deploy time.
function Import-Secrets {
    $loaded = @()
    foreach ($line in [IO.File]::ReadAllLines($secretsEnv)) {
        if ($line -match '^\s*(PZ_[A-Z0-9_]+)=(.*)$') {
            $name = $Matches[1]
            $value = $Matches[2].Trim().Trim('"')
            Set-Item "env:$name" $value
            $loaded += "{0} ({1} chars)" -f $name, $value.Length
        }
    }
    # Names and lengths only. This line exists so a truncated or half-pasted secret is
    # visible before it reaches a provider, without the value ever being printed.
    "secrets loaded: " + ($loaded -join ", ")
    foreach ($need in @("PZ_AKASH_API_KEY", "PZ_DEPLOY_KEY_B64", "PZ_WEBHOOK_SECRET")) {
        if (-not (Get-Item "env:$need" -ErrorAction SilentlyContinue).Value) {
            throw "$need is not in secrets.env"
        }
    }
}

function Get-SavedUrl {
    if ($ControllerUrl) { return $ControllerUrl }
    if (-not (Test-Path $stateFile)) {
        throw "no controller URL: pass -ControllerUrl or run -Phase controller first"
    }
    return (Get-Content $stateFile -Raw | ConvertFrom-Json).url
}

# Every capture of $pattern across $lines, in order, or an empty array.
#
# Plural because a deploy retries: `pzctl akash deploy` now loops up to
# akash.max_deploy_attempts, closing each failed deployment before the next, so the
# output can name several dseqs. The last is the live one; the earlier ones were
# closed by pzctl and are listed in the state file so the operator can confirm that
# against `pzctl akash leases` rather than take it on trust.
function Get-Captures {
    param([string[]]$Lines, [string]$Pattern)
    $found = @()
    foreach ($line in $Lines) {
        if ($line -match $Pattern) { $found += $Matches[1] }
    }
    return $found
}

# ---- controller ------------------------------------------------------------
# No --close, unlike every gate before this one: this deployment is meant to outlive
# the run. The controller is what deploys the game server, so this is the only
# deploy done by hand — every later one is the controller's own.
if ($Phase -eq "controller") {
    Import-Secrets
    "config: $config"
    & $pzctl config validate -c $config
    if ($LASTEXITCODE -ne 0) { throw "config validate failed — not deploying" }

    "--- deploying the controller (real money from here) ---"
    $out = & $pzctl akash deploy -c $config --role controller 2>&1
    $code = $LASTEXITCODE
    $out | ForEach-Object { "  $_" }

    # Parsed out of the output rather than asked for again, and written to disk before
    # anything is allowed to throw: the dseq is the handle that stops the billing.
    #
    # Match-or-empty, deliberately. The obvious `(... | Select-String ...).Matches`
    # throws "cannot index into a null array" when the pattern is absent — which is
    # exactly the failed run where the dseq matters most. The live controller deploy
    # printed a dseq and no url, and that line is what turned it into a stack trace
    # instead of a saved state file. The failure is now reported by the explicit
    # checks below rather than by an indexing accident.
    $dseqs = @(Get-Captures $out '^\s*dseq\s+(\S+)')
    $urls  = @(Get-Captures $out '^\s*url\s+(\S+)')
    $dseq  = if ($dseqs.Count) { $dseqs[-1] } else { "" }   # the live one; see Get-Captures
    $url   = if ($urls.Count)  { $urls[-1]  } else { "" }
    if ($dseqs.Count) {
        @{ dseq = $dseq; url = $url; role = "controller"; all_dseqs = $dseqs } |
            ConvertTo-Json | Set-Content -Path $stateFile -Encoding utf8
        "saved: $stateFile"
        if ($dseqs.Count -gt 1) {
            "note: $($dseqs.Count) deployments were created across retries; pzctl closed all " +
                "but $dseq — confirm with 'pzctl akash leases': " + ($dseqs -join ", ")
        }
    } else {
        "no dseq in the output — nothing was created, nothing is billing"
    }
    if ($code -ne 0) { throw "deploy exited $code (dseq above, if any — close it if this is not recoverable)" }
    if (-not $url) { throw "deploy reported no url; dseq $dseq is open" }
    "CONTROLLER dseq=$dseq url=$url"
    exit 0
}

# ---- dns -------------------------------------------------------------------
# The controller record is the one DNS entry pzctl cannot write on its own: a
# container has no way to learn the address Akash gave its own lease. The game record
# is written by the controller on every server deploy, so it is not touched here.
if ($Phase -eq "dns") {
    Import-Secrets
    $url = Get-SavedUrl
    "controller: $url"
    "--- dry run first ---"
    & $pzctl dns sync -c $config --controller $url --dry-run
    if ($LASTEXITCODE -ne 0) { throw "dns dry-run failed" }
    "--- applying ---"
    & $pzctl dns sync -c $config --controller $url
    if ($LASTEXITCODE -ne 0) { throw "dns sync failed" }
    exit 0
}

# ---- webhook ---------------------------------------------------------------
# The hook is what makes a trigger take effect in seconds instead of at the next
# idle poll. Installed with `gh`, so the secret travels as a JSON body field on a
# local HTTPS call and never as an argv element.
if ($Phase -eq "webhook") {
    Import-Secrets
    $url = Get-SavedUrl
    $hookUrl = ($url.TrimEnd("/")) + "/webhook"
    "hook target: $hookUrl"

    $existing = gh api repos/hrkcz001/pz-saves/hooks --jq '.[] | (.id|tostring) + " " + .config.url' 2>&1
    if ($LASTEXITCODE -ne 0) { throw "could not list hooks: $existing" }
    "existing hooks: " + $(if ($existing) { $existing -join "; " } else { "none" })

    # Rebuilt rather than patched when one already points at a dead lease: a hook's
    # secret cannot be read back, so reusing the row would leave us unable to say
    # which secret it holds.
    foreach ($row in @($existing)) {
        if ($row) {
            $hookId = ($row -split ' ')[0]
            "deleting stale hook $hookId"
            gh api -X DELETE "repos/hrkcz001/pz-saves/hooks/$hookId" | Out-Null
        }
    }

    $body = @{
        name   = "web"
        active = $true
        events = @("push")
        config = @{
            url          = $hookUrl
            content_type = "json"
            insecure_ssl = "0"
            secret       = $env:PZ_WEBHOOK_SECRET
        }
    } | ConvertTo-Json -Depth 5

    $tmp = Join-Path ([IO.Path]::GetTempPath()) "pz-hook.json"
    try {
        [IO.File]::WriteAllText($tmp, $body, (New-Object Text.UTF8Encoding $false))
        $created = gh api -X POST repos/hrkcz001/pz-saves/hooks --input $tmp 2>&1
        if ($LASTEXITCODE -ne 0) { throw "hook create failed: $created" }
    } finally {
        # The only file that ever holds the webhook secret in the clear, and it does
        # not outlive the call that needed it.
        if (Test-Path $tmp) { Remove-Item $tmp -Force }
    }
    gh api repos/hrkcz001/pz-saves/hooks --jq '.[] | "hook " + (.id|tostring) + " -> " + .config.url + " events=" + (.events|join(",")) + " active=" + (.active|tostring)'
    exit 0
}

# ---- start -----------------------------------------------------------------
# An empty triggers/start, and nothing else. backups.restore_policy is `latest`
# with an empty index, which is what "fresh world" means here — no restore trigger
# is pushed, because an empty restore body would only clear a target that is
# already unset.
if ($Phase -eq "start") {
    $trigger = "$saves\triggers\start"
    New-Item -ItemType Directory -Force -Path "$saves\triggers" | Out-Null
    [IO.File]::WriteAllText($trigger, "")
    Push-Location $saves
    try {
        git add triggers/start
        git -c user.name="hrkcz001" -c user.email="hrkcz001@users.noreply.github.com" `
            commit -q -m "start: bring up a fresh world on the v2 stack"
        git push origin main
        if ($LASTEXITCODE -ne 0) { throw "push failed" }
        "pushed " + (git log --oneline -1)
    } finally { Pop-Location }
    "the controller consumes the trigger and deletes it; watch -Phase status"
    exit 0
}

# ---- status ----------------------------------------------------------------
if ($Phase -eq "status") {
    Import-Secrets
    "--- leases ---"
    & $pzctl akash leases -c $config
    "--- state branches ---"
    Push-Location $saves
    try {
        git fetch -q origin "+refs/heads/*:refs/remotes/origin/*"
        git branch -r --list "origin/*" | ForEach-Object { "  " + $_.Trim() }
        "--- triggers still pending ---"
        $pending = git ls-tree -r --name-only origin/main -- triggers
        "  " + $(if ($pending) { $pending -join ", " } else { "(none - consumed)" })
    } finally { Pop-Location }
    "--- controller's view ---"
    & $pzctl state show -c $config
    exit 0
}
