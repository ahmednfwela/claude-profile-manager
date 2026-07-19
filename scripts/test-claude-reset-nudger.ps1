# test-claude-reset-nudger.ps1 — end-to-end DryRun tests for the loop guards.
# Runs the real script against a fixture USERPROFILE and asserts on the log it
# writes. No framework needed:  pwsh -NoProfile -File scripts/test-claude-reset-nudger.ps1
# Exits non-zero on any failure.

$ErrorActionPreference = 'Stop'
$nudger = Join-Path $PSScriptRoot 'claude-reset-nudger.ps1'
$script:failed = 0

function Assert([bool]$cond, [string]$msg) {
    if ($cond) { Write-Host "  ok: $msg" }
    else { Write-Host "  FAIL: $msg" -ForegroundColor Red; $script:failed++ }
}

function New-Fixture {
    $d = Join-Path ([IO.Path]::GetTempPath()) ("nudger-test-" + [guid]::NewGuid().ToString('N').Substring(0, 12))
    New-Item -ItemType Directory -Path (Join-Path $d '.claude') -Force | Out-Null
    New-Item -ItemType Directory -Path (Join-Path $d '.claude-profiles') -Force | Out-Null
    return $d
}

# Writes a profile with a resume flag; optionally a matching bg job record.
function Add-ProfileFixture(
    [string]$root, [string]$profile, [string]$sessionId, [datetime]$flagAtUtc,
    [string]$jobName = $null, [nullable[datetime]]$jobCreatedAtUtc = $null
) {
    $p = Join-Path $root ".claude-profiles\$profile"
    New-Item -ItemType Directory -Path (Join-Path $p '.bdaya-resume') -Force | Out-Null
    @{ error_type = 'unknown'; sessionId = $sessionId; at = $flagAtUtc.ToString('o') } |
        ConvertTo-Json | Set-Content (Join-Path $p '.bdaya-resume\latest.json')
    if ($jobName) {
        $short = $sessionId.Substring(0, 8)
        $jobDir = Join-Path $p "jobs\$short"
        New-Item -ItemType Directory -Path $jobDir -Force | Out-Null
        @{ name = $jobName; createdAt = $jobCreatedAtUtc.ToString('o'); state = 'working' } |
            ConvertTo-Json | Set-Content (Join-Path $jobDir 'state.json')
    }
}

function Invoke-Nudger([string]$root, [string[]]$targetOrder) {
    $saved = $env:USERPROFILE
    try {
        $env:USERPROFILE = $root
        & $nudger -DryRun -TargetOrder $targetOrder *> $null
    } finally {
        $env:USERPROFILE = $saved
    }
    $log = Join-Path $root '.claude\reset-nudger.log'
    if (Test-Path $log) { return (Get-Content $log -Raw) }
    return ''
}

$nowUtc = (Get-Date).ToUniversalTime()

Write-Host "scenario: dead-on-arrival fork pauses the chain"
$fix = New-Fixture
$flagAt = $nowUtc.AddMinutes(-10)
Add-ProfileFixture $fix 'pA' 'abcd1234-0000-4000-8000-000000000001' $flagAt `
    -jobName 'reset-deadbeef' -jobCreatedAtUtc $flagAt.AddSeconds(-16)
$log = Invoke-Nudger $fix @('pA', 'pB')
Assert ($log -match 'reset fork died 16s after spawn \(lineage deadbeef\)') 'DOA detected with lineage root'
Assert ($log -match 'pausing chain for manual /bdaya-resume') 'chain paused, not re-forked'
Assert ($log -notmatch 'DRYRUN would nudge') 'no nudge attempted'

Write-Host "scenario: lineage cap trips across hops (ledger keyed on chain root)"
$fix = New-Fixture
$flagAt = $nowUtc.AddMinutes(-10)
Add-ProfileFixture $fix 'pA' 'bbbb2222-0000-4000-8000-000000000002' $flagAt `
    -jobName 'reset-deadbeef-h2' -jobCreatedAtUtc $flagAt.AddHours(-2)
@(1..3 | ForEach-Object {
    @{ lineage = 'deadbeef'; sessionId = "hop$_"; profile = 'pX'; at = $nowUtc.AddHours(-$_).ToString('o') }
}) | ConvertTo-Json -AsArray | Set-Content (Join-Path $fix '.claude\reset-nudger-lineage.json')
$log = Invoke-Nudger $fix @('pA', 'pB')
Assert ($log -match 'lineage cap reached \(3/24h for root deadbeef\)') 'cap counted against chain root'
Assert ($log -notmatch 'DRYRUN would nudge') 'no nudge attempted past the cap'

Write-Host "scenario: untyped (error_type=unknown) fresh flag blocks the target profile"
$fix = New-Fixture
$flagAt = $nowUtc.AddMinutes(-10)
Add-ProfileFixture $fix 'pA' 'cccc3333-0000-4000-8000-000000000003' $flagAt `
    -jobName 'ordinary-worker' -jobCreatedAtUtc $flagAt.AddHours(-2)
# pB has a fresh flag with error_type=unknown and NO matching bg job — under the
# old error-type regex it would still have been selected as the handoff target.
Add-ProfileFixture $fix 'pB' 'dddd4444-0000-4000-8000-000000000004' $nowUtc.AddMinutes(-5)
$log = Invoke-Nudger $fix @('pB', 'pA')
Assert ($log -match 'cpm handoff cccc3333-0000-4000-8000-000000000003 pA pA as reset-cccc3333') `
    'falls back to source: fresh unknown flag blocks pB'
Assert ($log -match 'lineage cccc3333 hop 1') 'root session starts its own lineage'

Write-Host "scenario: chain hop keeps the root name with a hop counter"
$fix = New-Fixture
$flagAt = $nowUtc.AddMinutes(-10)
Add-ProfileFixture $fix 'pA' 'eeee5555-0000-4000-8000-000000000005' $flagAt `
    -jobName 'reset-deadbeef' -jobCreatedAtUtc $flagAt.AddHours(-2)
@(@{ lineage = 'deadbeef'; sessionId = 'hop1'; profile = 'pX'; at = $nowUtc.AddHours(-1).ToString('o') }) |
    ConvertTo-Json -AsArray | Set-Content (Join-Path $fix '.claude\reset-nudger-lineage.json')
$log = Invoke-Nudger $fix @('pA', 'pB')
Assert ($log -match 'as reset-deadbeef-h2 ') 'hop 2 named reset-<root>-h2'
Assert ($log -match 'lineage deadbeef hop 2') 'attempt attributed to the chain root'

Write-Host ''
if ($script:failed -gt 0) { Write-Host "$($script:failed) assertion(s) FAILED" -ForegroundColor Red; exit 1 }
Write-Host 'all assertions passed'
exit 0
