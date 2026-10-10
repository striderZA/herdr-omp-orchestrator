#requires -Version 5.1
<#
.SYNOPSIS
    Windows end-to-end acceptance for the Herdr-native OMP orchestrator plugin.

.DESCRIPTION
    Drives a REAL Herdr server, REAL OMP agents and the REAL plugin executable
    through the whole documented workflow, and asserts every step against
    authoritative state (Herdr's own API answers and the plugin's SQLite store).

    What it proves, in order:
      1.  CGO_ENABLED=0 build of cmd/herdr-omp-orchestrator.
      2.  `herdr plugin link` keeps the plugin registered with no manifest
          warnings, and `plugin action invoke setup` installs the extension into
          OMP's *effective profile* extension directory, leaving Herdr's own
          integration file byte-identical.
      3.  `bind` on a disposable repository records a primary whose launch/public
          pane, terminal, OMP session and agent name all match `herdr agent get`.
      4.  Two independent worker changes, each in its own Herdr worktree
          workspace with its own OMP session.
      5.  A moved worker pane whose source workspace Herdr closed: reporting,
          identification and cleanup resolution keep working through the current
          public pane and the recorded worktree provenance.
      6.  An ordinary worker question relayed by the primary, and a real
          blocked full-screen dialog approved with specific keys.
      7.  A `partial` report refused without leaving Running, then two complete
          `result` reports with evidence and checks.
      8.  The board renders the tasks. Phase 15 leaves every task fact unchanged and
          presses only navigation keys (never n or m), although the board is opened
          with ticket writes enabled; the ticket write keys belong to 8b.
      8b. Tickets (Phase 16b, after the reviews): through the real primary,
          `orch_ticket_create` records a backlog ticket, a dispatch naming a
          missing ticket is refused, the user's confirmation of the move dialog
          puts the ticket in todo, a dispatch naming it creates a worker whose
          task carries the ticket id, and after that worker's report and review
          the user's confirmation of `orch_ticket_review` records the ticket
          outcome; a worker session's `ticket_move` bridge call is `forbidden`.
      9.  A dirty checkout refuses cleanup; a live worker refuses cleanup.
      10. A timed-out `agent prompt` is treated as ambiguous: the script
          inspects authoritative state before deciding, and never resubmits.
      11. A Herdr server restart preserves identities, inbox, stages and
          worktrees, re-runs the plugin's startup hook, and the restored OMP
          session still has its active role tools.
      12. The session is MATERIALIZED before any resume hint is trusted.
      13. Everything the run created is removed by exact identity, and nothing
          else in the machine was touched.

    SCENARIOS
      Full (default) runs the complete workflow above.
      SingleWorker reuses the same isolated setup and teardown, then exercises
      one real task from primary dispatch through worker result report, board
      projection, and explicit user disposition. Its behavior phases have a
      separate wall-clock budget; setup and safety cleanup remain bounded by
      their own operation timeouts.

    PRE-RUN CLEANUP
      Before starting the new scenario, the script retires earlier acceptance
      runs only when their manifest, project key, exact temp root, session
      identity, dead owner and server process identity are verified.
      After stopping/deleting that exact Herdr session, it kills the recorded
      server PID only if its start time still matches. Version 2 journals also
      require the executable path; legacy journals require an exact PID/start
      match and a Herdr process name. Missing launch records are accepted only
      when a version 2 journal proves the server was never started.
      Preserved and pending-interaction runs are included; live or unverifiable
      owners, mismatched paths, shared state, or unexpected workspaces fail closed.
      The user's default session and OMP session-history files are never deleted.

    ISOLATION AND SAFETY
      * The whole scenario runs inside a DEDICATED named Herdr session
        (`--session orch-acc-<hex>`) whose server this script starts and owns
        (see docs: "Use named test sessions for experiments that need an
        isolated server"). The user's default session is never addressed by any
        command; the script proves that by comparing the default session's
        workspace/pane/agent projection before and after the run.
      * The server is started with HERDR_CONFIG_PATH pointing at a temporary
        config whose [worktrees] directory is inside the run's $env:TEMP root,
        so every checkout Herdr creates for this run stays under that root.
      * The workspace, panes and worktrees the run acts on are only ever the
        ones the run itself created, addressed by the exact IDs Herdr returned.
        Before `bind` the script asserts that the *focused workspace and pane*
        of the isolated session really is the temporary repository.
      * Focus is captured before anything is created and restored in `finally`.
      * Cleanup removes only: the run's named session, the workspaces/panes it
        created, the exact worktree paths it recorded, its temp root, and the
        state rows whose project key equals this run's canonical temp repo path
        (with a before/after proof that no other row changed). The existing
        plugin link, Herdr's OMP integration, the user's branches/worktrees and
        OMP's session files are all left in place.

    EXIT CODES
      0  every assertion PASSED
      1  at least one assertion FAILED
      2  no failure, but at least one item is BLOCKED (could not be driven with
         the supported public APIs) - see the printed evidence
      3  the run could not start (missing tooling / missing Herdr integration)

    USAGE
      powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\tests\acceptance_windows.ps1
      powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\tests\acceptance_windows.ps1 -TimeoutScale 2
      powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\tests\acceptance_windows.ps1 -Scenario SingleWorker
      powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\tests\acceptance_windows.ps1 -Scenario SingleWorker -ScenarioTimeoutSeconds 600
      powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\tests\acceptance_windows.ps1 -KeepResources
      powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\tests\acceptance_windows.ps1 -Model openai-codex/gpt-6-luna
      powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\tests\acceptance_windows.ps1 -Model ''
      powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\tests\acceptance_windows.ps1 -RetireStaleAcceptanceRunId 1ad8af22
#>
[CmdletBinding()]
param(
    # Multiplies every bounded wait. Raise it on a slow machine or when the
    # OMP model (pinned with -Model, or OMP's default) is under load.
    [double]$TimeoutScale = 1.0,
    # The focused path exercises one task from dispatch through report and review.
    [ValidateSet('Full', 'SingleWorker')]
    [string]$Scenario = 'Full',
    # Wall-clock budget for the SingleWorker behavior phases (setup and cleanup
    # remain separately bounded). The run still cleans up after budget exhaustion.
    [ValidateRange(120, 3600)]
    [int]$ScenarioTimeoutSeconds = 600,
    # Leave this run's isolated resources in place for inspection after it exits.
    # The next run automatically retires them after verifying this run's owner
    # process has exited and its manifest still identifies only acceptance resources.
    [switch]$KeepResources,
    # Do everything else, but keep the plugin's state rows (the store keeps the
    # task facts that explain what the run observed).
    [switch]$SkipStateCleanup,
    # The OMP model every session of this run uses, spelled as `omp --model`
    # spells it (provider/id, optionally :thinking). Herdr starts the primary and
    # every worker from the isolated server, so the model is pinned in that
    # server's environment only; the user's own OMP settings are never touched.
    # Empty leaves each session on whatever model OMP is configured to default to.
    [string]$Model = 'openai-codex/gpt-6-luna',
    # Explicitly authorize cleanup of only this verified acceptance run ID;
    # this may close its workspace group and stop its session, never the default.
    [string]$RetireStaleAcceptanceRunId = ''
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
if ($RetireStaleAcceptanceRunId -and $RetireStaleAcceptanceRunId -notmatch '^[0-9a-fA-F]{8}$') {
    throw 'RetireStaleAcceptanceRunId must be one exact eight-character acceptance run ID'
}
$script:OmpModelPattern = '^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._/-]*(:(off|minimal|low|medium|high|xhigh|max|auto))?\z'
if ($Model -and $Model -notmatch $script:OmpModelPattern) {
    throw "Model must be spelled provider/model[:thinking] (for example openai-codex/gpt-6-luna), got '$Model'"
}
$script:OmpModel = $Model

# ---------------------------------------------------------------------------
# run identity and paths
# ---------------------------------------------------------------------------

$script:RunId = ([guid]::NewGuid().ToString('N')).Substring(0, 8)
$script:SessionName = "orch-acc-$($script:RunId)"
$script:ScriptDir = if ($PSScriptRoot) { $PSScriptRoot } elseif ($MyInvocation.MyCommand.Path) { Split-Path -Parent $MyInvocation.MyCommand.Path } else { '' }
$script:RepoRoot = ''
$rootCandidates = New-Object System.Collections.ArrayList
if ($script:ScriptDir) {
    [void]$rootCandidates.Add($script:ScriptDir)
    [void]$rootCandidates.Add((Join-Path $script:ScriptDir '..'))
}
$here = (Get-Location).Path
[void]$rootCandidates.Add($here)
[void]$rootCandidates.Add((Join-Path $here '..'))
[void]$rootCandidates.Add((Join-Path $here 'tests'))
foreach ($candidate in $rootCandidates) {
    if ((Test-Path -LiteralPath (Join-Path $candidate 'herdr-plugin.toml')) -and (Test-Path -LiteralPath (Join-Path $candidate 'go.mod'))) {
        $script:RepoRoot = (Resolve-Path -LiteralPath $candidate).Path
        break
    }
}
if (-not $script:RepoRoot) {
    throw "the plugin root (a directory holding herdr-plugin.toml and go.mod) could not be found from $($rootCandidates -join ', ')"
}
$script:WorkRoot = Join-Path $env:TEMP "orch-accept-$($script:RunId)"
$script:RunArchiveRoot = Join-Path $env:LOCALAPPDATA 'herdr\acceptance-runs'
$script:RunRecordDir = Join-Path $script:RunArchiveRoot $script:RunId
$script:RunManifestPath = Join-Path $script:RunRecordDir 'manifest.json'
$script:ReportDir = $script:RunRecordDir
$script:ReportJson = Join-Path $script:ReportDir 'report.json'
$script:ReportText = Join-Path $script:ReportDir 'report.txt'
$script:RunLockPath = ''
$script:ServerLog = Join-Path $script:WorkRoot 'herdr-server.log'
$script:ServerErr = Join-Path $script:WorkRoot 'herdr-server.err'
$script:ConfigPath = Join-Path $script:WorkRoot 'herdr-config.toml'
$script:OmpModelOverlayPath = Join-Path $script:WorkRoot 'omp-model-overlay.yml'
$script:RepoPath = Join-Path $script:WorkRoot 'repo'
$script:WorktreesRoot = Join-Path $script:WorkRoot 'worktrees'
$script:StateHelper = Join-Path $script:ReportDir 'state_query.py'

function Write-AtomicRunManifest {
    param([Parameter(Mandatory)]$Manifest, [Parameter(Mandatory)][string]$Path)
    $directory = Split-Path -Parent $Path
    [void](New-Item -ItemType Directory -Path $directory -Force)
    $temporary = Join-Path $directory ('.{0}.{1}.{2}.tmp' -f (Split-Path -Leaf $Path), $PID, [guid]::NewGuid().ToString('N'))
    $backup = Join-Path $directory ('.{0}.{1}.{2}.bak' -f (Split-Path -Leaf $Path), $PID, [guid]::NewGuid().ToString('N'))
    $json = ConvertTo-Json -InputObject $Manifest -Depth 12
    $encoding = New-Object System.Text.UTF8Encoding($false)
    try {
        [System.IO.File]::WriteAllText($temporary, $json, $encoding)
        if ([System.IO.File]::Exists($Path)) {
            [System.IO.File]::Replace($temporary, $Path, $backup)
        } else {
            [System.IO.File]::Move($temporary, $Path)
        }
    } finally {
        if ([System.IO.File]::Exists($temporary)) { [System.IO.File]::Delete($temporary) }
        if ([System.IO.File]::Exists($backup)) { [System.IO.File]::Delete($backup) }
    }
}

function Test-RunOwnerAlive {
    param([Parameter(Mandatory)][int]$OwnerPid, [Parameter(Mandatory)][string]$OwnerStartedAt)
    if ($OwnerPid -le 0) { return $false }
    $expected = [DateTime]::MinValue
    if (-not [DateTime]::TryParse($OwnerStartedAt, [ref]$expected)) { return $false }
    try {
        $process = Get-Process -Id $OwnerPid -ErrorAction Stop
        $actual = $process.StartTime.ToUniversalTime()
        return ([math]::Abs(($actual - $expected.ToUniversalTime()).TotalSeconds) -lt 2)
    } catch {
        return $false
    }
}
function Get-AcceptanceProcessDisposition {
    param([Parameter(Mandatory)][int]$ProcessId, [Parameter(Mandatory)][string]$StartedAt)
    $expected = [DateTime]::MinValue
    if ($ProcessId -le 0 -or -not [DateTime]::TryParse($StartedAt, [ref]$expected)) { return 'unverified' }
    $process = $null
    try {
        $process = Get-Process -Id $ProcessId -ErrorAction Stop
    } catch {
        if ($_.CategoryInfo.Category -eq 'ObjectNotFound') { return 'dead' }
        return 'unverified'
    }
    try {
        $actual = $process.StartTime.ToUniversalTime()
        if ([math]::Abs(($actual - $expected.ToUniversalTime()).TotalSeconds) -lt 2) { return 'active' }
        return 'dead' # The PID was reused after the recorded process exited.
    } catch {
        return 'unverified'
    } finally {
        if ($process) { $process.Dispose() }
    }
}

function Stop-VerifiedAcceptanceServerProcess {
    param(
        [Parameter(Mandatory)][string]$RunId,
        [Parameter(Mandatory)]$Manifest,
        [Parameter(Mandatory)][ValidateSet('stopped', 'absent')][string]$SessionDisposition
    )
    if ($RunId -notmatch '^[0-9a-f]{8}$') { throw "invalid acceptance server identity: $RunId" }
    if ((Get-Field $Manifest 'runId') -cne $RunId -or
        (Get-Field $Manifest 'session') -cne "orch-acc-$RunId") {
        throw "refusing to stop a server whose manifest identity does not match $RunId"
    }
    $processId = 0
    if (-not [int]::TryParse([string](Get-Field $Manifest 'serverPid'), [ref]$processId) -or $processId -le 0) {
        $schemaVersion = 0
        [void][int]::TryParse([string](Get-Field $Manifest 'schemaVersion'), [ref]$schemaVersion)
        $attemptedProperty = $Manifest.PSObject.Properties['serverStartAttempted']
        $neverStarted = ($schemaVersion -ge 2 -and $null -ne $attemptedProperty -and
            $attemptedProperty.Value -is [bool] -and -not $attemptedProperty.Value -and
            [string](Get-Field $Manifest 'phase') -in @('initializing', 'preflight', 'temp-root-creating', 'temp-root-created') -and
            [string]::IsNullOrWhiteSpace([string](Get-Field $Manifest 'serverStartedAt')) -and
            [string]::IsNullOrWhiteSpace([string](Get-Field $Manifest 'sessionDir')))
        if ($SessionDisposition -eq 'absent' -and $neverStarted) { return 'not-recorded' }
        throw "refusing to clean ${RunId}: server process identity was never recorded after launch"
    }
    $startedAt = [string](Get-Field $Manifest 'serverStartedAt')
    $disposition = Get-AcceptanceProcessDisposition -ProcessId $processId -StartedAt $startedAt
    if ($disposition -eq 'unverified') { throw "refusing to clean ${RunId}: server PID/start-time identity is unverifiable" }
    if ($disposition -eq 'dead') { return 'already-exited' }
    if ($processId -eq $PID) { throw "refusing to clean ${RunId}: server PID identifies the current cleanup process" }

    $process = $null
    try {
        $process = Get-Process -Id $processId -ErrorAction Stop
        $actual = $process.StartTime.ToUniversalTime()
        $expected = [DateTime]::Parse($startedAt).ToUniversalTime()
        if ([math]::Abs(($actual - $expected).TotalSeconds) -ge 2) { return 'already-exited' }
        $schemaVersion = 0
        [void][int]::TryParse([string](Get-Field $Manifest 'schemaVersion'), [ref]$schemaVersion)
        if ($schemaVersion -ge 2) {
            $expectedExecutable = [string](Get-Field $Manifest 'serverExecutable')
            if ([string]::IsNullOrWhiteSpace($expectedExecutable)) {
                throw "refusing to clean ${RunId}: server executable identity is missing"
            }
            try { $actualExecutable = [string]$process.MainModule.FileName } catch {
                throw "refusing to clean ${RunId}: server executable path cannot be verified"
            }
            if (-not (Test-SamePath -A $actualExecutable -B $expectedExecutable)) {
                throw "refusing to clean ${RunId}: server PID belongs to a different executable"
            }
        } elseif ([string]$process.ProcessName -ine 'herdr') {
            throw "refusing to clean ${RunId}: legacy server PID does not identify a Herdr process"
        }
        if (-not $process.HasExited) { $process.Kill() }
        if (-not $process.WaitForExit(15000)) { throw "verified stale server process $processId did not exit after Kill" }
    } catch {
        $after = Get-AcceptanceProcessDisposition -ProcessId $processId -StartedAt $startedAt
        if ($after -eq 'dead') { return 'already-exited' }
        throw
    } finally {
        if ($process) { $process.Dispose() }
    }
    $after = Get-AcceptanceProcessDisposition -ProcessId $processId -StartedAt $startedAt
    if ($after -ne 'dead') { throw "verified stale server process $processId remains after Kill" }
    return 'killed'
}

function Enter-AcceptanceRunLock {
    param([Parameter(Mandatory)][string]$Path)
    $directory = Split-Path -Parent $Path
    [void](New-Item -ItemType Directory -Path $directory -Force)
    try {
        return [System.IO.File]::Open(
            $Path,
            [System.IO.FileMode]::OpenOrCreate,
            [System.IO.FileAccess]::ReadWrite,
            [System.IO.FileShare]::None
        )
    } catch [System.IO.IOException] {
        $nativeCode = $_.Exception.HResult -band 0xffff
        if ($nativeCode -in @(32, 33)) { return $null }
        throw
    }
}

function Exit-AcceptanceRunLock {
    param([Parameter(Mandatory)][System.IO.FileStream]$Lock)
    $Lock.Dispose()
}

function Get-AcceptanceStateDbPath {
    $stateDir = Join-Path (Join-Path $env:LOCALAPPDATA 'herdr\plugins') $script:PluginId
    try {
        $extensionPath = Join-Path (Get-EffectiveExtensionDir) 'herdr-orchestrator.ts'
        if (Test-Path -LiteralPath $extensionPath -PathType Leaf) {
            $embedded = [string](Get-Field (Get-ExtensionConstants -Path $extensionPath) 'PLUGIN_STATE_DIR')
            if ($embedded) { $stateDir = $embedded }
        }
    } catch { }
    return [System.IO.Path]::GetFullPath((Join-Path $stateDir 'orchestrator.sqlite'))
}

function Get-AcceptanceRunLockPath {
    param([Parameter(Mandatory)][string]$StateDb, [string]$Root = $script:RunArchiveRoot)
    $canonical = [System.IO.Path]::GetFullPath($StateDb).Replace('/', '\').ToUpperInvariant()
    $hash = [System.Security.Cryptography.SHA256]::Create()
    try {
        $key = ([System.BitConverter]::ToString($hash.ComputeHash([System.Text.Encoding]::UTF8.GetBytes($canonical))) -replace '-', '').ToLowerInvariant()
    } finally { $hash.Dispose() }
    return Join-Path (Join-Path $Root 'locks') "$key.lock"
}

function Initialize-AcceptanceRun {
    param(
        [Parameter(Mandatory)][string]$LockPath,
        [Parameter(Mandatory)][string]$RecordDir,
        [Parameter(Mandatory)]$Manifest
    )
    $lock = Enter-AcceptanceRunLock -Path $LockPath
    if (-not $lock) { return $null }
    try {
        Write-AtomicRunManifest -Manifest $Manifest -Path (Join-Path $RecordDir 'manifest.json')
        return $lock
    } catch {
        Exit-AcceptanceRunLock -Lock $lock
        throw
    }
}

function New-AcceptanceRunManifest {
    return [ordered]@{
        schemaVersion = 2
        runId = $script:RunId
        session = $script:SessionName
        projectKey = $script:RepoPath
        workRoot = $script:WorkRoot
        repoPath = $script:RepoPath
        worktreesRoot = $script:WorktreesRoot
        configPath = $script:ConfigPath
        stateDb = $script:RunLockStateDb
        ownerPid = $PID
        ownerStartedAt = $script:OwnerProcessStartedAt
        serverPid = 0
        serverStartedAt = ''
        serverExecutable = [string]$script:HerdrExe
        serverStartAttempted = [bool]$script:ServerStartAttempted
        sessionDir = ''
        phase = 'initializing'
        status = 'active'
        pendingUserInteraction = [bool]$script:PendingUserInteraction
        resourcesPreserved = [bool]$script:KeepResources
        keepResourcesRequested = [bool]$script:KeepResourcesRequested
        tempRootCreated = [bool]$script:TempRootCreated
        createdWorkspaces = @()
        createdPanes = @()
        startedAt = $script:StartedAt
        updatedAt = [DateTime]::UtcNow.ToString('o')
        finishedAt = ''
    }
}

function Save-AcceptanceRunManifest {
    param([Parameter(Mandatory)][string]$Phase, [string]$Status = 'active')
    if (-not $script:RunManifest) { $script:RunManifest = New-AcceptanceRunManifest }
    $script:LifecyclePhase = $Phase
    $script:RunManifest.runId = $script:RunId
    $script:RunManifest.session = $script:SessionName
    $script:RunManifest.projectKey = $script:RepoPath
    $script:RunManifest.workRoot = $script:WorkRoot
    $script:RunManifest.repoPath = $script:RepoPath
    $script:RunManifest.worktreesRoot = $script:WorktreesRoot
    $script:RunManifest.configPath = $script:ConfigPath
    $script:RunManifest.stateDb = $script:RunLockStateDb
    $script:RunManifest.ownerPid = $PID
    $script:RunManifest.ownerStartedAt = $script:OwnerProcessStartedAt
    $script:RunManifest.serverExecutable = [string]$script:HerdrExe
    $script:RunManifest.serverStartAttempted = [bool]$script:ServerStartAttempted
    if ($script:ServerProcess) {
        $script:RunManifest.serverPid = $script:ServerProcess.Id
        try { $script:RunManifest.serverStartedAt = $script:ServerProcess.StartTime.ToUniversalTime().ToString('o') } catch { }
    }
    $script:RunManifest.sessionDir = [string]$script:SessionDir
    $script:RunManifest.phase = $Phase
    $script:RunManifest.status = $Status
    $script:RunManifest.pendingUserInteraction = [bool]$script:PendingUserInteraction
    $script:RunManifest.resourcesPreserved = [bool]$script:KeepResources
    $script:RunManifest.keepResourcesRequested = [bool]$script:KeepResourcesRequested
    $script:RunManifest.tempRootCreated = [bool]$script:TempRootCreated
    $script:RunManifest.createdWorkspaces = @($script:CreatedWorkspaces)
    $script:RunManifest.createdPanes = @($script:CreatedPanes)
    $script:RunManifest.startedAt = $script:StartedAt
    $script:RunManifest.updatedAt = [DateTime]::UtcNow.ToString('o')
    $script:RunManifest.finishedAt = if ($Status -in @('complete', 'preserved', 'retired')) { [DateTime]::UtcNow.ToString('o') } else { '' }
    Write-AtomicRunManifest -Manifest $script:RunManifest -Path $script:RunManifestPath
}

function Set-PendingUserInteraction {
    param([Parameter(Mandatory)][bool]$Pending, [Parameter(Mandatory)][string]$Phase)
    $script:PendingUserInteraction = $Pending
    $status = if ($Pending) { 'awaiting_user' } else { 'active' }
    Save-AcceptanceRunManifest -Phase $Phase -Status $status
}

function Prune-RunReportArchive {
    param(
        [Parameter(Mandatory)][string]$Root,
        [ValidateRange(1, 1000)][int]$KeepCount = 20,
        [ValidateRange(1, 3650)][int]$RetentionDays = 30,
        [DateTime]$Now = [DateTime]::UtcNow
    )
    if (-not (Test-Path -LiteralPath $Root -PathType Container)) { return }
    foreach ($directory in @(Get-ChildItem -LiteralPath $Root -Directory -Force)) {
        $supportScript = Join-Path $directory.FullName 'state_query.py'
        $manifestPath = Join-Path $directory.FullName 'manifest.json'
        if (-not (Test-Path -LiteralPath $supportScript -PathType Leaf) -or
            -not (Test-Path -LiteralPath $manifestPath -PathType Leaf)) { continue }
        try { $manifest = Get-Content -LiteralPath $manifestPath -Raw | ConvertFrom-Json } catch { continue }
        if ((Get-Field $manifest 'status') -cne 'complete' -or
            (Test-JsonBool -Object $manifest -Name 'pendingUserInteraction') -ne $false -or
            (Test-JsonBool -Object $manifest -Name 'resourcesPreserved') -ne $false -or
            (Test-JsonBool -Object $manifest -Name 'keepResourcesRequested') -ne $false) { continue }
        $ownerPid = 0
        $ownerStartedAt = [string](Get-Field $manifest 'ownerStartedAt')
        if (-not [int]::TryParse([string](Get-Field $manifest 'ownerPid'), [ref]$ownerPid) -or
            $ownerPid -le 0 -or [string]::IsNullOrWhiteSpace($ownerStartedAt) -or
            (Test-RunOwnerAlive -OwnerPid $ownerPid -OwnerStartedAt $ownerStartedAt)) { continue }
        Remove-Item -LiteralPath $supportScript -Force -ErrorAction Stop
    }
    $cutoff = $Now.ToUniversalTime().AddDays(-$RetentionDays)
    $completed = New-Object System.Collections.ArrayList
    foreach ($directory in @(Get-ChildItem -LiteralPath $Root -Directory -Force)) {
        $manifestPath = Join-Path $directory.FullName 'manifest.json'
        if (-not (Test-Path -LiteralPath $manifestPath -PathType Leaf)) { continue }
        try { $manifest = Get-Content -LiteralPath $manifestPath -Raw | ConvertFrom-Json } catch { continue }
        if ((Get-Field $manifest 'status') -cne 'complete' -or
            (Test-JsonBool -Object $manifest -Name 'pendingUserInteraction') -ne $false -or
            (Test-JsonBool -Object $manifest -Name 'resourcesPreserved') -ne $false -or
            (Test-JsonBool -Object $manifest -Name 'keepResourcesRequested') -ne $false) { continue }
        $finishedAt = [DateTime]::MinValue
        if (-not [DateTime]::TryParse([string](Get-Field $manifest 'finishedAt'), [ref]$finishedAt)) { continue }
        [void]$completed.Add([pscustomobject]@{ Path = $directory.FullName; FinishedAt = $finishedAt.ToUniversalTime() })
    }
    $ordered = @($completed | Sort-Object FinishedAt -Descending)
    for ($index = 0; $index -lt $ordered.Count; $index++) {
        if ($index -lt $KeepCount -and $ordered[$index].FinishedAt -ge $cutoff) { continue }
        Remove-Item -LiteralPath $ordered[$index].Path -Recurse -Force -ErrorAction Stop
    }
}

$script:PluginId = 'user.omp-orchestrator'
# Every Herdr CLI/bridge child this script spawns starts from a clean Herdr
# caller context: the script's own process may itself run inside a Herdr pane of
# the user's default session, and that context must never leak into a call
# scoped to this run's session.
$script:HerdrCallerEnvKeys = @(
    'HERDR_ENV', 'HERDR_PANE_ID', 'HERDR_WORKSPACE_ID', 'HERDR_TAB_ID',
    'HERDR_PLUGIN_ID', 'HERDR_PLUGIN_ROOT', 'HERDR_PLUGIN_STATE_DIR',
    'HERDR_PLUGIN_CONFIG_DIR', 'HERDR_PLUGIN_CONTEXT_JSON',
    'HERDR_PLUGIN_ACTION_ID', 'HERDR_PLUGIN_EVENT', 'HERDR_PLUGIN_EVENT_JSON',
    'HERDR_PLUGIN_ENTRYPOINT_ID', 'HERDR_BIN_PATH', 'HERDR_SOCKET_PATH',
    'HERDR_ORCH_HERDR_BIN_PATH', 'HERDR_CONFIG_PATH'
)
$script:MarkerPrefix = 'ACCEPT-'          # every fact this run injects is greppable
# The tools a read-only coordinator session keeps (omp/herdr-orchestrator.ts): the
# primary and a planner may call only OMP's read-only tools plus their own role
# tools. Spelled out here so a changed extension list fails the run instead of
# silently widening what it accepts.
$script:ReadOnlyToolNames = @('read', 'grep', 'find', 'glob')
$script:PrimaryToolNames = @(
    'orch_dispatch', 'orch_answer', 'orch_approve_dialog', 'orch_review', 'orch_settle_input',
    'orch_adjudicate_dispatch', 'orch_cleanup', 'orch_ticket_create', 'orch_ticket_list',
    'orch_ticket_move', 'orch_ticket_review', 'orch_task_list', 'orch_inbox_list', 'orch_repoint'
)
$script:WorkerToolNames = @('orch_report')
$script:Assertions = New-Object System.Collections.ArrayList
$script:Failures = 0
$script:Blocked = 0
$script:Passed = 0
$script:ServerProcess = $null
$script:RunLockStream = $null
$script:RunLockStateDb = ''
$script:RunManifest = $null
$script:LifecyclePhase = 'initializing'
$script:PendingUserInteraction = $false
$script:OwnerProcessStartedAt = (Get-Process -Id $PID).StartTime.ToUniversalTime().ToString('o')
$script:SessionDir = $null
    $script:ServerStartedByScript = $false
    $script:ServerStartAttempted = $false
$script:HerdrExe = $null
$script:TempRootCreated = $false
$script:CreatedWorkspaces = New-Object System.Collections.ArrayList
$script:CreatedPanes = New-Object System.Collections.ArrayList
$script:InitialFocus = $null
$script:DefaultSessionBefore = $null
$script:DefaultSessionRecord = $null
$script:SessionSocket = ''
$script:PluginRegistrationBefore = $null
$script:IntegrationFileHashBefore = $null
$script:ExtensionFileHashBefore = $null
$script:ExtensionPath = $null
$script:StateDir = $null
$script:SessionServerVersion = $null
$script:Project = $null
$script:Tasks = @()
$script:PrimaryPane = $null
$script:ResumeHintAtBind = $null
$script:ResumeHintAfterTurn = $null
$script:PrimarySendKeysUsed = @()
$script:Undriven = New-Object System.Collections.ArrayList
$script:KeepResourcesRequested = [bool]$KeepResources
$script:RetireStaleAcceptanceRunId = ([string]$RetireStaleAcceptanceRunId).ToLowerInvariant()
# Values assigned by the phases; declared here because the script runs under
# Set-StrictMode -Version Latest.
$script:Python = $null
$script:StateDb = $null
$script:GoExe = $null
$script:BaseRevision = ''
$script:PrimaryWorkspace = ''
$script:PrimaryTab = ''
$script:TaskA = $null
$script:TaskB = $null
$script:BoardPane = ''
$script:MaterializedSession = ''
$script:RestoredPrimaryPane = ''
$script:SessionCreated = $false
$script:InitialWorkspaceCount = 0
$script:StartedAt = ''
$script:Aborted = $false
$script:MovedCleanupRefused = $null
$script:ExtensionFileHashAfter = ''
$script:BriefA = ''
$script:BriefB = ''
$script:ScenarioDeadline = $null

function ConvertTo-ScaledSeconds {
    param([double]$Seconds)
    return [math]::Max(1.0, $Seconds * $TimeoutScale)
}

# ---------------------------------------------------------------------------
# redaction
# ---------------------------------------------------------------------------

function Protect-Text {
    param([string]$Text, [int]$Limit = 2000)
    if ($null -eq $Text) { return '' }
    $value = [string]$Text
    # Secret-shaped literals.
    $value = [regex]::Replace($value, '(?i)\b(sk-[A-Za-z0-9_\-]{10,})', '$$REDACTED$$')
    $value = [regex]::Replace($value, '(?i)\b(gh[pousr]_[A-Za-z0-9]{16,})', '$$REDACTED$$')
    $value = [regex]::Replace($value, '(?i)\b(xox[baprs]-[A-Za-z0-9\-]{10,})', '$$REDACTED$$')
    $value = [regex]::Replace($value, '(?i)(bearer\s+)[A-Za-z0-9\.\-_=]{12,}', '$1$$REDACTED$$')
    $value = [regex]::Replace($value, '(?i)((?:api|secret|access|private|client)[_-]?key"\s*[:=]\s*")[^"]{8,}"', '$1$$REDACTED$$"')
    $value = [regex]::Replace($value, '(?i)((?:password|passwd|token|secret)\s*[:=]\s*)\S{8,}', '$1$$REDACTED$$')
    # Any environment value of the current process whose name looks secret.
    foreach ($name in @($env:Path, $env:PATH)) { }
    foreach ($entry in [System.Environment]::GetEnvironmentVariables().GetEnumerator()) {
        $key = [string]$entry.Key
        if ($key -notmatch '(?i)(key|token|secret|password|passwd|credential|auth)') { continue }
        $val = [string]$entry.Value
        if ($val.Length -ge 8) { $value = $value.Replace($val, '$$REDACTED$$') }
    }
    if ($value.Length -gt $Limit) {
        $value = $value.Substring(0, $Limit) + "...[truncated $($value.Length - $Limit) chars]"
    }
    return $value
}

# ---------------------------------------------------------------------------
# native command execution (argv arrays, no shell, bounded, with timeout)
# ---------------------------------------------------------------------------

function ConvertTo-ArgumentLiteral {
    # MSVCRT argv quoting: the single algorithm CreateProcess hands to the child.
    param([string]$Value)
    if ($null -eq $Value) { return '""' }
    if ($Value -eq '') { return '""' }
    if ($Value -notmatch '[\s"]') { return $Value }
    $sb = New-Object System.Text.StringBuilder
    [void]$sb.Append('"')
    $backslashes = 0
    foreach ($ch in $Value.ToCharArray()) {
        if ($ch -eq '\') { $backslashes++; continue }
        if ($ch -eq '"') {
            [void]$sb.Append(('\' * ($backslashes * 2 + 1)))
            [void]$sb.Append('"')
            $backslashes = 0
            continue
        }
        if ($backslashes -gt 0) { [void]$sb.Append(('\' * $backslashes)); $backslashes = 0 }
        [void]$sb.Append($ch)
    }
    if ($backslashes -gt 0) { [void]$sb.Append(('\' * ($backslashes * 2))) }
    [void]$sb.Append('"')
    return $sb.ToString()
}

function Invoke-Native {
    <#
        Runs one executable with an explicit argv array. Returns an object with
        ExitCode, Stdout, Stderr, TimedOut and the command line used. Never
        interpolates anything into a shell.
    #>
    param(
        [Parameter(Mandatory)][string]$File,
        [string[]]$Arguments = @(),
        [hashtable]$Environment = @{},
        [string[]]$RemoveEnvironment = @(),
        [string]$WorkingDirectory = $null,
        [double]$TimeoutSeconds = 120,
        [string]$StandardInputText = $null,
        [switch]$AllowNonZero
    )
    $TimeoutSeconds = Get-ScenarioBoundedTimeoutSeconds -Requested $TimeoutSeconds
    $psi = New-Object System.Diagnostics.ProcessStartInfo
    $psi.FileName = $File
    $commandLine = (ConvertTo-ArgumentLiteral $File)
    foreach ($arg in $Arguments) { $commandLine += ' ' + (ConvertTo-ArgumentLiteral $arg) }
    $psi.Arguments = ($Arguments | ForEach-Object { ConvertTo-ArgumentLiteral $_ }) -join ' '
    $psi.UseShellExecute = $false
    $psi.RedirectStandardInput = $true
    $psi.RedirectStandardOutput = $true
    $psi.RedirectStandardError = $true
    $psi.CreateNoWindow = $true
    $psi.StandardOutputEncoding = [System.Text.Encoding]::UTF8
    $psi.StandardErrorEncoding = [System.Text.Encoding]::UTF8
    if ($WorkingDirectory) { $psi.WorkingDirectory = $WorkingDirectory }
    foreach ($key in $RemoveEnvironment) {
        if ($psi.EnvironmentVariables.ContainsKey($key)) { [void]$psi.EnvironmentVariables.Remove($key) }
    }
    foreach ($key in $Environment.Keys) {
        $psi.EnvironmentVariables[[string]$key] = [string]$Environment[$key]
    }
    $proc = New-Object System.Diagnostics.Process
    $proc.StartInfo = $psi
    $stdout = ''
    $stderr = ''
    try {
        [void]$proc.Start()
        $outTask = $proc.StandardOutput.ReadToEndAsync()
        $errTask = $proc.StandardError.ReadToEndAsync()
        if ($null -ne $StandardInputText) {
            $proc.StandardInput.Write($StandardInputText)
        }
        $proc.StandardInput.Close()
        $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
        $timedOut = $false
        while (-not $proc.WaitForExit(250)) {
            if ((Get-Date) -gt $deadline) {
                $timedOut = $true
                try { $proc.Kill() } catch { }
                break
            }
        }
        if (-not $timedOut) { $proc.WaitForExit() }
        try { $stdout = $outTask.GetAwaiter().GetResult() } catch { $stdout = '' }
        try { $stderr = $errTask.GetAwaiter().GetResult() } catch { $stderr = '' }
        $exit = if ($timedOut) { -1 } else { $proc.ExitCode }
    } finally {
        $proc.Dispose()
    }
    return [pscustomobject]@{
        ExitCode    = $exit
        Stdout      = $stdout
        Stderr      = $stderr
        TimedOut    = $timedOut
        CommandLine = Protect-Text $commandLine 500
    }
}

function Invoke-HerdrForSession {
    param(
        [Parameter(Mandatory)][string]$SessionName,
        [Parameter(Mandatory)][string[]]$Arguments,
        [hashtable]$ExtraEnvironment = @{},
        [double]$TimeoutSeconds = 120
    )
    $argv = @('--session', $SessionName) + $Arguments
    $env = @{ 'NO_COLOR' = '1' }
    foreach ($k in $ExtraEnvironment.Keys) { $env[$k] = $ExtraEnvironment[$k] }
    return Invoke-Native -File $script:HerdrExe -Arguments $argv -Environment $env `
        -RemoveEnvironment $script:HerdrCallerEnvKeys `
        -WorkingDirectory $script:RepoRoot -TimeoutSeconds $TimeoutSeconds
}

function Get-HerdrResultForSession {
    param(
        [Parameter(Mandatory)][string]$SessionName,
        [Parameter(Mandatory)][string[]]$Arguments,
        [double]$TimeoutSeconds = 120
    )
    $response = Invoke-HerdrForSession -SessionName $SessionName -Arguments $Arguments -TimeoutSeconds $TimeoutSeconds
    if ($response.ExitCode -ne 0) {
        throw "herdr --session $SessionName $($Arguments -join ' ') failed: $(Protect-Text $response.Stderr 400)"
    }
    try { $parsed = $response.Stdout | ConvertFrom-Json } catch {
        throw "herdr --session $SessionName $($Arguments -join ' ') did not return JSON"
    }
    if ($null -eq $parsed -or $null -eq $parsed.PSObject.Properties['result']) {
        throw "herdr --session $SessionName $($Arguments -join ' ') returned no result"
    }
    return $parsed.result
}

function Invoke-Herdr {
    <#
        Runs the Herdr CLI against THIS RUN'S isolated session. Every command
        goes through here, so no call can accidentally address the default
        session or another client's focus.
    #>
    param(
        [Parameter(Mandatory)][string[]]$Arguments,
        [hashtable]$ExtraEnvironment = @{},
        [double]$TimeoutSeconds = 120,
        [switch]$NoSessionScope
    )
    if (-not $NoSessionScope) {
        return Invoke-HerdrForSession -SessionName $script:SessionName -Arguments $Arguments `
            -ExtraEnvironment $ExtraEnvironment -TimeoutSeconds $TimeoutSeconds
    }
    $env = @{ 'NO_COLOR' = '1' }
    foreach ($k in $ExtraEnvironment.Keys) { $env[$k] = $ExtraEnvironment[$k] }
    return Invoke-Native -File $script:HerdrExe -Arguments $Arguments -Environment $env `
        -RemoveEnvironment $script:HerdrCallerEnvKeys `
        -WorkingDirectory $script:RepoRoot -TimeoutSeconds $TimeoutSeconds
}

function Get-HerdrJson {
    param(
        [Parameter(Mandatory)][string[]]$Arguments,
        [hashtable]$ExtraEnvironment = @{},
        [double]$TimeoutSeconds = 120,
        [string]$AssertId = '',
        [switch]$NoSessionScope
    )
    $result = Invoke-Herdr -Arguments $Arguments -ExtraEnvironment $ExtraEnvironment `
        -TimeoutSeconds $TimeoutSeconds -NoSessionScope:$NoSessionScope
    if ($result.ExitCode -ne 0 -and -not $result.Stdout) {
        Report-CommandFailure -AssertId $AssertId -Arguments $Arguments -Result $result
        throw "herdr $($Arguments -join ' ') failed with exit code $($result.ExitCode): $(Protect-Text $result.Stderr 400)"
    }
    try {
        $parsed = $result.Stdout | ConvertFrom-Json
    } catch {
        Report-CommandFailure -AssertId $AssertId -Arguments $Arguments -Result $result
        throw "herdr $($Arguments -join ' ') did not answer JSON: $(Protect-Text $result.Stdout 400)"
    }
    return [pscustomobject]@{ Result = $parsed.result; Raw = $parsed; Invocation = $result }
}

function Get-Sessions {
    <#
        `session list --json` answers its own shape ({sessions:[...]}), not the
        standard {id,result} envelope every API command uses.
    #>
    $result = Invoke-Herdr -Arguments @('session', 'list', '--json') -NoSessionScope -TimeoutSeconds 30
    if ($result.ExitCode -ne 0) { throw "herdr session list failed: $(Protect-Text $result.Stderr 300)" }
    try { $parsed = $result.Stdout | ConvertFrom-Json } catch { throw 'herdr session list did not return JSON' }
    if ($null -eq $parsed -or $null -eq $parsed.PSObject.Properties['sessions']) {
        throw 'herdr session list did not contain sessions'
    }
    return @($parsed.sessions)
}

function Report-CommandFailure {
    param([string]$AssertId, [string[]]$Arguments, $Result)
    if (-not $Result) { return }
    $line = "[command] $($Result.CommandLine)"
    Write-Host $line -ForegroundColor DarkGray
    if ($Result.Stdout) { Write-Host ("  stdout: " + (Protect-Text $Result.Stdout)) -ForegroundColor DarkGray }
    if ($Result.Stderr) { Write-Host ("  stderr: " + (Protect-Text $Result.Stderr)) -ForegroundColor DarkGray }
    if ($AssertId) { Add-Content -Path $script:ReportText -Value "$line`n  stdout: $(Protect-Text $Result.Stdout)`n  stderr: $(Protect-Text $Result.Stderr)" }
}

# ---------------------------------------------------------------------------
# assertions
# ---------------------------------------------------------------------------

function Add-Assertion {
    param(
        [Parameter(Mandatory)][string]$Id,
        [Parameter(Mandatory)][string]$What,
        [Parameter(Mandatory)][ValidateSet('PASS', 'FAIL', 'BLOCKED', 'INFO')][string]$Status,
        [string]$TaskId = '',
        [string]$Command = '',
        [string]$Evidence = ''
    )
    $record = [pscustomobject]@{
        id       = $Id
        what     = $What
        status   = $Status
        task     = $TaskId
        command  = Protect-Text $Command 1000
        evidence = Protect-Text $Evidence 2000
        at       = (Get-Date).ToUniversalTime().ToString('o')
    }
    [void]$script:Assertions.Add($record)
    try {
        Add-Content -LiteralPath $script:ReportText -Value ("{0,-7} {1,-6} {2} {3}" -f $record.status, $record.id, $record.task, $record.what) -ErrorAction SilentlyContinue
    } catch { }
    switch ($Status) {
        'PASS' { $script:Passed++; Write-Host ("[PASS] {0} {1}" -f $Id, $What) -ForegroundColor Green }
        'INFO' { Write-Host ("[INFO] {0} {1}" -f $Id, $What) -ForegroundColor Cyan }
        'BLOCKED' {
            $script:Blocked++
            Write-Host ("[BLOCKED] {0} {1}" -f $Id, $What) -ForegroundColor Yellow
            if ($Evidence) { Write-Host ("         evidence: {0}" -f (Protect-Text $Evidence 600)) -ForegroundColor Yellow }
        }
        'FAIL' {
            $script:Failures++
            Write-Host ("[FAIL] {0} {1}" -f $Id, $What) -ForegroundColor Red
            if ($TaskId) { Write-Host ("         task: {0}" -f $TaskId) -ForegroundColor Red }
            if ($Command) { Write-Host ("         command: {0}" -f (Protect-Text $Command 600)) -ForegroundColor Red }
            if ($Evidence) { Write-Host ("         evidence: {0}" -f (Protect-Text $Evidence 600)) -ForegroundColor Red }
        }
    }
    return $record
}

function Assert-True {
    param(
        [Parameter(Mandatory)][string]$Id,
        [Parameter(Mandatory)][string]$What,
        [Parameter(Mandatory)][bool]$Condition,
        [string]$TaskId = '',
        [string]$Command = '',
        [string]$Evidence = ''
    )
    $status = if ($Condition) { 'PASS' } else { 'FAIL' }
    return Add-Assertion -Id $Id -What $What -Status $status -TaskId $TaskId -Command $Command -Evidence $Evidence
}

function Assert-Equal {
    param(
        [Parameter(Mandatory)][string]$Id,
        [Parameter(Mandatory)][string]$What,
        $Expected,
        $Actual,
        [string]$TaskId = '',
        [string]$Command = '',
        [string]$Evidence = '',
        [switch]$CaseInsensitive
    )
    $e = if ($null -eq $Expected) { '' } else { [string]$Expected }
    $a = if ($null -eq $Actual) { '' } else { [string]$Actual }
    $ok = if ($CaseInsensitive) { $e -ieq $a } else { $e -ceq $a }
    $detail = "expected=[$e] actual=[$a]"
    if ($Evidence) { $detail = $detail + ' ' + $Evidence }
    return Assert-True -Id $Id -What $What -Condition $ok -TaskId $TaskId -Command $Command -Evidence $detail
}

function Assert-Match {
    param(
        [Parameter(Mandatory)][string]$Id,
        [Parameter(Mandatory)][string]$What,
        [string]$Pattern,
        [string]$Actual,
        [string]$TaskId = '',
        [string]$Command = '',
        [string]$Evidence = ''
    )
    $ok = ($Actual -match $Pattern)
    $detail = "pattern=[$Pattern] actual=[$Actual]"
    if ($Evidence) { $detail = $detail + ' ' + $Evidence }
    return Assert-True -Id $Id -What $What -Condition $ok -TaskId $TaskId -Command $Command `
        -Evidence $detail
}

function Assert-NonEmpty {
    param(
        [Parameter(Mandatory)][string]$Id,
        [Parameter(Mandatory)][string]$What,
        $Value,
        [string]$TaskId = '',
        [string]$Command = '',
        [string]$Evidence = ''
    )
    $ok = -not [string]::IsNullOrWhiteSpace([string]$Value)
    $detail = "value=[$([string]$Value)]"
    if ($Evidence) { $detail += "; $Evidence" }
    return Assert-True -Id $Id -What $What -Condition $ok -TaskId $TaskId -Command $Command -Evidence $detail
}

function Assert-SamePath {
    param(
        [Parameter(Mandatory)][string]$Id,
        [Parameter(Mandatory)][string]$What,
        [string]$Expected,
        [string]$Actual,
        [string]$TaskId = ''
    )
    $ok = Test-SamePath -A $Expected -B $Actual
    return Assert-True -Id $Id -What $What -Condition $ok -TaskId $TaskId `
        -Evidence "expected=[$(ConvertTo-NormalPath $Expected)] actual=[$(ConvertTo-NormalPath $Actual)]"
}

function Block-Item {
    param(
        [Parameter(Mandatory)][string]$Id,
        [Parameter(Mandatory)][string]$What,
        [string]$TaskId = '',
        [string]$Evidence = '',
        [string]$Command = ''
    )
    [void]$script:Undriven.Add([pscustomobject]@{ id = $Id; what = $What; task = $TaskId; evidence = Protect-Text $Evidence 400 })
    return Add-Assertion -Id $Id -What $What -Status 'BLOCKED' -TaskId $TaskId -Command $Command -Evidence $Evidence
}
function Preserve-ResourcesForPendingTool {
    param([Parameter(Mandatory)][string]$AssertionId, [Parameter(Mandatory)][string]$Reason)
    $script:KeepResources = $true
    $script:PendingUserInteraction = $true
    if ($script:RunManifest) {
        Save-AcceptanceRunManifest -Phase 'awaiting_user' -Status 'preserved'
    }
    [void](Add-Assertion -Id $AssertionId -What 'isolated resources are preserved because a primary approval may still be active' -Status 'INFO' -Evidence $Reason)
}


# ---------------------------------------------------------------------------
# small helpers
# ---------------------------------------------------------------------------

function ConvertTo-NormalPath {
    param([string]$Path)
    if ([string]::IsNullOrWhiteSpace($Path)) { return '' }
    $value = $Path.Replace('/', '\')
    try { $value = [System.IO.Path]::GetFullPath($value) } catch { }
    return $value.TrimEnd('\')
}

function Test-SamePath {
    param([string]$A, [string]$B)
    return ((ConvertTo-NormalPath $A) -ieq (ConvertTo-NormalPath $B))
}

function Test-SessionSocketSeparation {
    param([Parameter(Mandatory)][bool]$ServerStarted, [string]$RunSocket, [string]$DefaultSocket)
    if (-not $ServerStarted) { return $null }
    if ([string]::IsNullOrWhiteSpace($RunSocket) -or [string]::IsNullOrWhiteSpace($DefaultSocket)) { return $false }
    return -not (Test-SamePath -A $RunSocket -B $DefaultSocket)
}

function Test-Below {
    # True when Path is inside Root (both normalised), which is how the script
    # proves a checkout belongs to this run before touching it.
    param([string]$Root, [string]$Path)
    $r = (ConvertTo-NormalPath $Root)
    $p = (ConvertTo-NormalPath $Path)
    if ($r -eq '' -or $p -eq '') { return $false }
    return ($p -ieq $r) -or $p.StartsWith($r + '\', [System.StringComparison]::OrdinalIgnoreCase)
}
function Get-AcceptanceRunIdFromProjectKey {
    param([Parameter(Mandatory)][string]$Key, [string]$TempRoot = $env:TEMP)
    $temp = ConvertTo-NormalPath $TempRoot
    $path = ConvertTo-NormalPath $Key
    if (-not (Test-Below -Root $temp -Path $path)) { return '' }
    $relative = $path.Substring($temp.Length).TrimStart([char[]]@('\', '/'))
    $match = [regex]::Match($relative, '^orch-accept-(?<run>[0-9a-f]{8})\\repo$', [System.Text.RegularExpressions.RegexOptions]::IgnoreCase)
    if (-not $match.Success) { return '' }
    return $match.Groups['run'].Value.ToLowerInvariant()
}


function Write-Step {
    param([string]$Text)
    Write-Host ''
    Write-Host ("=== {0}" -f $Text) -ForegroundColor White
    # The journal survives a hard host exit (which skips finally), so a failed
    # run still shows the last step it reached.
    try {
        Add-Content -LiteralPath $script:ReportText -Value ("--- step: {0} {1}" -f (Get-Date).ToUniversalTime().ToString('o'), $Text) -ErrorAction SilentlyContinue
    } catch { }
}
function Get-ScenarioBoundedTimeoutSeconds {
    param([Parameter(Mandatory)][double]$Requested)
    if (-not $script:ScenarioDeadline) { return $Requested }
    $remaining = ($script:ScenarioDeadline - (Get-Date)).TotalSeconds
    if ($remaining -le 0) {
        throw 'acceptance scenario behavior budget expired'
    }
    return [math]::Max(0.25, [math]::Min($Requested, $remaining))
}
function Get-AcceptanceScenarioPhases {
    param([ValidateSet('Full', 'SingleWorker')][string]$Scenario)
    if ($Scenario -eq 'SingleWorker') {
        return @('SW0', 'SW1', 'A0', 'V0')
    }
    return @('N7', 'D0', 'PR0', 'Q0', 'K0', 'M0', 'W0', 'A0', 'V0', 'TK0', 'C0', 'G0', 'T0')
}

function Wait-Until {
    <#
        Polls a script block until it returns a truthy value or the timeout
        expires. Returns {Ok, Value, Elapsed}. Sleeps between polls; never
        busy-waits a process.
    #>
    param(
        [Parameter(Mandatory)][scriptblock]$Probe,
        [double]$TimeoutSeconds = 60,
        [double]$IntervalSeconds = 3,
        [string]$What = 'condition'
    )
    $timeout = Get-ScenarioBoundedTimeoutSeconds -Requested $TimeoutSeconds
    $start = Get-Date
    $last = $null
    while (((Get-Date) - $start).TotalSeconds -lt $timeout) {
        if ($script:ScenarioDeadline -and (Get-Date) -ge $script:ScenarioDeadline) {
            throw 'acceptance scenario behavior budget expired'
        }
        try { $last = & $Probe } catch { $last = $null }
        if ($last) {
            return [pscustomobject]@{ Ok = $true; Value = $last; Elapsed = ((Get-Date) - $start).TotalSeconds }
        }
        Start-Sleep -Milliseconds ([int]([math]::Max(250, $IntervalSeconds * 1000)))
    }
    return [pscustomobject]@{ Ok = $false; Value = $last; Elapsed = ((Get-Date) - $start).TotalSeconds }
}

function Get-PaneScreen {
    param(
        [Parameter(Mandatory)][string]$PaneId,
        [ValidateSet('visible', 'recent', 'recent-unwrapped')][string]$Source = 'visible',
        [int]$Lines = 120,
        [switch]$Plain
    )
    $argv = if ($Plain) { @('pane', 'read', $PaneId) } else { @('agent', 'read', $PaneId) }
    $argv += @('--source', $Source)
    if ($Lines -gt 0) { $argv += @('--lines', [string]$Lines) }
    $result = Invoke-Herdr -Arguments $argv -TimeoutSeconds 60
    if ($result.ExitCode -ne 0 -and -not $result.Stdout) { return '' }
    return [string]$result.Stdout
}

function Get-EffectiveExtensionDir {
    $homeDir = if ($env:USERPROFILE) { $env:USERPROFILE } else { $env:HOME }
    $configDir = if ($env:PI_CONFIG_DIR) { $env:PI_CONFIG_DIR } else { '.omp' }
    $profileName = if (Test-Path env:OMP_PROFILE) { $env:OMP_PROFILE } elseif (Test-Path env:PI_PROFILE) { $env:PI_PROFILE } else { '' }
    $profileName = ([string]$profileName).Trim()
    if ($profileName -ne '' -and $profileName -ne 'default') {
        return (Join-Path $homeDir (Join-Path $configDir (Join-Path 'profiles' (Join-Path $profileName 'agent/extensions'))))
    }
    if ($env:PI_CODING_AGENT_DIR) { return (Join-Path $env:PI_CODING_AGENT_DIR 'extensions') }
    return (Join-Path $homeDir (Join-Path $configDir 'agent/extensions'))
}

function Get-FileHashSafe {
    # SHA-256 without Get-FileHash: this Windows PowerShell install does not
    # expose that cmdlet, and the script must not depend on it.
    param([string]$Path)
    if (-not (Test-Path -LiteralPath $Path)) { return '' }
    $sha = [System.Security.Cryptography.SHA256]::Create()
    try {
        $stream = [System.IO.File]::OpenRead($Path)
        try { $bytes = $sha.ComputeHash($stream) } finally { $stream.Dispose() }
    } finally {
        $sha.Dispose()
    }
    return ([System.BitConverter]::ToString($bytes) -replace '-', '')
}

# ---------------------------------------------------------------------------
# plugin state store (read-only inspection plus an exactly-scoped delete)
# ---------------------------------------------------------------------------
#
# The store is the plugin's own SQLite file (internal/state: orchestrator.sqlite,
# WAL, keyed by the canonical repository path). The script reads it to assert
# durable facts and, at the very end, deletes ONLY the rows whose project key is
# this run's canonical temporary repository path - and only after proving that no
# foreign row references any of this run's identities.
#
# python is used because Windows PowerShell has no SQLite provider; node's
# node:sqlite is the fallback. If neither exists the script reports BLOCKED and
# leaves every resource in place rather than guessing.

$script:StatePython = @'
import json, sqlite3, sys

TABLES = ("projects", "tasks", "inbox", "ready_sessions", "tickets")


def fact(row):
    out = dict(row)
    data = out.get("data")
    if isinstance(data, str) and data:
        try:
            out["fact"] = json.loads(data)
        except Exception:
            out["fact"] = {}
    return out


def rows(cur, table):
    try:
        return [fact(r) for r in cur.execute("SELECT rowid AS _rowid, * FROM " + table)]
    except sqlite3.OperationalError as error:
        # A store written before the tickets table existed has no such table: that
        # is an empty tickets list, not a failure of the whole read. Any other
        # missing table means this is not the plugin's store (sqlite3.connect creates
        # an empty file for a wrong path), so it must fail rather than read as empty.
        if table == "tickets" and "no such table" in str(error):
            return []
        raise


def identity(table, row):
    if table == "projects":
        return row.get("key")
    if table == "tasks":
        return row.get("id")
    if table == "inbox":
        return row.get("id")
    if table == "tickets":
        return row.get("id")
    return row.get("launch_pane_id")


def dump(cur):
    return {t: rows(cur, t) for t in TABLES}


def partitions(cur, key, panes):
    """Split every table into (ours, others) by exact identity."""
    data = dump(cur)
    ours = {}
    others = {}
    ours["projects"] = [r for r in data["projects"] if r.get("key") == key]
    others["projects"] = [r for r in data["projects"] if r.get("key") != key]
    ours["tasks"] = [r for r in data["tasks"] if r.get("project_key") == key]
    others["tasks"] = [r for r in data["tasks"] if r.get("project_key") != key]
    ours["inbox"] = [r for r in data["inbox"] if r.get("project_key") == key]
    others["inbox"] = [r for r in data["inbox"] if r.get("project_key") != key]
    ours["tickets"] = [r for r in data["tickets"] if r.get("project_key") == key]
    others["tickets"] = [r for r in data["tickets"] if r.get("project_key") != key]
    ours["ready_sessions"] = [r for r in data["ready_sessions"] if r.get("launch_pane_id") in panes]
    others["ready_sessions"] = [r for r in data["ready_sessions"] if r.get("launch_pane_id") not in panes]
    return data, ours, others


def canonical(table, rows_in):
    out = []
    for r in rows_in:
        ident = identity(table, r)
        out.append(ident + "|" + json.dumps(r, sort_keys=True))
    return sorted(out)


def foreign_reference(data, key, panes):
    """True when a row that is NOT this run's references one of its identities."""
    task_ids = set()
    session_ids = set()
    for t in data["tasks"]:
        if t.get("project_key") == key:
            task_ids.add(t.get("id"))
            session_ids.add((t.get("fact") or {}).get("OMPSessionID", ""))
    for p in data["projects"]:
        if p.get("key") == key:
            session_ids.add((p.get("fact") or {}).get("OMPSessionID", ""))
    session_ids.discard("")
    ticket_ids = {t.get("id") for t in data["tickets"] if t.get("project_key") == key}
    hits = []
    for t in data["tasks"]:
        if t.get("project_key") == key:
            continue
        f = t.get("fact") or {}
        if t.get("id") in task_ids or f.get("OMPSessionID") in session_ids or (f.get("TicketID") and f.get("TicketID") in ticket_ids):
            hits.append("tasks/" + str(t.get("id")))
    for p in data["projects"]:
        if p.get("key") == key:
            continue
        f = p.get("fact") or {}
        if f.get("OMPSessionID") in session_ids:
            hits.append("projects/" + str(p.get("key")))
    for i in data["inbox"]:
        if i.get("project_key") == key:
            continue
        if i.get("task_id") in task_ids:
            hits.append("inbox/" + str(i.get("id")))
    return hits


def main():
    db = sys.argv[1]
    op = sys.argv[2]
    con = sqlite3.connect(db, timeout=10.0)
    con.row_factory = sqlite3.Row
    cur = con.cursor()
    if op == "dump":
        print(json.dumps({"ok": True, "tables": dump(cur)}))
        return
    key = sys.argv[3] if len(sys.argv) > 3 else ""
    panes = [p for p in (sys.argv[4].split(",") if len(sys.argv) > 4 and sys.argv[4] else []) if p]
    data, ours, others = partitions(cur, key, panes)
    hits = foreign_reference(data, key, panes)
    counts = {t: len(ours[t]) for t in TABLES}
    others_before = {t: canonical(t, others[t]) for t in TABLES}
    if op == "plan-scoped":
        print(json.dumps({"ok": True, "counts": counts, "foreign": hits,
                          "ours": {t: [identity(t, r) for r in ours[t]] for t in TABLES}}))
        return
    if op != "delete-scoped":
        print(json.dumps({"ok": False, "error": "unknown op " + op}))
        sys.exit(2)
    if hits:
        print(json.dumps({"ok": False, "error": "a row outside this run references one of its identities",
                          "foreign": hits, "counts": counts}))
        sys.exit(3)
    cur.execute("BEGIN IMMEDIATE")
    for t in TABLES:
        for r in ours[t]:
            cur.execute("DELETE FROM " + t + " WHERE rowid = ?", (r["_rowid"],))
    con.commit()
    _, ours_after, others_after_rows = partitions(cur, key, panes)
    others_after = {t: canonical(t, others_after_rows[t]) for t in TABLES}
    identical = all(others_before[t] == others_after[t] for t in TABLES)
    print(json.dumps({"ok": True, "deleted": counts, "others_identical": identical,
                      "others_before": sum(len(others_before[t]) for t in TABLES),
                      "others_after": sum(len(others_after[t]) for t in TABLES),
                      "ours_remaining": sum(len(ours_after[t]) for t in TABLES)}))


main()
'@

function Initialize-StateTools {
    $python = $null
    foreach ($candidate in @('python', 'python3', 'py')) {
        $cmd = Get-Command $candidate -ErrorAction SilentlyContinue
        if ($cmd) { $python = $cmd.Source; break }
    }
    if (-not $python) { return $null }
    Set-Content -LiteralPath $script:StateHelper -Value $script:StatePython -Encoding UTF8
    $probe = Invoke-Native -File $python -Arguments @($script:StateHelper) -TimeoutSeconds 30
    # No arguments means the program exits with a usage error; what matters is
    # that the interpreter starts and the file parses.
    if ($probe.Stdout -match 'IndexError' -or $probe.Stderr -match 'SyntaxError') { return $null }
    return $python
}

function Invoke-State {
    param(
        [Parameter(Mandatory)][string]$Operation,
        [string[]]$Arguments = @()
    )
    if (-not $script:Python) { throw 'no SQLite-capable interpreter was found' }
    $argv = @($script:StateHelper, $script:StateDb, $Operation) + $Arguments
    $result = Invoke-Native -File $script:Python -Arguments $argv -TimeoutSeconds 120
    if ($result.ExitCode -ne 0 -and $result.ExitCode -ne 3) {
        throw "state $Operation failed (exit $($result.ExitCode)): $(Protect-Text $result.Stderr 400)"
    }
    return ($result.Stdout | ConvertFrom-Json)
}

function Get-StateDump {
    return (Invoke-State -Operation 'dump')
}
function Get-StaleAcceptanceSessionDisposition {
    param([string]$RunId, [object[]]$Sessions, [string]$SessionsRoot)
    if ($RunId -notmatch '^[0-9a-f]{8}$' -or -not $SessionsRoot) { throw "invalid acceptance session identity: $RunId" }
    $name = "orch-acc-$RunId"
    $matches = @($Sessions | Where-Object { (Get-Field $_ 'name') -eq $name })
    if ($matches.Count -eq 0) { return 'absent' }
    if ($matches.Count -ne 1 -or -not (Test-SamePath -A (Get-Field $matches[0] 'session_dir') -B (Join-Path $SessionsRoot $name))) {
        throw "refusing to clean ${name}: session directory or listing is ambiguous"
    }
    $running = Test-JsonBool -Object $matches[0] -Name 'running'
    if ($null -eq $running) { throw "refusing to clean ${name}: running state is unknown" }
    if ($running) { return 'running' }
    return 'stopped'
}

function Read-AcceptanceRunIdentity {
    param([string]$RunId, [string]$ProjectKey, [string]$TempRoot = $env:TEMP)
    $manifestPath = Join-Path (Join-Path $script:RunArchiveRoot $RunId) 'manifest.json'
    $identityPath = $manifestPath
    if (-not (Test-Path -LiteralPath $manifestPath -PathType Leaf)) {
        $identityPath = Join-Path (Join-Path $TempRoot "orch-accept-$RunId") 'report.json'
    }
    if (-not (Test-Path -LiteralPath $identityPath -PathType Leaf)) {
        if ($script:RetireStaleAcceptanceRunId -ceq $RunId -and
            (Get-AcceptanceRunIdFromProjectKey -Key $ProjectKey -TempRoot $TempRoot) -ceq $RunId -and
            (Test-SamePath -A $ProjectKey -B (Join-Path (Join-Path $TempRoot "orch-accept-$RunId") 'repo'))) {
            return [pscustomobject]@{
                runId = $RunId
                session = "orch-acc-$RunId"
                projectKey = $ProjectKey
                workRoot = Join-Path $TempRoot "orch-accept-$RunId"
                status = 'legacy_unknown'
                pendingUserInteraction = $true
                resourcesPreserved = $true
                keepResourcesRequested = $true
            }
        }
        throw "run $RunId has no verifiable durable manifest or legacy report; only an exact run-id retirement may use the database-bound legacy identity"
    }
    try { $identity = Get-Content -LiteralPath $identityPath -Raw | ConvertFrom-Json } catch {
        throw "run $RunId identity cannot be read"
    }
    if ((Get-Field $identity 'runId') -cne $RunId -or
        (Get-Field $identity 'session') -cne "orch-acc-$RunId" -or
        -not (Test-SamePath -A (Get-Field $identity 'projectKey') -B $ProjectKey)) {
        throw "run $RunId identity disagrees with its project key"
    }
    if ($identityPath -ceq $manifestPath -and
        -not (Test-SamePath -A (Get-Field $identity 'workRoot') -B (Join-Path $TempRoot "orch-accept-$RunId"))) {
        throw "run $RunId manifest names a different temporary root"
    }
    return $identity
}

function Get-AcceptanceRecoveryDisposition {
    param([Parameter(Mandatory)]$Manifest, [Parameter(Mandatory)][string]$SessionDisposition)
    if ($SessionDisposition -notin @('running', 'stopped', 'absent')) { return 'confirm' }
    $ownerPid = 0
    $ownerStartedAt = [DateTime]::MinValue
    if (-not [int]::TryParse([string](Get-Field $Manifest 'ownerPid'), [ref]$ownerPid) -or
        $ownerPid -le 0 -or
        -not [DateTime]::TryParse([string](Get-Field $Manifest 'ownerStartedAt'), [ref]$ownerStartedAt)) {
        return 'unverified'
    }
    try {
        $ownerProcess = Get-Process -Id $ownerPid -ErrorAction Stop
    } catch {
        if ($_.CategoryInfo.Category -ne 'ObjectNotFound') { return 'unverified' }
        $ownerProcess = $null
    }
    if ($ownerProcess) {
        try {
            $actualStartedAt = $ownerProcess.StartTime.ToUniversalTime()
        } catch {
            return 'unverified'
        }
        if ([math]::Abs(($actualStartedAt - $ownerStartedAt.ToUniversalTime()).TotalSeconds) -lt 2) {
            return 'active'
        }
        # The PID was reused. The process recorded by the manifest has exited.
    }
    $status = [string](Get-Field $Manifest 'status')
    if ($status -notin @('active', 'cleanup', 'incomplete', 'preserved', 'complete')) { return 'confirm' }
    if ($SessionDisposition -ne 'running' -and $status -eq 'complete') { return 'not-running' }
    return 'recover'
}

function Start-AcceptanceRunCleanup {
    param(
        [Parameter(Mandatory)][string]$RunId,
        [Parameter(Mandatory)][string]$ProjectKey,
        [Parameter(Mandatory)]$Identity,
        [Parameter(Mandatory)][bool]$RetirementAuthorized,
        [string]$TempRoot = $env:TEMP
    )
    if (-not $RetirementAuthorized) { throw "refusing cleanup transition for $RunId without a verified recovery decision" }
    $root = Assert-StaleAcceptanceRunRoot -RunId $RunId -ProjectKey $ProjectKey -TempRoot $TempRoot
    $recordDir = Join-Path $script:RunArchiveRoot $RunId
    [void](New-Item -ItemType Directory -Path $recordDir -Force)
    foreach ($name in @('report.json', 'report.txt')) {
        $source = Join-Path $root $name
        $destination = Join-Path $recordDir $name
        if ((Test-Path -LiteralPath $source -PathType Leaf) -and
            -not (Test-Path -LiteralPath $destination -PathType Leaf)) {
            Copy-Item -LiteralPath $source -Destination $destination -ErrorAction Stop
        }
    }
    $now = [DateTime]::UtcNow.ToString('o')
    $manifest = [ordered]@{
        schemaVersion = [int](Get-Field $Identity 'schemaVersion' 1)
        runId = $RunId
        session = "orch-acc-$RunId"
        projectKey = $ProjectKey
        workRoot = $root
        repoPath = $ProjectKey
        worktreesRoot = Join-Path $root 'worktrees'
        configPath = Join-Path $root 'herdr-config.toml'
        stateDb = if (Get-Field $Identity 'stateDb') { [string](Get-Field $Identity 'stateDb') } else { $script:RunLockStateDb }
        previousOwnerPid = [int](Get-Field $Identity 'ownerPid' 0)
        previousOwnerStartedAt = [string](Get-Field $Identity 'ownerStartedAt')
        ownerPid = $PID
        ownerStartedAt = $script:OwnerProcessStartedAt
        serverPid = [int](Get-Field $Identity 'serverPid' 0)
        serverStartedAt = [string](Get-Field $Identity 'serverStartedAt')
        serverExecutable = [string](Get-Field $Identity 'serverExecutable')
        serverStartAttempted = Test-JsonBool -Object $Identity -Name 'serverStartAttempted'
        sessionDir = [string](Get-Field $Identity 'sessionDir')
        phase = 'stale-cleanup'
        status = 'cleanup'
        previousPendingUserInteraction = (Test-JsonBool -Object $Identity -Name 'pendingUserInteraction')
        previousResourcesPreserved = (Test-JsonBool -Object $Identity -Name 'resourcesPreserved')
        previousKeepResourcesRequested = (Test-JsonBool -Object $Identity -Name 'keepResourcesRequested')
        pendingUserInteraction = $false
        resourcesPreserved = $false
        keepResourcesRequested = $false
        retirementAuthorized = $true
        tempRootCreated = (Test-JsonBool -Object $Identity -Name 'tempRootCreated')
        createdWorkspaces = @(Get-Field $Identity 'createdWorkspaces' @())
        createdPanes = @(Get-Field $Identity 'createdPanes' @())
        startedAt = [string](Get-Field $Identity 'startedAt')
        updatedAt = $now
        finishedAt = ''
    }
    Write-AtomicRunManifest -Manifest $manifest -Path (Join-Path $recordDir 'manifest.json')
    return $manifest
}

function Complete-AcceptanceRunCleanup {
    param([Parameter(Mandatory)][string]$RunId)
    $manifestPath = Join-Path (Join-Path $script:RunArchiveRoot $RunId) 'manifest.json'
    if (-not (Test-Path -LiteralPath $manifestPath -PathType Leaf)) { throw "cleanup journal is missing for $RunId" }
    try { $manifest = Get-Content -LiteralPath $manifestPath -Raw | ConvertFrom-Json } catch {
        throw "cleanup journal cannot be read for $RunId"
    }
    if ((Get-Field $manifest 'runId') -cne $RunId -or
        (Get-Field $manifest 'session') -cne "orch-acc-$RunId" -or
        (Get-Field $manifest 'status') -cne 'cleanup' -or
        (Test-JsonBool -Object $manifest -Name 'retirementAuthorized') -ne $true) {
        throw "refusing to complete an unverified cleanup journal for $RunId"
    }
    $manifest.phase = 'retired'
    $manifest.status = 'complete'
    $manifest.pendingUserInteraction = $false
    $manifest.resourcesPreserved = $false
    $manifest.keepResourcesRequested = $false
    $manifest.retirementAuthorized = $false
    $manifest.updatedAt = [DateTime]::UtcNow.ToString('o')
    $manifest.finishedAt = $manifest.updatedAt
    Write-AtomicRunManifest -Manifest $manifest -Path $manifestPath
}

# A just-stopped Herdr server or its children can keep a handle on a directory for a
# moment on Windows; retry briefly, then surface the real error so a genuine holder is not hidden.
function Remove-DirectoryTreeWithRetry {
    param([Parameter(Mandatory)][string]$Path, [int]$Attempts = 15, [int]$DelayMilliseconds = 1000)
    for ($attempt = 1; $attempt -le $Attempts; $attempt++) {
        try {
            if (Test-Path -LiteralPath $Path) { Remove-Item -LiteralPath $Path -Recurse -Force -ErrorAction Stop }
            return
        } catch {
            if ($attempt -eq $Attempts) { throw }
            Start-Sleep -Milliseconds $DelayMilliseconds
        }
    }
}


function Move-LegacyAcceptanceReportRootToArchive {
    param(
        [Parameter(Mandatory)][string]$RunId,
        [Parameter(Mandatory)][string]$TempRoot,
        [Parameter(Mandatory)][string]$ArchiveRoot,
        [Parameter(Mandatory)]$StateDump,
        [object[]]$Sessions = @(),
        [string]$DefaultProjection = ''
    )
    if ($RunId -notmatch '^[0-9a-f]{8}$') { return $false }
    $root = Join-Path $TempRoot "orch-accept-$RunId"
    if (-not (Test-Path -LiteralPath $root -PathType Container)) { return $false }
    if (@($Sessions | Where-Object { (Get-Field $_ 'name') -ceq "orch-acc-$RunId" }).Count -gt 0) { return $false }
    $reportPath = Join-Path $root 'report.json'
    if (-not (Test-Path -LiteralPath $reportPath -PathType Leaf)) { return $false }
    try { $report = Get-Content -LiteralPath $reportPath -Raw | ConvertFrom-Json } catch { return $false }
    if ((Get-Field $report 'runId') -cne $RunId -or
        (Get-Field $report 'session') -cne "orch-acc-$RunId" -or
        (Test-JsonBool -Object $report -Name 'keepResourcesRequested') -ne $false -or
        (Test-JsonBool -Object $report -Name 'resourcesPreserved') -ne $false) { return $false }
    $projectKey = [string](Get-Field $report 'projectKey')
    if ($projectKey -and
        ((Get-AcceptanceRunIdFromProjectKey -Key $projectKey -TempRoot $TempRoot) -cne $RunId -or
         -not (Test-SamePath -A $projectKey -B (Join-Path $root 'repo')))) { return $false }
    if ($DefaultProjection -and (Test-ProjectionMentions -Projection $DefaultProjection -Path $root)) { return $false }
    if ($projectKey) {
        foreach ($table in @('projects', 'tasks', 'inbox', 'tickets')) {
            foreach ($row in @(Get-Field $StateDump.tables $table @())) {
                $key = if ($table -eq 'projects') { [string](Get-Field $row 'key') } else { [string](Get-Field $row 'project_key') }
                if ($key -and (Test-SamePath -A $key -B $projectKey)) { return $false }
            }
        }
    }
    $entries = @(Get-ChildItem -LiteralPath $root -Force)
    if (@($entries | Where-Object { $_.Name -notin @('report.json', 'report.txt') -or $_.PSIsContainer }).Count -gt 0) { return $false }
    $allEntries = @((Get-Item -LiteralPath $root -Force)) + $entries
    $links = @($allEntries | Where-Object { $_.Attributes -band [System.IO.FileAttributes]::ReparsePoint })
    if ($links.Count -gt 0) { return $false }

    $recordDir = Join-Path $ArchiveRoot $RunId
    [void](New-Item -ItemType Directory -Path $recordDir -Force)
    $manifestPath = Join-Path $recordDir 'manifest.json'
    if (Test-Path -LiteralPath $manifestPath -PathType Leaf) {
        try { $existing = Get-Content -LiteralPath $manifestPath -Raw | ConvertFrom-Json } catch { return $false }
        if ((Get-Field $existing 'runId') -cne $RunId -or (Get-Field $existing 'session') -cne "orch-acc-$RunId" -or
            (Get-Field $existing 'status') -notin @('migrating', 'complete')) { return $false }
    }
    foreach ($name in @('report.json', 'report.txt')) {
        $source = Join-Path $root $name
        $destination = Join-Path $recordDir $name
        if (-not (Test-Path -LiteralPath $source -PathType Leaf)) { continue }
        if (Test-Path -LiteralPath $destination -PathType Leaf) {
            if ([System.IO.File]::ReadAllText($source) -cne [System.IO.File]::ReadAllText($destination)) {
                throw "legacy report archive identity collision for $RunId/$name"
            }
        } else {
            Copy-Item -LiteralPath $source -Destination $destination -ErrorAction Stop
        }
    }
    $now = [DateTime]::UtcNow.ToString('o')
    $manifest = [ordered]@{
        schemaVersion = 1
        runId = $RunId
        session = "orch-acc-$RunId"
        projectKey = $projectKey
        workRoot = $root
        repoPath = if ($projectKey) { $projectKey } else { Join-Path $root 'repo' }
        worktreesRoot = Join-Path $root 'worktrees'
        configPath = Join-Path $root 'herdr-config.toml'
        stateDb = ''
        ownerPid = 0
        ownerStartedAt = ''
        serverPid = 0
        serverStartedAt = ''
        sessionDir = ''
        phase = 'legacy-report-migration'
        status = 'migrating'
        pendingUserInteraction = $false
        resourcesPreserved = $false
        keepResourcesRequested = $false
        retirementAuthorized = $true
        tempRootCreated = $true
        createdWorkspaces = @()
        createdPanes = @()
        startedAt = [string](Get-Field $report 'startedAt')
        updatedAt = $now
        finishedAt = ''
    }
    Write-AtomicRunManifest -Manifest $manifest -Path $manifestPath
    Remove-DirectoryTreeWithRetry -Path $root
    $manifest.status = 'complete'
    $manifest.phase = 'legacy-report-archived'
    $manifest.retirementAuthorized = $false
    $manifest.finishedAt = [DateTime]::UtcNow.ToString('o')
    $manifest.updatedAt = $manifest.finishedAt
    Write-AtomicRunManifest -Manifest $manifest -Path $manifestPath
    return $true
}

function Assert-StaleAcceptanceRunRoot {
    param([string]$RunId, [string]$ProjectKey, [string]$TempRoot = $env:TEMP)
    $root = Join-Path $TempRoot "orch-accept-$RunId"
    if ((Get-AcceptanceRunIdFromProjectKey -Key $ProjectKey -TempRoot $TempRoot) -ne $RunId -or
        -not (Test-SamePath -A $ProjectKey -B (Join-Path $root 'repo'))) {
        throw "refusing to remove run root: project key does not identify $root"
    }
    if (-not (Test-Path -LiteralPath $root)) { return $root }
    try { [void](Read-AcceptanceRunIdentity -RunId $RunId -ProjectKey $ProjectKey -TempRoot $TempRoot) } catch {
        throw "refusing to remove ${root}: $($_.Exception.Message)"
    }
    $entries = @((Get-Item -LiteralPath $root -Force)) + @(Get-ChildItem -LiteralPath $root -Recurse -Force)
    $links = @($entries | Where-Object { $_.Attributes -band [System.IO.FileAttributes]::ReparsePoint })
    if ($links.Count -gt 0) { throw "refusing to remove ${root}: reparse point present" }
    return $root
}

function Remove-StaleAcceptanceRunRoot {
    param([string]$RunId, [string]$ProjectKey, [string]$TempRoot = $env:TEMP)
    $root = Assert-StaleAcceptanceRunRoot -RunId $RunId -ProjectKey $ProjectKey -TempRoot $TempRoot
    if (Test-Path -LiteralPath $root) {
        Remove-DirectoryTreeWithRetry -Path $root
    }
}

function Recover-ManifestOnlyAcceptanceRun {
    param(
        [Parameter(Mandatory)][string]$RunId,
        [Parameter(Mandatory)][string]$TempRoot,
        [Parameter(Mandatory)]$StateDump,
        [Parameter(Mandatory)][object[]]$Sessions,
        [Parameter(Mandatory)][string]$SessionsRoot,
        [string]$DefaultProjection = ''
    )
    if ($RunId -notmatch '^[0-9a-f]{8}$' -or $RunId -ceq $script:RunId) { return $false }
    $manifestPath = Join-Path (Join-Path $script:RunArchiveRoot $RunId) 'manifest.json'
    if (-not (Test-Path -LiteralPath $manifestPath -PathType Leaf)) { return $false }
    try { $manifest = Get-Content -LiteralPath $manifestPath -Raw | ConvertFrom-Json } catch {
        throw "refusing manifest-only recovery for ${RunId}: its central journal is unreadable"
    }
    if ((Get-Field $manifest 'runId') -cne $RunId -or
        (Get-Field $manifest 'session') -cne "orch-acc-$RunId") {
        throw "refusing manifest-only recovery for ${RunId}: journal identity is inconsistent"
    }
    if (-not (Test-SamePath -A ([string](Get-Field $manifest 'stateDb')) -B $script:RunLockStateDb)) { return $false }
    if ((Get-Field $manifest 'status') -ceq 'complete' -and
        -not (Test-Path -LiteralPath ([string](Get-Field $manifest 'workRoot')))) { return $false }

    $projectKey = [string](Get-Field $manifest 'projectKey')
    $root = Join-Path $TempRoot "orch-accept-$RunId"
    if (-not $projectKey -or
        (Get-AcceptanceRunIdFromProjectKey -Key $projectKey -TempRoot $TempRoot) -cne $RunId -or
        -not (Test-SamePath -A $projectKey -B (Join-Path $root 'repo')) -or
        -not (Test-SamePath -A (Get-Field $manifest 'workRoot') -B $root)) {
        throw "refusing manifest-only recovery for ${RunId}: recorded paths do not identify its exact temp root"
    }
    foreach ($table in @('projects', 'tasks', 'inbox', 'tickets')) {
        foreach ($row in @(Get-Field $StateDump.tables $table @())) {
            $key = if ($table -eq 'projects') { [string](Get-Field $row 'key') } else { [string](Get-Field $row 'project_key') }
            if ($key -and (Test-SamePath -A $key -B $projectKey)) {
                throw "refusing manifest-only recovery for ${RunId}: durable $table rows still reference its project"
            }
        }
    }
    if ($DefaultProjection -and (Test-ProjectionMentions -Projection $DefaultProjection -Path $root)) {
        throw "refusing manifest-only recovery for ${RunId}: the default session still references $root"
    }
    $disposition = Get-StaleAcceptanceSessionDisposition -RunId $RunId -Sessions $Sessions -SessionsRoot $SessionsRoot
    $recoveryMode = Get-AcceptanceRecoveryDisposition -Manifest $manifest -SessionDisposition $disposition
    if ($recoveryMode -in @('active', 'unverified')) {
        throw "refusing manifest-only recovery for ${RunId}: its owner process is live or cannot be verified"
    }
    $explicit = $script:RetireStaleAcceptanceRunId -ceq $RunId
    if ($recoveryMode -eq 'confirm' -and -not $explicit) {
        throw "refusing to retire preserved manifest-only acceptance run $RunId; pass -RetireStaleAcceptanceRunId $RunId after resolving its user interaction"
    }
    if ($disposition -eq 'running' -and $recoveryMode -ne 'recover' -and -not $explicit) {
        throw "refusing to stop manifest-only acceptance session orch-acc-$RunId without exact-run authorization"
    }
    [void](Start-AcceptanceRunCleanup -RunId $RunId -ProjectKey $projectKey -Identity $manifest `
        -RetirementAuthorized $true -TempRoot $TempRoot)

    if ($disposition -eq 'running') {
        $workspaceList = Get-HerdrResultForSession -SessionName "orch-acc-$RunId" -Arguments @('workspace', 'list') -TimeoutSeconds 30
        $workspaces = @(Get-Field $workspaceList 'workspaces' @())
        foreach ($workspace in $workspaces) {
            $checkout = [string](Get-Field (Get-Field $workspace 'worktree') 'checkout_path')
            if (-not $checkout -or -not (Test-Below -Root $root -Path $checkout)) {
                throw "refusing manifest-only recovery for ${RunId}: workspace $([string](Get-Field $workspace 'workspace_id')) is outside its temp root"
            }
        }
        foreach ($workspace in $workspaces) {
            $workspaceId = [string](Get-Field $workspace 'workspace_id')
            $close = Invoke-HerdrForSession -SessionName "orch-acc-$RunId" `
                -Arguments @('workspace', 'close', $workspaceId, '--group') -TimeoutSeconds 60
            $closeCode = Get-HerdrErrorCode -Result $close
            if ($close.ExitCode -ne 0 -and $closeCode -notin @('workspace_not_found', 'not_found')) {
                throw "failed to close manifest-owned workspace $workspaceId for $RunId"
            }
        }
        $remaining = Get-HerdrResultForSession -SessionName "orch-acc-$RunId" -Arguments @('workspace', 'list') -TimeoutSeconds 30
        if (@(Get-Field $remaining 'workspaces' @()).Count -gt 0) {
            throw "refusing to stop orch-acc-${RunId}: owned workspaces remain after close"
        }
        $stop = Invoke-HerdrForSession -SessionName "orch-acc-$RunId" -Arguments @('server', 'stop') -TimeoutSeconds 90
        if ($stop.ExitCode -ne 0) { throw "failed to stop manifest-owned session orch-acc-$RunId" }
        $stopped = Wait-Until -TimeoutSeconds 60 -IntervalSeconds 2 -What "manifest-owned session orch-acc-$RunId to stop" -Probe {
            if ((Get-StaleAcceptanceSessionDisposition -RunId $RunId -Sessions @(Get-Sessions) -SessionsRoot $SessionsRoot) -eq 'stopped') { return $true }
            return $null
        }
        if (-not $stopped.Ok) { throw "manifest-owned session orch-acc-$RunId did not stop" }
    }
    if ($disposition -ne 'absent') {
        $current = Get-StaleAcceptanceSessionDisposition -RunId $RunId -Sessions @(Get-Sessions) -SessionsRoot $SessionsRoot
        if ($current -ne 'stopped') { throw "refusing to delete orch-acc-${RunId}: it is not verified stopped" }
        $delete = Invoke-Herdr -Arguments @('session', 'delete', "orch-acc-$RunId") -NoSessionScope -TimeoutSeconds 60
        if ($delete.ExitCode -ne 0 -or
            (Get-StaleAcceptanceSessionDisposition -RunId $RunId -Sessions @(Get-Sessions) -SessionsRoot $SessionsRoot) -ne 'absent') {
            throw "failed to retire stopped manifest-only session orch-acc-$RunId"
        }
    }
    $serverProcessResult = Stop-VerifiedAcceptanceServerProcess -RunId $RunId `
        -Manifest $manifest -SessionDisposition 'absent'
    $serverProcessStatus = if ($serverProcessResult -eq 'not-recorded') { 'INFO' } else { 'PASS' }
    Add-Assertion -Id "P1.12f-$RunId" -What 'the manifest-recorded Herdr server process is absent or terminated' `
        -Status $serverProcessStatus -Evidence $serverProcessResult
    Remove-StaleAcceptanceRunRoot -RunId $RunId -ProjectKey $projectKey -TempRoot $TempRoot
    Complete-AcceptanceRunCleanup -RunId $RunId
    Add-Assertion -Id "P1.12e-$RunId" -What 'manifest-only interrupted run was safely retired by its recorded identity' -Status 'INFO' `
        -Evidence "session=$disposition recovery=$recoveryMode root=$root"
    return $true
}

function Clear-StaleAcceptanceState {
    $dump = Get-StateDump
    $staleProjects = @()
    foreach ($row in @($dump.tables.projects)) {
        $key = [string](Get-Field $row 'key')
        $runId = Get-AcceptanceRunIdFromProjectKey -Key $key
        if ($runId) {
            $staleProjects += [pscustomobject]@{ Key = $key; RunId = $runId; Row = $row }
        }
    }

    $staleKeySet = @{}
    $staleRunSet = @{}
    foreach ($item in $staleProjects) {
        $staleKeySet[$item.Key] = $true
        $staleRunSet[$item.RunId] = $true
    }
    $sessions = @(Get-Sessions)
    $defaultSession = @($sessions | Where-Object { (Get-Field $_ 'name') -eq 'default' })
    if ($defaultSession.Count -ne 1) { throw 'refusing stale cleanup: default session directory cannot be verified' }
    $sessionsRoot = Join-Path ([string](Get-Field $defaultSession[0] 'session_dir')) 'sessions'
    $retirementTargetFound = $false
    foreach ($item in $staleProjects) {
        $sessionName = "orch-acc-$($item.RunId)"
        if ($script:RetireStaleAcceptanceRunId -and $script:RetireStaleAcceptanceRunId -cne $item.RunId) {
            Add-Assertion -Id "P1.12c-$($item.RunId)" -What 'another acceptance run was left untouched because the retirement opt-in names a different run' -Status 'INFO' `
                -Evidence "requested=$($script:RetireStaleAcceptanceRunId) preserved=$($item.RunId)"
            continue
        }
        $root = Assert-StaleAcceptanceRunRoot -RunId $item.RunId -ProjectKey $item.Key
        $priorRecord = Read-AcceptanceRunIdentity -RunId $item.RunId -ProjectKey $item.Key
        $disposition = Get-StaleAcceptanceSessionDisposition -RunId $item.RunId -Sessions $sessions -SessionsRoot $sessionsRoot
        $priorKeepRequested = Test-JsonBool -Object $priorRecord -Name 'keepResourcesRequested'
        $priorResourcesPreserved = Test-JsonBool -Object $priorRecord -Name 'resourcesPreserved'
        $recoveryMode = Get-AcceptanceRecoveryDisposition -Manifest $priorRecord -SessionDisposition $disposition
        $keepLabel = if ($null -eq $priorKeepRequested) { 'unknown/not recorded' } else { [string]$priorKeepRequested }
        $preservedLabel = if ($null -eq $priorResourcesPreserved) { 'unknown/not recorded' } else { [string]$priorResourcesPreserved }
        if ($script:RetireStaleAcceptanceRunId -ceq $item.RunId) { $retirementTargetFound = $true }
        if ($recoveryMode -eq 'confirm') {
            Write-Host ("Verified stale acceptance run {0}: session={1} recovery={2}" -f `
                $sessionName, $disposition, $recoveryMode) -ForegroundColor Yellow
            if ($script:RetireStaleAcceptanceRunId -cne $item.RunId) {
                throw "refusing to retire $sessionName; pass -RetireStaleAcceptanceRunId $($item.RunId) to authorize only this verified run"
            }
        }
        if ($recoveryMode -in @('active', 'unverified')) {
            throw "refusing stale cleanup for ${sessionName}: its owner process is live or cannot be verified"
        }
        $projectTasks = @($dump.tables.tasks | Where-Object { $_.project_key -eq $item.Key })
        $projectFact = Get-Field $item.Row 'fact'
        if (-not $projectFact) { $projectFact = $item.Row }
        $rootWorkspace = [string](Get-Field $projectFact 'RepoWorkspaceID')
        $runWorkspaceIds = @()
        foreach ($workspace in @([string](Get-Field $projectFact 'RepoWorkspaceID'), [string](Get-Field $projectFact 'CurrentWorkspaceID'))) {
            if ($workspace -and $runWorkspaceIds -notcontains $workspace) { $runWorkspaceIds += $workspace }
        }
        $missingWorkspaceIdentity = $false
        foreach ($taskRow in $projectTasks) {
            $taskFact = Get-Field $taskRow 'fact'
            if (-not $taskFact) { $taskFact = $taskRow }
            $worktree = [string](Get-Field $taskFact 'WorktreePath')
            if ($worktree -and -not (Test-Below -Root $root -Path $worktree)) {
                throw "refusing to clean stale acceptance project $($item.Key): task worktree is outside $root"
            }
            $worktreeWorkspace = [string](Get-Field $taskFact 'WorktreeWorkspaceID')
            if ($worktree -and -not $worktreeWorkspace) { $missingWorkspaceIdentity = $true }
            foreach ($workspace in @($worktreeWorkspace, [string](Get-Field $taskFact 'CurrentWorkspaceID'))) {
                if ($workspace -and $runWorkspaceIds -notcontains $workspace) { $runWorkspaceIds += $workspace }
            }
        }
        if ($disposition -eq 'running' -and
            ([string]::IsNullOrWhiteSpace($rootWorkspace) -or $missingWorkspaceIdentity -or $runWorkspaceIds.Count -eq 0)) {
            throw "refusing to stop ${sessionName}: recorded workspace identities are incomplete"
        }
        if ($disposition -eq 'running') {
            foreach ($other in @($dump.tables.projects)) {
                if ([string](Get-Field $other 'key') -ceq $item.Key) { continue }
                $otherFact = Get-Field $other 'fact'
                foreach ($workspace in @([string](Get-Field $otherFact 'RepoWorkspaceID'), [string](Get-Field $otherFact 'CurrentWorkspaceID'))) {
                    if ($workspace -and $runWorkspaceIds -contains $workspace) {
                        throw "refusing to close stale acceptance workspace group: workspace $workspace is also owned by project $([string](Get-Field $other 'key'))"
                    }
                }
            }
            foreach ($other in @($dump.tables.tasks)) {
                if ([string](Get-Field $other 'project_key') -ceq $item.Key) { continue }
                $otherFact = Get-Field $other 'fact'
                foreach ($workspace in @([string](Get-Field $otherFact 'WorktreeWorkspaceID'), [string](Get-Field $otherFact 'CurrentWorkspaceID'))) {
                    if ($workspace -and $runWorkspaceIds -contains $workspace) {
                        throw "refusing to close stale acceptance workspace group: workspace $workspace is also owned by task $([string](Get-Field $other 'id'))"
                    }
                }
            }
        }
        $paneIds = @()
        foreach ($row in @($item.Row) + $projectTasks) {
            $fact = Get-Field $row 'fact'
            foreach ($field in @('LaunchPaneID', 'PublicPaneID')) {
                $pane = [string](Get-Field $fact $field)
                if ($pane -and $paneIds -notcontains $pane) { $paneIds += $pane }
            }
        }

        foreach ($other in @($dump.tables.projects)) {
            $otherKey = [string](Get-Field $other 'key')
            if ($staleKeySet.ContainsKey($otherKey)) { continue }
            $fact = Get-Field $other 'fact'
            foreach ($field in @('LaunchPaneID', 'PublicPaneID')) {
                $pane = [string](Get-Field $fact $field)
                if ($pane -and $paneIds -contains $pane) {
                    throw "refusing to clean stale acceptance project $($item.Key): pane $pane is also bound by non-test project $otherKey"
                }
            }
        }
        foreach ($other in @($dump.tables.tasks)) {
            $otherKey = [string](Get-Field $other 'project_key')
            if ($staleKeySet.ContainsKey($otherKey)) { continue }
            $fact = Get-Field $other 'fact'
            foreach ($field in @('LaunchPaneID', 'PublicPaneID')) {
                $pane = [string](Get-Field $fact $field)
                if ($pane -and $paneIds -contains $pane) {
                    throw "refusing to clean stale acceptance project $($item.Key): pane $pane is also bound by non-test task $([string](Get-Field $other 'id'))"
                }
            }
        }
        foreach ($ready in @($dump.tables.ready_sessions | Where-Object { $paneIds -contains $_.launch_pane_id })) {
            $file = [string](Get-Field $ready 'omp_session_file')
            $readyRun = [regex]::Match($file, '(?i)orch-accept-([0-9a-f]{8})')
            if (-not $readyRun.Success -or -not $staleRunSet.ContainsKey($readyRun.Groups[1].Value.ToLowerInvariant())) {
                throw "refusing to clean stale acceptance project $($item.Key): pane $($ready.launch_pane_id) has a non-stale ready-session identity"
            }
        }

        $paneArgument = $paneIds -join ','
        $plan = Invoke-State -Operation 'plan-scoped' -Arguments @($item.Key, $paneArgument)
        if (@($plan.foreign).Count -gt 0) {
            throw "refusing to clean stale acceptance project $($item.Key): foreign references remain"
        }
        [void](Start-AcceptanceRunCleanup -RunId $item.RunId -ProjectKey $item.Key `
            -Identity $priorRecord -RetirementAuthorized $true)
        if ($disposition -eq 'running') {
            if ($script:RetireStaleAcceptanceRunId -cne $item.RunId -and $recoveryMode -ne 'recover') {
                throw "refusing to stop live acceptance session ${sessionName} without a persisted recovery decision or exact run-id authorization"
            }
            [void](Assert-StaleAcceptanceRunRoot -RunId $item.RunId -ProjectKey $item.Key)
            if ((Get-StaleAcceptanceSessionDisposition -RunId $item.RunId -Sessions @(Get-Sessions) -SessionsRoot $sessionsRoot) -ne 'running') {
                throw "refusing to close ${sessionName}: session listing changed before retirement"
            }
            $close = Invoke-HerdrForSession -SessionName $sessionName `
                -Arguments @('workspace', 'close', $rootWorkspace, '--group') -TimeoutSeconds 60
            $closeCode = Get-HerdrErrorCode -Result $close
            if ($close.ExitCode -ne 0 -and $closeCode -notin @('workspace_not_found', 'not_found')) {
                throw "failed to close verified workspace group for ${sessionName}: $(Protect-Text $close.Stderr 300)"
            }
            $workspaceList = Get-HerdrResultForSession -SessionName $sessionName -Arguments @('workspace', 'list') -TimeoutSeconds 30
            $remainingWorkspaces = @(Get-Field $workspaceList 'workspaces' @())
            $sessionSnapshot = $null
            foreach ($workspace in $remainingWorkspaces) {
                $workspaceId = [string](Get-Field $workspace 'workspace_id')
                if (-not $workspaceId -or $runWorkspaceIds -notcontains $workspaceId) {
                    throw "refusing to close workspace $workspaceId in ${sessionName}: it is not in the recorded run workspace identities"
                }
                $checkout = [string](Get-Field (Get-Field $workspace 'worktree') 'checkout_path')
                if ($checkout) {
                    if (-not (Test-Below -Root $root -Path $checkout)) {
                        throw "refusing to close workspace $workspaceId in ${sessionName}: checkout $checkout is outside $root"
                    }
                    continue
                }
                if (-not $sessionSnapshot) {
                    $snapshotResult = Get-HerdrResultForSession -SessionName $sessionName -Arguments @('api', 'snapshot') -TimeoutSeconds 30
                    $sessionSnapshot = Get-Field $snapshotResult 'snapshot'
                }
                $workspacePanes = @((Get-Field $sessionSnapshot 'panes' @()) | Where-Object { (Get-Field $_ 'workspace_id') -ceq $workspaceId })
                if ($workspacePanes.Count -eq 0) {
                    throw "refusing to close workspace $workspaceId in ${sessionName}: its checkout is unavailable and no pane proves ownership under $root"
                }
                foreach ($pane in $workspacePanes) {
                    $paneId = [string](Get-Field $pane 'pane_id')
                    $cwd = [string](Get-Field $pane 'cwd')
                    if ($paneId -notin $paneIds -or -not (Test-Below -Root $root -Path $cwd)) {
                        throw "refusing to close workspace $workspaceId in ${sessionName}: pane $paneId is unrecorded or works outside $root"
                    }
                }
            }
            foreach ($workspace in $remainingWorkspaces) {
                if ((Get-StaleAcceptanceSessionDisposition -RunId $item.RunId -Sessions @(Get-Sessions) -SessionsRoot $sessionsRoot) -ne 'running') {
                    throw "refusing to close a workspace in ${sessionName}: session listing changed during retirement"
                }
                $workspaceId = [string](Get-Field $workspace 'workspace_id')
                $close = Invoke-HerdrForSession -SessionName $sessionName `
                    -Arguments @('workspace', 'close', $workspaceId, '--group') -TimeoutSeconds 60
                $closeCode = Get-HerdrErrorCode -Result $close
                if ($close.ExitCode -ne 0 -and $closeCode -notin @('workspace_not_found', 'not_found')) {
                    throw "failed to close verified workspace $workspaceId for ${sessionName}: $(Protect-Text $close.Stderr 300)"
                }
            }
            $workspaceList = Get-HerdrResultForSession -SessionName $sessionName -Arguments @('workspace', 'list') -TimeoutSeconds 30
            $liveWorkspaceIds = @((Get-Field $workspaceList 'workspaces' @()) | ForEach-Object { [string](Get-Field $_ 'workspace_id') })
            if ($liveWorkspaceIds.Count -gt 0) {
                throw "refusing to stop ${sessionName}: workspaces remain after closing recorded run workspaces: $($liveWorkspaceIds -join ',')"
            }
            $current = Get-StaleAcceptanceSessionDisposition -RunId $item.RunId -Sessions @(Get-Sessions) -SessionsRoot $sessionsRoot
            if ($current -eq 'running') {
                $stop = Invoke-HerdrForSession -SessionName $sessionName -Arguments @('server', 'stop') -TimeoutSeconds 90
                if ($stop.ExitCode -ne 0) { throw "failed to stop verified acceptance session ${sessionName}: $(Protect-Text $stop.Stderr 300)" }
            } elseif ($current -ne 'stopped') {
                throw "refusing to stop ${sessionName}: session identity changed after workspace close"
            }
            $stopped = Wait-Until -TimeoutSeconds 60 -IntervalSeconds 2 -What "acceptance session $sessionName to stop" -Probe {
                if ((Get-StaleAcceptanceSessionDisposition -RunId $item.RunId -Sessions @(Get-Sessions) -SessionsRoot $sessionsRoot) -eq 'stopped') {
                    return $true
                }
                return $null
            }
            if (-not $stopped.Ok) { throw "acceptance session ${sessionName} did not stop; leaving its state and files in place" }
        }
        if ($disposition -ne 'absent') {
            $current = Get-StaleAcceptanceSessionDisposition -RunId $item.RunId -Sessions @(Get-Sessions) -SessionsRoot $sessionsRoot
            if ($current -ne 'stopped') { throw "refusing to delete ${sessionName}: session is not verified stopped" }
            $retired = Invoke-Herdr -Arguments @('session', 'delete', $sessionName) -NoSessionScope -TimeoutSeconds 60
            if ($retired.ExitCode -ne 0 -or
                (Get-StaleAcceptanceSessionDisposition -RunId $item.RunId -Sessions @(Get-Sessions) -SessionsRoot $sessionsRoot) -ne 'absent') {
                throw "failed to retire stopped acceptance session $sessionName"
            }
            $sessions = @(Get-Sessions)
        }
        $serverProcessResult = Stop-VerifiedAcceptanceServerProcess -RunId $item.RunId `
            -Manifest $priorRecord -SessionDisposition 'absent'
        $serverProcessStatus = if ($serverProcessResult -eq 'not-recorded') { 'INFO' } else { 'PASS' }
        Add-Assertion -Id "P1.12f-$($item.RunId)" -What 'the manifest-recorded Herdr server process is absent or terminated' `
            -Status $serverProcessStatus -Evidence $serverProcessResult
        Remove-StaleAcceptanceRunRoot -RunId $item.RunId -ProjectKey $item.Key
        $deleted = Invoke-State -Operation 'delete-scoped' -Arguments @($item.Key, $paneArgument)
        if (-not (Get-Flag $deleted 'ok') -or -not (Get-Flag $deleted 'others_identical')) {
            throw "scoped cleanup did not preserve unrelated state for $($item.Key)"
        }
        Add-Assertion -Id "P1.12a-$($item.RunId)" -What 'stale acceptance state removed without changing unrelated rows' -Status 'INFO' `
            -Evidence ("project={0}; deleted={1}" -f $item.Key, (Get-Json $deleted.deleted))
        Complete-AcceptanceRunCleanup -RunId $item.RunId
        $dump = Get-StateDump
    }

    $manifestOnlyCandidates = New-Object System.Collections.ArrayList
    foreach ($recordDir in @(Get-ChildItem -LiteralPath $script:RunArchiveRoot -Directory -Force -ErrorAction SilentlyContinue)) {
        $manifestPath = Join-Path $recordDir.FullName 'manifest.json'
        if (-not (Test-Path -LiteralPath $manifestPath -PathType Leaf)) { continue }
        try { $manifest = Get-Content -LiteralPath $manifestPath -Raw | ConvertFrom-Json } catch {
            throw "refusing to recover unreadable acceptance journal $manifestPath"
        }
        $manifestRunId = [string](Get-Field $manifest 'runId')
        if ($manifestRunId -notmatch '^[0-9a-f]{8}$' -or
            (Split-Path -Leaf $recordDir.FullName) -cne $manifestRunId -or
            $manifestRunId -ceq $script:RunId -or $staleRunSet.ContainsKey($manifestRunId)) { continue }
        $manifestStateDb = [string](Get-Field $manifest 'stateDb')
        if ([string]::IsNullOrWhiteSpace($manifestStateDb) -or
            -not (Test-SamePath -A $manifestStateDb -B $script:RunLockStateDb)) { continue }
        [void]$manifestOnlyCandidates.Add([pscustomobject]@{ RunId = $manifestRunId; Manifest = $manifest })
    }
    if ($manifestOnlyCandidates.Count -gt 0) {
        $defaultSnapshot = Get-HerdrJson -Arguments @('api', 'snapshot') -NoSessionScope
        $defaultProjection = Get-FocusProjection -Snapshot (Get-JsonField $defaultSnapshot 'snapshot')
        foreach ($candidate in $manifestOnlyCandidates) {
            [void](Recover-ManifestOnlyAcceptanceRun -RunId $candidate.RunId -TempRoot $env:TEMP `
                -StateDump $dump -Sessions $sessions -SessionsRoot $sessionsRoot -DefaultProjection $defaultProjection)
            $sessions = @(Get-Sessions)
            $dump = Get-StateDump
            if ($script:RetireStaleAcceptanceRunId -ceq $candidate.RunId) { $retirementTargetFound = $true }
        }
    }
    if ($script:RetireStaleAcceptanceRunId -and -not $retirementTargetFound) {
        $orphanSessionName = "orch-acc-$($script:RetireStaleAcceptanceRunId)"
        $orphanRoot = Join-Path $env:TEMP "orch-accept-$($script:RetireStaleAcceptanceRunId)"
        if (Test-Path -LiteralPath $orphanRoot) {
            $orphanReportPath = Join-Path $orphanRoot 'report.json'
            if (-not (Test-Path -LiteralPath $orphanReportPath)) {
                throw "refusing orphan cleanup for ${orphanSessionName}: report identity is missing"
            }
            try { $orphanReport = Get-Content -LiteralPath $orphanReportPath -Raw | ConvertFrom-Json } catch {
                throw "refusing orphan cleanup for ${orphanSessionName}: report identity cannot be read"
            }
            $orphanProjectKey = [string](Get-Field $orphanReport 'projectKey')
            [void](Assert-StaleAcceptanceRunRoot -RunId $script:RetireStaleAcceptanceRunId -ProjectKey $orphanProjectKey)
            if ((Get-StaleAcceptanceSessionDisposition -RunId $script:RetireStaleAcceptanceRunId -Sessions @(Get-Sessions) -SessionsRoot $sessionsRoot) -ne 'absent') {
                throw "refusing orphan cleanup for ${orphanSessionName}: a Herdr session is still listed but its project state is missing"
            }
            $orphanDump = Get-StateDump
            $projectRefs = @($orphanDump.tables.projects | Where-Object { Test-SamePath -A ([string](Get-Field $_ 'key')) -B $orphanProjectKey })
            $taskRefs = @($orphanDump.tables.tasks | Where-Object { Test-SamePath -A ([string](Get-Field $_ 'project_key')) -B $orphanProjectKey })
            $inboxRefs = @($orphanDump.tables.inbox | Where-Object { Test-SamePath -A ([string](Get-Field $_ 'project_key')) -B $orphanProjectKey })
            $ticketRefs = @(@(Get-Field $orphanDump.tables 'tickets' @()) | Where-Object { Test-SamePath -A ([string](Get-Field $_ 'project_key')) -B $orphanProjectKey })
            $readyRefs = @($orphanDump.tables.ready_sessions | Where-Object {
                ([regex]::Match([string](Get-Field $_ 'omp_session_file'), '(?i)orch-accept-([0-9a-f]{8})').Groups[1].Value.ToLowerInvariant()) -ceq $script:RetireStaleAcceptanceRunId
            })
            if ($projectRefs.Count -or $taskRefs.Count -or $inboxRefs.Count -or $ticketRefs.Count -or $readyRefs.Count) {
                throw "refusing orphan cleanup for ${orphanSessionName}: state rows still reference its project or OMP sessions"
            }
            $default = Get-HerdrJson -Arguments @('api', 'snapshot') -NoSessionScope
            $defaultProjection = Get-FocusProjection -Snapshot (Get-JsonField $default 'snapshot')
            if (Test-ProjectionMentions -Projection $defaultProjection -Path $orphanRoot) {
                throw "refusing orphan cleanup for ${orphanSessionName}: the default session still references $orphanRoot"
            }
            $orphanKeepRequested = Test-JsonBool -Object $orphanReport -Name 'keepResourcesRequested'
            $orphanResourcesPreserved = Test-JsonBool -Object $orphanReport -Name 'resourcesPreserved'
            $orphanKeepLabel = if ($null -eq $orphanKeepRequested) { 'unknown/not recorded' } else { [string]$orphanKeepRequested }
            $orphanPreservedLabel = if ($null -eq $orphanResourcesPreserved) { 'unknown/not recorded' } else { [string]$orphanResourcesPreserved }
            Write-Host ("Verified orphan acceptance run {0}: session=absent report passed={1} failed={2} blocked={3} prior-KeepResources={4} resourcesPreserved={5}; exact run-id opt-in supplied" -f `
                $orphanSessionName, (Get-Field $orphanReport 'passed'), (Get-Field $orphanReport 'failed'), (Get-Field $orphanReport 'blocked'), $orphanKeepLabel, $orphanPreservedLabel) -ForegroundColor Yellow
            Remove-StaleAcceptanceRunRoot -RunId $script:RetireStaleAcceptanceRunId -ProjectKey $orphanProjectKey
            Add-Assertion -Id "P1.12c-$($script:RetireStaleAcceptanceRunId)" -What 'the explicitly selected, report-verified orphan acceptance root was removed without touching siblings' -Status 'INFO' `
                -Evidence $orphanRoot
            $retirementTargetFound = $true
        }
        if (-not $retirementTargetFound) {
            throw "no verified stale acceptance resources were found for RetireStaleAcceptanceRunId $($script:RetireStaleAcceptanceRunId)"
        }
    }
    $legacyRunRoots = @(Get-ChildItem -LiteralPath $env:TEMP -Directory -Filter 'orch-accept-*' -Force -ErrorAction SilentlyContinue)
    $legacyCandidates = New-Object System.Collections.ArrayList
    foreach ($legacyRoot in $legacyRunRoots) {
        $legacyMatch = [regex]::Match($legacyRoot.Name, '^orch-accept-([0-9a-fA-F]{8})$')
        if (-not $legacyMatch.Success) { continue }
        $legacyRunId = $legacyMatch.Groups[1].Value.ToLowerInvariant()
        if ($legacyRunId -ceq $script:RunId -or $staleRunSet.ContainsKey($legacyRunId)) { continue }
        $legacyManifestPath = Join-Path (Join-Path $script:RunArchiveRoot $legacyRunId) 'manifest.json'
        if (Test-Path -LiteralPath $legacyManifestPath -PathType Leaf) { continue }
        $legacyReportPath = Join-Path $legacyRoot.FullName 'report.json'
        if (-not (Test-Path -LiteralPath $legacyReportPath -PathType Leaf)) { continue }
        try { $legacyReport = Get-Content -LiteralPath $legacyReportPath -Raw | ConvertFrom-Json } catch { continue }
        if ((Get-Field $legacyReport 'runId') -cne $legacyRunId -or
            (Get-Field $legacyReport 'session') -cne "orch-acc-$legacyRunId" -or
            (Test-JsonBool -Object $legacyReport -Name 'keepResourcesRequested') -ne $false -or
            (Test-JsonBool -Object $legacyReport -Name 'resourcesPreserved') -ne $false) { continue }
        $legacyEntries = @(Get-ChildItem -LiteralPath $legacyRoot.FullName -Force)
        if (@($legacyEntries | Where-Object { $_.Name -notin @('report.json', 'report.txt') -or $_.PSIsContainer }).Count -gt 0) { continue }
        [void]$legacyCandidates.Add([pscustomobject]@{ RunId = $legacyRunId; Root = $legacyRoot.FullName })
    }
    if ($legacyCandidates.Count -gt 0) {
        $defaultSnapshot = Get-HerdrJson -Arguments @('api', 'snapshot') -NoSessionScope
        $defaultProjection = Get-FocusProjection -Snapshot (Get-JsonField $defaultSnapshot 'snapshot')
        foreach ($candidate in $legacyCandidates) {
            if (Move-LegacyAcceptanceReportRootToArchive -RunId $candidate.RunId -TempRoot $env:TEMP `
                -ArchiveRoot $script:RunArchiveRoot -StateDump $dump -Sessions $sessions -DefaultProjection $defaultProjection) {
                Add-Assertion -Id "P1.12d-$($candidate.RunId)" -What 'report-only legacy temp root was archived and removed without touching live/session or state resources' -Status 'INFO' `
                    -Evidence $candidate.Root
            }
        }
    }

    $dump = Get-StateDump
    $boundPanes = @{}
    foreach ($row in @($dump.tables.projects) + @($dump.tables.tasks)) {
        $fact = Get-Field $row 'fact'
        foreach ($field in @('LaunchPaneID', 'PublicPaneID')) {
            $pane = [string](Get-Field $fact $field)
            if ($pane) { $boundPanes[$pane] = $true }
        }
    }
    $orphanReady = @()
    foreach ($row in @($dump.tables.ready_sessions)) {
        $sessionFile = [string](Get-Field $row 'omp_session_file')
        $match = [regex]::Match($sessionFile, '(?i)orch-accept-([0-9a-f]{8})')
        if (-not $match.Success) { continue }
        $runId = $match.Groups[1].Value.ToLowerInvariant()
        $sessionName = "orch-acc-$runId"
        if (@($sessions | Where-Object { (Get-Field $_ 'name') -eq $sessionName }).Count -gt 0) {
            throw "refusing to clean acceptance ready-session row while session $sessionName is still listed"
        }
        $pane = [string](Get-Field $row 'launch_pane_id')
        if ($boundPanes.ContainsKey($pane)) {
            throw "refusing to clean orphan acceptance ready-session row: pane $pane still has a project/task binding"
        }
        $orphanReady += $row
    }
    if ($orphanReady.Count -gt 0) {
        $paneIds = @($orphanReady | ForEach-Object { [string](Get-Field $_ 'launch_pane_id') } | Select-Object -Unique)
        $plan = Invoke-State -Operation 'plan-scoped' -Arguments @('__stale_acceptance_ready__', ($paneIds -join ','))
        if (@($plan.foreign).Count -gt 0 -or [int](Get-Field $plan.counts 'ready_sessions') -ne $orphanReady.Count) {
            throw 'orphan acceptance ready-session cleanup plan did not match the identified rows'
        }
        $deleted = Invoke-State -Operation 'delete-scoped' -Arguments @('__stale_acceptance_ready__', ($paneIds -join ','))
        if (-not (Get-Flag $deleted 'ok') -or -not (Get-Flag $deleted 'others_identical')) {
            throw 'orphan acceptance ready-session cleanup did not preserve unrelated state'
        }
        Add-Assertion -Id 'P1.12b' -What 'orphan acceptance ready-session rows removed without changing unrelated rows' -Status 'INFO' `
            -Evidence (Get-Json $deleted.deleted)
    }
}


function Get-ProjectTasks {
    param([string]$Key)
    $dump = Get-StateDump
    return @($dump.tables.tasks | Where-Object { $_.project_key -eq $Key })
}

function Get-TaskById {
    param([string]$Id)
    $dump = Get-StateDump
    $row = @($dump.tables.tasks | Where-Object { $_.id -eq $Id })
    if ($row.Count -eq 0) { return $null }
    return $row[0]
}

function Get-ProjectTickets {
    param([string]$Key)
    $dump = Get-StateDump
    return @($dump.tables.tickets | Where-Object { $_.project_key -eq $Key })
}

function Get-TicketById {
    param([string]$Id)
    $dump = Get-StateDump
    $row = @($dump.tables.tickets | Where-Object { $_.id -eq $Id })
    if ($row.Count -eq 0) { return $null }
    return $row[0]
}

function Get-ReadySession {
    param([string]$LaunchPaneId)
    $dump = Get-StateDump
    $row = @($dump.tables.ready_sessions | Where-Object { $_.launch_pane_id -eq $LaunchPaneId })
    if ($row.Count -eq 0) { return $null }
    return $row[0]
}

function Get-TaskStage {
    param($Fact)
    if (-not $Fact) { return 'unknown' }
    if ((Get-Field $Fact 'Disposition') -ne '') { return 'done' }
    if ((Get-Field $Fact 'Blocker') -ne '' -or (Get-Field $Fact 'Attention') -ne '') { return 'needs_input' }
    $report = Get-Field $Fact 'Report' $null
    if ($null -ne $report -and ([string](Get-Field $report 'Kind')) -eq 'result') { return 'ready_for_review' }
    if ((Get-Field $Fact 'Phase') -in @('worktree_created', 'agent_started', 'prompt_submitted')) { return 'running' }
    return 'queued'
}

function Get-TaskProjection {
    <#
        A comparable projection of one task fact. Two projections are used:
        the full one (everything a refusal must not touch or change) and the
        move-insensitive one (only the fields a cross-workspace move and the
        worker's own activity are allowed to leave alone).
    #>
    param($Fact, [switch]$MoveSensitive, [switch]$CleanupInvariant)
    $fields = if ($CleanupInvariant) {
        # Exactly what a removal must leave untouched, and nothing a lifecycle
        # hook may independently refresh (Activity/ActivitySeenAt/Revision).
        @('Branch', 'WorktreePath', 'Report', 'Disposition', 'LaunchPaneID', 'PublicPaneID',
          'TerminalID', 'CurrentWorkspaceID', 'WorktreeWorkspaceID', 'AgentName', 'OMPSessionID')
    } elseif ($MoveSensitive) {
        @('ID', 'ProjectKey', 'Title', 'Brief', 'BaseRevision', 'Branch', 'Checks', 'Dependencies',
          'ReservedPaths', 'WorktreePath', 'WorktreeWorkspaceID', 'TerminalID', 'LaunchPaneID',
          'AgentName', 'OMPSessionID', 'CreatedAt')
    } else {
        @('ID', 'ProjectKey', 'Title', 'Brief', 'BaseRevision', 'Branch', 'Checks', 'Dependencies',
          'ReservedPaths', 'WorktreePath', 'WorktreeWorkspaceID', 'TerminalID', 'LaunchPaneID',
          'PublicPaneID', 'CurrentWorkspaceID', 'AgentName', 'OMPSessionID', 'Phase', 'Report',
          'Blocker', 'Attention', 'Disposition')
    }
    $parts = New-Object System.Collections.ArrayList
    foreach ($field in $fields) {
        $value = Get-Field $Fact $field
        if ($value -is [string] -or $null -eq $value) {
            [void]$parts.Add("$field=[$value]")
        } else {
            [void]$parts.Add("$field=$(Get-Json $value)")
        }
    }
    return ($parts -join ' ')

}

function Test-HandshakeMaterialized {
    <#
        Whether the session a handshake row announces is on disk. Herdr reports
        the native reference as a typed pair: a `path` reference is checkable
        from here, an `id`-only announcement is not, and that difference is
        returned as $null instead of a guessed false.
    #>
    param($Handshake)
    if ($null -eq $Handshake) { return $null }
    $file = [string](Get-Field $Handshake 'omp_session_file')
    if ([string]::IsNullOrWhiteSpace($file)) { return $null }
    if ($file -match '\.jsonl$' -or $file -match '[\\/]') { return (Test-Path -LiteralPath $file) }
    return $null
}

function Test-SessionIdentity {
    <#
        Links the native forms of one OMP session. Herdr publishes a TYPED
        reference (kind id or path), the bind answer carries Herdr reference
        value, and the store/announcement carries whichever native identity the
        runtime exposed - so a raw string compare between two of them is wrong.
        Two values name the same session when they are equal in one form, or when
        both appear among the announced identities of the same handshake row.
    #>
    param([string]$Left, [string]$Right, $Ready)
    if ([string]::IsNullOrWhiteSpace($Left) -or [string]::IsNullOrWhiteSpace($Right)) { return $false }
    if ($Left -ieq $Right) { return $true }
    if (Test-SamePath -A $Left -B $Right) { return $true }
    $announced = @()
    foreach ($field in @('omp_session_id', 'omp_session_file', 'verified_value')) {
        $value = [string](Get-Field $Ready $field)
        if (-not [string]::IsNullOrWhiteSpace($value)) { $announced += $value }
    }
    if ($announced.Count -eq 0) { return $false }
    $leftSeen = $false
    $rightSeen = $false
    foreach ($candidate in $announced) {
        foreach ($value in @($Left, $Right)) {
            $same = ($candidate -ieq $value)
            if (-not $same -and ($candidate -match '[\\/]' -or $value -match '[\\/]')) {
                $same = Test-SamePath -A $candidate -B $value
            }
            if ($same) {
                if ($value -ceq $Left) { $leftSeen = $true } else { $rightSeen = $true }
            }
        }
    }
    return ($leftSeen -and $rightSeen)
}

# ---------------------------------------------------------------------------
# OMP model pinning
# ---------------------------------------------------------------------------
# Herdr starts every OMP agent (the primary and each worker) from the isolated
# server this script launches, and those agents inherit the server's environment.
# -Model is therefore pinned there, as a one-file OMP config overlay named by
# PI_CONFIG_FILES. The overlay outranks the user's global and project settings for
# these processes only; nothing under the user's OMP profile is written.

function Get-OmpModelOverlayText {
    param([Parameter(Mandatory)][string]$Model)
    # Checked again here because the value is written into a YAML file.
    if ($Model -notmatch $script:OmpModelPattern) {
        throw "'$Model' is not an OMP model spelled provider/model[:thinking]"
    }
    return "modelRoles:`n  default: `"$Model`"`n"
}

function Join-OmpConfigFiles {
    <#
        PI_CONFIG_FILES is a platform path list: OMP loads its files in order and a
        later file wins. The overlay goes last so it outranks anything the invoking
        user already lists there, and it appears once even if they listed it.
    #>
    param([string]$Existing, [Parameter(Mandatory)][string]$Overlay)
    $separator = [string][System.IO.Path]::PathSeparator
    $files = New-Object System.Collections.ArrayList
    foreach ($entry in @($Existing -split [regex]::Escape($separator))) {
        if ([string]::IsNullOrWhiteSpace($entry) -or $entry -ieq $Overlay) { continue }
        if ($files -notcontains $entry) { [void]$files.Add($entry) }
    }
    [void]$files.Add($Overlay)
    return ($files -join $separator)
}

function Get-OmpConfigFilesValue {
    return (Join-OmpConfigFiles -Existing ([string]$env:PI_CONFIG_FILES) -Overlay $script:OmpModelOverlayPath)
}

function Get-OmpTranscriptModels {
    <#
        The distinct provider/model pairs that produced the assistant turns of one
        OMP transcript, in first-seen order. Every assistant message records the
        provider and model that generated it, so the transcript is the record of
        what a session ran on (the config overlay is only a request). Unreadable or
        half-written lines are skipped; a missing file has no models.
    #>
    param([string]$Path)
    $models = New-Object System.Collections.ArrayList
    if ([string]::IsNullOrWhiteSpace($Path) -or -not (Test-Path -LiteralPath $Path -PathType Leaf)) { return @() }
    $stream = $null
    $reader = $null
    try {
        $stream = [System.IO.File]::Open($Path, [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read, [System.IO.FileShare]::ReadWrite)
        $reader = New-Object System.IO.StreamReader($stream)
        while ($null -ne ($line = $reader.ReadLine())) {
            try { $entry = ConvertFrom-Json -InputObject $line -ErrorAction Stop } catch { continue }
            if ([string](Get-Field $entry 'type') -cne 'message') { continue }
            $message = Get-Field $entry 'message'
            if ([string](Get-Field $message 'role') -cne 'assistant') { continue }
            $provider = [string](Get-Field $message 'provider')
            $model = [string](Get-Field $message 'model')
            if ($provider -eq '' -or $model -eq '') { continue }
            $pair = "$provider/$model"
            if ($models -notcontains $pair) { [void]$models.Add($pair) }
        }
    } catch {
        # A transcript that cannot be read proves nothing beyond what was seen.
    } finally {
        if ($reader) { $reader.Dispose() }
        elseif ($stream) { $stream.Dispose() }
    }
    return @($models)
}

function Test-OmpTranscriptModels {
    <#
        True when the transcript holds at least one assistant turn and every turn
        was produced by the pinned model. The :thinking suffix is a request setting,
        not part of the model id a turn records.
    #>
    param([string]$Path, [Parameter(Mandatory)][string]$Model)
    $expected = $Model -replace ':(off|minimal|low|medium|high|xhigh|max|auto)\z', ''
    $seen = @(Get-OmpTranscriptModels -Path $Path)
    if ($seen.Count -eq 0) { return $false }
    return (@($seen | Where-Object { $_ -ne $expected }).Count -eq 0)
}

function Assert-OmpSessionModel {
    <#
        Evidence that one session really ran on the pinned model, read from the
        transcript its readiness handshake announced. With a model pinned this fails
        closed: no handshake, an id-only reference whose transcript cannot be read
        from here, an unreadable transcript, or any turn on another model is a FAIL,
        never a pass on faith. With no model pinned, the model OMP chose is recorded
        as INFO.
    #>
    param(
        [Parameter(Mandatory)][string]$Id,
        [Parameter(Mandatory)][string]$What,
        [Parameter(Mandatory)][string]$LaunchPaneId,
        [string]$TaskId = ''
    )
    $handshake = Get-ReadySession -LaunchPaneId $LaunchPaneId
    $path = [string](Get-Field $handshake 'omp_session_file')
    if ($path -notmatch '\.jsonl$' -and $path -notmatch '[\\/]') {
        $evidence = "launch pane=[$LaunchPaneId] handshake=[$(if ($null -eq $handshake) { 'none' } else { 'present' })] announced file=[$path]"
        if ($script:OmpModel) {
            [void](Assert-True -Id $Id -What $What -Condition $false -TaskId $TaskId -Evidence "$evidence (no readable transcript path, so the requested model cannot be proven)")
        } else {
            [void](Add-Assertion -Id $Id -What "$What (no model pinned; the session reference has no readable transcript path)" -Status 'INFO' -TaskId $TaskId -Evidence $evidence)
        }
        return
    }
    # The transcript is flushed as turns finish, so give the last one a moment.
    [void](Wait-Until -TimeoutSeconds 30 -IntervalSeconds 3 -What "an assistant turn in $path" -Probe {
        $now = @(Get-OmpTranscriptModels -Path $path)
        if ($now.Count -gt 0) { return ($now -join ', ') }
        return $null
    })
    $seen = @(Get-OmpTranscriptModels -Path $path)
    $evidence = "pinned=[$($script:OmpModel)] seen=[$($seen -join ', ')] file=[$path]"
    if (-not $script:OmpModel) {
        [void](Add-Assertion -Id $Id -What "$What (no model pinned; recording the model OMP chose)" -Status 'INFO' -TaskId $TaskId -Evidence $evidence)
        return
    }
    [void](Assert-True -Id $Id -What $What -Condition (Test-OmpTranscriptModels -Path $path -Model $script:OmpModel) -TaskId $TaskId -Evidence $evidence)
}

function Test-OmpSessionMaterialized {
    # A path-shaped session reference is materialized only when the file exists;
    # an id-shaped reference cannot be proven from here (the store records the
    # on-disk flag the runtime announced).
    param([string]$Reference)
    if ([string]::IsNullOrWhiteSpace($Reference)) { return $false }
    if ($Reference -match '\.jsonl$' -or $Reference -match '[\\/]') { return (Test-Path -LiteralPath $Reference) }
    return $false
}
function Test-OmpTranscriptContains {
    param([string]$Path, [string]$Text)
    if ([string]::IsNullOrWhiteSpace($Path) -or [string]::IsNullOrWhiteSpace($Text) -or
        -not (Test-Path -LiteralPath $Path -PathType Leaf)) { return $false }
    $stream = $null
    $reader = $null
    try {
        $stream = [System.IO.File]::Open($Path, [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read, [System.IO.FileShare]::ReadWrite)
        $reader = New-Object System.IO.StreamReader($stream)
        $content = $reader.ReadToEnd()
        return ($content.IndexOf($Text, [System.StringComparison]::OrdinalIgnoreCase) -ge 0)
    } catch {
        return $false
    } finally {
        if ($reader) { $reader.Dispose() }
        elseif ($stream) { $stream.Dispose() }
    }
}
function Test-OmpTranscriptContainsResultNote {
    param([string]$Path, [string]$TaskId, [string]$Summary)
    if ([string]::IsNullOrWhiteSpace($Path) -or [string]::IsNullOrWhiteSpace($TaskId) -or
        [string]::IsNullOrWhiteSpace($Summary) -or -not (Test-Path -LiteralPath $Path -PathType Leaf)) { return $false }
    # Inbox note lines are capped at 400 runes; this prefix survives even for long summaries.
    $summaryEnd = 0
    $summaryRunes = 0
    while ($summaryEnd -lt $Summary.Length -and $summaryRunes -lt 128) {
        if ([char]::IsHighSurrogate($Summary[$summaryEnd]) -and
            $summaryEnd + 1 -lt $Summary.Length -and
            [char]::IsLowSurrogate($Summary[$summaryEnd + 1])) {
            $summaryEnd += 2
        } else {
            $summaryEnd++
        }
        $summaryRunes++
    }
    $marker = $TaskId + ' [result]: ' + $Summary.Substring(0, $summaryEnd)
    $stream = $null
    $reader = $null
    try {
        $stream = [System.IO.File]::Open($Path, [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read, [System.IO.FileShare]::ReadWrite)
        $reader = New-Object System.IO.StreamReader($stream)
        while ($null -ne ($line = $reader.ReadLine())) {
            try { $entry = ConvertFrom-Json -InputObject $line -ErrorAction Stop } catch { continue }
            if ([string](Get-Field $entry 'type') -cne 'message') { continue }
            $message = Get-Field $entry 'message'
            if ([string](Get-Field $message 'role') -cne 'user') { continue }
            $textParts = @()
            foreach ($part in @(Get-Field $message 'content' @())) {
                $partText = Get-Field $part 'text'
                if ($partText -is [string]) { $textParts += $partText }
            }
            $text = $textParts -join "`n"
            if ($text.StartsWith('orchestrator:', [System.StringComparison]::Ordinal) -and
                $text.IndexOf($marker, [System.StringComparison]::Ordinal) -ge 0) { return $true }
        }
    } catch {
        return $false
    } finally {
        if ($reader) { $reader.Dispose() }
        elseif ($stream) { $stream.Dispose() }
    }
    return $false
}
function Test-OmpTranscriptContainsExactUserMessage {
    param([string]$Path, [string]$Text)
    if ([string]::IsNullOrWhiteSpace($Path) -or [string]::IsNullOrEmpty($Text) -or
        -not (Test-Path -LiteralPath $Path -PathType Leaf)) { return $false }
    $stream = $null
    $reader = $null
    try {
        $stream = [System.IO.File]::Open($Path, [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read, [System.IO.FileShare]::ReadWrite)
        $reader = New-Object System.IO.StreamReader($stream)
        while ($null -ne ($line = $reader.ReadLine())) {
            try { $entry = ConvertFrom-Json -InputObject $line -ErrorAction Stop } catch { continue }
            if ([string](Get-Field $entry 'type') -cne 'message') { continue }
            $message = Get-Field $entry 'message'
            if ([string](Get-Field $message 'role') -cne 'user') { continue }
            $textParts = @()
            foreach ($part in @(Get-Field $message 'content' @())) {
                $partText = Get-Field $part 'text'
                if ($partText -is [string]) { $textParts += $partText }
            }
            if (($textParts -join "`n") -ceq $Text) { return $true }
        }
    } catch {
        return $false
    } finally {
        if ($reader) { $reader.Dispose() }
        elseif ($stream) { $stream.Dispose() }
    }
    return $false
}

function Test-OmpTranscriptContainsAnswerToolCall {
    param([string]$Path, [string]$TaskId, [string]$Text)
    if ([string]::IsNullOrWhiteSpace($Path) -or [string]::IsNullOrWhiteSpace($TaskId) -or
        [string]::IsNullOrEmpty($Text) -or -not (Test-Path -LiteralPath $Path -PathType Leaf)) { return $false }
    $stream = $null
    $reader = $null
    try {
        $stream = [System.IO.File]::Open($Path, [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read, [System.IO.FileShare]::ReadWrite)
        $reader = New-Object System.IO.StreamReader($stream)
        while ($null -ne ($line = $reader.ReadLine())) {
            try { $entry = ConvertFrom-Json -InputObject $line -ErrorAction Stop } catch { continue }
            if ([string](Get-Field $entry 'type') -cne 'message') { continue }
            $message = Get-Field $entry 'message'
            if ([string](Get-Field $message 'role') -cne 'assistant') { continue }
            foreach ($part in @(Get-Field $message 'content' @())) {
                $arguments = Get-Field $part 'arguments'
                if ([string](Get-Field $part 'name') -ceq 'orch_answer' -and
                    [string](Get-Field $arguments 'taskId') -ceq $TaskId -and
                    [string](Get-Field $arguments 'text') -ceq $Text) { return $true }
            }
        }
    } catch {
        return $false
    } finally {
        if ($reader) { $reader.Dispose() }
        elseif ($stream) { $stream.Dispose() }
    }
    return $false
}


function Test-OmpTranscriptContainsWorkerQuestionNote {
    param([string]$Path, [string]$TaskId, [string]$QuestionMarker)
    if ([string]::IsNullOrWhiteSpace($Path) -or [string]::IsNullOrWhiteSpace($TaskId) -or
        [string]::IsNullOrWhiteSpace($QuestionMarker) -or -not (Test-Path -LiteralPath $Path -PathType Leaf)) { return $false }
    $stream = $null
    $reader = $null
    $pattern = '(?is)orchestrator:\s*\d+\s+worker note\(s\).*-\s*' + [regex]::Escape($TaskId) + '\s+\[question\]:.*' + [regex]::Escape($QuestionMarker)
    try {
        $stream = [System.IO.File]::Open($Path, [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read, [System.IO.FileShare]::ReadWrite)
        $reader = New-Object System.IO.StreamReader($stream)
        while ($null -ne ($line = $reader.ReadLine())) {
            try { $entry = ConvertFrom-Json -InputObject $line -ErrorAction Stop } catch { continue }
            if ([string](Get-Field $entry 'type') -cne 'message') { continue }
            $message = Get-Field $entry 'message'
            if ([string](Get-Field $message 'role') -cne 'user') { continue }
            $text = (@(Get-Field $message 'content' @()) | ForEach-Object { [string](Get-Field $_ 'text') }) -join "`n"
            if ([regex]::IsMatch($text, $pattern)) { return $true }
        }
    } catch {
        return $false
    } finally {
        if ($reader) { $reader.Dispose() }
        elseif ($stream) { $stream.Dispose() }
    }
    return $false
}

function Test-TaskIsReviewedDone {
    param([object]$Fact)
    $report = Get-Field $Fact 'Report' $null
    if ($null -eq $Fact -or $null -eq $report -or [string](Get-Field $report 'Kind') -cne 'result' -or
        [string]::IsNullOrWhiteSpace([string](Get-Field $Fact 'Disposition'))) { return $false }
    return ((Get-TaskStage -Fact $Fact) -eq 'done')
}

function Test-IsBlockedPrimaryDispositionPrerequisite {
    param([string]$PhaseId, [string]$Message)
    return ($PhaseId -ceq 'K0' -and $Message -ceq 'a primary disposition Ask blocked worker-dialog approval')
}

function Test-OmpTranscriptContainsLaterUserAuthorization {
    param([string]$Path, [string]$TaskId, [string]$QuestionMarker, [string]$Token)
    if ([string]::IsNullOrWhiteSpace($Path) -or [string]::IsNullOrWhiteSpace($TaskId) -or
        [string]::IsNullOrWhiteSpace($QuestionMarker) -or [string]::IsNullOrWhiteSpace($Token) -or
        -not (Test-Path -LiteralPath $Path -PathType Leaf)) { return $false }
    $stream = $null
    $reader = $null
    $questionObserved = $false
    $tokenPattern = '(?<![\p{L}\p{N}_-])' + [regex]::Escape($Token) + '(?![\p{L}\p{N}_-])'
    $questionNotePattern = '(?is)orchestrator:\s*\d+\s+worker note\(s\).*-\s*' + [regex]::Escape($TaskId) + '\s+\[question\]:.*' + [regex]::Escape($QuestionMarker)
    try {
        $stream = [System.IO.File]::Open($Path, [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read, [System.IO.FileShare]::ReadWrite)
        $reader = New-Object System.IO.StreamReader($stream)
        while ($null -ne ($line = $reader.ReadLine())) {
            try { $entry = ConvertFrom-Json -InputObject $line -ErrorAction Stop } catch { continue }
            if ([string](Get-Field $entry 'type') -cne 'message') { continue }
            $message = Get-Field $entry 'message'
            $role = [string](Get-Field $message 'role')
            $content = Get-Field $message 'content'
            $textParts = @()
            foreach ($part in @($content)) {
                $text = Get-Field $part 'text'
                if ($text -is [string]) { $textParts += $text }
            }
            if ($role -cne 'user') { continue }
            $userText = $textParts -join "`n"
            if ([regex]::IsMatch($userText, $questionNotePattern)) {
                $questionObserved = $true
                continue
            }
            $steering = Get-Field $message 'steering' $false
            $liveSteered = Get-Field $message 'liveSteered' $false
            $attribution = [string](Get-Field $message 'attribution')
            $directUserInput = (($steering -is [bool]) -and $steering) -or (($liveSteered -is [bool]) -and $liveSteered)
            if ($questionObserved -and $role -ceq 'user' -and $attribution -ceq 'user' -and $directUserInput -and
                [regex]::IsMatch(($textParts -join "`n"), $tokenPattern, [System.Text.RegularExpressions.RegexOptions]::IgnoreCase)) {
                return $true
            }
        }
    } catch {
        return $false
    } finally {
        if ($reader) { $reader.Dispose() }
        elseif ($stream) { $stream.Dispose() }
    }
    return $false
}
function Test-OmpAnswerCallAfterUserAuthorization {
    param([string]$Path, [string]$QuestionMarker, [string]$Token, [string]$TaskId)
    return ($null -ne (Get-OmpAnswerCallAfterUserAuthorization -Path $Path -QuestionMarker $QuestionMarker -Token $Token -TaskId $TaskId))
}

function Get-OmpAnswerCallAfterUserAuthorization {
    <#
        The exact text of the orch_answer call for this task that relays the direct
        user message carrying the token after the worker's question note, or $null
        when the transcript holds no such call. The answer confirmation dialog must
        show this same text before the acceptance user confirms it.
    #>
    param([string]$Path, [string]$QuestionMarker, [string]$Token, [string]$TaskId)
    if ([string]::IsNullOrWhiteSpace($Path) -or [string]::IsNullOrWhiteSpace($QuestionMarker) -or
        [string]::IsNullOrWhiteSpace($Token) -or [string]::IsNullOrWhiteSpace($TaskId) -or
        -not (Test-Path -LiteralPath $Path -PathType Leaf)) { return $null }
    $stream = $null
    $reader = $null
    $questionObserved = $false
    $authorizedText = $null
    $tokenPattern = '(?<![\p{L}\p{N}_-])' + [regex]::Escape($Token) + '(?![\p{L}\p{N}_-])'
    $questionNotePattern = '(?is)orchestrator:\s*\d+\s+worker note\(s\).*-\s*' + [regex]::Escape($TaskId) + '\s+\[question\]:.*' + [regex]::Escape($QuestionMarker)
    try {
        $stream = [System.IO.File]::Open($Path, [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read, [System.IO.FileShare]::ReadWrite)
        $reader = New-Object System.IO.StreamReader($stream)
        while ($null -ne ($line = $reader.ReadLine())) {
            try { $entry = ConvertFrom-Json -InputObject $line -ErrorAction Stop } catch { continue }
            if ([string](Get-Field $entry 'type') -cne 'message') { continue }
            $message = Get-Field $entry 'message'
            $role = [string](Get-Field $message 'role')
            $content = Get-Field $message 'content'
            $textParts = @()
            foreach ($part in @($content)) {
                $text = Get-Field $part 'text'
                if ($text -is [string]) { $textParts += $text }
            }
            if ($role -ceq 'assistant') {
                foreach ($part in @($content)) {
                    $toolName = [string](Get-Field $part 'name')
                    $arguments = Get-Field $part 'arguments'
                    if ($authorizedText -and $toolName -ceq 'orch_answer' -and
                        [string](Get-Field $arguments 'taskId') -ceq $TaskId -and
                        [string](Get-Field $arguments 'text') -ceq $authorizedText) { return $authorizedText }
                }
                continue
            }
            if ($role -cne 'user') { continue }
            $userText = $textParts -join "`n"
            if ([regex]::IsMatch($userText, $questionNotePattern)) {
                $questionObserved = $true
                continue
            }
            $steering = Get-Field $message 'steering' $false
            $liveSteered = Get-Field $message 'liveSteered' $false
            $attribution = [string](Get-Field $message 'attribution')
            $directUserInput = (($steering -is [bool]) -and $steering) -or (($liveSteered -is [bool]) -and $liveSteered)
            if ($questionObserved -and $role -ceq 'user' -and $attribution -ceq 'user' -and $directUserInput -and
                [regex]::IsMatch($userText, $tokenPattern, [System.Text.RegularExpressions.RegexOptions]::IgnoreCase)) {
                $authorizedText = $userText
            }
        }
    } catch {
        return $null
    } finally {
        if ($reader) { $reader.Dispose() }
        elseif ($stream) { $stream.Dispose() }
    }
    return $null
}

function Test-OmpApprovalToolCall {
    param([string]$Path, [string]$TaskId, [string[]]$Keys)
    if ([string]::IsNullOrWhiteSpace($Path) -or [string]::IsNullOrWhiteSpace($TaskId) -or
        @($Keys).Count -eq 0 -or -not (Test-Path -LiteralPath $Path -PathType Leaf)) { return $false }
    $stream = $null
    $reader = $null
    try {
        $stream = [System.IO.File]::Open($Path, [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read, [System.IO.FileShare]::ReadWrite)
        $reader = New-Object System.IO.StreamReader($stream)
        while ($null -ne ($line = $reader.ReadLine())) {
            if ([string]::IsNullOrWhiteSpace($line)) { continue }
            try { $entry = $line | ConvertFrom-Json -ErrorAction Stop } catch { continue }
            if ([string](Get-Field $entry 'type') -cne 'message') { continue }
            $message = Get-Field $entry 'message'
            if ([string](Get-Field $message 'role') -cne 'assistant') { continue }
            foreach ($part in @(Get-Field $message 'content' @())) {
                if ([string](Get-Field $part 'type') -cne 'toolCall' -or
                    [string](Get-Field $part 'name') -cne 'orch_approve_dialog') { continue }
                $arguments = Get-Field $part 'arguments'
                if ([string](Get-Field $arguments 'taskId') -cne $TaskId) { continue }
                $actualKeys = @(Get-Field $arguments 'keys' @())
                if ($actualKeys.Count -ne $Keys.Count) { continue }
                $sameKeys = $true
                for ($index = 0; $index -lt $Keys.Count; $index++) {
                    if ([string]$actualKeys[$index] -cne [string]$Keys[$index]) { $sameKeys = $false; break }
                }
                if ($sameKeys) { return $true }
            }
        }
    } catch {
        return $false
    } finally {
        if ($reader) { $reader.Dispose() }
        elseif ($stream) { $stream.Dispose() }
    }
    return $false
}
function Test-OmpReviewToolCall {
    param([string]$Path, [string]$TaskId, [string]$Disposition, [string]$Outcome = 'integrated')
    if ([string]::IsNullOrWhiteSpace($Path) -or [string]::IsNullOrWhiteSpace($TaskId) -or
        [string]::IsNullOrWhiteSpace($Disposition) -or [string]::IsNullOrWhiteSpace($Outcome) -or -not (Test-Path -LiteralPath $Path -PathType Leaf)) { return $false }
    $stream = $null
    $reader = $null
    try {
        $stream = [System.IO.File]::Open($Path, [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read, [System.IO.FileShare]::ReadWrite)
        $reader = New-Object System.IO.StreamReader($stream)
        while ($null -ne ($line = $reader.ReadLine())) {
            if ([string]::IsNullOrWhiteSpace($line)) { continue }
            try { $entry = $line | ConvertFrom-Json -ErrorAction Stop } catch { continue }
            if ([string](Get-Field $entry 'type') -cne 'message') { continue }
            $message = Get-Field $entry 'message'
            if ([string](Get-Field $message 'role') -cne 'assistant') { continue }
            foreach ($part in @(Get-Field $message 'content' @())) {
                if ([string](Get-Field $part 'type') -cne 'toolCall' -or
                    [string](Get-Field $part 'name') -cne 'orch_review') { continue }
                $arguments = Get-Field $part 'arguments'
                if ([string](Get-Field $arguments 'taskId') -ceq $TaskId -and
                    [string](Get-Field $arguments 'outcome') -ceq $Outcome -and
                    [string](Get-Field $arguments 'disposition') -ceq $Disposition) { return $true }
            }
        }
    } catch {
        return $false
    } finally {
        if ($reader) { $reader.Dispose() }
        elseif ($stream) { $stream.Dispose() }
    }
    return $false
}
function Test-RestoredReviewRefusal {
    param([string]$Path, [string]$TaskId, [string]$Disposition, [string]$Screen)
    if ($Path -and (Test-Path -LiteralPath $Path -PathType Leaf)) {
        return ((Test-OmpReviewToolCall -Path $Path -TaskId $TaskId -Disposition $Disposition) -and
            (Test-OmpTranscriptContains -Path $Path -Text 'already_decided'))
    }
    # A supported id-only handshake has no JSONL file. In that case the unique
    # attempted decision and the coded refusal must both be visible in this pane.
    return (-not [string]::IsNullOrWhiteSpace($Screen) -and
        $Screen.IndexOf($Disposition, [System.StringComparison]::Ordinal) -ge 0 -and
        $Screen.IndexOf('already_decided', [System.StringComparison]::Ordinal) -ge 0)
}

function Get-OmpTranscriptToolEntries {
    <#
        The tool calls the assistant made in one OMP transcript (its toolCall parts,
        in transcript order) and the ids of the calls whose tool result is recorded.
        Only assistant messages carry calls: a user message or a tool result that
        quotes one is not a call. Half-written lines are skipped. A missing or
        unreadable transcript yields nothing and Readable is false, so a check that
        needs this evidence fails closed.
    #>
    param([string]$Path)
    $entries = [pscustomobject]@{ Readable = $false; Calls = @(); ResultIds = @() }
    if ([string]::IsNullOrWhiteSpace($Path) -or -not (Test-Path -LiteralPath $Path -PathType Leaf)) { return $entries }
    $calls = New-Object System.Collections.ArrayList
    $resultIds = New-Object System.Collections.ArrayList
    $stream = $null
    $reader = $null
    try {
        $stream = [System.IO.File]::Open($Path, [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read, [System.IO.FileShare]::ReadWrite)
        $reader = New-Object System.IO.StreamReader($stream)
        while ($null -ne ($line = $reader.ReadLine())) {
            if ([string]::IsNullOrWhiteSpace($line)) { continue }
            try { $entry = ConvertFrom-Json -InputObject $line -ErrorAction Stop } catch { continue }
            if ([string](Get-Field $entry 'type') -cne 'message') { continue }
            $message = Get-Field $entry 'message' $null
            $role = [string](Get-Field $message 'role')
            if ($role -ceq 'toolResult') {
                $resultId = [string](Get-Field $message 'toolCallId')
                if ($resultId -ne '') { [void]$resultIds.Add($resultId) }
                continue
            }
            if ($role -cne 'assistant') { continue }
            foreach ($part in @(Get-Field $message 'content' @())) {
                if ([string](Get-Field $part 'type') -ceq 'toolCall') { [void]$calls.Add($part) }
            }
        }
    } catch {
        return $entries
    } finally {
        if ($reader) { $reader.Dispose() }
        elseif ($stream) { $stream.Dispose() }
    }
    $entries.Readable = $true
    $entries.Calls = @($calls)
    $entries.ResultIds = @($resultIds)
    return $entries
}

function Get-OmpToolCalls {
    # The arguments of every call the assistant made to one tool, in transcript
    # order; none for a missing or unreadable transcript.
    param([string]$Path, [Parameter(Mandatory)][string]$Name)
    $found = New-Object System.Collections.ArrayList
    foreach ($call in @((Get-OmpTranscriptToolEntries -Path $Path).Calls)) {
        if ([string](Get-Field $call 'name') -cne $Name) { continue }
        $callArguments = Get-Field $call 'arguments' $null
        if ($null -ne $callArguments) { [void]$found.Add($callArguments) }
    }
    return @($found)
}

function Get-OmpPendingToolCalls {
    <#
        The arguments of the calls to one tool that are still running: the
        assistant made the call and the transcript records no tool result for its
        id. A call without an id cannot be linked to its result, so it is never
        reported as pending.
    #>
    param([string]$Path, [Parameter(Mandatory)][string]$Name)
    $entries = Get-OmpTranscriptToolEntries -Path $Path
    $pending = New-Object System.Collections.ArrayList
    foreach ($call in @($entries.Calls)) {
        $callId = [string](Get-Field $call 'id')
        if ($callId -eq '' -or [string](Get-Field $call 'name') -cne $Name -or @($entries.ResultIds) -ccontains $callId) { continue }
        $callArguments = Get-Field $call 'arguments' $null
        if ($null -ne $callArguments) { [void]$pending.Add($callArguments) }
    }
    return @($pending)
}

function Test-OmpPendingToolCall {
    <#
        True when the transcript holds a pending call of this tool (made, with no
        recorded result) whose arguments carry exactly these values. Other
        arguments, such as the intent OMP adds, are ignored; a missing one reads as
        empty, as the extension's schema defaults it.
    #>
    param([string]$Path, [Parameter(Mandatory)][string]$Name, [Parameter(Mandatory)][hashtable]$Arguments)
    foreach ($call in @(Get-OmpPendingToolCalls -Path $Path -Name $Name)) {
        $same = $true
        foreach ($key in @($Arguments.Keys)) {
            if ([string](Get-Field $call $key) -cne [string]$Arguments[$key]) { $same = $false; break }
        }
        if ($same) { return $true }
    }
    return $false
}

function Select-OmpToolCallNames {
    # The distinct names of transcript tool calls, in first-seen order.
    param([object[]]$Calls = @())
    $names = New-Object System.Collections.ArrayList
    foreach ($call in @($Calls)) {
        $name = [string](Get-Field $call 'name')
        if ($name -ne '' -and -not ($names -ccontains $name)) { [void]$names.Add($name) }
    }
    return @($names)
}

function Get-OmpToolCallNames {
    # The distinct tool names the assistant called, in first-seen order.
    param([string]$Path)
    return @(Select-OmpToolCallNames -Calls @((Get-OmpTranscriptToolEntries -Path $Path).Calls))
}

function Test-OmpToolCallsWithin {
    <#
        Whether one session called only tools of an allowed set, read from its
        transcript. Ok needs a readable transcript with at least one tool call and
        no name outside Allowed: a session whose calls cannot be read, or that
        called nothing, proves nothing. Outside names the tools that broke the rule.
    #>
    param([string]$Path, [string[]]$Allowed = @())
    $entries = Get-OmpTranscriptToolEntries -Path $Path
    $names = @(Select-OmpToolCallNames -Calls @($entries.Calls))
    $outside = @($names | Where-Object { @($Allowed) -cnotcontains $_ })
    return [pscustomobject]@{
        Ok      = [bool]($entries.Readable -and $names.Count -gt 0 -and $outside.Count -eq 0)
        Names   = $names
        Outside = $outside
    }
}





# ---------------------------------------------------------------------------
# the plugin executable's own seams (bridge, plugin logs, actions)
# ---------------------------------------------------------------------------

function Get-PluginExe {
    return (Join-Path $script:RepoRoot 'herdr-omp-orchestrator.exe')
}

function Invoke-PluginAction {
    <#
        Invokes one manifest action through Herdr. When PaneContext is supplied
        the CLI runs with the real pane's inherited Herdr environment, which is
        exactly what Herdr injects into an action invoked from that pane; the
        returned context is asserted so the invocation provably named the
        temporary repository.
    #>
    param(
        [Parameter(Mandatory)][string]$ActionId,
        [hashtable]$PaneContext = @{},
        [double]$TimeoutSeconds = 180,
        [switch]$NoSessionScope
    )
    $argv = @('plugin', 'action', 'invoke', $ActionId, '--plugin', $script:PluginId)
    $invocation = Invoke-Herdr -Arguments $argv -ExtraEnvironment $PaneContext `
        -TimeoutSeconds $TimeoutSeconds -NoSessionScope:$NoSessionScope
    if ($invocation.ExitCode -ne 0) {
        Report-CommandFailure -AssertId "action/$ActionId" -Arguments $argv -Result $invocation
        return $null
    }
    $answer = $null
    try {
        $parsed = $invocation.Stdout | ConvertFrom-Json
        $answer = Get-Field $parsed 'result' $null
    } catch {
        Report-CommandFailure -AssertId "action/$ActionId" -Arguments $argv -Result $invocation
        return $null
    }
    return [pscustomobject]@{ Result = $answer; Invocation = $invocation }
}

function Get-PluginLogEntry {
    param([Parameter(Mandatory)][string]$LogId, [double]$TimeoutSeconds = 60, [switch]$NoSessionScope)
    $wait = Wait-Until -TimeoutSeconds $TimeoutSeconds -IntervalSeconds 1 -What "plugin log $LogId" -Probe {
        $json = Get-HerdrJson -Arguments @('plugin', 'log', 'list', '--plugin', $script:PluginId, '--limit', '40') -NoSessionScope:$NoSessionScope
        $entry = @((Get-JsonField $json 'logs' @()) | Where-Object { (Get-Field $_ 'log_id') -eq $LogId })
        if ($entry.Count -eq 0) { return $null }
        if ($entry[0].status -eq 'running') { return $null }
        return $entry[0]
    }
    return $wait.Value
}

function Get-ActionOutput {
    <#
        Reads the JSON the action's child printed (Herdr records it as the log's
        stdout). This is the plugin's own answer - the same object a human reads
        in the plugin log.
    #>
    param([Parameter(Mandatory)][string]$LogId, [double]$TimeoutSeconds = 60, [switch]$NoSessionScope)
    $entry = Get-PluginLogEntry -LogId $LogId -TimeoutSeconds $TimeoutSeconds -NoSessionScope:$NoSessionScope
    if (-not $entry) { return $null }
    $payload = $null
    if ($entry.stdout) {
        try { $payload = $entry.stdout | ConvertFrom-Json } catch { $payload = $null }
    }
    return [pscustomobject]@{ Entry = $entry; Payload = $payload }
}

function Invoke-BridgeAs {
    <#
        Replays one bridge call with a REAL recorded caller.

        The environment and the announced session identity are the ones the
        running OMP process itself inherited and announced (read back from the
        store's handshake row), and the script asserts beforehand that the pane
        still hosts that exact session. Because this replay scrubs inherited user
        context, it restores this run's socket explicitly for the Herdr client.
        This is the only public seam that can express the negative cases (a
        `partial` report, a cleanup refusal) which the OMP tool schemas themselves
        reject before the plugin is ever called; every positive path in this
        script goes through the real OMP agents.
    #>
    param(
        [Parameter(Mandatory)][string]$LaunchPaneId,
        [Parameter(Mandatory)][string]$Method,
        [hashtable]$Params = @{},
        [switch]$Raw
    )
    $handshake = Get-ReadySession -LaunchPaneId $LaunchPaneId
    if (-not $handshake) {
        return [pscustomobject]@{
            Envelope   = $false
            Ok         = $false
            Result     = $null
            Error      = [pscustomobject]@{ code = 'no_handshake'; message = "no ready_sessions row for launch pane $LaunchPaneId" }
            Raw        = ''
            Invocation = $null
        }
    }
    $body = @{
        version          = 1
        method           = $Method
        ompSessionID     = [string](Get-Field $handshake 'omp_session_id')
        ompSessionFile   = [string](Get-Field $handshake 'omp_session_file')
        ompSessionOnDisk = ([int](Get-Field $handshake 'omp_session_on_disk' 0) -eq 1)
        params           = $Params
    } | ConvertTo-Json -Depth 10 -Compress
    $env = @{
        'HERDR_ENV'                  = '1'
        'HERDR_PANE_ID'              = $LaunchPaneId
        'HERDR_SOCKET_PATH'          = $script:SessionSocket
        'HERDR_PLUGIN_STATE_DIR'     = $script:StateDir
        'HERDR_ORCH_HERDR_BIN_PATH'  = $script:HerdrExe
    }
    $result = Invoke-Native -File (Get-PluginExe) -Arguments @('bridge', $Method) -Environment $env `
        -RemoveEnvironment $script:HerdrCallerEnvKeys `
        -WorkingDirectory $script:RepoRoot -StandardInputText $body -TimeoutSeconds 180
    $answer = $null
    if ($result.Stdout) { try { $answer = $result.Stdout | ConvertFrom-Json } catch { $answer = $null } }
    if (-not $answer) {
        Report-CommandFailure -AssertId "bridge/$Method" -Arguments @('bridge', $Method) -Result $result
        return [pscustomobject]@{
            Envelope   = $false
            Ok         = $false
            Result     = $null
            Error      = [pscustomobject]@{ code = 'bridge_protocol'; message = (Protect-Text $result.Stderr 300) }
            Raw        = [string]$result.Stdout
            Invocation = $result
        }
    }
    # Envelopes are read with safe defaults and validated structurally (one rule,
    # implemented once in Test-BridgeEnvelopeShape so the helper harness can
    # exercise it): parseable JSON that is not a v1 envelope ({} or an unrelated
    # object) must NOT be reported as an envelope, or the assertions below would
    # pass on garbage.
    $isEnvelope = (Test-BridgeEnvelopeShape -Answer $answer)
    $ok = (Get-Flag $answer 'ok')
    $resultField = Get-Field $answer 'result' $null
    $errorField = Get-Field $answer 'error' $null
    if (-not $isEnvelope) {
        return [pscustomobject]@{
            Envelope   = $false
            Ok         = $false
            Result     = $null
            Error      = [pscustomobject]@{ code = 'bridge_protocol'; message = 'the bridge answer is not a v1 envelope' }
            Raw        = [string]$result.Stdout
            Invocation = $result
        }
    }
    return [pscustomobject]@{
        Envelope   = $true
        Ok         = $ok
        Result     = $resultField
        Error      = $errorField
        Raw        = [string]$result.Stdout
        Invocation = $result
    }
}

function Get-LiveAgent {
    <#
        Reads the authoritative live-agent projection for a pane, including the
        native session reference Herdr publishes asynchronously.
    #>
    param([Parameter(Mandatory)][string]$Target)
    $result = Invoke-Herdr -Arguments @('agent', 'get', $Target) -TimeoutSeconds 60
    if ($result.ExitCode -ne 0) {
        $code = ''
        try { $code = (($result.Stderr | ConvertFrom-Json).error.code) } catch { $code = '' }
        return [pscustomobject]@{ Found = $false; Code = $code; Agent = $null; Raw = $result }
    }
    try {
        $parsed = $result.Stdout | ConvertFrom-Json
        return [pscustomobject]@{ Found = $true; Code = ''; Agent = (Get-Field (Get-Field $parsed 'result' $null) 'agent' $null); Raw = $result }
    } catch {
        return [pscustomobject]@{ Found = $false; Code = 'unparsable'; Agent = $null; Raw = $result }
    }
}

function Get-WorktreeList {
    $json = Get-HerdrJson -Arguments @('worktree', 'list', '--cwd', $script:RepoPath)
    return $json.Result
}

function Get-Snapshot {
    $json = Get-HerdrJson -Arguments @('api', 'snapshot')
    return (Get-JsonField $json 'snapshot')
}

function Get-FocusProjection {
    <#
        The identity half of a snapshot: which workspaces, panes and agents exist
        and where they work, ignoring volatile counters. Used to prove the
        default session was never touched and that the isolated session's focus
        was restored.
    #>
    param($Snapshot)
    if (-not $Snapshot) { return 'unavailable' }
    $parts = New-Object System.Collections.ArrayList
    $workspaces = @(Get-Field $Snapshot 'workspaces' @())
    foreach ($w in @($workspaces | Sort-Object { [string](Get-Field $_ 'workspace_id') })) {
        [void]$parts.Add("w:$([string](Get-Field $w 'workspace_id')):$([string](Get-Field $w 'label')):$([string](Get-Field $w 'pane_count' 0))")
    }
    $panes = @(Get-Field $Snapshot 'panes' @())
    foreach ($p in @($panes | Sort-Object { [string](Get-Field $_ 'pane_id') })) {
        [void]$parts.Add("p:$([string](Get-Field $p 'pane_id')):$([string](Get-Field $p 'workspace_id')):$(ConvertTo-NormalPath ([string](Get-Field $p 'cwd'))):$([string](Get-Field $p 'agent'))")
    }
    $agents = @(Get-Field $Snapshot 'agents' @())
    foreach ($a in @($agents | Sort-Object { [string](Get-Field $_ 'pane_id') })) {
        [void]$parts.Add("a:$([string](Get-Field $a 'pane_id')):$([string](Get-Field $a 'agent')):$(ConvertTo-NormalPath ([string](Get-Field $a 'cwd')))")
    }
    # The focused identities are part of the projection: without them a phase could
    # leave focus somewhere else and still compare equal.
    [void]$parts.Add("fw:$([string](Get-Field $Snapshot 'focused_workspace_id'))")
    [void]$parts.Add("fp:$([string](Get-Field $Snapshot 'focused_pane_id'))")
    return ($parts -join ';')
}

# ---------------------------------------------------------------------------
# JSON access and agent helpers
# ---------------------------------------------------------------------------

function Get-Field {
    # StrictMode-safe property read for objects parsed from JSON, plus
    # dictionary keys (the extension constants are read into a hashtable).
    param($Object, [string]$Name, $Default = '')
    if ($null -eq $Object) { return $Default }
    if ($Object -is [System.Collections.IDictionary]) {
        if ($Object.Contains($Name)) {
            $raw = $Object[$Name]
            if ($null -eq $raw) { return $Default }
            return $raw
        }
        return $Default
    }
    $prop = $Object.PSObject.Properties[$Name]
    if (-not $prop) { return $Default }
    $value = $prop.Value
    if ($null -eq $value) { return $Default }
    return $value
}

function Get-Flag {
    param($Object, [string]$Name)
    return ([bool](Get-Field -Object $Object -Name $Name -Default $false))
}

function Get-JsonField {
    # One field of a parsed Herdr envelope (Get-HerdrJson returns {Result,Raw,
    # Invocation}); never raises when the field is absent under Set-StrictMode.
    param($Json, [string]$Name, $Default = $null)
    return (Get-Field (Get-Field $Json 'Result' $null) $Name $Default)
}

function Test-JsonBool {
    <#
        $true/$false only when the named property holds a REAL JSON boolean, and
        $null otherwise (absent, null, a string like "false", a number). Used by
        every assertion that must prove a value is true or false, so a malformed
        answer can never satisfy one through PowerShell's string coercion.
    #>
    param($Object, [string]$Name)
    if ($null -eq $Object) { return $null }
    $prop = $Object.PSObject.Properties[$Name]
    if ($null -eq $prop) { return $null }
    if ($prop.Value -isnot [bool]) { return $null }
    return [bool]$prop.Value
}

function Test-ResumeHintAvailability {
    <#
        Whether a bind hint says exactly one thing: either a resume command, or
        why no resume command can be built. It must never say both and never
        neither.
    #>
    param($Hint)
    $resume = [string](Get-Field $Hint 'resume')
    $unavailable = [string](Get-Field $Hint 'resumeUnavailable')
    return (($resume -ne '') -xor ($unavailable -ne ''))
}

function Test-ResumeHintImplication {
    <#
        The materialization gate as an implication that a malformed flag cannot
        satisfy: the hint must carry a REAL boolean, and then either it is not on
        disk (so it must offer no resume command) or it is on disk and offers one.
        The original condition was `!onDisk -or resume != ''`; this keeps that
        meaning while replacing the coercion.
    #>
    param($Hint)
    $onDisk = Test-JsonBool -Object $Hint -Name 'onDisk'
    if ($null -eq $onDisk) { return $false }
    $resume = [string](Get-Field $Hint 'resume')
    if (-not $onDisk) { return ($resume -eq '') }
    return ($resume -ne '')
}

function Assert-JsonTrue {
    param(
        [Parameter(Mandatory)][string]$Id,
        [Parameter(Mandatory)][string]$What,
        $Object,
        [Parameter(Mandatory)][string]$Name,
        [string]$TaskId = '',
        [string]$Command = '',
        [string]$Evidence = ''
    )
    $value = Test-JsonBool -Object $Object -Name $Name
    $detail = "$Name=[$(Get-Field $Object $Name 'absent')] (a real JSON boolean is required; null, string or number is a failure)"
    if ($Evidence) { $detail += "; $Evidence" }
    return Assert-True -Id $Id -What $What -Condition ($value -eq $true) -TaskId $TaskId -Command $Command -Evidence $detail
}

function Assert-JsonFalse {
    param(
        [Parameter(Mandatory)][string]$Id,
        [Parameter(Mandatory)][string]$What,
        $Object,
        [Parameter(Mandatory)][string]$Name,
        [string]$TaskId = '',
        [string]$Command = '',
        [string]$Evidence = ''
    )
    $value = Test-JsonBool -Object $Object -Name $Name
    $detail = "$Name=[$(Get-Field $Object $Name 'absent')] (a real JSON boolean is required; null, string or number is a failure)"
    if ($Evidence) { $detail += "; $Evidence" }
    return Assert-True -Id $Id -What $What -Condition ($value -eq $false) -TaskId $TaskId -Command $Command -Evidence $detail
}

function Get-Json {
    param($Object)
    return ($Object | ConvertTo-Json -Depth 12 -Compress)
}

function Invoke-AgentPrompt {
    <#
        Submits one prompt through the real agent surface with --wait, so the
        call returns when the agent settles. A timeout here is ambiguous and is
        reported as such by the caller; the script never resubmits blindly.
    #>
    param(
        [Parameter(Mandatory)][string]$PaneId,
        [Parameter(Mandatory)][string]$Text,
        [double]$TimeoutSeconds = 240,
        [switch]$NoWait
    )
    $seconds = ConvertTo-ScaledSeconds $TimeoutSeconds
    $argv = @('agent', 'prompt', $PaneId, $Text)
    if (-not $NoWait) { $argv += @('--wait', '--timeout', [string]([int]($seconds * 1000))) }
    return Invoke-Herdr -Arguments $argv -TimeoutSeconds ($seconds + 30)
}

function Test-ProjectionEqual {
    param([string]$Before, [string]$After)
    return ($Before -ceq $After)
}

function Get-ProjectionLines {
    param([string]$Projection)
    if ([string]::IsNullOrWhiteSpace($Projection)) { return @() }
    return @($Projection -split ';')
}

function Test-ProjectionMentions {
    param([string]$Projection, [string]$Path)
    $needle = ConvertTo-NormalPath $Path
    if ($needle -eq '') { return $false }
    foreach ($line in (Get-ProjectionLines -Projection $Projection)) {
        if ($line -like "*$needle*") { return $true }
    }
    return $false
}

function Get-HerdrErrorCode {
    param($Result)
    foreach ($stream in @($Result.Stderr, $Result.Stdout)) {
        if ([string]::IsNullOrWhiteSpace($stream)) { continue }
        try {
            $parsed = $stream | ConvertFrom-Json
            $code = [string](Get-Field (Get-Field $parsed 'error' $null) 'code')
            if ($code) { return $code }
        } catch { }
    }
    return ''
}

function Invoke-PrimaryPrompt {
    <#
        Submits one prompt to the primary session. The primary is a real agent
        that may legitimately be showing its own UI (a question of its own, or a
        tool approval), so this waits for a settled state first and reports an
        `agent_blocked` refusal instead of typing into a dialog.
    #>
    param(
        [Parameter(Mandatory)][string]$PaneId,
        [Parameter(Mandatory)][string]$Text,
        [double]$TimeoutSeconds = 300
    )
    $idle = Wait-Until -TimeoutSeconds 180 -IntervalSeconds 6 -What 'primary settled' -Probe {
        $live = Get-LiveAgent -Target $PaneId
        if (-not $live.Found) { return $null }
        $status = [string](Get-Field $live.Agent 'agent_status')
        if ($status -eq 'blocked') { return 'blocked' }
        if ($status -in @('idle', 'done')) { return $status }
        return $null
    }
    if (-not $idle.Ok -or $idle.Value -eq 'blocked') {
        $screen = Get-PaneScreen -PaneId $PaneId -Source 'visible' -Lines 60
        $code = if ($idle.Value -eq 'blocked') { 'agent_blocked' } else { 'primary_not_settled' }
        return [pscustomobject]@{
            ExitCode = -2
            Blocked = ($code -eq 'agent_blocked')
            Code = $code
            Submitted = $false
            CommandLine = "herdr agent prompt $PaneId <primary prompt>"
            Stderr = ''
            Stdout = ''
            Screen = $screen
        }
    }
    $result = Invoke-AgentPrompt -PaneId $PaneId -Text $Text -TimeoutSeconds $TimeoutSeconds
    $code = Get-HerdrErrorCode -Result $result
    return [pscustomobject]@{ ExitCode = $result.ExitCode; Blocked = ($code -eq 'agent_blocked'); Code = $code; Submitted = $true; CommandLine = $result.CommandLine; Stderr = $result.Stderr; Stdout = $result.Stdout; Screen = '' }
}

# ---------------------------------------------------------------------------
# Phase 1: preflight
# ---------------------------------------------------------------------------

function Invoke-Preflight {
    Write-Step 'Phase 1: preflight (tooling, integration, plugin registration, store baseline)'

    $herdr = Get-Command herdr -ErrorAction SilentlyContinue
    Assert-True -Id 'P0.1' -What 'the plugin root is the repository that holds this script (manifest + go.mod present)' `
        -Condition ((Test-Path -LiteralPath (Join-Path $script:RepoRoot 'herdr-plugin.toml')) -and (Test-Path -LiteralPath (Join-Path $script:RepoRoot 'go.mod'))) `
        -Evidence $script:RepoRoot
    if ($script:ScriptDir) {
        Assert-True -Id 'P0.2' -What 'the script itself lives in the repository tests directory' `
            -Condition ((ConvertTo-NormalPath $script:ScriptDir) -ieq (ConvertTo-NormalPath (Join-Path $script:RepoRoot 'tests'))) -Evidence $script:ScriptDir
    } else {
        Add-Assertion -Id 'P0.2' -What 'the script location could not be derived (dot-sourced execution)' -Status 'INFO'
    }

    Assert-True -Id 'P1.1' -What 'herdr CLI is on PATH' -Condition ([bool]$herdr) -Evidence "herdr=$(if ($herdr) { $herdr.Source } else { 'not found' })"
    if (-not $herdr) { throw 'herdr is not on PATH; the acceptance run cannot continue' }
    $script:HerdrExe = $herdr.Source

    $version = Invoke-Herdr -Arguments @('--version') -NoSessionScope
    Assert-Equal -Id 'P1.2' -What 'the herdr CLI answers a version' -Expected 0 -Actual $version.ExitCode -Command $version.CommandLine
    Add-Assertion -Id 'P1.3' -What 'reported CLI version' -Status 'INFO' -Evidence (Protect-Text $version.Stdout 200)

    $status = Invoke-Herdr -Arguments @('integration', 'status') -NoSessionScope
    $statusText = [string]$status.Stdout
    Assert-Equal -Id 'P1.4' -What 'herdr integration status exits 0' -Expected 0 -Actual $status.ExitCode -Command $status.CommandLine
    $ompLine = (@($statusText -split "`n") | Where-Object { $_.Trim().StartsWith('omp:') } | Select-Object -First 1)
    Assert-True -Id 'P1.5' -What 'integration status reports an omp line' -Condition (-not [string]::IsNullOrWhiteSpace($ompLine)) -Evidence (Protect-Text $statusText 400)
    Assert-True -Id 'P1.6' -What 'the Herdr OMP integration is installed (not "not installed")' `
        -Condition ((-not [string]::IsNullOrWhiteSpace($ompLine)) -and ($ompLine -notmatch '(?i)not installed')) `
        -Evidence (Protect-Text $ompLine 200)

    $pluginList = Get-HerdrJson -Arguments @('plugin', 'list', '--json', '--plugin', $script:PluginId) -NoSessionScope
    $plugin = @((Get-JsonField $pluginList 'plugins' @()) | Where-Object { (Get-Field $_ 'plugin_id') -eq $script:PluginId })
    Assert-Equal -Id 'P1.7' -What 'the plugin is registered before the run' -Expected 1 -Actual $plugin.Count
    if ($plugin.Count -eq 0) { throw 'the plugin is not linked; run this acceptance after `herdr plugin link` (the run never links a plugin it did not find)' }
    $script:PluginRegistrationBefore = [pscustomobject]@{
        Root     = [string](Get-Field $plugin[0] 'plugin_root')
        Manifest = [string](Get-Field $plugin[0] 'manifest_path')
        Enabled  = (Get-Flag $plugin[0] 'enabled')
    }
    Assert-SamePath -Id 'P1.8' -What 'the linked plugin root is this repository' -Expected $script:RepoRoot -Actual $script:PluginRegistrationBefore.Root
    Add-Assertion -Id 'P1.9' -What 'plugin registration captured before the run' -Status 'INFO' -Evidence (Get-Json $script:PluginRegistrationBefore)

    $configDir = (Invoke-Herdr -Arguments @('plugin', 'config-dir', $script:PluginId) -NoSessionScope).Stdout.Trim()
    Add-Assertion -Id 'P1.9b' -What 'plugin config directory (Herdr keeps config under Roaming, state under Local)' -Status 'INFO' -Evidence $configDir
    $candidateStateDir = Join-Path (Join-Path $env:LOCALAPPDATA 'herdr\plugins') $script:PluginId

    $extDir = Get-EffectiveExtensionDir
    $script:ExtensionPath = Join-Path $extDir 'herdr-orchestrator.ts'
    $integrationFile = Join-Path $extDir 'herdr-omp-agent-state.ts'
    $script:IntegrationFileHashBefore = Get-FileHashSafe $integrationFile
    $script:ExtensionFileHashBefore = Get-FileHashSafe $script:ExtensionPath
    Assert-True -Id 'P1.13' -What "Herdr's own OMP integration exists in the effective profile dir" -Condition (Test-Path -LiteralPath $integrationFile) -Evidence $integrationFile
    Assert-True -Id 'P1.14' -What 'the orchestrator extension is installed in the effective profile dir' -Condition (Test-Path -LiteralPath $script:ExtensionPath) -Evidence $script:ExtensionPath

    # The authoritative state directory is the one the installed extension
    # embeds (setup wrote it from HERDR_PLUGIN_STATE_DIR); Herdr's own
    # convention is asserted against it rather than assumed.
    $script:StateDir = $candidateStateDir
    if (Test-Path -LiteralPath $script:ExtensionPath) {
        $constants = Get-ExtensionConstants -Path $script:ExtensionPath
        $embedded = [string](Get-Field $constants 'PLUGIN_STATE_DIR')
        if ($embedded) {
            $script:StateDir = $embedded
            Assert-SamePath -Id 'P1.10a' -What "the state directory the plugin embeds is Herdr's per-plugin state directory" -Expected $candidateStateDir -Actual $embedded
        }
    }
    $script:StateDb = Join-Path $script:StateDir 'orchestrator.sqlite'
    $lockedStoreMatches = Test-SamePath -A $script:RunLockStateDb -B $script:StateDb
    Assert-True -Id 'P1.10b' -What 'preflight uses the same shared store path that the run lock protects' `
        -Condition $lockedStoreMatches -Evidence "locked=[$($script:RunLockStateDb)] resolved=[$($script:StateDb)]"
    if (-not $lockedStoreMatches) {
        throw 'preflight resolved a different shared state store than the run lock; refusing shared-state cleanup'
    }
    Assert-True -Id 'P1.10' -What 'the plugin state store exists (the plugin has run before)' -Condition (Test-Path -LiteralPath $script:StateDb) `
        -Evidence "state db expected at $($script:StateDb)"

    $script:Python = Initialize-StateTools
    Assert-True -Id 'P1.11' -What 'a SQLite-capable interpreter is available for store assertions' -Condition ([bool]$script:Python) `
        -Evidence "python=$(if ($script:Python) { $script:Python } else { 'not found' })"
    if ($script:Python) {
        Clear-StaleAcceptanceState
        $baseline = Get-StateDump
        Add-Assertion -Id 'P1.12' -What 'store baseline captured after scoped stale acceptance cleanup' -Status 'INFO' `
            -Evidence ("projects={0} tasks={1} inbox={2} ready_sessions={3}" -f @($baseline.tables.projects).Count, @($baseline.tables.tasks).Count, @($baseline.tables.inbox).Count, @($baseline.tables.ready_sessions).Count)
    }

    $defaultSession = Get-HerdrJson -Arguments @('api', 'snapshot') -NoSessionScope
    $script:DefaultSessionBefore = Get-FocusProjection -Snapshot (Get-JsonField $defaultSession 'snapshot')
    Add-Assertion -Id 'P1.15' -What 'default session projection captured (isolation context)' -Status 'INFO' -Evidence (Protect-Text $script:DefaultSessionBefore 600)
    $defaultRecord = @(Get-Sessions | Where-Object { (Get-Field $_ 'name') -eq 'default' })
    Assert-Equal -Id 'P1.15b' -What "the user's default session was found before the run" -Expected 1 -Actual $defaultRecord.Count
    if ($defaultRecord.Count -gt 0) {
        $script:DefaultSessionRecord = [pscustomobject]@{
            Name    = [string](Get-Field $defaultRecord[0] 'name')
            Dir     = [string](Get-Field $defaultRecord[0] 'session_dir')
            Socket  = [string](Get-Field $defaultRecord[0] 'socket_path')
            Running = (Get-Flag $defaultRecord[0] 'running')
        }
        Add-Assertion -Id 'P1.15c' -What 'default session identity captured' -Status 'INFO' -Evidence (Get-Json $script:DefaultSessionRecord)
    }

    $goExe = $null
    $goCmd = Get-Command go -ErrorAction SilentlyContinue
    if ($goCmd) {
        $goExe = $goCmd.Source
    } else {
        $candidate = Join-Path $env:LOCALAPPDATA 'tools\go\bin\go.exe'
        if (Test-Path -LiteralPath $candidate) { $goExe = $candidate }
    }
    Assert-True -Id 'P1.16' -What 'a Go toolchain is available' -Condition ([bool]$goExe) -Evidence "go=$(if ($goExe) { $goExe } else { 'not found' })"
    if (-not $goExe) { throw 'no Go toolchain found; the acceptance build step cannot run' }
    $script:GoExe = $goExe
}

# ---------------------------------------------------------------------------
# Phase 2: build
# ---------------------------------------------------------------------------

function Invoke-Build {
    Write-Step 'Phase 2: CGO_ENABLED=0 build'
    $exe = Get-PluginExe
    if (Test-Path -LiteralPath $exe) { Remove-Item -LiteralPath $exe -Force }
    $started = Get-Date
    $build = Invoke-Native -File $script:GoExe -Arguments @('build', '-o', 'herdr-omp-orchestrator.exe', './cmd/herdr-omp-orchestrator') `
        -Environment @{ 'CGO_ENABLED' = '0' } -WorkingDirectory $script:RepoRoot -TimeoutSeconds 600
    Add-Assertion -Id 'B1.1' -What 'go build exits 0 with CGO_ENABLED=0' -Status $(if ($build.ExitCode -eq 0) { 'PASS' } else { 'FAIL' }) `
        -Command "CGO_ENABLED=0 go build -o herdr-omp-orchestrator.exe ./cmd/herdr-omp-orchestrator" `
        -Evidence ("exit={0} stdout={1} stderr={2}" -f $build.ExitCode, (Protect-Text $build.Stdout 500), (Protect-Text $build.Stderr 800))
    if ($build.ExitCode -ne 0) { Report-CommandFailure -AssertId 'B1.1' -Arguments @('build') -Result $build; throw 'the build failed; nothing else can be proven' }
    $fresh = Test-Path -LiteralPath $exe
    Assert-True -Id 'B1.2' -What 'the plugin executable exists after the build' -Condition $fresh -Evidence $exe
    $size = if ($fresh) { (Get-Item -LiteralPath $exe).Length } else { 0 }
    Assert-True -Id 'B1.3' -What 'the plugin executable is a real binary' -Condition ($size -gt 1MB) -Evidence "size=$size"
    $stamp = if ($fresh) { (Get-Item -LiteralPath $exe).LastWriteTime } else { $started }
    Assert-True -Id 'B1.4' -What 'the executable was rebuilt by this run' -Condition ($stamp -ge $started.AddSeconds(-5)) -Evidence "stamp=$stamp started=$started"
    Add-Assertion -Id 'B1.5' -What 'built executable path (what the linked manifest runs)' -Status 'INFO' -Evidence $exe
}

# ---------------------------------------------------------------------------
# Phase 3: plugin registration is healthy
# ---------------------------------------------------------------------------

function Invoke-Link {
    Write-Step 'Phase 3: plugin link/registration is healthy (never unlinks)'
    $link = Invoke-Herdr -Arguments @('plugin', 'link', $script:RepoRoot) -NoSessionScope -TimeoutSeconds 120
    Assert-Equal -Id 'L1.1' -What 'herdr plugin link <repository> exits 0' -Expected 0 -Actual $link.ExitCode -Command $link.CommandLine
    if ($link.ExitCode -ne 0) { Report-CommandFailure -AssertId 'L1.1' -Arguments @('plugin', 'link') -Result $link }

    $json = Get-HerdrJson -Arguments @('plugin', 'list', '--json', '--plugin', $script:PluginId) -NoSessionScope
    $plugin = @((Get-JsonField $json 'plugins' @()) | Where-Object { (Get-Field $_ 'plugin_id') -eq $script:PluginId })
    Assert-Equal -Id 'L1.2' -What 'the plugin is registered after linking' -Expected 1 -Actual $plugin.Count
    if ($plugin.Count -gt 0) {
        Assert-SamePath -Id 'L1.3' -What 'plugin_root is unchanged' -Expected $script:PluginRegistrationBefore.Root -Actual (Get-Field $plugin[0] 'plugin_root')
        Assert-SamePath -Id 'L1.4' -What 'manifest_path is unchanged' -Expected $script:PluginRegistrationBefore.Manifest -Actual (Get-Field $plugin[0] 'manifest_path')
        Assert-Equal -Id 'L1.5' -What 'the plugin is still enabled' -Expected $script:PluginRegistrationBefore.Enabled -Actual (Get-Flag $plugin[0] 'enabled')
        $warnings = Get-Field $plugin[0] 'warnings' @()
        Assert-Equal -Id 'L1.6' -What 'the manifest links with no warnings (no unrecognized event or field)' -Expected 0 -Actual @($warnings).Count -Evidence (Protect-Text (Get-Json $warnings) 400)
        $panes = Get-Field $plugin[0] 'panes' @()
        Assert-Equal -Id 'L1.7' -What 'the manifest declares the board pane' -Expected 1 -Actual @($panes | Where-Object { (Get-Field $_ 'id') -eq 'board' }).Count
        $events = Get-Field $plugin[0] 'events' @()
        Assert-Equal -Id 'L1.8' -What 'the manifest declares both reconciliation events' -Expected 2 -Actual @($events).Count `
            -Evidence (Protect-Text (Get-Json @($events | ForEach-Object { Get-Field $_ 'on' })) 300)
    }

    $actions = Get-HerdrJson -Arguments @('plugin', 'action', 'list', '--plugin', $script:PluginId) -NoSessionScope
    foreach ($id in @('setup', 'bind', 'board')) {
        $found = @((Get-JsonField $actions 'actions' @()) | Where-Object { (Get-Field $_ 'action_id') -eq $id })
        Assert-Equal -Id "L2.$id" -What "action '$id' is registered" -Expected 1 -Actual $found.Count
    }
    $bindAction = @((Get-JsonField $actions 'actions' @()) | Where-Object { (Get-Field $_ 'action_id') -eq 'bind' })
    if ($bindAction.Count -gt 0) {
        $contexts = @(Get-Field $bindAction[0] 'contexts' @())
        Assert-Equal -Id 'L2.bind-contexts' -What 'bind declares pane and workspace contexts' -Expected 2 -Actual $contexts.Count -Evidence ($contexts -join ',')
    }
}

# ---------------------------------------------------------------------------
# Phase 4: isolated Herdr session
# ---------------------------------------------------------------------------

function New-RunServerStartInfo {
    $psi = New-Object System.Diagnostics.ProcessStartInfo
    $psi.FileName = $script:HerdrExe
    $psi.Arguments = (@('--session', $script:SessionName, 'server') | ForEach-Object { ConvertTo-ArgumentLiteral $_ }) -join ' '
    $psi.UseShellExecute = $false
    $psi.CreateNoWindow = $true
    $psi.WorkingDirectory = $script:RepoRoot
    foreach ($key in $script:HerdrCallerEnvKeys) {
        if ($psi.EnvironmentVariables.ContainsKey($key)) { [void]$psi.EnvironmentVariables.Remove($key) }
    }
    # The isolated server must not inherit the invoking pane's caller identity or
    # configuration. Reapply only this run's dedicated config path.
    $psi.EnvironmentVariables['HERDR_CONFIG_PATH'] = $script:ConfigPath
    # Every OMP agent this server starts, primary and workers alike, inherits this;
    # it is how -Model reaches them. Left alone when no model is pinned.
    if ($script:OmpModel) {
        $psi.EnvironmentVariables['PI_CONFIG_FILES'] = Get-OmpConfigFilesValue
    }
    return $psi
}

function Start-RunServer {
    <#
        Starts the isolated session's headless server as a child of this script.
        The child inherits this console on purpose: no OutputDataReceived /
        ErrorDataReceived handlers are attached, because those run on
        thread-pool threads where an exception is not covered by this script's
        try/finally and terminates the whole Windows PowerShell process without
        running any cleanup (leaving the session server and the temp repository
        behind). The server's own lines therefore appear inline in this console;
        nothing in the run depends on capturing them.
    #>
    $script:ServerStartAttempted = $true
    Save-AcceptanceRunManifest -Phase 'server-starting' -Status 'active'
    $proc = New-Object System.Diagnostics.Process
    $proc.StartInfo = New-RunServerStartInfo
    [void]$proc.Start()
    $script:ServerProcess = $proc
    $script:ServerStartedByScript = $true
    Save-AcceptanceRunManifest -Phase 'server-started' -Status 'active'
    return $proc
}

function Invoke-IsolatedSession {
    Write-Step "Phase 4: isolated Herdr session '$($script:SessionName)' under $($script:WorkRoot)"
    Save-AcceptanceRunManifest -Phase 'temp-root-creating' -Status 'active'
    [void](New-Item -ItemType Directory -Path $script:WorkRoot -Force)
    $script:TempRootCreated = $true
    Save-AcceptanceRunManifest -Phase 'temp-root-created' -Status 'active'
    Assert-True -Id 'S1.1' -What 'the run root is a unique directory under $env:TEMP' `
        -Condition ((Test-Below -Root $env:TEMP -Path $script:WorkRoot) -and (Test-Path -LiteralPath $script:WorkRoot)) -Evidence $script:WorkRoot

    $config = @"
# Accept-run Herdr server config: keep every checkout Herdr creates for this run
# inside the run's temporary root.
[worktrees]
directory = "$($script:WorktreesRoot.Replace('\', '/'))"
"@
    Set-Content -LiteralPath $script:ConfigPath -Value $config -Encoding UTF8
    Assert-True -Id 'S1.2' -What 'the run config points Herdr worktrees at the run temp root' -Condition (Test-Path -LiteralPath $script:ConfigPath) -Evidence $script:ConfigPath

    if ($script:OmpModel) {
        [System.IO.File]::WriteAllText($script:OmpModelOverlayPath, (Get-OmpModelOverlayText -Model $script:OmpModel), (New-Object System.Text.UTF8Encoding($false)))
        # Prove OMP honors the overlay before anything runs on it: a silently ignored
        # overlay would run the whole suite on a model nobody asked for.
        $omp = Get-Command omp -ErrorAction SilentlyContinue
        if (-not $omp) { throw 'omp is not on PATH; the model overlay cannot be verified and the agents cannot start' }
        $overlayProbe = Invoke-Native -File $omp.Source -Arguments @('config', 'get', 'modelRoles', '--json') `
            -Environment @{ 'PI_CONFIG_FILES' = (Get-OmpConfigFilesValue) } -TimeoutSeconds 60
        $resolvedModel = ''
        try { $resolvedModel = [string](Get-Field (Get-Field ($overlayProbe.Stdout | ConvertFrom-Json) 'value') 'default') } catch { }
        Assert-Equal -Id 'S1.2b' -What 'OMP resolves the run overlay as its default model role' -Expected $script:OmpModel -Actual $resolvedModel `
            -Command $overlayProbe.CommandLine -Evidence ("overlay=$($script:OmpModelOverlayPath) stderr=[" + (Protect-Text $overlayProbe.Stderr 300) + "]")
        if ($resolvedModel -cne $script:OmpModel) { throw "OMP did not apply the model overlay (resolved '$resolvedModel'); refusing to run on a model that was not requested" }
    } else {
        Add-Assertion -Id 'S1.2b' -What 'no model is pinned; every session uses the default of the OMP profile' -Status 'INFO'
    }

    [void](Start-RunServer)
    $ready = Wait-Until -TimeoutSeconds 60 -IntervalSeconds 2 -What 'our session server' -Probe {
        $sessions = Get-Sessions
        $mine = @($sessions | Where-Object { (Get-Field $_ 'name') -eq $script:SessionName })
        if ($mine.Count -eq 0) { return $null }
        if (-not (Get-Flag $mine[0] 'running')) { return $null }
        return $mine[0]
    }
    Assert-True -Id 'S1.3' -What 'the isolated session reports running' -Condition $ready.Ok `
        -Evidence ("after {0:N1}s; server stdout: {1}" -f $ready.Elapsed, (Protect-Text (Get-Content -LiteralPath $script:ServerLog -Raw -ErrorAction SilentlyContinue) 400))

    $script:SessionSocket = [string](Get-Field $ready.Value 'socket_path')
    $script:SessionDir = [string](Get-Field $ready.Value 'session_dir')
    Save-AcceptanceRunManifest -Phase 'session-ready' -Status 'active'
    Add-Assertion -Id 'S1.3b' -What 'the run session socket captured (it must differ from the default session)' -Status 'INFO' -Evidence $script:SessionSocket

    $sessionStatus = Invoke-Herdr -Arguments @('status', 'server')
    $script:SessionServerVersion = ((@([string]$sessionStatus.Stdout -split "`n") | Where-Object { $_ -match '^version:' } | Select-Object -First 1) -replace '^version:\s*', '').Trim()
    Add-Assertion -Id 'S1.4' -What 'isolated server version (may differ from the CLI)' -Status 'INFO' -Evidence $script:SessionServerVersion

    $default = @(Get-Sessions | Where-Object { (Get-Field $_ 'name') -eq 'default' })
    Assert-True -Id 'S1.5' -What "the user's default session is still running (never stopped/restarted by this run)" `
        -Condition ($default.Count -gt 0 -and (Get-Flag $default[0] 'running')) -Evidence (Protect-Text (Get-Json $default) 400)

    $workspaces = Get-HerdrJson -Arguments @('workspace', 'list')
    $script:InitialWorkspaceCount = @((Get-JsonField $workspaces 'workspaces' @())).Count
    Assert-Equal -Id 'S1.6' -What 'the isolated session starts empty (it is not the user session)' -Expected 0 -Actual $script:InitialWorkspaceCount
}

# ---------------------------------------------------------------------------
# Phase 5: disposable repository and workspace
# ---------------------------------------------------------------------------

function Invoke-Git {
    param([string[]]$Arguments, [string]$Directory, [double]$TimeoutSeconds = 120)
    $argv = @('-C', $Directory) + $Arguments
    return Invoke-Native -File 'git' -Arguments $argv -TimeoutSeconds $TimeoutSeconds
}

function Invoke-TempRepo {
    Write-Step 'Phase 5: disposable repository, workspace and focus proof'
    Save-AcceptanceRunManifest -Phase 'temp-repository-creating' -Status 'active'
    [void](New-Item -ItemType Directory -Path $script:RepoPath -Force)
    Save-AcceptanceRunManifest -Phase 'temp-repository-created' -Status 'active'
    $init = Invoke-Git -Arguments @('init', '-q') -Directory $script:RepoPath
    Assert-Equal -Id 'R1.1' -What 'git init succeeds in the temp repository' -Expected 0 -Actual $init.ExitCode -Command $init.CommandLine -Evidence (Protect-Text $init.Stderr 300)

    Set-Content -LiteralPath (Join-Path $script:RepoPath 'a.js') -Value "// sample A`nexport function add(a, b) { return a + b; }`n" -Encoding UTF8
    Set-Content -LiteralPath (Join-Path $script:RepoPath 'b.js') -Value "// sample B`nexport function divide(a, b) { return a / b; }`n" -Encoding UTF8
    [void](Invoke-Git -Arguments @('add', '-A') -Directory $script:RepoPath)
    $commit = Invoke-Git -Arguments @('-c', 'user.email=acceptance@localhost', '-c', 'user.name=acceptance', 'commit', '-qm', 'base', '--no-gpg-sign') -Directory $script:RepoPath
    Assert-Equal -Id 'R1.2' -What 'the base commit is created' -Expected 0 -Actual $commit.ExitCode -Evidence (Protect-Text $commit.Stderr 300)
    $head = Invoke-Git -Arguments @('rev-parse', 'HEAD') -Directory $script:RepoPath
    $script:BaseRevision = $head.Stdout.Trim()
    Assert-Match -Id 'R1.3' -What 'the base revision is a committed commit id' -Pattern '^[0-9a-f]{40}$' -Actual $script:BaseRevision
    Assert-True -Id 'R1.4' -What 'the temp repository is outside the plugin checkout' `
        -Condition (-not (Test-Below -Root $script:RepoRoot -Path $script:RepoPath)) -Evidence "$($script:RepoPath) vs $($script:RepoRoot)"

    Save-AcceptanceRunManifest -Phase 'primary-workspace-creating' -Status 'active'
    $created = Get-HerdrJson -Arguments @('workspace', 'create', '--cwd', $script:RepoPath, '--no-focus', '--label', "accept-$($script:RunId)")
    $ws = Get-Field $created.Result 'workspace'
    $pane = Get-Field $created.Result 'root_pane'
    $tab = Get-Field $created.Result 'tab'
    $script:PrimaryWorkspace = [string](Get-Field $ws 'workspace_id')
    $script:PrimaryPane = [string](Get-Field $pane 'pane_id')
    $script:PrimaryTab = [string](Get-Field $tab 'tab_id')
    [void]$script:CreatedWorkspaces.Add($script:PrimaryWorkspace)
    [void]$script:CreatedPanes.Add($script:PrimaryPane)
    Save-AcceptanceRunManifest -Phase 'primary-workspace-created' -Status 'active'
    Assert-NonEmpty -Id 'R2.1' -What 'workspace create returned a workspace id' -Value $script:PrimaryWorkspace
    Assert-NonEmpty -Id 'R2.2' -What 'workspace create returned a root pane id' -Value $script:PrimaryPane
    Assert-SamePath -Id 'R2.3' -What 'the new pane works in the temp repository' -Expected $script:RepoPath -Actual (Get-Field $pane 'cwd')

    $focus = Get-HerdrJson -Arguments @('workspace', 'focus', $script:PrimaryWorkspace)
    Assert-True -Id 'R2.4' -What 'the temp workspace can be focused in the isolated session' -Condition ($focus.Result -ne $null)
    $snapshot = Get-Snapshot
    Assert-Equal -Id 'R2.5' -What 'the focused workspace is the temp workspace' -Expected $script:PrimaryWorkspace -Actual (Get-Field $snapshot 'focused_workspace_id')
    Assert-Equal -Id 'R2.6' -What 'the focused pane is the temp pane' -Expected $script:PrimaryPane -Actual (Get-Field $snapshot 'focused_pane_id')
    $focusedPane = @($snapshot.panes | Where-Object { (Get-Field $_ 'pane_id') -eq $script:PrimaryPane })
    Assert-Equal -Id 'R2.7' -What 'the focused pane context resolves to the temp repository before bind' -Expected 1 -Actual $focusedPane.Count
    if ($focusedPane.Count -gt 0) {
        Assert-SamePath -Id 'R2.8' -What 'the focused pane cwd is the temp repository (asserted before any bind/action)' -Expected $script:RepoPath -Actual (Get-Field $focusedPane[0] 'cwd')
    }
    $script:InitialFocus = Get-FocusProjection -Snapshot $snapshot
    Add-Assertion -Id 'R2.9' -What 'focus captured before the scenario (restored in finally)' -Status 'INFO' -Evidence (Protect-Text $script:InitialFocus 400)

    $worktrees = Get-WorktreeList
    $entries = @(Get-Field $worktrees 'worktrees' @())
    Assert-Equal -Id 'R2.10' -What 'the temp repository has exactly its main checkout before dispatch' -Expected 1 -Actual $entries.Count
}

# ---------------------------------------------------------------------------
# Phase 6: the explicit setup action installs the extension
# ---------------------------------------------------------------------------

function Test-BridgeEnvelopeShape {
    <#
        Whether one parsed bridge answer is a v1 envelope. A success carries
        `ok: true` and a `result`; a failure carries `ok: false` and an error with
        a code. Anything else - parseable JSON that is not an envelope, or an `ok`
        that is not a real boolean - is not an envelope, so the callers cannot
        mistake junk for a coded answer.
    #>
    param($Answer)
    if ($null -eq $Answer) { return $false }
    $okProperty = $Answer.PSObject.Properties['ok']
    if ($null -eq $okProperty) { return $false }
    # `ok` must be a real JSON boolean. Get-Flag would coerce a non-empty string
    # ("false") or a number to true, which would let malformed output pass as a
    # successful v1 envelope.
    if ($okProperty.Value -isnot [bool]) { return $false }
    $ok = [bool]$okProperty.Value
    $resultField = Get-Field $Answer 'result' $null
    $errorField = Get-Field $Answer 'error' $null
    $hasError = ($null -ne $errorField) -and ([string](Get-Field $errorField 'code') -ne '')
    return (($ok -and ($null -ne $resultField)) -or ((-not $ok) -and $hasError))
}

function Get-ExpectedExtensionContent {
    <#
        The exact bytes setup should install: the plugin's canonical extension
        source with the three placeholders replaced by JSON-escaped absolute paths
        (the same substitution internal/setup performs). Comparing the installed
        file against this proves it is the CURRENT source, not merely a file that
        happens to carry the marker and the right three paths.
    #>
    param([Parameter(Mandatory)][string]$Executable, [Parameter(Mandatory)][string]$StateDir, [Parameter(Mandatory)][string]$HerdrBinary)
    $sourcePath = Join-Path $script:RepoRoot 'omp\herdr-orchestrator.ts'
    # The assertion record must not leak into this function's output.
    [void](Assert-True -Id 'U2.4a' -What 'the canonical extension source exists in the plugin root' -Condition (Test-Path -LiteralPath $sourcePath) -Evidence $sourcePath)
    if (-not (Test-Path -LiteralPath $sourcePath)) { return $null }
    $source = Get-Content -LiteralPath $sourcePath -Raw
    foreach ($pair in @(
            @('@@OMP_ORCH_EXECUTABLE@@', $Executable),
            @('@@OMP_ORCH_STATE_DIR@@', $StateDir),
            @('@@OMP_ORCH_HERDR_BINARY@@', $HerdrBinary))) {
        $escaped = (ConvertTo-Json -Compress -InputObject ([string]$pair[1])).Trim('"')
        $source = $source.Replace([string]$pair[0], $escaped)
    }
    return ($source -replace "`r`n", "`n")
}

function Get-ExtensionConstants {
    param([string]$Path)
    $text = Get-Content -LiteralPath $Path -Raw
    $values = @{}
    foreach ($name in @('PLUGIN_EXECUTABLE', 'PLUGIN_STATE_DIR', 'HERDR_BINARY')) {
        $m = [regex]::Match($text, "const\s+$name\s*=\s*""((?:[^""\\]|\\.)*)""")
        if ($m.Success) {
            $values[$name] = ($m.Groups[1].Value -replace '\\"', '"' -replace '\\\\', '\')
        }
    }
    return $values
}

function Invoke-SetupAction {
    Write-Step 'Phase 6: setup action (effective profile install)'
    # Setup writes the profile-global extension. Run it through this scenario's
    # isolated server so its embedded HERDR_BIN_PATH matches the server this
    # acceptance exercises; the other actions use the same isolated session.
    $invoke = Invoke-PluginAction -ActionId 'setup'
    Assert-True -Id 'U1.1' -What 'plugin action invoke setup returns an invocation envelope' -Condition ($null -ne $invoke) -Evidence (Protect-Text (Get-Json $invoke) 500)
    if (-not $invoke) { throw 'the setup action could not be invoked' }
    Assert-True -Id 'U1.0' -What 'setup runs in this isolated server while installing the profile-global extension' `
        -Condition ($invoke.Invocation.CommandLine -match [regex]::Escape("--session $($script:SessionName)")) `
        -Command $invoke.Invocation.CommandLine -Evidence $invoke.Invocation.CommandLine
    Assert-Match -Id 'U1.0b' -What 'the setup invocation names the setup action of this plugin' `
        -Pattern 'plugin action invoke setup --plugin user\.omp-orchestrator' -Actual $invoke.Invocation.CommandLine
    $action = Get-Field $invoke 'Result'
    $logId = [string](Get-Field (Get-Field $action 'log') 'log_id')
    Assert-NonEmpty -Id 'U1.2' -What 'the setup invocation is recorded in the plugin log' -Value $logId

    $output = Get-ActionOutput -LogId $logId -TimeoutSeconds 90
    Assert-True -Id 'U1.3' -What 'the setup command finished' -Condition ($null -ne $output) -Evidence (Protect-Text (Get-Json $output) 400)
    if (-not $output) { throw 'the setup action never finished' }
    Assert-Equal -Id 'U1.4' -What 'the setup command exited 0' -Expected 0 -Actual (Get-Field $output.Entry 'exit_code')
    Assert-Equal -Id 'U1.5' -What 'the setup command reports success' -Expected 'succeeded' -Actual (Get-Field $output.Entry 'status')
    $payload = $output.Payload
    Assert-JsonTrue -Id 'U1.6' -What 'setup answered with ok=true (a real boolean)' -Object $payload -Name 'ok'
    Assert-Equal -Id 'U1.7' -What 'setup reports the bridge protocol version' -Expected 1 -Actual (Get-Field $payload 'protocol')

    $reported = [string](Get-Field $payload 'extension')
    $expected = Get-EffectiveExtensionDir
    Assert-SamePath -Id 'U2.1' -What 'the installed extension lands in the effective profile extension directory' -Expected (Join-Path $expected 'herdr-orchestrator.ts') -Actual $reported
    Assert-True -Id 'U2.2' -What 'the extension file exists at that path' -Condition (Test-Path -LiteralPath $reported) -Evidence $reported

    $firstLine = (Get-Content -LiteralPath $reported -TotalCount 1)
    Assert-Equal -Id 'U2.3' -What 'the installed file is the plugin-managed extension' -Expected '// herdr-omp-orchestrator managed OMP extension (edit this source, not the installed copy).' -Actual $firstLine

    $constants = Get-ExtensionConstants -Path $reported
    Assert-SamePath -Id 'U3.1' -What 'the installed extension embeds the executable this run built' -Expected (Get-PluginExe) -Actual ([string]$constants['PLUGIN_EXECUTABLE'])
    Assert-SamePath -Id 'U3.2' -What 'the installed extension embeds this plugin state directory' -Expected $script:StateDir -Actual ([string]$constants['PLUGIN_STATE_DIR'])
    Assert-SamePath -Id 'U3.3' -What 'the installed extension embeds the verified Herdr CLI' -Expected $script:HerdrExe -Actual ([string]$constants['HERDR_BINARY'])

    Assert-Equal -Id 'U4.1' -What "Herdr's own integration file is byte-identical after setup" -Expected $script:IntegrationFileHashBefore -Actual (Get-FileHashSafe (Join-Path $expected 'herdr-omp-agent-state.ts'))
    # The installed file can legitimately predate this run's source (the operator
    # may have restored it out of band), so the pre/post transition is recorded
    # rather than required - and U2.3/U3.1-U3.3 already prove the installed file is
    # THIS plugin environment's extension. What is required is that setup
    # converges and then stays converged, asserted by running it a second time.
    $firstHash = Get-FileHashSafe $reported
    if ($firstHash -ceq $script:ExtensionFileHashBefore) {
        Add-Assertion -Id 'U4.2' -What 'setup left the installed extension byte-identical (it was already current)' -Status 'INFO' -Evidence $firstHash
    } else {
        Add-Assertion -Id 'U4.2' -What 'setup refreshed the installed extension' -Status 'INFO' `
            -Evidence ("before=$($script:ExtensionFileHashBefore) after=$firstHash")
    }
    $second = Invoke-PluginAction -ActionId 'setup'
    Assert-True -Id 'U4.2a' -What 'a repeated setup action returns an invocation envelope' -Condition ($null -ne $second) -Evidence (Protect-Text (Get-Json $second) 400)
    if ($second) {
        Assert-True -Id 'U4.2b' -What 'a repeated setup stays on the isolated server' `
            -Condition ($second.Invocation.CommandLine -match [regex]::Escape("--session $($script:SessionName)")) -Evidence $second.Invocation.CommandLine
        $secondOut = Get-ActionOutput -LogId ([string](Get-Field (Get-Field (Get-Field $second 'Result') 'log') 'log_id')) -TimeoutSeconds 90
        Assert-Equal -Id 'U4.2c' -What 'the repeated setup exits 0 and reports success' -Expected 0 -Actual (Get-Field $secondOut.Entry 'exit_code')
        Assert-JsonTrue -Id 'U4.2d' -What 'the repeated setup answered ok=true (a real boolean)' -Object $secondOut.Payload -Name 'ok'
    }
    Assert-Equal -Id 'U4.2e' -What 'setup is idempotent: a second run leaves the extension byte-identical' -Expected $firstHash -Actual (Get-FileHashSafe $reported)
    # Currentness: the installed bytes must equal what this build renders from its
    # own source, so a stale or altered body with the marker and the right paths
    # cannot pass.
    $expectedContent = Get-ExpectedExtensionContent -Executable (Get-PluginExe) -StateDir $script:StateDir -HerdrBinary $script:HerdrExe
    if ($null -ne $expectedContent) {
        $installedContent = (Get-Content -LiteralPath $reported -Raw) -replace "`r`n", "`n"
        Assert-True -Id 'U4.2f' -What 'the installed extension is exactly what this build renders from its canonical source' `
            -Condition ($installedContent -ceq $expectedContent) `
            -Evidence ("installed_bytes={0} expected_bytes={1}" -f $installedContent.Length, $expectedContent.Length)
    }
    $script:ExtensionFileHashAfter = Get-FileHashSafe $reported

    $status = Invoke-Herdr -Arguments @('integration', 'status') -NoSessionScope
    $ompLine = (@([string]$status.Stdout -split "`n") | Where-Object { $_.Trim().StartsWith('omp:') } | Select-Object -First 1)
    Assert-True -Id 'U4.3' -What 'the Herdr OMP integration is still installed after setup' -Condition ($ompLine -and $ompLine -notmatch '(?i)not installed') -Evidence (Protect-Text $ompLine 200)

    Assert-True -Id 'U4.4' -What 'the state store the extension embeds is the store this script reads' -Condition (Test-SamePath -A $script:StateDb -B (Join-Path ([string]$constants['PLUGIN_STATE_DIR']) 'orchestrator.sqlite')) `
        -Evidence "$($script:StateDb) vs $([string]$constants['PLUGIN_STATE_DIR'])"
}

# ---------------------------------------------------------------------------
# Phase 7: bind the primary
# ---------------------------------------------------------------------------

function Get-BindPayload {
    param([Parameter(Mandatory)]$Result, [double]$TimeoutSeconds = 120)
    $logId = [string](Get-Field (Get-Field $Result 'log') 'log_id')
    return (Get-ActionOutput -LogId $logId -TimeoutSeconds $TimeoutSeconds)
}

function Write-BindRunIdentity {
    # Persist the central lifecycle identity before bind may create durable rows.
    if (-not (Test-Below -Root $script:WorkRoot -Path $script:RepoPath) -or
        -not (Test-Path -LiteralPath $script:RepoPath -PathType Container)) {
        throw 'refusing to bind without an owned temporary repository'
    }
    Save-AcceptanceRunManifest -Phase 'bind' -Status 'active'
    [pscustomobject]@{
        runId = $script:RunId
        session = $script:SessionName
        projectKey = $script:RepoPath
        startedAt = $script:StartedAt
    } | ConvertTo-Json | Set-Content -LiteralPath $script:ReportJson -Encoding UTF8
}

function Invoke-Bind {
    Write-Step 'Phase 7: bind the primary (real action, real pane context)'
    Write-BindRunIdentity
    # The context env below is exactly what Herdr injects into a plugin command
    # it starts from a pane; the script supplies the pane it just proved is the
    # temp repository, and asserts the context Herdr resolved.
    $context = @{
        'HERDR_ENV'          = '1'
        'HERDR_PANE_ID'      = $script:PrimaryPane
        'HERDR_WORKSPACE_ID' = $script:PrimaryWorkspace
        'HERDR_TAB_ID'       = $script:PrimaryTab
    }
    $invoke = Invoke-PluginAction -ActionId 'bind' -PaneContext $context -TimeoutSeconds 240
    Assert-True -Id 'N1.1' -What 'plugin action invoke bind returns an invocation envelope' -Condition ($null -ne $invoke) -Evidence (Protect-Text (Get-Json $invoke) 500)
    if (-not $invoke) { throw 'the bind action could not be invoked' }
    Assert-True -Id 'N1.0' -What 'the bind invocation names the isolated session' `
        -Condition ($invoke.Invocation.CommandLine -match [regex]::Escape("--session $($script:SessionName)")) `
        -Command $invoke.Invocation.CommandLine -Evidence $invoke.Invocation.CommandLine
    $action = Get-Field $invoke 'Result'
    $resolved = Get-Field $action 'context'
    Assert-SamePath -Id 'N1.2' -What 'the invocation context names the temp repository' -Expected $script:RepoPath -Actual ([string](Get-Field $resolved 'focused_pane_cwd'))
    Assert-Equal -Id 'N1.3' -What 'the invocation context names the temp workspace' -Expected $script:PrimaryWorkspace -Actual ([string](Get-Field $resolved 'workspace_id'))
    Assert-Equal -Id 'N1.4' -What 'the invocation context names the temp pane' -Expected $script:PrimaryPane -Actual ([string](Get-Field $resolved 'focused_pane_id'))

    $output = Get-BindPayload -Result $action
    Assert-True -Id 'N2.1' -What 'the bind command finished' -Condition ($null -ne $output) -Evidence (Protect-Text (Get-Json $output) 1500)
    if (-not $output) { throw 'the bind action never finished' }
    Assert-Equal -Id 'N2.2' -What 'the bind command exited 0' -Expected 0 -Actual (Get-Field $output.Entry 'exit_code')
    $payload = $output.Payload
    Assert-JsonTrue -Id 'N2.3' -What 'bind answered with ok=true (a real boolean)' -Object $payload -Name 'ok'
    if ((Test-JsonBool -Object $payload -Name 'ok') -ne $true) { throw 'bind failed; the rest of the workflow cannot be driven' }

    $project = Get-Field $payload 'project'
    $script:Project = $project
    $topLevel = (Invoke-Git -Arguments @('rev-parse', '--show-toplevel') -Directory $script:RepoPath).Stdout.Trim()
    Assert-SamePath -Id 'N3.1' -What 'the bound project key is the canonical repository root' -Expected $topLevel -Actual ([string](Get-Field $project 'Key'))
    Assert-Equal -Id 'N3.2' -What 'the project records the temp workspace' -Expected $script:PrimaryWorkspace -Actual ([string](Get-Field $project 'RepoWorkspaceID'))
    Assert-Equal -Id 'N3.3' -What 'the project launch pane is the temp pane' -Expected $script:PrimaryPane -Actual ([string](Get-Field $project 'LaunchPaneID'))
    Assert-Equal -Id 'N3.4' -What 'the project public pane starts equal to the launch pane' -Expected $script:PrimaryPane -Actual ([string](Get-Field $project 'PublicPaneID'))
    Assert-NonEmpty -Id 'N3.5' -What 'the project records a primary agent name' -Value ([string](Get-Field $project 'AgentName'))
    Assert-NonEmpty -Id 'N3.6' -What 'the project records an OMP session identity' -Value ([string](Get-Field $project 'OMPSessionID'))
    Assert-NonEmpty -Id 'N3.7' -What 'the project records a terminal identity' -Value ([string](Get-Field $project 'TerminalID'))

    if ($script:Python) {
        $dump = Get-StateDump
        $row = @($dump.tables.projects | Where-Object { (Get-Field $_ 'key') -eq (Get-Field $project 'Key') })
        Assert-Equal -Id 'N4.1' -What 'the store holds exactly one project row for this repository' -Expected 1 -Actual $row.Count
        $answerSession = [string](Get-Field $project 'OMPSessionID')
        $handshake = Get-ReadySession -LaunchPaneId $script:PrimaryPane
        if ($row.Count -gt 0) {
            $storedSession = [string](Get-Field (Get-Field $row[0] 'fact') 'OMPSessionID')
            # The bind answer carries Herdr reference value; the store carries the
            # native identity the OMP runtime announced. They are the same session
            # in two forms, linked by the handshake row - never by raw equality.
            Assert-True -Id 'N4.2' -What 'the action answer and the stored row name the same session, each in the form it records' `
                -Condition (Test-SessionIdentity -Left $answerSession -Right $storedSession -Ready $handshake) `
                -Evidence ("answer=[$answerSession] stored=[$storedSession] handshake=" + (Protect-Text (Get-Json $handshake) 400))
            Assert-Equal -Id 'N4.3' -What 'the stored agent name matches the action answer' -Expected ([string](Get-Field $project 'AgentName')) -Actual ([string](Get-Field (Get-Field $row[0] 'fact') 'AgentName'))
        }
        Assert-True -Id 'N4.4' -What 'the primary announced a readiness handshake' -Condition ($null -ne $handshake) -Evidence (Protect-Text (Get-Json $handshake) 400)
        if ($handshake) {
            Assert-Equal -Id 'N4.5' -What 'the handshake was confirmed against `agent get` (the real extension announced it)' -Expected 1 -Actual ([int](Get-Field $handshake 'verified'))
            Assert-Equal -Id 'N4.6' -What 'the handshake protocol version is 1' -Expected 1 -Actual ([int](Get-Field $handshake 'protocol'))
            Assert-True -Id 'N4.7' -What 'the confirmed reference kind is id or path' -Condition ((Get-Field $handshake 'verified_kind') -in @('id', 'path')) -Evidence (Get-Field $handshake 'verified_kind')
            $materialized = Test-HandshakeMaterialized -Handshake $handshake
            $onDisk = ([int](Get-Field $handshake 'omp_session_on_disk') -eq 1)
            if ($null -eq $materialized) {
                Add-Assertion -Id 'N4.8' -What 'the announced session reference is id-only, so materialization cannot be checked from the filesystem' -Status 'INFO' `
                    -Evidence ("onDisk=$onDisk file=[$(Get-Field $handshake 'omp_session_file')]")
            } else {
                Assert-True -Id 'N4.8' -What 'the announced on-disk flag agrees with the session file on disk (materialization invariant)' `
                    -Condition ($materialized -eq $onDisk) -Evidence ("file=[$(Get-Field $handshake 'omp_session_file')] exists=$materialized onDisk=$onDisk")
            }
        }
    }

    $live = Wait-Until -TimeoutSeconds 60 -IntervalSeconds 3 -What 'primary agent session' -Probe {
        $agent = Get-LiveAgent -Target $script:PrimaryPane
        if (-not $agent.Found) { return $null }
        if ([string]::IsNullOrWhiteSpace([string](Get-Field (Get-Field $agent.Agent 'agent_session') 'value'))) { return $null }
        return $agent
    }
    Assert-True -Id 'N5.1' -What 'Herdr reports a live agent on the primary pane' -Condition $live.Ok -Evidence (Protect-Text (Get-Json $live.Value) 400)
    if ($live.Ok) {
        $agent = $live.Value.Agent
        Assert-Equal -Id 'N5.2' -What 'the primary pane runs the omp agent kind' -Expected 'omp' -Actual ([string](Get-Field $agent 'agent')) -CaseInsensitive
        Assert-Equal -Id 'N5.3' -What 'Herdr agrees on the primary agent name' -Expected ([string](Get-Field $project 'AgentName')) -Actual ([string](Get-Field $agent 'name'))
        Assert-SamePath -Id 'N5.4' -What 'the primary agent works in the bound repository' -Expected $script:RepoPath -Actual ([string](Get-Field $agent 'cwd'))
        $ref = Get-Field $agent 'agent_session'
        Assert-Equal -Id 'N5.5' -What 'Herdr publishes the recorded OMP session for the primary pane' -Expected ([string](Get-Field $project 'OMPSessionID')) -Actual ([string](Get-Field $ref 'value')) -CaseInsensitive
        Assert-NonEmpty -Id 'N5.6' -What 'Herdr publishes the session reference kind' -Value ([string](Get-Field $ref 'kind'))
    }

    # Resume hint: it must exist exactly when the session is materialized.
    $hint = Get-Field $payload 'session'
    $script:ResumeHintAtBind = $hint
    Add-Assertion -Id 'N6.1' -What 'bind reported a session hint' -Status $(if ($null -ne $hint) { 'PASS' } else { 'FAIL' }) -Evidence (Protect-Text (Get-Json $hint) 400)
    $resume = [string](Get-Field $hint 'resume')
    $unavailable = [string](Get-Field $hint 'resumeUnavailable')
    # Read strictly: a malformed value takes the stricter branch (no resume offered).
    $onDiskHint = ((Test-JsonBool -Object $hint -Name 'onDisk') -eq $true)
    Assert-True -Id 'N6.2' -What 'the hint either offers a resume command or states why it cannot, never both and never neither' `
        -Condition (Test-ResumeHintAvailability -Hint $hint) -Evidence "resume=[$resume] unavailable=[$unavailable]"
    Assert-True -Id 'N6.3' -What 'a resume hint is offered exactly when the session is on disk, and a malformed onDisk flag cannot satisfy this' `
        -Condition (Test-ResumeHintImplication -Hint $hint) -Evidence (Protect-Text (Get-Json $hint) 400)
    if ($onDiskHint) {
        Add-Assertion -Id 'N6.4' -What 'the session was already materialized at bind time, so the unmaterialized branch is not observable here' -Status 'INFO' -Evidence "resume=[$resume]"
    } else {
        Assert-True -Id 'N6.5' -What 'an unmaterialized session advertises no resume command' -Condition ($resume -eq '') -Evidence "resume=[$resume]"
        Assert-True -Id 'N6.6' -What 'the unmaterialized hint explains itself' -Condition ($unavailable -ne '') -Evidence $unavailable
    }
}

# ---------------------------------------------------------------------------
# Phase 8: primary turn and the materialized resume hint
# ---------------------------------------------------------------------------

function Invoke-PrimaryTurn {
    Write-Step 'Phase 8: a real primary turn, then the materialized resume hint'
    $prompt = 'Acceptance probe. Reply with exactly this one line and do not call any tool: ACCEPT-PRIMARY-1 primary-ready'
    $result = Invoke-PrimaryPrompt -PaneId $script:PrimaryPane -Text $prompt -TimeoutSeconds 240
    Assert-Equal -Id 'N7.1' -What 'the primary accepts and completes a real prompt' -Expected 0 -Actual $result.ExitCode -Command $result.CommandLine -Evidence ("code=[$($result.Code)] blocked=[$($result.Blocked)] stderr=[" + (Protect-Text $result.Stderr 300) + "] screen=[" + (Protect-Text $result.Screen 500) + "]")
    if ($result.Blocked) { Block-Item -Id 'N7.1b' -What 'the primary was showing its own UI at the probe' -Evidence (Protect-Text $result.Screen 500) }
    $screen = Get-PaneScreen -PaneId $script:PrimaryPane -Source 'recent-unwrapped' -Lines 80
    Assert-Match -Id 'N7.2' -What 'the primary answered the probe in its own terminal' -Pattern 'primary-ready' -Actual $screen
    $script:MaterializedSession = [string](Get-Field $script:Project 'OMPSessionID')
    $materialized = Wait-Until -TimeoutSeconds 60 -IntervalSeconds 3 -What 'session materialization' -Probe {
        $handshakeNow = Get-ReadySession -LaunchPaneId $script:PrimaryPane
        $state = Test-HandshakeMaterialized -Handshake $handshakeNow
        if ($null -eq $state) { return 'uncheckable' }
        if ($state) { return 'on-disk' }
        return $null
    }
    if (-not $materialized.Ok) {
        Assert-True -Id 'N7.3' -What 'the OMP session is now materialized on disk' -Condition $false `
            -Evidence ("session=$($script:MaterializedSession) after=$($materialized.Elapsed)s; announcement file=[$(Get-Field (Get-ReadySession -LaunchPaneId $script:PrimaryPane) 'omp_session_file')]")
    } elseif ($materialized.Value -eq 'uncheckable') {
        Add-Assertion -Id 'N7.3' -What 'the announced session reference is id-only, so this run cannot prove materialization from the filesystem' -Status 'INFO' `
            -Evidence ("session=$($script:MaterializedSession)")
    } else {
        Assert-True -Id 'N7.3' -What 'the OMP session is now materialized on disk' -Condition $true -Evidence ("session=$($script:MaterializedSession) after=$($materialized.Elapsed)s")
    }
    Assert-OmpSessionModel -Id 'N7.4' -What 'the primary session ran on the requested model' -LaunchPaneId $script:PrimaryPane

    $context = @{
        'HERDR_ENV'          = '1'
        'HERDR_PANE_ID'      = $script:PrimaryPane
        'HERDR_WORKSPACE_ID' = $script:PrimaryWorkspace
        'HERDR_TAB_ID'       = $script:PrimaryTab
    }
    $invoke = Invoke-PluginAction -ActionId 'bind' -PaneContext $context -TimeoutSeconds 240
    Assert-True -Id 'N8.0' -What 'the re-bind invocation names the isolated session' `
        -Condition ($null -ne $invoke -and $invoke.Invocation.CommandLine -match [regex]::Escape("--session $($script:SessionName)")) `
        -Evidence $(if ($invoke) { $invoke.Invocation.CommandLine } else { 'no invocation' })
    $output = if ($invoke) { Get-BindPayload -Result (Get-Field $invoke 'Result') } else { $null }
    Assert-Equal -Id 'N8.1' -What 're-binding an existing project exits 0 (reopen path)' -Expected 0 -Actual $(if ($output) { Get-Field $output.Entry 'exit_code' } else { 'no invocation' })
    $payload = if ($output) { $output.Payload } else { $null }
    Assert-JsonTrue -Id 'N8.2' -What 're-bind answered ok=true (a real boolean)' -Object $payload -Name 'ok'
    $project2 = Get-Field $payload 'project'
    $rebindHandshake = Get-ReadySession -LaunchPaneId $script:PrimaryPane
    Assert-True -Id 'N8.3' -What 're-bind keeps the recorded OMP session (identity retained across native forms)' `
        -Condition (Test-SessionIdentity -Left ([string](Get-Field $script:Project 'OMPSessionID')) -Right ([string](Get-Field $project2 'OMPSessionID')) -Ready $rebindHandshake) `
        -Evidence ("before=[$(Get-Field $script:Project 'OMPSessionID')] after=[$(Get-Field $project2 'OMPSessionID')] handshake=" + (Protect-Text (Get-Json $rebindHandshake) 300))
    Assert-Equal -Id 'N8.4' -What 're-bind keeps the primary agent name' -Expected ([string](Get-Field $script:Project 'AgentName')) -Actual ([string](Get-Field $project2 'AgentName'))
    Assert-SamePath -Id 'N8.5' -What 're-bind keeps the project key' -Expected ([string](Get-Field $script:Project 'Key')) -Actual ([string](Get-Field $project2 'Key'))

    $hint = Get-Field $payload 'session'
    $script:ResumeHintAfterTurn = $hint
    Assert-JsonTrue -Id 'N8.6' -What 'the materialized session now advertises a resume command (a real boolean)' -Object $hint -Name 'onDisk'
    $handshake = Get-ReadySession -LaunchPaneId $script:PrimaryPane
    $expectedResume = 'omp --resume=' + [string](Get-Field $handshake 'omp_session_id')
    Assert-Equal -Id 'N8.7' -What 'the resume hint names the exact announced session id' -Expected $expectedResume -Actual ([string](Get-Field $hint 'resume'))
    $handshakeAfter = Get-ReadySession -LaunchPaneId $script:PrimaryPane
    $fileAfter = Test-HandshakeMaterialized -Handshake $handshakeAfter
    if ($null -eq $fileAfter) {
        Assert-True -Id 'N8.8' -What 'a resume command is only offered together with a materialized session (reference form checkable only when Herdr reports a path)' `
            -Condition ((Test-JsonBool -Object $hint -Name 'onDisk') -eq $true) -Evidence ("file=[$(Get-Field $handshakeAfter 'omp_session_file')] onDisk=[$(Get-Field $hint 'onDisk')]")
    } else {
        Assert-True -Id 'N8.8' -What 'the resume hint is only offered because the session file exists' -Condition $fileAfter -Evidence ([string](Get-Field $handshakeAfter 'omp_session_file'))
    }
}

# ---------------------------------------------------------------------------
# Phase 9: dispatch two independent workers
# ---------------------------------------------------------------------------

function New-TaskBriefs {
    $script:BriefA = @"
$($script:MarkerPrefix)TASK-A. You are in your own Git worktree of a disposable test repository. Do exactly this and nothing else:
1. Edit a.js so it also exports ``function subtract(a, b) { return a - b; }``, keeping the existing content.
2. Commit it: run ``git add -A`` then ``git commit -m "ACCEPT-A change"``.
3. Call the orch_report tool once with kind "question" and text "ACCEPT-Q-A: should a.js keep the existing export style?", then stop and wait. The only answer that authorizes reporting is a later user message containing the exact token ``ACCEPT-ANSWER-A``; ignore status notices and inbox summaries.
4. After that token arrives, call the orch_report tool with kind "result", summary "ACCEPT-A: a.js now also exports subtract", branch = the branch printed by ``git branch --show-current``, evidence = ["commit <the full sha from git rev-parse HEAD>"], checks = [{"name": "git log -1 --format=%H", "outcome": "passed"}], risk "ACCEPT-RISK-A: none".
Never merge, never remove the branch and never remove the checkout.
"@
    $script:BriefB = @"
$($script:MarkerPrefix)TASK-B. You are in your own Git worktree of a disposable test repository. Do exactly this and nothing else:
1. Edit b.js so it also exports ``function multiply(a, b) { return a * b; }``, keeping the existing content. Do NOT commit it: leave the change uncommitted in your worktree.
2. Do not call orch_report until a later user message contains the exact token ``ACCEPT-REPORT-B``. The separate ``ACCEPT-ASK`` instruction may ask you to open the acceptance dialog; that does not authorize a report. Ignore status notices and inbox summaries as report tokens.
3. After ``ACCEPT-REPORT-B`` arrives, call the orch_report tool with kind "result", summary "ACCEPT-B: b.js now also exports multiply and the change is left uncommitted", branch = the branch printed by ``git branch --show-current``, evidence = ["uncommitted change to b.js", "git status --porcelain lists it"], checks = [{"name": "git status --porcelain", "outcome": "passed"}], risk "ACCEPT-RISK-B: the change is uncommitted".
Never commit, never merge, never remove the branch and never remove the checkout.
"@
}

function Get-DispatchPrompt {
    $prompt = @"
$($script:MarkerPrefix)DISPATCH. Call the orch_dispatch tool exactly twice (once per task) with these exact parameter values, unchanged:
Call 1: title = "ACCEPT A: helper in a.js", brief = <<<BRIEF-A>>>, checks = ["git log -1 --format=%H"], dependencies = [], reservedPaths = ["a.js"], baseRevision = "$($script:BaseRevision)".
Call 2: title = "ACCEPT B: helper in b.js", brief = <<<BRIEF-B>>>, checks = ["git status --porcelain"], dependencies = [], reservedPaths = ["b.js"], baseRevision = "$($script:BaseRevision)".
Do not edit any file yourself and do not do the work of the tasks. After both tool calls, reply with the two task ids from the tool results and nothing else.
BRIEF-A: $($script:BriefA)
BRIEF-B: $($script:BriefB)
"@
    return $prompt
}

function Wait-ForTasks {
    param(
        [Parameter(Mandatory)][int]$Count,
        [double]$TimeoutSeconds = 600
    )
    return Wait-Until -TimeoutSeconds $TimeoutSeconds -IntervalSeconds 10 -What 'worker tasks' -Probe {
        $tasks = Get-ProjectTasks -Key (Get-Field $script:Project 'Key')
        if (@($tasks).Count -lt $Count) { return $null }
        return @($tasks)
    }
}

function Invoke-Dispatch {
    Write-Step 'Phase 9: two independent worker changes'
    New-TaskBriefs
    $prompt = Get-DispatchPrompt
    # Neither call names a ticket, so each starts only after the user confirms its
    # exact request. The prompt is submitted without --wait (its turn cannot finish
    # while a dialog is open) and both dialogs are confirmed from the transcript.
    $result = Invoke-PrimaryPromptNoWaitAfterDispositionAsks -PaneId $script:PrimaryPane -Text $prompt -TimeoutSeconds 420 -AssertionPrefix 'D1'
    Assert-True -Id 'D1.1' -What 'the primary accepts the dispatch prompt' -Condition $result.Submitted -Command $result.CommandLine -Evidence ("code=[$($result.Code)] blocked=[$($result.Blocked)] stderr=[" + (Protect-Text $result.Stderr 300) + "] screen=[" + (Protect-Text $result.Screen 500) + "]")
    if ($result.Blocked) { Block-Item -Id 'D1.1b' -What 'the primary was blocked when the dispatch prompt was due' -Evidence (Protect-Text $result.Screen 500) }
    if (-not $result.Submitted) {
        Block-Item -Id 'D1.1c' -What 'the dispatch prompt could not be submitted, so the worker phases cannot run' -Evidence (Protect-Text ($result.Stdout + $result.Stderr) 500)
        return
    }
    $expectedDispatchRequests = @(
        [pscustomobject]@{
            title = 'ACCEPT A: helper in a.js'
            brief = $script:BriefA
            checks = @('git log -1 --format=%H')
            dependencies = @()
            reservedPaths = @('a.js')
            baseRevision = $script:BaseRevision
        },
        [pscustomobject]@{
            title = 'ACCEPT B: helper in b.js'
            brief = $script:BriefB
            checks = @('git status --porcelain')
            dependencies = @()
            reservedPaths = @('b.js')
            baseRevision = $script:BaseRevision
        }
    )
    $dispatchConfirmation = Confirm-PrimaryDispatchModals -ExpectedRequests $expectedDispatchRequests -TimeoutSeconds 300
    $confirmedRequests = @($dispatchConfirmation.Requests)
    $dispatchConfirmed = [bool]$dispatchConfirmation.Confirmed -and $confirmedRequests.Count -eq 2
    Assert-True -Id 'D1.1d' -What 'the acceptance user confirmed the exact dialog of both ticketless dispatch requests' -Condition $dispatchConfirmed `
        -Evidence ("confirmed=[" + (@($confirmedRequests | ForEach-Object { [string](Get-Field $_ 'title') }) -join ' | ') + "]")
    if (-not $dispatchConfirmed) {
        Preserve-ResourcesForPendingTool -AssertionId 'D1.1d.resources-preserved' -Reason 'a ticketless dispatch dialog may still be open on the primary'
        Block-Item -Id 'D1.1e' -What 'the ticketless dispatch dialogs were not safely confirmed, so the worker phases cannot run' `
            -Evidence (Protect-Text (Get-PaneScreen -PaneId $script:PrimaryPane -Source 'visible' -Lines 100) 800)
        return
    }

    $wait = Wait-ForTasks -Count 2 -TimeoutSeconds 600
    Assert-True -Id 'D1.2' -What 'two worker tasks exist for this project' -Condition $wait.Ok `
        -Evidence ("after {0:N0}s: {1}" -f $wait.Elapsed, (Protect-Text (Get-Json @($wait.Value | ForEach-Object { Get-Field $_ 'id' })) 300))
    if (-not $wait.Ok) {
        $screen = Get-PaneScreen -PaneId $script:PrimaryPane -Source 'recent-unwrapped' -Lines 120
        Block-Item -Id 'D1.3' -What 'the primary dispatched two tasks through orch_dispatch' `
            -Evidence ("the store holds {0} task(s); primary screen: {1}" -f @($wait.Value).Count, (Protect-Text $screen 800))
        return
    }
    $script:Tasks = @($wait.Value | Sort-Object { Get-Field $_ 'id' })
    Assert-Equal -Id 'D2.1' -What 'exactly two tasks were dispatched' -Expected 2 -Actual $script:Tasks.Count

    $ids = @()
    foreach ($row in $script:Tasks) {
        $fact = Get-Field $row 'fact'
        $id = [string](Get-Field $fact 'ID')
        $ids += $id
        Assert-Match -Id "D2.id/$id" -What "task $id has a generated orch_<12 hex> id" -Pattern '^orch_[0-9a-f]{12}$' -Actual $id
        Assert-Match -Id "D2.branch/$id" -What "task $id owns a matching orch/<12 hex> branch" -Pattern '^orch/[0-9a-f]{12}$' -Actual ([string](Get-Field $fact 'Branch'))
        Assert-Equal -Id "D2.base/$id" -What "task $id is based on the committed base revision" -Expected $script:BaseRevision -Actual ([string](Get-Field $fact 'BaseRevision'))
        $request = @($confirmedRequests | Where-Object { ([string](Get-Field $_ 'title')).Trim() -ceq [string](Get-Field $fact 'Title') })
        $stored = [pscustomobject]@{
            Title = [string](Get-Field $fact 'Title'); Brief = [string](Get-Field $fact 'Brief'); Checks = @(Get-Field $fact 'Checks' @())
            Dependencies = @(Get-Field $fact 'Dependencies' @()); ReservedPaths = @(Get-Field $fact 'ReservedPaths' @())
            BaseRevision = [string](Get-Field $fact 'BaseRevision'); TicketID = [string](Get-Field $fact 'TicketID')
        }
        Assert-True -Id "D2.request/$id" -What "task $id stores exactly the request the user confirmed, and no ticket" `
            -Condition ($request.Count -eq 1 -and (Test-TaskMatchesDispatchRequest -Fact $fact -Request $request[0])) -TaskId $id `
            -Evidence ("confirmed=" + (Protect-Text (Get-Json $request) 400) + " stored=" + (Protect-Text (Get-Json $stored) 400))
        Assert-True -Id "D2.phase/$id" -What "task $id reached a provisioned phase" -Condition ((Get-Field $fact 'Phase') -in @('worktree_created', 'agent_started', 'prompt_submitted')) -Evidence (Get-Field $fact 'Phase')
        Assert-NonEmpty -Id "D2.worktree/$id" -What "task $id records its checkout path" -Value ([string](Get-Field $fact 'WorktreePath'))
        Assert-NonEmpty -Id "D2.workspace/$id" -What "task $id records its worktree workspace" -Value ([string](Get-Field $fact 'WorktreeWorkspaceID'))
        Assert-NonEmpty -Id "D2.pane/$id" -What "task $id records its public pane" -Value ([string](Get-Field $fact 'PublicPaneID'))
        Assert-NonEmpty -Id "D2.session/$id" -What "task $id records its own OMP session" -Value ([string](Get-Field $fact 'OMPSessionID'))
        Assert-NonEmpty -Id "D2.terminal/$id" -What "task $id records its terminal" -Value ([string](Get-Field $fact 'TerminalID'))
        Assert-Equal -Id "D2.agentname/$id" -What "task $id's live agent name is its task id" -Expected $id -Actual ([string](Get-Field $fact 'AgentName'))
    }

    Assert-Equal -Id 'D3.1' -What 'the two task ids differ' -Expected 2 -Actual (@($ids | Select-Object -Unique).Count)
    $values = @{}
    foreach ($key in @('WorktreePath', 'WorktreeWorkspaceID', 'OMPSessionID', 'PublicPaneID', 'LaunchPaneID', 'Branch', 'TerminalID')) {
        $set = @($script:Tasks | ForEach-Object { [string](Get-Field (Get-Field $_ 'fact') $key) } | Sort-Object -Unique)
        $values[$key] = $set
        Assert-Equal -Id "D3.$key" -What "the two tasks have distinct $key" -Expected 2 -Actual $set.Count -Evidence ($set -join ' | ')
    }

    foreach ($row in $script:Tasks) {
        $fact = Get-Field $row 'fact'
        $id = [string](Get-Field $fact 'ID')
        $pane = [string](Get-Field $fact 'PublicPaneID')
        $live = Get-LiveAgent -Target $pane
        $handshake = Get-ReadySession -LaunchPaneId ([string](Get-Field $fact 'LaunchPaneID'))
        Assert-True -Id "D4.live/$id" -What "Herdr reports the worker agent of $id on its recorded pane" -Condition $live.Found -Evidence (Protect-Text (Get-Json $live.Agent) 400)
        if ($live.Found) {
            $ref = Get-Field $live.Agent 'agent_session'
            # The task records the native OMP identity; Herdr publishes a typed
            # reference. The handshake row links the two forms.
            Assert-True -Id "D4.session/$id" -What "the task and Herdr name the same session for $id, each in the form it records" `
                -Condition (Test-SessionIdentity -Left ([string](Get-Field $fact 'OMPSessionID')) -Right ([string](Get-Field $ref 'value')) -Ready $handshake) `
                -Evidence ("task=[$(Get-Field $fact 'OMPSessionID')] ref_kind=[$(Get-Field $ref 'kind')] ref=[$(Get-Field $ref 'value')] handshake=" + (Protect-Text (Get-Json $handshake) 400))
            Assert-SamePath -Id "D4.cwd/$id" -What "the worker agent of $id works in its own checkout" -Expected ([string](Get-Field $fact 'WorktreePath')) -Actual ([string](Get-Field $live.Agent 'cwd'))
        }
        Assert-True -Id "D4.handshake/$id" -What "the worker's OMP extension announced and was confirmed" -Condition ($null -ne $handshake -and [int](Get-Field $handshake 'verified') -eq 1) -Evidence (Protect-Text (Get-Json $handshake) 400)
        if ($script:Python -and $handshake) {
            $materialized = Test-HandshakeMaterialized -Handshake $handshake
            $onDisk = ([int](Get-Field $handshake 'omp_session_on_disk') -eq 1)
            if ($null -eq $materialized) {
                Add-Assertion -Id "D4.materialized/$id" -What "the worker's session reference is id-only, so materialization cannot be checked" -Status 'INFO' -Evidence "onDisk=$onDisk"
            } elseif ($onDisk) {
                Assert-True -Id "D4.materialized/$id" -What "a materialized worker session has a file on disk" -Condition $materialized -Evidence "exists=$materialized onDisk=$onDisk"
            } elseif ($materialized) {
                Add-Assertion -Id "D4.materialized/$id" -What "the session file appeared after this handshake; agent_end refreshes the on-disk flag" -Status 'INFO' -Evidence "exists=$materialized onDisk=$onDisk"
            } else {
                Assert-True -Id "D4.materialized/$id" -What "an unmaterialized worker session has no file yet" -Condition ($materialized -eq $onDisk) -Evidence "exists=$materialized onDisk=$onDisk"
            }
        }
    }

    $listing = Get-WorktreeList
    $entries = @(Get-Field $listing 'worktrees' @())
    Assert-Equal -Id 'D5.1' -What 'Herdr lists the base checkout plus both worker worktrees' -Expected 3 -Actual $entries.Count
    foreach ($row in $script:Tasks) {
        $fact = Get-Field $row 'fact'
        $id = [string](Get-Field $fact 'ID')
        $path = [string](Get-Field $fact 'WorktreePath')
        $branch = [string](Get-Field $fact 'Branch')
        $workspace = [string](Get-Field $fact 'WorktreeWorkspaceID')
        $entry = @($entries | Where-Object { Test-SamePath -A (Get-Field $_ 'path') -B $path })
        Assert-Equal -Id "D5.listed/$id" -What "Herdr lists the checkout of $id at its recorded path" -Expected 1 -Actual $entry.Count -Evidence $path
        if ($entry.Count -gt 0) {
            Assert-Equal -Id "D5.branch/$id" -What "the listed checkout of $id tracks its recorded branch" -Expected $branch -Actual ([string](Get-Field $entry[0] 'branch'))
            Assert-Equal -Id "D5.workspace/$id" -What "the listed checkout of $id is open in its recorded workspace" -Expected $workspace -Actual ([string](Get-Field $entry[0] 'open_workspace_id'))
        }
        Assert-True -Id "D5.underroot/$id" -What "the checkout of $id is inside this run's temp worktree root" -Condition (Test-Below -Root $script:WorktreesRoot -Path $path) -Evidence $path
        $common = Invoke-Git -Arguments @('rev-parse', '--git-common-dir') -Directory $path
        Assert-True -Id "D5.sameRepo/$id" -What "the checkout of $id belongs to the temp repository" `
            -Condition (Test-Below -Root (Join-Path $script:RepoPath '.git') -Path $common.Stdout.Trim()) -Evidence "common=$($common.Stdout.Trim())"
    }
}

function Invoke-SingleWorkerDispatch {
    Write-Step 'SingleWorker: dispatch one deterministic change through the real primary tools'
    $brief = @"
$($script:MarkerPrefix)SINGLE-WORKER. You are in your own Git worktree of a disposable test repository. Do exactly this:
1. Edit a.js to also export ``function subtract(a, b) { return a - b; }``; keep the existing export.
2. Run ``git add a.js``, ``git commit -m "ACCEPT single-worker change"``, and ``git rev-parse HEAD``.
3. Immediately call orch_report with kind "result", summary "ACCEPT-SINGLE: a.js now exports subtract", branch from ``git branch --show-current``, evidence ["commit <full SHA from git rev-parse HEAD>"], checks [{"name": "git log -1 --format=%H", "outcome": "passed"}], and risk "ACCEPT-SINGLE-RISK: none".
Do not ask a question, wait for another message, modify other files, merge, or clean up the checkout.
"@
    $prompt = @"
$($script:MarkerPrefix)SINGLE-DISPATCH. Call orch_dispatch exactly once with title "ACCEPT SINGLE: helper in a.js", the following brief unchanged, checks ["git log -1 --format=%H"], dependencies [], reservedPaths ["a.js"], and baseRevision "$($script:BaseRevision)". Do no implementation work yourself. Reply with the new task id only.
BRIEF: $brief
"@
    # The call names no ticket, so it starts only after the user confirms its exact
    # request; the prompt is submitted without --wait and the dialog is confirmed.
    $result = Invoke-PrimaryPromptNoWaitAfterDispositionAsks -PaneId $script:PrimaryPane -Text $prompt -TimeoutSeconds 300 -AssertionPrefix 'SW0'
    Assert-True -Id 'SW0.1' -What 'the real primary accepts the single-worker dispatch prompt' `
        -Condition $result.Submitted -Command $result.CommandLine `
        -Evidence ("code=[$($result.Code)] blocked=[$($result.Blocked)] stderr=[" + (Protect-Text $result.Stderr 300) + "]")
    if (-not $result.Submitted) { throw 'single-worker dispatch prompt was not accepted' }
    $expectedDispatchRequest = [pscustomobject]@{
        title = 'ACCEPT SINGLE: helper in a.js'
        brief = $brief
        checks = @('git log -1 --format=%H')
        dependencies = @()
        reservedPaths = @('a.js')
        baseRevision = $script:BaseRevision
    }
    $dispatchConfirmation = Confirm-PrimaryDispatchModals -ExpectedRequests @($expectedDispatchRequest) -TimeoutSeconds 300
    $confirmedRequests = @($dispatchConfirmation.Requests)
    $dispatchConfirmed = [bool]$dispatchConfirmation.Confirmed -and $confirmedRequests.Count -eq 1
    Assert-True -Id 'SW0.1c' -What 'the acceptance user confirmed the exact dialog of the ticketless dispatch request' -Condition $dispatchConfirmed `
        -Evidence ("confirmed=[" + (@($confirmedRequests | ForEach-Object { [string](Get-Field $_ 'title') }) -join ' | ') + "] screen=[" +
            (Protect-Text (Get-PaneScreen -PaneId $script:PrimaryPane -Source 'visible' -Lines 100) 500) + "]")
    if (-not $dispatchConfirmed) {
        Preserve-ResourcesForPendingTool -AssertionId 'SW0.1c.resources-preserved' -Reason 'the ticketless dispatch dialog may still be open on the primary'
        throw 'the single-worker dispatch dialog was not safely confirmed'
    }
    Assert-OmpSessionModel -Id 'SW0.1b' -What 'the primary session ran on the requested model' -LaunchPaneId $script:PrimaryPane

    $wait = Wait-ForTasks -Count 1 -TimeoutSeconds 300
    Assert-True -Id 'SW0.2' -What 'exactly one task was created for the bound project' `
        -Condition ($wait.Ok -and @($wait.Value).Count -eq 1) `
        -Evidence ("after {0:N0}s tasks={1}" -f $wait.Elapsed, $(if ($null -eq $wait.Value) { 0 } else { @($wait.Value).Count }))
    if (-not $wait.Ok -or @($wait.Value).Count -ne 1) { throw 'the single-worker task was not created' }

    $script:Tasks = @($wait.Value)
    $script:TaskA = Get-Field $script:Tasks[0] 'fact'
    $id = [string](Get-Field $script:TaskA 'ID')
    Assert-Match -Id 'SW0.3' -What 'the worker task has a generated task id' -Pattern '^orch_[0-9a-f]{12}$' -Actual $id
    Assert-Match -Id 'SW0.4' -What 'the task owns a generated worker branch' -Pattern '^orch/[0-9a-f]{12}$' `
        -Actual ([string](Get-Field $script:TaskA 'Branch')) -TaskId $id
    Assert-True -Id 'SW0.5' -What 'the task has its own recorded worktree and public pane' `
        -Condition (([string](Get-Field $script:TaskA 'WorktreePath') -ne '') -and ([string](Get-Field $script:TaskA 'PublicPaneID') -ne '')) `
        -TaskId $id -Evidence ("worktree=$(Get-Field $script:TaskA 'WorktreePath') pane=$(Get-Field $script:TaskA 'PublicPaneID')")
    Assert-True -Id 'SW0.request' -What 'the task stores exactly the request the user confirmed, and no ticket' `
        -Condition (Test-TaskMatchesDispatchRequest -Fact $script:TaskA -Request $confirmedRequests[0]) -TaskId $id `
        -Evidence ("confirmed=" + (Protect-Text (Get-Json $confirmedRequests[0]) 400) + " stored title=[$(Get-Field $script:TaskA 'Title')] ticket=[$(Get-Field $script:TaskA 'TicketID')]")
    $live = Get-LiveAgent -Target ([string](Get-Field $script:TaskA 'PublicPaneID'))
    $handshake = Get-ReadySession -LaunchPaneId ([string](Get-Field $script:TaskA 'LaunchPaneID'))
    Assert-True -Id 'SW0.6' -What 'Herdr reports a live worker on its recorded pane' `
        -Condition $live.Found -TaskId $id -Evidence (Protect-Text (Get-Json $live.Agent) 400)
    if ($live.Found) {
        Assert-Equal -Id 'SW0.7' -What 'the task pane runs an OMP agent named for this task' `
            -Expected 'omp' -Actual ([string](Get-Field $live.Agent 'agent')) -TaskId $id -CaseInsensitive
        Assert-Equal -Id 'SW0.8' -What 'the worker agent name matches the task id' `
            -Expected $id -Actual ([string](Get-Field $live.Agent 'name')) -TaskId $id
        Assert-SamePath -Id 'SW0.9' -What 'the worker runs inside its recorded worktree' `
            -Expected ([string](Get-Field $script:TaskA 'WorktreePath')) -Actual ([string](Get-Field $live.Agent 'cwd')) -TaskId $id
        $ref = Get-Field $live.Agent 'agent_session'
        Assert-True -Id 'SW0.10' -What 'the task and Herdr identify the same OMP session through the confirmed handshake' `
            -Condition ($null -ne $handshake -and [int](Get-Field $handshake 'verified') -eq 1 -and
                (Test-SessionIdentity -Left ([string](Get-Field $script:TaskA 'OMPSessionID')) `
                    -Right ([string](Get-Field $ref 'value')) -Ready $handshake)) `
            -TaskId $id -Evidence ("task=$(Get-Field $script:TaskA 'OMPSessionID') herdr=$(Get-Field $ref 'value') handshake=$(Protect-Text (Get-Json $handshake) 300)")
    }
}

# ---------------------------------------------------------------------------
# Phase 10: a partial report is refused while the task is Running
# ---------------------------------------------------------------------------

function Get-TaskCaller {
    param($Fact)
    return @{
        LaunchPane = [string](Get-Field $Fact 'LaunchPaneID')
        PublicPane = [string](Get-Field $Fact 'PublicPaneID')
        Session    = [string](Get-Field $Fact 'OMPSessionID')
        Branch     = [string](Get-Field $Fact 'Branch')
        TaskId     = [string](Get-Field $Fact 'ID')
    }
}

function Invoke-PartialReportRejection {
    Write-Step 'Phase 10: a partial report is refused and leaves the task Running'
    if ($script:Tasks.Count -lt 1) { Block-Item -Id 'PR0.0' -What 'partial-report rejection' -Evidence 'no tasks were dispatched'; return }
    # The refusal is asserted while the task is Running, so the candidate is
    # re-read immediately before the call: a worker may have asked a question or
    # reported already, which legitimately changes the stage.
    $candidate = $null
    foreach ($row in $script:Tasks) {
        $probe = Get-TaskById -Id ([string](Get-Field (Get-Field $row 'fact') 'ID'))
        if (-not $probe) { continue }
        $probeFact = Get-Field $probe 'fact'
        if ((Get-TaskStage -Fact $probeFact) -eq 'running') { $candidate = $probeFact; break }
    }
    if (-not $candidate) {
        Block-Item -Id 'PR0.1' -What 'a task was Running when the partial report was due' `
            -Evidence (Protect-Text (Get-Json @($script:Tasks | ForEach-Object { [pscustomobject]@{ id = (Get-Field (Get-Field $_ 'fact') 'ID'); stage = (Get-TaskStage -Fact (Get-Field $_ 'fact')) } })) 500)
        return
    }
    $fact = $candidate
    $id = [string](Get-Field $fact 'ID')
    $caller = Get-TaskCaller -Fact $fact
    $before = Get-Json $fact

    $live = Get-LiveAgent -Target $caller.PublicPane
    $workerHandshake = Get-ReadySession -LaunchPaneId $caller.LaunchPane
    $liveSession = [string](Get-Field (Get-Field $live.Agent 'agent_session') 'value')
    Assert-True -Id 'PR1.1' -What "the pane the bridge will be told about really hosts $id (replayed caller identity is real)" `
        -Condition ($live.Found -and (Test-SessionIdentity -Left $caller.Session -Right $liveSession -Ready $workerHandshake)) `
        -TaskId $id -Evidence ("live=[$liveSession] recorded=[$($caller.Session)] handshake=" + (Protect-Text (Get-Json $workerHandshake) 300))

    $params = @{
        kind     = 'partial'
        summary  = 'ACCEPT-PARTIAL: only half of the work is done'
        branch   = $caller.Branch
        evidence = @('nothing yet')
        checks   = @('nothing yet')
        risk     = 'ACCEPT-PARTIAL unknown'
    }
    $answer = Invoke-BridgeAs -LaunchPaneId $caller.LaunchPane -Method 'report' -Params $params
    Assert-True -Id 'PR2.1' -What "the bridge refuses a partial report from $id" -Condition (-not $answer.Ok) -TaskId $id `
        -Command (Get-PluginExe) -Evidence (Protect-Text (Get-Json $answer.Error) 400)
    Assert-Equal -Id 'PR2.2' -What 'the refusal is classified as invalid_report' -Expected 'invalid_report' -Actual ([string](Get-Field $answer.Error 'code')) -TaskId $id
    Assert-Match -Id 'PR2.3' -What 'the refusal explains that only a complete result is final' -Pattern 'not a final result' -Actual ([string](Get-Field $answer.Error 'message')) -TaskId $id

    $after = Get-TaskById -Id $id
    $factAfter = Get-Field $after 'fact'
    Assert-True -Id 'PR3.1' -What "the refused report wrote no report and changed no stage fact on $id" `
        -Condition ((Get-TaskProjection -Fact $factAfter) -ceq (Get-TaskProjection -Fact $fact)) -TaskId $id `
        -Evidence ("after=[" + (Protect-Text (Get-TaskProjection -Fact $factAfter) 600) + "] before=[" + (Protect-Text (Get-TaskProjection -Fact $fact) 600) + "]")
    Assert-Equal -Id 'PR3.2' -What "the task is still Running after the refusal" -Expected 'running' -Actual (Get-TaskStage -Fact $factAfter) -TaskId $id
    Assert-True -Id 'PR3.3' -What "no report was recorded for $id" -Condition ($null -eq (Get-Field $factAfter 'Report' $null)) -TaskId $id

    $params = @{ kind = 'result'; summary = 'ACCEPT-EMPTY'; branch = $caller.Branch; evidence = @(); checks = @(); risk = 'none' }
    $empty = Invoke-BridgeAs -LaunchPaneId $caller.LaunchPane -Method 'report' -Params $params
    Assert-True -Id 'PR4.1' -What 'a result with no evidence is refused' -Condition (-not $empty.Ok) -TaskId $id -Evidence (Protect-Text (Get-Json $empty.Error) 400)
    Assert-Equal -Id 'PR4.2' -What 'that refusal is also invalid_report' -Expected 'invalid_report' -Actual ([string](Get-Field $empty.Error 'code')) -TaskId $id

    # Issue #20: a check has to say how it ended. Everything else in this result is valid,
    # so the missing outcome is the only reason to refuse it.
    $params = @{ kind = 'result'; summary = 'ACCEPT-NO-OUTCOME'; branch = $caller.Branch; evidence = @('commit 0123456'); checks = @('go test ./...'); risk = 'none' }
    $noOutcome = Invoke-BridgeAs -LaunchPaneId $caller.LaunchPane -Method 'report' -Params $params
    Assert-True -Id 'PR4.3' -What 'a result whose check states no outcome is refused' -Condition (-not $noOutcome.Ok) -TaskId $id -Evidence (Protect-Text (Get-Json $noOutcome.Error) 400)
    Assert-Equal -Id 'PR4.4' -What 'that refusal is invalid_report' -Expected 'invalid_report' -Actual ([string](Get-Field $noOutcome.Error 'code')) -TaskId $id
    Assert-Match -Id 'PR4.5' -What 'the refusal tells the worker which outcomes a check may state' -Pattern 'passed, failed or skipped' -Actual ([string](Get-Field $noOutcome.Error 'message')) -TaskId $id
    $params = @{ kind = 'result'; summary = 'ACCEPT-BAD-OUTCOME'; branch = $caller.Branch; evidence = @('commit 0123456'); checks = @(@{ name = 'go test ./...'; outcome = 'ok' }); risk = 'none' }
    $badOutcome = Invoke-BridgeAs -LaunchPaneId $caller.LaunchPane -Method 'report' -Params $params
    Assert-Equal -Id 'PR4.6' -What 'a check with an outcome outside passed/failed/skipped is invalid_report too' -Expected 'invalid_report' -Actual ([string](Get-Field $badOutcome.Error 'code')) -TaskId $id

    $preview = Invoke-BridgeAs -LaunchPaneId $script:PrimaryPane -Method 'cleanup_preview' -Params @{ taskId = $id }
    Assert-True -Id 'PR5.1' -What 'cleanup_preview answers for a running task' -Condition $preview.Ok -TaskId $id -Evidence (Protect-Text (Get-Json $preview.Error) 300)
    if ($preview.Ok) {
        Assert-JsonFalse -Id 'PR5.2' -What 'a running task is not removable (a real boolean)' -Object $preview.Result -Name 'removable' -TaskId $id
        Assert-True -Id 'PR5.3' -What 'the preview names the exact recorded checkout path' -Condition (Test-SamePath -A (Get-Field $preview.Result 'path') -B ([string](Get-Field $fact 'WorktreePath'))) -TaskId $id
    }
    $cleanup = Invoke-BridgeAs -LaunchPaneId $script:PrimaryPane -Method 'cleanup' -Params @{ taskId = $id }
    Assert-True -Id 'PR5.3b' -What 'cleanup answered with a bridge envelope (ok or a coded error)' -Condition $cleanup.Envelope -TaskId $id `
        -Evidence (Protect-Text $cleanup.Raw 300)
    Assert-Equal -Id 'PR5.4' -What 'cleanup refuses a task with no reviewed result' -Expected 'not_reviewed' -Actual ([string](Get-Field $cleanup.Error 'code')) -TaskId $id
    Assert-True -Id 'PR5.5' -What 'the refused cleanup removed nothing' -Condition (Test-Path -LiteralPath ([string](Get-Field $fact 'WorktreePath'))) -TaskId $id

    # The store must still be byte-identical for this task after all refusals.
    $final = Get-TaskById -Id $id
    Assert-True -Id 'PR5.6' -What "every refusal above left $id's stage facts unchanged" `
        -Condition ((Get-TaskProjection -Fact (Get-Field $final 'fact')) -ceq (Get-TaskProjection -Fact $fact)) -TaskId $id
}

# ---------------------------------------------------------------------------
# Phase 11: an ordinary worker question relayed by the primary
# ---------------------------------------------------------------------------

function Get-TaskByTitle {
    param([string]$Pattern)
    $tasks = Get-ProjectTasks -Key (Get-Field $script:Project 'Key')
    $match = @($tasks | Where-Object { ([string](Get-Field (Get-Field $_ 'fact') 'Title')) -match $Pattern })
    if ($match.Count -eq 0) { return $null }
    return $match[0]
}

function Invoke-QuestionRelay {
    Write-Step 'Phase 11: ordinary worker question relay'
    $taskA = Get-TaskByTitle -Pattern 'ACCEPT A'
    Assert-True -Id 'Q1.1' -What 'task A is identifiable by its exact title' -Condition ($null -ne $taskA) -Evidence (Protect-Text (Get-Json @($script:Tasks | ForEach-Object { Get-Field (Get-Field $_ 'fact') 'Title' })) 300)
    if (-not $taskA) { Block-Item -Id 'Q1.2' -What 'worker question relay' -Evidence 'task A was not dispatched'; return }
    $script:TaskA = Get-Field $taskA 'fact'
    $idA = [string](Get-Field $script:TaskA 'ID')

    # Task A's dispatch brief instructs it to ask before waiting for the answer
    # token, so this phase observes the real question rather than sending a second
    # worker prompt that could accidentally release the report gate.
    Assert-Match -Id 'Q1.3' -What 'task A brief requests the ordinary question and exact answer token' `
        -Pattern 'orch_report.*kind "question".*ACCEPT-Q-A.*ACCEPT-ANSWER-A' -Actual $script:BriefA -TaskId $idA

    Set-PendingUserInteraction -Pending $true -Phase 'awaiting-worker-question-and-user-answer'
    $wait = Wait-Until -TimeoutSeconds 420 -IntervalSeconds 10 -What 'worker question' -Probe {
        $row = Get-TaskById -Id $idA
        if (-not $row) { return $null }
        $fact = Get-Field $row 'fact'
        if ([string](Get-Field $fact 'Attention') -match 'worker question:.*ACCEPT-Q-A') { return $fact }
        return $null
    }
    Assert-True -Id 'Q2.1' -What "worker A's question is recorded as attention" -Condition $wait.Ok -TaskId $idA `
        -Evidence ("after {0:N0}s: attention=[{1}]" -f $wait.Elapsed, [string](Get-Field (Get-Field (Get-TaskById -Id $idA) 'fact' $null) 'Attention'))
    if ($wait.Ok) {
        $script:TaskA = $wait.Value
        Assert-Equal -Id 'Q2.2' -What 'the task is Needs input while the question is open' -Expected 'needs_input' -Actual (Get-TaskStage -Fact $script:TaskA) -TaskId $idA
        $dump = Get-StateDump
        $note = @($dump.tables.inbox | Where-Object { (Get-Field $_ 'task_id') -eq $idA -and (Get-Field $_ 'kind') -eq 'question' })
        Assert-Equal -Id 'Q2.3' -What 'exactly one durable question note exists for task A' -Expected 1 -Actual $note.Count -TaskId $idA
        if ($note.Count -gt 0) {
            Assert-Match -Id 'Q2.4' -What 'the note carries the worker question verbatim' -Pattern 'ACCEPT-Q-A' -Actual ([string](Get-Field $note[0] 'summary')) -TaskId $idA
            Assert-Equal -Id 'Q2.5' -What 'the note is a worker question delivery' -Expected 'worker question' -Actual ([string](Get-Field $note[0] 'delivery')) -TaskId $idA
            Add-Assertion -Id 'Q2.6' -What 'inbox delivery state (the plugin delivers notes to an idle primary)' -Status 'INFO' `
                -Evidence ("state={0} detail={1}" -f (Get-Field $note[0] 'delivery_state'), (Protect-Text (Get-Field $note[0] 'delivery_detail') 200))
        }
        $ready = Get-ReadySession -LaunchPaneId ([string](Get-Field $script:Project 'LaunchPaneID'))
        $transcriptPath = [string](Get-Field $ready 'omp_session_file')
        $delivery = Wait-Until -TimeoutSeconds 120 -IntervalSeconds 3 -What 'worker question in primary OMP transcript' -Probe {
            if (Test-OmpTranscriptContainsWorkerQuestionNote -Path $transcriptPath -TaskId $idA -QuestionMarker 'ACCEPT-Q-A') { return $true }
            return $null
        }
        Assert-True -Id 'Q2.7' -What 'the exact task-A question note was prompted into the primary OMP session' -Condition $delivery.Ok -TaskId $idA `
            -Evidence ("taskId=$idA question marker ACCEPT-Q-A observed after {0:N0}s; path={1}" -f $delivery.Elapsed, (Protect-Text $transcriptPath 300))
    }

    $focusBefore = Get-FocusProjection -Snapshot (Get-Snapshot)

    $primaryReady = Get-ReadySession -LaunchPaneId ([string](Get-Field $script:Project 'LaunchPaneID'))
    $primaryTranscript = [string](Get-Field $primaryReady 'omp_session_file')
    Set-PendingUserInteraction -Pending $true -Phase 'awaiting-direct-authorization-token'
    $authorization = Wait-Until -TimeoutSeconds 120 -IntervalSeconds 3 -What 'later user authorization token for worker A' -Probe {
        if (Test-OmpTranscriptContainsLaterUserAuthorization -Path $primaryTranscript -TaskId $idA `
            -QuestionMarker 'ACCEPT-Q-A' -Token 'ACCEPT-ANSWER-A') { return $true }
        return $null
    }
    if (-not $authorization.Ok) {
        Preserve-ResourcesForPendingTool -AssertionId 'Q2.8b.resources-preserved' -Reason 'worker A is still gated on direct user authorization'
        $screen = Get-PaneScreen -PaneId $script:PrimaryPane -Source 'recent-unwrapped' -Lines 100
        Block-Item -Id 'Q2.8b' -What 'worker A remains gated because no later user message supplied the exact authorization token' `
            -Evidence ("token not found after the worker-question note; " + (Protect-Text $screen 500))
        Block-Item -Id 'Q3.1' -What 'no worker answer was relayed because exact user authorization is absent' `
            -Evidence 'No orch_answer call or report prompt was submitted.'
        $focusAfter = Get-FocusProjection -Snapshot (Get-Snapshot)
        Assert-Equal -Id 'Q3.5' -What 'relaying a question never moved the session focus' -Expected $focusBefore -Actual $focusAfter
        return
    }
    Add-Assertion -Id 'Q2.8a' -What 'a later user message after the worker-question note contains the exact authorization token' `
        -Status 'PASS' -Evidence "observed in $primaryTranscript" -TaskId $idA
    $relayPrompt = 'ACCEPT-Q-A-RELAY. The primary transcript contains a later user message with the exact ACCEPT-ANSWER-A token. Call orch_answer for taskId "' + $idA + '" using that user message verbatim; do not add or change any authorization text. Do not report. Reply relayed.'
    # orch_answer now waits for the user to confirm the exact text, so the prompt is
    # submitted without --wait and Confirm-PrimaryAnswerModal gates the Enter.
    $relay = Invoke-PrimaryPromptNoWaitAfterDispositionAsks -PaneId $script:PrimaryPane -Text $relayPrompt -TimeoutSeconds 300 -AssertionPrefix 'Q3'
    if (-not $relay.Submitted) {
        Block-Item -Id 'Q3.1' -What 'the primary could not relay the already-authorized user answer' `
            -Evidence ("code=[$($relay.Code)] screen=" + (Protect-Text $relay.Screen 500))
        $focusAfter = Get-FocusProjection -Snapshot (Get-Snapshot)
        Assert-Equal -Id 'Q3.5' -What 'relaying a question never moved the session focus' -Expected $focusBefore -Actual $focusAfter
        Preserve-ResourcesForPendingTool -AssertionId 'Q3.1.resources-preserved' -Reason 'the primary did not relay the authorized worker answer'
        return
    }
    $answerDelivery = Wait-Until -TimeoutSeconds 120 -IntervalSeconds 3 -What 'exact authorized orch_answer call after the direct user token' -Probe {
        $authorizedText = Get-OmpAnswerCallAfterUserAuthorization -Path $primaryTranscript `
            -QuestionMarker 'ACCEPT-Q-A' -Token 'ACCEPT-ANSWER-A' -TaskId $idA
        if (-not [string]::IsNullOrEmpty($authorizedText)) { return $authorizedText }
        return $null
    }
    if (-not $answerDelivery.Ok) {
        Block-Item -Id 'Q3.1' -What 'the authorized orch_answer call was not observed after the direct user token' `
            -Evidence ("tokenized tool call not found; " + (Protect-Text (Get-PaneScreen -PaneId $script:PrimaryPane -Source 'recent-unwrapped' -Lines 100) 500))
        $focusAfter = Get-FocusProjection -Snapshot (Get-Snapshot)
        Assert-Equal -Id 'Q3.5' -What 'relaying a question never moved the session focus' -Expected $focusBefore -Actual $focusAfter
        Preserve-ResourcesForPendingTool -AssertionId 'Q3.1.resources-preserved' -Reason 'the direct user token arrived but orch_answer was not proven'
        return
    }
    $workerReadyA = Get-ReadySession -LaunchPaneId ([string](Get-Field $script:TaskA 'LaunchPaneID'))
    $answerModal = Confirm-PrimaryAnswerModal -TaskId $idA -Text ([string]$answerDelivery.Value) -TranscriptPath $primaryTranscript `
        -WorkerTranscriptPath ([string](Get-Field $workerReadyA 'omp_session_file'))
    Assert-True -Id 'Q3.1b' -What 'the authorized answer had not reached worker A before the user confirmed it' `
        -Condition (-not $answerModal.DeliveredBeforeConfirm) -TaskId $idA -Evidence ("observed=[$($answerModal.Observed)] delivered_before_confirm=[$($answerModal.DeliveredBeforeConfirm)]")
    if (-not $answerModal.Confirmed) {
        Block-Item -Id 'Q3.1' -What 'the answer confirmation for worker A did not show the exact authorized text, so nothing was confirmed' `
            -Evidence ("observed=[$($answerModal.Observed)] screen=" + (Protect-Text $answerModal.Screen 500))
        $focusAfter = Get-FocusProjection -Snapshot (Get-Snapshot)
        Assert-Equal -Id 'Q3.5' -What 'relaying a question never moved the session focus' -Expected $focusBefore -Actual $focusAfter
        Preserve-ResourcesForPendingTool -AssertionId 'Q3.1.resources-preserved' -Reason 'the answer confirmation for worker A may still be open on the primary'
        return
    }
    Assert-True -Id 'Q3.1' -What 'the primary relayed the exact later user authorization to worker A after the user confirmed that text' `
        -Condition $true -TaskId $idA -Command $relay.CommandLine `
        -Evidence 'orch_answer taskId and text matched the exact direct user message after authorization, and the answer dialog showed that exact text before it was confirmed'
    $cleared = Wait-Until -TimeoutSeconds 240 -IntervalSeconds 8 -What 'question answered' -Probe {
        $row = Get-TaskById -Id $idA
        if (-not $row) { return $null }
        $fact = Get-Field $row 'fact'
        if ([string]::IsNullOrWhiteSpace([string](Get-Field $fact 'Attention'))) { return $fact }
        return $null
    }
    Assert-True -Id 'Q3.2' -What 'the question is cleared once it has been answered' -Condition $cleared.Ok -TaskId $idA `
        -Evidence ("after {0:N0}s: attention=[{1}]" -f $cleared.Elapsed, [string](Get-Field (Get-Field (Get-TaskById -Id $idA) 'fact' $null) 'Attention'))
    if ($cleared.Ok) {
        $script:TaskA = $cleared.Value
        $answerStage = Get-TaskStage -Fact $script:TaskA
        Assert-True -Id 'Q3.3' -What 'the answered task is Running or has finished its report' -Condition ($answerStage -in @('running', 'ready_for_review')) -TaskId $idA -Evidence "stage=$answerStage"
        Set-PendingUserInteraction -Pending $false -Phase 'worker-question-answered'
    } else {
        Preserve-ResourcesForPendingTool -AssertionId 'Q3.2.resources-preserved' -Reason 'worker A attention remained open after the answer relay'
    }
    $workerScreen = Get-PaneScreen -PaneId ([string](Get-Field $script:TaskA 'PublicPaneID')) -Source 'recent-unwrapped' -Lines 200
    $answeredRow = Get-TaskById -Id $idA
    $answeredFact = Get-Field $answeredRow 'fact'
    $answeredStage = Get-TaskStage -Fact $answeredFact
    $answerProgressed = $cleared.Ok -and ($answeredStage -in @('running', 'ready_for_review'))
    if ($answerProgressed) {
        Assert-True -Id 'Q3.4' -What 'the authorized answer left the worker Running or Ready for review' -Condition $true -TaskId $idA `
            -Evidence ("stage=$answeredStage report=$($null -ne (Get-Field $answeredFact 'Report' $null)) answer_visible=$($workerScreen -match 'ACCEPT-ANSWER-A')")
    } else {
        Block-Item -Id 'Q3.4' -What 'the authorized answer did not advance the worker task' -Evidence ("stage=$answeredStage screen=" + (Protect-Text $workerScreen 400))
    }
    $focusAfter = Get-FocusProjection -Snapshot (Get-Snapshot)
    Assert-Equal -Id 'Q3.5' -What 'relaying a question never moved the session focus' -Expected $focusBefore -Actual $focusAfter
}

# ---------------------------------------------------------------------------
# Phase 12: a real blocked full-screen dialog approved with specific keys
# ---------------------------------------------------------------------------

function Send-AgentKeys {
    param([Parameter(Mandatory)][string]$PaneId, [Parameter(Mandatory)][string[]]$Keys)
    return Invoke-Herdr -Arguments (@('agent', 'send-keys', $PaneId) + $Keys) -TimeoutSeconds 60
}

function Get-PrimaryAskId {
    param([string]$Screen)
    if ([string]::IsNullOrWhiteSpace($Screen)) { return '' }
    if (Get-PrimaryConsentDialogTitle -Screen $Screen) { return '' }
    $askIndex = $Screen.LastIndexOf('Ask 1 questions', [System.StringComparison]::OrdinalIgnoreCase)
    if ($askIndex -lt 0) { return '' }
    $panel = $Screen.Substring($askIndex)
    $match = [regex]::Match($panel, '\[(?<id>[a-z0-9_-]+)\]', [System.Text.RegularExpressions.RegexOptions]::IgnoreCase)
    if (-not $match.Success) { return '' }
    $id = $match.Groups['id'].Value.ToLowerInvariant()
    if ($id -in @('disposition_a', 'disposition_b')) {
        return $id -replace '^disposition_', 'worker_' -replace '$', '_disposition'
    }
    return $id
}

function Test-PrimaryDispositionAskScreen {
    param([string]$Screen, [string]$AskId, [string]$TaskId = '')
    if ([string]::IsNullOrWhiteSpace($Screen) -or [string]::IsNullOrWhiteSpace($AskId)) { return $false }
    $askIndex = $Screen.LastIndexOf('Ask 1 questions', [System.StringComparison]::OrdinalIgnoreCase)
    if ($askIndex -lt 0) { return $false }
    # Only inspect the active modal; older task markers in scrollback must never
    # authorize Enter against a different Ask.
    $panel = $Screen.Substring($askIndex)
    if ((Get-PrimaryAskId -Screen $panel) -ine $AskId) { return $false }
    if ($AskId -in @('task_a_review', 'task_b_review')) {
        $acceptIndex = $panel.IndexOf('Accept report', [System.StringComparison]::OrdinalIgnoreCase)
        $changesIndex = $panel.IndexOf('Request changes', [System.StringComparison]::OrdinalIgnoreCase)
        return ($acceptIndex -ge 0 -and $changesIndex -gt $acceptIndex)
    }
    if ($AskId -notin @('worker_a_disposition', 'worker_b_disposition') -or [string]::IsNullOrWhiteSpace($TaskId)) { return $false }
    $taskRow = Get-TaskById -Id $TaskId
    $fact = Get-Field $taskRow 'fact'
    $report = Get-Field $fact 'Report' $null
    if (-not $taskRow -or $null -eq $report -or
        -not [string]::IsNullOrWhiteSpace([string](Get-Field $fact 'Disposition'))) { return $false }
    $commit = ''
    foreach ($item in @(Get-Field $report 'Evidence' @())) {
        $match = [regex]::Match([string]$item, '(?i)\b[0-9a-f]{40}\b')
        if ($match.Success) { $commit = $match.Value.ToLowerInvariant(); break }
    }
    if (-not $commit) { return $false }
    $worker = if ($AskId -eq 'worker_a_disposition') { 'Worker A' } else { 'Worker B' }
    $paths = @(Get-Field $fact 'ReservedPaths' @())
    $summary = [string](Get-Field $report 'Summary')
    $expectedFile = ''
    if ($paths.Count -gt 0) { $expectedFile = [string]$paths[0] }
    if (-not $expectedFile) {
        $fileMatch = [regex]::Match($summary, '(?i)\b[\w.-]+\.[a-z0-9]+\b')
        if ($fileMatch.Success) { $expectedFile = $fileMatch.Value }
    }
    if (-not $expectedFile -or
        $panel.IndexOf("$worker reports commit $commit", [System.StringComparison]::OrdinalIgnoreCase) -lt 0 -or
        $panel.IndexOf($expectedFile, [System.StringComparison]::OrdinalIgnoreCase) -lt 0 -or
        $panel.IndexOf('What disposition should I record?', [System.StringComparison]::OrdinalIgnoreCase) -lt 0) { return $false }
    $acceptIndex = $panel.IndexOf('Accept', [System.StringComparison]::OrdinalIgnoreCase)
    $acceptDescriptionIndex = $panel.IndexOf('Record the reported result as accepted; no merge or cleanup is performed.', [System.StringComparison]::OrdinalIgnoreCase)
    $changesIndex = $panel.IndexOf('Request changes', [System.StringComparison]::OrdinalIgnoreCase)
    $changesDescriptionIndex = $panel.IndexOf('Keep the result unaccepted and specify what should change.', [System.StringComparison]::OrdinalIgnoreCase)
    $otherIndex = $panel.IndexOf('Other (type your own)', [System.StringComparison]::OrdinalIgnoreCase)
    return ($acceptIndex -ge 0 -and $acceptDescriptionIndex -gt $acceptIndex -and
        $changesIndex -gt $acceptDescriptionIndex -and $changesDescriptionIndex -gt $changesIndex -and
        $otherIndex -gt $changesDescriptionIndex)
}

function Test-PrimaryWorkerQuestionAskScreen {
    param([string]$Screen, [string]$AskId, [string]$QuestionText, [string]$AnswerToken)
    if ([string]::IsNullOrWhiteSpace($Screen) -or [string]::IsNullOrWhiteSpace($AskId) -or
        [string]::IsNullOrWhiteSpace($QuestionText) -or [string]::IsNullOrWhiteSpace($AnswerToken)) { return $false }
    $askIndex = $Screen.LastIndexOf('Ask 1 questions', [System.StringComparison]::OrdinalIgnoreCase)
    if ($askIndex -lt 0) { return $false }
    $panel = $Screen.Substring($askIndex)
    if ((Get-PrimaryAskId -Screen $panel) -ine $AskId) { return $false }
    $questionIndex = $panel.IndexOf($QuestionText, [System.StringComparison]::OrdinalIgnoreCase)
    if ($questionIndex -lt 0) { return $false }
    $questionTokenIndex = $panel.IndexOf($AnswerToken, $questionIndex, [System.StringComparison]::Ordinal)
    if ($questionTokenIndex -le $questionIndex) { return $false }
    $yesIndex = $panel.IndexOf('Yes', $questionTokenIndex, [System.StringComparison]::OrdinalIgnoreCase)
    if ($yesIndex -le $questionTokenIndex) { return $false }
    $yesTokenIndex = $panel.IndexOf($AnswerToken, $yesIndex, [System.StringComparison]::Ordinal)
    if ($yesTokenIndex -le $yesIndex) { return $false }
    $authorizeIndex = $panel.IndexOf('Authorize reporting', $yesTokenIndex, [System.StringComparison]::OrdinalIgnoreCase)
    if ($authorizeIndex -le $yesTokenIndex) { return $false }
    $noIndex = $panel.IndexOf('No', $authorizeIndex, [System.StringComparison]::OrdinalIgnoreCase)
    if ($noIndex -le $authorizeIndex) { return $false }
    $noAuthorizeIndex = $panel.IndexOf('do not authorize', $noIndex, [System.StringComparison]::OrdinalIgnoreCase)
    return ($noAuthorizeIndex -gt $noIndex)
}

function Test-PrimaryWorkerAnswerChoiceAskScreen {
    param([string]$Screen, [string]$AskId, [string]$QuestionText)
    if ([string]::IsNullOrWhiteSpace($Screen) -or [string]::IsNullOrWhiteSpace($AskId) -or
        [string]::IsNullOrWhiteSpace($QuestionText)) { return $false }
    $askIndex = $Screen.LastIndexOf('Ask 1 questions', [System.StringComparison]::OrdinalIgnoreCase)
    if ($askIndex -lt 0) { return $false }
    $panel = $Screen.Substring($askIndex)
    if ((Get-PrimaryAskId -Screen $panel) -ine $AskId) { return $false }
    $fileMatch = [regex]::Match($QuestionText, '(?i)\b[\w.-]+\.[a-z0-9]+\b')
    if (-not $fileMatch.Success) { return $false }
    if ($panel.IndexOf($QuestionText, [System.StringComparison]::OrdinalIgnoreCase) -ge 0) {
        $yesLabel = 'Yes, keep it'
        $yesDescription = 'Preserve the current style while adding subtract.'
        $noLabel = 'No, change it'
        $noDescription = 'Have the worker change to another export style.'
    } else {
        $expectedQuestion = "How should I answer the worker's question about $($fileMatch.Value) export style?"
        if ($panel.IndexOf($expectedQuestion, [System.StringComparison]::OrdinalIgnoreCase) -lt 0) { return $false }
        $yesLabel = 'Yes, keep the existing style'
        $yesDescription = 'Preserve current exports and follow their pattern for subtract.'
        $noLabel = 'No, use a different style'
        $noDescription = 'Give the worker a different export style instruction.'
    }
    $yesIndex = $panel.IndexOf($yesLabel, [System.StringComparison]::OrdinalIgnoreCase)
    $yesDescriptionIndex = $panel.IndexOf($yesDescription, [System.StringComparison]::OrdinalIgnoreCase)
    $noIndex = $panel.IndexOf($noLabel, [System.StringComparison]::OrdinalIgnoreCase)
    $noDescriptionIndex = $panel.IndexOf($noDescription, [System.StringComparison]::OrdinalIgnoreCase)
    $otherIndex = $panel.IndexOf('Other (type your own)', [System.StringComparison]::OrdinalIgnoreCase)
    return ($yesIndex -ge 0 -and $yesDescriptionIndex -gt $yesIndex -and $noIndex -gt $yesDescriptionIndex -and
        $noDescriptionIndex -gt $noIndex -and $otherIndex -gt $noDescriptionIndex)
}

function Test-PrimaryWorkerQuestionAskForTask {
    param([string]$Screen, [string]$QuestionText, [string]$AnswerToken)
    $askId = Get-PrimaryAskId -Screen $Screen
    if (-not $askId) { return $false }
    return ((Test-PrimaryWorkerQuestionAskScreen -Screen $Screen -AskId $askId -QuestionText $QuestionText -AnswerToken $AnswerToken) -or
        (Test-PrimaryWorkerAnswerChoiceAskScreen -Screen $Screen -AskId $askId -QuestionText $QuestionText))
}

function Test-PrimaryReviewConfirmScreen {
    <#
        True only when the primary's ACTIVE confirmation is the review dialog of
        recordReviewWithConsent (omp/herdr-orchestrator.ts) for exactly this task,
        report revision, outcome and note: the dialog names the outcome first, then
        the note on its own line, then the stored report, and ends with the sentence
        its outcome implies (an accepting outcome may start queued dependent work,
        any other keeps dependents queued). The outcome is the one the harness
        expects the real orch_review call to carry (Test-OmpReviewToolCall checks the
        transcript for the same value), so a dialog for a different decision refuses.
    #>
    param([string]$Screen, [string]$TaskId, [long]$Revision, [string]$Disposition, [string]$Outcome = 'integrated')
    if ([string]::IsNullOrWhiteSpace($Screen) -or [string]::IsNullOrWhiteSpace($TaskId) -or
        [string]::IsNullOrWhiteSpace($Disposition) -or [string]::IsNullOrWhiteSpace($Outcome) -or $Revision -le 0) { return $false }
    $start = $Screen.LastIndexOf('Record review disposition', [System.StringComparison]::Ordinal)
    if ($start -lt 0) { return $false }
    $panel = $Screen.Substring($start)
    $normalized = ConvertTo-ConfirmPanelText -Panel $panel
    $description = "Confirm that you reviewed the worker's final report for task $TaskId and chose this outcome:"
    $descriptionIndex = $normalized.IndexOf($description, [System.StringComparison]::Ordinal)
    if ($descriptionIndex -lt 0) { return $false }
    $afterDescription = $normalized.Substring($descriptionIndex + $description.Length).TrimStart()
    # The extension quotes the note (quoteForDialog). Only whitespace is folded, as
    # on the screen: a pipe or a box character inside the note is data and must match.
    $quotedNote = (ConvertTo-JsonStringLiteral -Text $Disposition) -replace '\s+', ' '
    $expectedDecision = $Outcome.Trim() + ' Note: ' + $quotedNote + " Stored report (revision ${Revision}):"
    $closing = if ($Outcome -ceq 'integrated' -or $Outcome -ceq 'kept') {
        'This closes the task and may start queued dependent work.'
    } else {
        'This closes the task and frees its slot and paths, but tasks that depend on it stay queued.'
    }
    if (-not $afterDescription.StartsWith($expectedDecision, [System.StringComparison]::Ordinal) -or
        $afterDescription.IndexOf($closing, [System.StringComparison]::Ordinal) -lt 0) { return $false }
    return (Test-ConfirmChoicesEndPanel -Panel $panel)
}

function ConvertTo-ConfirmPanelText {
    <#
        One confirmation panel as a single line of text. Only the modal's chrome at
        each line's edges is removed: the vertical border, and the box-drawing run
        OMP draws around the title in the top border (corner, dashes, title, dashes,
        corner). Whitespace runs fold to one space. A pipe or a box character inside
        a quoted value is part of the text and stays, so it must match exactly.
    #>
    param([string]$Panel)
    $visibleLines = foreach ($line in ($Panel -split "`r?`n")) {
        $line.Trim() -replace '^[\u2500-\u257F\u00B3|](?:\s*[\u2500-\u257F\u00B3|])*\s*', '' -replace '\s*[\u2500-\u257F\u00B3|](?:\s*[\u2500-\u257F\u00B3|])*$', ''
    }
    return ((@($visibleLines) -join ' ') -replace '\s+', ' ').Trim()
}

function Test-ConfirmChoicesEndPanel {
    <#
        True only when an OMP confirmation panel ends in exactly its Yes/No choices:
        nothing but the selector's key-hint footer, the modal's closing border and
        OMP's single status-bar line may follow "No". Any other text could be a
        newer Ask or action modal, so it fails closed.
    #>
    param([string]$Panel)
    $choices = @()
    $seenNo = $false
    $modalClosed = $false
    $statusBarSeen = $false
    foreach ($line in ($Panel -split "`r?`n")) {
        $trimmed = $line.Trim()
        if ($trimmed.Length -eq 0) { continue }
        if ($modalClosed) {
            # After the modal's closing border only OMP's single status-bar line
            # (several middle-dot separated segments) may follow; any other text could
            # be a newer Ask or action modal, so it fails closed.
            if ($statusBarSeen -or ([regex]::Matches($trimmed, '\s\u00B7\s').Count -lt 3)) { return $false }
            $statusBarSeen = $true
            continue
        }
        if ($trimmed[0] -eq [char]0x2570) { $modalClosed = $true; continue }
        $label = $trimmed -replace '^[^\p{L}]*', '' -replace '[^\p{L}]*$', ''
        # OMP's selector draws its key hints under the choices; that exact line is
        # chrome, not another active panel. OMP draws it either in words
        # ("up/down navigate  enter select  esc cancel") or with an arrow pair and
        # icon glyphs ("<arrows> navigate <icon> select <icon> cancel"); the leading
        # arrows are already stripped from $label. Anything else after "No" still fails closed.
        if ($seenNo -and ($label -cmatch '^up/down navigate\s+enter select\s+esc cancel$' -or
                $label -cmatch '^navigate\s+[\p{Co}\p{Cs}]+\s+select\s+[\p{Co}\p{Cs}]+\s+cancel$')) { continue }
        if ($seenNo -and $label -match '[\p{L}\p{N}]') { return $false }
        if ($label -ceq 'Yes' -or $label -ceq 'No') { $choices += $label }
        if ($label -ceq 'No') { $seenNo = $true }
    }
    return ($choices.Count -eq 2 -and $choices[0] -ceq 'Yes' -and $choices[1] -ceq 'No')
}

function Get-ExtensionConsentDialogTitles {
    <#
        The titles of the extension's consent dialogs: every ConfirmRequest in
        omp/herdr-orchestrator.ts (the helper tests compare this list with that
        source). Each heads an OMP Yes/No panel, and the extension marks the
        primary blocked in Herdr while one is open.
    #>
    return @(
        'Approve worker choice',
        'Settle interrupted checkout cleanup',
        'Remove worker checkout',
        'Settle worker input',
        'Decide an uncertain dispatch',
        'Record review disposition',
        'Move ticket to todo',
        'Dispatch work without a ticket',
        'Record ticket decision',
        'Send answer to worker',
        'Re-point queued dependents'
    )
}

function Get-PrimaryConsentDialogTitle {
    <#
        The title of the extension consent dialog that is the primary's ACTIVE
        panel, or $null. The panel starts at the newest line whose text, without the
        modal's edge chrome, is exactly one of the titles (OMP draws it in the top
        border), and it must end in its Yes/No choices (Test-ConfirmChoicesEndPanel).
        A tool header that names the dialog, a title quoted inside a value, an older
        dialog in the scrollback and a newer Ask or action below all fail closed. The
        title is evidence only: the one thing the harness does to a dialog no step
        asked for is decline it.
    #>
    param([string]$Screen)
    if ([string]::IsNullOrWhiteSpace($Screen)) { return $null }
    $titles = @(Get-ExtensionConsentDialogTitles)
    $lines = @($Screen -split "`r?`n")
    for ($index = $lines.Count - 1; $index -ge 0; $index--) {
        $heading = ConvertTo-ConfirmPanelText -Panel $lines[$index]
        if ($titles -cnotcontains $heading) { continue }
        if (Test-ConfirmChoicesEndPanel -Panel (@($lines[$index..($lines.Count - 1)]) -join "`n")) { return $heading }
        return $null
    }
    return $null
}

function Test-PrimaryApprovalConfirmScreen {
    param([string]$Screen, [string]$DialogMarker, [Parameter(Mandatory)][string]$ExpectedQuestion, [string[]]$Keys)
    if ([string]::IsNullOrWhiteSpace($Screen) -or [string]::IsNullOrWhiteSpace($DialogMarker) -or
        [string]::IsNullOrWhiteSpace($ExpectedQuestion) -or @($Keys).Count -eq 0) { return $false }
    $markerIndex = $Screen.LastIndexOf($DialogMarker, [System.StringComparison]::OrdinalIgnoreCase)
    if ($markerIndex -lt 0) { return $false }
    $askIndex = $Screen.LastIndexOf('Ask', $markerIndex, [System.StringComparison]::OrdinalIgnoreCase)
    if ($askIndex -lt 0) { return $false }
    $questionFound = $false
    foreach ($line in ($Screen -split "`r?`n")) {
        $candidate = $line.Trim() -replace '^[\u2502\u00B3|](?:\s*[\u2502\u00B3|])*\s*', '' -replace '\s*[\u2502\u00B3|](?:\s*[\u2502\u00B3|])*$',''
        if ($candidate -ceq $ExpectedQuestion) { $questionFound = $true; break }
    }
    if (-not $questionFound) { return $false }
    $keepIndex = $Screen.IndexOf('keep the current contract', $markerIndex, [System.StringComparison]::OrdinalIgnoreCase)
    $changeIndex = $Screen.IndexOf('change the contract', $markerIndex, [System.StringComparison]::OrdinalIgnoreCase)
    $keysIndex = $Screen.IndexOf(('Keys: ' + ($Keys -join ' ')), [System.StringComparison]::OrdinalIgnoreCase)
    if ($keepIndex -lt $markerIndex -or $changeIndex -le $keepIndex -or $keysIndex -le $changeIndex) { return $false }
    $yesIndex = $Screen.IndexOf('Yes', $keysIndex, [System.StringComparison]::OrdinalIgnoreCase)
    if ($yesIndex -le $keysIndex) { return $false }
    $yesLineStart = $Screen.LastIndexOf("`n", $yesIndex, [System.StringComparison]::Ordinal) + 1
    $yesLineEnd = $Screen.IndexOf("`n", $yesIndex, [System.StringComparison]::Ordinal)
    if ($yesLineEnd -lt 0) { $yesLineEnd = $Screen.Length }
    $yesLabel = $Screen.Substring($yesLineStart, $yesLineEnd - $yesLineStart).Trim() -replace '^[^\p{L}]*', '' -replace '[^\p{L}]*$', ''
    if ($yesLabel -cne 'Yes') { return $false }
    $noIndex = $Screen.IndexOf('No', $yesIndex, [System.StringComparison]::OrdinalIgnoreCase)
    if ($noIndex -le $yesIndex) { return $false }
    $noLineStart = $Screen.LastIndexOf("`n", $noIndex, [System.StringComparison]::Ordinal) + 1
    $noLineEnd = $Screen.IndexOf("`n", $noIndex, [System.StringComparison]::Ordinal)
    if ($noLineEnd -lt 0) { $noLineEnd = $Screen.Length }
    $noLabel = $Screen.Substring($noLineStart, $noLineEnd - $noLineStart).Trim() -replace '^[^\p{L}]*', '' -replace '[^\p{L}]*$', ''
    return ($noLabel -ceq 'No')
}


function Confirm-PrimaryReviewModal {
    <#
        Presses Enter once on the primary's review dialog for this task, and only
        while the task's stored report is undecided at the revision the dialog
        shows, the primary transcript holds the pending orch_review call with this
        exact decision, and the visible dialog matches it as the last panel. The
        extension marks the primary blocked while its dialog is open (herdr:blocked;
        TK1.3c-blocked observes it), but the gate is any active turn (working or
        blocked): the exact call and dialog decide, not the status. The call, the
        dialog and the task are read again before the key.
    #>
    param(
        [Parameter(Mandatory)][string]$PaneId,
        [Parameter(Mandatory)][string]$LaunchPaneId,
        [Parameter(Mandatory)][string]$TaskId,
        [Parameter(Mandatory)][string]$Disposition,
        [string]$Outcome = 'integrated'
    )
    $transcript = [string](Get-Field (Get-ReadySession -LaunchPaneId $LaunchPaneId) 'omp_session_file')
    $reviewCall = @{ taskId = $TaskId; outcome = $Outcome; disposition = $Disposition }
    $wait = Wait-Until -TimeoutSeconds 120 -IntervalSeconds 2 -What "review confirmation for task $TaskId" -Probe {
        $fact = Get-Field (Get-TaskById -Id $TaskId) 'fact'
        $revision = [long](Get-Field $fact 'Revision' 0)
        if ($null -eq (Get-Field $fact 'Report' $null) -or $revision -le 0 -or
            -not [string]::IsNullOrEmpty([string](Get-Field $fact 'Disposition'))) { return $null }
        $live = Get-LiveAgent -Target $PaneId
        if (-not $live.Found -or [string](Get-Field $live.Agent 'agent_status') -notin @('working', 'blocked')) { return $null }
        if (-not (Test-OmpPendingToolCall -Path $transcript -Name 'orch_review' -Arguments $reviewCall)) { return $null }
        $screen = Get-PaneScreen -PaneId $PaneId -Source 'visible' -Lines 100
        if (Test-PrimaryReviewConfirmScreen -Screen $screen -TaskId $TaskId -Revision $revision -Disposition $Disposition -Outcome $Outcome) {
            return [pscustomobject]@{ Kind = 'modal'; Screen = $screen; Revision = $revision }
        }
        return $null
    }
    if (-not $wait.Ok) { return $false }
    if ($wait.Value.Kind -ne 'modal') { return $false }
    $revision = [long]$wait.Value.Revision
    if (-not (Test-OmpPendingToolCall -Path $transcript -Name 'orch_review' -Arguments $reviewCall)) { return $false }
    $screen = Get-PaneScreen -PaneId $PaneId -Source 'visible' -Lines 100
    if (-not (Test-PrimaryReviewConfirmScreen -Screen $screen -TaskId $TaskId -Revision $revision -Disposition $Disposition -Outcome $Outcome)) { return $false }
    $fact = Get-Field (Get-TaskById -Id $TaskId) 'fact'
    if ([long](Get-Field $fact 'Revision' 0) -ne $revision -or $null -eq (Get-Field $fact 'Report' $null) -or
        -not [string]::IsNullOrEmpty([string](Get-Field $fact 'Disposition'))) { return $false }
    $screen = Get-PaneScreen -PaneId $PaneId -Source 'visible' -Lines 100
    if (-not (Test-PrimaryReviewConfirmScreen -Screen $screen -TaskId $TaskId -Revision $revision -Disposition $Disposition -Outcome $Outcome)) { return $false }
    if (-not (Test-OmpPendingToolCall -Path $transcript -Name 'orch_review' -Arguments $reviewCall)) { return $false }
    $sent = Send-AgentKeys -PaneId $PaneId -Keys @('enter')
    [void]($script:PrimarySendKeysUsed += 'enter')
    return ($sent.ExitCode -eq 0)
}

function Test-PrimaryTicketConfirmScreen {
    <#
        True only when the primary's active dialog is exactly this ticket's move or
        decision: id/title/revision, plus the body for a move or outcome/note/tasks
        for a decision, with Yes/No as the last panel on screen. The extension's
        wording (recordTicketMoveWithConsent and recordTicketReviewWithConsent in
        omp/herdr-orchestrator.ts) is matched, so changed dialogs, older matching
        modals, a newer Ask or another action below this one all fail closed.
    #>
    param(
        [string]$Screen,
        [ValidateSet('move', 'review')][string]$Kind,
        [string]$TicketId,
        [string]$TicketTitle,
        [string]$TicketBody = '',
        [long]$Revision,
        [string]$Outcome = '',
        [string]$Disposition = '',
        [string[]]$LinkedTaskIds = @(),
        [string]$Stage = 'ready_for_review'
    )
    if ([string]::IsNullOrWhiteSpace($Screen) -or [string]::IsNullOrWhiteSpace($TicketId) -or
        [string]::IsNullOrWhiteSpace($TicketTitle) -or $Revision -le 0) { return $false }
    if ($Kind -eq 'review' -and ([string]::IsNullOrWhiteSpace($Outcome) -or [string]::IsNullOrWhiteSpace($Disposition))) { return $false }
    $heading = if ($Kind -eq 'move') { 'Move ticket to todo' } else { 'Record ticket decision' }
    $start = $Screen.LastIndexOf($heading, [System.StringComparison]::Ordinal)
    if ($start -lt 0) { return $false }
    $panel = $Screen.Substring($start)
    $normalized = ConvertTo-ConfirmPanelText -Panel $panel
    # The extension quotes ticket text with quoteForDialog; only whitespace is
    # folded, so pipes and terminal controls inside the quoted strings must match.
    $quotedTitle = (ConvertTo-JsonStringLiteral -Text $TicketTitle) -replace '\s+', ' '
    $quotedBody = (ConvertTo-JsonStringLiteral -Text $TicketBody) -replace '\s+', ' '
    if ($Kind -eq 'move') {
        $description = "Confirm moving ticket $TicketId to todo and starting its planner."
        $expectedFacts = "Title: $quotedTitle Body: $quotedBody Now: backlog (stage backlog), revision $Revision " +
            'Moving records one planner task and starts or queues its worker. The worker returns a plan only; implementation work still requires your approval and explicit dispatch.'
    } else {
        $description = "Confirm closing ticket $TicketId with this outcome: $Outcome"
        $tasksText = if (@($LinkedTaskIds).Count -gt 0) { (@($LinkedTaskIds) -join ', ') } else { 'none' }
        $quotedNote = (ConvertTo-JsonStringLiteral -Text $Disposition) -replace '\s+', ' '
        $expectedFacts = "Title: $quotedTitle Note: $quotedNote" +
            " Ticket: revision $Revision, stage ${Stage}; linked tasks: $tasksText " +
            'This records your decision on the ticket only: it changes no task and merges nothing. ' +
            'A ticket cannot be closed while any of its linked tasks is still open.'
    }
    $descriptionIndex = $normalized.IndexOf($description, [System.StringComparison]::Ordinal)
    if ($descriptionIndex -lt 0) { return $false }
    $afterDescription = $normalized.Substring($descriptionIndex + $description.Length).TrimStart()
    if (-not $afterDescription.StartsWith($expectedFacts, [System.StringComparison]::Ordinal)) { return $false }
    return (Test-ConfirmChoicesEndPanel -Panel $panel)
}

function Confirm-PrimaryTicketModal {
    <#
        Presses Enter once on the primary's ticket move or decision dialog, and only
        while the ticket's persisted revision, the pending orch_ticket_move or
        orch_ticket_review call in the primary transcript and the exact visible text
        all match. The move caller also supplies the stored ticket body, so neither
        title nor planner context can be changed. The extension marks the primary
        blocked while its dialog is open (TK1.3c-blocked observes it), but the gate is
        any active turn (working or blocked): the exact call and dialog decide; the
        revision, the call and the dialog are read again before the key is sent.
    #>
    param(
        [Parameter(Mandatory)][ValidateSet('move', 'review')][string]$Kind,
        [Parameter(Mandatory)][string]$TicketId,
        [Parameter(Mandatory)][string]$TicketTitle,
        [string]$TicketBody = '',
        [string]$Outcome = '',
        [string]$Disposition = '',
        [string[]]$LinkedTaskIds = @(),
        [double]$TimeoutSeconds = 120
    )
    $transcript = [string](Get-Field (Get-ReadySession -LaunchPaneId ([string](Get-Field $script:Project 'LaunchPaneID'))) 'omp_session_file')
    $ticketCallName = if ($Kind -eq 'move') { 'orch_ticket_move' } else { 'orch_ticket_review' }
    $ticketCall = if ($Kind -eq 'move') { @{ ticketId = $TicketId } } else { @{ ticketId = $TicketId; outcome = $Outcome; disposition = $Disposition } }
    $wait = Wait-Until -TimeoutSeconds $TimeoutSeconds -IntervalSeconds 2 -What "ticket $Kind confirmation for $TicketId" -Probe {
        $fact = Get-Field (Get-TicketById -Id $TicketId) 'fact'
        $revision = [long](Get-Field $fact 'Revision' 0)
        if ($revision -le 0) { return $null }
        $live = Get-LiveAgent -Target $script:PrimaryPane
        if (-not $live.Found -or [string](Get-Field $live.Agent 'agent_status') -notin @('working', 'blocked')) { return $null }
        if (-not (Test-OmpPendingToolCall -Path $transcript -Name $ticketCallName -Arguments $ticketCall)) { return $null }
        $screen = Get-PaneScreen -PaneId $script:PrimaryPane -Source 'visible' -Lines 100
        if (Test-PrimaryTicketConfirmScreen -Screen $screen -Kind $Kind -TicketId $TicketId -TicketTitle $TicketTitle `
                -Revision $revision -TicketBody $TicketBody -Outcome $Outcome -Disposition $Disposition -LinkedTaskIds $LinkedTaskIds) {
            return [pscustomobject]@{ Revision = $revision }
        }
        return $null
    }
    if (-not $wait.Ok) { return $false }
    $revision = [long]$wait.Value.Revision
    $latest = Get-Field (Get-TicketById -Id $TicketId) 'fact'
    if ([long](Get-Field $latest 'Revision' 0) -ne $revision) { return $false }
    if (-not (Test-OmpPendingToolCall -Path $transcript -Name $ticketCallName -Arguments $ticketCall)) { return $false }
    $screen = Get-PaneScreen -PaneId $script:PrimaryPane -Source 'visible' -Lines 100
    if (-not (Test-PrimaryTicketConfirmScreen -Screen $screen -Kind $Kind -TicketId $TicketId -TicketTitle $TicketTitle `
            -Revision $revision -TicketBody $TicketBody -Outcome $Outcome -Disposition $Disposition -LinkedTaskIds $LinkedTaskIds)) { return $false }
    $sent = Send-AgentKeys -PaneId $script:PrimaryPane -Keys @('enter')
    [void]($script:PrimarySendKeysUsed += 'enter')
    return ($sent.ExitCode -eq 0)
}

function ConvertTo-JsonStringLiteral {
    <#
        The string the extension's quoteForDialog shows for one text: JSON.stringify
        escapes the quotes, backslash and C0 control characters, and quoteForDialog
        also escapes DEL, the C1 controls, the line and paragraph separators and the
        bidirectional marks, embeddings, overrides and isolates. Every other
        character (non-ASCII included) is verbatim. PowerShell's ConvertTo-Json
        escapes more than that, so it cannot be used to predict the dialog text.
    #>
    param([AllowEmptyString()][string]$Text)
    $builder = New-Object System.Text.StringBuilder
    [void]$builder.Append('"')
    foreach ($ch in $Text.ToCharArray()) {
        $code = [int]$ch
        if ($code -eq 0x22) { [void]$builder.Append('\"') }
        elseif ($code -eq 0x5C) { [void]$builder.Append('\\') }
        elseif ($code -eq 0x0A) { [void]$builder.Append('\n') }
        elseif ($code -eq 0x0D) { [void]$builder.Append('\r') }
        elseif ($code -eq 0x09) { [void]$builder.Append('\t') }
        elseif ($code -eq 0x08) { [void]$builder.Append('\b') }
        elseif ($code -eq 0x0C) { [void]$builder.Append('\f') }
        elseif ($code -lt 0x20 -or ($code -ge 0x7F -and $code -le 0x9F) -or $code -eq 0x061C -or
            $code -eq 0x200E -or $code -eq 0x200F -or $code -eq 0x2028 -or $code -eq 0x2029 -or
            ($code -ge 0x202A -and $code -le 0x202E) -or ($code -ge 0x2066 -and $code -le 0x2069)) {
            [void]$builder.Append(('\u{0:x4}' -f $code))
        }
        else { [void]$builder.Append($ch) }
    }
    [void]$builder.Append('"')
    return $builder.ToString()
}

function ConvertTo-JsonArrayLiteral {
    <#
        The text quoteForDialog shows for a list of strings: JSON.stringify's array
        form, each item quoted as ConvertTo-JsonStringLiteral quotes it, joined by
        commas with no spaces. An empty list is [].
    #>
    param([string[]]$Items = @())
    if ($null -eq $Items) { return '[]' }
    $quoted = foreach ($item in $Items) { ConvertTo-JsonStringLiteral -Text ([string]$item) }
    return '[' + (@($quoted) -join ',') + ']'
}

function Test-PrimaryDispatchConfirmScreen {
    <#
        True only when the primary's ACTIVE confirmation is the dialog of
        dispatchConfirmRequest (omp/herdr-orchestrator.ts) for exactly this
        orch_dispatch request: its title, brief, checks, dependencies, reserved paths
        and base revision, each as quoteForDialog shows it, in the last "Dispatch
        work without a ticket" panel, followed directly by its Yes/No choices, which
        end the panel. A request without dependencies or reserved paths shows [] for
        them, as the tool's schema defaults do. Only whitespace is folded, so a
        changed request, an older matching dialog, extra text before the choices or
        a newer panel below it all refuse.
    #>
    param([string]$Screen, $Request)
    if ([string]::IsNullOrWhiteSpace($Screen) -or $null -eq $Request) { return $false }
    $title = [string](Get-Field $Request 'title')
    $brief = [string](Get-Field $Request 'brief')
    $baseRevision = [string](Get-Field $Request 'baseRevision')
    if ([string]::IsNullOrWhiteSpace($title) -or [string]::IsNullOrWhiteSpace($brief) -or [string]::IsNullOrWhiteSpace($baseRevision)) { return $false }
    $start = $Screen.LastIndexOf('Dispatch work without a ticket', [System.StringComparison]::Ordinal)
    if ($start -lt 0) { return $false }
    $panel = $Screen.Substring($start)
    $expected = ((@(
        'Dispatch work without a ticket',
        'Confirm starting or queueing one worker for this request. It names no ticket, so no accepted plan covers it.',
        ('Title: ' + (ConvertTo-JsonStringLiteral -Text $title)),
        ('Brief: ' + (ConvertTo-JsonStringLiteral -Text $brief)),
        ('Checks: ' + (ConvertTo-JsonArrayLiteral -Items @(Get-Field $Request 'checks' @()))),
        ('Dependencies: ' + (ConvertTo-JsonArrayLiteral -Items @(Get-Field $Request 'dependencies' @()))),
        ('Reserved paths: ' + (ConvertTo-JsonArrayLiteral -Items @(Get-Field $Request 'reservedPaths' @()))),
        ('Base revision: ' + (ConvertTo-JsonStringLiteral -Text $baseRevision)),
        'The worker gets its own branch and checkout, and nothing merges without your review.'
    ) -join ' ') -replace '\s+', ' ').Trim()
    $normalized = ConvertTo-ConfirmPanelText -Panel $panel
    if (-not $normalized.StartsWith($expected, [System.StringComparison]::Ordinal)) { return $false }
    # Only the choices may follow the request; the selector's marker before "Yes"
    # is not a letter.
    $rest = $normalized.Substring($expected.Length) -replace '^[^\p{L}]*', ''
    if (-not $rest.StartsWith('Yes', [System.StringComparison]::Ordinal)) { return $false }
    return (Test-ConfirmChoicesEndPanel -Panel $panel)
}

function Test-TaskMatchesDispatchRequest {
    <#
        True when a stored task holds exactly the work of one orch_dispatch request
        as the coordinator records it: title, brief, each check and each dependency
        trimmed, reserved paths trimmed, slash-separated and case-folded (Windows),
        the same base revision, and no ticket.
    #>
    param($Fact, $Request)
    if ($null -eq $Fact -or $null -eq $Request) { return $false }
    $requestChecks = @(@(Get-Field $Request 'checks' @()) | ForEach-Object { ([string]$_).Trim() })
    $requestDependencies = @(@(Get-Field $Request 'dependencies' @()) | ForEach-Object { ([string]$_).Trim() })
    $requestPaths = @(@(Get-Field $Request 'reservedPaths' @()) | ForEach-Object { ([string]$_).Trim().Replace('\', '/').ToLowerInvariant() })
    $storedPaths = @(@(Get-Field $Fact 'ReservedPaths' @()) | ForEach-Object { ([string]$_).ToLowerInvariant() })
    return ([string](Get-Field $Fact 'Title') -ceq ([string](Get-Field $Request 'title')).Trim() -and
        [string](Get-Field $Fact 'Brief') -ceq ([string](Get-Field $Request 'brief')).Trim() -and
        (ConvertTo-JsonArrayLiteral -Items @(Get-Field $Fact 'Checks' @())) -ceq (ConvertTo-JsonArrayLiteral -Items $requestChecks) -and
        (ConvertTo-JsonArrayLiteral -Items @(Get-Field $Fact 'Dependencies' @())) -ceq (ConvertTo-JsonArrayLiteral -Items $requestDependencies) -and
        (ConvertTo-JsonArrayLiteral -Items $storedPaths) -ceq (ConvertTo-JsonArrayLiteral -Items $requestPaths) -and
        [string](Get-Field $Fact 'BaseRevision') -ceq [string](Get-Field $Request 'baseRevision') -and
        [string]::IsNullOrEmpty([string](Get-Field $Fact 'TicketID')))
}

function Test-DispatchRequestMatchesExpected {
    param($Request, $Expected)
    if ($null -eq $Request -or $null -eq $Expected) { return $false }
    foreach ($field in @('title', 'brief', 'baseRevision')) {
        $actual = Get-Field $Request $field $null
        $wanted = Get-Field $Expected $field $null
        if ($null -eq $actual -or $null -eq $wanted -or [string]$actual -cne [string]$wanted) { return $false }
    }
    foreach ($field in @('checks', 'dependencies', 'reservedPaths')) {
        $actual = @(Get-Field $Request $field @())
        $wanted = @(Get-Field $Expected $field @())
        if ($actual.Count -ne $wanted.Count) { return $false }
        for ($index = 0; $index -lt $wanted.Count; $index++) {
            if ([string]$actual[$index] -cne [string]$wanted[$index]) { return $false }
        }
    }
    return ($null -eq (Get-Field $Request 'ticketId' $null))
}

function Find-PrimaryDispatchModal {
    <#
        The pending ticketless orch_dispatch request whose exact dialog is the
        primary's active confirmation, or $null. The primary's turn must still be
        running (working, or blocked: the extension marks the primary blocked while
        its dialog is open), so an idle or finished primary is refused.
        The candidates are the latest pending call for each expected title that
        matches every expected argument and names no ticket; a finished or
        superseded call never qualifies. The transcript is read before the screen.
    #>
    param([string]$LaunchPaneId, [object[]]$ExpectedRequests = @())
    $live = Get-LiveAgent -Target $script:PrimaryPane
    if (-not $live.Found -or [string](Get-Field $live.Agent 'agent_status') -notin @('working', 'blocked')) { return $null }
    $transcript = [string](Get-Field (Get-ReadySession -LaunchPaneId $LaunchPaneId) 'omp_session_file')
    $ticketless = @(Get-OmpPendingToolCalls -Path $transcript -Name 'orch_dispatch' | Where-Object { $null -eq (Get-Field $_ 'ticketId' $null) })
    $candidates = @(foreach ($expected in @($ExpectedRequests)) {
        $title = [string](Get-Field $expected 'title' '')
        if ([string]::IsNullOrWhiteSpace($title)) { continue }
        $sameTitle = @($ticketless | Where-Object { [string](Get-Field $_ 'title') -ceq $title })
        if ($sameTitle.Count -gt 0) {
            $candidate = $sameTitle[$sameTitle.Count - 1]
            if (Test-DispatchRequestMatchesExpected -Request $candidate -Expected $expected) { $candidate }
        }
    })
    if ($candidates.Count -eq 0) { return $null }
    $screen = Get-PaneScreen -PaneId $script:PrimaryPane -Source 'visible' -Lines 100
    $shown = @($candidates | Where-Object { Test-PrimaryDispatchConfirmScreen -Screen $screen -Request $_ })
    if ($shown.Count -ne 1) { return $null }
    return $shown[0]
}

function Confirm-PrimaryDispatchModals {
    <#
        Presses Enter only on each expected ticketless dispatch request, in
        whatever order its exact confirmation appears. The pending transcript call
        must match the prompt's fixed title, brief, checks, dependencies, paths and
        base revision, and its visible dialog must match that call. Before each key
        the transcript and screen are read again; any changed request receives
        nothing. Requests contains only those confirmed, in confirmation order.
        One result object keeps Requests flat whether a caller assigns, wraps or pipes it.
    #>
    param(
        [Parameter(Mandatory)][object[]]$ExpectedRequests,
        [double]$TimeoutSeconds = 180
    )
    $expected = @($ExpectedRequests)
    if ($expected.Count -eq 0) { return [pscustomobject]@{ Confirmed = $false; Requests = @() } }
    $unconfirmed = New-Object System.Collections.ArrayList
    foreach ($request in $expected) {
        $title = [string](Get-Field $request 'title' '')
        if ([string]::IsNullOrWhiteSpace($title) -or $unconfirmed -ccontains $title) {
            return [pscustomobject]@{ Confirmed = $false; Requests = @() }
        }
        [void]$unconfirmed.Add($title)
    }
    $launchPane = [string](Get-Field $script:Project 'LaunchPaneID')
    $confirmed = New-Object System.Collections.ArrayList
    $complete = $true
    while ($unconfirmed.Count -gt 0) {
        $remaining = @($expected | Where-Object { $unconfirmed -ccontains [string](Get-Field $_ 'title' '') })
        $round = Wait-Until -TimeoutSeconds $TimeoutSeconds -IntervalSeconds 2 -What ('ticketless dispatch confirmation for ' + ($unconfirmed -join ' | ')) -Probe {
            Find-PrimaryDispatchModal -LaunchPaneId $launchPane -ExpectedRequests $remaining
        }
        if (-not $round.Ok -or $null -eq $round.Value) { $complete = $false; break }
        $found = $round.Value
        $request = @($expected | Where-Object { [string](Get-Field $_ 'title' '') -ceq [string](Get-Field $found 'title') })
        if ($request.Count -ne 1) { $complete = $false; break }
        $current = Find-PrimaryDispatchModal -LaunchPaneId $launchPane -ExpectedRequests $request
        if ($null -eq $current -or (Get-Json $current) -cne (Get-Json $found)) { $complete = $false; break }
        $sent = Send-AgentKeys -PaneId $script:PrimaryPane -Keys @('enter')
        [void]($script:PrimarySendKeysUsed += 'enter')
        if ($sent.ExitCode -ne 0) { $complete = $false; break }
        [void]$confirmed.Add($current)
        $unconfirmed.Remove([string](Get-Field $current 'title'))
    }
    return [pscustomobject]@{ Confirmed = $complete; Requests = $confirmed.ToArray() }
}

function Test-PrimaryAnswerConfirmScreen {
    <#
        True only when the primary's ACTIVE confirmation is the answer dialog of
        sendAnswerWithConsent (omp/herdr-orchestrator.ts) for exactly this task and
        this text, quoted as JSON.stringify quotes it, with its Yes/No choices as
        the last panel on screen. A changed wording, another task, another text, an
        older matching modal or a newer panel below it all refuse.
    #>
    param([string]$Screen, [string]$TaskId, [string]$Text)
    if ([string]::IsNullOrWhiteSpace($Screen) -or [string]::IsNullOrWhiteSpace($TaskId) -or
        [string]::IsNullOrEmpty($Text)) { return $false }
    $start = $Screen.LastIndexOf('Send answer to worker', [System.StringComparison]::Ordinal)
    if ($start -lt 0) { return $false }
    $panel = $Screen.Substring($start)
    $normalized = ConvertTo-ConfirmPanelText -Panel $panel
    $expected = (("Send answer to worker`nConfirm sending this answer to the worker of task ${TaskId}:`n" +
        'Answer: ' + (ConvertTo-JsonStringLiteral -Text $Text) + "`n" +
        "It is typed into that worker's session as your reply. Decline if this is not your answer.") -replace '\s+', ' ').Trim()
    if (-not $normalized.StartsWith($expected, [System.StringComparison]::Ordinal)) { return $false }
    return (Test-ConfirmChoicesEndPanel -Panel $panel)
}

function Confirm-PrimaryAnswerModal {
    <#
        Presses Enter on the primary's answer confirmation, and only on that exact
        dialog: the primary transcript must hold the orch_answer call for this task
        and text, and the visible dialog must show the same task and text. Both are
        re-read immediately before the key, so a replaced dialog or a different call
        receives nothing. With the worker's transcript, it also records whether the
        text had already reached the worker while the dialog was still open, which
        would mean the answer was sent without the user's confirmation.
    #>
    param(
        [Parameter(Mandatory)][string]$TaskId,
        [Parameter(Mandatory)][string]$Text,
        [Parameter(Mandatory)][string]$TranscriptPath,
        [string]$WorkerTranscriptPath = '',
        [double]$TimeoutSeconds = 180
    )
    $result = [pscustomobject]@{ Observed = $false; Confirmed = $false; DeliveredBeforeConfirm = $false; Screen = '' }
    $wait = Wait-Until -TimeoutSeconds $TimeoutSeconds -IntervalSeconds 2 -What "answer confirmation for task $TaskId" -Probe {
        # The status is not a gate: the extension marks the primary blocked while
        # its dialog is open, but before that mark existed Herdr reported it
        # "working" (run da682eab). Like Confirm-PrimaryModal, the exact visible
        # dialog and the transcript's tool call are the gate.
        $screen = Get-PaneScreen -PaneId $script:PrimaryPane -Source 'visible' -Lines 100
        if (-not (Test-PrimaryAnswerConfirmScreen -Screen $screen -TaskId $TaskId -Text $Text)) { return $null }
        if (-not (Test-OmpTranscriptContainsAnswerToolCall -Path $TranscriptPath -TaskId $TaskId -Text $Text)) { return $null }
        return $true
    }
    if (-not $wait.Ok) {
        $result.Screen = Get-PaneScreen -PaneId $script:PrimaryPane -Source 'visible' -Lines 100
        return $result
    }
    $result.Observed = $true
    if (-not [string]::IsNullOrWhiteSpace($WorkerTranscriptPath)) {
        $result.DeliveredBeforeConfirm = [bool](Test-OmpTranscriptContainsExactUserMessage -Path $WorkerTranscriptPath -Text $Text)
    }
    # A text already in the worker's session was sent without the user: confirming
    # now would send it a second time, so nothing is pressed.
    if ($result.DeliveredBeforeConfirm) { return $result }
    if (-not (Test-OmpTranscriptContainsAnswerToolCall -Path $TranscriptPath -TaskId $TaskId -Text $Text)) { return $result }
    $screen = Get-PaneScreen -PaneId $script:PrimaryPane -Source 'visible' -Lines 100
    $result.Screen = $screen
    if (-not (Test-PrimaryAnswerConfirmScreen -Screen $screen -TaskId $TaskId -Text $Text)) { return $result }
    $sent = Send-AgentKeys -PaneId $script:PrimaryPane -Keys @('enter')
    [void]($script:PrimarySendKeysUsed += 'enter')
    $result.Confirmed = ($sent.ExitCode -eq 0)
    return $result
}

function Resolve-PrimaryDispositionAsk {
    param(
        [Parameter(Mandatory)][string]$Marker,
        [Parameter(Mandatory)][string]$AskId,
        [Parameter(Mandatory)][string]$TaskId,
        [Parameter(Mandatory)][string]$AssertionId
    )
    $wait = Wait-Until -TimeoutSeconds 90 -IntervalSeconds 2 -What "primary disposition ask $AskId for $Marker" -Probe {
        $live = Get-LiveAgent -Target $script:PrimaryPane
        if (-not $live.Found) { return $null }
        $status = [string](Get-Field $live.Agent 'agent_status')
        if ($status -in @('idle', 'done')) {
            $row = Get-TaskById -Id $TaskId
            $fact = Get-Field $row 'fact'
            if (-not [string]::IsNullOrWhiteSpace([string](Get-Field $fact 'Disposition'))) {
                return [pscustomobject]@{ Kind = 'settled'; Screen = '' }
            }
            return $null
        }
        if ($status -ne 'blocked') { return $null }
        $screen = Get-PaneScreen -PaneId $script:PrimaryPane -Source 'visible' -Lines 100
        if (Test-PrimaryDispositionAskScreen -Screen $screen -AskId $AskId -TaskId $TaskId) {
            return [pscustomobject]@{ Kind = 'ask'; Screen = $screen }
        }
        return [pscustomobject]@{ Kind = 'other'; Screen = $screen }
    }
    if (-not $wait.Ok) {
        return [pscustomobject]@{
            Observed = $false
            Resolved = $false
            Screen   = Get-PaneScreen -PaneId $script:PrimaryPane -Source 'visible' -Lines 100
        }
    }
    if ($wait.Value.Kind -eq 'settled') {
        return [pscustomobject]@{ Observed = $false; Resolved = $true; Screen = '' }
    }
    if ($wait.Value.Kind -ne 'ask') {
        return [pscustomobject]@{ Observed = $false; Resolved = $false; Screen = $wait.Value.Screen }
    }

    $taskRow = Get-TaskById -Id $TaskId
    $fact = Get-Field $taskRow 'fact'
    if (-not $taskRow -or $null -eq (Get-Field $fact 'Report' $null) -or
        -not [string]::IsNullOrWhiteSpace([string](Get-Field $fact 'Disposition'))) {
        return [pscustomobject]@{ Observed = $true; Resolved = $false; Screen = $wait.Value.Screen }
    }

    # Both the task state and active Ask can change while the acceptance is
    # deciding. Re-read the report state, then the exact Ask, immediately before
    # Enter; no stale task or replacement dialog receives input.
    $screen = Get-PaneScreen -PaneId $script:PrimaryPane -Source 'visible' -Lines 100
    if (-not (Test-PrimaryDispositionAskScreen -Screen $screen -AskId $AskId -TaskId $TaskId)) {
        return [pscustomobject]@{ Observed = $true; Resolved = $false; Screen = $screen }
    }
    $latestTaskRow = Get-TaskById -Id $TaskId
    $latestFact = Get-Field $latestTaskRow 'fact'
    if (-not $latestTaskRow -or $null -eq (Get-Field $latestFact 'Report' $null) -or
        -not [string]::IsNullOrWhiteSpace([string](Get-Field $latestFact 'Disposition'))) {
        return [pscustomobject]@{ Observed = $true; Resolved = $false; Screen = $screen }
    }
    $screen = Get-PaneScreen -PaneId $script:PrimaryPane -Source 'visible' -Lines 100
    if (-not (Test-PrimaryDispositionAskScreen -Screen $screen -AskId $AskId -TaskId $TaskId)) {
        return [pscustomobject]@{ Observed = $true; Resolved = $false; Screen = $screen }
    }
    $sent = Send-AgentKeys -PaneId $script:PrimaryPane -Keys @('enter')
    $closed = Wait-Until -TimeoutSeconds 120 -IntervalSeconds 3 -What "primary disposition ask $AskId closed" -Probe {
        $live = Get-LiveAgent -Target $script:PrimaryPane
        if (-not $live.Found) { return $null }
        if ([string](Get-Field $live.Agent 'agent_status') -ne 'blocked') { return $true }
        $current = Get-PaneScreen -PaneId $script:PrimaryPane -Source 'visible' -Lines 100
        if (-not (Test-PrimaryDispositionAskScreen -Screen $current -AskId $AskId -TaskId $TaskId)) { return $true }
        return $null
    }
    $reviewConfirmed = $false
    if ($sent.ExitCode -eq 0 -and $closed.Ok) {
        $reviewConfirmed = Confirm-PrimaryReviewModal -PaneId $script:PrimaryPane `
            -LaunchPaneId ([string](Get-Field $script:Project 'LaunchPaneID')) -TaskId $TaskId `
            -Disposition 'Accepted the reported result.'
    }
    if (-not $reviewConfirmed) {
        return [pscustomobject]@{ Observed = $true; Resolved = $false; Screen = (Get-PaneScreen -PaneId $script:PrimaryPane -Source 'visible' -Lines 100) }
    }
    $settled = Wait-Until -TimeoutSeconds 180 -IntervalSeconds 3 -What "primary recorded $Marker disposition" -Probe {
        $row = Get-TaskById -Id $TaskId
        $currentFact = Get-Field $row 'fact'
        if (-not [string]::IsNullOrWhiteSpace([string](Get-Field $currentFact 'Disposition'))) { return 'disposed' }
        $live = Get-LiveAgent -Target $script:PrimaryPane
        if ($live.Found -and [string](Get-Field $live.Agent 'agent_status') -in @('idle', 'done')) { return 'idle' }
        return $null
    }
    $resolved = ($sent.ExitCode -eq 0 -and $closed.Ok -and $reviewConfirmed -and $settled.Ok -and $settled.Value -eq 'disposed')
    [void](Assert-True -Id $AssertionId -What "the acceptance user selected and confirmed Accept report for $Marker" -Condition $resolved `
        -Command $sent.CommandLine -Evidence ("Ask=$AskId; selected the first option; confirmed=$reviewConfirmed closed=$($closed.Ok) settled=$($settled.Value) screen=" + (Protect-Text $screen 600)))
    [void]($script:PrimarySendKeysUsed += 'enter')
    return [pscustomobject]@{ Observed = $true; Resolved = $resolved; Screen = $screen }
}

function Resolve-PrimaryWorkerQuestionAsk {
    param(
        [Parameter(Mandatory)][string]$TaskId,
        [Parameter(Mandatory)][string]$QuestionMarker,
        [Parameter(Mandatory)][string]$QuestionText,
        [Parameter(Mandatory)][string]$AnswerToken,
        [Parameter(Mandatory)][string]$AssertionId
    )
    $wait = Wait-Until -TimeoutSeconds 120 -IntervalSeconds 2 -What "primary worker-question Ask for $QuestionMarker" -Probe {
        $live = Get-LiveAgent -Target $script:PrimaryPane
        if (-not $live.Found) { return $null }
        $status = [string](Get-Field $live.Agent 'agent_status')
        if ($status -in @('idle', 'done')) { return [pscustomobject]@{ Kind = 'settled'; Screen = '' } }
        if ($status -ne 'blocked') { return $null }
        $screen = Get-PaneScreen -PaneId $script:PrimaryPane -Source 'visible' -Lines 100
        $askId = Get-PrimaryAskId -Screen $screen
        if (-not $askId) { return [pscustomobject]@{ Kind = 'other'; Screen = $screen } }
        if (Test-PrimaryWorkerQuestionAskForTask -Screen $screen -QuestionText $QuestionText -AnswerToken $AnswerToken) {
            return [pscustomobject]@{ Kind = 'ask'; AskId = $askId; Screen = $screen }
        }
        return [pscustomobject]@{ Kind = 'other'; Screen = $screen }
    }
    if (-not $wait.Ok) {
        return [pscustomobject]@{
            Observed = $false
            Resolved = $false
            Screen = Get-PaneScreen -PaneId $script:PrimaryPane -Source 'visible' -Lines 100
        }
    }
    if ($wait.Value.Kind -eq 'settled') {
        return [pscustomobject]@{ Observed = $false; Resolved = $true; Screen = '' }
    }
    if ($wait.Value.Kind -ne 'ask') {
        return [pscustomobject]@{ Observed = $false; Resolved = $false; Screen = $wait.Value.Screen }
    }

    $taskRow = Get-TaskById -Id $TaskId
    $fact = Get-Field $taskRow 'fact'
    if (-not $taskRow -or (Get-TaskStage -Fact $fact) -ne 'needs_input' -or
        [string](Get-Field $fact 'Attention') -notmatch [regex]::Escape($QuestionMarker) -or
        $null -ne (Get-Field $fact 'Report' $null) -or
        -not [string]::IsNullOrWhiteSpace([string](Get-Field $fact 'Disposition'))) {
        return [pscustomobject]@{ Observed = $true; Resolved = $false; Screen = $wait.Value.Screen }
    }

    $screen = Get-PaneScreen -PaneId $script:PrimaryPane -Source 'visible' -Lines 100
    $askId = Get-PrimaryAskId -Screen $screen
    if ($askId -ine $wait.Value.AskId -or
        -not (Test-PrimaryWorkerQuestionAskForTask -Screen $screen -QuestionText $QuestionText -AnswerToken $AnswerToken)) {
        return [pscustomobject]@{ Observed = $true; Resolved = $false; Screen = $screen }
    }
    $latestTaskRow = Get-TaskById -Id $TaskId
    $latestFact = Get-Field $latestTaskRow 'fact'
    if (-not $latestTaskRow -or (Get-TaskStage -Fact $latestFact) -ne 'needs_input' -or
        [string](Get-Field $latestFact 'Attention') -notmatch [regex]::Escape($QuestionMarker) -or
        $null -ne (Get-Field $latestFact 'Report' $null) -or
        -not [string]::IsNullOrWhiteSpace([string](Get-Field $latestFact 'Disposition'))) {
        return [pscustomobject]@{ Observed = $true; Resolved = $false; Screen = $screen }
    }
    $screen = Get-PaneScreen -PaneId $script:PrimaryPane -Source 'visible' -Lines 100
    $askId = Get-PrimaryAskId -Screen $screen
    if ($askId -ine $wait.Value.AskId -or
        -not (Test-PrimaryWorkerQuestionAskForTask -Screen $screen -QuestionText $QuestionText -AnswerToken $AnswerToken)) {
        return [pscustomobject]@{ Observed = $true; Resolved = $false; Screen = $screen }
    }

    $sent = Send-AgentKeys -PaneId $script:PrimaryPane -Keys @('enter')
    $closed = Wait-Until -TimeoutSeconds 120 -IntervalSeconds 3 -What "primary worker-question Ask $($wait.Value.AskId) closed" -Probe {
        $live = Get-LiveAgent -Target $script:PrimaryPane
        if (-not $live.Found) { return $null }
        if ([string](Get-Field $live.Agent 'agent_status') -ne 'blocked') { return $true }
        $current = Get-PaneScreen -PaneId $script:PrimaryPane -Source 'visible' -Lines 100
        $currentAskId = Get-PrimaryAskId -Screen $current
        if ($currentAskId -ine $wait.Value.AskId -or
            -not (Test-PrimaryWorkerQuestionAskForTask -Screen $current -QuestionText $QuestionText -AnswerToken $AnswerToken)) { return $true }
        return $null
    }
    $resolved = ($sent.ExitCode -eq 0 -and $closed.Ok)
    [void](Assert-True -Id $AssertionId -What "the acceptance user selected the authorized worker-question answer for $QuestionMarker" `
        -Condition $resolved -Command $sent.CommandLine `
        -Evidence ("Ask=$($wait.Value.AskId); token=$AnswerToken; closed=$($closed.Ok); screen=" + (Protect-Text $screen 600)))
    [void]($script:PrimarySendKeysUsed += 'enter')
    return [pscustomobject]@{ Observed = $true; Resolved = $resolved; Screen = $screen }
}

function Deny-PrimaryConsentDialog {
    <#
        Declines the extension consent dialog that is the primary's active panel,
        for a dialog no step of this run asked for. The acceptance user confirms
        only the exact dialog a step requested (the Confirm-* drivers); any other is
        an action the user never asked for. Escape closes OMP's selector as "No", so
        the extension refuses the tool and records nothing. Enter would select the
        highlighted Yes, so it is never sent here. Escape on OMP's editor would
        interrupt a running turn instead, so the pane is read again right before
        the key and nothing is sent once the dialog is gone. Returns {Declined,
        Title, Reason, Screen}; Title is empty when no consent dialog was open at
        that read.
    #>
    param([string]$AssertionPrefix = 'prompt')
    $screen = Get-PaneScreen -PaneId $script:PrimaryPane -Source 'visible' -Lines 100
    $title = Get-PrimaryConsentDialogTitle -Screen $screen
    if (-not $title) {
        return [pscustomobject]@{ Declined = $false; Title = ''; Reason = 'no extension consent dialog is open on the primary'; Screen = $screen }
    }
    $sent = Send-AgentKeys -PaneId $script:PrimaryPane -Keys @('esc')
    [void]($script:PrimarySendKeysUsed += 'esc')
    $closed = Wait-Until -TimeoutSeconds 30 -IntervalSeconds 1 -What "the declined '$title' dialog closed" -Probe {
        $now = Get-PaneScreen -PaneId $script:PrimaryPane -Source 'visible' -Lines 100
        if ($null -eq (Get-PrimaryConsentDialogTitle -Screen $now)) { return $true }
        return $null
    }
    [void](Add-Assertion -Id "$AssertionPrefix.declined" -Status 'INFO' -Command $sent.CommandLine `
        -What "the acceptance user declined an unrequested '$title' dialog on the primary (a declined dialog records nothing)" `
        -Evidence ("sent=$($sent.ExitCode) closed=$($closed.Ok) screen=" + (Protect-Text $screen 600)))
    $reason = if ($sent.ExitCode -ne 0) {
        "Escape could not be sent to the unrequested '$title' dialog (exit $($sent.ExitCode))"
    } elseif (-not $closed.Ok) {
        "the unrequested '$title' dialog stayed open after Escape"
    } else { '' }
    return [pscustomobject]@{ Declined = ($reason -eq ''); Title = $title; Reason = $reason; Screen = $screen }
}

function Get-PrimaryUnsettledReason {
    # Why the primary did not settle in time, for a refusal that typed nothing. A
    # consent dialog marks the pane blocked while it is open, so one that is open
    # while Herdr reports anything else is named: that is the extension's defect.
    # A pane that stays blocked with neither a disposition Ask nor a consent dialog
    # on screen is blocked on something this run does not answer.
    param([string]$Screen, [double]$TimeoutSeconds)
    $live = Get-LiveAgent -Target $script:PrimaryPane
    $status = if ($live.Found) { [string](Get-Field $live.Agent 'agent_status') } else { 'absent' }
    if ([string]::IsNullOrWhiteSpace($status)) { $status = 'without a status' }
    $title = Get-PrimaryConsentDialogTitle -Screen $Screen
    if ($status -ceq 'blocked' -and -not $title -and -not (Get-PrimaryAskId -Screen $Screen)) {
        return "the primary stayed blocked for ${TimeoutSeconds}s on a screen that is neither a disposition Ask this run resolves nor an extension consent dialog"
    }
    $reason = "the primary did not settle (idle, done or blocked) within ${TimeoutSeconds}s; Herdr reports it $status"
    if ($title) { $reason += "; the extension consent dialog '$title' is open, which should have marked it blocked" }
    return $reason
}

function Resolve-PendingPrimaryDispositionAsks {
    <#
        Settles the primary before the harness types a prompt into it. Returns
        {Resolved, Observed, Screen, Reason}: Resolved only once Herdr reports the
        primary idle or done, never mid-turn. While it is blocked, a disposition Ask
        this run recognizes exactly, for a task whose report waits for a decision,
        is resolved (Observed), and an extension consent dialog no step asked for is
        declined (Deny-PrimaryConsentDialog). Anything else stops with the reason
        and nothing typed.
    #>
    param([string]$AssertionPrefix = 'V0', [double]$TimeoutSeconds = 30)
    $observed = $false
    for ($attempt = 0; $attempt -lt 4; $attempt++) {
        $wait = Wait-Until -TimeoutSeconds $TimeoutSeconds -IntervalSeconds 2 -What 'primary disposition Ask or idle' -Probe {
            $live = Get-LiveAgent -Target $script:PrimaryPane
            if (-not $live.Found) { return $null }
            $status = [string](Get-Field $live.Agent 'agent_status')
            if ($status -in @('idle', 'done')) { return [pscustomobject]@{ Kind = 'settled'; Screen = '' } }
            if ($status -ne 'blocked') { return $null }
            $screen = Get-PaneScreen -PaneId $script:PrimaryPane -Source 'visible' -Lines 100
            if (Get-PrimaryConsentDialogTitle -Screen $screen) { return [pscustomobject]@{ Kind = 'consent'; Screen = $screen } }
            $askId = Get-PrimaryAskId -Screen $screen
            if ($askId) { return [pscustomobject]@{ Kind = 'ask'; AskId = $askId; Screen = $screen } }
            # Blocked with neither on screen: Herdr can still report a dialog that just
            # closed (the cleared mark reaches it a moment later), so keep waiting; at
            # the timeout the reason names the screen.
            return $null
        }
        if (-not $wait.Ok) {
            $screen = Get-PaneScreen -PaneId $script:PrimaryPane -Source 'visible' -Lines 100
            return [pscustomobject]@{ Resolved = $false; Observed = $observed; Screen = $screen
                Reason = (Get-PrimaryUnsettledReason -Screen $screen -TimeoutSeconds $TimeoutSeconds) }
        }
        if ($wait.Value.Kind -eq 'settled') {
            return [pscustomobject]@{ Resolved = $true; Observed = $observed; Screen = ''; Reason = '' }
        }
        if ($wait.Value.Kind -eq 'consent') {
            $declined = Deny-PrimaryConsentDialog -AssertionPrefix $AssertionPrefix
            # Declined, or already gone at the read before the key: look again.
            if ($declined.Declined -or -not $declined.Title) { continue }
            return [pscustomobject]@{ Resolved = $false; Observed = $observed; Screen = $declined.Screen; Reason = $declined.Reason }
        }

        $taskAFact = $script:TaskA
        if (-not $taskAFact) {
            $taskARow = Get-TaskByTitle -Pattern 'ACCEPT A'
            if ($taskARow) { $taskAFact = Get-Field $taskARow 'fact' }
        }
        $taskBFact = $script:TaskB
        if (-not $taskBFact) {
            $taskBRow = Get-TaskByTitle -Pattern 'ACCEPT B'
            if ($taskBRow) { $taskBFact = Get-Field $taskBRow 'fact' }
        }
        $targets = @(
            [pscustomobject]@{ AskId = 'task_a_review'; Marker = 'ACCEPT-A'; TaskId = [string](Get-Field $taskAFact 'ID'); AssertionId = "$AssertionPrefix.askA" },
            [pscustomobject]@{ AskId = 'worker_a_disposition'; Marker = 'ACCEPT-A'; TaskId = [string](Get-Field $taskAFact 'ID'); AssertionId = "$AssertionPrefix.askA" },
            [pscustomobject]@{ AskId = 'task_b_review'; Marker = 'ACCEPT-B'; TaskId = [string](Get-Field $taskBFact 'ID'); AssertionId = "$AssertionPrefix.askB" },
            [pscustomobject]@{ AskId = 'worker_b_disposition'; Marker = 'ACCEPT-B'; TaskId = [string](Get-Field $taskBFact 'ID'); AssertionId = "$AssertionPrefix.askB" }
        )
        $target = @($targets | Where-Object { $_.AskId -eq $wait.Value.AskId })
        if ($target.Count -ne 1 -or [string]::IsNullOrWhiteSpace($target[0].TaskId)) {
            return [pscustomobject]@{ Resolved = $false; Observed = $observed; Screen = $wait.Value.Screen
                Reason = "the primary is blocked on Ask '$($wait.Value.AskId)', which is not a disposition Ask this run resolves" }
        }
        $task = Get-TaskById -Id $target[0].TaskId
        $fact = Get-Field $task 'fact'
        if (-not $task -or $null -eq (Get-Field $fact 'Report' $null) -or
            -not [string]::IsNullOrWhiteSpace([string](Get-Field $fact 'Disposition'))) {
            return [pscustomobject]@{ Resolved = $false; Observed = $observed; Screen = $wait.Value.Screen
                Reason = "Ask '$($wait.Value.AskId)' names task $($target[0].TaskId), which has no report waiting for a decision" }
        }
        $result = Resolve-PrimaryDispositionAsk -Marker $target[0].Marker -AskId $target[0].AskId `
            -TaskId $target[0].TaskId -AssertionId $target[0].AssertionId
        if (-not $result.Resolved) {
            return [pscustomobject]@{ Resolved = $false; Observed = $observed; Screen = $result.Screen
                Reason = "the disposition Ask '$($wait.Value.AskId)' for task $($target[0].TaskId) was not resolved" }
        }
        $observed = $observed -or $result.Observed
    }
    return [pscustomobject]@{ Resolved = $false; Observed = $observed; Screen = Get-PaneScreen -PaneId $script:PrimaryPane -Source 'visible' -Lines 100
        Reason = 'the primary was still not settled after four rounds of resolving its pending input' }
}

function Invoke-PrimaryReviewForPendingReport {
    <#
        Records the user's decision on a worker report that waits for review before
        the next phase needs the primary. The primary has no Ask tool, so the
        decision is the review dialog itself: the primary is asked to call
        orch_review for exactly that task, and the acceptance user confirms the
        exact dialog (Confirm-PrimaryReviewModal). Returns {Resolved, Observed,
        Screen}: Observed when a review was requested, Resolved when nothing was
        pending or the confirmed disposition is recorded.
    #>
    param([string]$AssertionPrefix = 'review')
    $disposition = 'Accepted the reported result.'
    foreach ($target in @(
        @{ fact = $script:TaskA; marker = 'ACCEPT-A' },
        @{ fact = $script:TaskB; marker = 'ACCEPT-B' }
    )) {
        $taskFact = $target.fact
        if (-not $taskFact) {
            $taskRow = Get-TaskByTitle -Pattern $target.marker
            if ($taskRow) { $taskFact = Get-Field $taskRow 'fact' }
        }
        $taskId = [string](Get-Field $taskFact 'ID')
        if (-not $taskId) { continue }
        $taskRow = Get-TaskById -Id $taskId
        $fact = Get-Field $taskRow 'fact'
        if (-not $taskRow -or $null -eq (Get-Field $fact 'Report' $null) -or
            -not [string]::IsNullOrWhiteSpace([string](Get-Field $fact 'Disposition'))) { continue }

        $prompt = 'ACCEPT-REVIEW. Review this one worker result only: ' + $target.marker + ' taskId "' + $taskId +
            '". Call orch_review exactly once for this exact taskId with outcome "integrated" and disposition "' + $disposition +
            '". The user confirms the dialog. Do not review any other task and do not merge or clean up either checkout. Reply reviewed after the tool result.'
        $submitted = Invoke-PrimaryPromptNoWaitAfterDispositionAsks -PaneId $script:PrimaryPane -Text $prompt -TimeoutSeconds 240 -AssertionPrefix "$AssertionPrefix.$($target.marker)"
        if (-not $submitted.Submitted) {
            [void](Assert-True -Id "$AssertionPrefix.review/$($target.marker)" -What "the primary accepted the review prompt for the pending report of $($target.marker)" -Condition $false `
                -TaskId $taskId -Command $submitted.CommandLine -Evidence ("code=[$($submitted.Code)] screen=" + (Protect-Text $submitted.Screen 600)))
            return [pscustomobject]@{ Resolved = $false; Observed = $false; Screen = $submitted.Screen }
        }
        $confirmed = Confirm-PrimaryReviewModal -PaneId $script:PrimaryPane -LaunchPaneId ([string](Get-Field $script:Project 'LaunchPaneID')) `
            -TaskId $taskId -Disposition $disposition
        $recorded = $false
        if ($confirmed) {
            $settled = Wait-Until -TimeoutSeconds 180 -IntervalSeconds 3 -What "the recorded disposition of $($target.marker)" -Probe {
                $latest = Get-Field (Get-TaskById -Id $taskId) 'fact'
                if ([string](Get-Field $latest 'Disposition') -ceq $disposition) { return $true }
                return $null
            }
            $recorded = [bool]$settled.Ok
        }
        $screen = if ($recorded) { '' } else { Get-PaneScreen -PaneId $script:PrimaryPane -Source 'visible' -Lines 100 }
        [void](Assert-True -Id "$AssertionPrefix.review/$($target.marker)" -What "the acceptance user confirmed the exact review dialog for $($target.marker) and its disposition is recorded" `
            -Condition $recorded -TaskId $taskId -Command $submitted.CommandLine -Evidence ("confirmed=[$confirmed] recorded=[$recorded] screen=" + (Protect-Text $screen 600)))
        return [pscustomobject]@{ Resolved = $recorded; Observed = $true; Screen = $screen }
    }
    return [pscustomobject]@{ Resolved = $true; Observed = $false; Screen = '' }
}

function Invoke-PrimaryPromptAfterDispositionAsks {
    param([Parameter(Mandatory)][string]$PaneId, [Parameter(Mandatory)][string]$Text, [double]$TimeoutSeconds = 300, [string]$AssertionPrefix = 'prompt')
    $pending = Resolve-PendingPrimaryDispositionAsks -AssertionPrefix $AssertionPrefix -TimeoutSeconds 30
    if (-not $pending.Resolved) {
        return [pscustomobject]@{ ExitCode = -1; Code = 'agent_blocked'; Blocked = $true; Submitted = $false; Screen = $pending.Screen
            Stderr = [string](Get-Field $pending 'Reason' 'the primary''s pending input could not be settled'); CommandLine = '' }
    }
    $result = Invoke-PrimaryPrompt -PaneId $PaneId -Text $Text -TimeoutSeconds $TimeoutSeconds
    if ($result.Blocked -and $result.Code -eq 'agent_blocked' -and -not $result.Submitted) {
        $pending = Resolve-PendingPrimaryDispositionAsks -AssertionPrefix $AssertionPrefix -TimeoutSeconds 60
        if ($pending.Resolved) {
            $result = Invoke-PrimaryPrompt -PaneId $PaneId -Text $Text -TimeoutSeconds $TimeoutSeconds
        }
    }
    return $result
}

function Invoke-PrimaryPromptNoWaitAfterDispositionAsks {
    <#
        The no-wait twin of Invoke-PrimaryPromptAfterDispositionAsks, with the same
        single retry: a primary seen blocked before anything was submitted gets its
        pending disposition Asks resolved once and the prompt is tried once more.
        The prompt itself is never submitted while the primary is blocked; between
        the attempts only Resolve-PendingPrimaryDispositionAsks may press Enter, and
        only on a disposition Ask it recognizes exactly.
    #>
    param(
        [Parameter(Mandatory)][string]$PaneId,
        [Parameter(Mandatory)][string]$Text,
        [double]$TimeoutSeconds = 300,
        [string]$AssertionPrefix = 'prompt'
    )
    $result = Invoke-PrimaryPromptNoWaitOnce -PaneId $PaneId -Text $Text -TimeoutSeconds $TimeoutSeconds -AssertionPrefix $AssertionPrefix
    if ($result.Blocked -and $result.Code -eq 'agent_blocked' -and -not $result.Submitted) {
        $pending = Resolve-PendingPrimaryDispositionAsks -AssertionPrefix $AssertionPrefix -TimeoutSeconds 60
        if ($pending.Resolved) {
            $result = Invoke-PrimaryPromptNoWaitOnce -PaneId $PaneId -Text $Text -TimeoutSeconds $TimeoutSeconds -AssertionPrefix $AssertionPrefix
        }
    }
    return $result
}

function Invoke-PrimaryPromptNoWaitOnce {
    param(
        [Parameter(Mandatory)][string]$PaneId,
        [Parameter(Mandatory)][string]$Text,
        [double]$TimeoutSeconds = 300,
        [string]$AssertionPrefix = 'prompt'
    )
    $pending = Resolve-PendingPrimaryDispositionAsks -AssertionPrefix $AssertionPrefix -TimeoutSeconds 30
    if (-not $pending.Resolved) {
        return [pscustomobject]@{
            ExitCode = -1
            Code = 'agent_blocked'
            Blocked = $true
            Submitted = $false
            Screen = $pending.Screen
            Stderr = [string](Get-Field $pending 'Reason' 'the primary''s pending input could not be settled')
            Stdout = ''
            CommandLine = ''
        }
    }
    $idle = Wait-Until -TimeoutSeconds 180 -IntervalSeconds 6 -What 'primary settled before no-wait prompt' -Probe {
        $live = Get-LiveAgent -Target $PaneId
        if (-not $live.Found) { return $null }
        $status = [string](Get-Field $live.Agent 'agent_status')
        if ($status -eq 'blocked') { return 'blocked' }
        if ($status -in @('idle', 'done')) { return $status }
        return $null
    }
    if (-not $idle.Ok -or $idle.Value -eq 'blocked') {
        $code = if ($idle.Value -eq 'blocked') { 'agent_blocked' } else { 'primary_not_settled' }
        return [pscustomobject]@{
            ExitCode = -2
            Code = $code
            Blocked = ($code -eq 'agent_blocked')
            Submitted = $false
            Screen = Get-PaneScreen -PaneId $PaneId -Source 'visible' -Lines 60
            Stderr = ''
            Stdout = ''
            CommandLine = "herdr agent prompt $PaneId <primary prompt>"
        }
    }
    $result = Invoke-AgentPrompt -PaneId $PaneId -Text $Text -TimeoutSeconds $TimeoutSeconds -NoWait
    $code = Get-HerdrErrorCode -Result $result
    return [pscustomobject]@{
        ExitCode = $result.ExitCode
        Code = $code
        Blocked = ($code -eq 'agent_blocked')
        Submitted = ($result.ExitCode -eq 0)
        Screen = ''
        Stderr = $result.Stderr
        Stdout = $result.Stdout
        CommandLine = $result.CommandLine
    }
}

function Confirm-PrimaryModal {
    <#
        Confirm only the exact worker-dialog action from the primary transcript:
        the task id/key call and the visible dialog/Yes-No preview must both match.
    #>
    param(
        [Parameter(Mandatory)][string]$TaskId,
        [Parameter(Mandatory)][string]$DialogMarker,
        [Parameter(Mandatory)][string]$ExpectedQuestion,
        [Parameter(Mandatory)][string[]]$ExpectedKeys,
        [Parameter(Mandatory)][string]$TranscriptPath,
        [double]$TimeoutSeconds = 180
    )
    if (-not (Test-OmpApprovalToolCall -Path $TranscriptPath -TaskId $TaskId -Keys $ExpectedKeys)) {
        return [pscustomobject]@{
            Observed = $false
            ToolCallRecorded = $false
            Screen = Get-PaneScreen -PaneId $script:PrimaryPane -Source 'visible' -Lines 100
            Keys = @()
            Closed = $false
        }
    }
    $wait = Wait-Until -TimeoutSeconds $TimeoutSeconds -IntervalSeconds 3 -What "approval confirmation for task $TaskId" -Probe {
        $screen = Get-PaneScreen -PaneId $script:PrimaryPane -Source 'visible' -Lines 100
        if (Test-PrimaryApprovalConfirmScreen -Screen $screen -DialogMarker $DialogMarker -ExpectedQuestion $ExpectedQuestion -Keys $ExpectedKeys) { return $screen }
        return $null
    }
    if (-not $wait.Ok) {
        return [pscustomobject]@{
            Observed = $false
            ToolCallRecorded = $true
            Screen = Get-PaneScreen -PaneId $script:PrimaryPane -Source 'visible' -Lines 100
            Keys = @()
            Closed = $false
        }
    }
    # Validate the exact transcript call first, then read the live pane as the
    # final authorization check immediately before sending the confirmation key.
    $toolCallRecorded = Test-OmpApprovalToolCall -Path $TranscriptPath -TaskId $TaskId -Keys $ExpectedKeys
    if (-not $toolCallRecorded) {
        return [pscustomobject]@{
            Observed = $false
            ToolCallRecorded = $false
            Screen = Get-PaneScreen -PaneId $script:PrimaryPane -Source 'visible' -Lines 100
            Keys = @()
            Closed = $false
        }
    }
    $keys = @('enter')
    $screen = Get-PaneScreen -PaneId $script:PrimaryPane -Source 'visible' -Lines 100
    if (-not (Test-PrimaryApprovalConfirmScreen -Screen $screen -DialogMarker $DialogMarker -ExpectedQuestion $ExpectedQuestion -Keys $ExpectedKeys)) {
        return [pscustomobject]@{ Observed = $false; ToolCallRecorded = $true; Screen = $screen; Keys = @(); Closed = $false }
    }
    $sent = Send-AgentKeys -PaneId $script:PrimaryPane -Keys $keys
    [void]($script:PrimarySendKeysUsed += ($keys -join ' '))
    return [pscustomobject]@{
        Observed = $true
        ToolCallRecorded = $true
        Screen = $screen
        Keys = $keys
        Sent = ($sent.ExitCode -eq 0)
        SendResult = $sent
    }
}

function Invoke-BlockedDialog {
    Write-Step 'Phase 12: real blocked full-screen dialog with a visible preview'
    $taskB = Get-TaskByTitle -Pattern 'ACCEPT B'
    Assert-True -Id 'K1.1' -What 'task B is identifiable by its exact title' -Condition ($null -ne $taskB) -Evidence (Protect-Text (Get-Json @($script:Tasks | ForEach-Object { Get-Field (Get-Field $_ 'fact') 'Title' })) 300)
    if (-not $taskB) { Block-Item -Id 'K1.2' -What 'blocked full-screen dialog' -Evidence 'task B was not dispatched'; return }
    $script:TaskB = Get-Field $taskB 'fact'
    $idB = [string](Get-Field $script:TaskB 'ID')
    $idA = if ($script:TaskA) { [string](Get-Field $script:TaskA 'ID') } else { '' }
    if (-not $idA) { Block-Item -Id 'K3.6a' -What 'task A is identifiable before resolving its disposition Ask' -Evidence 'task A is unavailable'; return }
    $paneB = [string](Get-Field $script:TaskB 'PublicPaneID')

    # Same as the question above: the dialog is requested explicitly, so the
    # partial-report gate earlier had a Running task to work with.
    $dialogPrompt = 'ACCEPT-ASK. Call the ask tool exactly once with a single question "ACCEPT-Q-DIALOG: which contract should b.js use?" and exactly two options "keep the current contract" and "change the contract". Then stop and wait for that dialog to be answered.'
    Set-PendingUserInteraction -Pending $true -Phase 'awaiting-worker-dialog-and-report-token'
    $dialogResult = Invoke-AgentPrompt -PaneId $paneB -Text $dialogPrompt -TimeoutSeconds 240
    Assert-True -Id 'K1.3' -What 'worker B accepts the instruction to open its dialog' -Condition ($dialogResult.ExitCode -eq 0) -TaskId $idB `
        -Command $dialogResult.CommandLine -Evidence (Protect-Text $dialogResult.Stderr 300)

    $blocked = Wait-Until -TimeoutSeconds 420 -IntervalSeconds 8 -What 'worker B blocked' -Probe {
        $live = Get-LiveAgent -Target $paneB
        if (-not $live.Found) { return $null }
        if (([string](Get-Field $live.Agent 'agent_status')) -ieq 'blocked') { return $live.Agent }
        return $null
    }
    Assert-True -Id 'K2.1' -What 'Herdr reports worker B blocked on a dialog' -Condition $blocked.Ok -TaskId $idB `
        -Evidence ("status now: {0}" -f [string](Get-Field (Get-LiveAgent -Target $paneB).Agent 'agent_status'))

    $visible = Get-PaneScreen -PaneId $paneB -Source 'visible' -Lines 60
    Assert-Match -Id 'K2.2' -What 'the worker screen shows the dialog text (passive visible read)' -Pattern 'ACCEPT-Q-DIALOG' -Actual $visible -TaskId $idB
    if ($visible -match 'ACCEPT-Q-DIALOG') {
        Add-Assertion -Id 'K2.3' -What 'visible dialog captured as evidence' -Status 'INFO' -Evidence (Protect-Text $visible 800)
    } else {
        Block-Item -Id 'K2.3' -What 'visible dialog captured as evidence' -Evidence ("worker status is {0} but the dialog text was not visible" -f [string](Get-Field (Get-LiveAgent -Target $paneB).Agent 'agent_status'))
    }

    $primaryFact = Get-Field $script:Project 'Key'
    $preview = Invoke-BridgeAs -LaunchPaneId ([string](Get-Field $script:Project 'LaunchPaneID')) -Method 'dialog_preview' -Params @{ taskId = $idB }
    Assert-True -Id 'K3.0' -What 'dialog_preview answered with a bridge envelope (ok or a coded error)' -Condition $preview.Envelope -TaskId $idB `
        -Evidence (Protect-Text $preview.Raw 400)
    Assert-True -Id 'K3.1' -What 'the primary-side dialog preview answers' -Condition $preview.Ok -TaskId $idB -Evidence (Protect-Text (Get-Json $preview.Error) 300)
    if ($preview.Ok) {
        Assert-JsonTrue -Id 'K3.2' -What 'the preview reports the worker blocked (a real boolean)' -Object $preview.Result -Name 'blocked' -TaskId $idB
        Assert-Match -Id 'K3.3' -What 'the preview carries the exact worker question and ordered choices' -Pattern '(?s)ACCEPT-Q-DIALOG: which contract should b\.js use\?.*keep the current contract.*change the contract' -Actual ([string](Get-Field $preview.Result 'dialog')) -TaskId $idB
        Assert-NonEmpty -Id 'K3.4' -What 'the preview carries a digest that ties an approval to this dialog' -Value ([string](Get-Field $preview.Result 'digest')) -TaskId $idB
        Assert-Equal -Id 'K3.5' -What 'the preview names the exact current public pane' -Expected $paneB -Actual ([string](Get-Field $preview.Result 'publicPaneId')) -TaskId $idB
    }
    # A report already waiting for review is decided first, through the primary's
    # own review dialog (the primary has no Ask tool), so no review dialog can open
    # in the middle of the approval below.
    $primaryAsks = Resolve-PendingPrimaryDispositionAsks -AssertionPrefix 'K3.6' -TimeoutSeconds 60
    if ($primaryAsks.Resolved -and -not $primaryAsks.Observed) {
        $pendingReview = Invoke-PrimaryReviewForPendingReport -AssertionPrefix 'K3.6'
        if (-not $pendingReview.Resolved) {
            $primaryAsks = [pscustomobject]@{ Resolved = $false; Observed = $pendingReview.Observed; Screen = $pendingReview.Screen }
        } elseif ($pendingReview.Observed) {
            $primaryAsks = Resolve-PendingPrimaryDispositionAsks -AssertionPrefix 'K3.6' -TimeoutSeconds 60
        }
    }
    if (-not $primaryAsks.Resolved) {
        Block-Item -Id 'K3.6b' -What 'the pending report review could not be settled before dialog approval' `
            -Evidence ("reason=[$([string](Get-Field $primaryAsks 'Reason'))] screen=" + (Protect-Text ([string](Get-Field $primaryAsks 'Screen')) 600))
        throw 'a pending report review blocked worker-dialog approval'
    }

    $prompt = 'ACCEPT-DIALOG. I have reviewed the exact dialog for worker ' + $idB + ': question "ACCEPT-Q-DIALOG: which contract should b.js use?" with options "keep the current contract" and "change the contract". As the acceptance user, I explicitly authorize sending only enter to choose "keep the current contract" for this task. Call orch_approve_dialog with taskId "' + $idB + '" and keys ["enter"]; do not act if the confirmation UI shows a different dialog. Then reply with the word approved.'
    # This prompt starts an approval tool that waits for explicit user confirmation.
    # Submit without --wait; the transcript proves the tool call and
    # Confirm-PrimaryModal independently gates any Enter input.
    $result = Invoke-PrimaryPromptNoWaitAfterDispositionAsks -PaneId $script:PrimaryPane -Text $prompt -TimeoutSeconds 240 -AssertionPrefix 'K4'
    $primaryReady = Get-ReadySession -LaunchPaneId ([string](Get-Field $script:Project 'LaunchPaneID'))
    $primaryTranscript = [string](Get-Field $primaryReady 'omp_session_file')
    $approvalRequest = Wait-Until -TimeoutSeconds 120 -IntervalSeconds 3 -What 'exact worker approval call in the primary transcript' -Probe {
        if (Test-OmpApprovalToolCall -Path $primaryTranscript -TaskId $idB -Keys @('enter')) { return $true }
        return $null
    }
    $approvalRecorded = [bool]($result.Submitted -and $approvalRequest.Ok)
    Assert-True -Id 'K4.1' -What 'the primary requested approval for this exact task and key in its OMP transcript' `
        -Condition $approvalRecorded -TaskId $idB -Command $result.CommandLine `
        -Evidence ("code=[$($result.Code)] submitted=[$($result.Submitted)] transcriptCall=[$($approvalRequest.Ok)] transcript=[" + (Protect-Text $primaryTranscript 300) + "]")
    if (-not $approvalRecorded) {
        Block-Item -Id 'K4.1b' -What 'the exact approval request was not recorded; refusing to confirm an unknown dialog' `
            -Evidence ("screen=[" + (Protect-Text (Get-PaneScreen -PaneId $script:PrimaryPane -Source 'visible' -Lines 100) 700) + "]")
        throw 'the exact worker-dialog approval request was not observed'
    }

    $modal = Confirm-PrimaryModal -TaskId $idB -DialogMarker 'ACCEPT-Q-DIALOG' `
        -ExpectedQuestion 'ACCEPT-Q-DIALOG: which contract should b.js use?' -ExpectedKeys @('enter') `
        -TranscriptPath $primaryTranscript -TimeoutSeconds 180
    if ($modal.Observed) {
        Assert-True -Id 'K5.1' -What 'the user confirmation showed the exact worker dialog and its approval key' -Condition $true -TaskId $idB
        Assert-Match -Id 'K5.2' -What 'the confirmation quotes the worker dialog question' -Pattern 'ACCEPT-Q-DIALOG' -Actual $modal.Screen -TaskId $idB
        Add-Assertion -Id 'K5.3' -What "the verified confirmation key was sent [$($modal.Keys -join ' ')]" `
            -Status $(if ($modal.Sent) { 'PASS' } else { 'FAIL' }) -TaskId $idB `
            -Command "herdr agent send-keys $($script:PrimaryPane) $($modal.Keys -join ' ')" -Evidence (Protect-Text $modal.Screen 800)
    } else {
        Block-Item -Id 'K5.1' -What 'the primary did not expose the exact transcript-linked user confirmation' `
            -Evidence (Protect-Text $modal.Screen 700)
        throw 'the worker approval confirmation was not completed; refusing to send keys to an unverified modal'
    }

    $unblocked = Wait-Until -TimeoutSeconds 240 -IntervalSeconds 5 -What 'worker unblocked' -Probe {
        $live = Get-LiveAgent -Target $paneB
        if (-not $live.Found) { return $null }
        $status = [string](Get-Field $live.Agent 'agent_status')
        if ($status -in @('idle', 'working', 'done')) { return $status }
        return $null
    }
    if ($unblocked.Ok) {
        Assert-True -Id 'K6.1' -What 'the approved keys answered the worker dialog and released it' -Condition $true -TaskId $idB -Evidence ("status={0}" -f $unblocked.Value)
        $afterAgent = Get-LiveAgent -Target $paneB
        $afterStatus = [string](Get-Field $afterAgent.Agent 'agent_status')
        $afterScreen = Get-PaneScreen -PaneId $paneB -Source 'visible' -Lines 60
        Assert-True -Id 'K6.2' -What 'Herdr reports no active worker dialog even if its old question remains in scrollback' `
            -Condition ($afterAgent.Found -and $afterStatus -in @('idle', 'working', 'done')) -TaskId $idB `
            -Evidence ("agent_status=$afterStatus historical_question_marker=$($afterScreen -match 'ACCEPT-Q-DIALOG')")
    } else {
        $status = [string](Get-Field (Get-LiveAgent -Target $paneB).Agent 'agent_status')
        Block-Item -Id 'K6.1' -What 'the approved keys answered the worker dialog' `
            -Evidence ("worker B is still $status after the approval; visible screen: " + (Protect-Text (Get-PaneScreen -PaneId $paneB -Source 'visible' -Lines 40) 600))
        throw 'the blocked worker dialog remained active after the user confirmation'
    }
    $reportPrompt = 'ACCEPT-B-REPORT-TRIGGER. Call the orch_answer tool exactly once with taskId "' + $idB + '" and text "ACCEPT-REPORT-B". Then reply with the words report token sent.'
    # orch_answer waits for the user to confirm the exact text, so the prompt is
    # submitted without --wait and Confirm-PrimaryAnswerModal gates the Enter.
    $reportResult = Invoke-PrimaryPromptNoWaitAfterDispositionAsks -PaneId $script:PrimaryPane -Text $reportPrompt -TimeoutSeconds 240 -AssertionPrefix 'K6'
    $primaryReady = Get-ReadySession -LaunchPaneId ([string](Get-Field $script:Project 'LaunchPaneID'))
    $primaryTranscript = [string](Get-Field $primaryReady 'omp_session_file')
    $workerReady = Get-ReadySession -LaunchPaneId ([string](Get-Field $script:TaskB 'LaunchPaneID'))
    $workerTranscript = [string](Get-Field $workerReady 'omp_session_file')
    $reportDelivery = [pscustomobject]@{ Ok = $false }
    $reportModal = [pscustomobject]@{ Observed = $false; Confirmed = $false; DeliveredBeforeConfirm = $false; Screen = '' }
    if ($reportResult.Submitted) {
        $reportModal = Confirm-PrimaryAnswerModal -TaskId $idB -Text 'ACCEPT-REPORT-B' -TranscriptPath $primaryTranscript `
            -WorkerTranscriptPath $workerTranscript
        Assert-True -Id 'K6.3a' -What 'the report token had not reached worker B before the user confirmed the exact answer dialog' `
            -Condition ($reportModal.Observed -and -not $reportModal.DeliveredBeforeConfirm) -TaskId $idB `
            -Evidence ("observed=[$($reportModal.Observed)] confirmed=[$($reportModal.Confirmed)] delivered_before_confirm=[$($reportModal.DeliveredBeforeConfirm)] screen=" + (Protect-Text $reportModal.Screen 400))
    }
    if ($reportModal.Confirmed) {
        $reportDelivery = Wait-Until -TimeoutSeconds 300 -IntervalSeconds 3 -What 'exact B report authorization orch_answer reached the worker transcript' -Probe {
            $answerCall = Test-OmpTranscriptContainsAnswerToolCall -Path $primaryTranscript -TaskId $idB -Text 'ACCEPT-REPORT-B'
            $workerMessage = Test-OmpTranscriptContainsExactUserMessage -Path $workerTranscript -Text 'ACCEPT-REPORT-B'
            if ($answerCall -and $workerMessage) { return $true }
            return $null
        }
    }
    $reportAnswerCallObserved = Test-OmpTranscriptContainsAnswerToolCall -Path $primaryTranscript -TaskId $idB -Text 'ACCEPT-REPORT-B'
    $reportTokenObserved = Test-OmpTranscriptContainsExactUserMessage -Path $workerTranscript -Text 'ACCEPT-REPORT-B'
    $reportSent = [bool]($reportResult.Submitted -and $reportModal.Confirmed -and $reportDelivery.Ok -and $reportAnswerCallObserved -and $reportTokenObserved)
    Assert-True -Id 'K6.3' -What 'the primary called orch_answer for worker B, the user confirmed its exact text, and the report token reached its transcript' `
        -Condition $reportSent -TaskId $idB -Command $reportResult.CommandLine `
        -Evidence ("code=[$($reportResult.Code)] submitted=[$($reportResult.Submitted)] confirmed=[$($reportModal.Confirmed)] answerCall=[$reportAnswerCallObserved] tokenDelivered=[$reportTokenObserved] workerSession=[$(Protect-Text $workerTranscript 300)]")
    if (-not $reportSent) {
        Preserve-ResourcesForPendingTool -AssertionId 'K6.3.resources-preserved' -Reason 'worker B report authorization delivery was not proven'
        throw 'the exact orch_answer call and later worker user message were not proven; no retry was sent'
    }

    Set-PendingUserInteraction -Pending $false -Phase 'dialog-and-report-token-resolved'

    $stale = Invoke-BridgeAs -LaunchPaneId ([string](Get-Field $script:Project 'LaunchPaneID')) -Method 'approve_dialog' `
        -Params @{ taskId = $idB; observedDigest = [string](Get-Field $preview.Result 'digest'); keys = @('enter') }
    Assert-True -Id 'K7.1' -What 'the approved digest cannot be replayed after the dialog is gone' -Condition (-not $stale.Ok) -TaskId $idB `
        -Evidence (Protect-Text (Get-Json $stale.Error) 400)
    # The refusal must be the dialog-specific one: `not_blocked` when the worker
    # is no longer blocked, `dialog_changed` when it shows a different dialog.
    $statusNow = [string](Get-Field (Get-LiveAgent -Target $paneB).Agent 'agent_status')
    $expectedCode = if ($statusNow -ieq 'blocked') { 'dialog_changed' } else { 'not_blocked' }
    Assert-Equal -Id 'K7.2' -What 'the replayed approval is refused with the dialog-specific code for the observed state' `
        -Expected $expectedCode -Actual ([string](Get-Field $stale.Error 'code')) -TaskId $idB `
        -Evidence ("agent_status=[$statusNow] " + (Protect-Text ([string](Get-Field $stale.Error 'message')) 300))
}

# ---------------------------------------------------------------------------
# Phase 13: a moved worker pane whose source workspace closes
# ---------------------------------------------------------------------------

function Invoke-PaneMove {
    Write-Step 'Phase 13: move the worker pane so its source workspace closes'
    $fact = $script:TaskB
    $idB = [string](Get-Field $fact 'ID')
    $pane = [string](Get-Field $fact 'PublicPaneID')
    $sourceWorkspace = [string](Get-Field $fact 'WorktreeWorkspaceID')
    $before = Get-Json $fact

    $move = Invoke-Herdr -Arguments @('pane', 'move', $pane, '--new-workspace', '--label', "accept-moved-$($script:RunId)", '--no-focus') -TimeoutSeconds 120
    Assert-Equal -Id 'M1.1' -What 'pane move exits 0' -Expected 0 -Actual $move.ExitCode -Command $move.CommandLine -Evidence (Protect-Text $move.Stderr 400)
    if ($move.ExitCode -ne 0) { Block-Item -Id 'M1.2' -What 'cross-workspace move' -Evidence (Protect-Text $move.Stderr 400); return }
    $moveParsed = $move.Stdout | ConvertFrom-Json
    $moved = Get-Field (Get-Field $moveParsed 'result' $null) 'move_result' $null
    $newPane = [string](Get-Field (Get-Field $moved 'pane') 'pane_id')
    $newWorkspace = [string](Get-Field (Get-Field $moved 'pane') 'workspace_id')
    [void]$script:CreatedPanes.Add($newPane)
    [void]$script:CreatedWorkspaces.Add($newWorkspace)
    Save-AcceptanceRunManifest -Phase 'worker-pane-moved' -Status 'active'

    Assert-Equal -Id 'M2.1' -What 'the move reports the launch-time pane id as previous_pane_id' -Expected $pane -Actual ([string](Get-Field $moved 'previous_pane_id'))
    Assert-True -Id 'M2.2' -What 'the move produced a new public pane id' -Condition ($newPane -ne '' -and $newPane -ne $pane) -Evidence "new=$newPane old=$pane"
    Assert-Equal -Id 'M2.3' -What 'the moved pane keeps its terminal' -Expected ([string](Get-Field $fact 'TerminalID')) -Actual ([string](Get-Field (Get-Field $moved 'pane') 'terminal_id'))
    Assert-Equal -Id 'M2.4' -What 'the move closed the source (worktree) workspace' -Expected $sourceWorkspace -Actual ([string](Get-Field $moved 'closed_workspace_id'))
    Assert-NonEmpty -Id 'M2.5' -What 'the move closed the source tab' -Value ([string](Get-Field $moved 'closed_tab_id'))
    Assert-SamePath -Id 'M2.6' -What 'the moved pane still works in the recorded checkout' -Expected ([string](Get-Field $fact 'WorktreePath')) -Actual ([string](Get-Field (Get-Field $moved 'pane') 'cwd'))

    $gone = Invoke-Herdr -Arguments @('workspace', 'get', $sourceWorkspace)
    Assert-True -Id 'M3.1' -What 'the closed source workspace can no longer be read' -Condition ($gone.ExitCode -ne 0) -Evidence (Protect-Text $gone.Stderr 300)
    $list = Get-HerdrJson -Arguments @('workspace', 'list')
    $stillThere = @((Get-JsonField $list 'workspaces' @()) | Where-Object { (Get-Field $_ 'workspace_id') -eq $sourceWorkspace })
    Assert-Equal -Id 'M3.2' -What 'the source workspace is absent from the live list' -Expected 0 -Actual $stillThere.Count
    [void]$script:CreatedWorkspaces.Remove($sourceWorkspace)
    Save-AcceptanceRunManifest -Phase 'source-workspace-closed' -Status 'active'

    $wait = Wait-Until -TimeoutSeconds 240 -IntervalSeconds 5 -What 'move applied to the store' -Probe {
        $row = Get-TaskById -Id $idB
        if (-not $row) { return $null }
        $now = Get-Field $row 'fact'
        if (([string](Get-Field $now 'PublicPaneID')) -eq $newPane) { return $now }
        return $null
    }
    Assert-True -Id 'M4.1' -What 'the pane.moved hook updated the recorded public pane' -Condition $wait.Ok -TaskId $idB `
        -Evidence ("recorded={0} expected={1}" -f [string](Get-Field (Get-Field (Get-TaskById -Id $idB) 'fact' $null) 'PublicPaneID'), $newPane)
    if ($wait.Ok) {
        $script:TaskB = $wait.Value
        Assert-Equal -Id 'M4.2' -What 'the current workspace follows the pane' -Expected $newWorkspace -Actual ([string](Get-Field $script:TaskB 'CurrentWorkspaceID')) -TaskId $idB
        Assert-Equal -Id 'M4.3' -What 'the launch-time pane id is never re-keyed by a move' -Expected $pane -Actual ([string](Get-Field $script:TaskB 'LaunchPaneID')) -TaskId $idB
        Assert-Equal -Id 'M4.4' -What 'the worktree workspace provenance survives the close' -Expected $sourceWorkspace -Actual ([string](Get-Field $script:TaskB 'WorktreeWorkspaceID')) -TaskId $idB
        Assert-Equal -Id 'M4.5' -What 'the checkout path provenance survives the move' -Expected ([string](Get-Field $fact 'WorktreePath')) -Actual ([string](Get-Field $script:TaskB 'WorktreePath')) -TaskId $idB
        Assert-Equal -Id 'M4.6' -What 'the terminal identity survives the move' -Expected ([string](Get-Field $fact 'TerminalID')) -Actual ([string](Get-Field $script:TaskB 'TerminalID')) -TaskId $idB
        Assert-Equal -Id 'M4.7' -What 'the OMP session identity survives the move' -Expected ([string](Get-Field $fact 'OMPSessionID')) -Actual ([string](Get-Field $script:TaskB 'OMPSessionID')) -TaskId $idB
        Assert-Equal -Id 'M4.8' -What 'the move left every identity, provenance and content field of the task untouched' `
            -Expected (Get-TaskProjection -Fact $fact -MoveSensitive) `
            -Actual (Get-TaskProjection -Fact $script:TaskB -MoveSensitive) -TaskId $idB
    }

    $listing = Get-WorktreeList
    $entry = @((Get-Field $listing 'worktrees' @()) | Where-Object { Test-SamePath -A (Get-Field $_ 'path') -B ([string](Get-Field $fact 'WorktreePath')) })
    Assert-Equal -Id 'M5.1' -What 'the worktree listing still names the moved checkout (provenance, not the closed workspace)' -Expected 1 -Actual $entry.Count -TaskId $idB
    if ($entry.Count -gt 0) {
        Assert-Equal -Id 'M5.2' -What 'the listing still reports the recorded branch' -Expected ([string](Get-Field $fact 'Branch')) -Actual ([string](Get-Field $entry[0] 'branch')) -TaskId $idB
        $open = [string](Get-Field $entry[0] 'open_workspace_id')
        Assert-True -Id 'M5.3' -What 'the listing never points at the closed workspace' -Condition ($open -ne $sourceWorkspace) -TaskId $idB -Evidence "open=$open closed=$sourceWorkspace"
    }

    $resolve = Invoke-BridgeAs -LaunchPaneId $pane -Method 'resolve' -Params @{}
    Assert-True -Id 'M6.0' -What 'resolve answered with a bridge envelope (ok or a coded error)' -Condition $resolve.Envelope -TaskId $idB `
        -Evidence (Protect-Text $resolve.Raw 400)
    Assert-True -Id 'M6.1' -What 'reporting still resolves for the moved worker through its launch pane' -Condition $resolve.Ok -TaskId $idB -Evidence (Protect-Text (Get-Json $resolve.Error) 300)
    if ($resolve.Ok) {
        Assert-JsonTrue -Id 'M6.2' -What 'resolve still binds the task to the worker role (a real boolean)' -Object $resolve.Result -Name 'bound' -TaskId $idB
        Assert-Equal -Id 'M6.2b' -What 'resolve reports the worker role' -Expected 'worker' -Actual ([string](Get-Field $resolve.Result 'role')) -TaskId $idB
        Assert-Equal -Id 'M6.3' -What 'resolve names the exact task id' -Expected $idB -Actual ([string](Get-Field $resolve.Result 'taskId')) -TaskId $idB
        Assert-Equal -Id 'M6.4' -What 'resolve still reports the recorded worktree workspace as provenance (not the closed source)' `
            -Expected ([string](Get-Field $fact 'WorktreeWorkspaceID')) -Actual ([string](Get-Field $resolve.Result 'worktreeId')) -TaskId $idB
        Assert-Equal -Id 'M6.5' -What 'resolve reports the moved pane as the current public pane' -Expected $newPane -Actual ([string](Get-Field $resolve.Result 'publicPaneId')) -TaskId $idB
        Assert-Equal -Id 'M6.6' -What 'resolve keeps the launch-time pane identity' -Expected $pane -Actual ([string](Get-Field $resolve.Result 'launchPaneId')) -TaskId $idB
        Assert-Equal -Id 'M6.7' -What 'resolve keeps the recorded terminal identity' -Expected ([string](Get-Field $fact 'TerminalID')) -Actual ([string](Get-Field $resolve.Result 'terminalId')) -TaskId $idB
    }
}

# ---------------------------------------------------------------------------
# Phase 14: two complete reports
# ---------------------------------------------------------------------------

function Wait-ForReport {
    param([string]$TaskId, [double]$TimeoutSeconds = 600)
    return Wait-Until -TimeoutSeconds $TimeoutSeconds -IntervalSeconds 10 -What "report of $TaskId" -Probe {
        $row = Get-TaskById -Id $TaskId
        if (-not $row) { return $null }
        $fact = Get-Field $row 'fact'
        $report = Get-Field $fact 'Report' $null
        if ($null -ne $report -and ([string](Get-Field $report 'Kind')) -eq 'result') { return $fact }
        return $null
    }
}

function Invoke-WorkerReports {
    $isSingle = $Scenario -eq 'SingleWorker'
    Write-Step $(if ($isSingle) { 'SingleWorker: wait for one final result report' } else { 'Phase 14: two complete reports with evidence and checks' })
    foreach ($pair in @(@{ Name = 'A'; Fact = $script:TaskA }, @{ Name = 'B'; Fact = $script:TaskB })) {
        $fact = $pair.Fact
        if (-not $fact) { continue }
        $id = [string](Get-Field $fact 'ID')
        if (-not $isSingle -and $pair.Name -eq 'A') {
            $primaryReady = Get-ReadySession -LaunchPaneId ([string](Get-Field $script:Project 'LaunchPaneID'))
            $primaryTranscript = [string](Get-Field $primaryReady 'omp_session_file')
            if (-not (Test-OmpTranscriptContainsLaterUserAuthorization -Path $primaryTranscript -TaskId $id `
                -QuestionMarker 'ACCEPT-Q-A' -Token 'ACCEPT-ANSWER-A')) {
                Block-Item -Id 'W1.A' -What 'worker A cannot report before a later user message supplies ACCEPT-ANSWER-A' `
                    -Evidence 'the worker remains gated; no report timeout was started and no answer was injected'
                Preserve-ResourcesForPendingTool -AssertionId 'W1.A.resources-preserved' -Reason 'worker A remains gated on its direct user token'
                continue
            }
        }
        $liveBeforeReport = Get-LiveAgent -Target ([string](Get-Field $fact 'PublicPaneID'))
        if ($liveBeforeReport.Found -and [string](Get-Field $liveBeforeReport.Agent 'agent_status') -ieq 'blocked') {
            Set-PendingUserInteraction -Pending $true -Phase "worker-$($pair.Name)-awaiting-user"
        }
        $reportTimeout = if ($isSingle) { 300 } else { 600 }
        $wait = Wait-ForReport -TaskId $id -TimeoutSeconds $reportTimeout
        Assert-True -Id "W1.$($pair.Name)" -What "worker $($pair.Name) submitted a final result" -Condition $wait.Ok -TaskId $id `
            -Evidence ("after {0:N0}s: phase=[{1}] attention=[{2}]" -f $wait.Elapsed, [string](Get-Field (Get-Field (Get-TaskById -Id $id) 'fact' $null) 'Phase'), [string](Get-Field (Get-Field (Get-TaskById -Id $id) 'fact' $null) 'Attention'))
        if (-not $wait.Ok) {
            $screen = Get-PaneScreen -PaneId ([string](Get-Field $fact 'PublicPaneID')) -Source 'recent-unwrapped' -Lines 120
            Block-Item -Id "W1.$($pair.Name)-screen" -What "worker $($pair.Name) terminal content at timeout" -Evidence (Protect-Text $screen 800)
            $liveAtTimeout = Get-LiveAgent -Target ([string](Get-Field $fact 'PublicPaneID'))
            if ($liveAtTimeout.Found -and [string](Get-Field $liveAtTimeout.Agent 'agent_status') -ieq 'blocked') {
                Preserve-ResourcesForPendingTool -AssertionId "W1.$($pair.Name).resources-preserved" -Reason 'worker report wait ended while the worker remained blocked'
            }
            if ($isSingle) { throw 'single-worker final result was not observed before its report timeout' }
            continue
        }
        if ($script:PendingUserInteraction) {
            Set-PendingUserInteraction -Pending $false -Phase "worker-$($pair.Name)-report-observed"
        }
        $reported = $wait.Value
        if ($pair.Name -eq 'A') { $script:TaskA = $reported } else { $script:TaskB = $reported }
        $report = Get-Field $reported 'Report'
        $actualStage = Get-TaskStage -Fact $reported
        $disposition = [string](Get-Field $reported 'Disposition')
        if ($disposition) {
            Assert-Equal -Id "W2.$($pair.Name)-stage" -What "task $id is Done because an explicit disposition was recorded" -Expected 'done' -Actual $actualStage -TaskId $id
            Assert-NonEmpty -Id "W2.$($pair.Name)-disposition" -What 'the earlier user choice is recorded on the report' -Value $disposition -TaskId $id
        } else {
            Assert-Equal -Id "W2.$($pair.Name)-stage" -What "task $id is Ready for review" -Expected 'ready_for_review' -Actual $actualStage -TaskId $id
        }
        Assert-Equal -Id "W2.$($pair.Name)-branch" -What "the report names the branch this task owns" -Expected ([string](Get-Field $reported 'Branch')) -Actual ([string](Get-Field $report 'Branch')) -TaskId $id
        Assert-NonEmpty -Id "W2.$($pair.Name)-summary" -What 'the report carries a summary' -Value ([string](Get-Field $report 'Summary')) -TaskId $id
        Assert-True -Id "W2.$($pair.Name)-evidence" -What 'the report carries changed-file/commit evidence' -Condition (@(Get-Field $report 'Evidence' @()).Count -ge 1) -TaskId $id -Evidence (Protect-Text (Get-Json (Get-Field $report 'Evidence' @())) 300)
        Assert-True -Id "W2.$($pair.Name)-checks" -What 'the report carries the checks that ran' -Condition (@(Get-Field $report 'Checks' @()).Count -ge 1) -TaskId $id -Evidence (Protect-Text (Get-Json (Get-Field $report 'Checks' @())) 300)
        $reportChecks = @(Get-Field $report 'Checks' @())
        $statedOutcomes = @($reportChecks | Where-Object { [string](Get-Field $_ 'Outcome') -cin @('passed', 'failed', 'skipped') })
        Assert-True -Id "W2.$($pair.Name)-check-outcomes" -What 'every check in the report states its outcome (passed, failed or skipped)' `
            -Condition ($reportChecks.Count -ge 1 -and $statedOutcomes.Count -eq $reportChecks.Count) -TaskId $id -Evidence (Protect-Text (Get-Json $reportChecks) 300)
        Assert-NonEmpty -Id "W2.$($pair.Name)-risk" -What 'the report states its risk' -Value ([string](Get-Field $report 'Risk')) -TaskId $id
        Assert-True -Id "W2.$($pair.Name)-clean-attention" -What 'a final report clears the worker question attention' -Condition ([string]::IsNullOrWhiteSpace([string](Get-Field $reported 'Attention'))) -TaskId $id
        $dump = Get-StateDump
        $note = @($dump.tables.inbox | Where-Object { (Get-Field $_ 'task_id') -eq $id -and (Get-Field $_ 'kind') -eq 'result' })
        Assert-Equal -Id "W3.$($pair.Name)-note" -What 'exactly one result note was recorded for the primary' -Expected 1 -Actual $note.Count -TaskId $id
        Assert-OmpSessionModel -Id "W2.$($pair.Name)-model" -What "worker $($pair.Name) ran on the requested model" `
            -LaunchPaneId ([string](Get-Field $reported 'LaunchPaneID')) -TaskId $id
    }

    # The reported evidence must be true of the real checkout.
    if ($script:TaskA) {
        $pathA = [string](Get-Field $script:TaskA 'WorktreePath')
        $headA = (Invoke-Git -Arguments @('rev-parse', 'HEAD') -Directory $pathA).Stdout.Trim()
        Assert-True -Id 'W4.A-changed' -What 'worker A really committed a change on its branch' -Condition ($headA -ne $script:BaseRevision) -TaskId ([string](Get-Field $script:TaskA 'ID')) -Evidence "head=$headA base=$($script:BaseRevision)"
        $report = Get-Field $script:TaskA 'Report'
        if ($null -eq $report) {
            Block-Item -Id 'W4.A-evidence-true' -What "worker A's evidence cannot be checked before an authorized report exists" -Evidence 'the result-report gate was not satisfied'
        } else {
            $checkNames = @(Get-Field $report 'Checks' @()) | ForEach-Object { if ($_ -is [string]) { $_ } else { [string](Get-Field $_ 'Name') } }
            $joined = (@(Get-Field $report 'Evidence' @()) + @($checkNames)) -join ' '
            Assert-True -Id 'W4.A-evidence-true' -What "worker A's evidence names its real commit or the file it changed" `
                -Condition (($joined -match [regex]::Escape($headA.Substring(0, 12))) -or ($joined -match 'a\.js')) -TaskId ([string](Get-Field $script:TaskA 'ID')) -Evidence (Protect-Text $joined 400)
        }
    }
    if ($script:TaskB) {
        $pathB = [string](Get-Field $script:TaskB 'WorktreePath')
        $statusB = (Invoke-Git -Arguments @('status', '--porcelain') -Directory $pathB).Stdout.Trim()
        Assert-True -Id 'W4.B-dirty' -What "worker B's change is really present and uncommitted (the dirt the cleanup refusal relies on)" -Condition ($statusB -ne '') -TaskId ([string](Get-Field $script:TaskB 'ID')) -Evidence (Protect-Text $statusB 300)
    }

    $primaryReady = Get-ReadySession -LaunchPaneId ([string](Get-Field $script:Project 'LaunchPaneID'))
    $primaryTranscript = [string](Get-Field $primaryReady 'omp_session_file')
    $resultNoteMarkers = @()
    foreach ($resultTask in @($script:TaskA, $script:TaskB)) {
        if ($null -eq $resultTask) { continue }
        $resultReport = Get-Field $resultTask 'Report' $null
        if ($null -eq $resultReport -or [string](Get-Field $resultReport 'Kind') -cne 'result') { continue }
        $resultNoteMarkers += [pscustomobject]@{
            TaskId  = [string](Get-Field $resultTask 'ID')
            Summary = [string](Get-Field $resultReport 'Summary')
        }
    }
    $delivery = Wait-Until -TimeoutSeconds 120 -IntervalSeconds 3 -What 'result notes reached the primary session' -Probe {
        if ($resultNoteMarkers.Count -eq 0) { return $null }
        foreach ($marker in $resultNoteMarkers) {
            if (-not (Test-OmpTranscriptContainsResultNote -Path $primaryTranscript `
                -TaskId $marker.TaskId -Summary $marker.Summary)) { return $null }
        }
        return $true
    }
    if ($delivery.Ok) {
        Assert-True -Id 'W5.1' -What 'the result notes were delivered into the primary session by the plugin' `
            -Condition $true -Evidence ("matched {0} result note(s) in the primary OMP transcript" -f $resultNoteMarkers.Count)
    } else {
        $primaryScreen = Get-PaneScreen -PaneId $script:PrimaryPane -Source 'recent-unwrapped' -Lines 300
        Block-Item -Id 'W5.1' -What 'the result notes were delivered into the primary session by the plugin' `
            -Evidence ("the report markers did not reach the primary OMP transcript within 120s: " + (Protect-Text $primaryScreen 500))
    }
}

# ---------------------------------------------------------------------------
# Phase 15: the board renders the tasks (task facts unchanged, no ticket write key)
# ---------------------------------------------------------------------------

function Get-BoardSelectionKey {
    param([string]$Screen, [string]$CurrentId, [string]$TargetId)
    if ([string]::IsNullOrWhiteSpace($Screen) -or [string]::IsNullOrWhiteSpace($CurrentId) -or
        [string]::IsNullOrWhiteSpace($TargetId) -or $CurrentId -eq $TargetId) { return '' }
    $currentIndex = $Screen.IndexOf("[$CurrentId]", [System.StringComparison]::Ordinal)
    $targetIndex = $Screen.IndexOf("[$TargetId]", [System.StringComparison]::Ordinal)
    if ($currentIndex -lt 0 -or $targetIndex -lt 0) { return '' }
    if ($targetIndex -lt $currentIndex) { return 'up' }
    return 'down'
}

function Invoke-Board {
    Write-Step 'Phase 15: the board renders the tasks and no ticket write key is pressed'
    $tasksBefore = @(Get-ProjectTasks -Key (Get-Field $script:Project 'Key'))
    # Worker-status hooks may legitimately update Activity, ActivitySeenAt and
    # Revision while the interactive board is open; compare workflow facts only.
    $taskWorkflowBefore = (@($tasksBefore | Sort-Object { Get-Field $_ 'id' } |
        ForEach-Object { Get-TaskProjection -Fact (Get-Field $_ 'fact') }) -join '|')
    $focusSnapshotBefore = Get-Snapshot
    $focusBefore = Get-FocusProjection -Snapshot $focusSnapshotBefore
    $focusWorkspaceBefore = [string](Get-Field $focusSnapshotBefore 'focused_workspace_id')
    if ([string]::IsNullOrWhiteSpace($focusWorkspaceBefore)) { $focusWorkspaceBefore = $script:PrimaryWorkspace }

    # The registered `board` action is what a user invokes, and it is the route
    # that knows the bound workspace: `plugin pane open --no-focus` cannot place an
    # overlay (overlay/popup target the active pane), so the action is the correct
    # surface and this phase asserts its answer instead of guessing the argv.
    $context = @{
        'HERDR_ENV'          = '1'
        'HERDR_PANE_ID'      = $script:PrimaryPane
        'HERDR_WORKSPACE_ID' = $script:PrimaryWorkspace
        'HERDR_TAB_ID'       = $script:PrimaryTab
    }
    $invoke = Invoke-PluginAction -ActionId 'board' -PaneContext $context -TimeoutSeconds 180
    Assert-True -Id 'A1.0' -What 'the board action returns an invocation envelope' -Condition ($null -ne $invoke) -Evidence (Protect-Text (Get-Json $invoke) 400)
    if (-not $invoke) { Block-Item -Id 'A1.1' -What 'board pane' -Evidence 'the board action could not be invoked'; return }
    Assert-True -Id 'A1.0b' -What 'the board invocation names the isolated session' `
        -Condition ($invoke.Invocation.CommandLine -match [regex]::Escape("--session $($script:SessionName)")) `
        -Evidence $invoke.Invocation.CommandLine
    $output = Get-BindPayload -Result (Get-Field $invoke 'Result') -TimeoutSeconds 180
    Assert-True -Id 'A1.1' -What 'the board action finished' -Condition ($null -ne $output) -Evidence (Protect-Text (Get-Json $output) 400)
    if (-not $output) { Block-Item -Id 'A1.2' -What 'board pane' -Evidence 'the board action never finished'; return }
    Assert-Equal -Id 'A1.1b' -What 'the board action exits 0' -Expected 0 -Actual (Get-Field $output.Entry 'exit_code') -Evidence (Protect-Text (Get-Json $output.Entry) 400)
    $payload = $output.Payload
    Assert-JsonTrue -Id 'A1.1c' -What 'the board action answered ok=true (a real boolean)' -Object $payload -Name 'ok'
    Assert-SamePath -Id 'A1.1d' -What 'the board action opened the board for the bound project' -Expected ([string](Get-Field $script:Project 'Key')) -Actual ([string](Get-Field $payload 'project'))
    Assert-Equal -Id 'A1.1e' -What 'the board action opened the board in the bound workspace' -Expected $script:PrimaryWorkspace -Actual ([string](Get-Field $payload 'workspace'))
    # Herdr wraps the PluginPaneInfo under `plugin_pane` in the CLI result.
    $opened = Get-Field $payload 'pane'
    $pluginPane = Get-Field $opened 'plugin_pane' $opened
    $boardPane = [string](Get-Field (Get-Field $pluginPane 'pane') 'pane_id')
    if (-not $boardPane) {
        # Fall back to the only pane that appeared in the project workspace while
        # the action ran, so a wrapped answer shape cannot hide a real pane.
        $live = @((Get-JsonField (Get-HerdrJson -Arguments @('pane', 'list', '--workspace', $script:PrimaryWorkspace)) 'panes' @()) |
            Where-Object { (Get-Field $_ 'pane_id') -ne $script:PrimaryPane })
        if ($live.Count -eq 1) { $boardPane = [string](Get-Field $live[0] 'pane_id') }
    }
    Assert-NonEmpty -Id 'A1.2' -What 'the board pane has an id' -Value $boardPane -Evidence (Protect-Text (Get-Json $opened) 400)
    if (-not $boardPane) { Block-Item -Id 'A1.2b' -What 'the board pane could not be identified' -Evidence (Protect-Text (Get-Json $payload) 500); return }
    [void]$script:CreatedPanes.Add($boardPane)
    Save-AcceptanceRunManifest -Phase 'board-pane-created' -Status 'active'
    $script:BoardPane = $boardPane
    Assert-Equal -Id 'A1.3' -What 'the board pane is the declared plugin entrypoint' -Expected 'board' -Actual ([string](Get-Field $pluginPane 'entrypoint'))
    Assert-Equal -Id 'A1.4' -What 'the board pane belongs to this plugin' -Expected $script:PluginId -Actual ([string](Get-Field $pluginPane 'plugin_id'))

    $screen = ''
    $rendered = Wait-Until -TimeoutSeconds 60 -IntervalSeconds 3 -What 'board render' -Probe {
        $text = Get-PaneScreen -PaneId $boardPane -Source 'visible' -Lines 60 -Plain
        if ($text -match 'ACCEPT') { return $text }
        if ($text -match 'Queued|Ready for review|Needs input') { return $text }
        return $null
    }
    if ($rendered.Ok) { $screen = $rendered.Value }
    Add-Assertion -Id 'A2.1' -What 'the board rendered something' -Status $(if ($rendered.Ok) { 'PASS' } else { 'FAIL' }) -Evidence (Protect-Text $screen 900)
    $columnStages = @('queued', 'running', 'needs_input', 'ready_for_review', 'done')
    foreach ($task in @($script:TaskA, $script:TaskB)) {
        if (-not $task) { continue }
        $id = [string](Get-Field $task 'ID')
        $branch = [string](Get-Field $task 'Branch')
        Assert-Match -Id "A2.id/$id" -What "the board shows task $id" -Pattern ([regex]::Escape($id)) -Actual $screen
        $selectedId = [regex]::Match($screen, 'Selected\s+(?<id>orch_[0-9a-f]{12})\s+\|').Groups['id'].Value
        if ($selectedId -ne $id) {
            $selectedFact = $null
            foreach ($candidate in @($script:TaskA, $script:TaskB)) {
                if ([string](Get-Field $candidate 'ID') -eq $selectedId) { $selectedFact = $candidate; break }
            }
            $fromIndex = if ($selectedFact) { [Array]::IndexOf($columnStages, (Get-TaskStage -Fact $selectedFact)) } else { 0 }
            $toIndex = [Array]::IndexOf($columnStages, (Get-TaskStage -Fact $task))
            if ($fromIndex -ge 0 -and $toIndex -ge 0 -and $fromIndex -ne $toIndex) {
                $key = if ($toIndex -gt $fromIndex) { 'tab' } else { 'shift+tab' }
                $steps = [Math]::Abs($toIndex - $fromIndex)
                $keys = @()
                for ($step = 0; $step -lt $steps; $step++) { $keys += $key }
            } else {
                $key = Get-BoardSelectionKey -Screen $screen -CurrentId $selectedId -TargetId $id
                if ($key) { $keys = @($key) } else { $keys = @('down') }
            }
            foreach ($key in $keys) {
                $move = Invoke-Herdr -Arguments @('pane', 'send-keys', $boardPane, $key) -TimeoutSeconds 30
                Assert-Equal -Id "A2.key/$id/$key" -What "the board accepts $key to navigate to task $id" -Expected 0 -Actual $move.ExitCode -Command $move.CommandLine -Evidence (Protect-Text $move.Stderr 200)
                if ($move.ExitCode -ne 0) { break }
            }
            $selected = Wait-Until -TimeoutSeconds 20 -IntervalSeconds 1 -What "board selection $id" -Probe {
                $candidateScreen = Get-PaneScreen -PaneId $boardPane -Source 'visible' -Lines 60 -Plain
                $selectedPattern = 'Selected\s+' + [regex]::Escape($id) + '\s+\|\s+branch\s+' + [regex]::Escape($branch)
                if ($candidateScreen -match $selectedPattern) { return $candidateScreen }
                return $null
            }
            if ($selected.Ok) { $screen = $selected.Value }
        }
        Assert-Match -Id "A2.branch/$id" -What "the selected board card shows the branch of $id" -Pattern ([regex]::Escape($branch)) -Actual $screen
    }
    Assert-Match -Id 'A2.columns' -What 'the board shows the stage columns of this build' -Pattern 'Queued|Ready for review|Running|Needs input' -Actual $screen
    if ($screen -match 'Queued' -and $screen -match 'Done') {
        Assert-True -Id 'A2.wide' -What 'the wide layout shows all five stage columns' -Condition (($screen -match 'Running') -and ($screen -match 'Needs input') -and ($screen -match 'Ready for review')) -Evidence (Protect-Text $screen 500)
    }
    $boardPaneInfo = (Get-HerdrJson -Arguments @('pane', 'get', $boardPane)).Result
    # The action route does not pass --no-focus (an overlay/popup cannot be placed
    # without an active pane), so the board may take focus while it is up. That is
    # expected; what is asserted is that the session is returned to the bound
    # workspace's pane at the end of the phase.
    Add-Assertion -Id 'A3.1' -What 'focus while the board pane is open (recorded, not required to be elsewhere)' -Status 'INFO' `
        -Evidence ("focused=[$(Get-Field $boardPaneInfo 'focused')] workspace=[$(Get-Field $boardPaneInfo 'workspace_id')]")

    $tasksAfter = @(Get-ProjectTasks -Key (Get-Field $script:Project 'Key'))
    $taskWorkflowAfter = (@($tasksAfter | Sort-Object { Get-Field $_ 'id' } |
        ForEach-Object { Get-TaskProjection -Fact (Get-Field $_ 'fact') }) -join '|')
    Assert-Equal -Id 'A4.1' -What 'reading the board left task workflow facts unchanged (live activity/revisions excluded)' `
        -Expected $taskWorkflowBefore -Actual $taskWorkflowAfter

    $close = Invoke-Herdr -Arguments @('pane', 'close', $boardPane) -TimeoutSeconds 60
    Assert-Equal -Id 'A5.1' -What 'the board pane closes cleanly' -Expected 0 -Actual $close.ExitCode -Evidence (Protect-Text $close.Stderr 300)
    $gone = Invoke-Herdr -Arguments @('pane', 'get', $boardPane) -TimeoutSeconds 60
    Assert-True -Id 'A5.2' -What 'the board pane is gone' -Condition ($gone.ExitCode -ne 0) -Evidence (Protect-Text $gone.Stderr 200)
    [void]$script:CreatedPanes.Remove($boardPane)
    Save-AcceptanceRunManifest -Phase 'board-pane-closed' -Status 'active'
    # Put the session back exactly where the phase found it (the board action
    # legitimately moved focus to its own pane): the captured focused workspace is
    # restored, and the whole projection - which now includes the focused workspace
    # and pane ids - must match the pre-phase one.
    [void](Invoke-Herdr -Arguments @('workspace', 'focus', $focusWorkspaceBefore) -TimeoutSeconds 60)
    $restored = Wait-Until -TimeoutSeconds 60 -IntervalSeconds 2 -What 'focus restored' -Probe {
        $snap = Get-Snapshot
        if ([string](Get-Field $snap 'focused_workspace_id') -ne $focusWorkspaceBefore) { return $null }
        return (Get-FocusProjection -Snapshot $snap)
    }
    Assert-True -Id 'A5.3' -What 'the session focus (workspace and pane) is exactly what it was before the board phase' `
        -Condition ($restored.Ok -and $restored.Value -ceq $focusBefore) -Evidence ("restored=[" + (Protect-Text ([string]$restored.Value) 400) + "] before=[" + (Protect-Text $focusBefore 400) + "]")
}

# ---------------------------------------------------------------------------
# Phase 16: the primary records the user's disposition
# ---------------------------------------------------------------------------

function Invoke-Reviews {
    $isSingle = $Scenario -eq 'SingleWorker'
    Write-Step $(if ($isSingle) { 'SingleWorker: record one explicit review disposition' } else { 'Phase 16: explicit dispositions (nothing merges automatically)' })
    $idA = if ($script:TaskA) { [string](Get-Field $script:TaskA 'ID') } else { '' }
    $idB = if ($script:TaskB) { [string](Get-Field $script:TaskB 'ID') } else { '' }
    $reviewIds = @(@($idA, $idB) | Where-Object { $_ })
    if (($isSingle -and $reviewIds.Count -eq 0) -or (-not $isSingle -and (-not $idA -or -not $idB))) {
        Block-Item -Id 'V0.0' -What 'dispositions' -Evidence 'the required worker reports were not dispatched'; return
    }

    Set-PendingUserInteraction -Pending $true -Phase 'resolving-primary-disposition-asks'
    $pending = Resolve-PendingPrimaryDispositionAsks -AssertionPrefix 'V0' -TimeoutSeconds 60
    if (-not $pending.Resolved) {
        Preserve-ResourcesForPendingTool -AssertionId 'V0.ask-blocked.resources-preserved' -Reason 'a primary disposition Ask was not safely resolved'
        Block-Item -Id 'V0.ask-blocked' -What 'the primary disposition Ask could not be safely resolved' `
            -Evidence ("reason=[$([string](Get-Field $pending 'Reason'))] screen=" + (Protect-Text ([string](Get-Field $pending 'Screen')) 600))
        return
    }

    $reviewTasks = New-Object System.Collections.ArrayList
    $reviewTargets = @(
        @{ id = $idA; marker = 'ACCEPT-A'; label = 'Task A' },
        @{ id = $idB; marker = 'ACCEPT-B'; label = 'Task B' }
    )
    foreach ($item in $reviewTargets) {
        if (-not $item.id) { continue }
        $row = Get-TaskById -Id $item.id
        $fact = Get-Field $row 'fact'
        if ($null -eq (Get-Field $fact 'Report' $null)) {
            Block-Item -Id "V0.report/$($item.id)" -What "task $($item.id) has no final report to review" -Evidence (Protect-Text (Get-Json $fact) 500)
            Preserve-ResourcesForPendingTool -AssertionId "V0.report/$($item.id).resources-preserved" -Reason 'the worker has no final report yet, so its task remains unresolved'
            return
        }
        if ([string]::IsNullOrWhiteSpace([string](Get-Field $fact 'Disposition'))) {
            [void]$reviewTasks.Add([pscustomobject]$item)
        }
    }
    if ($reviewTasks.Count -gt 0) {
        $reviewDisposition = 'Accepted the reported result.'
        foreach ($reviewTask in $reviewTasks) {
            # The primary has no Ask tool: the user decides in orch_review's own dialog.
            $prompt = 'ACCEPT-REVIEW. Review this one worker result only: ' + $reviewTask.marker +
                ' taskId "' + $reviewTask.id + '". Call orch_review exactly once for this exact taskId with outcome "integrated" and disposition "' +
                $reviewDisposition + '". The user confirms the dialog. Do not review any other task and do not merge or clean up either checkout. Reply reviewed after the tool result.'
            $result = Invoke-PrimaryPromptNoWaitAfterDispositionAsks -PaneId $script:PrimaryPane -Text $prompt -TimeoutSeconds 300 -AssertionPrefix "V1.$($reviewTask.marker)"
            Assert-True -Id "V1.submit/$($reviewTask.marker)" -What "the primary accepted the review prompt for $($reviewTask.label)" -Condition ($result.Submitted -and ($result.ExitCode -eq 0 -or $result.Blocked)) `
                -Command $result.CommandLine -Evidence ("code=[$($result.Code)] blocked=[$($result.Blocked)] submitted=[$($result.Submitted)] stderr=[" + (Protect-Text $result.Stderr 300) + "] screen=[" + (Protect-Text $result.Screen 500) + "]")
            if (-not $result.Submitted) {
                Block-Item -Id "V1.submit-blocked/$($reviewTask.marker)" -What "the review prompt for $($reviewTask.label) was not submitted" -Evidence (Protect-Text $result.Screen 500)
                Preserve-ResourcesForPendingTool -AssertionId "V1.submit-blocked/$($reviewTask.marker).resources-preserved" -Reason 'a review dialog may still be open on the primary'
                return
            }
            $confirmed = Confirm-PrimaryReviewModal -PaneId $script:PrimaryPane -LaunchPaneId ([string](Get-Field $script:Project 'LaunchPaneID')) `
                -TaskId $reviewTask.id -Disposition $reviewDisposition
            Assert-True -Id "V1.confirm/$($reviewTask.marker)" -What "the acceptance user confirmed the exact review dialog for $($reviewTask.label)" -Condition $confirmed -TaskId $reviewTask.id
            if (-not $confirmed) {
                Preserve-ResourcesForPendingTool -AssertionId "V1.confirm/$($reviewTask.marker).resources-preserved" -Reason 'the review dialog may still be open on the primary'
                Block-Item -Id "V1.confirm-blocked/$($reviewTask.marker)" -What "the review dialog for $($reviewTask.label) was not safely confirmed" `
                    -Evidence (Protect-Text (Get-PaneScreen -PaneId $script:PrimaryPane -Source 'visible' -Lines 100) 600)
                return
            }
        }
        Add-Assertion -Id 'V1.1' -What 'each outstanding report was decided in its own confirmed review dialog' -Status 'PASS'
    } else {
        Add-Assertion -Id 'V1.1' -What 'all report dispositions were already recorded through confirmed review dialogs' -Status 'PASS'
    }


    $done = Wait-Until -TimeoutSeconds 300 -IntervalSeconds 8 -What 'dispositions recorded' -Probe {
        $rows = @()
        foreach ($id in $reviewIds) {
            $row = Get-TaskById -Id $id
            if (-not $row -or [string]::IsNullOrWhiteSpace([string](Get-Field (Get-Field $row 'fact') 'Disposition'))) { return $null }
            $rows += $row
        }
        return $rows
    }
    Assert-True -Id 'V1.2' -What 'each task carries its recorded disposition' -Condition $done.Ok `
        -Evidence ("after {0:N0}s tasks={1}" -f $done.Elapsed, $reviewIds.Count)
    if ($done.Ok) {
        Set-PendingUserInteraction -Pending $false -Phase 'dispositions-recorded'
    } else {
        Preserve-ResourcesForPendingTool -AssertionId 'V1.2.resources-preserved' -Reason 'one or more review dispositions were not recorded'
    }
    foreach ($id in $reviewIds) {
        $row = Get-TaskById -Id $id
        $fact = Get-Field $row 'fact'
        Assert-Equal -Id "V2.$id-stage" -What "task $id is Done only because a disposition was recorded" -Expected 'done' -Actual (Get-TaskStage -Fact $fact) -TaskId $id
        Assert-True -Id "V2.$id-kept" -What "task $id keeps its report, branch and checkout provenance" `
            -Condition (($null -ne (Get-Field $fact 'Report' $null)) -and ([string](Get-Field $fact 'Branch') -ne '') -and ([string](Get-Field $fact 'WorktreePath') -ne '')) -TaskId $id
    }

    $branches = (Invoke-Git -Arguments @('branch', '--format=%(refname:short)') -Directory $script:RepoPath).Stdout
    foreach ($id in $reviewIds) {
        $fact = Get-Field (Get-TaskById -Id $id) 'fact'
        $branch = [string](Get-Field $fact 'Branch')
        Assert-Match -Id "V3.$id-branch" -What "branch $branch is preserved after the review" -Pattern ([regex]::Escape($branch)) -Actual $branches -TaskId $id
        Assert-True -Id "V3.$id-checkout" -What "the checkout of $id still exists on disk" -Condition (Test-Path -LiteralPath ([string](Get-Field $fact 'WorktreePath'))) -TaskId $id
    }
    $mainHead = (Invoke-Git -Arguments @('rev-parse', 'HEAD') -Directory $script:RepoPath).Stdout.Trim()
    Assert-Equal -Id 'V4.1' -What 'nothing was merged into the base checkout (HEAD unchanged)' -Expected $script:BaseRevision -Actual $mainHead
}

# ---------------------------------------------------------------------------
# Phase 16b: a ticket moves backlog -> todo, is dispatched, reviewed and closed
# ---------------------------------------------------------------------------
#
# Every positive step goes through the real primary and its real tools; the user's
# confirmation of each move and decision is the acceptance script pressing Enter on
# the exact dialog. The one negative step (a worker's `ticket_move`) is a bridge
# replay with the worker's real recorded caller, because the worker has no ticket
# tool at all.

function Wait-ForTickets {
    param(
        [Parameter(Mandatory)][int]$Count,
        [double]$TimeoutSeconds = 300
    )
    return Wait-Until -TimeoutSeconds $TimeoutSeconds -IntervalSeconds 6 -What 'project tickets' -Probe {
        $tickets = Get-ProjectTickets -Key (Get-Field $script:Project 'Key')
        if (@($tickets).Count -lt $Count) { return $null }
        return @($tickets)
    }
}

function Invoke-Tickets {
    Write-Step 'Phase 16b: a ticket move starts a planner, the approved plan permits explicit dispatch, and the ticket is reviewed'
    $key = [string](Get-Field $script:Project 'Key')
    $launch = [string](Get-Field $script:Project 'LaunchPaneID')
    if (-not $script:PrimaryPane -or -not $key) {
        Block-Item -Id 'TK0.0' -What 'the ticket phase' -Evidence 'the primary or the bound project is not available'
        return
    }
    $ticketTitle = "ACCEPT TICKET $($script:RunId): add an exported ticket helper"
    $ticketBody = 'Add an exported ticket() helper to t.js that returns 1. Disposable acceptance ticket.'
    $tasksBefore = @(Get-ProjectTasks -Key $key)
    $ticketsBefore = @(Get-ProjectTickets -Key $key)

    # TK1.1: orch_ticket_create records a backlog ticket.
    $prompt = "$($script:MarkerPrefix)TICKET-CREATE. Call orch_ticket_create exactly once with title ""$ticketTitle"" and body ""$ticketBody"". Reply with the new ticket id only."
    $result = Invoke-PrimaryPrompt -PaneId $script:PrimaryPane -Text $prompt -TimeoutSeconds 300
    Assert-True -Id 'TK1.0' -What 'the real primary accepts the ticket-create prompt' -Condition ($result.ExitCode -eq 0) -Command $result.CommandLine `
        -Evidence ("code=[$($result.Code)] blocked=[$($result.Blocked)] stderr=[" + (Protect-Text $result.Stderr 300) + "] screen=[" + (Protect-Text $result.Screen 400) + "]")
    if ($result.ExitCode -ne 0) {
        Block-Item -Id 'TK1.0b' -What 'the ticket-create prompt was not submitted, so the ticket phase cannot run' -Evidence (Protect-Text ($result.Stdout + $result.Stderr + $result.Screen) 500)
        return
    }
    [void](Wait-ForTickets -Count ($ticketsBefore.Count + 1) -TimeoutSeconds 120)
    $created = @(Get-ProjectTickets -Key $key | Where-Object { [string](Get-Field (Get-Field $_ 'fact') 'Title') -ceq $ticketTitle })
    Assert-Equal -Id 'TK1.1' -What 'orch_ticket_create left exactly one new ticket with the requested title' -Expected 1 -Actual $created.Count
    if ($created.Count -ne 1) {
        Block-Item -Id 'TK1.1b' -What 'the ticket was not created through orch_ticket_create' -Evidence (Protect-Text (Get-PaneScreen -PaneId $script:PrimaryPane -Source 'recent-unwrapped' -Lines 120) 800)
        return
    }
    $ticketFact = Get-Field $created[0] 'fact'
    $ticketId = [string](Get-Field $ticketFact 'ID')
    Assert-Match -Id 'TK1.1c' -What 'the ticket has a generated id' -Pattern '^tkt_[0-9a-f]{12}$' -Actual $ticketId
    Assert-Equal -Id 'TK1.1d' -What 'a new ticket is in the backlog at revision 1 with no decision' -Expected 'backlog|1|' `
        -Actual ('{0}|{1}|{2}' -f (Get-Field $ticketFact 'Column'), (Get-Field $ticketFact 'Revision'), (Get-Field $ticketFact 'Disposition'))
    Assert-Equal -Id 'TK1.1e' -What 'the new ticket stores the requested planner context' -Expected $ticketBody -Actual ([string](Get-Field $ticketFact 'Body'))

    # TK1.2: a dispatch naming a ticket that does not exist is refused and creates nothing.
    $ghost = 'tkt_000000000000'
    $prompt = "$($script:MarkerPrefix)TICKET-GHOST. Call orch_dispatch exactly once with title ""ACCEPT GHOST: must be refused"", brief ""This ticket does not exist, so the dispatch must be refused."", checks [""git log -1 --format=%H""], dependencies [], reservedPaths [""ghost.js""], baseRevision ""$($script:BaseRevision)"" and ticketId ""$ghost"". Do not retry and do not dispatch anything else. Reply with the refusal in one short sentence."
    $result = Invoke-PrimaryPrompt -PaneId $script:PrimaryPane -Text $prompt -TimeoutSeconds 300
    Assert-True -Id 'TK1.2a' -What 'the real primary accepts the ghost-ticket dispatch prompt' -Condition ($result.ExitCode -eq 0) -Command $result.CommandLine `
        -Evidence ("code=[$($result.Code)] blocked=[$($result.Blocked)] stderr=[" + (Protect-Text $result.Stderr 300) + "]")
    $afterGhost = @(Get-ProjectTasks -Key $key)
    Assert-Equal -Id 'TK1.2' -What 'a dispatch naming a ticket that does not exist is refused and creates no task' -Expected $tasksBefore.Count -Actual $afterGhost.Count
    Assert-Equal -Id 'TK1.2b' -What 'no task carries the missing ticket id' -Expected 0 `
        -Actual @($afterGhost | Where-Object { [string](Get-Field (Get-Field $_ 'fact') 'TicketID') -ceq $ghost }).Count

    # TK1.3: the user confirms the move dialog; the ticket is todo and its planner is recorded.
    $prompt = "$($script:MarkerPrefix)TICKET-MOVE. Call orch_ticket_move exactly once for ticketId ""$ticketId"". The user confirms the dialog. Reply moved after the tool result."
    $result = Invoke-PrimaryPromptNoWaitAfterDispositionAsks -PaneId $script:PrimaryPane -Text $prompt -TimeoutSeconds 300 -AssertionPrefix 'TK1.move'
    Assert-True -Id 'TK1.3a' -What 'the real primary accepts the ticket-move prompt' -Condition $result.Submitted -Command $result.CommandLine `
        -Evidence ("code=[$($result.Code)] blocked=[$($result.Blocked)] stderr=[" + (Protect-Text $result.Stderr 300) + "] screen=[" + (Protect-Text $result.Screen 400) + "]")
    if (-not $result.Submitted) {
        Block-Item -Id 'TK1.3b' -What 'the ticket-move prompt was not submitted' -Evidence (Protect-Text $result.Screen 500)
        Preserve-ResourcesForPendingTool -AssertionId 'TK1.3b.resources-preserved' -Reason 'the ticket-move prompt was not submitted, so its confirmation dialog may be open on the primary'
        return
    }
    # The move dialog marks the primary blocked in Herdr while it waits for the user
    # (the extension's herdr:blocked), which is what makes Herdr refuse an `agent
    # prompt` instead of typing into it. Observed before the dialog is confirmed;
    # the confirmation does not depend on it.
    $blockedOnMove = Wait-Until -TimeoutSeconds 120 -IntervalSeconds 2 -What 'the primary blocked on its ticket move dialog' -Probe {
        $live = Get-LiveAgent -Target $script:PrimaryPane
        if (-not $live.Found -or [string](Get-Field $live.Agent 'agent_status') -cne 'blocked') { return $null }
        $screen = Get-PaneScreen -PaneId $script:PrimaryPane -Source 'visible' -Lines 100
        if ((Get-PrimaryConsentDialogTitle -Screen $screen) -cne 'Move ticket to todo') { return $null }
        return 'blocked'
    }
    [void](Assert-True -Id 'TK1.3c-blocked' -What 'Herdr reports the primary blocked while its ticket move dialog is open' -Condition $blockedOnMove.Ok `
        -Evidence (("after {0:N0}s; screen=" -f $blockedOnMove.Elapsed) + (Protect-Text (Get-PaneScreen -PaneId $script:PrimaryPane -Source 'visible' -Lines 60) 400)))
    $moveConfirmed = Confirm-PrimaryTicketModal -Kind 'move' -TicketId $ticketId -TicketTitle $ticketTitle -TicketBody $ticketBody
    Assert-True -Id 'TK1.3c' -What 'the acceptance user confirmed the exact ticket move dialog' -Condition $moveConfirmed
    if (-not $moveConfirmed) {
        Preserve-ResourcesForPendingTool -AssertionId 'TK1.3c.resources-preserved' -Reason 'the ticket move dialog may still be open on the primary'
        Block-Item -Id 'TK1.3d' -What 'the ticket move dialog was not safely confirmed' -Evidence (Protect-Text (Get-PaneScreen -PaneId $script:PrimaryPane -Source 'visible' -Lines 100) 600)
        return
    }
    $moved = Wait-Until -TimeoutSeconds 120 -IntervalSeconds 4 -What 'ticket moved to todo' -Probe {
        $row = Get-TicketById -Id $ticketId
        if ($row -and [string](Get-Field (Get-Field $row 'fact') 'Column') -ceq 'todo') { return $row }
        return $null
    }
    Assert-True -Id 'TK1.3' -What 'after the user confirms the dialog the ticket is in todo' -Condition $moved.Ok
    if (-not $moved.Ok) { return }

    $plannerWait = Wait-Until -TimeoutSeconds 180 -IntervalSeconds 4 -What 'the planner task recorded for this ticket' -Probe {
        $rows = @(Get-ProjectTasks -Key $key | Where-Object {
            [string](Get-Field (Get-Field $_ 'fact') 'TicketID') -ceq $ticketId -and
            [string](Get-Field (Get-Field $_ 'fact') 'Role') -ceq 'plan'
        })
        if ($rows.Count -lt 1) { return $null }
        return $rows
    }
    Assert-True -Id 'TK1.3e' -What 'the confirmed move recorded its planner task' -Condition ($plannerWait.Ok -and @($plannerWait.Value).Count -eq 1) `
        -Evidence ("after {0:N0}s planner tasks={1}" -f $plannerWait.Elapsed, @($plannerWait.Value).Count)
    if (-not $plannerWait.Ok -or @($plannerWait.Value).Count -ne 1) {
        Block-Item -Id 'TK1.3f' -What 'the ticket move did not record exactly one planner task' -Evidence (Protect-Text (Get-Json @(Get-ProjectTasks -Key $key)) 800)
        return
    }
    $plannerRow = @($plannerWait.Value)[0]
    $plannerFact = Get-Field $plannerRow 'fact'
    $plannerId = [string](Get-Field $plannerFact 'ID')
    $script:Tasks = @($script:Tasks | Where-Object { [string](Get-Field $_ 'id') -cne [string](Get-Field $plannerRow 'id') }) + @($plannerRow)
    Assert-Match -Id 'TK1.3g' -What 'the planner task has a generated task id' -Pattern '^orch_[0-9a-f]{12}$' -Actual $plannerId
    Assert-Equal -Id 'TK1.3h' -What 'the task records the planning role and links to this ticket' -Expected "plan|$ticketId" `
        -Actual ("{0}|{1}" -f (Get-Field $plannerFact 'Role'), (Get-Field $plannerFact 'TicketID')) -TaskId $plannerId
    Assert-Equal -Id 'TK1.3i' -What 'the planner title includes the ticket title' -Expected "Plan: $ticketTitle" -Actual ([string](Get-Field $plannerFact 'Title')) -TaskId $plannerId
    $plannerBrief = [string](Get-Field $plannerFact 'Brief')
    Assert-True -Id 'TK1.3j' -What 'the planner brief includes the ticket context and forbids implementation' `
        -Condition ($plannerBrief.Contains($ticketTitle) -and $plannerBrief.Contains($ticketBody) -and $plannerBrief.ToLowerInvariant().Contains('do not edit')) -TaskId $plannerId
    Assert-Equal -Id 'TK1.3k' -What 'planning has no reserved implementation paths' -Expected 0 -Actual @((Get-Field $plannerFact 'ReservedPaths' @())).Count -TaskId $plannerId
    Assert-True -Id 'TK1.3l' -What 'the planner follows the ordinary durable dispatch phases' `
        -Condition ((Get-Field $plannerFact 'Phase') -in @('intent', 'worktree_created', 'agent_started', 'prompt_submitted')) -TaskId $plannerId `
        -Evidence ([string](Get-Field $plannerFact 'Phase'))

    # TK1.4: the ordinary planner reports a plan; the primary reviews it, but
    # accepting it does not dispatch implementation work.
    $planReported = Wait-Until -TimeoutSeconds 600 -IntervalSeconds 10 -What 'the planner final report' -Probe {
        $fact = Get-Field (Get-TaskById -Id $plannerId) 'fact'
        $report = Get-Field $fact 'Report' $null
        if ($report -and [string](Get-Field $report 'Kind') -ceq 'result') { return $fact }
        return $null
    }
    Assert-True -Id 'TK1.4a' -What 'the planner returned its plan as a normal final report' -Condition $planReported.Ok -TaskId $plannerId
    if (-not $planReported.Ok) {
        $latestPlanner = Get-TaskById -Id $plannerId
        if ($latestPlanner) {
            $script:Tasks = @($script:Tasks | Where-Object { [string](Get-Field $_ 'id') -cne [string](Get-Field $latestPlanner 'id') }) + @($latestPlanner)
        }
        Block-Item -Id 'TK1.4b' -What 'the planner did not report, so plan review and explicit dispatch cannot run' `
            -Evidence (Protect-Text (Get-Json (Get-Field (Get-TaskById -Id $plannerId) 'fact')) 600)
        return
    }
    # The planner plans from a read-only session: its transcript may hold only
    # read-only tools and its own report tool.
    $plannerTranscript = [string](Get-Field (Get-ReadySession -LaunchPaneId ([string](Get-Field $planReported.Value 'LaunchPaneID'))) 'omp_session_file')
    $plannerTools = Test-OmpToolCallsWithin -Path $plannerTranscript -Allowed (@($script:ReadOnlyToolNames) + @($script:WorkerToolNames))
    Assert-True -Id 'TK1.4k' -What 'the planner called only read-only tools and orch_report' -Condition $plannerTools.Ok -TaskId $plannerId `
        -Evidence ("transcript=[$plannerTranscript] tools=[" + (@($plannerTools.Names) -join ', ') + "] outside=[" + (@($plannerTools.Outside) -join ', ') + "]")
    $planPreview = Invoke-BridgeAs -LaunchPaneId $launch -Method 'ticket_move_preview' -Params @{ ticketId = $ticketId }
    Assert-True -Id 'TK1.4b-stage' -What 'the ticket preview derives plan_ready from the planner report' -Condition $planPreview.Ok -TaskId $plannerId `
        -Evidence (Protect-Text (Get-Json $planPreview.Result) 400)
    if ($planPreview.Ok) {
        Assert-Equal -Id 'TK1.4b-stage-value' -What 'the planner report projects to plan_ready before review' -Expected 'plan_ready' `
            -Actual ([string](Get-Field $planPreview.Result 'stage')) -TaskId $plannerId
    }
    $plannerRow = Get-TaskById -Id $plannerId
    $plannerFact = Get-Field $plannerRow 'fact'
    $plannerPath = [string](Get-Field $plannerFact 'WorktreePath')
    Assert-NonEmpty -Id 'TK1.4c-path' -What 'the planner has a recorded checkout' -Value $plannerPath -TaskId $plannerId
    $plannerStatusResult = if ($plannerPath) { Invoke-Git -Arguments @('status', '--porcelain') -Directory $plannerPath } else { $null }
    $plannerStatusExit = if ($plannerStatusResult) { [int]$plannerStatusResult.ExitCode } else { -1 }
    $plannerStatus = if ($plannerStatusResult) { $plannerStatusResult.Stdout.Trim() } else { '<missing planner checkout>' }
    Assert-Equal -Id 'TK1.4c-exit' -What 'git status successfully checked the planner checkout' -Expected 0 -Actual $plannerStatusExit -TaskId $plannerId
    Assert-Equal -Id 'TK1.4c' -What 'the planning-only worker left its checkout unchanged' -Expected '' -Actual $plannerStatus -TaskId $plannerId
    if ($plannerStatusExit -ne 0 -or $plannerStatus -ne '') { return }
    $script:Tasks = @($script:Tasks | Where-Object { [string](Get-Field $_ 'id') -cne [string](Get-Field $plannerRow 'id') }) + @($plannerRow)
    $linkedBeforePlanReview = @(Get-ProjectTasks -Key $key | Where-Object { [string](Get-Field (Get-Field $_ 'fact') 'TicketID') -ceq $ticketId })
    Assert-Equal -Id 'TK1.4d' -What 'the planner is the only task linked before plan approval' -Expected 1 -Actual $linkedBeforePlanReview.Count

    $plannerDisposition = 'Accepted the plan; implementation still requires an explicit dispatch.'
    $prompt = "$($script:MarkerPrefix)TICKET-PLAN-REVIEW. Call orch_review exactly once for taskId ""$plannerId"" with outcome ""integrated"" and disposition ""$plannerDisposition"". The user confirms the dialog. Reply reviewed after the tool result."
    $result = Invoke-PrimaryPromptNoWaitAfterDispositionAsks -PaneId $script:PrimaryPane -Text $prompt -TimeoutSeconds 300 -AssertionPrefix 'TK1.planreview'
    Assert-True -Id 'TK1.4e' -What 'the primary submitted the planner review prompt' -Condition $result.Submitted -Command $result.CommandLine `
        -Evidence ("code=[$($result.Code)] stderr=[" + (Protect-Text $result.Stderr 300) + "] screen=[" + (Protect-Text $result.Screen 400) + "]")
    if (-not $result.Submitted) {
        Preserve-ResourcesForPendingTool -AssertionId 'TK1.4e.resources-preserved' -Reason 'the planner review prompt may still be open on the primary'
        Block-Item -Id 'TK1.4f' -What 'the planner review prompt was not submitted' -Evidence (Protect-Text $result.Screen 500)
        return
    }
    $plannerConfirmed = Confirm-PrimaryReviewModal -PaneId $script:PrimaryPane -LaunchPaneId $launch -TaskId $plannerId -Disposition $plannerDisposition
    Assert-True -Id 'TK1.4g' -What 'the acceptance user confirmed the exact planner review dialog' -Condition $plannerConfirmed -TaskId $plannerId
    if (-not $plannerConfirmed) {
        Preserve-ResourcesForPendingTool -AssertionId 'TK1.4g.resources-preserved' -Reason 'the planner review dialog may still be open on the primary'
        Block-Item -Id 'TK1.4h' -What 'the planner review dialog was not safely confirmed' -Evidence (Protect-Text (Get-PaneScreen -PaneId $script:PrimaryPane -Source 'visible' -Lines 100) 600)
        return
    }
    $plannerAccepted = Wait-Until -TimeoutSeconds 180 -IntervalSeconds 4 -What 'the accepted planner disposition' -Probe {
        $fact = Get-Field (Get-TaskById -Id $plannerId) 'fact'
        if ([string](Get-Field $fact 'Outcome') -ceq 'integrated' -and [string](Get-Field $fact 'Disposition') -ceq $plannerDisposition) { return $fact }
        return $null
    }
    Assert-True -Id 'TK1.4i' -What 'the user disposition accepts the planner report' -Condition $plannerAccepted.Ok -TaskId $plannerId
    if (-not $plannerAccepted.Ok) { return }
    $afterPlanReview = @(Get-ProjectTasks -Key $key | Where-Object { [string](Get-Field (Get-Field $_ 'fact') 'TicketID') -ceq $ticketId })
    Assert-Equal -Id 'TK1.4j' -What 'accepting a plan does not auto-dispatch implementation work' -Expected 1 -Actual $afterPlanReview.Count
    $acceptedPlanPreview = Invoke-BridgeAs -LaunchPaneId $launch -Method 'ticket_move_preview' -Params @{ ticketId = $ticketId }
    Assert-True -Id 'TK1.4j-stage' -What 'the accepted planner leaves the ticket ready for explicit implementation dispatch' `
        -Condition $acceptedPlanPreview.Ok -TaskId $plannerId -Evidence (Protect-Text (Get-Json $acceptedPlanPreview.Result) 400)
    if ($acceptedPlanPreview.Ok) {
        Assert-Equal -Id 'TK1.4j-stage-value' -What 'a closed accepted planner with no implementation task projects to todo' `
            -Expected 'todo' -Actual ([string](Get-Field $acceptedPlanPreview.Result 'stage')) -TaskId $plannerId
    }
    if (-not $acceptedPlanPreview.Ok -or [string](Get-Field $acceptedPlanPreview.Result 'stage') -cne 'todo') { return }

    # TK1.5: only an explicit orch_dispatch creates the implementation worker.
    $brief = @"
$($script:MarkerPrefix)TICKET-WORKER. You are in your own Git worktree of a disposable test repository. Do exactly this:
1. Create t.js containing ``function ticket() { return 1; }`` and ``module.exports = { ticket };``.
2. Run ``git add t.js``, ``git commit -m "ACCEPT ticket change"``, and ``git rev-parse HEAD``.
3. Immediately call orch_report with kind "result", summary "ACCEPT-TICKET: t.js exports ticket", branch from ``git branch --show-current``, evidence ["commit <full SHA from git rev-parse HEAD>"], checks [{"name": "git log -1 --format=%H", "outcome": "passed"}], and risk "ACCEPT-TICKET-RISK: none".
Do not ask a question, wait for another message, modify other files, merge, or clean up the checkout.
"@
    $prompt = @"
$($script:MarkerPrefix)TICKET-DISPATCH. Call orch_dispatch exactly once with title "ACCEPT TICKET: helper in t.js", ticketId "$ticketId", the following brief unchanged, checks ["git log -1 --format=%H"], dependencies [], reservedPaths ["t.js"], and baseRevision "$($script:BaseRevision)". Do no implementation work yourself. Reply with the new task id only.
BRIEF: $brief
"@
    $result = Invoke-PrimaryPrompt -PaneId $script:PrimaryPane -Text $prompt -TimeoutSeconds 420
    Assert-True -Id 'TK1.5a' -What 'the primary accepts the explicit implementation dispatch prompt' -Condition ($result.ExitCode -eq 0) -Command $result.CommandLine `
        -Evidence ("code=[$($result.Code)] blocked=[$($result.Blocked)] stderr=[" + (Protect-Text $result.Stderr 300) + "]")
    if ($result.ExitCode -ne 0) {
        Block-Item -Id 'TK1.5b' -What 'the implementation dispatch prompt was not submitted' -Evidence (Protect-Text ($result.Stdout + $result.Stderr + $result.Screen) 500)
        return
    }
    $linked = Wait-Until -TimeoutSeconds 300 -IntervalSeconds 8 -What 'the implementation task linked to the ticket' -Probe {
        $rows = @(Get-ProjectTasks -Key $key | Where-Object {
            [string](Get-Field (Get-Field $_ 'fact') 'TicketID') -ceq $ticketId -and
            [string](Get-Field (Get-Field $_ 'fact') 'Role') -cne 'plan'
        })
        if ($rows.Count -lt 1) { return $null }
        return $rows
    }
    Assert-True -Id 'TK1.5c' -What 'explicit dispatch created one ordinary implementation task linked to the ticket' `
        -Condition ($linked.Ok -and @($linked.Value).Count -eq 1) -Evidence ("after {0:N0}s implementation tasks={1}" -f $linked.Elapsed, @($linked.Value).Count)
    if (-not $linked.Ok -or @($linked.Value).Count -ne 1) {
        Block-Item -Id 'TK1.5d' -What 'no linked implementation worker task was created' -Evidence (Protect-Text (Get-PaneScreen -PaneId $script:PrimaryPane -Source 'recent-unwrapped' -Lines 120) 800)
        return
    }
    $workerRow = @($linked.Value)[0]
    $workerFact = Get-Field $workerRow 'fact'
    $workerId = [string](Get-Field $workerFact 'ID')
    $script:Tasks = @($script:Tasks | Where-Object { [string](Get-Field $_ 'id') -cne [string](Get-Field $workerRow 'id') }) + @($workerRow)
    Assert-Match -Id 'TK1.5e' -What 'the implementation task has a generated task id' -Pattern '^orch_[0-9a-f]{12}$' -Actual $workerId
    Assert-Equal -Id 'TK1.5f' -What 'the implementation task remains an ordinary task linked to this ticket' -Expected "|$ticketId" `
        -Actual ("{0}|{1}" -f [string](Get-Field $workerFact 'Role'), [string](Get-Field $workerFact 'TicketID')) -TaskId $workerId
    Assert-Equal -Id 'TK1.5g' -What 'the explicit dispatch left exactly the planner and implementation task' -Expected ($tasksBefore.Count + 2) -Actual @(Get-ProjectTasks -Key $key).Count
    $live = Get-LiveAgent -Target ([string](Get-Field $workerFact 'PublicPaneID'))
    Assert-True -Id 'TK1.5h' -What 'Herdr reports a live implementation worker on the ticket task pane' -Condition $live.Found -TaskId $workerId -Evidence (Protect-Text (Get-Json $live.Agent) 300)

    # TK1.6: the explicit implementation worker reports and is reviewed separately.
    $reported = Wait-Until -TimeoutSeconds 600 -IntervalSeconds 10 -What 'the ticket implementation worker final report' -Probe {
        $fact = Get-Field (Get-TaskById -Id $workerId) 'fact'
        $report = Get-Field $fact 'Report' $null
        if ($report -and [string](Get-Field $report 'Kind') -eq 'result') { return $fact }
        return $null
    }
    Assert-True -Id 'TK1.6a' -What 'the implementation worker recorded its final result report' -Condition $reported.Ok -TaskId $workerId
    if (-not $reported.Ok) {
        Block-Item -Id 'TK1.6b' -What 'the implementation worker never reported, so its review and the ticket decision cannot run' -Evidence (Protect-Text (Get-Json (Get-Field (Get-TaskById -Id $workerId) 'fact')) 500)
        return
    }
    $implementationReportView = Invoke-BridgeAs -LaunchPaneId $launch -Method 'ticket_move_preview' -Params @{ ticketId = $ticketId }
    Assert-True -Id 'TK1.6c' -What 'the open implementation task keeps the ticket in progress' -Condition $implementationReportView.Ok -TaskId $workerId
    if ($implementationReportView.Ok) {
        Assert-Equal -Id 'TK1.6c-stage' -What 'an implementation report is not a ticket completion decision' -Expected 'in_progress' `
            -Actual ([string](Get-Field $implementationReportView.Result 'stage')) -TaskId $workerId
    }
    $taskDisposition = 'Accepted the reported implementation.'
    $prompt = "$($script:MarkerPrefix)TICKET-TASK-REVIEW. Call orch_review exactly once for taskId ""$workerId"" with outcome ""integrated"" and disposition ""$taskDisposition"". The user confirms the dialog. Reply reviewed after the tool result."
    $result = Invoke-PrimaryPromptNoWaitAfterDispositionAsks -PaneId $script:PrimaryPane -Text $prompt -TimeoutSeconds 300 -AssertionPrefix 'TK1.impl-review'
    Assert-True -Id 'TK1.6d' -What 'the real primary accepts the implementation review prompt' -Condition $result.Submitted -Command $result.CommandLine `
        -Evidence ("code=[$($result.Code)] stderr=[" + (Protect-Text $result.Stderr 300) + "] screen=[" + (Protect-Text $result.Screen 400) + "]")
    if (-not $result.Submitted) {
        Block-Item -Id 'TK1.6e' -What 'the implementation review prompt was not submitted' -Evidence (Protect-Text $result.Screen 500)
        Preserve-ResourcesForPendingTool -AssertionId 'TK1.6e.resources-preserved' -Reason 'the implementation review prompt may still be open on the primary'
        return
    }
    $taskConfirmed = Confirm-PrimaryReviewModal -PaneId $script:PrimaryPane -LaunchPaneId $launch -TaskId $workerId -Disposition $taskDisposition
    Assert-True -Id 'TK1.6f' -What 'the acceptance user confirmed the exact implementation review dialog' -Condition $taskConfirmed -TaskId $workerId
    if (-not $taskConfirmed) {
        Preserve-ResourcesForPendingTool -AssertionId 'TK1.6f.resources-preserved' -Reason 'the implementation review dialog may still be open on the primary'
        Block-Item -Id 'TK1.6g' -What 'the implementation review dialog was not safely confirmed' -Evidence (Protect-Text (Get-PaneScreen -PaneId $script:PrimaryPane -Source 'visible' -Lines 100) 600)
        return
    }
    $taskClosed = Wait-Until -TimeoutSeconds 180 -IntervalSeconds 4 -What 'the implementation worker disposition' -Probe {
        $fact = Get-Field (Get-TaskById -Id $workerId) 'fact'
        if (-not [string]::IsNullOrWhiteSpace([string](Get-Field $fact 'Disposition'))) { return $fact }
        return $null
    }
    Assert-True -Id 'TK1.6h' -What 'the implementation task carries its recorded disposition' -Condition $taskClosed.Ok -TaskId $workerId
    if (-not $taskClosed.Ok) { return }

    $readyPreview = Invoke-BridgeAs -LaunchPaneId $launch -Method 'ticket_move_preview' -Params @{ ticketId = $ticketId }
    Assert-True -Id 'TK1.6i' -What 'the closed plan and implementation tasks project the ticket to ready_for_review' -Condition $readyPreview.Ok -TaskId $workerId
    if ($readyPreview.Ok) {
        Assert-Equal -Id 'TK1.6i-stage' -What 'all linked implementation tasks are closed before ticket review' -Expected 'ready_for_review' `
            -Actual ([string](Get-Field $readyPreview.Result 'stage')) -TaskId $workerId
    }
    $ticketTaskIds = @((Get-Field $readyPreview.Result 'taskIds' @()) | ForEach-Object { [string]$_ })
    $expectedTicketTaskIds = @($plannerId, $workerId)
    $expectedTicketTaskIds = @($expectedTicketTaskIds | Sort-Object)
    $actualTicketTaskIds = @($ticketTaskIds | Sort-Object)
    Assert-Equal -Id 'TK1.6j' -What 'the ticket preview includes exactly the reviewed planner and implementation tasks' `
        -Expected ($expectedTicketTaskIds -join '|') -Actual ($actualTicketTaskIds -join '|')
    if (-not $readyPreview.Ok -or
        [string](Get-Field $readyPreview.Result 'stage') -cne 'ready_for_review' -or
        ($actualTicketTaskIds -join '|') -cne ($expectedTicketTaskIds -join '|')) { return }

    # TK1.7: ticket closure remains a separate explicit user decision.
    $ticketNote = 'Ticket accepted after its plan and implementation were reviewed.'
    $prompt = "$($script:MarkerPrefix)TICKET-REVIEW. Call orch_ticket_review exactly once for ticketId ""$ticketId"" with outcome ""integrated"" and disposition ""$ticketNote"". The user confirms the dialog. Reply closed after the tool result."
    $result = Invoke-PrimaryPromptNoWaitAfterDispositionAsks -PaneId $script:PrimaryPane -Text $prompt -TimeoutSeconds 300 -AssertionPrefix 'TK1.ticketreview'
    Assert-True -Id 'TK1.7a' -What 'the real primary accepts the ticket review prompt' -Condition $result.Submitted -Command $result.CommandLine `
        -Evidence ("code=[$($result.Code)] stderr=[" + (Protect-Text $result.Stderr 300) + "] screen=[" + (Protect-Text $result.Screen 400) + "]")
    if (-not $result.Submitted) {
        Block-Item -Id 'TK1.7b' -What 'the ticket review prompt was not submitted' -Evidence (Protect-Text $result.Screen 500)
        Preserve-ResourcesForPendingTool -AssertionId 'TK1.7b.resources-preserved' -Reason 'the ticket decision dialog may be open on the primary'
        return
    }
    $ticketConfirmed = Confirm-PrimaryTicketModal -Kind 'review' -TicketId $ticketId -TicketTitle $ticketTitle `
        -Outcome 'integrated' -Disposition $ticketNote -LinkedTaskIds $ticketTaskIds
    Assert-True -Id 'TK1.7c' -What 'the acceptance user confirmed the exact ticket decision dialog with both linked tasks' -Condition $ticketConfirmed
    if (-not $ticketConfirmed) {
        Preserve-ResourcesForPendingTool -AssertionId 'TK1.7c.resources-preserved' -Reason 'the ticket decision dialog may still be open on the primary'
        Block-Item -Id 'TK1.7d' -What 'the ticket decision dialog was not safely confirmed' -Evidence (Protect-Text (Get-PaneScreen -PaneId $script:PrimaryPane -Source 'visible' -Lines 100) 600)
        return
    }
    $decided = Wait-Until -TimeoutSeconds 120 -IntervalSeconds 4 -What 'the ticket outcome' -Probe {
        $row = Get-TicketById -Id $ticketId
        if ($row -and [string](Get-Field (Get-Field $row 'fact') 'Outcome') -ceq 'integrated') { return $row }
        return $null
    }
    Assert-True -Id 'TK1.7e' -What 'after the user confirms the dialog the ticket records its outcome' -Condition $decided.Ok
    if ($decided.Ok) {
        $decidedFact = Get-Field $decided.Value 'fact'
        Assert-Equal -Id 'TK1.7f' -What 'the ticket keeps the user note as its disposition' -Expected $ticketNote -Actual ([string](Get-Field $decidedFact 'Disposition'))
    }

    # TK1.8: a worker session has no ticket authority: its bridge call is forbidden
    # and writes nothing.
    $revisionBefore = [long](Get-Field (Get-Field (Get-TicketById -Id $ticketId) 'fact') 'Revision' 0)
    $forbidden = Invoke-BridgeAs -LaunchPaneId ([string](Get-Field $workerFact 'LaunchPaneID')) -Method 'ticket_move' `
        -Params @{ ticketId = $ticketId; digest = 'not-the-digest' }
    Assert-True -Id 'TK1.8' -What 'an implementation worker cannot move a ticket: the bridge answers forbidden' `
        -Condition ($forbidden.Envelope -and -not $forbidden.Ok -and ([string](Get-Field $forbidden.Error 'code') -ceq 'forbidden')) `
        -TaskId $workerId -Evidence ("envelope=[$($forbidden.Envelope)] ok=[$($forbidden.Ok)] error=[" + (Protect-Text (Get-Json $forbidden.Error) 300) + "]")
    $revisionAfter = [long](Get-Field (Get-Field (Get-TicketById -Id $ticketId) 'fact') 'Revision' 0)
    Assert-Equal -Id 'TK1.8b' -What 'the refused worker call wrote nothing to the ticket' -Expected $revisionBefore -Actual $revisionAfter

    # TK1.9: the primary coordinated the run from a read-only session: its
    # transcript may hold only read-only tools and the primary's own tools.
    $primaryTranscript = [string](Get-Field (Get-ReadySession -LaunchPaneId $launch) 'omp_session_file')
    $primaryTools = Test-OmpToolCallsWithin -Path $primaryTranscript -Allowed (@($script:ReadOnlyToolNames) + @($script:PrimaryToolNames))
    Assert-True -Id 'TK1.9' -What 'the primary called only read-only tools and its own orchestrator tools' -Condition $primaryTools.Ok `
        -Evidence ("transcript=[$primaryTranscript] tools=[" + (@($primaryTools.Names) -join ', ') + "] outside=[" + (@($primaryTools.Outside) -join ', ') + "]")
}

# ---------------------------------------------------------------------------
# Phase 17: cleanup refusals (dirty, then live worker) on the moved task
# ---------------------------------------------------------------------------

function Invoke-CleanupRefusals {
    Write-Step 'Phase 17: a dirty checkout and a live worker both refuse cleanup'
    if (-not $script:TaskB) { Block-Item -Id 'C0.0' -What 'cleanup refusals' -Evidence 'task B was never dispatched'; return }
    $fact = Get-TaskById -Id ([string](Get-Field $script:TaskB 'ID'))
    $idB = [string](Get-Field $script:TaskB 'ID')
    $script:TaskB = Get-Field $fact 'fact'
    if (-not (Test-TaskIsReviewedDone -Fact $script:TaskB)) {
        Block-Item -Id 'C0.not-reviewed' -What 'cleanup refusal checks require the worker report to be reviewed first' `
            -Evidence 'the task has no recorded final disposition; no cleanup-preview/removal probes were sent'
        return
    }
    $path = [string](Get-Field $script:TaskB 'WorktreePath')
    $branch = [string](Get-Field $script:TaskB 'Branch')
    $primaryLaunch = [string](Get-Field $script:Project 'LaunchPaneID')

    $status = (Invoke-Git -Arguments @('status', '--porcelain') -Directory $path).Stdout.Trim()
    Assert-True -Id 'C1.1' -What 'the moved worker checkout is dirty before the cleanup tests' -Condition ($status -ne '') -TaskId $idB -Evidence (Protect-Text $status 300)

    $preview = Invoke-BridgeAs -LaunchPaneId $primaryLaunch -Method 'cleanup_preview' -Params @{ taskId = $idB }
    Assert-True -Id 'C2.0' -What 'cleanup_preview answered with a bridge envelope (ok or a coded error)' -Condition $preview.Envelope -TaskId $idB `
        -Evidence (Protect-Text $preview.Raw 400)
    Assert-True -Id 'C2.1' -What 'cleanup_preview answers for the reviewed done task' -Condition $preview.Ok -TaskId $idB -Evidence (Protect-Text (Get-Json $preview.Error) 300)
    if ($preview.Ok) {
        Assert-JsonTrue -Id 'C2.2' -What 'the preview recognises the task as reviewed and decided (a real boolean)' -Object $preview.Result -Name 'reviewed' -TaskId $idB
        Assert-JsonFalse -Id 'C2.3' -What 'the preview reports the checkout as not clean (a real boolean)' -Object $preview.Result -Name 'clean' -TaskId $idB
        Assert-JsonFalse -Id 'C2.4' -What 'the preview refuses removal (a real boolean)' -Object $preview.Result -Name 'removable' -TaskId $idB
        Assert-True -Id 'C2.5' -What 'the preview names the exact recorded checkout path' -Condition (Test-SamePath -A (Get-Field $preview.Result 'path') -B $path) -TaskId $idB
        Assert-Equal -Id 'C2.6' -What 'the preview names the exact recorded branch' -Expected $branch -Actual ([string](Get-Field $preview.Result 'branch')) -TaskId $idB
        Assert-True -Id 'C2.7' -What 'the preview resolves a workspace from the recorded provenance (not the closed source workspace)' `
            -Condition (([string](Get-Field $preview.Result 'workspaceId')) -in @([string](Get-Field $script:TaskB 'CurrentWorkspaceID'), [string](Get-Field $script:TaskB 'WorktreeWorkspaceID'))) -TaskId $idB `
            -Evidence "workspaceId=[$(Get-Field $preview.Result 'workspaceId')] current=[$(Get-Field $script:TaskB 'CurrentWorkspaceID')] source=[$(Get-Field $script:TaskB 'WorktreeWorkspaceID')]"
        Assert-Match -Id 'C2.8' -What 'the preview explains the dirt' -Pattern 'uncommitted or untracked changes' -Actual ([string](Get-Field $preview.Result 'detail')) -TaskId $idB
    }

    $cleanup = Invoke-BridgeAs -LaunchPaneId $primaryLaunch -Method 'cleanup' -Params @{ taskId = $idB }
    Assert-True -Id 'C3.1' -What 'cleanup refuses a dirty checkout' -Condition (-not $cleanup.Ok) -TaskId $idB -Evidence (Protect-Text (Get-Json $cleanup.Error) 400)
    Assert-Equal -Id 'C3.2' -What 'the refusal is classified as checkout_dirty' -Expected 'checkout_dirty' -Actual ([string](Get-Field $cleanup.Error 'code')) -TaskId $idB
    Assert-Match -Id 'C3.3' -What 'the refusal names the checkout and that it still holds work' -Pattern 'still holds work' -Actual ([string](Get-Field $cleanup.Error 'message')) -TaskId $idB
    Assert-True -Id 'C3.4' -What 'the refused cleanup removed nothing' -Condition (Test-Path -LiteralPath $path) -TaskId $idB

    # Explicit operator action on this run's own temporary checkout only: commit
    # the leftover so the next refusal exercises the live-worker rule instead.
    $commit = Invoke-Git -Arguments @('-c', 'user.email=acceptance@localhost', '-c', 'user.name=acceptance', 'commit', '-q', '-a', '-m', 'ACCEPT-B: commit leftover for the cleanup-refusal check', '--no-gpg-sign') -Directory $path
    Assert-Equal -Id 'C4.1' -What 'the temp checkout can be made clean by an explicit commit' -Expected 0 -Actual $commit.ExitCode -TaskId $idB -Evidence (Protect-Text $commit.Stderr 300)
    $status2 = (Invoke-Git -Arguments @('status', '--porcelain') -Directory $path).Stdout.Trim()
    Assert-Equal -Id 'C4.2' -What 'the checkout is clean now' -Expected '' -Actual $status2 -TaskId $idB

    $cleanup2 = Invoke-BridgeAs -LaunchPaneId $primaryLaunch -Method 'cleanup' -Params @{ taskId = $idB }
    Assert-True -Id 'C5.1' -What 'cleanup refuses while the worker agent is still live' -Condition (-not $cleanup2.Ok) -TaskId $idB -Evidence (Protect-Text (Get-Json $cleanup2.Error) 400)
    Assert-Equal -Id 'C5.2' -What 'the refusal is classified as worker_live' -Expected 'worker_live' -Actual ([string](Get-Field $cleanup2.Error 'code')) -TaskId $idB
    Assert-True -Id 'C5.3' -What 'the live-worker refusal removed nothing' -Condition (Test-Path -LiteralPath $path) -TaskId $idB

    $pane = [string](Get-Field $script:TaskB 'PublicPaneID')
    $close = Invoke-Herdr -Arguments @('pane', 'close', $pane) -TimeoutSeconds 60
    Assert-Equal -Id 'C6.1' -What 'the worker pane this run created closes' -Expected 0 -Actual $close.ExitCode -Evidence (Protect-Text $close.Stderr 300)
    [void]$script:CreatedPanes.Remove($pane)
    [void]$script:CreatedWorkspaces.Remove([string](Get-Field $script:TaskB 'CurrentWorkspaceID'))
    Save-AcceptanceRunManifest -Phase 'worker-workspace-closed' -Status 'active'
    $dead = Wait-Until -TimeoutSeconds 120 -IntervalSeconds 5 -What 'worker gone' -Probe {
        $live = Get-LiveAgent -Target $pane
        if (-not $live.Found) { return $true }
        if ([string]::IsNullOrWhiteSpace([string](Get-Field $live.Agent 'agent'))) { return $true }
        return $false
    }
    Assert-True -Id 'C6.2' -What 'no agent is left on the closed pane' -Condition $dead.Ok -TaskId $idB

    $finalCleanupPreview = Invoke-BridgeAs -LaunchPaneId $primaryLaunch -Method 'cleanup_preview' -Params @{ taskId = $idB }
    $finalCleanupDetail = [string](Get-Field $finalCleanupPreview.Result 'detail')
    $cleanupReopensByPath = $finalCleanupDetail -match 'no workspace holds it, so it is opened by its recorded path first'
    Assert-True -Id 'C6.3' -What 'the clean closed worktree preview identifies its recorded-path reopen plan' `
        -Condition ($finalCleanupPreview.Ok -and (Test-JsonBool -Object $finalCleanupPreview.Result -Name 'removable') -and $cleanupReopensByPath) `
        -TaskId $idB -Evidence (Protect-Text (Get-Json $finalCleanupPreview.Result) 500)

    # Everything the removal must preserve is captured from a FRESH store read
    # taken here, after the worker pane was closed and observed gone, so both the
    # success and the refusal comparison are exact rather than "still non-empty".
    $preCleanupRow = Get-TaskById -Id $idB
    Assert-True -Id 'C7.0' -What 'the task fact was re-read immediately before the final cleanup call' `
        -Condition ($null -ne $preCleanupRow) -TaskId $idB -Evidence (Protect-Text (Get-Json $preCleanupRow) 500)
    if (-not $preCleanupRow) { Block-Item -Id 'C7.0b' -What 'the moved task could not be re-read before cleanup' -Evidence $idB; return }
    $preCleanup = Get-Field $preCleanupRow 'fact'
    $reportBefore = Get-Json (Get-Field $preCleanup 'Report' $null)
    $dispositionBefore = [string](Get-Field $preCleanup 'Disposition')
    $pathBefore = [string](Get-Field $preCleanup 'WorktreePath')
    $branchBefore = [string](Get-Field $preCleanup 'Branch')
    $cleanupInvariantBefore = Get-TaskProjection -Fact $preCleanup -CleanupInvariant

    $cleanup3 = Invoke-BridgeAs -LaunchPaneId $primaryLaunch -Method 'cleanup' -Params @{ taskId = $idB }
    Assert-True -Id 'C7.0a' -What 'the final cleanup answered with a bridge envelope (ok or a coded error)' -Condition $cleanup3.Envelope -TaskId $idB `
        -Evidence (Protect-Text $cleanup3.Raw 400)
    Add-Assertion -Id 'C7.1' -What 'the plugin’s removal path never asks Herdr to force a removal' -Status 'INFO' `
        -Evidence 'internal/coord/report.go (removeCheckout) calls herdr WorktreeRemove(...,false); internal/herdr/client.go builds `worktree remove --workspace <id>` with no --force; a forced removal is refused by verifyRemoval'

    # Required: cleanup of the moved, now-clean, worker-free checkout must succeed.
    # A refusal here (in particular `not_linked_worktree`) is an acceptance failure,
    # not an observation: the task's worktree provenance must still be enough to
    # remove its own checkout after a move closed the linked source workspace.
    $cleanupCode = [string](Get-Field $cleanup3.Error 'code')
    Assert-True -Id 'C7.2' -What 'cleanup of the moved, now-clean checkout succeeds through its recorded provenance' `
        -Condition $cleanup3.Ok -TaskId $idB `
        -Command "herdr-omp-orchestrator.exe bridge cleanup (taskId=$idB)" `
        -Evidence $(if ($cleanup3.Ok) { Protect-Text (Get-Json $cleanup3.Result) 400 } else { "code=[$cleanupCode] " + (Protect-Text (Get-Json $cleanup3.Error) 500) })
    if (-not $cleanup3.Ok) {
        Assert-Equal -Id 'C7.2b' -What 'the removal of a moved checkout is not refused for a workspace-linking reason' `
            -Expected 'ok' -Actual $cleanupCode -TaskId $idB `
            -Evidence ("the checkout is [$pathBefore] on branch [$branchBefore], the worker is closed and the checkout is clean; a refusal means the plugin could not resolve its own recorded provenance")
    }

    # The removal must report the force flag Herdr actually used. A result that
    # does not carry it cannot prove anything, so its absence is a failure.
    $forcedProperty = $null
    if ($cleanup3.Result) { $forcedProperty = $cleanup3.Result.PSObject.Properties['forced'] }
    Assert-True -Id 'C7.2c' -What 'the cleanup result reports the force flag Herdr actually used' `
        -Condition ($cleanup3.Ok -and ($null -ne $forcedProperty)) -TaskId $idB `
        -Evidence (Protect-Text (Get-Json $cleanup3.Result) 400)
    Assert-True -Id 'C7.2d' -What 'the removal was not forced' `
        -Condition ($cleanup3.Ok -and ((Test-JsonBool -Object $cleanup3.Result -Name 'forced') -eq $false)) -TaskId $idB `
        -Evidence ("forced=[$(Get-Field $cleanup3.Result 'forced' 'absent')]")

    Assert-True -Id 'C7.3' -What 'the branch survives the removal (the plugin never deletes it)' `
        -Condition ((Invoke-Git -Arguments @('branch', '--format=%(refname:short)') -Directory $script:RepoPath).Stdout -match [regex]::Escape($branch)) -TaskId $idB
    Assert-True -Id 'C7.4' -What 'the removal reports the exact recorded checkout as removed' `
        -Condition ($cleanup3.Ok -and ((Test-JsonBool -Object $cleanup3.Result -Name 'removed') -eq $true) -and (Test-SamePath -A (Get-Field $cleanup3.Result 'path') -B $path)) -TaskId $idB `
        -Evidence (Protect-Text (Get-Json $cleanup3.Result) 400)
    Assert-True -Id 'C7.5' -What 'the removed checkout is gone from disk' `
        -Condition ($cleanup3.Ok -and (-not (Test-Path -LiteralPath $path))) -TaskId $idB

    if ($cleanup3.Ok) {
        $after = Get-Field (Get-TaskById -Id $idB) 'fact'
        Assert-True -Id 'C7.6' -What 'the task keeps its branch and path as provenance after removal' `
            -Condition (([string](Get-Field $after 'Branch') -eq $branchBefore) -and (Test-SamePath -A ([string](Get-Field $after 'WorktreePath')) -B $pathBefore)) -TaskId $idB `
            -Evidence ("branch=[$(Get-Field $after 'Branch')] path=[$(Get-Field $after 'WorktreePath')]")
        Assert-Equal -Id 'C7.7' -What 'the removal preserves the report byte-for-byte' `
            -Expected $reportBefore -Actual (Get-Json (Get-Field $after 'Report' $null)) -TaskId $idB
        Assert-Equal -Id 'C7.7b' -What 'the removal preserves the recorded disposition' `
            -Expected $dispositionBefore -Actual ([string](Get-Field $after 'Disposition')) -TaskId $idB
        Assert-True -Id 'C7.7c' -What 'the review is still what makes the task Done after the removal' `
            -Condition ((Get-TaskStage -Fact $after) -eq 'done') -TaskId $idB
        $listingAfter = Get-WorktreeList
        $stillListed = @((Get-Field $listingAfter 'worktrees' @()) | Where-Object { Test-SamePath -A (Get-Field $_ 'path') -B $path })
        Assert-Equal -Id 'C7.8' -What 'Herdr no longer lists the removed checkout' -Expected 0 -Actual $stillListed.Count -TaskId $idB

        # Every live identity must be gone; a stale session or agent name would
        # let a later call resolve a task whose checkout no longer exists.
        foreach ($identity in @('PublicPaneID', 'LaunchPaneID', 'TerminalID', 'CurrentWorkspaceID', 'WorktreeWorkspaceID', 'AgentName', 'OMPSessionID')) {
            Assert-Equal -Id "C7.9.$identity" -What "the removed task records no $identity" `
                -Expected '' -Actual ([string](Get-Field $after $identity)) -TaskId $idB `
                -Evidence (Protect-Text (Get-Json $after) 400)
        }
        Assert-True -Id 'C7.10' -What 'the removed task keeps its branch and checkout path in the store' `
            -Condition (([string](Get-Field $after 'Branch') -ne '') -and ([string](Get-Field $after 'WorktreePath') -ne '')) -TaskId $idB
        Assert-Equal -Id 'C7.11' -What 'the cleanup result names the branch it kept' `
            -Expected $branchBefore -Actual ([string](Get-Field $cleanup3.Result 'branch')) -TaskId $idB
        $cleanupWorkspace = [string](Get-Field $cleanup3.Result 'workspaceId')
        $recordedWorkspaces = @(
            [string](Get-Field $preCleanup 'CurrentWorkspaceID')
            [string](Get-Field $preCleanup 'WorktreeWorkspaceID')
        ) | Where-Object { -not [string]::IsNullOrWhiteSpace($_) }
        $workspaceRecorded = $cleanupWorkspace -and $cleanupWorkspace -in $recordedWorkspaces
        $workspaceReopened = $cleanupWorkspace -and $cleanupReopensByPath -and $cleanupWorkspace -notin $recordedWorkspaces
        Assert-True -Id 'C7.12' -What 'the cleanup result names a recorded or temporary path-reopen workspace' `
            -Condition ($workspaceRecorded -or $workspaceReopened) -TaskId $idB `
            -Evidence ("workspace=[$cleanupWorkspace] current=[$(Get-Field $preCleanup 'CurrentWorkspaceID')] worktree=[$(Get-Field $preCleanup 'WorktreeWorkspaceID')] reopened_by_path=[$cleanupReopensByPath]")
    } else {
        $script:MovedCleanupRefused = $cleanup3.Error
        Assert-True -Id 'C7.6b' -What 'a refused removal left the checkout in place untouched' -Condition (Test-Path -LiteralPath $path) -TaskId $idB
        Assert-True -Id 'C7.7b' -What 'a refused removal left the branch in place' `
            -Condition ((Invoke-Git -Arguments @('branch', '--format=%(refname:short)') -Directory $script:RepoPath).Stdout -match [regex]::Escape($branch)) -TaskId $idB
        # A refusal must leave every cleanup-relevant fact exactly as it was: the
        # eleven fields a removal touches, compared against the fresh pre-call read
        # (never Activity/ActivitySeenAt/Revision, which a lifecycle hook may
        # independently refresh when the pane closes).
        $afterRefusal = Get-Field (Get-TaskById -Id $idB) 'fact'
        $cleanupInvariantAfter = Get-TaskProjection -Fact $afterRefusal -CleanupInvariant
        Assert-Equal -Id 'C7.13' -What 'a refused removal left Branch, WorktreePath, Report, Disposition and all seven live identities byte-identical' `
            -Expected $cleanupInvariantBefore -Actual $cleanupInvariantAfter -TaskId $idB `
            -Evidence ("code=[$cleanupCode] before=[" + (Protect-Text $cleanupInvariantBefore 600) + "] after=[" + (Protect-Text $cleanupInvariantAfter 600) + "]")
        Assert-Equal -Id 'C7.7c' -What 'a refused removal left the report and disposition untouched' `
            -Expected $reportBefore -Actual (Get-Json (Get-Field $afterRefusal 'Report' $null)) -TaskId $idB
        Assert-Equal -Id 'C7.7d' -What 'a refused removal left the disposition untouched' `
            -Expected $dispositionBefore -Actual ([string](Get-Field $afterRefusal 'Disposition')) -TaskId $idB
    }
}

# ---------------------------------------------------------------------------
# Phase 18: a timed-out prompt is ambiguous - inspect before retrying
# ---------------------------------------------------------------------------

function Invoke-AmbiguousPrompt {
    Write-Step 'Phase 18: an ambiguous agent prompt timeout, inspected before any retry'
    # The probe is typed only into a settled primary. `agent prompt` types into
    # whatever the pane shows - Herdr refuses only a blocked agent before any
    # input - and typed into an open selector, its Enter picks the highlighted Yes.
    # So the primary's pending input is settled first, as for every other prompt.
    $ready = Resolve-PendingPrimaryDispositionAsks -AssertionPrefix 'G0' -TimeoutSeconds 120
    if (-not $ready.Resolved) {
        [void](Block-Item -Id 'G0.settled' -What 'the ambiguous prompt needs a settled primary; it is never typed into an open dialog or a running turn' `
            -Evidence ("reason=[$([string](Get-Field $ready 'Reason'))] screen=" + (Protect-Text ([string](Get-Field $ready 'Screen')) 500)))
        return
    }
    [void](Assert-True -Id 'G0.settled' -What 'the primary was settled (idle or done) before the ambiguous prompt' -Condition $true)
    $marker = "ACCEPT-PING-$($script:RunId)"
    $text = "$marker reply with exactly the word pong and nothing else"
    $result = Invoke-Herdr -Arguments @('agent', 'prompt', $script:PrimaryPane, $text, '--wait', '--timeout', '1500') -TimeoutSeconds 60
    $code = ''
    try { $code = [string]((($result.Stderr | ConvertFrom-Json).error).code) } catch { $code = '' }
    if ($code -eq '' -and $result.Stdout) {
        try { $code = [string]((($result.Stdout | ConvertFrom-Json).error).code) } catch { $code = '' }
    }
    Assert-True -Id 'G1.1' -What 'a too-short wait on agent prompt does not report success' -Condition ($result.ExitCode -ne 0) -Command $result.CommandLine -Evidence (Protect-Text ($result.Stdout + $result.Stderr) 500)
    Assert-True -Id 'G1.2' -What 'the failure is classified as an ambiguous timeout, not a delivery failure' `
        -Condition ($code -in @('timeout', 'agent_prompt_stalled', 'agent_prompt_stalled_timeout')) -Command $result.CommandLine -Evidence "code=[$code]"

    # Inspection, in the order the brief requires: live agent state, the terminal,
    # then the durable facts - all before any decision to resubmit.
    $live = Get-LiveAgent -Target $script:PrimaryPane
    Assert-True -Id 'G2.1' -What 'the live agent state was inspected after the timeout' -Condition ($live.Found) -Evidence (Protect-Text (Get-Json $live.Agent) 400)
    $recent = Get-PaneScreen -PaneId $script:PrimaryPane -Source 'recent-unwrapped' -Lines 120
    $occurrences = ([regex]::Matches($recent, [regex]::Escape($marker))).Count
    Assert-True -Id 'G2.2' -What 'the terminal was inspected after the timeout' -Condition ($recent -ne '') -Evidence (Protect-Text $recent 500)
    $projectBefore = Get-Json (Get-Field (Get-ProjectTasks -Key (Get-Field $script:Project 'Key'))[0] 'fact')
    Assert-True -Id 'G2.3' -What 'the durable facts were inspected after the timeout' -Condition ($projectBefore -ne '') -Evidence (Protect-Text $projectBefore 300)
    Assert-Equal -Id 'G2.4' -What 'the ambiguous prompt was submitted exactly once (no blind resubmission)' -Expected 1 -Actual $occurrences -Evidence (Protect-Text $recent 500)
    Add-Assertion -Id 'G2.5' -What 'inspection evidence recorded before any retry' -Status 'INFO' -Evidence ("agent_status={0}; marker occurrences={1}" -f [string](Get-Field $live.Agent 'agent_status'), $occurrences)

    $settled = Wait-Until -TimeoutSeconds 300 -IntervalSeconds 8 -What 'primary settled after the ambiguous prompt' -Probe {
        $now = Get-LiveAgent -Target $script:PrimaryPane
        if (-not $now.Found) { return $null }
        $status = [string](Get-Field $now.Agent 'agent_status')
        if ($status -in @('idle', 'done')) { return $status }
        return $null
    }
    Assert-True -Id 'G3.1' -What 'the primary settled after the ambiguous prompt' -Condition $settled.Ok -Evidence ("status={0}" -f $settled.Value)
    $after = Get-PaneScreen -PaneId $script:PrimaryPane -Source 'recent-unwrapped' -Lines 120
    if ($after -match '(?i)pong') {
        Assert-True -Id 'G3.2' -What 'the prompt had in fact been delivered (the timeout was ambiguous, not a failure)' -Condition $true
    } else {
        Block-Item -Id 'G3.2' -What 'the prompt had in fact been delivered' -Evidence ("no 'pong' answer was visible after the agent settled: " + (Protect-Text $after 500))
    }
}

# ---------------------------------------------------------------------------
# Phase 19: restart the isolated server and prove recovery
# ---------------------------------------------------------------------------

function Get-ProjectFacts {
    $tasks = Get-ProjectTasks -Key (Get-Field $script:Project 'Key')
    $dump = Get-StateDump
    $key = [string](Get-Field $script:Project 'Key')
    $project = @($dump.tables.projects | Where-Object { (Get-Field $_ 'key') -eq $key })
    return [pscustomobject]@{
        Project = if ($project.Count -gt 0) { Get-Field $project[0] 'fact' } else { $null }
        Tasks   = @($tasks | Sort-Object { Get-Field $_ 'id' })
        Inbox   = @($dump.tables.inbox | Where-Object { (Get-Field $_ 'project_key') -eq $key } | Sort-Object { Get-Field $_ 'id' })
    }
}

function Get-IdentityProjection {
    # Herdr may assign a restored process a new pane ID. The restart check below
    # verifies any current pane against the binding's recorded OMP session.
    # Inbox contents are checked separately because delivery recovery may advance
    # a note and startup may add new worker-activity notes.
    param($Facts)
    $parts = New-Object System.Collections.ArrayList
    $p = $Facts.Project
    if ($p) {
        foreach ($field in @('Key', 'AgentName', 'OMPSessionID', 'RepoWorkspaceID')) {
            [void]$parts.Add("project/$field=$([string](Get-Field $p $field))")
        }
    }
    foreach ($row in @($Facts.Tasks)) {
        $f = Get-Field $row 'fact'
        foreach ($field in @('ID', 'ProjectKey', 'Title', 'Branch', 'BaseRevision', 'WorktreePath', 'WorktreeWorkspaceID', 'OMPSessionID', 'Phase', 'Blocker', 'Attention', 'Disposition', 'CurrentWorkspaceID')) {
            [void]$parts.Add("task/$([string](Get-Field $f 'ID'))/$field=$([string](Get-Field $f $field))")
        }
        $report = Get-Field $f 'Report' $null
        [void]$parts.Add("task/$([string](Get-Field $f 'ID'))/Report=$((Get-Json $report))")
    }
    return ($parts -join ';')
}

function Get-DecidedTaskIdentityProjection {
    # Decided tasks are history: startup does not rederive their pane, terminal
    # or agent identity. Compare those durable fields separately from active tasks,
    # whose pane ids may legitimately be rekeyed during reconciliation.
    param($Facts)
    $parts = New-Object System.Collections.ArrayList
    foreach ($row in @($Facts.Tasks)) {
        $fact = Get-Field $row 'fact'
        if ([string]::IsNullOrWhiteSpace([string](Get-Field $fact 'Disposition'))) { continue }
        $taskId = [string](Get-Field $fact 'ID')
        foreach ($field in @('LaunchPaneID', 'PublicPaneID', 'TerminalID', 'AgentName')) {
            [void]$parts.Add("task/$taskId/$field=$([string](Get-Field $fact $field))")
        }
    }
    return (@($parts.ToArray() | Sort-Object -CaseSensitive) -join ';')
}
function Test-InboxRowsPreserved {
    param($Before, $After)
    $afterById = @{}
    foreach ($row in @($After)) {
        $id = [string](Get-Field $row 'id')
        if (-not [string]::IsNullOrWhiteSpace($id)) { $afterById[$id] = $row }
    }
    foreach ($row in @($Before)) {
        $id = [string](Get-Field $row 'id')
        if ([string]::IsNullOrWhiteSpace($id) -or -not $afterById.ContainsKey($id)) { return $false }
        $preserved = $afterById[$id]
        foreach ($field in @('project_key', 'task_id', 'kind', 'summary', 'delivery')) {
            if ([string](Get-Field $row $field) -cne [string](Get-Field $preserved $field)) { return $false }
        }
    }
    return $true
}

function Get-StartupLogEntries {
    param($Logs, [long]$AfterUnixMs)
    $matches = New-Object System.Collections.ArrayList
    foreach ($entry in @($Logs)) {
        $command = @(Get-Field $entry 'command' @())
        $isStartup = ([string](Get-Field $entry 'event') -ieq 'startup') -or
            ($command.Count -gt 0 -and [string]$command[-1] -ieq 'startup')
        $started = [int64](Get-Field $entry 'started_unix_ms' 0)
        if ($isStartup -and $started -gt $AfterUnixMs) { [void]$matches.Add($entry) }
    }
    return @($matches.ToArray())
}

function Test-RestartReconcilesTask {
    <#
        True when startup reconciliation re-derives this task's live identity after
        a restart - the rule of reconciles() in internal/coord/reconcile.go: an
        undecided task in a provisioned phase. A decided task is history: startup
        leaves its pane, terminal and session as recorded, so a restored pane is not
        expected to prove them.
    #>
    param($Fact)
    if ($null -eq $Fact) { return $false }
    if (-not [string]::IsNullOrWhiteSpace([string](Get-Field $Fact 'Disposition'))) { return $false }
    return ([string](Get-Field $Fact 'Phase') -cin @('worktree_created', 'agent_started', 'prompt_submitted'))
}


function Invoke-Restart {
    Write-Step 'Phase 19: restart the isolated Herdr server and prove recovery'
    $before = Get-ProjectFacts
    $projectionBefore = Get-IdentityProjection -Facts $before
    Add-Assertion -Id 'T1.1' -What 'identity projection captured before the restart' -Status 'INFO' -Evidence (Protect-Text $projectionBefore 800)
    $decidedIdentityBefore = Get-DecidedTaskIdentityProjection -Facts $before

    $script:RestartLogWatermarkMs = [DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds()
    $stop = Invoke-Herdr -Arguments @('server', 'stop') -TimeoutSeconds 90
    Assert-Equal -Id 'T2.1' -What 'the isolated server stops on request' -Expected 0 -Actual $stop.ExitCode -Command $stop.CommandLine -Evidence (Protect-Text $stop.Stderr 300)
    $stopped = Wait-Until -TimeoutSeconds 60 -IntervalSeconds 3 -What 'session stopped' -Probe {
        $mine = @(Get-Sessions | Where-Object { (Get-Field $_ 'name') -eq $script:SessionName })
        if ($mine.Count -eq 0) { return $null }
        if (Get-Flag $mine[0] 'running') { return $null }
        return $true
    }
    Assert-True -Id 'T2.2' -What 'the isolated session is not running after the stop' -Condition $stopped.Ok
    $default = @(Get-Sessions | Where-Object { (Get-Field $_ 'name') -eq 'default' })
    Assert-True -Id 'T2.3' -What "the user's default session was not touched by the restart" -Condition ($default.Count -gt 0 -and (Get-Flag $default[0] 'running')) -Evidence (Protect-Text (Get-Json $default) 300)

    [void](Start-RunServer)
    $back = Wait-Until -TimeoutSeconds 90 -IntervalSeconds 3 -What 'session back' -Probe {
        $mine = @(Get-Sessions | Where-Object { (Get-Field $_ 'name') -eq $script:SessionName })
        if ($mine.Count -eq 0) { return $null }
        if (-not (Get-Flag $mine[0] 'running')) { return $null }
        return $true
    }
    Assert-True -Id 'T3.1' -What 'the isolated session comes back up after the restart' -Condition $back.Ok `
        -Evidence ("after {0:N0}s; server log: {1}" -f $back.Elapsed, (Protect-Text (Get-Content -LiteralPath $script:ServerLog -Raw -ErrorAction SilentlyContinue) 300))

    $script:LastStartupLogs = @()
    $startup = Wait-Until -TimeoutSeconds 120 -IntervalSeconds 5 -What 'startup hook' -Probe {
        $json = Get-HerdrJson -Arguments @('plugin', 'log', 'list', '--plugin', $script:PluginId, '--limit', '50')
        $script:LastStartupLogs = @(Get-JsonField $json 'logs' @())
        $entries = @(Get-StartupLogEntries -Logs $script:LastStartupLogs -AfterUnixMs $script:RestartLogWatermarkMs)
        if ($entries.Count -eq 0) { return $null }
        $newest = $entries | Sort-Object { [int64](Get-Field $_ 'started_unix_ms') } | Select-Object -Last 1
        if ((Get-Field $newest 'status') -eq 'running') { return $null }
        return $newest
    }
    $startupEvidence = if ($startup.Ok) {
        Protect-Text (Get-Json $startup.Value) 400
    } else {
        "watermark=$($script:RestartLogWatermarkMs) recent=" + (Protect-Text (Get-Json $script:LastStartupLogs) 400)
    }
    Assert-True -Id 'T4.1' -What 'the plugin startup hook ran again after the restart' -Condition $startup.Ok -Evidence $startupEvidence
    if ($startup.Ok) {
        Assert-Equal -Id 'T4.2' -What 'the startup reconciliation succeeded' -Expected 'succeeded' -Actual ([string](Get-Field $startup.Value 'status'))
        Assert-Match -Id 'T4.3' -What 'the reconciliation reported ok' -Pattern '"ok":true' -Actual ([string](Get-Field $startup.Value 'stdout'))
    }

    $after = Get-ProjectFacts
    $projectionAfter = Get-IdentityProjection -Facts $after
    $projectionBeforeParts = @($projectionBefore -split ';')
    $projectionAfterParts = @($projectionAfter -split ';')
    $projectionDifferences = @(Compare-Object -ReferenceObject $projectionBeforeParts -DifferenceObject $projectionAfterParts -CaseSensitive |
        ForEach-Object { "$($_.SideIndicator) $($_.InputObject)" })
    Assert-True -Id 'T5.1' -What 'stable identities, stages, reports and dispositions survive the restart' `
        -Condition ($projectionDifferences.Count -eq 0) -Evidence (Protect-Text ($projectionDifferences -join ' | ') 1600)
    $decidedIdentityAfter = Get-DecidedTaskIdentityProjection -Facts $after
    $decidedIdentityDifferences = @(Compare-Object -ReferenceObject @($decidedIdentityBefore -split ';') `
        -DifferenceObject @($decidedIdentityAfter -split ';') -CaseSensitive |
        ForEach-Object { "$($_.SideIndicator) $($_.InputObject)" })
    Assert-True -Id 'T5.decided-identities' -What 'pane, terminal and agent identities of decided task history survive restart without a live-pane check' `
        -Condition ($decidedIdentityDifferences.Count -eq 0) -Evidence (Protect-Text ($decidedIdentityDifferences -join ' | ') 1200)
    $restoredProjectPane = [string](Get-Field $after.Project 'LaunchPaneID')
    $restoredProjectReady = Get-ReadySession -LaunchPaneId $restoredProjectPane
    $restoredProjectAgent = Get-LiveAgent -Target $restoredProjectPane
    $restoredProjectSession = [string](Get-Field (Get-Field $restoredProjectAgent.Agent 'agent_session') 'value')
    $restoredProjectSnapshot = @(Get-Field (Get-Snapshot) 'panes' @() | Where-Object { (Get-Field $_ 'pane_id') -ceq $restoredProjectPane })
    # The pane can be absent after a restart, which is the case this assertion
    # exists to catch. Indexing the empty array below would abort the whole run
    # with "Index was outside the bounds of the array" instead of recording that
    # failure, so the terminal id is read only when there is a row to read.
    $restoredProjectTerminal = if ($restoredProjectSnapshot.Count -gt 0) {
        [string](Get-Field $restoredProjectSnapshot[0] 'terminal_id')
    } else {
        '(no pane in the snapshot)'
    }
    Assert-True -Id 'T5.project-session' -What 'the restored primary pane still proves the recorded OMP session and terminal' `
        -Condition ($restoredProjectAgent.Found -and $restoredProjectSnapshot.Count -eq 1 -and
            [int](Get-Field $restoredProjectReady 'verified' 0) -eq 1 -and
            (Test-SessionIdentity -Left ([string](Get-Field $after.Project 'OMPSessionID')) -Right $restoredProjectSession -Ready $restoredProjectReady) -and
            (Test-SamePath -A ([string](Get-Field $after.Project 'Key')) -B ([string](Get-Field $restoredProjectAgent.Agent 'cwd'))) -and
            $restoredProjectTerminal -ceq [string](Get-Field $after.Project 'TerminalID')) `
        -Evidence ("pane=$restoredProjectPane session=$restoredProjectSession terminal=$restoredProjectTerminal")
    foreach ($row in @($after.Tasks)) {
        $fact = Get-Field $row 'fact'
        $taskId = [string](Get-Field $fact 'ID')
        # Only the tasks startup reconciles must prove a live identity here. Herdr
        # gives a restored pane a new terminal id, and startup re-derives it for
        # those tasks alone; a decided task keeps its recorded pane and terminal, and
        # T5.1 already holds its durable facts unchanged.
        if (-not (Test-RestartReconcilesTask -Fact $fact)) {
            if (-not [string]::IsNullOrWhiteSpace([string](Get-Field $fact 'Disposition'))) {
                [void](Add-Assertion -Id "T5.session/$taskId" -Status 'INFO' -TaskId $taskId `
                    -What "decided task $taskId is history: startup does not re-derive its pane, so its live identity is not checked" `
                    -Evidence ("disposition=[$([string](Get-Field $fact 'Disposition'))] recorded terminal=$([string](Get-Field $fact 'TerminalID'))"))
            }
            continue
        }
        # The pane that must exist after a restart is the task's *current* pane,
        # not its launch-time one. The M phase moves a worker pane and by design a
        # move never re-keys the launch id (M4.3), so the launch id names a pane
        # that no longer exists while the live pane is the recorded public one.
        # Here the launch id would require Herdr to restore a pane id that is gone
        # by construction; the public id is the pane the worker is actually in.
        $launchPane = [string](Get-Field $fact 'LaunchPaneID')
        $publicPane = [string](Get-Field $fact 'PublicPaneID')
        $pane = if ($publicPane) { $publicPane } else { $launchPane }
        $session = [string](Get-Field $fact 'OMPSessionID')
        if (-not $pane -or -not $session) { continue }
        $ready = Get-ReadySession -LaunchPaneId $pane
        $agent = Get-LiveAgent -Target $pane
        $liveSession = [string](Get-Field (Get-Field $agent.Agent 'agent_session') 'value')
        $paneSnapshot = @(Get-Field (Get-Snapshot) 'panes' @() | Where-Object { (Get-Field $_ 'pane_id') -ceq $pane })
        # Same guard as the project pane above: a worker whose pane is gone must
        # produce a failed assertion with readable evidence, not an indexing
        # exception that ends the run before the failure is recorded.
        $paneTerminal = if ($paneSnapshot.Count -gt 0) {
            [string](Get-Field $paneSnapshot[0] 'terminal_id')
        } else {
            '(no pane in the snapshot)'
        }
        Assert-True -Id "T5.session/$([string](Get-Field $fact 'ID'))" `
            -What "the restored pane for $([string](Get-Field $fact 'ID')) still proves its recorded OMP session and terminal" `
            -Condition ($agent.Found -and $paneSnapshot.Count -eq 1 -and
                [int](Get-Field $ready 'verified' 0) -eq 1 -and
                (Test-SessionIdentity -Left $session -Right $liveSession -Ready $ready) -and
                (Test-SamePath -A ([string](Get-Field $fact 'WorktreePath')) -B ([string](Get-Field $agent.Agent 'cwd'))) -and
                $paneTerminal -ceq [string](Get-Field $fact 'TerminalID')) `
            -TaskId ([string](Get-Field $fact 'ID')) -Evidence ("pane=$pane (launch=$launchPane) session=$liveSession terminal=$paneTerminal")
    }
    $beforeInboxIds = @($before.Inbox | ForEach-Object { [string](Get-Field $_ 'id') })
    $afterInboxIds = @($after.Inbox | ForEach-Object { [string](Get-Field $_ 'id') })
    $missingInboxIds = @($beforeInboxIds | Where-Object { $_ -notin $afterInboxIds })
    $inboxPreserved = Test-InboxRowsPreserved -Before $before.Inbox -After $after.Inbox
    Assert-True -Id 'T5.2' -What 'all pre-restart inbox notes and their content survive restart' -Condition $inboxPreserved `
        -Evidence ("before={0} after={1} missing=[{2}]" -f @($before.Inbox).Count, @($after.Inbox).Count, ($missingInboxIds -join ','))

    $listing = Get-WorktreeList
    $entries = @(Get-Field $listing 'worktrees' @())
    foreach ($row in @($after.Tasks)) {
        $f = Get-Field $row 'fact'
        $path = [string](Get-Field $f 'WorktreePath')
        if ([string]::IsNullOrWhiteSpace($path)) { continue }
        $exists = Test-Path -LiteralPath $path
        $listed = @($entries | Where-Object { Test-SamePath -A (Get-Field $_ 'path') -B $path })
        if ($exists) {
            Assert-Equal -Id "T6.$([string](Get-Field $f 'ID'))" -What "the worktree of $([string](Get-Field $f 'ID')) is still listed after the restart" -Expected 1 -Actual $listed.Count -Evidence $path
        } else {
            Assert-True -Id "T6.$([string](Get-Field $f 'ID'))-removed" -What "the task whose checkout cleanup removed records no live checkout" -Condition $true -Evidence $path
        }
    }

    $restored = Wait-Until -TimeoutSeconds 240 -IntervalSeconds 8 -What 'restored primary' -Probe {
        $snapshot = Get-Snapshot
        foreach ($agent in @($snapshot.agents)) {
            if ((ConvertTo-NormalPath ([string](Get-Field $agent 'cwd'))) -ieq (ConvertTo-NormalPath $script:RepoPath)) { return $agent }
        }
        return $null
    }
    Assert-True -Id 'T7.1' -What 'Herdr restored an agent working in the bound repository' -Condition $restored.Ok -Evidence (Protect-Text (Get-Json $restored.Value) 400)
    if ($restored.Ok) {
        $restoredPane = [string](Get-Field $restored.Value 'pane_id')
        $agent = Get-LiveAgent -Target $restoredPane
        if ($agent.Found) {
            $ref = Get-Field $agent.Agent 'agent_session'
            $restoredReady = Get-ReadySession -LaunchPaneId ([string](Get-Field $script:Project 'LaunchPaneID'))
            Assert-True -Id 'T7.2' -What 'the restored agent still carries the recorded OMP session (compared by reference kind)' `
                -Condition (Test-SessionIdentity -Left ([string](Get-Field $script:Project 'OMPSessionID')) -Right ([string](Get-Field $ref 'value')) -Ready $restoredReady) `
                -Evidence ("recorded=[$(Get-Field $script:Project 'OMPSessionID')] ref=[$(Get-Field $ref 'value')] handshake=" + (Protect-Text (Get-Json $restoredReady) 300))
        } else {
            Block-Item -Id 'T7.2' -What 'the restored agent still carries the recorded OMP session' -Evidence (Protect-Text (Get-Json $agent.Raw) 300)
        }
        $script:RestoredPrimaryPane = $restoredPane
        $restoredProject = (Get-ProjectFacts).Project
        $restoredTerminalId = [string](Get-Field $restored.Value 'terminal_id')
        Assert-NonEmpty -Id 'T7.3' -What 'Herdr restored the primary with a live terminal identity' -Value $restoredTerminalId
        Assert-Equal -Id 'T7.4' -What 'the project terminal identity was refreshed to match the restored primary' -Expected $restoredTerminalId -Actual ([string](Get-Field $restoredProject 'TerminalID'))
    }

    if ($script:RestoredPrimaryPane) {
        $idA = if ($script:TaskA) { [string](Get-Field $script:TaskA 'ID') } else { '' }
        if ($idA) {
            $beforeReview = Get-Field (Get-TaskById -Id $idA) 'fact'
            if (-not (Test-TaskIsReviewedDone -Fact $beforeReview)) {
                Block-Item -Id 'T8.precondition' -What 'restore review probe requires a final report with a recorded disposition' `
                    -TaskId $idA -Evidence 'no review tool prompt or confirmation was sent'
            } else {
                $recordedDisposition = [string](Get-Field $beforeReview 'Disposition')
                Assert-NonEmpty -Id 'T8.precondition' -What 'task A already has a recorded disposition before restore probe' -Value $recordedDisposition -TaskId $idA
                $restoredDisposition = 'ACCEPT-TOOLS-RESTORED-' + $script:RunId
            $probe = 'ACCEPT-TOOLS-RESTORED. The user already decided task "' + $idA + '". To prove your primary tools survived restore, call orch_review with taskId "' + $idA + '", outcome "integrated" and disposition "' + $restoredDisposition + '". Show the stored report and confirmation; the backend must refuse to rewrite the first decision. If the tool is unavailable, reply exactly NOTOOLHERE.'
            $result = Invoke-PrimaryPromptNoWaitAfterDispositionAsks -PaneId $script:RestoredPrimaryPane -Text $probe -TimeoutSeconds 300 -AssertionPrefix 'T8'
            Assert-True -Id 'T8.1' -What 'the restored primary session accepts a prompt' -Condition ($result.Submitted -and ($result.ExitCode -eq 0 -or $result.Blocked)) -Command $result.CommandLine -Evidence ("code=[$($result.Code)] blocked=[$($result.Blocked)] stderr=[" + (Protect-Text $result.Stderr 300) + "] screen=[" + (Protect-Text $result.Screen 500) + "]")
            $confirmed = $false
            if ($result.Submitted) {
                $confirmed = Confirm-PrimaryReviewModal -PaneId $script:RestoredPrimaryPane `
                    -LaunchPaneId ([string](Get-Field $restoredProject 'LaunchPaneID')) -TaskId $idA -Disposition $restoredDisposition
            }
            Assert-True -Id 'T8.confirm' -What 'the acceptance user confirmed the exact restored review call' -Condition $confirmed -TaskId $idA `
                -Evidence (Protect-Text (Get-PaneScreen -PaneId $script:RestoredPrimaryPane -Source 'visible' -Lines 100) 500)
            if (-not $confirmed) { throw 'restored review confirmation was not verified' }
            $ready = Get-ReadySession -LaunchPaneId ([string](Get-Field $restoredProject 'LaunchPaneID'))
            $transcript = [string](Get-Field $ready 'omp_session_file')
            $toolRan = Wait-Until -TimeoutSeconds 240 -IntervalSeconds 8 -What 'restored role tool refused a second decision' -Probe {
                $screen = Get-PaneScreen -PaneId $script:RestoredPrimaryPane -Source 'recent-unwrapped' -Lines 100
                if (Test-RestoredReviewRefusal -Path $transcript -TaskId $idA -Disposition $restoredDisposition -Screen $screen) {
                    return $true
                }
                return $null
            }
            $afterReview = Get-Field (Get-TaskById -Id $idA) 'fact'
            Assert-True -Id 'T8.2' -What 'the restored role tool ran but could not rewrite the recorded user decision' `
                -Condition ($toolRan.Ok -and [string](Get-Field $afterReview 'Disposition') -ceq $recordedDisposition -and
                    [long](Get-Field $afterReview 'Revision' 0) -eq [long](Get-Field $beforeReview 'Revision' 0)) -TaskId $idA `
                -Evidence (Protect-Text (Get-PaneScreen -PaneId $script:RestoredPrimaryPane -Source 'recent-unwrapped' -Lines 80) 600)
            }
        }
    } else {
        Block-Item -Id 'T8.1' -What 'the restored session role tools' -Evidence 'no restored primary pane was identified after the restart'
    }

    $context = @{
        'HERDR_ENV'          = '1'
        'HERDR_PANE_ID'      = [string](Get-Field $before.Project 'LaunchPaneID')
        'HERDR_WORKSPACE_ID' = [string](Get-Field $before.Project 'RepoWorkspaceID')
    }
    $invoke = Invoke-PluginAction -ActionId 'bind' -PaneContext $context -TimeoutSeconds 240
    Assert-True -Id 'T8.9' -What 'the re-bind after the restart names the isolated session' `
        -Condition ($null -ne $invoke -and $invoke.Invocation.CommandLine -match [regex]::Escape("--session $($script:SessionName)")) `
        -Evidence $(if ($invoke) { $invoke.Invocation.CommandLine } else { 'no invocation' })
    $output = if ($invoke) { Get-BindPayload -Result (Get-Field $invoke 'Result') } else { $null }
    Assert-Equal -Id 'T9.1' -What 're-binding after the restart exits 0' -Expected 0 -Actual $(if ($output) { Get-Field $output.Entry 'exit_code' } else { 'no invocation' })
    if ($output -and $output.Payload) {
        $p = Get-Field $output.Payload 'project'
        Assert-SamePath -Id 'T9.2' -What 'the project key survives the restart' -Expected ([string](Get-Field $before.Project 'Key')) -Actual ([string](Get-Field $p 'Key'))
        Assert-Equal -Id 'T9.3' -What 'the primary agent name survives the restart' -Expected ([string](Get-Field $before.Project 'AgentName')) -Actual ([string](Get-Field $p 'AgentName'))
        Assert-Equal -Id 'T9.4' -What 'the OMP session identity survives the restart' -Expected ([string](Get-Field $before.Project 'OMPSessionID')) -Actual ([string](Get-Field $p 'OMPSessionID'))
        $hint = Get-Field $output.Payload 'session'
        $resume = [string](Get-Field $hint 'resume')
        Assert-True -Id 'T10.1' -What 'the resume hint after the restart is still gated on a materialized session' `
            -Condition (Test-ResumeHintImplication -Hint $hint) -Evidence (Protect-Text (Get-Json $hint) 400)
        if ($resume -ne '') {
            $restoredHandshake = Get-ReadySession -LaunchPaneId ([string](Get-Field $p 'LaunchPaneID'))
            if (-not $restoredHandshake) { $restoredHandshake = Get-ReadySession -LaunchPaneId ([string](Get-Field $before.Project 'LaunchPaneID')) }
            $fileState = Test-HandshakeMaterialized -Handshake $restoredHandshake
            if ($null -eq $fileState) {
                Add-Assertion -Id 'T10.2' -What 'a resume command was advertised for a session whose reference is id-only (materialization is the runtime-announced flag, not a filesystem path)' -Status 'INFO' `
                    -Evidence ("onDisk=[$(Get-Field $hint 'onDisk')] file=[$(Get-Field $restoredHandshake 'omp_session_file')]")
            } else {
                Assert-True -Id 'T10.2' -What 'the advertised resume session file exists on disk' -Condition $fileState -Evidence ([string](Get-Field $restoredHandshake 'omp_session_file'))
            }
        } else {
            Block-Item -Id 'T10.2' -What 'a resume command was advertised after the restart' -Evidence (Protect-Text (Get-Json $hint) 400)
        }
    }
}

# ---------------------------------------------------------------------------
# Phase 20: cleanup, isolation proof and report
# ---------------------------------------------------------------------------

function Get-RunLaunchPanes {
    $panes = New-Object System.Collections.ArrayList
    if ($script:Project) {
        foreach ($field in @('LaunchPaneID', 'PublicPaneID')) {
            $value = [string](Get-Field $script:Project $field)
            if ($value -and -not $panes.Contains($value)) { [void]$panes.Add($value) }
        }
    }
    foreach ($row in @($script:Tasks)) {
        $fact = Get-Field $row 'fact'
        if (-not $fact) { $fact = $row }
        foreach ($field in @('LaunchPaneID', 'PublicPaneID')) {
            $value = [string](Get-Field $fact $field)
            if ($value -and -not $panes.Contains($value)) { [void]$panes.Add($value) }
        }
    }
    return $panes
}

function Invoke-Cleanup {
    Write-Step 'Phase 20: cleanup, isolation proof and restoration'
    if ($script:RunManifest) { Save-AcceptanceRunManifest -Phase 'cleanup-started' -Status 'cleanup' }

    # 0. Capture the session files this run materialized BEFORE any state row is
    # deleted, so the preservation check below can still name them afterwards.
    $materializedSessionFiles = New-Object System.Collections.ArrayList
    if ($script:Python) {
        $handshakes = New-Object System.Collections.ArrayList
        if ($script:Project) {
            $primaryHandshake = Get-ReadySession -LaunchPaneId ([string](Get-Field $script:Project 'LaunchPaneID'))
            if ($primaryHandshake) { [void]$handshakes.Add($primaryHandshake) }
        }
        foreach ($row in @($script:Tasks)) {
            $factRef = Get-Field $row 'fact'
            if (-not $factRef) { $factRef = $row }
            $workerHandshake = Get-ReadySession -LaunchPaneId ([string](Get-Field $factRef 'LaunchPaneID'))
            if ($workerHandshake) { [void]$handshakes.Add($workerHandshake) }
        }
        foreach ($handshake in @($handshakes)) {
            $file = [string](Get-Field $handshake 'omp_session_file')
            if ($file -and (Test-OmpSessionMaterialized -Reference $file)) { [void]$materializedSessionFiles.Add($file) }
        }
        Add-Assertion -Id 'Z0.1' -What 'materialized OMP session files captured before any state row was deleted' -Status 'INFO' `
            -Evidence ("count={0} files={1}" -f @($materializedSessionFiles).Count, (Protect-Text (Get-Json @($materializedSessionFiles)) 400))
    }

    # 1. Focus restoration, always, even when the scenario failed early.
    if ($script:SessionCreated) {
        try {
            $list = Get-HerdrJson -Arguments @('workspace', 'list') -TimeoutSeconds 60
            $live = @((Get-JsonField $list 'workspaces' @()) | ForEach-Object { [string](Get-Field $_ 'workspace_id') })
            $ours = @($script:CreatedWorkspaces | Where-Object { $live -contains $_ } | Select-Object -Unique)
            if ($ours.Count -gt 0) {
                $focus = Invoke-Herdr -Arguments @('workspace', 'focus', $ours[0]) -TimeoutSeconds 60
                Assert-True -Id 'Z1.1' -What 'the session focus was restored before the resources were closed' -Condition ($focus.ExitCode -eq 0) -Evidence (Protect-Text $focus.Stderr 200)
            } else {
                Assert-True -Id 'Z1.1' -What 'no workspace of this run was left to restore focus to' -Condition $true
            }
        } catch {
            Add-Assertion -Id 'Z1.1' -What 'focus restoration' -Status 'FAIL' -Evidence (Protect-Text $_.Exception.Message 400)
        }

        # The root repository and linked worktree panes form one Herdr workspace
        # group. Close that group as a unit; individual closes require confirmation.
        if (-not $KeepResources) {
            $rootWorkspace = [string](Get-Field $script:Project 'RepoWorkspaceID')
            if ([string]::IsNullOrWhiteSpace($rootWorkspace)) { $rootWorkspace = [string]$script:PrimaryWorkspace }
            $close = Invoke-Herdr -Arguments @('workspace', 'close', $rootWorkspace, '--group') -TimeoutSeconds 60
            $closeCode = Get-HerdrErrorCode -Result $close
            $closeOk = ($close.ExitCode -eq 0) -or ($closeCode -in @('workspace_not_found', 'not_found'))
            Assert-True -Id 'Z2.group-close' -What 'Herdr closes the isolated repository and its linked worktree group' -Condition $closeOk `
                -Command $close.CommandLine -Evidence (Protect-Text ($close.Stderr + $close.Stdout) 400)
            $script:CreatedPanes.Clear()
            $script:CreatedWorkspaces.Clear()
            $remaining = Get-HerdrJson -Arguments @('workspace', 'list') -TimeoutSeconds 60
            Assert-Equal -Id 'Z2.1' -What 'every workspace this run created is closed again' -Expected 0 -Actual @((Get-JsonField $remaining 'workspaces' @())).Count `
                -Evidence (Protect-Text (Get-Json (Get-JsonField $remaining 'workspaces' @())) 400)
            $finalSnapshot = Get-Snapshot
            Assert-Equal -Id 'Z2.2' -What 'the isolated session is back to the empty focus state it had before this run created anything' `
                -Expected $script:InitialWorkspaceCount -Actual @($finalSnapshot.workspaces).Count `
                -Evidence ("focused_workspace_id=[{0}] focused_pane_id=[{1}]" -f (Get-Field $finalSnapshot 'focused_workspace_id'), (Get-Field $finalSnapshot 'focused_pane_id'))
        } else {
            Add-Assertion -Id 'Z2.3' -What 'the run workspaces, panes and session server were intentionally kept (-KeepResources)' -Status 'INFO' `
                -Evidence ("workspaces=[$($script:CreatedWorkspaces -join ',')] panes=[$($script:CreatedPanes -join ',')] session=$($script:SessionName)")
        }
    }

    # 3. Remove the temporary checkouts and repository, by exact recorded path.
    if (Test-Path -LiteralPath $script:RepoPath) {
        $paths = New-Object System.Collections.ArrayList
        foreach ($row in @($script:Tasks)) {
            $fact = Get-Field $row 'fact'
            if (-not $fact) { $fact = $row }
            $path = [string](Get-Field $fact 'WorktreePath')
            if ($path) { [void]$paths.Add($path) }
        }
        foreach ($path in @($paths | Select-Object -Unique)) {
            Add-Assertion -Id "Z3.1/$(Split-Path -Leaf $path)" -What 'the recorded checkout belongs to this run before it is deleted' `
                -Status $(if (Test-Below -Root $script:WorkRoot -Path $path) { 'PASS' } else { 'BLOCKED' }) -Evidence $path
        }
    }
    if ($KeepResources) {
        Add-Assertion -Id 'Z3.2' -What 'the temporary checkouts and repository were intentionally kept (-KeepResources)' -Status 'INFO' `
            -Evidence ("repo=$($script:RepoPath) worktrees=$($script:WorktreesRoot)")
    } else {
        if (Test-Path -LiteralPath $script:WorktreesRoot) { Remove-Item -LiteralPath $script:WorktreesRoot -Recurse -Force -ErrorAction SilentlyContinue }
        if (Test-Path -LiteralPath $script:RepoPath) { Remove-Item -LiteralPath $script:RepoPath -Recurse -Force -ErrorAction SilentlyContinue }
    }

    # 4. Delete exactly this run's state rows, after proving the scope.
    if ($KeepResources -or $SkipStateCleanup) {
        Add-Assertion -Id 'Z4.0a' -What 'the state rows were intentionally kept (cleanup opt-out)' -Status 'INFO' `
            -Evidence "KeepResources=$([bool]$KeepResources) SkipStateCleanup=$([bool]$SkipStateCleanup) project=[$(Get-Field $script:Project 'Key')]"
    } elseif ($script:Python -and $script:StateDb -and (Test-Path -LiteralPath $script:StateDb) -and (Get-Field $script:Project 'Key')) {
        try {
            $panes = @(Get-RunLaunchPanes)
            $plan = Invoke-State -Operation 'plan-scoped' -Arguments @([string](Get-Field $script:Project 'Key'), ($panes -join ','))
            Assert-True -Id 'Z4.1' -What 'no row outside this run references any of its identities' -Condition (@($plan.foreign).Count -eq 0) `
                -Evidence (Protect-Text (Get-Json $plan.foreign) 400)
            if (@($plan.foreign).Count -eq 0) {
                $deleted = Invoke-State -Operation 'delete-scoped' -Arguments @([string](Get-Field $script:Project 'Key'), ($panes -join ','))
                Assert-JsonTrue -Id 'Z4.2' -What 'the state cleanup deleted rows only for this run project key (a real boolean)' -Object $deleted -Name 'ok'
                Assert-JsonTrue -Id 'Z4.3' -What 'every other row in the store is byte-identical after the delete (a real boolean)' -Object $deleted -Name 'others_identical' `
                    -Evidence ("others_before={0} others_after={1}" -f (Get-Field $deleted 'others_before'), (Get-Field $deleted 'others_after'))
                Assert-Equal -Id 'Z4.4' -What 'no row of this run is left behind' -Expected 0 -Actual ([int](Get-Field $deleted 'ours_remaining'))
                Add-Assertion -Id 'Z4.5' -What 'rows deleted for this run' -Status 'INFO' -Evidence (Protect-Text (Get-Json (Get-Field $deleted 'deleted')) 300)
            } else {
                Block-Item -Id 'Z4.2' -What 'state rows were left in place because the scope could not be proven' -Evidence (Protect-Text (Get-Json $plan.foreign) 400)
            }
        } catch {
            Add-Assertion -Id 'Z4.ERR' -What 'state cleanup' -Status 'FAIL' -Evidence (Protect-Text $_.Exception.Message 600)
        }
    } elseif (Get-Field $script:Project 'Key') {
        Block-Item -Id 'Z4.0' -What 'state rows were left in place because no SQLite interpreter was available' -Evidence $script:StateDb
    } elseif (-not ($KeepResources -or $SkipStateCleanup)) {
        Add-Assertion -Id 'Z4.0' -What 'no project was bound, so no state rows needed deleting' -Status 'PASS'
    }

    # 5. Stop and delete the isolated session (kept under -KeepResources). The
    # user's default session is never touched either way.
    if ($script:ServerStartedByScript -and -not $KeepResources) {
        [void](Invoke-Herdr -Arguments @('server', 'stop') -TimeoutSeconds 90)
        $stopped = Wait-Until -TimeoutSeconds 60 -IntervalSeconds 3 -What 'session stopped' -Probe {
            $mine = @(Get-Sessions | Where-Object { (Get-Field $_ 'name') -eq $script:SessionName })
            if ($mine.Count -eq 0) { return $null }
            if (Get-Flag $mine[0] 'running') { return $null }
            return $true
        }
        Assert-True -Id 'Z5.1' -What 'the isolated session server is stopped' -Condition $stopped.Ok
        $delete = Invoke-Herdr -Arguments @('session', 'delete', $script:SessionName) -NoSessionScope -TimeoutSeconds 60
        Assert-Equal -Id 'Z5.2' -What 'the isolated session is deleted' -Expected 0 -Actual $delete.ExitCode -Evidence (Protect-Text $delete.Stderr 300)
        $remainingSessionCount = @((Get-Sessions) | Where-Object { (Get-Field $_ 'name') -eq $script:SessionName }).Count
        Assert-Equal -Id 'Z5.3' -What 'this run session is gone from the session list' -Expected 0 -Actual $remainingSessionCount
        if ($delete.ExitCode -ne 0 -or $remainingSessionCount -ne 0) {
            throw "refusing to kill the server process for $($script:SessionName): its session was not verified absent"
        }
        $serverProcessResult = Stop-VerifiedAcceptanceServerProcess -RunId $script:RunId `
            -Manifest $script:RunManifest -SessionDisposition 'absent'
        Assert-True -Id 'Z5.server-process' -What 'the exact isolated Herdr server process exited after session deletion' `
            -Condition ($serverProcessResult -in @('already-exited', 'killed')) -Evidence $serverProcessResult
    }

    # 6. Prove nothing of the user's was changed.
    $default = Get-HerdrJson -Arguments @('api', 'snapshot') -NoSessionScope
    $defaultProjection = Get-FocusProjection -Snapshot (Get-JsonField $default 'snapshot')
    $defaultRecord = @(Get-Sessions | Where-Object { (Get-Field $_ 'name') -eq 'default' })
    Assert-Equal -Id 'Z6.1' -What "the user's default session still has its own server, directory and socket (this run never stopped, restarted or replaced it)" `
        -Expected 1 -Actual $defaultRecord.Count
    if ($defaultRecord.Count -gt 0 -and $script:DefaultSessionRecord) {
        $now = [pscustomobject]@{
            Name    = [string](Get-Field $defaultRecord[0] 'name')
            Dir     = [string](Get-Field $defaultRecord[0] 'session_dir')
            Socket  = [string](Get-Field $defaultRecord[0] 'socket_path')
            Running = (Get-Flag $defaultRecord[0] 'running')
        }
        Assert-True -Id 'Z6.1a' -What "the default session's directory, socket and running state are unchanged" `
            -Condition ((Get-Json $now) -ceq (Get-Json $script:DefaultSessionRecord)) -Evidence (Get-Json $now)
    }
    Assert-True -Id 'Z6.1b' -What "no pane or agent of the user's default session works anywhere in this run's temp root" `
        -Condition (-not (Test-ProjectionMentions -Projection $defaultProjection -Path $script:WorkRoot)) `
        -Evidence (Protect-Text $defaultProjection 500)
    $defaultSocket = [string](Get-Field $script:DefaultSessionRecord 'Socket')
    $socketIsolation = Test-SessionSocketSeparation -ServerStarted $script:ServerStartedByScript `
        -RunSocket $script:SessionSocket -DefaultSocket $defaultSocket
    if ($null -eq $socketIsolation) {
        Add-Assertion -Id 'Z6.1c' -What 'isolated-server socket check was not applicable because this run never started its server' -Status 'INFO' `
            -Evidence 'preflight or setup stopped before the run socket was captured'
    } else {
        Assert-True -Id 'Z6.1c' -What 'this run used its own server, not the default session socket' `
            -Condition $socketIsolation -Evidence "run=$(Protect-Text $script:SessionSocket 200) default=$(Protect-Text $defaultSocket 200)"
    }

    $pluginList = Get-HerdrJson -Arguments @('plugin', 'list', '--json', '--plugin', $script:PluginId) -NoSessionScope
    $plugin = @((Get-JsonField $pluginList 'plugins' @()) | Where-Object { (Get-Field $_ 'plugin_id') -eq $script:PluginId })
    Assert-Equal -Id 'Z6.2' -What 'the plugin is still linked exactly once (this run never unlinked it)' -Expected 1 -Actual $plugin.Count
    if ($plugin.Count -gt 0) {
        Assert-SamePath -Id 'Z6.3' -What 'the plugin root is unchanged' -Expected $script:PluginRegistrationBefore.Root -Actual ([string](Get-Field $plugin[0] 'plugin_root'))
        Assert-SamePath -Id 'Z6.4' -What 'the manifest path is unchanged' -Expected $script:PluginRegistrationBefore.Manifest -Actual ([string](Get-Field $plugin[0] 'manifest_path'))
        Assert-Equal -Id 'Z6.5' -What 'the plugin is still enabled' -Expected $script:PluginRegistrationBefore.Enabled -Actual (Get-Flag $plugin[0] 'enabled')
    }
    $extDir = Get-EffectiveExtensionDir
    $hashAtStart = if ($script:ExtensionFileHashAfter) { $script:ExtensionFileHashAfter } else { $script:ExtensionFileHashBefore }
    Assert-Equal -Id 'Z6.6' -What 'the run left the installed orchestrator extension exactly as its own setup installed it' -Expected $hashAtStart -Actual (Get-FileHashSafe (Join-Path $extDir 'herdr-orchestrator.ts'))
    Assert-Equal -Id 'Z6.7' -What "this run left Herdr's own OMP integration byte-identical" -Expected $script:IntegrationFileHashBefore -Actual (Get-FileHashSafe (Join-Path $extDir 'herdr-omp-agent-state.ts'))
    $integration = Invoke-Herdr -Arguments @('integration', 'status') -NoSessionScope
    $ompLine = (@([string]$integration.Stdout -split "`n") | Where-Object { $_.Trim().StartsWith('omp:') } | Select-Object -First 1)
    Assert-True -Id 'Z6.8' -What 'the Herdr OMP integration is still installed' -Condition ($ompLine -and $ompLine -notmatch '(?i)not installed') -Evidence (Protect-Text $ompLine 200)

    # OMP's own session history is never deleted.
    if (@($materializedSessionFiles).Count -gt 0) {
        $stillThere = @($materializedSessionFiles | Where-Object { Test-OmpSessionMaterialized -Reference $_ })
        Assert-Equal -Id 'Z6.9' -What "this run did not delete OMP's session history: every materialized session file (primary and workers) captured before the run's rows were removed is still on disk" `
            -Expected @($materializedSessionFiles).Count -Actual $stillThere.Count `
            -Evidence ("files: " + (Protect-Text (Get-Json @($materializedSessionFiles)) 500))
    } else {
        Add-Assertion -Id 'Z6.9' -What 'no materialized OMP session file was present to preserve' -Status 'INFO' `
            -Evidence 'either the runtime had not written a session file yet, or Herdr reports an id-only reference that cannot be checked from the filesystem'
    }

    if (-not $KeepResources) {
        $removeError = ''
        if (Test-Path -LiteralPath $script:WorkRoot) {
            try { Remove-Item -LiteralPath $script:WorkRoot -Recurse -Force -ErrorAction Stop } catch {
                $removeError = $_.Exception.Message
                Add-Assertion -Id 'Z7.ERR' -What 'working temp root removal' -Status 'FAIL' -Evidence (Protect-Text $removeError 500)
            }
        }
        Assert-True -Id 'Z7.1' -What 'the temporary working root is gone after cleanup' `
            -Condition (-not (Test-Path -LiteralPath $script:WorkRoot)) -Evidence (Protect-Text $removeError 300)
    } else {
        Add-Assertion -Id 'Z7.1' -What 'resources were intentionally kept (-KeepResources)' -Status 'INFO' -Evidence $script:WorkRoot
    }
    $stateHelperGone = $true
    $stateHelperError = ''
    if (Test-Path -LiteralPath $script:StateHelper) {
        try { Remove-Item -LiteralPath $script:StateHelper -Force -ErrorAction Stop } catch {
            $stateHelperGone = $false
            $stateHelperError = $_.Exception.Message
        }
    }
    Assert-True -Id 'Z7.2' -What 'the temporary SQLite helper is removed from the report archive' `
        -Condition ($stateHelperGone -and -not (Test-Path -LiteralPath $script:StateHelper)) -Evidence (Protect-Text $stateHelperError 300)
}

function Write-Report {
    $summary = [pscustomobject]@{
        runId        = $script:RunId
        session      = $script:SessionName
        startedAt    = $script:StartedAt
        finishedAt   = (Get-Date).ToUniversalTime().ToString('o')
        passed                  = $script:Passed
        failed                  = $script:Failures
        blocked                 = $script:Blocked
        scenario                = $Scenario
        scenarioTimeoutSeconds  = $ScenarioTimeoutSeconds
        exitCode                = 0
        projectKey              = if ($script:Project) { [string](Get-Field $script:Project 'Key') } elseif ($script:RunManifest) { [string]$script:RepoPath } else { '' }
        workRoot                = $script:WorkRoot
        manifestPath            = $script:RunManifestPath
        serverVersion           = $script:SessionServerVersion
        keepResourcesRequested  = $script:KeepResourcesRequested
        resourcesPreserved      = [bool]$KeepResources
        assertions              = @($script:Assertions)
    }
    $code = if ($script:Failures -gt 0) { 1 } elseif ($script:Blocked -gt 0) { 2 } else { 0 }
    $summary.exitCode = $code
    $summary | ConvertTo-Json -Depth 6 | Set-Content -LiteralPath $script:ReportJson -Encoding UTF8

    $lines = New-Object System.Collections.ArrayList
    [void]$lines.Add("acceptance run $($script:RunId) on session $($script:SessionName) (server $($script:SessionServerVersion))")
    [void]$lines.Add("passed=$($script:Passed) failed=$($script:Failures) blocked=$($script:Blocked) exit=$code")
    foreach ($record in $script:Assertions) {
        [void]$lines.Add(("{0,-7} {1,-6} {2}" -f $record.status, $record.id, $record.what))
        if ($record.status -in @('FAIL', 'BLOCKED')) {
            if ($record.task) { [void]$lines.Add("          task: $($record.task)") }
            if ($record.command) { [void]$lines.Add("          command: $($record.command)") }
            if ($record.evidence) { [void]$lines.Add("          evidence: $($record.evidence)") }
        }
    }
    $lines | Set-Content -LiteralPath $script:ReportText -Encoding UTF8
    return $code
}

# ---------------------------------------------------------------------------
# driver
# ---------------------------------------------------------------------------

function Step-Phase {
    param(
        [Parameter(Mandatory)][string]$Id,
        [Parameter(Mandatory)][string]$Name,
        [Parameter(Mandatory)][scriptblock]$Body,
        [switch]$Critical
    )
    try {
        [void](Get-ScenarioBoundedTimeoutSeconds -Requested 1)
        & $Body
    } catch {
        if (Test-IsBlockedPrimaryDispositionPrerequisite -PhaseId $Id -Message $_.Exception.Message) {
            $script:BlockedRunAbort = $true
            Preserve-ResourcesForPendingTool -AssertionId 'K0.resources-preserved' -Reason (Protect-Text $_.Exception.Message 600)
            throw
        }
        Add-Assertion -Id "$Id.ERR" -What "$Name raised an error" -Status 'FAIL' -Evidence (Protect-Text ($_.Exception.Message + ' ' + $_.ScriptStackTrace) 800)
        if ($Critical) {
            if ($Id -eq 'K0') {
                Preserve-ResourcesForPendingTool -AssertionId 'K0.resources-preserved' -Reason (Protect-Text $_.Exception.Message 600)
            }
            throw
        }
    }
}

$script:StartedAt = (Get-Date).ToUniversalTime().ToString('o')
$script:Aborted = $false
$script:BlockedRunAbort = $false
$script:KeepResources = [bool]$KeepResources
$script:RunLockStateDb = Get-AcceptanceStateDbPath
$script:StateDb = $script:RunLockStateDb
$script:RunLockPath = Get-AcceptanceRunLockPath -StateDb $script:RunLockStateDb
$script:RunManifest = New-AcceptanceRunManifest
$script:RunLockStream = Initialize-AcceptanceRun -LockPath $script:RunLockPath `
    -RecordDir $script:RunRecordDir -Manifest $script:RunManifest
if (-not $script:RunLockStream) {
    Write-Host 'Another acceptance run for this Herdr state store is active; no new working temp root was created.' -ForegroundColor Yellow
    exit 3
}
Save-AcceptanceRunManifest -Phase 'preflight' -Status 'active'
Set-Content -LiteralPath $script:ReportText -Value "acceptance run $($script:RunId) starting at $($script:StartedAt)" -Encoding UTF8
Write-Host "herdr OMP orchestrator acceptance run $($script:RunId)" -ForegroundColor White
Write-Host "plugin root : $($script:RepoRoot)"
Write-Host "run root    : $($script:WorkRoot) (created only after preflight)"
Write-Host "session     : $($script:SessionName)"

try {
    Step-Phase -Id 'P0' -Name 'preflight' -Critical -Body { Invoke-Preflight }
    Step-Phase -Id 'B0' -Name 'build' -Critical -Body { Invoke-Build }
    Step-Phase -Id 'L0' -Name 'link' -Critical -Body { Invoke-Link }
    Step-Phase -Id 'S0' -Name 'isolated session' -Critical -Body {
        Invoke-IsolatedSession
        $script:SessionCreated = $true
    }
    Step-Phase -Id 'R0' -Name 'temp repository' -Critical -Body { Invoke-TempRepo }
    Step-Phase -Id 'U0' -Name 'setup action' -Critical -Body { Invoke-SetupAction }
    Step-Phase -Id 'N0' -Name 'bind' -Critical -Body { Invoke-Bind }
    if ($Scenario -eq 'SingleWorker') {
        $script:ScenarioDeadline = (Get-Date).AddSeconds($ScenarioTimeoutSeconds)
    }
    foreach ($phase in (Get-AcceptanceScenarioPhases -Scenario $Scenario)) {
        switch ($phase) {
            'N7' { Step-Phase -Id 'N7' -Name 'primary turn' -Body { Invoke-PrimaryTurn }; break }
            'D0' { Step-Phase -Id 'D0' -Name 'dispatch' -Critical -Body { Invoke-Dispatch }; break }
            'PR0' { Step-Phase -Id 'PR0' -Name 'partial report rejection' -Body { Invoke-PartialReportRejection }; break }
            'Q0' { Step-Phase -Id 'Q0' -Name 'question relay' -Body { Invoke-QuestionRelay }; break }
            'K0' { Step-Phase -Id 'K0' -Name 'blocked dialog' -Critical -Body { Invoke-BlockedDialog }; break }
            'M0' { Step-Phase -Id 'M0' -Name 'pane move' -Body { Invoke-PaneMove }; break }
            'W0' { Step-Phase -Id 'W0' -Name 'worker reports' -Body { Invoke-WorkerReports }; break }
            'SW0' { Step-Phase -Id 'SW0' -Name 'single-worker dispatch' -Critical -Body { Invoke-SingleWorkerDispatch }; break }
            'SW1' { Step-Phase -Id 'SW1' -Name 'single-worker report' -Critical -Body { Invoke-WorkerReports }; break }
            'A0' { Step-Phase -Id 'A0' -Name 'board' -Body { Invoke-Board }; break }
            'V0' { Step-Phase -Id 'V0' -Name 'reviews' -Body { Invoke-Reviews }; break }
            'TK0' { Step-Phase -Id 'TK0' -Name 'tickets' -Body { Invoke-Tickets }; break }
            'C0' { Step-Phase -Id 'C0' -Name 'cleanup refusals' -Body { Invoke-CleanupRefusals }; break }
            'G0' { Step-Phase -Id 'G0' -Name 'ambiguous prompt' -Body { Invoke-AmbiguousPrompt }; break }
            'T0' { Step-Phase -Id 'T0' -Name 'restart' -Body { Invoke-Restart }; break }
            default { throw "unknown acceptance phase '$phase' for scenario '$Scenario'" }
        }
    }
} catch {
    $script:Aborted = $true
    if (-not $script:BlockedRunAbort) {
        Add-Assertion -Id 'RUN.ABORT' -What 'the run stopped at a prerequisite failure' -Status 'FAIL' -Evidence (Protect-Text $_.Exception.Message 800)
    }
} finally {
    $script:ScenarioDeadline = $null
    if ($script:PendingUserInteraction -and -not $KeepResources) {
        Preserve-ResourcesForPendingTool -AssertionId 'RUN.pending-interaction-preserved' -Reason 'the final journal still records an unresolved user interaction'
    }
    try { Invoke-Cleanup } catch {
        Add-Assertion -Id 'Z0.ERR' -What 'cleanup raised an error' -Status 'FAIL' -Evidence (Protect-Text ($_.Exception.Message + ' ' + $_.ScriptStackTrace) 800)
    }
}

$exit = 1
try {
    $cleanupFailure = @($script:Assertions | Where-Object {
        $_.status -in @('FAIL', 'BLOCKED') -and $_.id -match '^Z([0-5]|7)(\.|$)'
    }).Count -gt 0
    $workRootGone = -not (Test-Path -LiteralPath $script:WorkRoot)
    $sessionGone = $true
    if ($script:ServerStartedByScript) {
        try {
            $sessionGone = @((Get-Sessions) | Where-Object { (Get-Field $_ 'name') -eq $script:SessionName }).Count -eq 0
        } catch { $sessionGone = $false }
    }
    $runStatus = if ($KeepResources) { 'preserved' } elseif ($workRootGone -and $sessionGone -and -not $cleanupFailure) { 'complete' } else { 'incomplete' }
    Save-AcceptanceRunManifest -Phase 'finished' -Status $runStatus
    try {
        Prune-RunReportArchive -Root $script:RunArchiveRoot -KeepCount 20 -RetentionDays 30
    } catch {
        Add-Assertion -Id 'Z8.ERR' -What 'report archive retention cleanup' -Status 'FAIL' -Evidence (Protect-Text $_.Exception.Message 500)
        $runStatus = 'incomplete'
        Save-AcceptanceRunManifest -Phase 'finished' -Status $runStatus
    }
    $exit = Write-Report
    Save-AcceptanceRunManifest -Phase 'finished' -Status $runStatus
} finally {
    if ($script:RunLockStream) {
        Exit-AcceptanceRunLock -Lock $script:RunLockStream
        $script:RunLockStream = $null
    }
}
Write-Host ''
Write-Host "passed=$($script:Passed) failed=$($script:Failures) blocked=$($script:Blocked)"
Write-Host "report: $($script:ReportJson)"
if ($script:Blocked -gt 0) {
    Write-Host "BLOCKED items (could not be driven with the supported public APIs):" -ForegroundColor Yellow
    foreach ($item in $script:Undriven) { Write-Host ("  - {0} {1}: {2}" -f $item.id, $item.what, $item.evidence) -ForegroundColor Yellow }
}
if ($script:Failures -gt 0) {
    Write-Host "FAILED items:" -ForegroundColor Red
    foreach ($record in @($script:Assertions | Where-Object { $_.status -eq 'FAIL' })) {
        Write-Host ("  - {0} {1}: {2}" -f $record.id, $record.what, $record.evidence) -ForegroundColor Red
    }
}
exit $exit
