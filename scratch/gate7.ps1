# gate7.ps1 — the step 7 gate: the dashboard renders, and it renders the same
# thing v1 did.
#
# Two halves. The first is machine-checkable and fails the gate: the package's own
# suite, which includes the smoke test that executes both templates for every
# stage, locale and unlock combination, plus the canary assertions that catch a
# {{.Typo}} or an escaping failure. The second is the gallery — every appearance
# written to a file for a person to look at. Feature parity against a page a human
# designed is not a property a test can assert, so the gate produces the evidence
# and says so rather than pretending to have checked.
#
#   pwsh scratch/gate7.ps1              # test + gallery
#   pwsh scratch/gate7.ps1 -Open        # ...and open it in a browser

[CmdletBinding()]
param(
    [switch]$Open,
    [string]$OutDir = "$PSScriptRoot\gallery"
)

$ErrorActionPreference = 'Stop'
$module = Join-Path (Split-Path -Parent $PSScriptRoot) 'pzctl'
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

Push-Location $module
try {
    Step 'gofmt' { $out = gofmt -l .; if ($out) { Write-Host $out; $global:LASTEXITCODE = 1 } else { $global:LASTEXITCODE = 0 } }
    Step 'go vet ./...' { go vet ./... }

    # The whole package, not just the dashboard: the port's other half is the
    # wiring in cmd/pzctl, and a dashboard that renders but is not mounted is not
    # a ported dashboard.
    Step 'go test ./internal/dashboard/ ./cmd/pzctl/ ./internal/httpapi/' {
        go test ./internal/dashboard/ ./cmd/pzctl/ ./internal/httpapi/ -count=1
    }

    # The gallery is regenerated from scratch, so a page that stopped being
    # produced disappears instead of lingering from the last run.
    if (Test-Path $OutDir) { Remove-Item -Recurse -Force $OutDir }
    Step 'gallery' {
        $env:PZ_GALLERY_DIR = $OutDir
        try { go test ./internal/dashboard/ -run TestWriteGallery -count=1 -v }
        finally { Remove-Item Env:PZ_GALLERY_DIR }
    }
}
finally {
    Pop-Location
}

$index = Join-Path $OutDir 'index.html'
if (Test-Path $index) {
    $pages = (Get-ChildItem $OutDir -Filter '*.html').Count - 1
    Write-Host "`n$pages pages in $OutDir" -ForegroundColor Cyan
    Write-Host "  open $index and compare against the running v1 dashboard:"
    Write-Host "  status badge, address, price, player count, three download cards,"
    Write-Host "  the guide, the backups table, and both locales."
    if ($Open) { Start-Process $index }
}

if ($fail -gt 0) {
    Write-Host "`ngate 7: $fail step(s) failed" -ForegroundColor Red
    exit 1
}
Write-Host "`ngate 7: machine checks pass — the visual diff is yours to make" -ForegroundColor Green
