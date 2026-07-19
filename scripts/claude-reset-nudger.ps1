# claude-reset-nudger.ps1 — auto-continue background Claude sessions after a
# rate-limit / StopFailure interruption, on the SAME account (pause-until-reset).
#
# How it works: the bdaya-defaults plugin writes <profile>\.bdaya-resume\latest.json
# ({error_type, sessionId, at}) when a turn dies on a StopFailure. This script
# (scheduled every 15 min) picks up FRESH flags whose session is a BACKGROUND
# worker in that profile and re-forks it on the same account via
# `cpm handoff <id> <profile> <profile>` (stop + `claude --bg --resume`).
# Claude's built-in retry backoff rides out a remaining rate-limit window, but
# NOT a session-limit window — those forks die again on their first call.
#
# Safety rails: only background sessions (never forks your interactive work),
# flags older than MaxFlagAgeMinutes are ignored, each flag `at` is nudged at
# most once, each session is nudged at most MaxAttempts times per 24h, and —
# because every re-fork gets a NEW session id — attempts are ALSO capped per
# chain root ("lineage") in a machine-global ledger; a reset fork whose first
# turn died within DeadOnArrivalSeconds of spawn pauses its chain instead of
# bouncing to the next profile.
#
# Install the scheduled task:  claude-reset-nudger.ps1 -Install
# Preview without acting:      claude-reset-nudger.ps1 -DryRun

param(
    [switch]$DryRun,
    [switch]$Install,
    [int]$IntervalMinutes = 15,
    [int]$MaxFlagAgeMinutes = 120,
    [int]$MaxAttempts = 3,
    # A reset fork whose flag lands this soon after its spawn "died on
    # arrival": the account it landed on is still limited, so re-forking would
    # only bounce the session to the next profile and back. Observed
    # 2026-07-19: session-limited forks died ~16s after spawn, every 15 min,
    # for two hours (digrum<->gmail ping-pong).
    [int]$DeadOnArrivalSeconds = 180,
    # Cross-profile routing order for usage-limit interruptions: first listed
    # profile that (a) isn't the source and (b) isn't itself freshly limited
    # receives the session via `cpm handoff <id> <from> <to>`. Empty = legacy
    # same-account pause-until-reset behavior. All candidates must junction the
    # same projects store (cpm handoff verifies and refuses otherwise).
    [string[]]$TargetOrder = @()
)

# Normalize -TargetOrder: `pwsh -File script.ps1 -TargetOrder digrum,gmail` (the
# scheduled-task launch shape, direct or via the wscript launcher) binds the whole
# comma-list as ONE array element, so profile matching in Select-HandoffTarget
# silently never fires. Split any element on commas so both launch shapes work.
$TargetOrder = @($TargetOrder | ForEach-Object { $_ -split ',' } | Where-Object { $_ })

$ErrorActionPreference = 'Stop'
$profilesRoot = Join-Path $env:USERPROFILE '.claude-profiles'
$logFile = Join-Path $env:USERPROFILE '.claude\reset-nudger.log'
# Machine-global (NOT per-profile): a ping-pong chain alternates profiles, so
# per-profile state can never cap it.
$lineageLedgerPath = Join-Path $env:USERPROFILE '.claude\reset-nudger-lineage.json'

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
    $pwshArgs = "-NoProfile -NonInteractive -File `"$PSCommandPath`"$targetArg"
    $s4uAction = New-ScheduledTaskAction -Execute 'pwsh.exe' -Argument "-WindowStyle Hidden $pwshArgs"
    $hiddenLauncher = Join-Path $PSScriptRoot 'hidden-launch.vbs'
    $fallbackAction = New-ScheduledTaskAction -Execute 'wscript.exe' `
        -Argument "`"$hiddenLauncher`" pwsh.exe $pwshArgs"
    $trigger = New-ScheduledTaskTrigger -Once -At (Get-Date).AddMinutes(1) `
        -RepetitionInterval (New-TimeSpan -Minutes $IntervalMinutes)
    # S4U: the task runs in a non-interactive session — no console window can
    # ever flash, regardless of pwsh/child spawning ( -WindowStyle Hidden alone
    # still flashes a console under interactive task logons). Registering an
    # S4U principal requires elevation; fall back to Interactive when denied.
    # The Interactive fallback routes pwsh through scripts/hidden-launch.vbs, a
    # wscript.exe GUI-subsystem launcher (wscript never allocates a console, so
    # its `.Run cmd, 0, False` hides the child unconditionally) instead of
    # invoking pwsh.exe directly — so the fallback is ALSO zero-flash and needs
    # no elevation (children stay windowless via cpm's CREATE_NO_WINDOW
    # handling either way). hidden-launch.vbs must sit next to this script
    # ($PSScriptRoot) — copy both files together.
    $desc = 'Auto-continue background Claude Code sessions after rate-limit StopFailures (cross-profile routing via -TargetOrder).'
    $mode = 'S4U non-interactive'
    try {
        $principal = New-ScheduledTaskPrincipal -UserId $env:USERNAME -LogonType S4U -RunLevel Limited
        Register-ScheduledTask -TaskName 'ClaudeResetNudger' -Action $s4uAction -Trigger $trigger `
            -Principal $principal -Description $desc -Force -ErrorAction Stop | Out-Null
    } catch {
        $mode = "Interactive fallback, zero-flash via wscript hidden launcher (S4U needs an elevated shell: $($_.Exception.Message.Trim()))"
        Register-ScheduledTask -TaskName 'ClaudeResetNudger' -Action $fallbackAction -Trigger $trigger `
            -Description $desc -Force | Out-Null
    }
    Write-Host "Scheduled task 'ClaudeResetNudger' registered (every $IntervalMinutes min, $mode$targetArg)."
    return
}

# A candidate target profile is "freshly limited" when it carries ANY fresh
# incident flag — handing a session to it would just bounce. The old
# error_type filter ('limit|rate|overload|429|529') let session-limit
# incidents through: they reach the StopFailure hook untyped
# (error_type=unknown), so a limited profile looked healthy and the handoff
# ping-ponged between two limited accounts (proven 2026-07-19).
function Test-ProfileFreshlyLimited([string]$profileName) {
    $p = Join-Path $profilesRoot "$profileName\.bdaya-resume\latest.json"
    if (-not (Test-Path $p)) { return $false }
    try { $f = Get-Content $p -Raw | ConvertFrom-Json } catch { return $false }
    $age = ((Get-Date).ToUniversalTime() - (AsDateUtc $f.at)).TotalMinutes
    return ($age -lt $MaxFlagAgeMinutes)
}

# ---- lineage loop guard -----------------------------------------------------
# A nudge re-fork gets a NEW session id, so the per-session attempts cap
# cannot see a chain: original -> reset fork -> reset fork -> ... Each hop
# writes a fresh flag under its new id and the cap never accumulates
# (observed 2026-07-19: a session-limited job ping-ponged digrum<->gmail every
# 15 min for two hours — 7 hops, 7 session ids, zero cap hits). The guard
# keys attempts on the chain ROOT ("lineage"), which rides the job name:
# reset forks are named "reset-<rootShort>[-h<N>]".

# Read a flagged session's job record (jobs\<short>\state.json) from its own
# profile; $null when absent or unparsable.
function Get-JobRecord([string]$profilePath, [string]$sessionId) {
    $p = Join-Path $profilePath "jobs\$($sessionId.Substring(0, 8))\state.json"
    if (-not (Test-Path $p)) { return $null }
    try { Get-Content $p -Raw | ConvertFrom-Json } catch { $null }
}

# Lineage root short-id: from the job name for chain members, else the session
# is itself a root.
function Get-LineageKey($jobRecord, [string]$sessionId) {
    if ($jobRecord -and "$($jobRecord.name)" -match '^reset-([0-9a-f]{8})') { return $Matches[1] }
    return $sessionId.Substring(0, 8)
}

# A malformed entry (missing/garbage `at`) must be skipped, not thrown on —
# under ErrorActionPreference=Stop an unhandled parse would abort every
# future scheduled run until someone clears the ledger by hand.
function Test-EntryFresh($entry, [int]$windowHours) {
    if (-not ($entry -and $entry.at)) { return $false }
    try { return ((AsDateUtc $entry.at) -gt (Get-Date).ToUniversalTime().AddHours(-$windowHours)) } catch { return $false }
}

function Get-LineageAttempts([string]$lineage) {
    if (-not (Test-Path $lineageLedgerPath)) { return @() }
    try { $ledger = Get-Content $lineageLedgerPath -Raw | ConvertFrom-Json } catch { return @() }
    return @($ledger | Where-Object { $_ -and $_.lineage -eq $lineage -and (Test-EntryFresh $_ 24) })
}

function Add-LineageAttempt([string]$lineage, [string]$sessionId, [string]$profileName) {
    $ledger = @()
    if (Test-Path $lineageLedgerPath) {
        try { $ledger = @(Get-Content $lineageLedgerPath -Raw | ConvertFrom-Json) } catch { $ledger = @() }
    }
    # keep 48h (cap window is 24h; the rest is post-mortem context);
    # malformed entries are dropped rather than thrown on
    $ledger = @($ledger | Where-Object { Test-EntryFresh $_ 48 })
    $ledger += @{ lineage = $lineage; sessionId = $sessionId; profile = $profileName; at = (Get-Date).ToUniversalTime().ToString('o') }
    ConvertTo-Json @($ledger) -Depth 5 -AsArray | Set-Content $lineageLedgerPath
}

# Dead-on-arrival: the flagged session is itself a reset fork whose first turn
# died within seconds of spawn — the account it landed on is still limited,
# and the built-in backoff does NOT ride out session-limit windows, so
# re-forking would only bounce it onward. Pause the chain instead.
function Test-DeadOnArrival($jobRecord, $flagAt) {
    if (-not $jobRecord -or "$($jobRecord.name)" -notmatch '^reset-') { return $false }
    if (-not $jobRecord.createdAt) { return $false }
    $lifeSec = ((AsDateUtc $flagAt) - (AsDateUtc $jobRecord.createdAt)).TotalSeconds
    return ($lifeSec -ge 0 -and $lifeSec -lt $DeadOnArrivalSeconds)
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

    $job = Get-JobRecord $profileDir.FullName $flag.sessionId
    $lineage = Get-LineageKey $job $flag.sessionId

    # Loop guard 1 — dead-on-arrival fork: the previous hop died seconds after
    # spawn, so every routable account is still limited. Pause the chain for
    # manual /bdaya-resume instead of bouncing it to another profile.
    if (Test-DeadOnArrival $job $flag.at) {
        $lifeSec = [int](((AsDateUtc $flag.at) - (AsDateUtc $job.createdAt)).TotalSeconds)
        Write-Log "[$name] $($flag.sessionId): reset fork died ${lifeSec}s after spawn (lineage $lineage) — account still limited, pausing chain for manual /bdaya-resume"
        if (-not $DryRun) {
            @{ lastHandledAt = $flagAtStr; attempts = @($state.attempts | Where-Object { $_ }) } | ConvertTo-Json -Depth 5 | Set-Content $statePath
        }
        continue
    }

    # Loop guard 2 — lineage cap: attempts count against the chain ROOT in the
    # machine-global ledger, so a chain that alternates profiles (fresh session
    # id + fresh flag every hop) still runs out of attempts.
    $lineageAttempts = @(Get-LineageAttempts $lineage)
    if ($lineageAttempts.Count -ge $MaxAttempts) {
        Write-Log "[$name] $($flag.sessionId): lineage cap reached ($($lineageAttempts.Count)/24h for root $lineage) — leaving for manual /bdaya-resume"
        if (-not $DryRun) {
            @{ lastHandledAt = $flagAtStr; attempts = @($state.attempts | Where-Object { $_ }) } | ConvertTo-Json -Depth 5 | Set-Content $statePath
        }
        continue
    }

    $target = Select-HandoffTarget $name

    # Stable lineage naming: every hop keeps the ROOT short-id in its name
    # (hop counter for uniqueness), so the chain stays traceable and the
    # lineage key survives any number of hops across any profiles.
    $hop = $lineageAttempts.Count + 1
    $forkName = if ($hop -le 1) { "reset-$lineage" } else { "reset-$lineage-h$hop" }

    if ($DryRun) {
        Write-Log "[$name] DRYRUN would nudge: cpm handoff $($flag.sessionId) $name $target as $forkName (flag age $([int]$ageMin)m, error_type=$($flag.error_type), lineage $lineage hop $hop)"
        continue
    }

    Write-Log "[$name] nudging $($flag.sessionId) -> profile '$target' as $forkName (error_type=$($flag.error_type), flag age $([int]$ageMin)m, lineage $lineage hop $hop)"
    & cpm handoff $flag.sessionId $name $target --name $forkName `
        --prompt 'Your previous turn was interrupted by a usage-limit error. Continue the task from exactly where it left off. Ignore hook housekeeping notices.' 2>&1 |
        ForEach-Object { Write-Log "[$name]   $_" }

    Add-LineageAttempt $lineage $flag.sessionId $name
    $newAttempts = @($state.attempts | Where-Object { $_ }) + @(@{ sessionId = $flag.sessionId; at = (Get-Date -Format o) })
    @{ lastHandledAt = $flagAtStr; attempts = $newAttempts } | ConvertTo-Json -Depth 5 | Set-Content $statePath
}
