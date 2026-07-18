# claude-reset-nudger.ps1 — auto-continue background Claude sessions after a
# rate-limit / StopFailure interruption, on the SAME account (pause-until-reset).
#
# How it works: the bdaya-defaults plugin writes <profile>\.bdaya-resume\latest.json
# ({error_type, sessionId, at}) when a turn dies on a StopFailure. This script
# (scheduled every 15 min) picks up FRESH flags whose session is a BACKGROUND
# worker in that profile and re-forks it on the same account via
# `cpm handoff <id> <profile> <profile>` (stop + `claude --bg --resume`).
# Claude's built-in retry backoff rides out any remaining limit window.
#
# Safety rails: only background sessions (never forks your interactive work),
# flags older than MaxFlagAgeMinutes are ignored, each flag `at` is nudged at
# most once, and each session is nudged at most MaxAttempts times per 24h.
#
# Install the scheduled task:  claude-reset-nudger.ps1 -Install
# Preview without acting:      claude-reset-nudger.ps1 -DryRun

param(
    [switch]$DryRun,
    [switch]$Install,
    [int]$IntervalMinutes = 15,
    [int]$MaxFlagAgeMinutes = 120,
    [int]$MaxAttempts = 3,
    # Cross-profile routing order for usage-limit interruptions: first listed
    # profile that (a) isn't the source and (b) isn't itself freshly limited
    # receives the session via `cpm handoff <id> <from> <to>`. Empty = legacy
    # same-account pause-until-reset behavior. All candidates must junction the
    # same projects store (cpm handoff verifies and refuses otherwise).
    [string[]]$TargetOrder = @()
)

$ErrorActionPreference = 'Stop'
$profilesRoot = Join-Path $env:USERPROFILE '.claude-profiles'
$logFile = Join-Path $env:USERPROFILE '.claude\reset-nudger.log'

function Write-Log([string]$msg) {
    $line = "$(Get-Date -Format o) $msg"
    Add-Content -Path $logFile -Value $line
    Write-Host $line
}

# ConvertFrom-Json (PS7) auto-converts ISO-8601 strings to [datetime]; accept both.
# Always return UTC — mixing Kind=Utc values with local Get-Date silently skews
# age math by the UTC offset (observed: 10 min read as 190 min).
function AsDateUtc($v) {
    if ($v -is [datetime]) { return $v.ToUniversalTime() }
    return ([DateTimeOffset]::Parse([string]$v, [Globalization.CultureInfo]::InvariantCulture)).UtcDateTime
}

if ($Install) {
    $targetArg = if ($TargetOrder.Count -gt 0) { " -TargetOrder $($TargetOrder -join ',')" } else { '' }
    $action = New-ScheduledTaskAction -Execute 'pwsh.exe' `
        -Argument "-NoProfile -NonInteractive -WindowStyle Hidden -File `"$PSCommandPath`"$targetArg"
    $trigger = New-ScheduledTaskTrigger -Once -At (Get-Date).AddMinutes(1) `
        -RepetitionInterval (New-TimeSpan -Minutes $IntervalMinutes)
    # S4U: the task runs in a non-interactive session — no console window can
    # ever flash, regardless of pwsh/child spawning ( -WindowStyle Hidden alone
    # still flashes a console under interactive task logons). Registering an
    # S4U principal requires elevation; fall back to Interactive when denied
    # (children stay windowless via cpm's CREATE_NO_WINDOW handling — only
    # pwsh's own brief console flash remains in that mode).
    $desc = 'Auto-continue background Claude Code sessions after rate-limit StopFailures (cross-profile routing via -TargetOrder).'
    $mode = 'S4U non-interactive'
    try {
        $principal = New-ScheduledTaskPrincipal -UserId $env:USERNAME -LogonType S4U -RunLevel Limited
        Register-ScheduledTask -TaskName 'ClaudeResetNudger' -Action $action -Trigger $trigger `
            -Principal $principal -Description $desc -Force -ErrorAction Stop | Out-Null
    } catch {
        $mode = "Interactive fallback (S4U needs an elevated shell: $($_.Exception.Message.Trim()))"
        Register-ScheduledTask -TaskName 'ClaudeResetNudger' -Action $action -Trigger $trigger `
            -Description $desc -Force | Out-Null
    }
    Write-Host "Scheduled task 'ClaudeResetNudger' registered (every $IntervalMinutes min, $mode$targetArg)."
    return
}

# A candidate target profile is "freshly limited" when its own resume-flag is a
# recent usage-limit incident — handing a session to it would just bounce.
function Test-ProfileFreshlyLimited([string]$profileName) {
    $p = Join-Path $profilesRoot "$profileName\.bdaya-resume\latest.json"
    if (-not (Test-Path $p)) { return $false }
    try { $f = Get-Content $p -Raw | ConvertFrom-Json } catch { return $false }
    $age = ((Get-Date).ToUniversalTime() - (AsDateUtc $f.at)).TotalMinutes
    return ($age -lt $MaxFlagAgeMinutes -and "$($f.error_type)" -match 'limit|rate|overload|429|529')
}

# Pick the handoff destination: first TargetOrder profile that exists, is not
# the source, and is not itself freshly limited. Falls back to the source
# profile (legacy same-account pause-until-reset).
function Select-HandoffTarget([string]$source) {
    foreach ($cand in $TargetOrder) {
        if ($cand -eq $source) { continue }
        if (-not (Test-Path (Join-Path $profilesRoot $cand))) { continue }
        if (Test-ProfileFreshlyLimited $cand) { continue }
        return $cand
    }
    return $source
}

foreach ($profileDir in Get-ChildItem $profilesRoot -Directory) {
    $name = $profileDir.Name
    $flagPath = Join-Path $profileDir.FullName '.bdaya-resume\latest.json'
    if (-not (Test-Path $flagPath)) { continue }

    $flag = Get-Content $flagPath -Raw | ConvertFrom-Json
    $flagAtStr = (AsDateUtc $flag.at).ToString('o')   # canonical marker
    $ageMin = ((Get-Date).ToUniversalTime() - (AsDateUtc $flag.at)).TotalMinutes
    if ($ageMin -gt $MaxFlagAgeMinutes) { continue }          # stale incident
    if ($ageMin -lt 2) { continue }                            # still settling

    $statePath = Join-Path $profileDir.FullName '.bdaya-resume\nudger-state.json'
    $state = if (Test-Path $statePath) { Get-Content $statePath -Raw | ConvertFrom-Json } else { $null }
    # compare as normalized UTC dates — ConvertFrom-Json returns the stored ISO
    # string as a [datetime], so a string -eq comparison silently never matches
    if ($state -and $state.lastHandledAt -and ((AsDateUtc $state.lastHandledAt) -eq (AsDateUtc $flag.at))) { continue }   # this flag already nudged

    # attempts cap: max $MaxAttempts nudges per session per 24h
    $attempts = @()
    if ($state -and $state.attempts) {
        $attempts = @($state.attempts | Where-Object {
            $_.sessionId -eq $flag.sessionId -and
            (AsDateUtc $_.at) -gt (Get-Date).ToUniversalTime().AddHours(-24)
        })
    }
    if ($attempts.Count -ge $MaxAttempts) {
        Write-Log "[$name] $($flag.sessionId): attempts cap reached ($($attempts.Count)/24h) — leaving for manual /bdaya-resume"
        continue
    }

    # only background sessions: the session's short id must be a job in this profile
    $short = $flag.sessionId.Substring(0, 8)
    $isBg = Test-Path (Join-Path $profileDir.FullName "jobs\$short")
    if (-not $isBg) {
        Write-Log "[$name] $($flag.sessionId): not a background session here — skipping (interactive sessions are never auto-forked)"
        # mark handled so we don't re-log every cycle
        if (-not $DryRun) {
            @{ lastHandledAt = $flagAtStr; attempts = @($state.attempts | Where-Object { $_ }) } | ConvertTo-Json -Depth 5 | Set-Content $statePath
        }
        continue
    }

    $target = Select-HandoffTarget $name

    if ($DryRun) {
        Write-Log "[$name] DRYRUN would nudge: cpm handoff $($flag.sessionId) $name $target (flag age $([int]$ageMin)m, error_type=$($flag.error_type))"
        continue
    }

    Write-Log "[$name] nudging $($flag.sessionId) -> profile '$target' (error_type=$($flag.error_type), flag age $([int]$ageMin)m)"
    & cpm handoff $flag.sessionId $name $target --name "reset-$short" `
        --prompt 'Your previous turn was interrupted by a usage-limit error. Continue the task from exactly where it left off. Ignore hook housekeeping notices.' 2>&1 |
        ForEach-Object { Write-Log "[$name]   $_" }

    $newAttempts = @($state.attempts | Where-Object { $_ }) + @(@{ sessionId = $flag.sessionId; at = (Get-Date -Format o) })
    @{ lastHandledAt = $flagAtStr; attempts = $newAttempts } | ConvertTo-Json -Depth 5 | Set-Content $statePath
}
