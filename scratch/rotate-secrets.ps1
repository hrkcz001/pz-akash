# scratch/rotate-secrets.ps1 — generate v2's secrets and store them outside git.
#
# This implements the user's cutover decision to rotate rather than reuse. It
# supersedes scratch/set-github-secrets.ps1, which moved v1's values as-is under the
# earlier "reuse everything" decision; those values are all publicly readable in
# hrkcz001/pz-saves (see PLAN step 9 item 5), so v2 starts from new ones.
#
# What is rotated: every password, the webhook secret, and the pz-saves deploy key.
# What is carried over: PZ_AKASH_API_KEY and PZ_CLOUDFLARE_API_TOKEN — third-party
# credentials the user owns, which rotate in Akash's and Cloudflare's UIs, not here.
#
# Where values land: $OutDir, which is OUTSIDE both repositories on purpose. A file
# at the root of pz-akash would sit in the Docker build context; one in scratch/ is
# excluded from the image but still a keystroke away from `git add -A`. Outside the
# tree it can be neither.
#
# Character set: alphanumeric only, for every game and HTTP password. Not for
# entropy reasons — 32 alphanumerics is ~190 bits — but because these values are
# parsed by four things that each choke on different punctuation. `=` ends a key in
# a PZ .ini, `:` separates the halves of an HTTP basic credential, and both a shell
# and a URL have their own opinions. Length buys the entropy back.
#
# No value is printed. The report gives names and lengths, and the file is where the
# values are read from.
$ErrorActionPreference = "Stop"

$OutDir = "C:\Users\hrkcz001\.pz-akash"
$EnvFile = Join-Path $OutDir "secrets.env"
$KeyFile = Join-Path $OutDir "pz-saves-deploy-key"

if (-not (Test-Path $OutDir)) { New-Item -ItemType Directory -Path $OutDir | Out-Null }

# NewPassword returns n alphanumeric characters from the OS CSPRNG.
#
# Rejection sampling, not modulo: mapping 256 byte values onto a 62-character
# alphabet with % leaves the first 8 characters roughly 13% likelier than the rest.
# It costs nothing to draw again instead. Multi-word name so it cannot collide with
# a cmdlet alias — that mistake printed two live passwords into a terminal earlier.
function NewPassword {
    param([int]$Length)
    $alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
    $limit = [int]([Math]::Floor(256 / $alphabet.Length) * $alphabet.Length)
    $chars = [char[]]::new($Length)
    $buffer = [byte[]]::new(1)
    $rng = [System.Security.Cryptography.RandomNumberGenerator]::Create()
    try {
        for ($i = 0; $i -lt $Length) {
            $rng.GetBytes($buffer)
            if ($buffer[0] -lt $limit) {
                $chars[$i] = $alphabet[$buffer[0] % $alphabet.Length]
                $i++
            }
        }
    } finally { $rng.Dispose() }
    return -join $chars
}

function NewHexSecret {
    param([int]$Bytes)
    $buffer = [byte[]]::new($Bytes)
    $rng = [System.Security.Cryptography.RandomNumberGenerator]::Create()
    try { $rng.GetBytes($buffer) } finally { $rng.Dispose() }
    return ([BitConverter]::ToString($buffer) -replace '-').ToLower()
}

# Carried returns a third-party credential this script does not generate.
#
# It used to read them out of v1's rendered manifest, pz-controller/deployment.yaml.
# That file left git at cutover — v1's two workflows are path-filtered to its
# directory, and a first push to a fresh repository counts every file as added, which
# would have published v1's images into v2's registry. It is gitignored, so a copy
# lingers untracked in the working tree, which is exactly why this no longer reads it:
# a value that survives only in an ignored file one `git clean` from deletion is not a
# source of truth. $EnvFile is, and the last run that could still read the manifest put
# both values there.
#
# A hard error rather than a regeneration. An Akash API key cannot be minted from here
# — it comes from the console — and quietly writing a new random string in its place
# would produce a deployment that fails to authenticate with no indication why.
function Carried {
    param([string]$Name)
    if ($existing.ContainsKey($Name) -and -not [string]::IsNullOrWhiteSpace($existing[$Name])) {
        return $existing[$Name]
    }
    throw "$Name is not in $EnvFile and cannot be generated; copy it from the Akash or Cloudflare console"
}

# Values already in the file, so a re-run does not invalidate what is deployed.
$existing = @{}
if (Test-Path $EnvFile) {
    foreach ($line in [IO.File]::ReadAllLines($EnvFile)) {
        if ($line -match '^([A-Z0-9_]+)=(.*)$') { $existing[$Matches[1]] = $Matches[2] }
    }
}

# KeepOrNew is for the one password a human types. The join password is kept across
# a rotation on purpose: every player has it, it lives in a pinned chat message, and
# rotating it locks out everyone who is not reading chat that day. The others are
# typed by nothing but the agent, so rotating them costs no one anything.
#
# Kept by reading the file rather than by a literal in this script: a value written
# here would be a password committed to a public repository, which is the thing v2
# exists to stop. Delete the line from secrets.env to force a new one.
function KeepOrNew {
    param([string]$Name, [int]$Length)
    if ($existing.ContainsKey($Name) -and -not [string]::IsNullOrWhiteSpace($existing[$Name])) {
        return $existing[$Name]
    }
    return NewPassword -Length $Length
}

# --- the pz-saves deploy key ------------------------------------------------
#
# New, not reused. v1's key is in the history of two repositories, one of which was
# public, so it is a published private key; and the new pz-saves is private, which
# only means something if the credential is not.
#
# ed25519 rather than the old RSA: shorter, and every git host has accepted it for
# years. ssh-keygen prompts before overwriting, and stdin here is the null device,
# so an existing file has to go first or the script hangs.
#
# The empty passphrase goes through cmd rather than PowerShell. `-N '""'` from
# PowerShell reaches ssh-keygen as a literal two-character passphrase and produces an
# *encrypted* key that looks fine until something tries to use it — verified by
# `ssh-keygen -y` hanging on the prompt. cmd's `""""` is genuinely empty, and the
# assertion below is what proves it rather than assuming it.
if (Test-Path $KeyFile) { Remove-Item $KeyFile -Force }
if (Test-Path "$KeyFile.pub") { Remove-Item "$KeyFile.pub" -Force }
cmd /c "ssh-keygen -t ed25519 -N """" -C ""pz-akash v2 pz-saves deploy key"" -f ""$KeyFile"" -q" 2>&1 | Out-Null
if (-not (Test-Path $KeyFile)) { throw "ssh-keygen produced no key at $KeyFile" }

# Reading the public key out of the private one succeeds only if no passphrase is
# needed. Stdin is closed so a prompt fails immediately instead of hanging.
cmd /c "ssh-keygen -y -f ""$KeyFile"" < NUL" 2>&1 | Out-Null
if ($LASTEXITCODE -ne 0) { throw "the generated key is passphrase-protected; a container cannot use it" }

$keyPem = [IO.File]::ReadAllBytes($KeyFile)
$keyB64 = [Convert]::ToBase64String($keyPem)

# --- the set ----------------------------------------------------------------
#
# Lengths differ by what has to type the value. The join password is entered by a
# player from a chat message, so it is short enough to be typed and long enough that
# guessing it is pointless. Nothing types the HTTP credentials but the agent.
$map = [ordered]@{
    PZ_DEPLOY_KEY_B64        = $keyB64
    PZ_AKASH_API_KEY         = Carried "PZ_AKASH_API_KEY"
    PZ_WEBHOOK_SECRET        = NewHexSecret -Bytes 32
    PZ_CLOUDFLARE_API_TOKEN  = Carried "PZ_CLOUDFLARE_API_TOKEN"
    PZ_STORAGE_PASSWORD      = NewPassword -Length 32
    PZ_SERVER_FILES_PASSWORD = NewPassword -Length 32
    PZ_BACKUPS_PASSWORD      = NewPassword -Length 32
    PZ_RCON_PASSWORD         = NewPassword -Length 24
    PZ_ADMIN_PASSWORD        = NewPassword -Length 24
    PZ_JOIN_PASSWORD         = KeepOrNew "PZ_JOIN_PASSWORD" 14
}

# v1 used one string for both RCON and storage. Two variables now hold two values,
# which is the point of rotating rather than moving: a leaked dashboard credential
# should not also be a remote console login.
if ($map.PZ_RCON_PASSWORD -ceq $map.PZ_STORAGE_PASSWORD) { throw "generator returned a collision" }

$lines = foreach ($name in $map.Keys) {
    $value = $map[$name]
    if ([string]::IsNullOrWhiteSpace($value)) { throw "$name resolved to an empty value" }
    "$name=$value"
}
# LF and no BOM: this file is read by PowerShell here and by bash under WSL, and a
# BOM lands inside the first variable's name.
[IO.File]::WriteAllText($EnvFile, (($lines -join "`n") + "`n"), (New-Object Text.UTF8Encoding $false))

Write-Output "wrote $EnvFile"
foreach ($name in $map.Keys) {
    $rotated = "rotated"
    if ($name -in 'PZ_AKASH_API_KEY', 'PZ_CLOUDFLARE_API_TOKEN') { $rotated = "carried over from v1" }
    if ($existing.ContainsKey($name) -and $existing[$name] -ceq $map[$name]) { $rotated = "kept" }
    Write-Output ("  {0,-26} {1,4} chars  {2}" -f $name, $map[$name].Length, $rotated)
}
Write-Output ""
Write-Output "public key to install as a write-enabled deploy key on the new pz-saves:"
Write-Output ("  " + (Get-Content "$KeyFile.pub" -Raw).Trim())

