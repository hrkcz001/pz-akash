# gate8.ps1 — the local half of the step 8 gate.
#
# Step 8's real gate is a green run of .github/workflows/images.yml, because the
# only honest way to check what is in an image is to build it. There is no Docker
# daemon on this machine, so this script checks everything that can be checked
# without one:
#
#   * the Go half CI runs — gofmt, vet, the affected packages, config validate;
#   * the facts about the Dockerfile and the workflow that a person reading them
#     would have to hold in their head, asserted instead;
#   * that config.yaml's image names are the names the workflow publishes, which
#     is the one seam between the build and the deployment that nothing else
#     covers: a mismatch here produces a funded lease pulling a tag that does not
#     exist.
#
#   pwsh scratch/gate8.ps1
#
# A green run here means the workflow is worth spending a runner on. It does not
# mean the images are correct — docker/check_image.sh asserts that, in CI.

[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$module = Join-Path $root 'pzctl'
$fail = 0

function Step($name, [scriptblock]$body) {
    Write-Host "`n=== $name" -ForegroundColor Cyan
    & $body
    if ($LASTEXITCODE -ne 0) {
        Write-Host "FAIL: $name (exit $LASTEXITCODE)" -ForegroundColor Red
        $script:fail++
    } else {
        Write-Host "ok: $name" -ForegroundColor Green
    }
}

# Assert is for the static checks: many small facts, each worth naming, none
# worth a subprocess.
function Assert($ok, $name, $detail = '') {
    if ($ok) {
        Write-Host "  ok: $name" -ForegroundColor Green
    } else {
        Write-Host "  FAIL: $name" -ForegroundColor Red
        if ($detail) { Write-Host "        $detail" -ForegroundColor DarkGray }
        $script:fail++
    }
}

# ---- the Go half, exactly what the `check` job runs -------------------------

Push-Location $module
try {
    Step 'gofmt' {
        $out = gofmt -l .
        if ($out) { Write-Host $out; $global:LASTEXITCODE = 1 } else { $global:LASTEXITCODE = 0 }
    }
    Step 'go vet ./...' { go vet ./... }

    # The packages step 8 touched. The full suite with -race is the WSL runner's
    # job (scratch/wsl-test.sh); this is the fast signal.
    Step 'go test (bootstrap, sdl, cmd)' {
        go test ./internal/bootstrap/ ./internal/sdl/ ./cmd/pzctl/ -count=1
    }

    Step 'pzctl config validate' { go run ./cmd/pzctl config validate -c config.yaml }
}
finally {
    Pop-Location
}

# ---- the Dockerfile ---------------------------------------------------------

Write-Host "`n=== docker/Dockerfile" -ForegroundColor Cyan
$dockerfile = Get-Content (Join-Path $root 'docker\Dockerfile') -Raw
# Instructions only. The header comment says at length which packages v2 does not
# install, so a naive search over the whole file finds every name it is asserting
# the absence of.
$dfCode = (Get-Content (Join-Path $root 'docker\Dockerfile') |
    Where-Object { $_ -notmatch '^\s*#' }) -join "`n"

Assert ($dockerfile -match '(?m)^FROM .+ AS controller$') 'a controller target'
Assert ($dockerfile -match '(?m)^FROM .+ AS server$') 'a server target'
Assert ($dockerfile -match '(?m)^CMD \["pzctl", "controller"\]$') 'controller CMD is pzctl controller'
Assert ($dockerfile -match '(?m)^CMD \["pzctl", "agent"\]$') 'server CMD is pzctl agent'
Assert ($dockerfile -notmatch '(?m)^ENTRYPOINT') 'no ENTRYPOINT — bug 2 was v1s entrypoint.sh returning'

# One Go build, static, and it proves itself before being copied anywhere.
Assert ($dockerfile -match 'CGO_ENABLED=0') 'CGO is off (one static binary)'
Assert (([regex]::Matches($dockerfile, 'go build')).Count -eq 1) 'exactly one go build'
Assert ($dockerfile -match '/out/pzctl version') 'the build smoke-tests the binary'

# Neither image runs as root, and the ports are config.yaml's business.
Assert ($dockerfile -match '(?m)^USER pzctl$') 'the controller drops to pzctl'
Assert ($dockerfile -match '(?m)^USER steam$') 'the server drops to steam'
Assert ($dockerfile -notmatch '(?m)^USER root') 'no stage ends as root'
Assert ($dockerfile -notmatch '(?m)^EXPOSE') 'no EXPOSE — the ports come from the SDL'

# PZ resolves its world from HOME and Docker will not do it for us.
Assert ($dockerfile -match '(?m)^ENV HOME=/home/steam$') 'HOME is set explicitly'

# The ssh path bug 4 lived in, gone by construction rather than by intent.
foreach ($pkg in 'openssh-server', 'openssh-client', 'gosu', 'sudo', 'dos2unix') {
    Assert ($dfCode -notmatch [regex]::Escape($pkg)) "no $pkg in any stage"
}

# A secret-shaped build argument would be readable in `docker history` forever.
$secretArg = [regex]::Matches($dockerfile, '(?m)^ARG\s+(\w*(?:PASSWORD|SECRET|TOKEN|API_KEY|DEPLOY_KEY|PRIVATE)\w*)')
$secretArgNames = ($secretArg | ForEach-Object { $_.Groups[1].Value }) -join ', '
Assert ($secretArg.Count -eq 0) 'no secret-shaped ARG' $secretArgNames

# ---- the workflow -----------------------------------------------------------

Write-Host "`n=== .github/workflows/images.yml" -ForegroundColor Cyan
$wf = Get-Content (Join-Path $root '.github\workflows\images.yml') -Raw

# Every script the workflow runs has to exist. This is the class of mistake that
# costs a runner and a round trip to find out about.
$scripts = [regex]::Matches($wf, 'docker/[\w.-]+\.sh') | ForEach-Object { $_.Value } | Sort-Object -Unique
foreach ($s in $scripts) {
    Assert (Test-Path (Join-Path $root ($s -replace '/', '\'))) "$s exists"
}
Assert ($scripts.Count -gt 0) 'the workflow runs the image gate at all'

# No test, no image: both build jobs wait on `check`.
Assert (([regex]::Matches($wf, '(?m)^\s+needs: check$')).Count -eq 2) 'both image jobs need check'
Assert ($wf -match 'go test -race') 'the check job runs -race'
Assert ($wf -match 'config validate -c config\.yaml') 'the check job validates the in-repo config'
Assert ($wf -match 'config validate -c \.\./pz-saves/config\.yaml') 'the controller job validates the shipped config'

# The gate has to be able to fail before anything is published, so the build
# loads and a later step pushes.
Assert (([regex]::Matches($wf, '(?m)^\s+load: true$')).Count -eq 2) 'both builds load into the daemon'
Assert (([regex]::Matches($wf, '(?m)^\s+push: false$')).Count -eq 2) 'neither build pushes'
Assert (([regex]::Matches($wf, 'docker push')).Count -eq 2) 'a separate push step per image'
Assert ($wf -notmatch "push: \$\{\{ github\.event_name") 'push is not an expression on the build step'

# The one secret the build needs, and the token it does not have to be given.
Assert ($wf -match 'secrets\.PZ_SAVES_SSH_KEY') 'pz-saves is checked out with its deploy key'
Assert ($wf -notmatch '(?m)secrets\.AKASH') 'no Akash credentials in the build'
Assert ($wf -notmatch '(?m)secrets\.CLOUDFLARE') 'no Cloudflare credentials in the build'

# ---- the build context ------------------------------------------------------

Write-Host "`n=== .dockerignore" -ForegroundColor Cyan
$di = (Get-Content (Join-Path $root '.dockerignore')) | Where-Object { $_ -and $_ -notmatch '^\s*#' }

# v1's rendered manifests hold the live deploy key, the Akash key and the game
# passwords. The context is uploaded whole; this line is what keeps them out.
Assert ($di -contains '**/deployment.yaml') 'rendered v1 manifests are excluded'
Assert ($di -contains 'scratch/') 'the scratch area is excluded'
Assert ($di -contains 'pz-controller/') 'v1s tree is excluded'
# And the one directory that must NOT be excluded, because the packages stage
# reads it.
Assert ($di -notcontains 'pz-saves/' -and $di -notcontains '/pz-saves/') 'pz-saves is in the context'

# The same directory must be gitignored, though: CI checks it out into the working
# tree, and a stray commit would put the private saves in a public repository.
$gi = Get-Content (Join-Path $root '.gitignore')
Assert ($gi -contains '/pz-saves/') 'pz-saves is gitignored at the root'

# ---- the seam between the build and the deployment --------------------------

Write-Host "`n=== config.yaml against the workflow" -ForegroundColor Cyan
$remote = git -C $root remote get-url origin
if ($remote -match '[:/]([^/:]+)/([^/]+?)(\.git)?$') {
    $slug = "$($Matches[1])/$($Matches[2])"
    Write-Host "  origin: $slug" -ForegroundColor DarkGray
    $cfg = Get-Content (Join-Path $module 'config.yaml') -Raw

    # What the workflow publishes: ghcr.io/<owner>/<repo>-controller and -server.
    foreach ($role in 'controller', 'server') {
        $want = "ghcr.io/$slug-$role"
        Assert ($cfg -match "(?m)^\s+image:\s+$([regex]::Escape($want))\s*$") "config pins $want"
    }

    # And the tag shape metadata-action's type=sha,format=short produces. A tag
    # that is not this shape was hand-written and probably does not exist.
    $tags = [regex]::Matches($cfg, '(?m)^\s+image_tag:\s+(\S+)') | ForEach-Object { $_.Groups[1].Value }
    Assert ($tags.Count -eq 2) 'both images pin a tag' ($tags -join ', ')
    foreach ($t in $tags) {
        Assert ($t -match '^sha-[0-9a-f]{7,}$') "image_tag $t has the shape CI produces"
    }
} else {
    Assert $false 'origin remote parses' $remote
}

# ---- the image gate itself --------------------------------------------------

Write-Host "`n=== docker/check_image.sh" -ForegroundColor Cyan
$checkPath = Join-Path $root 'docker\check_image.sh'
$check = Get-Content $checkPath -Raw
Assert ($check -match '(?m)^set -euo pipefail$') 'the gate fails on the first error'
Assert ($check -match '\$role" = controller') 'it has a controller section'
Assert ($check -match '\$role" = server') 'it has a server section'
Assert ($check -match 'exit 1') 'it exits non-zero when a check fails'
# It prints variable names, never values: this runs in a public build log.
Assert ($check -match "cut -d= -f1") 'secret-shaped variables are reported by name only'

# The workflow invokes it as a program, so the executable bit has to be in the
# index — Windows checkouts do not carry file modes, git does.
$mode = (git -C $root ls-files -s 'docker/check_image.sh') -split '\s+' | Select-Object -First 1
if ($mode) {
    Assert ($mode -eq '100755') 'check_image.sh is executable in the index' "mode $mode; fix with: git update-index --chmod=+x docker/check_image.sh"
} else {
    Write-Host "  note: check_image.sh is untracked; `git add --chmod=+x` it" -ForegroundColor Yellow
}

# bash -n under WSL if there is one. A syntax error in a gate script reads as a
# failing gate, which is the most expensive kind of typo here.
$wsl = Get-Command wsl -ErrorAction SilentlyContinue
if ($wsl) {
    $unix = '/mnt/c' + ($checkPath -replace '^[A-Za-z]:', '' -replace '\\', '/')
    Step 'bash -n docker/check_image.sh' { wsl -d Debian -- bash -n $unix }
} else {
    Write-Host "  note: no wsl; check_image.sh was not syntax-checked" -ForegroundColor Yellow
}

# ---- verdict ----------------------------------------------------------------

if ($fail -gt 0) {
    Write-Host "`ngate 8: $fail check(s) failed" -ForegroundColor Red
    exit 1
}
Write-Host "`ngate 8: local checks pass" -ForegroundColor Green
Write-Host "  no Docker daemon here, so nothing was built. The images are gated by"
Write-Host "  .github/workflows/images.yml; docker/check_image.sh runs there."
