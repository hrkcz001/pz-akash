# scratch/pzlog-triage.ps1 — turn a raw pz-server container log into a triage report.
#
# The log arrives as JSON-lines from the provider's WebSocket ({"name":...,
# "message":...}), 1.1MB of it for one PZ start with ~100 mods, so reading it by eye
# is not an option. This groups it the way the decisions are made: what failed hard,
# what Lua threw, which mods the game could not resolve, and what merely complained.
#
# Redaction is not optional here. The pz-server container prints the ini it just
# wrote and the agent's own narration, and the ini has live credentials substituted
# into it. Every known secret is replaced before a single line reaches the terminal.
param(
    [string]$Path = "",
    [int]$Show = 40
)

$ErrorActionPreference = "Stop"
if (-not $Path) { throw "give -Path to a raw lease log" }

$secrets = @{}
foreach ($line in [IO.File]::ReadAllLines("C:\Users\hrkcz001\.pz-akash\secrets.env")) {
    if ($line -match '^\s*(PZ_[A-Z0-9_]+)=(.*)$') { $secrets[$Matches[1]] = $Matches[2].Trim().Trim('"') }
}
# Longest-first, so a secret containing another is not half-replaced.
$order = @($secrets.Keys | Sort-Object { -$secrets[$_].Length })
function Redact([string]$t) {
    foreach ($n in $order) { $v = $secrets[$n]; if ($v.Length -ge 6) { $t = $t.Replace($v, "<redacted:$n>") } }
    return $t
}

# Each frame is one JSON object; a frame can also be a bare line if the provider
# flushed mid-object, so fall back to the raw text rather than dropping it.
$msgs = New-Object 'System.Collections.Generic.List[string]'
foreach ($l in [IO.File]::ReadAllLines($Path)) {
    if (-not $l.Trim()) { continue }
    if ($l.StartsWith("{")) {
        try { $msgs.Add(($l | ConvertFrom-Json).message); continue } catch { }
    }
    $msgs.Add($l)
}
"total log lines: $($msgs.Count)"

# PZ's own severity prefixes, plus the JVM's.
$buckets = [ordered]@{
    "hard failures (exception / stack trace)" = '^\s*(java\.|at [a-zA-Z0-9_.$]+\()|Exception in thread|java\.lang\.\w+Exception|java\.lang\.\w+Error'
    "ERROR"                                   = '^ERROR\s*:|\bERROR\b'
    "WARN"                                    = '^WARN\s*:'
    "Lua callframe / script errors"           = 'function:.*file:.*line|Callframe at|LuaManager|attempted index|nil value'
    "mod resolution"                          = "Can't find mod|requires mod|missing mod|mod not found|Unknown mod|invalid mod"
}

$seen = New-Object 'System.Collections.Generic.HashSet[int]'
foreach ($b in $buckets.Keys) {
    $rx = [regex]$buckets[$b]
    $hits = New-Object 'System.Collections.Generic.List[string]'
    for ($i = 0; $i -lt $msgs.Count; $i++) {
        if ($seen.Contains($i)) { continue }
        if ($rx.IsMatch($msgs[$i])) { [void]$seen.Add($i); $hits.Add($msgs[$i]) }
    }
    ""
    "=== $b : $($hits.Count) line(s) ==="
    if ($hits.Count -eq 0) { "  (none)"; continue }
    # Collapse repeats: PZ repeats the same complaint once per tick, and 400 copies
    # of one line is one finding, not four hundred.
    $hits | Group-Object { $_ -replace '\d+', '#' } | Sort-Object Count -Descending |
        Select-Object -First $Show | ForEach-Object {
            $t = Redact($_.Group[0].Trim())
            if ($t.Length -gt 300) { $t = $t.Substring(0, 300) + " …" }
            "  [x$($_.Count)] $t"
        }
}
