# gate.ps1 — the step 3 gate: a full start / backup / halt cycle driven by real
# trigger files against a real git remote, with the Akash driver stubbed.
#
# Everything happens on a throwaway bare repo under scratch/gate. Nothing here
# touches the live pz-saves remote, and --dry-run creates no deployment.
#
# The agent is written by hand, because the agent binary is step 4. Its branch is
# force-pushed as an orphan commit, which is exactly what gitbus.PutOrphan does.

$ErrorActionPreference = "Stop"

$gate   = Join-Path $PSScriptRoot "gate"
$remote = Join-Path $gate "remote.git"
$work   = Join-Path $gate "work"
$agent  = Join-Path $gate "agentwork"
# The controller's backups.dir. It stands in for the container's mounted volume, and
# it is what makes the backup half of this gate real: since step 6 the index is
# generated from this directory, so an archive the agent claims to have uploaded has
# to actually be here or the controller refuses to follow it.
$backups = Join-Path $gate "backups"
$pzctl  = Join-Path $PSScriptRoot "pzctl.exe"
$src    = Join-Path (Split-Path $PSScriptRoot -Parent) "pzctl\config.yaml"
$env:PZ_CONFIG = Join-Path $work "config.yaml"

function Step($msg) { Write-Host "`n=== $msg ===" -ForegroundColor Cyan }

# Seed builds the whole world from nothing, every run. A gate that depends on
# leftovers from the last run proves nothing about a cold start, which is the one
# state the controller is guaranteed to be in after a redeploy.
Step "seed"
if (Test-Path $gate) { Remove-Item -Recurse -Force $gate }
New-Item -ItemType Directory -Force $gate | Out-Null
New-Item -ItemType Directory -Force $backups | Out-Null
git init -q --bare --initial-branch=main $remote
git init -q --initial-branch=main $work
Copy-Item $src (Join-Path $work "config.yaml")
Push-Location $work
git add -A
git -c user.name=op -c user.email=op@example.invalid commit -q -m "config"
git remote add origin $remote
git push -q -u origin main
Pop-Location
Write-Host "seeded $remote from $src" -ForegroundColor DarkGray

# pass runs one reconcile: fetch, read the agent, consume triggers, advance, and
# wait for whatever job it started.
#
# --dry-state is what makes chaining passes meaningful. A real provider outlives the
# controller, so without it every fresh process would find the lease it created last
# pass missing and — correctly — declare it vanished.
function Pass() {
  & $pzctl controller --dry-run --once --repo $remote `
      --cache-dir (Join-Path $gate "mirror.git") `
      --backups-dir $backups `
      --dry-state (Join-Path $gate "provider.json")
  if ($LASTEXITCODE -ne 0) { throw "controller pass failed with $LASTEXITCODE" }
}

# Upload writes a real archive into backups.dir, standing in for the agent's HTTP
# upload, and returns its size and sha256.
#
# The file has to exist. The controller generates its index from this directory and
# refuses to follow a report naming an archive that is not in it, because pointing
# restore_target at a name the next boot cannot fetch is bug 4 wearing a different
# hat. A report with no file behind it would therefore leave both the index and
# restore_target empty — so this is the half of the gate that proves the storage
# layer and the state machine agree on what exists.
#
# The content is not a real zip. Nothing in this path parses it: the store hashes
# bytes and the restore is the agent's job, which is not stubbed here. Embedding the
# name keeps the two archives' digests distinct.
function Upload($name) {
  $path = Join-Path $backups $name
  [System.IO.File]::WriteAllText($path, "PK-not-really-a-zip $name`n" + ("x" * 4096))
  $f = Get-Item $path
  [ordered]@{
    size   = $f.Length
    sha256 = (Get-FileHash -Algorithm SHA256 $path).Hash.ToLower()
  }
}

# Trigger writes one trigger file and pushes it, the way an operator does.
function Trigger($name, $body) {
  Push-Location $work
  git pull --rebase -q origin main
  New-Item -ItemType Directory -Force (Join-Path $work "triggers") | Out-Null
  Set-Content -NoNewline -Path (Join-Path $work "triggers\$name") -Value $body
  git add -A
  git -c user.name=op -c user.email=op@example.invalid commit -q -m "trigger $name"
  git push -q origin main
  Pop-Location
  Write-Host "pushed triggers/$name" -ForegroundColor DarkGray
}

# AgentSay force-pushes an agent document. $extra is merged into the JSON, so a
# backup report can be attached without restating the whole document.
function AgentSay($phase, $extra) {
  $now = (Get-Date).ToString("yyyy-MM-ddTHH:mm:sszzz")
  $doc = [ordered]@{
    version = 1; updated_at = $now
    phase = $phase; since = $now
    players_count = 3; players_at = $now
    restarts = 0; liveness_at = $now
  }
  if ($extra) { foreach ($k in $extra.Keys) { $doc[$k] = $extra[$k] } }

  if (Test-Path $agent) { Remove-Item -Recurse -Force $agent }
  New-Item -ItemType Directory -Force $agent | Out-Null
  git init -q --initial-branch=state $agent
  $doc | ConvertTo-Json -Depth 6 | Set-Content -Path (Join-Path $agent "agent.json")
  Push-Location $agent
  git add -A
  git -c user.name=pz-agent -c user.email=agent@example.invalid commit -q -m "agent: $phase"
  git push -q --force $remote "HEAD:refs/heads/state/agent"
  Pop-Location
  Write-Host "agent says $phase" -ForegroundColor DarkGray
}

# GitQuiet runs a read that is allowed to fail — a ref or path that does not exist
# yet is a legitimate state here. ErrorActionPreference=Stop turns any native
# command's stderr into a terminating error, and 2>$null does not prevent that, so
# it is relaxed for the duration of the call. Returns $null on failure.
function GitQuiet([string[]]$rest) {
  $prev = $ErrorActionPreference
  $ErrorActionPreference = "Continue"
  try {
    $out = & git -C $remote @rest 2>$null
    if ($LASTEXITCODE -ne 0) { return $null }
    return $out
  } finally { $ErrorActionPreference = $prev }
}

# Doc reads the controller's published document, or $null when the branch does not
# exist yet.
function Doc() {
  $raw = GitQuiet @("show", "state/controller:controller.json") | Out-String
  if (-not $raw.Trim()) { return $null }
  $raw | ConvertFrom-Json
}

# Request reads the outstanding backup request off the controller's branch, which
# is how the real agent learns what to answer.
function Request() {
  $d = Doc
  if ($d) { $d.backup_request }
}

function Show($label) {
  $d = Doc
  if (-not $d) {
    Write-Host ("{0,-22} (no state branch published yet)" -f $label) -ForegroundColor Yellow
    return
  }
  $lease = if ($d.lease) { $d.lease.dseq } else { "none" }
  $req = if ($d.backup_request) { "$($d.backup_request.id)/$($d.backup_request.reason)" } else { "none" }
  Write-Host ("{0,-22} status={1,-10} intent={2,-8} lease={3,-10} request={4,-16} restore={5}" -f `
    $label, $d.status, $d.intent, $lease, $req, $d.restore_target) -ForegroundColor Green
}

Step "0. cold start"
Pass
Show "offline"

Step "1. start"
Trigger "start" ""
Pass
Show "after start"

Step "2. the agent comes online"
AgentSay "online" $null
Pass
Show "online"

Step "3. operator backup"
Trigger "backup" ""
Pass
Show "requested"
$r = Request
if (-not $r) { throw "no backup request was published" }
$name = "backup_" + (Get-Date).ToString("yyyyMMdd_HHmmss") + ".zip"
$up = Upload $name
AgentSay "online" @{ backup = [ordered]@{
  request_id = $r.id; state = "done"; name = $name; size = $up.size
  sha256 = $up.sha256; started_at = $r.requested_at
  ended_at = (Get-Date).ToString("yyyy-MM-ddTHH:mm:sszzz") } }
Pass
Show "backup done"
$d = Doc
if ($d.restore_target -ne $name) {
  throw "restore_target is '$($d.restore_target)', want '$name': the controller did not follow the report"
}

Step "4. halt"
Trigger "halt" ""
Pass
Show "stopping"
$h = Request
if (-not $h) { throw "the halt published no final backup request" }
if ($h.id -eq $r.id) { throw "the halt reused the operator request id" }
$final = "backup_" + (Get-Date).ToString("yyyyMMdd_HHmmss") + ".zip"
# One second of resolution: two archives minted inside the same second are one
# archive, and the second Upload would silently replace the first.
while ($final -eq $name) {
  Start-Sleep -Milliseconds 500
  $final = "backup_" + (Get-Date).ToString("yyyyMMdd_HHmmss") + ".zip"
}
$upf = Upload $final
AgentSay "stopped" @{ backup = [ordered]@{
  request_id = $h.id; state = "done"; name = $final; size = $upf.size
  sha256 = $upf.sha256; started_at = $h.requested_at
  ended_at = (Get-Date).ToString("yyyy-MM-ddTHH:mm:sszzz") } }
Pass
Show "after the final backup"
Pass
Show "closed"

Step "5. the index and the leftover triggers"
$raw = GitQuiet @("show", "state/controller:backups.json") | Out-String
if (-not $raw.Trim()) { throw "no backups.json was published" }
$idx = $raw | ConvertFrom-Json
$names = @($idx.items | ForEach-Object { $_.name })
Write-Host "index: $($names -join ', ')"
foreach ($want in @($name, $final)) {
  if ($names -notcontains $want) { throw "backups.json does not list $want" }
}
# Invariant I10, checked from the outside: every entry describes the file that is
# there, and every file that is there has an entry. The sizes and digests are the
# point — they can only be right if the store generated this from the directory,
# because nothing else in the system knows them.
foreach ($e in $idx.items) {
  $p = Join-Path $backups $e.name
  if (-not (Test-Path $p)) { throw "backups.json lists $($e.name), which is not on disk" }
  $f = Get-Item $p
  if ($e.size -ne $f.Length) {
    throw "$($e.name): the index says $($e.size) bytes, the file is $($f.Length)"
  }
  $sum = (Get-FileHash -Algorithm SHA256 $p).Hash.ToLower()
  if ($e.sha256 -ne $sum) { throw "$($e.name): the index says $($e.sha256), the file hashes to $sum" }
}
$onDisk = @(Get-ChildItem $backups -Filter "backup_*.zip" | ForEach-Object { $_.Name })
if ($onDisk.Count -ne $names.Count) {
  throw "the directory holds $($onDisk.Count) archive(s), the index lists $($names.Count)"
}
$d = Doc
if ($d.restore_target -ne $final) {
  throw "restore_target is '$($d.restore_target)', want the halt's archive '$final'"
}
# Every trigger consumed removes its file, so the directory itself disappears and
# ls-tree finds nothing. That is the pass condition, not a failure: a trigger left
# behind is a trigger that fires again on the next pass.
$left = GitQuiet @("ls-tree", "--name-only", "main:triggers")
Write-Host "triggers left on main: $(if ($left) { $left -join ', ' } else { '(none)' })"
if ($left) { throw "triggers survived: $($left -join ', ')" }
Write-Host "`nGATE PASSED" -ForegroundColor Green
