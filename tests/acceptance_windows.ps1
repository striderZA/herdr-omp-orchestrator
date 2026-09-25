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
      8.  The read-only board renders the tasks and mutates nothing.
      9.  A dirty checkout refuses cleanup; a live worker refuses cleanup.
      10. A timed-out `agent prompt` is treated as ambiguous: the script
          inspects authoritative state before deciding, and never resubmits.
      11. A Herdr server restart preserves identities, inbox, stages and
          worktrees, re-runs the plugin's startup hook, and the restored OMP
          session still has its active role tools.
      12. The session is MATERIALIZED before any resume hint is trusted.
      13. Everything the run created is removed by exact identity, and nothing
          else in the machine was touched.

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
      powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\tests\acceptance_windows.ps1 -KeepResources
#>
[CmdletBinding()]
param(
    # Multiplies every bounded wait. Raise it on a slow machine or when the
    # configured OMP model is under load.
    [double]$TimeoutScale = 1.0,
    # Leave everything this run created in place for inspection: the isolated
    # session and its server, its workspaces and panes, the temporary repository
    # and checkouts, and the rows in the plugin's state store. The script then
    # reports what is still there instead of pretending it was removed.
    [switch]$KeepResources,
    # Do everything else, but keep the plugin's state rows (the store keeps the
    # task facts that explain what the run observed).
    [switch]$SkipStateCleanup
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

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
$script:ReportDir = $script:WorkRoot
$script:ReportJson = Join-Path $script:ReportDir 'report.json'
$script:ReportText = Join-Path $script:ReportDir 'report.txt'
$script:ServerLog = Join-Path $script:WorkRoot 'herdr-server.log'
$script:ServerErr = Join-Path $script:WorkRoot 'herdr-server.err'
$script:ConfigPath = Join-Path $script:WorkRoot 'herdr-config.toml'
$script:RepoPath = Join-Path $script:WorkRoot 'repo'
$script:WorktreesRoot = Join-Path $script:WorkRoot 'worktrees'
$script:StateHelper = Join-Path $script:WorkRoot 'state_query.py'

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
$script:Assertions = New-Object System.Collections.ArrayList
$script:Failures = 0
$script:Blocked = 0
$script:Passed = 0
$script:ServerProcess = $null
$script:ServerStartedByScript = $false
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
    $argv = @()
    if (-not $NoSessionScope) { $argv += @('--session', $script:SessionName) }
    $argv += $Arguments
    $env = @{ 'NO_COLOR' = '1' }
    foreach ($k in $ExtraEnvironment.Keys) { $env[$k] = $ExtraEnvironment[$k] }
    return Invoke-Native -File $script:HerdrExe -Arguments $argv -Environment $env `
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
    if ($result.ExitCode -ne 0 -and -not $result.Stdout) { return @() }
    $parsed = $null
    try { $parsed = $result.Stdout | ConvertFrom-Json } catch { return @() }
    if ($null -eq $parsed) { return @() }
    return @(Get-Field $parsed 'sessions' @())
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
        [string]$Command = ''
    )
    $ok = -not [string]::IsNullOrWhiteSpace([string]$Value)
    return Assert-True -Id $Id -What $What -Condition $ok -TaskId $TaskId -Command $Command `
        -Evidence "value=[$([string]$Value)]"
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
        [string]$Evidence = '',
        [string]$Command = ''
    )
    [void]$script:Undriven.Add([pscustomobject]@{ id = $Id; what = $What; evidence = Protect-Text $Evidence 400 })
    return Add-Assertion -Id $Id -What $What -Status 'BLOCKED' -Command $Command -Evidence $Evidence
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

function Test-Below {
    # True when Path is inside Root (both normalised), which is how the script
    # proves a checkout belongs to this run before touching it.
    param([string]$Root, [string]$Path)
    $r = (ConvertTo-NormalPath $Root)
    $p = (ConvertTo-NormalPath $Path)
    if ($r -eq '' -or $p -eq '') { return $false }
    return ($p -ieq $r) -or $p.StartsWith($r + '\', [System.StringComparison]::OrdinalIgnoreCase)
}

function Write-Step {
    param([string]$Text)
    Write-Host ''
    Write-Host ("=== {0}" -f $Text) -ForegroundColor White
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
    $start = Get-Date
    $last = $null
    while (((Get-Date) - $start).TotalSeconds -lt $TimeoutSeconds) {
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

TABLES = ("projects", "tasks", "inbox", "ready_sessions")


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
    return [fact(r) for r in cur.execute("SELECT rowid AS _rowid, * FROM " + table)]


def identity(table, row):
    if table == "projects":
        return row.get("key")
    if table == "tasks":
        return row.get("id")
    if table == "inbox":
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
    hits = []
    for t in data["tasks"]:
        if t.get("project_key") == key:
            continue
        f = t.get("fact") or {}
        if t.get("id") in task_ids or f.get("OMPSessionID") in session_ids:
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
    param($Fact, [switch]$MoveSensitive)
    $fields = if ($MoveSensitive) {
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

function Test-OmpSessionMaterialized {
    # A path-shaped session reference is materialized only when the file exists;
    # an id-shaped reference cannot be proven from here (the store records the
    # on-disk flag the runtime announced).
    param([string]$Reference)
    if ([string]::IsNullOrWhiteSpace($Reference)) { return $false }
    if ($Reference -match '\.jsonl$' -or $Reference -match '[\\/]') { return (Test-Path -LiteralPath $Reference) }
    return $false
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
        [double]$TimeoutSeconds = 180
    )
    $result = Invoke-Herdr -Arguments @('plugin', 'action', 'invoke', $ActionId, '--plugin', $script:PluginId) `
        -ExtraEnvironment $PaneContext -TimeoutSeconds $TimeoutSeconds
    if ($result.ExitCode -ne 0) {
        Report-CommandFailure -AssertId "action/$ActionId" -Arguments @('plugin', 'action', 'invoke', $ActionId) -Result $result
        return $null
    }
    try { return ($result.Stdout | ConvertFrom-Json).result } catch {
        Report-CommandFailure -AssertId "action/$ActionId" -Arguments @('plugin', 'action', 'invoke', $ActionId) -Result $result
        return $null
    }
}

function Get-PluginLogEntry {
    param([Parameter(Mandatory)][string]$LogId, [double]$TimeoutSeconds = 60)
    $wait = Wait-Until -TimeoutSeconds $TimeoutSeconds -IntervalSeconds 1 -What "plugin log $LogId" -Probe {
        $json = Get-HerdrJson -Arguments @('plugin', 'log', 'list', '--plugin', $script:PluginId, '--limit', '40')
        $entry = @($json.Result.logs | Where-Object { $_.log_id -eq $LogId })
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
    param([Parameter(Mandatory)][string]$LogId, [double]$TimeoutSeconds = 60)
    $entry = Get-PluginLogEntry -LogId $LogId -TimeoutSeconds $TimeoutSeconds
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
        still hosts that exact session. This is the only public seam that can
        express the negative cases (a `partial` report, a cleanup refusal) which
        the OMP tool schemas themselves reject before the plugin is ever called;
        every positive path in this script goes through the real OMP agents.
    #>
    param(
        [Parameter(Mandatory)][string]$LaunchPaneId,
        [Parameter(Mandatory)][string]$Method,
        [hashtable]$Params = @{},
        [switch]$Raw
    )
    $handshake = Get-ReadySession -LaunchPaneId $LaunchPaneId
    if (-not $handshake) {
        return [pscustomobject]@{ Ok = $false; Error = [pscustomobject]@{ code = 'no_handshake'; message = "no ready_sessions row for launch pane $LaunchPaneId" }; Result = $null; Invocation = $null }
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
        return [pscustomobject]@{ Ok = $false; Error = [pscustomobject]@{ code = 'bridge_protocol'; message = (Protect-Text $result.Stderr 300) }; Result = $null; Invocation = $result }
    }
    return [pscustomobject]@{
        Ok         = ($answer.ok -eq $true)
        Result     = $answer.result
        Error      = $answer.error
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
    try { return [pscustomobject]@{ Found = $true; Code = ''; Agent = ($result.Stdout | ConvertFrom-Json).result.agent; Raw = $result } } catch {
        return [pscustomobject]@{ Found = $false; Code = 'unparsable'; Agent = $null; Raw = $result }
    }
}

function Get-WorktreeList {
    $json = Get-HerdrJson -Arguments @('worktree', 'list', '--cwd', $script:RepoPath)
    return $json.Result
}

function Get-Snapshot {
    $json = Get-HerdrJson -Arguments @('api', 'snapshot')
    return $json.Result.snapshot
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
        if ($status -in @('idle', 'done')) { return $status }
        return $null
    }
    if (-not $idle.Ok) {
        $screen = Get-PaneScreen -PaneId $PaneId -Source 'visible' -Lines 60
        return [pscustomobject]@{ ExitCode = -2; Blocked = $true; CommandLine = "herdr agent prompt $PaneId <primary prompt>"; Stderr = ''; Stdout = ''; Screen = $screen }
    }
    $result = Invoke-AgentPrompt -PaneId $PaneId -Text $Text -TimeoutSeconds $TimeoutSeconds
    $code = Get-HerdrErrorCode -Result $result
    return [pscustomobject]@{ ExitCode = $result.ExitCode; Blocked = ($code -eq 'agent_blocked'); Code = $code; CommandLine = $result.CommandLine; Stderr = $result.Stderr; Stdout = $result.Stdout; Screen = '' }
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
    $plugin = @($pluginList.Result.plugins | Where-Object { (Get-Field $_ 'plugin_id') -eq $script:PluginId })
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
    Assert-True -Id 'P1.10' -What 'the plugin state store exists (the plugin has run before)' -Condition (Test-Path -LiteralPath $script:StateDb) `
        -Evidence "state db expected at $($script:StateDb)"

    $script:Python = Initialize-StateTools
    Assert-True -Id 'P1.11' -What 'a SQLite-capable interpreter is available for store assertions' -Condition ([bool]$script:Python) `
        -Evidence "python=$(if ($script:Python) { $script:Python } else { 'not found' })"
    if ($script:Python) {
        $baseline = Get-StateDump
        Add-Assertion -Id 'P1.12' -What 'store baseline captured' -Status 'INFO' `
            -Evidence ("projects={0} tasks={1} inbox={2} ready_sessions={3}" -f @($baseline.tables.projects).Count, @($baseline.tables.tasks).Count, @($baseline.tables.inbox).Count, @($baseline.tables.ready_sessions).Count)
    }

    $defaultSession = Get-HerdrJson -Arguments @('api', 'snapshot') -NoSessionScope
    $script:DefaultSessionBefore = Get-FocusProjection -Snapshot $defaultSession.Result.snapshot
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
    $plugin = @($json.Result.plugins | Where-Object { (Get-Field $_ 'plugin_id') -eq $script:PluginId })
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
        $found = @($actions.Result.actions | Where-Object { (Get-Field $_ 'action_id') -eq $id })
        Assert-Equal -Id "L2.$id" -What "action '$id' is registered" -Expected 1 -Actual $found.Count
    }
    $bindAction = @($actions.Result.actions | Where-Object { (Get-Field $_ 'action_id') -eq 'bind' })
    if ($bindAction.Count -gt 0) {
        $contexts = @(Get-Field $bindAction[0] 'contexts' @())
        Assert-Equal -Id 'L2.bind-contexts' -What 'bind declares pane and workspace contexts' -Expected 2 -Actual $contexts.Count -Evidence ($contexts -join ',')
    }
}

# ---------------------------------------------------------------------------
# Phase 4: isolated Herdr session
# ---------------------------------------------------------------------------

function Start-RunServer {
    $psi = New-Object System.Diagnostics.ProcessStartInfo
    $psi.FileName = $script:HerdrExe
    $psi.Arguments = (@('--session', $script:SessionName, 'server') | ForEach-Object { ConvertTo-ArgumentLiteral $_ }) -join ' '
    $psi.UseShellExecute = $false
    $psi.CreateNoWindow = $true
    $psi.RedirectStandardOutput = $true
    $psi.RedirectStandardError = $true
    $psi.WorkingDirectory = $script:RepoRoot
    $psi.EnvironmentVariables['HERDR_CONFIG_PATH'] = $script:ConfigPath
    $proc = New-Object System.Diagnostics.Process
    $proc.StartInfo = $psi
    [void]$proc.Start()
    # Drain both streams continuously so the child never blocks on a full pipe.
    $proc.add_OutputDataReceived({ param($s, $e) if ($e.Data) { Add-Content -LiteralPath $script:ServerLog -Value $e.Data } })
    $proc.add_ErrorDataReceived({ param($s, $e) if ($e.Data) { Add-Content -LiteralPath $script:ServerErr -Value $e.Data } })
    $proc.BeginOutputReadLine()
    $proc.BeginErrorReadLine()
    $script:ServerProcess = $proc
    $script:ServerStartedByScript = $true
    return $proc
}

function Invoke-IsolatedSession {
    Write-Step "Phase 4: isolated Herdr session '$($script:SessionName)' under $($script:WorkRoot)"
    [void](New-Item -ItemType Directory -Path $script:WorkRoot -Force)
    $script:TempRootCreated = $true
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
    Add-Assertion -Id 'S1.3b' -What 'the run session socket captured (it must differ from the default session)' -Status 'INFO' -Evidence $script:SessionSocket

    $sessionStatus = Invoke-Herdr -Arguments @('status', 'server')
    $script:SessionServerVersion = ((@([string]$sessionStatus.Stdout -split "`n") | Where-Object { $_ -match '^version:' } | Select-Object -First 1) -replace '^version:\s*', '').Trim()
    Add-Assertion -Id 'S1.4' -What 'isolated server version (may differ from the CLI)' -Status 'INFO' -Evidence $script:SessionServerVersion

    $default = @(Get-Sessions | Where-Object { (Get-Field $_ 'name') -eq 'default' })
    Assert-True -Id 'S1.5' -What "the user's default session is still running (never stopped/restarted by this run)" `
        -Condition ($default.Count -gt 0 -and (Get-Flag $default[0] 'running')) -Evidence (Protect-Text (Get-Json $default) 400)

    $workspaces = Get-HerdrJson -Arguments @('workspace', 'list')
    $script:InitialWorkspaceCount = @($workspaces.Result.workspaces).Count
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
    [void](New-Item -ItemType Directory -Path $script:RepoPath -Force)
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

    $created = Get-HerdrJson -Arguments @('workspace', 'create', '--cwd', $script:RepoPath, '--no-focus', '--label', "accept-$($script:RunId)")
    $ws = Get-Field $created.Result 'workspace'
    $pane = Get-Field $created.Result 'root_pane'
    $tab = Get-Field $created.Result 'tab'
    $script:PrimaryWorkspace = [string](Get-Field $ws 'workspace_id')
    $script:PrimaryPane = [string](Get-Field $pane 'pane_id')
    $script:PrimaryTab = [string](Get-Field $tab 'tab_id')
    [void]$script:CreatedWorkspaces.Add($script:PrimaryWorkspace)
    [void]$script:CreatedPanes.Add($script:PrimaryPane)
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
    $invoke = Invoke-PluginAction -ActionId 'setup'
    Assert-True -Id 'U1.1' -What 'plugin action invoke setup returns an invocation envelope' -Condition ($null -ne $invoke) -Evidence (Protect-Text (Get-Json $invoke) 500)
    if (-not $invoke) { throw 'the setup action could not be invoked' }
    $logId = [string](Get-Field (Get-Field $invoke 'log') 'log_id')
    Assert-NonEmpty -Id 'U1.2' -What 'the setup invocation is recorded in the plugin log' -Value $logId

    $output = Get-ActionOutput -LogId $logId -TimeoutSeconds 90
    Assert-True -Id 'U1.3' -What 'the setup command finished' -Condition ($null -ne $output) -Evidence (Protect-Text (Get-Json $output) 400)
    if (-not $output) { throw 'the setup action never finished' }
    Assert-Equal -Id 'U1.4' -What 'the setup command exited 0' -Expected 0 -Actual (Get-Field $output.Entry 'exit_code')
    Assert-Equal -Id 'U1.5' -What 'the setup command reports success' -Expected 'succeeded' -Actual (Get-Field $output.Entry 'status')
    $payload = $output.Payload
    Assert-True -Id 'U1.6' -What 'setup answered with ok=true' -Condition (Get-Flag $payload 'ok') -Evidence (Protect-Text (Get-Json $payload) 400)
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
    Assert-Equal -Id 'U4.2' -What 'the orchestrator extension is byte-identical after a repeated setup (idempotent)' -Expected $script:ExtensionFileHashBefore -Actual (Get-FileHashSafe $reported)
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
    param([Parameter(Mandatory)]$Invoke, [double]$TimeoutSeconds = 120)
    $logId = [string](Get-Field (Get-Field $Invoke 'log') 'log_id')
    return (Get-ActionOutput -LogId $logId -TimeoutSeconds $TimeoutSeconds)
}

function Invoke-Bind {
    Write-Step 'Phase 7: bind the primary (real action, real pane context)'
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
    $resolved = Get-Field $invoke 'context'
    Assert-SamePath -Id 'N1.2' -What 'the invocation context names the temp repository' -Expected $script:RepoPath -Actual ([string](Get-Field $resolved 'focused_pane_cwd'))
    Assert-Equal -Id 'N1.3' -What 'the invocation context names the temp workspace' -Expected $script:PrimaryWorkspace -Actual ([string](Get-Field $resolved 'workspace_id'))
    Assert-Equal -Id 'N1.4' -What 'the invocation context names the temp pane' -Expected $script:PrimaryPane -Actual ([string](Get-Field $resolved 'focused_pane_id'))

    $output = Get-BindPayload -Invoke $invoke
    Assert-True -Id 'N2.1' -What 'the bind command finished' -Condition ($null -ne $output) -Evidence (Protect-Text (Get-Json $output) 400)
    if (-not $output) { throw 'the bind action never finished' }
    Assert-Equal -Id 'N2.2' -What 'the bind command exited 0' -Expected 0 -Actual (Get-Field $output.Entry 'exit_code')
    $payload = $output.Payload
    Assert-True -Id 'N2.3' -What 'bind answered with ok=true' -Condition (Get-Flag $payload 'ok') -Evidence (Protect-Text (Get-Json $payload) 500)
    if (-not (Get-Flag $payload 'ok')) { throw 'bind failed; the rest of the workflow cannot be driven' }

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
        if ($row.Count -gt 0) {
            Assert-Equal -Id 'N4.2' -What 'the stored session identity matches the action answer' -Expected ([string](Get-Field $project 'OMPSessionID')) -Actual ([string](Get-Field (Get-Field $row[0] 'fact') 'OMPSessionID'))
            Assert-Equal -Id 'N4.3' -What 'the stored agent name matches the action answer' -Expected ([string](Get-Field $project 'AgentName')) -Actual ([string](Get-Field (Get-Field $row[0] 'fact') 'AgentName'))
        }
        $handshake = Get-ReadySession -LaunchPaneId $script:PrimaryPane
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
    $onDiskHint = Get-Flag $hint 'onDisk'
    Assert-True -Id 'N6.2' -What 'the hint either offers a resume command or states why it cannot' `
        -Condition (($resume -ne '') -xor ($unavailable -eq '')) -Evidence "resume=[$resume] unavailable=[$unavailable]"
    Assert-True -Id 'N6.3' -What 'a resume hint is only offered for a session that is on disk (materialization gate)' `
        -Condition ((-not $onDiskHint) -or ($resume -ne '')) -Evidence "onDisk=$onDiskHint resume=[$resume]"
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
    Assert-Equal -Id 'N7.1' -What 'the primary accepts and completes a real prompt' -Expected 0 -Actual $result.ExitCode -Command $result.CommandLine -Evidence ("code=[$($result.Code)] blocked=[$($result.Blocked)] " + (Protect-Text $result.Stderr 300))
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

    $context = @{
        'HERDR_ENV'          = '1'
        'HERDR_PANE_ID'      = $script:PrimaryPane
        'HERDR_WORKSPACE_ID' = $script:PrimaryWorkspace
        'HERDR_TAB_ID'       = $script:PrimaryTab
    }
    $invoke = Invoke-PluginAction -ActionId 'bind' -PaneContext $context -TimeoutSeconds 240
    $output = Get-BindPayload -Invoke $invoke
    Assert-Equal -Id 'N8.1' -What 're-binding an existing project exits 0 (reopen path)' -Expected 0 -Actual (Get-Field $output.Entry 'exit_code')
    $payload = $output.Payload
    Assert-True -Id 'N8.2' -What 're-bind answered ok=true' -Condition (Get-Flag $payload 'ok') -Evidence (Protect-Text (Get-Json $payload) 500)
    $project2 = Get-Field $payload 'project'
    Assert-Equal -Id 'N8.3' -What 're-bind keeps the recorded OMP session (identity retained)' -Expected ([string](Get-Field $script:Project 'OMPSessionID')) -Actual ([string](Get-Field $project2 'OMPSessionID'))
    Assert-Equal -Id 'N8.4' -What 're-bind keeps the primary agent name' -Expected ([string](Get-Field $script:Project 'AgentName')) -Actual ([string](Get-Field $project2 'AgentName'))
    Assert-SamePath -Id 'N8.5' -What 're-bind keeps the project key' -Expected ([string](Get-Field $script:Project 'Key')) -Actual ([string](Get-Field $project2 'Key'))

    $hint = Get-Field $payload 'session'
    $script:ResumeHintAfterTurn = $hint
    Assert-True -Id 'N8.6' -What 'the materialized session now advertises a resume command' -Condition (Get-Flag $hint 'onDisk') -Evidence (Protect-Text (Get-Json $hint) 400)
    $handshake = Get-ReadySession -LaunchPaneId $script:PrimaryPane
    $expectedResume = 'omp --resume=' + [string](Get-Field $handshake 'omp_session_id')
    Assert-Equal -Id 'N8.7' -What 'the resume hint names the exact announced session id' -Expected $expectedResume -Actual ([string](Get-Field $hint 'resume'))
    $handshakeAfter = Get-ReadySession -LaunchPaneId $script:PrimaryPane
    $fileAfter = Test-HandshakeMaterialized -Handshake $handshakeAfter
    if ($null -eq $fileAfter) {
        Assert-True -Id 'N8.8' -What 'a resume command is only offered together with a materialized session (reference form checkable only when Herdr reports a path)' `
            -Condition (Get-Flag $hint 'onDisk') -Evidence ("file=[$(Get-Field $handshakeAfter 'omp_session_file')] onDisk=[$(Get-Field $hint 'onDisk')]")
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
3. Call the orch_question tool once with text "ACCEPT-Q-A: should a.js keep the existing export style?" and then stop and wait: the answer arrives later as a user message. Wait for it before doing anything else.
4. After the answer arrives, call the orch_report tool with kind "result", summary "ACCEPT-A: a.js now also exports subtract", branch = the branch printed by ``git branch --show-current``, evidence = ["commit <the full sha from git rev-parse HEAD>"], checks = ["git log -1 --format=%H"], risk "ACCEPT-RISK-A: none".
Never merge, never remove the branch and never remove the checkout.
"@
    $script:BriefB = @"
$($script:MarkerPrefix)TASK-B. You are in your own Git worktree of a disposable test repository. Do exactly this and nothing else:
1. Edit b.js so it also exports ``function multiply(a, b) { return a * b; }``, keeping the existing content. Do NOT commit it: leave the change uncommitted in your worktree.
2. Call the ask tool exactly once with a single question: "ACCEPT-Q-DIALOG: which contract should b.js use?" and exactly two options: "keep the current contract" and "change the contract". Then stop and wait for that dialog to be answered.
3. After the dialog is answered, call the orch_report tool with kind "result", summary "ACCEPT-B: b.js now also exports multiply and the change is left uncommitted", branch = the branch printed by ``git branch --show-current``, evidence = ["uncommitted change to b.js", "git status --porcelain lists it"], checks = ["git status --porcelain"], risk "ACCEPT-RISK-B: the change is uncommitted".
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
    $result = Invoke-PrimaryPrompt -PaneId $script:PrimaryPane -Text $prompt -TimeoutSeconds 420
    Assert-True -Id 'D1.1' -What 'the primary accepts the dispatch prompt' -Condition ($result.ExitCode -eq 0) -Command $result.CommandLine -Evidence ("code=[$($result.Code)] blocked=[$($result.Blocked)] " + (Protect-Text $result.Stderr 300))
    if ($result.Blocked) { Block-Item -Id 'D1.1b' -What 'the primary was blocked when the dispatch prompt was due' -Evidence (Protect-Text $result.Screen 500) }
    if ($result.ExitCode -ne 0) {
        Block-Item -Id 'D1.1c' -What 'the dispatch prompt could not be submitted, so the worker phases cannot run' -Evidence (Protect-Text ($result.Stdout + $result.Stderr) 500)
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
        Assert-True -Id "D4.live/$id" -What "Herdr reports the worker agent of $id on its recorded pane" -Condition $live.Found -Evidence (Protect-Text (Get-Json $live.Agent) 400)
        if ($live.Found) {
            $ref = Get-Field $live.Agent 'agent_session'
            Assert-Equal -Id "D4.session/$id" -What "Herdr publishes $id's recorded OMP session" -Expected ([string](Get-Field $fact 'OMPSessionID')) -Actual ([string](Get-Field $ref 'value')) -CaseInsensitive
            Assert-SamePath -Id "D4.cwd/$id" -What "the worker agent of $id works in its own checkout" -Expected ([string](Get-Field $fact 'WorktreePath')) -Actual ([string](Get-Field $live.Agent 'cwd'))
        }
        $handshake = Get-ReadySession -LaunchPaneId ([string](Get-Field $fact 'LaunchPaneID'))
        Assert-True -Id "D4.handshake/$id" -What "the worker's OMP extension announced and was confirmed" -Condition ($null -ne $handshake -and [int](Get-Field $handshake 'verified') -eq 1) -Evidence (Protect-Text (Get-Json $handshake) 400)
        if ($script:Python -and $handshake) {
            $materialized = Test-HandshakeMaterialized -Handshake $handshake
            $onDisk = ([int](Get-Field $handshake 'omp_session_on_disk') -eq 1)
            if ($null -eq $materialized) {
                Add-Assertion -Id "D4.materialized/$id" -What "the worker's session reference is id-only, so materialization cannot be checked" -Status 'INFO' -Evidence "onDisk=$onDisk"
            } else {
                Assert-True -Id "D4.materialized/$id" -What "the worker's on-disk flag agrees with its session file" -Condition ($materialized -eq $onDisk) -Evidence "exists=$materialized onDisk=$onDisk"
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
    Assert-True -Id 'PR1.1' -What "the pane the bridge will be told about really hosts $id (replayed caller identity is real)" `
        -Condition ($live.Found -and ([string](Get-Field (Get-Field $live.Agent 'agent_session') 'value') -ieq $caller.Session)) `
        -TaskId $id -Evidence (Protect-Text (Get-Json $live.Agent) 400)

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

    $preview = Invoke-BridgeAs -LaunchPaneId $caller.LaunchPane -Method 'cleanup_preview' -Params @{ taskId = $id }
    Assert-True -Id 'PR5.1' -What 'cleanup_preview answers for a running task' -Condition $preview.Ok -TaskId $id -Evidence (Protect-Text (Get-Json $preview.Error) 300)
    if ($preview.Ok) {
        Assert-True -Id 'PR5.2' -What 'a running task is not removable' -Condition (-not (Get-Flag $preview.Result 'removable')) -TaskId $id -Evidence (Protect-Text (Get-Json $preview.Result) 400)
        Assert-True -Id 'PR5.3' -What 'the preview names the exact recorded checkout path' -Condition (Test-SamePath -A (Get-Field $preview.Result 'path') -B ([string](Get-Field $fact 'WorktreePath'))) -TaskId $id
    }
    $cleanup = Invoke-BridgeAs -LaunchPaneId $caller.LaunchPane -Method 'cleanup' -Params @{ taskId = $id }
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
        $primaryScreen = Get-PaneScreen -PaneId $script:PrimaryPane -Source 'recent-unwrapped' -Lines 200
        $delivered = ($primaryScreen -match 'orchestrator:') -and ($primaryScreen -match 'ACCEPT-Q-A')
        if ($delivered) {
            Assert-True -Id 'Q2.7' -What 'the worker note was prompted into the primary session (no independent inbox UI)' -Condition $true
        } else {
            Block-Item -Id 'Q2.7' -What 'the worker note was prompted into the primary session' `
                -Evidence ("the question is durable in the store, but it was not visible in the primary's scrollback at inspection time: " + (Protect-Text $primaryScreen 400))
        }
    }

    $focusBefore = Get-FocusProjection -Snapshot (Get-Snapshot)
    $prompt = 'ACCEPT-Q-A-RELAY. A worker asked an ordinary question. Call the orch_answer tool with taskId "' + $idA + '" and text "ACCEPT-A1: keep the existing export style." Then reply with the word relayed.'
    $result = Invoke-PrimaryPrompt -PaneId $script:PrimaryPane -Text $prompt -TimeoutSeconds 300
    Assert-True -Id 'Q3.1' -What 'the primary accepted the answer-relay prompt' -Condition ($result.ExitCode -eq 0) -TaskId $idA -Command $result.CommandLine -Evidence ("code=[$($result.Code)] blocked=[$($result.Blocked)] " + (Protect-Text $result.Stderr 300))
    if ($result.Blocked) { Block-Item -Id 'Q3.1b' -What 'the primary was blocked when the answer relay was due' -Evidence (Protect-Text $result.Screen 500) }
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
        Assert-Equal -Id 'Q3.3' -What 'the answered task returns to Running' -Expected 'running' -Actual (Get-TaskStage -Fact $script:TaskA) -TaskId $idA
    }
    $workerScreen = Get-PaneScreen -PaneId ([string](Get-Field $script:TaskA 'PublicPaneID')) -Source 'recent-unwrapped' -Lines 200
    if ($workerScreen -match 'ACCEPT-A1') {
        Assert-True -Id 'Q3.4' -What 'the answer reached the worker terminal through its current public pane' -Condition $true
    } else {
        Block-Item -Id 'Q3.4' -What 'the answer reached the worker terminal' `
            -Evidence ("the store cleared the question but the answer text was not in the worker scrollback: " + (Protect-Text $workerScreen 400))
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

function Confirm-PrimaryModal {
    <#
        Waits for the primary's own confirmation UI, reports the on-screen
        choices as evidence, answers it with real keys, and waits for it to
        close. The keys are chosen from what the screen actually shows.
    #>
    param([Parameter(Mandatory)][string]$Title, [double]$TimeoutSeconds = 180)
    $wait = Wait-Until -TimeoutSeconds $TimeoutSeconds -IntervalSeconds 3 -What "confirmation '$Title'" -Probe {
        $screen = Get-PaneScreen -PaneId $script:PrimaryPane -Source 'visible' -Lines 60
        if ($screen -match [regex]::Escape($Title)) { return $screen }
        return $null
    }
    if (-not $wait.Ok) {
        $screen = Get-PaneScreen -PaneId $script:PrimaryPane -Source 'visible' -Lines 60
        return [pscustomobject]@{ Observed = $false; Screen = $screen; Keys = @(); Closed = $false }
    }
    $screen = $wait.Value
    $keys = @('enter')
    if ($screen -match '(?i)\[y/N\]|\(y/n\)|y\s*=\s*yes') { $keys = @('y') }
    $sent = Send-AgentKeys -PaneId $script:PrimaryPane -Keys $keys
    $closed = Wait-Until -TimeoutSeconds 120 -IntervalSeconds 3 -What 'confirmation closed' -Probe {
        $now = Get-PaneScreen -PaneId $script:PrimaryPane -Source 'visible' -Lines 60
        if ($now -notmatch [regex]::Escape($Title)) { return $true }
        return $false
    }
    if (-not $closed.Ok -and $keys -ne @('enter')) {
        [void](Send-AgentKeys -PaneId $script:PrimaryPane -Keys @('enter'))
        $closed = Wait-Until -TimeoutSeconds 60 -IntervalSeconds 3 -What 'confirmation closed' -Probe {
            $now = Get-PaneScreen -PaneId $script:PrimaryPane -Source 'visible' -Lines 60
            if ($now -notmatch [regex]::Escape($Title)) { return $true }
            return $false
        }
        $keys = $keys + 'enter'
    }
    $script:PrimarySendKeysUsed += ($keys -join ' ')
    return [pscustomobject]@{ Observed = $true; Screen = $screen; Keys = $keys; Closed = $closed.Ok; SendResult = $sent }
}

function Invoke-BlockedDialog {
    Write-Step 'Phase 12: real blocked full-screen dialog with a visible preview'
    $taskB = Get-TaskByTitle -Pattern 'ACCEPT B'
    Assert-True -Id 'K1.1' -What 'task B is identifiable by its exact title' -Condition ($null -ne $taskB) -Evidence (Protect-Text (Get-Json @($script:Tasks | ForEach-Object { Get-Field (Get-Field $_ 'fact') 'Title' })) 300)
    if (-not $taskB) { Block-Item -Id 'K1.2' -What 'blocked full-screen dialog' -Evidence 'task B was not dispatched'; return }
    $script:TaskB = Get-Field $taskB 'fact'
    $idB = [string](Get-Field $script:TaskB 'ID')
    $paneB = [string](Get-Field $script:TaskB 'PublicPaneID')

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
    Assert-True -Id 'K3.1' -What 'the primary-side dialog preview answers' -Condition $preview.Ok -TaskId $idB -Evidence (Protect-Text (Get-Json $preview.Error) 300)
    if ($preview.Ok) {
        Assert-True -Id 'K3.2' -What 'the preview reports the worker blocked' -Condition (Get-Flag $preview.Result 'blocked') -TaskId $idB
        Assert-Match -Id 'K3.3' -What 'the preview carries the on-screen dialog text' -Pattern 'ACCEPT-Q-DIALOG' -Actual ([string](Get-Field $preview.Result 'dialog')) -TaskId $idB
        Assert-NonEmpty -Id 'K3.4' -What 'the preview carries a digest that ties an approval to this dialog' -Value ([string](Get-Field $preview.Result 'digest')) -TaskId $idB
        Assert-Equal -Id 'K3.5' -What 'the preview names the exact current public pane' -Expected $paneB -Actual ([string](Get-Field $preview.Result 'publicPaneId')) -TaskId $idB
    }

    $prompt = 'ACCEPT-DIALOG. Worker ' + $idB + ' is blocked on a full-screen dialog. Call the orch_approve_dialog tool with taskId "' + $idB + '" and keys ["enter"], and confirm the approval exactly as the tool asks. Then reply with the word approved.'
    $result = Invoke-PrimaryPrompt -PaneId $script:PrimaryPane -Text $prompt -TimeoutSeconds 240
    Assert-True -Id 'K4.1' -What 'the primary accepted the approval prompt' -Condition ($result.ExitCode -eq 0) -TaskId $idB -Command $result.CommandLine -Evidence ("code=[$($result.Code)] blocked=[$($result.Blocked)] " + (Protect-Text $result.Stderr 300))
    if ($result.Blocked) { Block-Item -Id 'K4.1b' -What 'the primary was blocked when the dialog approval was due' -Evidence (Protect-Text $result.Screen 500) }

    $modal = Confirm-PrimaryModal -Title 'Approve worker choice' -TimeoutSeconds 240
    if ($modal.Observed) {
        Assert-True -Id 'K5.1' -What 'the primary showed the user the dialog preview and the exact keys before acting' -Condition $true -TaskId $idB
        Assert-Match -Id 'K5.2' -What 'the confirmation quotes the task id' -Pattern ([regex]::Escape($idB)) -Actual $modal.Screen -TaskId $idB
        Add-Assertion -Id 'K5.3' -What "confirmation answered with keys [$($modal.Keys -join ' ')]" -Status $(if ($modal.Closed) { 'PASS' } else { 'FAIL' }) -TaskId $idB `
            -Command "herdr agent send-keys $($script:PrimaryPane) $($modal.Keys -join ' ')" `
            -Evidence (Protect-Text $modal.Screen 800)
    } else {
        Block-Item -Id 'K5.1' -What 'the primary asked the user to confirm the exact keys' `
            -Evidence ("no 'Approve worker choice' confirmation was visible in the primary pane within the timeout; last screen: " + (Protect-Text $modal.Screen 800)) `
            -Command "herdr agent read $($script:PrimaryPane) --source visible"
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
        $afterScreen = Get-PaneScreen -PaneId $paneB -Source 'visible' -Lines 60
        Assert-True -Id 'K6.2' -What 'the approved dialog is no longer on screen' -Condition ($afterScreen -notmatch 'ACCEPT-Q-DIALOG') -TaskId $idB -Evidence (Protect-Text $afterScreen 500)
    } else {
        $status = [string](Get-Field (Get-LiveAgent -Target $paneB).Agent 'agent_status')
        Block-Item -Id 'K6.1' -What 'the approved keys answered the worker dialog' `
            -Evidence ("worker B is still $status after the approval; visible screen: " + (Protect-Text (Get-PaneScreen -PaneId $paneB -Source 'visible' -Lines 40) 600))
    }

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
    $moved = (($move.Stdout | ConvertFrom-Json).result).move_result
    $newPane = [string](Get-Field (Get-Field $moved 'pane') 'pane_id')
    $newWorkspace = [string](Get-Field (Get-Field $moved 'pane') 'workspace_id')
    [void]$script:CreatedPanes.Add($newPane)
    [void]$script:CreatedWorkspaces.Add($newWorkspace)

    Assert-Equal -Id 'M2.1' -What 'the move reports the launch-time pane id as previous_pane_id' -Expected $pane -Actual ([string](Get-Field $moved 'previous_pane_id'))
    Assert-True -Id 'M2.2' -What 'the move produced a new public pane id' -Condition ($newPane -ne '' -and $newPane -ne $pane) -Evidence "new=$newPane old=$pane"
    Assert-Equal -Id 'M2.3' -What 'the moved pane keeps its terminal' -Expected ([string](Get-Field $fact 'TerminalID')) -Actual ([string](Get-Field (Get-Field $moved 'pane') 'terminal_id'))
    Assert-Equal -Id 'M2.4' -What 'the move closed the source (worktree) workspace' -Expected $sourceWorkspace -Actual ([string](Get-Field $moved 'closed_workspace_id'))
    Assert-NonEmpty -Id 'M2.5' -What 'the move closed the source tab' -Value ([string](Get-Field $moved 'closed_tab_id'))
    Assert-SamePath -Id 'M2.6' -What 'the moved pane still works in the recorded checkout' -Expected ([string](Get-Field $fact 'WorktreePath')) -Actual ([string](Get-Field (Get-Field $moved 'pane') 'cwd'))

    $gone = Invoke-Herdr -Arguments @('workspace', 'get', $sourceWorkspace)
    Assert-True -Id 'M3.1' -What 'the closed source workspace can no longer be read' -Condition ($gone.ExitCode -ne 0) -Evidence (Protect-Text $gone.Stderr 300)
    $list = Get-HerdrJson -Arguments @('workspace', 'list')
    $stillThere = @($list.Result.workspaces | Where-Object { (Get-Field $_ 'workspace_id') -eq $sourceWorkspace })
    Assert-Equal -Id 'M3.2' -What 'the source workspace is absent from the live list' -Expected 0 -Actual $stillThere.Count
    [void]$script:CreatedWorkspaces.Remove($sourceWorkspace)

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
    Assert-True -Id 'M6.1' -What 'reporting still resolves for the moved worker through its launch pane' -Condition $resolve.Ok -TaskId $idB -Evidence (Protect-Text (Get-Json $resolve.Error) 300)
    if ($resolve.Ok) {
        Assert-True -Id 'M6.2' -What 'resolve still binds the task to the worker role' -Condition ((Get-Flag $resolve.Result 'bound') -and ([string](Get-Field $resolve.Result 'role')) -eq 'worker') -TaskId $idB -Evidence (Protect-Text (Get-Json $resolve.Result) 400)
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
    Write-Step 'Phase 14: two complete reports with evidence and checks'
    foreach ($pair in @(@{ Name = 'A'; Fact = $script:TaskA }, @{ Name = 'B'; Fact = $script:TaskB })) {
        $fact = $pair.Fact
        $id = [string](Get-Field $fact 'ID')
        $wait = Wait-ForReport -TaskId $id -TimeoutSeconds 600
        Assert-True -Id "W1.$($pair.Name)" -What "worker $($pair.Name) submitted a final result" -Condition $wait.Ok -TaskId $id `
            -Evidence ("after {0:N0}s: phase=[{1}] attention=[{2}]" -f $wait.Elapsed, [string](Get-Field (Get-Field (Get-TaskById -Id $id) 'fact' $null) 'Phase'), [string](Get-Field (Get-Field (Get-TaskById -Id $id) 'fact' $null) 'Attention'))
        if (-not $wait.Ok) {
            $screen = Get-PaneScreen -PaneId ([string](Get-Field $fact 'PublicPaneID')) -Source 'recent-unwrapped' -Lines 120
            Block-Item -Id "W1.$($pair.Name)-screen" -What "worker $($pair.Name) terminal content at timeout" -Evidence (Protect-Text $screen 800)
            continue
        }
        $reported = $wait.Value
        if ($pair.Name -eq 'A') { $script:TaskA = $reported } else { $script:TaskB = $reported }
        $report = Get-Field $reported 'Report'
        Assert-Equal -Id "W2.$($pair.Name)-stage" -What "task $id is Ready for review" -Expected 'ready_for_review' -Actual (Get-TaskStage -Fact $reported) -TaskId $id
        Assert-Equal -Id "W2.$($pair.Name)-branch" -What "the report names the branch this task owns" -Expected ([string](Get-Field $reported 'Branch')) -Actual ([string](Get-Field $report 'Branch')) -TaskId $id
        Assert-NonEmpty -Id "W2.$($pair.Name)-summary" -What 'the report carries a summary' -Value ([string](Get-Field $report 'Summary')) -TaskId $id
        Assert-True -Id "W2.$($pair.Name)-evidence" -What 'the report carries changed-file/commit evidence' -Condition (@(Get-Field $report 'Evidence' @()).Count -ge 1) -TaskId $id -Evidence (Protect-Text (Get-Json (Get-Field $report 'Evidence' @())) 300)
        Assert-True -Id "W2.$($pair.Name)-checks" -What 'the report carries the checks that ran' -Condition (@(Get-Field $report 'Checks' @()).Count -ge 1) -TaskId $id -Evidence (Protect-Text (Get-Json (Get-Field $report 'Checks' @())) 300)
        Assert-NonEmpty -Id "W2.$($pair.Name)-risk" -What 'the report states its risk' -Value ([string](Get-Field $report 'Risk')) -TaskId $id
        Assert-True -Id "W2.$($pair.Name)-clean-attention" -What 'a final report clears the worker question attention' -Condition ([string]::IsNullOrWhiteSpace([string](Get-Field $reported 'Attention'))) -TaskId $id
        $dump = Get-StateDump
        $note = @($dump.tables.inbox | Where-Object { (Get-Field $_ 'task_id') -eq $id -and (Get-Field $_ 'kind') -eq 'result' })
        Assert-Equal -Id "W3.$($pair.Name)-note" -What 'exactly one result note was recorded for the primary' -Expected 1 -Actual $note.Count -TaskId $id
    }

    # The reported evidence must be true of the real checkout.
    if ($script:TaskA) {
        $pathA = [string](Get-Field $script:TaskA 'WorktreePath')
        $headA = (Invoke-Git -Arguments @('rev-parse', 'HEAD') -Directory $pathA).Stdout.Trim()
        Assert-True -Id 'W4.A-changed' -What 'worker A really committed a change on its branch' -Condition ($headA -ne $script:BaseRevision) -TaskId ([string](Get-Field $script:TaskA 'ID')) -Evidence "head=$headA base=$($script:BaseRevision)"
        $report = Get-Field $script:TaskA 'Report'
        $joined = (@(Get-Field $report 'Evidence' @()) + @(Get-Field $report 'Checks' @())) -join ' '
        Assert-True -Id 'W4.A-evidence-true' -What "worker A's evidence names its real commit or the file it changed" `
            -Condition (($joined -match [regex]::Escape($headA.Substring(0, 12))) -or ($joined -match 'a\.js')) -TaskId ([string](Get-Field $script:TaskA 'ID')) -Evidence (Protect-Text $joined 400)
    }
    if ($script:TaskB) {
        $pathB = [string](Get-Field $script:TaskB 'WorktreePath')
        $statusB = (Invoke-Git -Arguments @('status', '--porcelain') -Directory $pathB).Stdout.Trim()
        Assert-True -Id 'W4.B-dirty' -What "worker B's change is really present and uncommitted (the dirt the cleanup refusal relies on)" -Condition ($statusB -ne '') -TaskId ([string](Get-Field $script:TaskB 'ID')) -Evidence (Protect-Text $statusB 300)
    }

    $primaryScreen = Get-PaneScreen -PaneId $script:PrimaryPane -Source 'recent-unwrapped' -Lines 300
    if ($primaryScreen -match 'worker result' -or $primaryScreen -match 'orchestrator:') {
        Assert-True -Id 'W5.1' -What 'the result notes were delivered into the primary session by the plugin' -Condition $true
    } else {
        Block-Item -Id 'W5.1' -What 'the result notes were delivered into the primary session by the plugin' `
            -Evidence ("the notes are durable in the store but not visible in the primary scrollback at inspection time: " + (Protect-Text $primaryScreen 500))
    }
}

# ---------------------------------------------------------------------------
# Phase 15: the read-only board
# ---------------------------------------------------------------------------

function Invoke-Board {
    Write-Step 'Phase 15: the read-only board renders the tasks'
    $tasksBefore = @(Get-ProjectTasks -Key (Get-Field $script:Project 'Key'))
    $jsonBefore = (@($tasksBefore | Sort-Object { Get-Field $_ 'id' } | ForEach-Object { Get-Json $_ }) -join '|')
    $focusBefore = Get-FocusProjection -Snapshot (Get-Snapshot)

    $open = Invoke-Herdr -Arguments @('plugin', 'pane', 'open', '--plugin', $script:PluginId, '--entrypoint', 'board', '--workspace', $script:PrimaryWorkspace, '--no-focus') -TimeoutSeconds 120
    Assert-Equal -Id 'A1.1' -What 'opening the board pane exits 0' -Expected 0 -Actual $open.ExitCode -Command $open.CommandLine -Evidence (Protect-Text $open.Stderr 400)
    if ($open.ExitCode -ne 0) { Block-Item -Id 'A1.2' -What 'board pane' -Evidence (Protect-Text $open.Stderr 400); return }
    $opened = ($open.Stdout | ConvertFrom-Json).result
    $boardPane = [string](Get-Field (Get-Field $opened 'pane') 'pane_id')
    Assert-NonEmpty -Id 'A1.2' -What 'the board pane has an id' -Value $boardPane
    [void]$script:CreatedPanes.Add($boardPane)
    $script:BoardPane = $boardPane
    Assert-Equal -Id 'A1.3' -What 'the board pane is the declared plugin entrypoint' -Expected 'board' -Actual ([string](Get-Field $opened 'entrypoint'))
    Assert-Equal -Id 'A1.4' -What 'the board pane belongs to this plugin' -Expected $script:PluginId -Actual ([string](Get-Field $opened 'plugin_id'))

    $screen = ''
    $rendered = Wait-Until -TimeoutSeconds 60 -IntervalSeconds 3 -What 'board render' -Probe {
        $text = Get-PaneScreen -PaneId $boardPane -Source 'visible' -Lines 60 -Plain
        if ($text -match 'ACCEPT') { return $text }
        if ($text -match 'Queued|Ready for review|Needs input') { return $text }
        return $null
    }
    if ($rendered.Ok) { $screen = $rendered.Value }
    Add-Assertion -Id 'A2.1' -What 'the board rendered something' -Status $(if ($rendered.Ok) { 'PASS' } else { 'FAIL' }) -Evidence (Protect-Text $screen 900)
    foreach ($task in @($script:TaskA, $script:TaskB)) {
        if (-not $task) { continue }
        $id = [string](Get-Field $task 'ID')
        $branch = [string](Get-Field $task 'Branch')
        Assert-Match -Id "A2.id/$id" -What "the board shows task $id" -Pattern ([regex]::Escape($id)) -Actual $screen
        Assert-Match -Id "A2.branch/$id" -What "the board shows the branch of $id" -Pattern ([regex]::Escape($branch)) -Actual $screen
    }
    Assert-Match -Id 'A2.columns' -What 'the board shows the stage columns of this build' -Pattern 'Queued|Ready for review|Running|Needs input' -Actual $screen
    if ($screen -match 'Queued' -and $screen -match 'Done') {
        Assert-True -Id 'A2.wide' -What 'the wide layout shows all five stage columns' -Condition (($screen -match 'Running') -and ($screen -match 'Needs input') -and ($screen -match 'Ready for review')) -Evidence (Protect-Text $screen 500)
    }
    $boardPaneInfo = (Get-HerdrJson -Arguments @('pane', 'get', $boardPane)).Result
    Assert-True -Id 'A3.1' -What 'the board pane never stole focus' -Condition (-not (Get-Flag $boardPaneInfo 'focused')) -Evidence (Protect-Text (Get-Json $boardPaneInfo) 300)

    $tasksAfter = @(Get-ProjectTasks -Key (Get-Field $script:Project 'Key'))
    $jsonAfter = (@($tasksAfter | Sort-Object { Get-Field $_ 'id' } | ForEach-Object { Get-Json $_ }) -join '|')
    Assert-Equal -Id 'A4.1' -What 'reading the board mutated no task fact (revisions included)' -Expected $jsonBefore -Actual $jsonAfter

    $close = Invoke-Herdr -Arguments @('pane', 'close', $boardPane) -TimeoutSeconds 60
    Assert-Equal -Id 'A5.1' -What 'the board pane closes cleanly' -Expected 0 -Actual $close.ExitCode -Evidence (Protect-Text $close.Stderr 300)
    $gone = Invoke-Herdr -Arguments @('pane', 'get', $boardPane) -TimeoutSeconds 60
    Assert-True -Id 'A5.2' -What 'the board pane is gone' -Condition ($gone.ExitCode -ne 0) -Evidence (Protect-Text $gone.Stderr 200)
    [void]$script:CreatedPanes.Remove($boardPane)
    Assert-Equal -Id 'A5.3' -What 'closing the board left the session focus exactly where it was' -Expected $focusBefore -Actual (Get-FocusProjection -Snapshot (Get-Snapshot))
}

# ---------------------------------------------------------------------------
# Phase 16: the primary records the user's disposition
# ---------------------------------------------------------------------------

function Invoke-Reviews {
    Write-Step 'Phase 16: explicit dispositions (nothing merges automatically)'
    $idA = if ($script:TaskA) { [string](Get-Field $script:TaskA 'ID') } else { '' }
    $idB = if ($script:TaskB) { [string](Get-Field $script:TaskB 'ID') } else { '' }
    if (-not $idA -or -not $idB) { Block-Item -Id 'V0.0' -What 'dispositions' -Evidence 'one or both tasks never reported'; return }
    $dispositionA = "ACCEPT-DISP-A-$($script:RunId): keep the branch, do not merge"
    $dispositionB = "ACCEPT-DISP-B-$($script:RunId): keep the branch, do not merge"
    $prompt = 'ACCEPT-REVIEW. Call the orch_review tool twice: first with taskId "' + $idA + '" and disposition "' + $dispositionA + '", then with taskId "' + $idB + '" and disposition "' + $dispositionB + '". Then reply with the word reviewed.'
    $result = Invoke-PrimaryPrompt -PaneId $script:PrimaryPane -Text $prompt -TimeoutSeconds 300
    Assert-True -Id 'V1.1' -What 'the primary accepted the review prompt' -Condition ($result.ExitCode -eq 0) -Command $result.CommandLine -Evidence ("code=[$($result.Code)] blocked=[$($result.Blocked)] " + (Protect-Text $result.Stderr 300))
    if ($result.Blocked) { Block-Item -Id 'V1.1b' -What 'the primary was blocked when the review was due' -Evidence (Protect-Text $result.Screen 500) }

    $done = Wait-Until -TimeoutSeconds 300 -IntervalSeconds 8 -What 'dispositions recorded' -Probe {
        $a = Get-TaskById -Id $idA
        $b = Get-TaskById -Id $idB
        if (-not $a -or -not $b) { return $null }
        if ([string]::IsNullOrWhiteSpace([string](Get-Field (Get-Field $a 'fact') 'Disposition'))) { return $null }
        if ([string]::IsNullOrWhiteSpace([string](Get-Field (Get-Field $b 'fact') 'Disposition'))) { return $null }
        return @($a, $b)
    }
    Assert-True -Id 'V1.2' -What 'both tasks carry a recorded disposition' -Condition $done.Ok `
        -Evidence ("after {0:N0}s" -f $done.Elapsed)
    foreach ($id in @($idA, $idB)) {
        $row = Get-TaskById -Id $id
        $fact = Get-Field $row 'fact'
        Assert-Equal -Id "V2.$id-stage" -What "task $id is Done only because a disposition was recorded" -Expected 'done' -Actual (Get-TaskStage -Fact $fact) -TaskId $id
        Assert-True -Id "V2.$id-kept" -What "task $id keeps its report, branch and checkout provenance" `
            -Condition (($null -ne (Get-Field $fact 'Report' $null)) -and ([string](Get-Field $fact 'Branch') -ne '') -and ([string](Get-Field $fact 'WorktreePath') -ne '')) -TaskId $id
    }

    $branches = (Invoke-Git -Arguments @('branch', '--format=%(refname:short)') -Directory $script:RepoPath).Stdout
    foreach ($id in @($idA, $idB)) {
        $fact = Get-Field (Get-TaskById -Id $id) 'fact'
        $branch = [string](Get-Field $fact 'Branch')
        Assert-Match -Id "V3.$id-branch" -What "branch $branch is preserved after the review" -Pattern ([regex]::Escape($branch)) -Actual $branches -TaskId $id
        Assert-True -Id "V3.$id-checkout" -What "the checkout of $id still exists on disk" -Condition (Test-Path -LiteralPath ([string](Get-Field $fact 'WorktreePath'))) -TaskId $id
    }
    $mainHead = (Invoke-Git -Arguments @('rev-parse', 'HEAD') -Directory $script:RepoPath).Stdout.Trim()
    Assert-Equal -Id 'V4.1' -What 'nothing was merged into the base checkout (HEAD unchanged)' -Expected $script:BaseRevision -Actual $mainHead
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
    $path = [string](Get-Field $script:TaskB 'WorktreePath')
    $branch = [string](Get-Field $script:TaskB 'Branch')
    $primaryLaunch = [string](Get-Field $script:Project 'LaunchPaneID')

    $status = (Invoke-Git -Arguments @('status', '--porcelain') -Directory $path).Stdout.Trim()
    Assert-True -Id 'C1.1' -What 'the moved worker checkout is dirty before the cleanup tests' -Condition ($status -ne '') -TaskId $idB -Evidence (Protect-Text $status 300)

    $preview = Invoke-BridgeAs -LaunchPaneId $primaryLaunch -Method 'cleanup_preview' -Params @{ taskId = $idB }
    Assert-True -Id 'C2.1' -What 'cleanup_preview answers for the reviewed done task' -Condition $preview.Ok -TaskId $idB -Evidence (Protect-Text (Get-Json $preview.Error) 300)
    if ($preview.Ok) {
        Assert-True -Id 'C2.2' -What 'the preview recognises the task as reviewed and decided' -Condition (Get-Flag $preview.Result 'reviewed') -TaskId $idB
        Assert-True -Id 'C2.3' -What 'the preview reports the checkout as not clean' -Condition (-not (Get-Flag $preview.Result 'clean')) -TaskId $idB
        Assert-True -Id 'C2.4' -What 'the preview refuses removal' -Condition (-not (Get-Flag $preview.Result 'removable')) -TaskId $idB
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
    $dead = Wait-Until -TimeoutSeconds 120 -IntervalSeconds 5 -What 'worker gone' -Probe {
        $live = Get-LiveAgent -Target $pane
        if (-not $live.Found) { return $true }
        if ([string]::IsNullOrWhiteSpace([string](Get-Field $live.Agent 'agent'))) { return $true }
        return $false
    }
    Assert-True -Id 'C6.2' -What 'no agent is left on the closed pane' -Condition $dead.Ok -TaskId $idB

    $cleanup3 = Invoke-BridgeAs -LaunchPaneId $primaryLaunch -Method 'cleanup' -Params @{ taskId = $idB }
    Add-Assertion -Id 'C7.1' -What 'the plugin’s removal path never asks Herdr to force a removal' -Status 'INFO' `
        -Evidence 'internal/coord/report.go (removeCheckout) calls herdr WorktreeRemove(...,false); internal/herdr/client.go builds `worktree remove --workspace <id>` with no --force; a forced removal is refused by verifyRemoval'
    Assert-True -Id 'C7.2' -What 'the branch survives whatever the removal did' -Condition ((Invoke-Git -Arguments @('branch', '--format=%(refname:short)') -Directory $script:RepoPath).Stdout -match [regex]::Escape($branch)) -TaskId $idB
    if ($cleanup3.Ok) {
        Assert-True -Id 'C7.3' -What 'the removal reports the recorded checkout as removed' -Condition ((Get-Flag $cleanup3.Result 'removed') -and (Test-SamePath -A (Get-Field $cleanup3.Result 'path') -B $path)) -TaskId $idB -Evidence (Protect-Text (Get-Json $cleanup3.Result) 400)
        Assert-True -Id 'C7.4' -What 'the removed checkout is gone from disk' -Condition (-not (Test-Path -LiteralPath $path)) -TaskId $idB
        $after = Get-Field (Get-TaskById -Id $idB) 'fact'
        Assert-True -Id 'C7.5' -What 'the task keeps its branch and path as provenance after removal' -Condition (([string](Get-Field $after 'Branch') -eq $branch) -and ([string](Get-Field $after 'WorktreePath') -eq $path)) -TaskId $idB
    } else {
        Add-Assertion -Id 'C7.3' -What 'cleanup after a cross-workspace move was refused by Herdr' -Status 'INFO' `
            -Evidence (Protect-Text (Get-Json $cleanup3.Error) 500)
        $script:MovedCleanupRefused = $cleanup3.Error
        Assert-True -Id 'C7.4' -What 'the refusal left the checkout in place (nothing was removed)' -Condition (Test-Path -LiteralPath $path) -TaskId $idB
    }
}

# ---------------------------------------------------------------------------
# Phase 18: a timed-out prompt is ambiguous - inspect before retrying
# ---------------------------------------------------------------------------

function Invoke-AmbiguousPrompt {
    Write-Step 'Phase 18: an ambiguous agent prompt timeout, inspected before any retry'
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
    # The identity half of the durable facts: what must survive a restart.
    param($Facts)
    $parts = New-Object System.Collections.ArrayList
    $p = $Facts.Project
    if ($p) {
        foreach ($field in @('Key', 'AgentName', 'LaunchPaneID', 'OMPSessionID', 'TerminalID', 'RepoWorkspaceID')) {
            [void]$parts.Add("project/$field=$([string](Get-Field $p $field))")
        }
    }
    foreach ($row in @($Facts.Tasks)) {
        $f = Get-Field $row 'fact'
        foreach ($field in @('ID', 'ProjectKey', 'Title', 'Branch', 'BaseRevision', 'WorktreePath', 'WorktreeWorkspaceID', 'LaunchPaneID', 'OMPSessionID', 'Phase', 'Blocker', 'Attention', 'Disposition', 'CurrentWorkspaceID')) {
            [void]$parts.Add("task/$([string](Get-Field $f 'ID'))/$field=$([string](Get-Field $f $field))")
        }
        $report = Get-Field $f 'Report' $null
        [void]$parts.Add("task/$([string](Get-Field $f 'ID'))/Report=$((Get-Json $report))")
    }
    foreach ($row in @($Facts.Inbox)) {
        [void]$parts.Add("inbox/$([string](Get-Field $row 'id'))/$([string](Get-Field $row 'kind'))/$([string](Get-Field $row 'delivery_state'))")
    }
    return ($parts -join ';')
}

function Invoke-Restart {
    Write-Step 'Phase 19: restart the isolated Herdr server and prove recovery'
    $before = Get-ProjectFacts
    $projectionBefore = Get-IdentityProjection -Facts $before
    Add-Assertion -Id 'T1.1' -What 'identity projection captured before the restart' -Status 'INFO' -Evidence (Protect-Text $projectionBefore 800)

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

    $startup = Wait-Until -TimeoutSeconds 120 -IntervalSeconds 5 -What 'startup hook' -Probe {
        $json = Get-HerdrJson -Arguments @('plugin', 'log', 'list', '--plugin', $script:PluginId, '--limit', '50')
        $entries = @($json.Result.logs | Where-Object { (Get-Field $_ 'event') -eq 'startup' })
        if ($entries.Count -lt 2) { return $null }
        $newest = $entries | Sort-Object { [int64](Get-Field $_ 'started_unix_ms') } | Select-Object -Last 1
        if ((Get-Field $newest 'status') -eq 'running') { return $null }
        return $newest
    }
    Assert-True -Id 'T4.1' -What 'the plugin startup hook ran again after the restart' -Condition $startup.Ok -Evidence (Protect-Text (Get-Json $startup.Value) 400)
    if ($startup.Ok) {
        Assert-Equal -Id 'T4.2' -What 'the startup reconciliation succeeded' -Expected 'succeeded' -Actual ([string](Get-Field $startup.Value 'status'))
        Assert-Match -Id 'T4.3' -What 'the reconciliation reported ok' -Pattern '"ok":true' -Actual ([string](Get-Field $startup.Value 'stdout'))
    }

    $after = Get-ProjectFacts
    $projectionAfter = Get-IdentityProjection -Facts $after
    Assert-Equal -Id 'T5.1' -What 'identities, stages, reports, dispositions and inbox rows survive the restart' -Expected $projectionBefore -Actual $projectionAfter
    Assert-Equal -Id 'T5.2' -What 'the inbox rows are all still there' -Expected @($before.Inbox).Count -Actual @($after.Inbox).Count

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
            Assert-Equal -Id 'T7.2' -What 'the restored agent still carries the recorded OMP session' -Expected ([string](Get-Field $script:Project 'OMPSessionID')) -Actual ([string](Get-Field $ref 'value')) -CaseInsensitive
        } else {
            Block-Item -Id 'T7.2' -What 'the restored agent still carries the recorded OMP session' -Evidence (Protect-Text (Get-Json $agent.Raw) 300)
        }
        $script:RestoredPrimaryPane = $restoredPane
    }

    if ($script:RestoredPrimaryPane) {
        $idA = if ($script:TaskA) { [string](Get-Field $script:TaskA 'ID') } else { '' }
        if ($idA) {
            $probe = 'ACCEPT-TOOLS-RESTORED. Call the orch_review tool with taskId "' + $idA + '" and disposition "ACCEPT-TOOLS-RESTORED-' + $script:RunId + '". If that tool is not available in this session, reply exactly NOTOOLHERE and do nothing else.'
            $result = Invoke-PrimaryPrompt -PaneId $script:RestoredPrimaryPane -Text $probe -TimeoutSeconds 300
            Assert-True -Id 'T8.1' -What 'the restored primary session accepts a prompt' -Condition ($result.ExitCode -eq 0) -Command $result.CommandLine -Evidence ("code=[$($result.Code)] blocked=[$($result.Blocked)] " + (Protect-Text $result.Stderr 300))
            $toolRan = Wait-Until -TimeoutSeconds 240 -IntervalSeconds 8 -What 'restored role tool' -Probe {
                $fact = Get-Field (Get-TaskById -Id $idA) 'fact'
                if ([string](Get-Field $fact 'Disposition') -match 'ACCEPT-TOOLS-RESTORED') { return $fact }
                return $null
            }
            Assert-True -Id 'T8.2' -What 'the restored OMP session still has its active role tools (the disposition was recorded by the primary tool)' -Condition $toolRan.Ok -TaskId $idA `
                -Evidence (Protect-Text (Get-PaneScreen -PaneId $script:RestoredPrimaryPane -Source 'recent-unwrapped' -Lines 80) 600)
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
    $output = Get-BindPayload -Invoke $invoke
    Assert-Equal -Id 'T9.1' -What 're-binding after the restart exits 0' -Expected 0 -Actual (Get-Field $output.Entry 'exit_code')
    if ($output.Payload) {
        $p = Get-Field $output.Payload 'project'
        Assert-SamePath -Id 'T9.2' -What 'the project key survives the restart' -Expected ([string](Get-Field $before.Project 'Key')) -Actual ([string](Get-Field $p 'Key'))
        Assert-Equal -Id 'T9.3' -What 'the primary agent name survives the restart' -Expected ([string](Get-Field $before.Project 'AgentName')) -Actual ([string](Get-Field $p 'AgentName'))
        Assert-Equal -Id 'T9.4' -What 'the OMP session identity survives the restart' -Expected ([string](Get-Field $before.Project 'OMPSessionID')) -Actual ([string](Get-Field $p 'OMPSessionID'))
        $hint = Get-Field $output.Payload 'session'
        $resume = [string](Get-Field $hint 'resume')
        Assert-True -Id 'T10.1' -What 'the resume hint after the restart is still gated on a materialized session' `
            -Condition ((-not (Get-Flag $hint 'onDisk')) -or ($resume -ne '')) -Evidence (Protect-Text (Get-Json $hint) 400)
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
            $live = @($list.Result.workspaces | ForEach-Object { [string](Get-Field $_ 'workspace_id') })
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

        # 2. Close the panes and workspaces this run created. With
        # -KeepResources the whole session is left standing for inspection.
        if (-not $KeepResources) {
        foreach ($pane in @($script:CreatedPanes | Select-Object -Unique)) {
            [void](Invoke-Herdr -Arguments @('pane', 'close', $pane) -TimeoutSeconds 60)
        }
        $script:CreatedPanes.Clear()
        foreach ($workspace in @($script:CreatedWorkspaces | Select-Object -Unique)) {
            [void](Invoke-Herdr -Arguments @('workspace', 'close', $workspace) -TimeoutSeconds 60)
        }
        $script:CreatedWorkspaces.Clear()
        $remaining = Get-HerdrJson -Arguments @('workspace', 'list') -TimeoutSeconds 60
        Assert-Equal -Id 'Z2.1' -What 'every workspace this run created is closed again' -Expected 0 -Actual @($remaining.Result.workspaces).Count `
            -Evidence (Protect-Text (Get-Json $remaining.Result.workspaces) 400)
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
                Assert-True -Id 'Z4.2' -What 'the state cleanup deleted rows only for this run project key' -Condition (Get-Flag $deleted 'ok') -Evidence (Protect-Text (Get-Json $deleted) 400)
                Assert-True -Id 'Z4.3' -What 'every other row in the store is byte-identical after the delete' -Condition (Get-Flag $deleted 'others_identical') `
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
        if ($script:ServerProcess -and -not $script:ServerProcess.HasExited) {
            try { $script:ServerProcess.Kill() } catch { }
        }
        $delete = Invoke-Herdr -Arguments @('session', 'delete', $script:SessionName) -NoSessionScope -TimeoutSeconds 60
        Assert-Equal -Id 'Z5.2' -What 'the isolated session is deleted' -Expected 0 -Actual $delete.ExitCode -Evidence (Protect-Text $delete.Stderr 300)
        Assert-Equal -Id 'Z5.3' -What 'this run session is gone from the session list' -Expected 0 -Actual @(Get-Sessions | Where-Object { (Get-Field $_ 'name') -eq $script:SessionName }).Count
    }

    # 6. Prove nothing of the user's was changed.
    $default = Get-HerdrJson -Arguments @('api', 'snapshot') -NoSessionScope
    $defaultProjection = Get-FocusProjection -Snapshot $default.Result.snapshot
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
    Assert-True -Id 'Z6.1c' -What 'this run used its own server, not the default session socket' `
        -Condition ($script:SessionSocket -and $script:DefaultSessionRecord -and ((ConvertTo-NormalPath $script:SessionSocket) -ine (ConvertTo-NormalPath $script:DefaultSessionRecord.Socket))) `
        -Evidence "run=$(Protect-Text $script:SessionSocket 200) default=$(Protect-Text $script:DefaultSessionRecord.Socket 200)"

    $pluginList = Get-HerdrJson -Arguments @('plugin', 'list', '--json', '--plugin', $script:PluginId) -NoSessionScope
    $plugin = @($pluginList.Result.plugins | Where-Object { (Get-Field $_ 'plugin_id') -eq $script:PluginId })
    Assert-Equal -Id 'Z6.2' -What 'the plugin is still linked exactly once (this run never unlinked it)' -Expected 1 -Actual $plugin.Count
    if ($plugin.Count -gt 0) {
        Assert-SamePath -Id 'Z6.3' -What 'the plugin root is unchanged' -Expected $script:PluginRegistrationBefore.Root -Actual ([string](Get-Field $plugin[0] 'plugin_root'))
        Assert-SamePath -Id 'Z6.4' -What 'the manifest path is unchanged' -Expected $script:PluginRegistrationBefore.Manifest -Actual ([string](Get-Field $plugin[0] 'manifest_path'))
        Assert-Equal -Id 'Z6.5' -What 'the plugin is still enabled' -Expected $script:PluginRegistrationBefore.Enabled -Actual (Get-Flag $plugin[0] 'enabled')
    }
    $extDir = Get-EffectiveExtensionDir
    Assert-Equal -Id 'Z6.6' -What 'this run left the installed orchestrator extension byte-identical' -Expected $script:ExtensionFileHashBefore -Actual (Get-FileHashSafe (Join-Path $extDir 'herdr-orchestrator.ts'))
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
        if (Test-Path -LiteralPath $script:WorkRoot) {
            Get-ChildItem -LiteralPath $script:WorkRoot -Force | Where-Object { $_.Name -notin @('report.json', 'report.txt') } | ForEach-Object {
                Remove-Item -LiteralPath $_.FullName -Recurse -Force -ErrorAction SilentlyContinue
            }
        }
        $left = @(Get-ChildItem -LiteralPath $script:WorkRoot -Force -ErrorAction SilentlyContinue | Where-Object { $_.Name -notin @('report.json', 'report.txt') })
        Assert-Equal -Id 'Z7.1' -What 'the temporary run root holds nothing but the report' -Expected 0 -Actual $left.Count -Evidence (Protect-Text (Get-Json @($left | ForEach-Object { $_.Name })) 300)
    } else {
        Add-Assertion -Id 'Z7.1' -What 'resources were intentionally kept (-KeepResources)' -Status 'INFO' -Evidence $script:WorkRoot
    }
}

function Write-Report {
    $summary = [pscustomobject]@{
        runId        = $script:RunId
        session      = $script:SessionName
        startedAt    = $script:StartedAt
        finishedAt   = (Get-Date).ToUniversalTime().ToString('o')
        passed       = $script:Passed
        failed       = $script:Failures
        blocked      = $script:Blocked
        exitCode     = 0
        projectKey   = if ($script:Project) { [string](Get-Field $script:Project 'Key') } else { '' }
        serverVersion = $script:SessionServerVersion
        assertions   = @($script:Assertions)
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
        & $Body
    } catch {
        Add-Assertion -Id "$Id.ERR" -What "$Name raised an error" -Status 'FAIL' -Evidence (Protect-Text ($_.Exception.Message + ' ' + $_.ScriptStackTrace) 800)
        if ($Critical) { throw }
    }
}

$script:StartedAt = (Get-Date).ToUniversalTime().ToString('o')
$script:Aborted = $false
New-Item -ItemType Directory -Path $script:WorkRoot -Force | Out-Null
$script:TempRootCreated = $true
Set-Content -LiteralPath $script:ReportText -Value "acceptance run $($script:RunId) starting at $($script:StartedAt)" -Encoding UTF8
Write-Host "herdr OMP orchestrator acceptance run $($script:RunId)" -ForegroundColor White
Write-Host "plugin root : $($script:RepoRoot)"
Write-Host "run root    : $($script:WorkRoot)"
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
    Step-Phase -Id 'N7' -Name 'primary turn' -Body { Invoke-PrimaryTurn }
    Step-Phase -Id 'D0' -Name 'dispatch' -Critical -Body { Invoke-Dispatch }
    Step-Phase -Id 'PR0' -Name 'partial report rejection' -Body { Invoke-PartialReportRejection }
    Step-Phase -Id 'Q0' -Name 'question relay' -Body { Invoke-QuestionRelay }
    Step-Phase -Id 'K0' -Name 'blocked dialog' -Body { Invoke-BlockedDialog }
    Step-Phase -Id 'M0' -Name 'pane move' -Body { Invoke-PaneMove }
    Step-Phase -Id 'W0' -Name 'worker reports' -Body { Invoke-WorkerReports }
    Step-Phase -Id 'A0' -Name 'board' -Body { Invoke-Board }
    Step-Phase -Id 'V0' -Name 'reviews' -Body { Invoke-Reviews }
    Step-Phase -Id 'C0' -Name 'cleanup refusals' -Body { Invoke-CleanupRefusals }
    Step-Phase -Id 'G0' -Name 'ambiguous prompt' -Body { Invoke-AmbiguousPrompt }
    Step-Phase -Id 'T0' -Name 'restart' -Body { Invoke-Restart }
} catch {
    $script:Aborted = $true
    Add-Assertion -Id 'RUN.ABORT' -What 'the run stopped at a prerequisite failure' -Status 'FAIL' -Evidence (Protect-Text $_.Exception.Message 800)
} finally {
    try { Invoke-Cleanup } catch {
        Add-Assertion -Id 'Z0.ERR' -What 'cleanup raised an error' -Status 'FAIL' -Evidence (Protect-Text ($_.Exception.Message + ' ' + $_.ScriptStackTrace) 800)
    }
}

$exit = Write-Report
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
        Write-Host ("  - {0} {1}" -f $record.id, $record.what) -ForegroundColor Red
    }
}
exit $exit






