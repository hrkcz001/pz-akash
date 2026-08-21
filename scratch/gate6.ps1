# gate6.ps1 — the step 6 gate: a manual download/upload round-trip against a real
# controller process, over real HTTP.
#
# What this proves, in the order the assertions run:
#   * the realms are closed by default and open only to the right token,
#   * an upload is streamed, not buffered — the v1 defect this step exists to fix
#     was `body_bytes = self.rfile.read(content_length)` in a 2Gi container,
#   * the bytes that come back are the bytes that went in, digest included,
#   * a corrupt upload leaves nothing behind,
#   * the index the store generates reaches the state branch (the step 6 handoff),
#   * and server.zip has the real passwords substituted into it on the way out.
#
# Everything lives under scratch/gate6. Nothing touches the live remote, and
# --dry-run creates no deployment.

$ErrorActionPreference = "Stop"

$gate    = Join-Path $PSScriptRoot "gate6"
$remote  = Join-Path $gate "remote.git"
$work    = Join-Path $gate "work"
$backups = Join-Path $gate "backups"
$pkgs    = Join-Path $gate "packages"
$tmp     = Join-Path $gate "tmp"
$pzctl   = Join-Path $PSScriptRoot "pzctl.exe"
$src     = Join-Path (Split-Path $PSScriptRoot -Parent) "pzctl\config.yaml"

# A high port, because 8000 is a port a laptop is likely to already be serving
# something on and a gate that fails for that reason teaches nothing.
$port = 18000
$base = "http://127.0.0.1:$port"

# The size of the streaming test upload, in MiB. Large enough that a server which
# buffered it would be obvious in the process's RSS, small enough to run on a laptop.
$bigMB = 256

# The realm tokens. Test values: this process never talks to anything real.
$env:PZ_BACKUPS_PASSWORD      = "gate6-backups-token"
$env:PZ_SERVER_FILES_PASSWORD = "gate6-server-files-token"
$env:PZ_JOIN_PASSWORD         = "gate6-join-password"
$env:PZ_ADMIN_PASSWORD        = "gate6-admin-password"
$env:PZ_RCON_PASSWORD         = "gate6-rcon-password"

$pass = 0
function Step($msg) { Write-Host "`n=== $msg ===" -ForegroundColor Cyan }
function Ok($msg) { $script:pass++; Write-Host "  ok  $msg" -ForegroundColor Green }
function Bad($msg) { throw $msg }

# Req is one HTTP request. curl rather than Invoke-WebRequest because the gate cares
# about status codes on failures, and curl reports them without the two shells'
# different opinions about which of those are exceptions. The arguments are passed as
# an array so that a leading -H is never mistaken for a parameter of this function.
function Req([string[]]$CurlArgs, [string]$Out, [switch]$Binary) {
  $body = if ($Out) { $Out } else { Join-Path $tmp "body" }
  $head = Join-Path $tmp "head"
  $prev = $ErrorActionPreference
  $ErrorActionPreference = "Continue"
  try {
    $code = & curl.exe -sS --max-time 600 -o $body -D $head -w "%{http_code}" @CurlArgs 2>&1
  } finally { $ErrorActionPreference = $prev }
  # -Binary leaves the body on disk and out of this process. A 256 MiB archive read
  # into a string would make the gate itself the memory hog it is testing for.
  $text = if ($Binary) { $null } else { Get-Content -Raw -ErrorAction SilentlyContinue $body }
  [ordered]@{
    code    = [int]($code | Select-Object -Last 1)
    body    = $body
    text    = $text
    headers = (Get-Content -Raw -ErrorAction SilentlyContinue $head)
  }
}

# Archive makes one backup file of n random-but-deterministic bytes and returns its
# path and digest. The bytes are not a real zip: nothing in the storage layer parses
# a backup, and pretending otherwise would only hide that. The seed is a parameter so
# two archives of the same size can differ, which is what a replacement needs.
function Archive($file, $bytes, $seed = 42) {
  $path = Join-Path $tmp $file
  $fs = [System.IO.File]::Create($path)
  try {
    $rand = New-Object System.Random $seed
    $chunk = New-Object byte[] (1MB)
    $left = $bytes
    while ($left -gt 0) {
      $rand.NextBytes($chunk)
      $n = [Math]::Min($left, $chunk.Length)
      $fs.Write($chunk, 0, $n)
      $left -= $n
    }
  } finally { $fs.Dispose() }
  [ordered]@{ path = $path; size = (Get-Item $path).Length; sha = (Sha $path) }
}

function AsJson($r) {
  try { return ($r.text | ConvertFrom-Json) }
  catch { Bad "the response is not JSON (HTTP $($r.code)): $($r.text)" }
}

function Header($r, $name) {
  foreach ($line in ($r.headers -split "`r?`n")) {
    if ($line -match "^(?i)$([regex]::Escape($name))\s*:\s*(.+)$") { return $Matches[1].Trim() }
  }
  return $null
}

function Sha($path) { (Get-FileHash -Algorithm SHA256 $path).Hash.ToLower() }

# --- the world ---

# Built here rather than assumed. A gate that passes against yesterday's binary is
# worse than no gate, and the build is two seconds.
Step "build"
Push-Location (Join-Path (Split-Path $PSScriptRoot -Parent) "pzctl")
try {
  & go build -o $pzctl ./cmd/pzctl
  if ($LASTEXITCODE -ne 0) { throw "go build failed" }
} finally { Pop-Location }
Write-Host "built $pzctl" -ForegroundColor DarkGray

Step "seed"
if (Test-Path $gate) { Remove-Item -Recurse -Force $gate }
foreach ($d in @($gate, $backups, $pkgs, $tmp)) { New-Item -ItemType Directory -Force $d | Out-Null }

git init -q --bare --initial-branch=main $remote
git init -q --initial-branch=main $work

# The committed config is written for the container: absolute container paths and a
# port the deployment exposes. Rewriting those three lines is what an operator does
# to run the same binary on a laptop, and doing it here rather than adding flags
# keeps the gate honest about which values the real config supplies.
$cfg = Get-Content -Raw $src
$cfg = $cfg -replace '(?m)^(\s*packages_dir:).*$', "`$1 $($pkgs -replace '\\','/')"
$cfg = $cfg -replace '(?m)^(\s*dir:)\s*/data/backups\s*$', "`$1 $($backups -replace '\\','/')"
$cfg = $cfg -replace '(?m)^(\s*http_port:).*$', "`$1 $port"
Set-Content -Path (Join-Path $work "config.yaml") -Value $cfg -NoNewline
$env:PZ_CONFIG = Join-Path $work "config.yaml"

Push-Location $work
git add -A
git -c user.name=op -c user.email=op@example.invalid commit -q -m "config"
git remote add origin $remote
git push -q -u origin main
Pop-Location
Write-Host "seeded $remote, backups=$backups, port=$port" -ForegroundColor DarkGray

# The three packages. common.zip and client.zip are public and served verbatim;
# server.zip carries the .ini files with placeholders, which is the archive the
# substituter rewrites on the way out.
Step "packages"
$stage = Join-Path $tmp "stage"
New-Item -ItemType Directory -Force (Join-Path $stage "Server") | Out-Null
Set-Content -NoNewline -Path (Join-Path $stage "Server\vsrania.ini") -Value @"
Password=__JOIN_PASSWORD__
RCONPassword=__RCON_PASSWORD__
DefaultPort=16261
"@
# A second entry, not matched by substitute_entries (Server/*.ini), which must come
# across byte-identical and still compressed. Its digest is checked below.
$modBytes = [byte[]](1..4096 | ForEach-Object { $_ % 251 })
[System.IO.File]::WriteAllBytes((Join-Path $stage "mods.bin"), $modBytes)
Compress-Archive -Force -Path (Join-Path $stage "*") -DestinationPath (Join-Path $pkgs "server.zip")
Compress-Archive -Force -Path (Join-Path $stage "mods.bin") -DestinationPath (Join-Path $pkgs "common.zip")
Compress-Archive -Force -Path (Join-Path $stage "mods.bin") -DestinationPath (Join-Path $pkgs "client.zip")

# substitute_entries matches zip entry names with path.Match, which knows only "/".
# Some older zip writers store "Server\vsrania.ini", and the symptom of that is a
# substitution that silently does nothing — so the fixture is checked here rather
# than being diagnosed three assertions later.
Add-Type -AssemblyName System.IO.Compression.FileSystem
$zip = [System.IO.Compression.ZipFile]::OpenRead((Join-Path $pkgs "server.zip"))
try { $entries = @($zip.Entries.FullName) } finally { $zip.Dispose() }
if ($entries -notcontains "Server/vsrania.ini") {
  throw "the fixture zip stores $($entries -join ', '): Server/vsrania.ini is what substitute_entries can match"
}
Write-Host "built server.zip, common.zip, client.zip" -ForegroundColor DarkGray

# --- the controller ---

Step "start the controller"
$log = Join-Path $gate "controller.log"
# --dry-run because nothing here needs a lease. The file service is live regardless:
# it is not part of the Akash driver, which is the whole point of step 6 being
# testable without spending money.
$proc = Start-Process -FilePath $pzctl -PassThru -NoNewWindow `
  -RedirectStandardError $log -RedirectStandardOutput (Join-Path $gate "controller.out") `
  -ArgumentList @(
    "controller", "--dry-run", "--repo", $remote,
    "--cache-dir", (Join-Path $gate "mirror.git"),
    "--backups-dir", $backups,
    "--dry-state", (Join-Path $gate "provider.json"),
    "--timeout", "10m")

# Everything after this point must bring the process down, including a throw.
$ok = $false
try {
  $up = $false
  foreach ($i in 1..80) {
    Start-Sleep -Milliseconds 250
    if ($proc.HasExited) { Bad "the controller exited during startup:`n$(Get-Content -Raw $log)" }
    $h = Req "$base/healthz"
    if ($h.code -eq 200) { $up = $true; break }
  }
  if (-not $up) { Bad "the file service never came up on $base :`n$(Get-Content -Raw $log)" }
  Ok "the file service answers /healthz"

  # --- what is open and what is closed ---

  Step "realms"
  # backups.json is public on purpose: it lists names, sizes and digests, which are
  # not secrets, and the dashboard reads it from a browser that holds no bearer
  # token. Downloading anything it names is a different question, asked below.
  $r = Req @("$base/backups.json")
  if ($r.code -ne 200) { Bad "GET /backups.json = $($r.code), want 200 (it is public)" }
  $idx = AsJson $r
  if ($idx.items.Count -ne 0) { Bad "the index starts with $($idx.items.Count) item(s), want none" }
  Ok "/backups.json is public and starts empty"

  $name = "backup_20260819_120000.zip"
  # Auth is asked before existence, so this says nothing about whether the archive is
  # there. That is the property being checked: an unauthenticated caller cannot probe
  # the directory by watching 404 turn into 401.
  $r = Req @("$base/backups/$name")
  if ($r.code -ne 401) { Bad "GET /backups/$name without a token = $($r.code), want 401" }
  if ((Header $r "WWW-Authenticate") -notmatch 'realm="backups"') {
    Bad "the challenge does not name the realm: $(Header $r 'WWW-Authenticate')"
  }
  $r = Req @("-H", "Authorization: Bearer not-the-token", "$base/backups/$name")
  if ($r.code -ne 401) { Bad "GET /backups/$name with a wrong token = $($r.code), want 401" }
  Ok "a backup download is closed without the backups token, and the challenge names the realm"

  $r = Req @("-H", "Authorization: Bearer $env:PZ_BACKUPS_PASSWORD", "$base/backups/sub/$name")
  if ($r.code -ne 404) { Bad "GET /backups/sub/$name = $($r.code), want 404" }
  Ok "a nested backup path is refused before anything joins it onto a directory"

  # --- the round trip ---

  Step "upload"
  $tok = "Authorization: Bearer $env:PZ_BACKUPS_PASSWORD"
  $a = Archive $name (512KB)

  $r = Req @("-T", $a.path, "-H", "X-PZ-SHA256: $($a.sha)", "$base/backups/$name")
  if ($r.code -ne 401) { Bad "PUT without a token = $($r.code), want 401" }
  if (Test-Path (Join-Path $backups $name)) { Bad "an unauthenticated PUT still wrote the file" }
  Ok "an upload without the token writes nothing"

  $r = Req @("-T", $a.path, "-H", $tok, "-H", "X-PZ-SHA256: $($a.sha)",
    "-H", "X-PZ-Request-Id: gate6-req-1", "-H", "X-PZ-Phase: halting",
    "$base/backups/$name")
  if ($r.code -ne 201) { Bad "PUT = $($r.code), want 201 for a new archive: $($r.text)" }
  $res = AsJson $r
  if ($res.name -ne $name -or $res.size -ne $a.size -or $res.sha256 -ne $a.sha) {
    Bad "the upload echoed name=$($res.name) size=$($res.size) sha=$($res.sha256), want $name/$($a.size)/$($a.sha)"
  }
  Ok "PUT /backups/<name> stores the archive and echoes the size and digest it wrote"

  # The index is generated from the directory, so these two numbers can only agree if
  # the store looked at the file rather than believing the uploader.
  $idx = AsJson (Req @("$base/backups.json"))
  if ($idx.items.Count -ne 1) { Bad "the index lists $($idx.items.Count) item(s), want 1" }
  $e = $idx.items[0]
  if ($e.name -ne $name -or $e.size -ne $a.size -or $e.sha256 -ne $a.sha) {
    Bad "the index says name=$($e.name) size=$($e.size) sha=$($e.sha256), want $name/$($a.size)/$($a.sha)"
  }
  if ($e.downloaded_at) { Bad "the archive is marked downloaded before anyone fetched it: $($e.downloaded_at)" }
  Ok "the index describes the archive the directory holds, and nobody has downloaded it yet"

  Step "download"
  $dl = Join-Path $tmp "downloaded.zip"
  $r = Req @("-H", $tok, "$base/backups/$name") -Out $dl -Binary
  if ($r.code -ne 200) { Bad "GET /backups/$name = $($r.code), want 200" }
  if ((Sha $dl) -ne $a.sha) { Bad "the bytes that came back are not the bytes that went in" }
  if ((Header $r "X-PZ-SHA256") -ne $a.sha) {
    Bad "the download advertised digest $(Header $r 'X-PZ-SHA256'), want $($a.sha)"
  }
  Ok "the archive comes back byte-identical, with the digest on the response"

  # The one fact the disk cannot hold. Without persistent storage it is the only
  # evidence a copy exists anywhere else.
  $e = (AsJson (Req @("$base/backups.json"))).items[0]
  if (-not $e.downloaded_at) { Bad "the fetch was not recorded in the index" }
  Ok "the fetch is recorded as downloaded_at ($($e.downloaded_at))"

  # --- what must not land ---

  Step "refusals"
  $corrupt = "backup_20260819_130000.zip"
  $c = Archive "corrupt.bin" (64KB) 7
  $wrong = "0" * 64
  $r = Req @("-T", $c.path, "-H", $tok, "-H", "X-PZ-SHA256: $wrong", "$base/backups/$corrupt")
  # 422, not 400: the request was well formed and the intent was clear, so the agent
  # should retry the transfer rather than stop and report a bad request.
  if ($r.code -ne 422) { Bad "a wrong-digest PUT = $($r.code), want 422: $($r.text)" }
  if (Test-Path (Join-Path $backups $corrupt)) {
    Bad "a corrupt upload is on disk under a name the restore path could choose"
  }
  # The temp file lands in the same directory as the archive, so a leak here is a leak
  # of the disk the next free-space check is about to measure.
  $left = @(Get-ChildItem -Force $backups | Where-Object { $_.Name -like ".*" })
  if ($left.Count -ne 0) { Bad "a refused upload left $($left.Count) temp file(s) behind: $($left.Name -join ', ')" }
  $idx = AsJson (Req @("$base/backups.json"))
  if ($idx.items.Count -ne 1) { Bad "the index grew to $($idx.items.Count) after a refused upload" }
  Ok "a body that does not match its digest is refused and leaves nothing behind"

  $r = Req @("-T", $c.path, "-H", $tok, "$base/backups/world.zip")
  if ($r.code -ne 400) { Bad "PUT /backups/world.zip = $($r.code), want 400" }
  Ok "a name that is not a backup filename is refused, so the directory stays self-describing"

  Step "replacement"
  # A different size as well as different bytes, because the digest reuse key is
  # (name, size, mtime): a replacement that changed only the contents is the case a
  # size-only key would miss, and internal/httpapi tests that one directly.
  $a2 = Archive "replacement.bin" (300KB) 9
  $r = Req @("-T", $a2.path, "-H", $tok, "-H", "X-PZ-SHA256: $($a2.sha)", "$base/backups/$name")
  # 200 rather than 201: both are success, and the difference is what lets a caller
  # tell a retry that landed twice from two archives it thought it had.
  if ($r.code -ne 200) { Bad "a repeat PUT = $($r.code), want 200 for a replacement: $($r.text)" }
  $idx = AsJson (Req @("$base/backups.json"))
  if ($idx.items.Count -ne 1) { Bad "a replacement produced $($idx.items.Count) entries" }
  $e = $idx.items[0]
  if ($e.size -ne $a2.size -or $e.sha256 -ne $a2.sha) {
    Bad "the index still describes the archive that was replaced: size=$($e.size) sha=$($e.sha256)"
  }
  Ok "PUT is idempotent, and the index follows the bytes that are actually there now"

  # --- the defect this step exists to fix ---

  Step "streaming"
  # v1's handler was `body_bytes = self.rfile.read(content_length)` in a container with
  # 2Gi, so the halt backup of a 3Gi world OOM-killed the controller at the one moment
  # the world depended on it. The only honest test of the fix is to watch the process:
  # a buffering server cannot receive $bigMB MiB without holding $bigMB MiB.
  $big = "backup_20260819_140000.zip"
  $b = Archive "big.bin" ($bigMB * 1MB) 11
  $proc.Refresh()
  $before = $proc.PeakWorkingSet64
  $r = Req @("-T", $b.path, "-H", $tok, "-H", "X-PZ-SHA256: $($b.sha)",
    "-H", "X-PZ-Request-Id: gate6-req-big", "$base/backups/$big")
  if ($r.code -ne 201) { Bad "the large PUT = $($r.code), want 201: $($r.text)" }
  $proc.Refresh()
  $peak = $proc.PeakWorkingSet64
  # Half the upload, with a floor so the assertion is about buffering rather than about
  # the Go runtime's own footprint.
  $budget = [int64][Math]::Max([double]96MB, [double]($bigMB * 1MB) / 2)
  Write-Host ("    peak RSS {0:N0} MiB before, {1:N0} MiB after a {2} MiB upload" -f
    ($before / 1MB), ($peak / 1MB), $bigMB) -ForegroundColor DarkGray
  if ($peak -ge $budget) {
    Bad ("the controller's peak RSS reached {0:N0} MiB uploading {1} MiB, over the {2:N0} MiB budget, which is the v1 defect" -f
      ($peak / 1MB), $bigMB, ($budget / 1MB))
  }
  $stored = Join-Path $backups $big
  if ((Get-Item $stored).Length -ne $b.size) { Bad "the stored archive is $((Get-Item $stored).Length) bytes, want $($b.size)" }
  if ((Sha $stored) -ne $b.sha) { Bad "the stored archive does not hash to what was sent" }
  Ok "a $bigMB MiB upload is streamed to disk, not buffered in RAM"

  $dlbig = Join-Path $tmp "big-back.bin"
  $r = Req @("-H", $tok, "$base/backups/$big") -Out $dlbig -Binary
  if ($r.code -ne 200) { Bad "the large GET = $($r.code), want 200" }
  if ((Sha $dlbig) -ne $b.sha) { Bad "the large archive did not come back identical" }
  Ok "and it comes back identical"

  # --- the packages ---

  Step "server.zip and the public packages"
  foreach ($p in @("common.zip", "client.zip")) {
    $r = Req @("$base/$p") -Out (Join-Path $tmp $p) -Binary
    if ($r.code -ne 200) { Bad "GET /$p = $($r.code), want 200: these are public" }
  }
  # common.zip carries mods and non-secret config, client.zip is what players
  # download, and a realm on either would break a player's first join with a 401.
  Ok "common.zip and client.zip are public"

  $r = Req @("$base/server.zip") -Out (Join-Path $tmp "server-unauth.zip") -Binary
  if ($r.code -ne 401) { Bad "GET /server.zip without a token = $($r.code), want 401" }
  if ((Header $r "WWW-Authenticate") -notmatch 'realm="server-files"') {
    Bad "the server.zip challenge does not name its realm: $(Header $r 'WWW-Authenticate')"
  }
  # v1 served this unauthenticated whenever the env var failed to reach the container,
  # and a working download looks identical either way, so nobody could see it.
  Ok "server.zip is closed without the server-files token"

  $got = Join-Path $tmp "server-got.zip"
  $r = Req @("-H", "Authorization: Bearer $env:PZ_SERVER_FILES_PASSWORD", "$base/server.zip") -Out $got -Binary
  if ($r.code -ne 200) { Bad "GET /server.zip with the token = $($r.code), want 200" }

  $out = Join-Path $tmp "server-out"
  if (Test-Path $out) { Remove-Item -Recurse -Force $out }
  Expand-Archive -Path $got -DestinationPath $out
  $ini = Get-Content -Raw (Join-Path $out "Server\vsrania.ini")
  if ($ini -match "__[A-Z_]+__") { Bad "a placeholder survived into the served .ini: $($Matches[0])" }
  if ($ini -notmatch "(?m)^Password=$([regex]::Escape($env:PZ_JOIN_PASSWORD))$") {
    Bad "the join password was not substituted"
  }
  if ($ini -notmatch "(?m)^RCONPassword=$([regex]::Escape($env:PZ_RCON_PASSWORD))$") {
    Bad "the RCON password was not substituted"
  }
  if ($ini -notmatch "(?m)^DefaultPort=16261$") { Bad "the rest of the .ini did not survive the rewrite" }
  Ok "server.zip is served with the real passwords substituted into Server/*.ini"

  # The entry that does not match substitute_entries is copied with its compressed
  # bytes intact, which is what makes rewriting a few hundred megabytes of mods on
  # every boot affordable. If it were being inflated and re-deflated it would still
  # compare equal, so this is a cheaper claim than the code makes; what it does catch
  # is a rewrite that corrupts everything it was supposed to leave alone.
  if ((Sha (Join-Path $out "mods.bin")) -ne (Sha (Join-Path $stage "mods.bin"))) {
    Bad "the non-matching entry did not survive the substitution byte-identical"
  }
  Ok "the entries that match nothing come through unchanged"

  # --- the step 6 handoff ---

  Step "the index reaches the state branch"
  # The store generates the index and the machine publishes it, and this is the only
  # assertion that both halves are wired to each other over the real transport: the
  # store's onChange nudges the FSM, the FSM compares and commits, and git pushes.
  # git.min_push_interval coalesces, so this waits.
  $published = $null
  foreach ($i in 1..60) {
    # git show fails until the branch exists, and in PowerShell 7 a non-zero native
    # exit code under ErrorActionPreference=Stop is a terminating error, so the
    # expected failure has to be caught rather than inspected afterwards.
    $raw = $null
    try { $raw = git -C $remote show "state/controller:backups.json" 2>$null } catch { $raw = $null }
    if ($raw) {
      $published = ($raw -join "`n" | ConvertFrom-Json)
      if ($published.items.Count -eq 2) { break }
    }
    Start-Sleep -Milliseconds 500
  }
  if (-not $published) { Bad "backups.json never appeared on state/controller:`n$(Get-Content -Raw $log)" }
  $names = @($published.items.name | Sort-Object)
  $want = @($name, $big | Sort-Object)
  if (($names -join ",") -ne ($want -join ",")) {
    Bad "state/controller:backups.json lists $($names -join ', '), want $($want -join ', ')"
  }
  $pe = $published.items | Where-Object { $_.name -eq $big }
  if ($pe.size -ne $b.size -or $pe.sha256 -ne $b.sha) {
    Bad "the published entry for $big says size=$($pe.size) sha=$($pe.sha256), want $($b.size)/$($b.sha)"
  }
  $pn = $published.items | Where-Object { $_.name -eq $name }
  if (-not $pn.downloaded_at) { Bad "the download stamp did not reach the state branch" }
  Ok "state/controller:backups.json is the directory, digests and download stamps included"

  Write-Host "`nGATE PASSED ($pass assertions)" -ForegroundColor Green
  $ok = $true
}
finally {
  # The process first, so the redirected log is closed and complete before it is read.
  if (-not $proc.HasExited) {
    Stop-Process -Id $proc.Id -Force
    $null = $proc.WaitForExit(5000)
  }
  if (-not $ok) {
    Write-Host "`n--- controller log (tail) ---" -ForegroundColor DarkGray
    Get-Content -Tail 60 -ErrorAction SilentlyContinue $log |
      ForEach-Object { Write-Host "  $_" -ForegroundColor DarkGray }
  }
}
