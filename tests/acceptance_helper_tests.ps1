#requires -Version 5.1
<#
.SYNOPSIS
    Helper-contract tests for tests/acceptance_windows.ps1.

.DESCRIPTION
    Runs the pure, environment-independent checks behind the acceptance script:
    MSVCRT argv quoting, stdin/timeout/stream handling and environment scoping of
    the native-command runner; path comparison and containment; redaction; the
    stage, task and cleanup-invariant projections; the strict JSON boolean reader
    and the two boolean assertions; the bridge-envelope shape rules; the
    resume-hint implication; the extension renderer; and the scoped SQLite store
    reader (plan, delete, "others byte-identical" proof, foreign-reference
    refusal) against a synthetic store built with the plugin's real DDL.

    This file needs no Herdr, no OMP session and no network. It extracts the
    helper half of tests/acceptance_windows.ps1 (everything before its driver
    block) into a temporary file, dot-sources it, and exercises it against
    synthetic data under $env:TEMP. It never starts Herdr, never touches a
    session, pane, worktree or the plugin's real state store, and the only
    real-machine file it reads is the extension installed for this OMP profile
    (read-only); that one check is skipped with an INFO line when absent.

.EXAMPLE
    powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\tests\acceptance_helper_tests.ps1
    -> prints "helper check failures: 0" and exits 0 when the helpers are sound.
#>
[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$script:HarnessPath = if ($PSCommandPath) { $PSCommandPath } elseif ($MyInvocation.MyCommand.Path) { $MyInvocation.MyCommand.Path } else { '' }
if (-not $script:HarnessPath) { throw 'run this file with -File so its location is known' }
$script:TestsDir = Split-Path -Parent $script:HarnessPath
$script:PluginRoot = Split-Path -Parent $script:TestsDir
$script:AcceptanceScript = Join-Path $script:TestsDir 'acceptance_windows.ps1'
if (-not (Test-Path -LiteralPath (Join-Path $script:PluginRoot 'herdr-plugin.toml')) -or -not (Test-Path -LiteralPath $script:AcceptanceScript)) {
    throw "the acceptance script and its plugin root could not be resolved from $($script:HarnessPath)"
}

# The helper half resolves the plugin root from the extracted copy's directory and
# the current location, so run from the plugin root.
$previousLocation = (Get-Location).Path
Set-Location -LiteralPath $script:PluginRoot

# Everything this harness writes lives under one uniquely named directory it owns,
# so a run can never collide with or delete a file it did not create, and cleanup
# is one removal.
$script:HarnessRoot = Join-Path $env:TEMP ("orch-acc-helpertest-" + ([guid]::NewGuid().ToString('N')).Substring(0, 6))
New-Item -ItemType Directory -Path $script:HarnessRoot -Force | Out-Null

# Everything below runs inside try/finally so the owned directory is removed on
# success, on a terminating error and on exit - an aborted run leaves nothing.
try {

$script:HelperCopy = Join-Path $script:HarnessRoot 'helpers.ps1'
$lines = Get-Content -LiteralPath $script:AcceptanceScript
$driver = @($lines | Select-String -Pattern '^\$script:StartedAt = \(Get-Date\)')
if ($driver.Count -eq 0) { throw "the driver block was not found in $($script:AcceptanceScript)" }
Set-Content -LiteralPath $script:HelperCopy -Value $lines[0..($driver[-1].LineNumber - 2)] -Encoding UTF8
. $script:HelperCopy

# The helper half writes assertion journal lines; keep them inside this owned dir.
$script:WorkRoot = $script:HarnessRoot
$script:ReportText = Join-Path $script:HarnessRoot 'report.txt'
Set-Content -LiteralPath $script:ReportText -Value 'helper tests' -Encoding UTF8

$fails = 0
$checks = 0
function Check {
    param([string]$Name, [bool]$Ok, [string]$Detail = '')
    $script:checks++
    if ($Ok) { Write-Host "ok   $Name" } else { Write-Host "FAIL $Name :: $Detail" -ForegroundColor Red; $script:fails++ }
}
# --- scenario selection ------------------------------------------------------
$phasePlanCommand = Get-Command Get-AcceptanceScenarioPhases -ErrorAction SilentlyContinue
Check 'single-worker scenario phase plan exists' ($null -ne $phasePlanCommand) 'missing scenario plan'
if ($phasePlanCommand) {
    $singlePhases = @(Get-AcceptanceScenarioPhases -Scenario 'SingleWorker')
    Check 'single-worker scenario keeps only one-worker lifecycle phases' `
        (($singlePhases -join ',') -ceq 'SW0,SW1,A0,V0') ($singlePhases -join ',')
    $fullPhases = @(Get-AcceptanceScenarioPhases -Scenario 'Full')
    Check 'full scenario retains the broad regression phases' `
        (($fullPhases -join ',') -ceq 'N7,D0,PR0,Q0,K0,M0,W0,A0,V0,TK0,C0,G0,T0') ($fullPhases -join ',')
    Check 'scenario timeout leaves unrelated bounded waits unchanged' `
        ((Get-ScenarioBoundedTimeoutSeconds -Requested 7) -eq 7) 'ordinary timeout changed'
    $script:ScenarioDeadline = (Get-Date).AddSeconds(2)
    $boundedTimeout = Get-ScenarioBoundedTimeoutSeconds -Requested 60
    Check 'scenario timeout clamps long operations to the remaining budget' `
        ($boundedTimeout -gt 0 -and $boundedTimeout -le 2) $boundedTimeout
    $script:ScenarioDeadline = (Get-Date).AddSeconds(-1)
    $expiredScenarioRefused = $false
    try { [void](Get-ScenarioBoundedTimeoutSeconds -Requested 60) } catch { $expiredScenarioRefused = $true }
    Check 'expired scenario budget stops new operations' $expiredScenarioRefused 'expired operation was allowed'
    $script:ScenarioDeadline = $null
}
$savedAssertions = $script:Assertions
$savedUndriven = $script:Undriven
$script:Assertions = New-Object System.Collections.ArrayList
$script:Undriven = New-Object System.Collections.ArrayList
$blockRecordsTask = $false
try {
    Block-Item -Id 'TDD.block-task' -What 'blocked task context survives assertion recording' `
        -TaskId 'orch_test1234' -Evidence 'precondition unavailable'
    $blockRecord = @($script:Assertions | Where-Object { $_.id -eq 'TDD.block-task' })
    $undrivenRecord = @($script:Undriven | Where-Object { $_.id -eq 'TDD.block-task' })
    $blockRecordsTask = ($blockRecord.Count -eq 1 -and $blockRecord[0].task -ceq 'orch_test1234' -and
        $undrivenRecord.Count -eq 1 -and $undrivenRecord[0].task -ceq 'orch_test1234')
} catch { }
Check 'Block-Item accepts and records task context' $blockRecordsTask 'TaskId rejected or not recorded'
$script:Assertions = $savedAssertions
$script:Undriven = $savedUndriven

# --- Invoke-Native argv integrity (spaces, quotes, newline, backslash) -------
$py = (Get-Command python).Source
$args = @('-c', 'import sys,json;print(json.dumps(sys.argv[1:]))', 'plain', 'with space', 'quote"inside', "new`nline", 'trail\', '', 'tab	inside')
$r = Invoke-Native -File $py -Arguments $args -TimeoutSeconds 60
Check 'Invoke-Native exit code' ($r.ExitCode -eq 0) ("exit=$($r.ExitCode) err=$($r.Stderr)")
$parsed = $null
try { $parsed = $r.Stdout | ConvertFrom-Json } catch { }
Check 'Invoke-Native stdout parses' ($null -ne $parsed) $r.Stdout
if ($parsed) {
    Check 'Invoke-Native argv[0]' ($parsed[0] -eq 'plain') $parsed[0]
    Check 'Invoke-Native argv[1] spaces' ($parsed[1] -eq 'with space') $parsed[1]
    Check 'Invoke-Native argv[2] quote' ($parsed[2] -eq 'quote"inside') $parsed[2]
    Check 'Invoke-Native argv[3] newline' ($parsed[3] -eq "new`nline") ("[$($parsed[3])]")
    Check 'Invoke-Native argv[4] trailing backslash' ($parsed[4] -eq 'trail\') $parsed[4]
    Check 'Invoke-Native argv[5] empty' ($parsed[5] -eq '') $parsed[5]
    Check 'Invoke-Native argv count' ($parsed.Count -eq 7) $parsed.Count
}

# --- stdin -------------------------------------------------------------------
$r2 = Invoke-Native -File $py -Arguments @('-c', 'import sys;print(sys.stdin.read())') -StandardInputText 'hello-json-body' -TimeoutSeconds 60
Check 'Invoke-Native writes stdin' ($r2.Stdout.Trim() -eq 'hello-json-body') $r2.Stdout

# --- timeout kills ----------------------------------------------------------
$r3 = Invoke-Native -File $py -Arguments @('-c', 'import time;time.sleep(30)') -TimeoutSeconds 3
Check 'Invoke-Native times out and kills' ($r3.TimedOut -and $r3.ExitCode -eq -1) "timedout=$($r3.TimedOut) exit=$($r3.ExitCode)"
$script:ScenarioDeadline = (Get-Date).AddSeconds(1)
$deadlineWatch = [System.Diagnostics.Stopwatch]::StartNew()
try {
    $deadlineResult = Invoke-Native -File $py -Arguments @('-c', 'import time;time.sleep(30)') -TimeoutSeconds 30
} finally {
    $script:ScenarioDeadline = $null
    $deadlineWatch.Stop()
}
Check 'scenario deadline kills a native command within the remaining budget' `
    ($deadlineResult.TimedOut -and $deadlineWatch.Elapsed.TotalSeconds -lt 5) `
    "timedout=$($deadlineResult.TimedOut) elapsed=$($deadlineWatch.Elapsed.TotalSeconds)"

# --- stderr separation ------------------------------------------------------
$r4 = Invoke-Native -File $py -Arguments @('-c', 'import sys;print("out");print("err",file=sys.stderr)') -TimeoutSeconds 60
Check 'Invoke-Native separates streams' (($r4.Stdout.Trim() -eq 'out') -and ($r4.Stderr.Trim() -eq 'err')) "out=[$($r4.Stdout)] err=[$($r4.Stderr)]"

# --- env removal ------------------------------------------------------------
$env:HERDR_PANE_ID = 'w1:p1'
$r5 = Invoke-Native -File $py -Arguments @('-c', 'import os;print(os.environ.get("HERDR_PANE_ID","absent"))') -RemoveEnvironment @('HERDR_PANE_ID') -TimeoutSeconds 60
Check 'Invoke-Native removes inherited env' ($r5.Stdout.Trim() -eq 'absent') $r5.Stdout
$r6 = Invoke-Native -File $py -Arguments @('-c', 'import os;print(os.environ.get("HERDR_PANE_ID","absent"))') -Environment @{ 'HERDR_PANE_ID' = 'w9:p9' } -RemoveEnvironment @('HERDR_PANE_ID') -TimeoutSeconds 60
Check 'Invoke-Native applies overrides after removal' ($r6.Stdout.Trim() -eq 'w9:p9') $r6.Stdout
Remove-Item env:HERDR_PANE_ID
$oldRunServerValues = @{
    HerdrExe = $script:HerdrExe
    SessionName = $script:SessionName
    RepoRoot = $script:RepoRoot
    ConfigPath = $script:ConfigPath
}
$oldCallerEnvironment = @{}
foreach ($key in $script:HerdrCallerEnvKeys) {
    $oldCallerEnvironment[$key] = [System.Environment]::GetEnvironmentVariable($key, 'Process')
}
try {
    foreach ($key in $script:HerdrCallerEnvKeys) {
        [System.Environment]::SetEnvironmentVariable($key, "inherited-$key", 'Process')
    }
    $script:HerdrExe = Join-Path $env:SystemRoot 'System32\herdr-test.exe'
    $script:SessionName = 'orch-acc-test1234'
    $script:RepoRoot = $script:HarnessRoot
    $script:ConfigPath = Join-Path $script:HarnessRoot 'isolated-config.toml'
    $serverStartInfo = if (Get-Command New-RunServerStartInfo -ErrorAction SilentlyContinue) {
        New-RunServerStartInfo
    } else {
        $null
    }
    $callerContextClear = ($null -ne $serverStartInfo)
    if ($serverStartInfo) {
        foreach ($key in $script:HerdrCallerEnvKeys) {
            if ($key -eq 'HERDR_CONFIG_PATH') {
                if ($serverStartInfo.EnvironmentVariables[$key] -ne $script:ConfigPath) { $callerContextClear = $false }
            } elseif ($serverStartInfo.EnvironmentVariables.ContainsKey($key)) {
                $callerContextClear = $false
            }
        }
    }
    Check 'isolated server strips inherited Herdr caller context' $callerContextClear 'caller Herdr variables remain in server environment'
} finally {
    foreach ($key in $script:HerdrCallerEnvKeys) {
        [System.Environment]::SetEnvironmentVariable($key, $oldCallerEnvironment[$key], 'Process')
    }
    $script:HerdrExe = $oldRunServerValues.HerdrExe
    $script:SessionName = $oldRunServerValues.SessionName
    $script:RepoRoot = $oldRunServerValues.RepoRoot
    $script:ConfigPath = $oldRunServerValues.ConfigPath
}


# --- paths ------------------------------------------------------------------
Check 'Test-SamePath mixed slashes' (Test-SamePath -A 'C:/Users/Jaco/x' -B 'C:\Users\Jaco\x') 'mixed'
Check 'Test-SamePath case/backslash' (Test-SamePath -A 'C:\Users\JACO\x\' -B 'c:/users/jaco/x') 'case'
Check 'Test-SamePath rejects sibling' (-not (Test-SamePath -A 'C:\a\b' -B 'C:\a\bc')) 'sibling'
Check 'Test-Below child' (Test-Below -Root 'C:\a\b' -Path 'C:\a\b\c\d') 'child'
Check 'Test-Below rejects prefix sibling' (-not (Test-Below -Root 'C:\a\b' -Path 'C:\a\bc')) 'prefix'
Check 'ConvertTo-NormalPath trims trailing' ((ConvertTo-NormalPath 'C:/x/y/') -eq 'C:\x\y') (ConvertTo-NormalPath 'C:/x/y/')
$socketCheckCommand = Get-Command Test-SessionSocketSeparation -ErrorAction SilentlyContinue
$socketCheckExists = $null -ne $socketCheckCommand
Check 'socket separation helper exists' $socketCheckExists 'missing helper'
$socketCheckSupportsServerState = $socketCheckExists -and $socketCheckCommand.Parameters.ContainsKey('ServerStarted')
Check 'socket separation helper requires startup state' $socketCheckSupportsServerState 'missing ServerStarted parameter'
if ($socketCheckSupportsServerState) {
    Check 'socket check is skipped when isolated server never started' `
        ($null -eq (Test-SessionSocketSeparation -ServerStarted $false -RunSocket '' -DefaultSocket 'C:\default\herdr.sock')) 'server never started'
    Check 'socket check fails when server started but run socket is missing' `
        ((Test-SessionSocketSeparation -ServerStarted $true -RunSocket '' -DefaultSocket 'C:\default\herdr.sock') -eq $false) 'started server has no captured socket'
    Check 'socket check distinguishes separate run server' `
        ((Test-SessionSocketSeparation -ServerStarted $true -RunSocket 'C:\run\herdr.sock' -DefaultSocket 'C:\default\herdr.sock') -eq $true) 'different sockets'
    Check 'socket check rejects default server reuse' `
        ((Test-SessionSocketSeparation -ServerStarted $true -RunSocket 'C:\default\herdr.sock' -DefaultSocket 'C:\default\herdr.sock') -eq $false) 'same socket'
}

if (Get-Command Get-AcceptanceRunIdFromProjectKey -ErrorAction SilentlyContinue) {
    $staleProjectKey = Join-Path (Join-Path $env:TEMP 'orch-accept-f762c17c') 'repo'
    $acceptanceRunId = Get-AcceptanceRunIdFromProjectKey -Key $staleProjectKey -TempRoot $env:TEMP
    $nonAcceptanceKey = Join-Path (Join-Path $env:TEMP 'ordinary-project') 'repo'
    $nonAcceptanceRunId = Get-AcceptanceRunIdFromProjectKey -Key $nonAcceptanceKey -TempRoot $env:TEMP
    $siblingTemp = "$($env:TEMP)-sibling"
    $siblingKey = Join-Path (Join-Path $siblingTemp 'orch-accept-f762c17c') 'repo'
    $siblingRunId = Get-AcceptanceRunIdFromProjectKey -Key $siblingKey -TempRoot $env:TEMP
} else {
    $acceptanceRunId = ''
    $nonAcceptanceRunId = ''
    $siblingRunId = ''
}
Check 'stale acceptance key recognizes exact temp repo' ($acceptanceRunId -eq 'f762c17c') $acceptanceRunId
Check 'ordinary temp repo is not stale acceptance state' ($nonAcceptanceRunId -eq '') $nonAcceptanceRunId
Check 'temp-prefix sibling is not stale acceptance state' ($siblingRunId -eq '') $siblingRunId
# --- redaction --------------------------------------------------------------
$secret = 'sk-abcdefghijklmnopqrstuvwxyz'
$red = Protect-Text "token=$secret and ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ012345"
Check 'Protect-Text masks sk- keys' ($red -notmatch 'sk-abcdefg') $red
Check 'Protect-Text masks gh tokens' ($red -notmatch 'ghp_ABCDEF') $red
$long = 'x' * 5000
Check 'Protect-Text truncates' ((Protect-Text $long 100).Length -lt 200) 'truncate'

# --- stage projection (mirrors task.StageOf) --------------------------------
Check 'StageOf intent' ((Get-TaskStage -Fact ([pscustomobject]@{ Phase = 'intent' })) -eq 'queued') 'intent'
Check 'StageOf partial report stays running' ((Get-TaskStage -Fact ([pscustomobject]@{ Phase = 'prompt_submitted'; Report = [pscustomobject]@{ Kind = 'partial' } })) -eq 'running') 'partial'
Check 'StageOf result is ready' ((Get-TaskStage -Fact ([pscustomobject]@{ Phase = 'prompt_submitted'; Report = [pscustomobject]@{ Kind = 'result' } })) -eq 'ready_for_review') 'result'
Check 'StageOf attention is needs_input' ((Get-TaskStage -Fact ([pscustomobject]@{ Phase = 'prompt_submitted'; Attention = 'x' })) -eq 'needs_input') 'attention'
Check 'StageOf disposition is done' ((Get-TaskStage -Fact ([pscustomobject]@{ Phase = 'prompt_submitted'; Disposition = 'kept'; Report = [pscustomobject]@{ Kind = 'result' } })) -eq 'done') 'done'

# --- Get-Field under StrictMode ---------------------------------------------
Check 'Get-Field missing property' ((Get-Field ([pscustomobject]@{ A = 1 }) 'B' 'def') -eq 'def') 'missing'
Check 'Get-Field null object' ((Get-Field $null 'A' 'def') -eq 'def') 'null'
Check 'Get-Flag false default' ((Get-Flag ([pscustomobject]@{ A = 1 }) 'Z') -eq $false) 'flag'

# --- extension constants parsing -------------------------------------------
# Environment-specific, READ-ONLY input: the extension installed for this OMP
# profile. It is not required by the helper contract, so an absent file is an
# explicit INFO skip rather than a failure.
$extDir = Get-EffectiveExtensionDir
$ext = Join-Path $extDir 'herdr-orchestrator.ts'
if (Test-Path -LiteralPath $ext) {
    $c = Get-ExtensionConstants -Path $ext
    Check 'extension executable parsed' ((Get-Field $c 'PLUGIN_EXECUTABLE') -match 'herdr-omp-orchestrator\.exe$') (Get-Field $c 'PLUGIN_EXECUTABLE')
    Check 'extension state dir parsed' ((Get-Field $c 'PLUGIN_STATE_DIR') -match 'user\.omp-orchestrator') (Get-Field $c 'PLUGIN_STATE_DIR')
    Check 'extension herdr binary parsed' ((Get-Field $c 'HERDR_BINARY') -match 'herdr\.exe$') (Get-Field $c 'HERDR_BINARY')
} else {
    Write-Host ("INFO  skipped the installed-extension parse check: no extension file at " + $ext)
}

# --- state helper against a synthetic store with the real schema ------------
$work = $script:HarnessRoot
$script:WorkRoot = $work
$script:StateHelper = Join-Path $work 'state_query.py'
$db = Join-Path $work 'orchestrator.sqlite'
$script:StateDb = $db
$script:Python = Initialize-StateTools
Check 'state interpreter found' ([bool]$script:Python) 'no python'

$seed = @'
import json, sqlite3, sys
db = sys.argv[1]
con = sqlite3.connect(db)
cur = con.cursor()
cur.executescript("""
CREATE TABLE IF NOT EXISTS projects (key TEXT PRIMARY KEY, data JSON NOT NULL);
CREATE TABLE IF NOT EXISTS tasks (id TEXT PRIMARY KEY, project_key TEXT NOT NULL, launch_pane_id TEXT NULL,
  public_pane_id TEXT NOT NULL DEFAULT '', revision INTEGER NOT NULL, data JSON NOT NULL);
CREATE UNIQUE INDEX IF NOT EXISTS tasks_launch_pane_id ON tasks (launch_pane_id) WHERE launch_pane_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS tasks_project_key ON tasks (project_key);
CREATE TABLE IF NOT EXISTS inbox (id TEXT PRIMARY KEY, project_key TEXT NOT NULL, task_id TEXT NOT NULL DEFAULT '',
  kind TEXT NOT NULL, summary TEXT NOT NULL, delivery TEXT NOT NULL, delivery_state TEXT NOT NULL DEFAULT 'pending',
  delivery_detail TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS ready_sessions (launch_pane_id TEXT PRIMARY KEY, omp_session_id TEXT NOT NULL DEFAULT '',
  omp_session_file TEXT NOT NULL DEFAULT '', omp_session_on_disk INTEGER NOT NULL DEFAULT 0, protocol INTEGER NOT NULL,
  verified INTEGER NOT NULL DEFAULT 0, verified_kind TEXT NOT NULL DEFAULT '', verified_value TEXT NOT NULL DEFAULT '',
  seen_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS inbox_pending ON inbox (project_key, delivery_state);
CREATE TABLE IF NOT EXISTS tickets (id TEXT PRIMARY KEY, project_key TEXT NOT NULL, revision INTEGER NOT NULL, data JSON NOT NULL);
CREATE INDEX IF NOT EXISTS tickets_project_key ON tickets (project_key);
""")
key = "C:\\temp\\run\\repo"
cur.execute("INSERT INTO projects VALUES (?,?)", (key, json.dumps({"Key": key, "AgentName": "orch_p_x", "OMPSessionID": "sess-1"})))
cur.execute("INSERT INTO tasks VALUES (?,?,?,?,?,?)", ("orch_aaaa11112222", key, "w1:p1", "w1:p1", 3,
  json.dumps({"ID": "orch_aaaa11112222", "ProjectKey": key, "Branch": "orch/aaaa11112222", "Report": {"Kind": "result"}})))
cur.execute("INSERT INTO tasks VALUES (?,?,?,?,?,?)", ("orch_bbbb33334444", key, "w2:p1", "w2:p1", 1,
  json.dumps({"ID": "orch_bbbb33334444", "ProjectKey": key, "Branch": "orch/bbbb33334444"})))
cur.execute("INSERT INTO inbox VALUES (?,?,?,?,?,?,?,?)", ("q1", key, "orch_aaaa11112222", "question", "hi", "worker question", "delivered", "ok"))
cur.execute("INSERT INTO ready_sessions VALUES (?,?,?,?,?,?,?,?,?)", ("w1:p1", "sess-1", "", 0, 1, 1, "path", "C:\\s.jsonl", 1))
cur.execute("INSERT INTO tickets VALUES (?,?,?,?)", ("tkt_aaaa11112222", key, 2,
  json.dumps({"ID": "tkt_aaaa11112222", "ProjectKey": key, "Title": "ACCEPT TICKET: one", "Column": "todo", "Revision": 2, "Outcome": "", "Disposition": ""})))
cur.execute("INSERT INTO tickets VALUES (?,?,?,?)", ("tkt_bbbb33334444", key, 1,
  json.dumps({"ID": "tkt_bbbb33334444", "ProjectKey": key, "Title": "ACCEPT TICKET: two", "Column": "backlog", "Revision": 1, "Outcome": "", "Disposition": ""})))
# a foreign row that must survive untouched
other = "C:\\other\\project"
cur.execute("INSERT INTO projects VALUES (?,?)", (other, json.dumps({"Key": other})))
cur.execute("INSERT INTO tasks VALUES (?,?,?,?,?,?)", ("orch_cccc55556666", other, None, "w9:p1", 1,
  json.dumps({"ID": "orch_cccc55556666", "ProjectKey": other})))
cur.execute("INSERT INTO inbox VALUES (?,?,?,?,?,?,?,?)", ("q9", other, "orch_cccc55556666", "progress", "x", "worker progress", "pending", ""))
cur.execute("INSERT INTO ready_sessions VALUES (?,?,?,?,?,?,?,?,?)", ("w9:p1", "sess-9", "", 0, 1, 1, "id", "sess-9", 1))
cur.execute("INSERT INTO tickets VALUES (?,?,?,?)", ("tkt_cccc55556666", other, 1,
  json.dumps({"ID": "tkt_cccc55556666", "ProjectKey": other, "Title": "someone else's", "Column": "backlog", "Revision": 1})))
con.commit()
print("seeded")
'@
$seedPath = Join-Path $work 'seed.py'
Set-Content -LiteralPath $seedPath -Value $seed -Encoding UTF8
$seeded = Invoke-Native -File $script:Python -Arguments @($seedPath, $db) -TimeoutSeconds 60
Check 'synthetic store seeded' ($seeded.Stdout -match 'seeded') ($seeded.Stderr)

$dump = Get-StateDump
Check 'dump reads all four tables' ((@($dump.tables.projects).Count -eq 2) -and (@($dump.tables.tasks).Count -eq 3) -and (@($dump.tables.inbox).Count -eq 2) -and (@($dump.tables.ready_sessions).Count -eq 2)) `
    ("p=$( @($dump.tables.projects).Count ) t=$(@($dump.tables.tasks).Count) i=$(@($dump.tables.inbox).Count) r=$(@($dump.tables.ready_sessions).Count)")
$tasks = Get-ProjectTasks -Key 'C:\temp\run\repo'
Check 'project task filter' (@($tasks).Count -eq 2) (@($tasks).Count)
Check 'fact json decoded' ((Get-Field (Get-Field $tasks[0] 'fact') 'Branch') -match '^orch/') (Get-Json $tasks[0])
$ready = Get-ReadySession -LaunchPaneId 'w1:p1'
Check 'ready session read' ((Get-Field $ready 'verified') -eq 1) (Get-Json $ready)
# --- tickets: the harness reads the tickets table the way it reads tasks -----
Check 'dump reads the tickets table (this run two, foreign one)' (@($dump.tables.tickets).Count -eq 3) "tickets=$(@($dump.tables.tickets).Count)"
$ticketRow = Get-TicketById -Id 'tkt_aaaa11112222'
Check 'Get-TicketById returns the ticket row with its column and revision' `
    (($null -ne $ticketRow) -and ((Get-Field (Get-Field $ticketRow 'fact') 'Column') -ceq 'todo') -and ([int](Get-Field $ticketRow 'revision') -eq 2) -and ((Get-Field $ticketRow 'project_key') -ceq 'C:\temp\run\repo')) `
    (Get-Json $ticketRow)
Check 'Get-TicketById decodes the ticket fact (title and an empty decision)' `
    (((Get-Field (Get-Field $ticketRow 'fact') 'Title') -ceq 'ACCEPT TICKET: one') -and ([string](Get-Field (Get-Field $ticketRow 'fact') 'Outcome') -eq '')) (Get-Json $ticketRow)
Check 'Get-TicketById returns null for an unknown ticket' ($null -eq (Get-TicketById -Id 'tkt_missing')) 'unknown id'
Check 'Get-ProjectTickets filters to the project' (@(Get-ProjectTickets -Key 'C:\temp\run\repo').Count -eq 2) "n=$(@(Get-ProjectTickets -Key 'C:\temp\run\repo').Count)"

$plan = Invoke-State -Operation 'plan-scoped' -Arguments @('C:\temp\run\repo', 'w1:p1,w2:p1')
Check 'plan counts ours' ((Get-Field $plan 'counts').tasks -eq 2) (Get-Json $plan)
Check 'plan sees no foreign reference' (@($plan.foreign).Count -eq 0) (Get-Json $plan.foreign)
Check 'plan counts this run tickets' ((Get-Field $plan 'counts').tickets -eq 2) (Get-Json $plan)

$del = Invoke-State -Operation 'delete-scoped' -Arguments @('C:\temp\run\repo', 'w1:p1,w2:p1')
Check 'delete reports ok' ((Get-Flag $del 'ok')) (Get-Json $del)
Check 'delete removed exactly our rows' (((Get-Field $del 'deleted').tasks -eq 2) -and ((Get-Field $del 'deleted').projects -eq 1) -and ((Get-Field $del 'deleted').inbox -eq 1) -and ((Get-Field $del 'deleted').ready_sessions -eq 1)) (Get-Json $del)
Check 'delete left other rows identical' ((Get-Flag $del 'others_identical')) (Get-Json $del)
Check 'delete left nothing of ours' ((Get-Field $del 'ours_remaining') -eq 0) (Get-Json $del)
Check 'delete removed exactly this run tickets' ((Get-Field $del 'deleted').tickets -eq 2) (Get-Json $del)
$after = Get-StateDump
Check 'foreign rows survive' ((@($after.tables.projects).Count -eq 1) -and (@($after.tables.tasks).Count -eq 1) -and (@($after.tables.ready_sessions).Count -eq 1)) `
    ("p=$(@($after.tables.projects).Count) t=$(@($after.tables.tasks).Count) r=$(@($after.tables.ready_sessions).Count)")
Check 'the foreign ticket survives the scoped delete' ((@($after.tables.tickets).Count -eq 1) -and ((Get-Field @($after.tables.tickets)[0] 'id') -ceq 'tkt_cccc55556666')) "tickets=$(@($after.tables.tickets).Count)"

# a foreign row that references one of our task ids must block deletion
$db2 = Join-Path $work 'second.sqlite'
Set-Content -LiteralPath $seedPath -Value $seed -Encoding UTF8
[void](Invoke-Native -File $script:Python -Arguments @($seedPath, $db2) -TimeoutSeconds 60)
$script:StateDb = $db2
$inject = @'
import sqlite3, sys
con = sqlite3.connect(sys.argv[1])
con.execute("INSERT INTO inbox VALUES ('qx', 'C:\\other\\project', 'orch_aaaa11112222','question','x','worker question','pending','')")
con.commit()
print("injected")
'@
$injectPath = Join-Path $work 'inject.py'
Set-Content -LiteralPath $injectPath -Value $inject -Encoding UTF8
$injected = Invoke-Native -File $script:Python -Arguments @($injectPath, $db2) -TimeoutSeconds 60
Check 'foreign reference injected' ($injected.Stdout -match 'injected') $injected.Stderr
$blocked = Invoke-Native -File $script:Python -Arguments @($script:StateHelper, $db2, 'delete-scoped', 'C:\temp\run\repo', 'w1:p1,w2:p1') -TimeoutSeconds 60
Check 'delete refuses when a foreign row references our identity' (($blocked.ExitCode -eq 3) -and ($blocked.Stdout -match 'foreign')) "exit=$($blocked.ExitCode) out=$($blocked.Stdout)"

# a foreign TASK that links to one of our tickets must block deletion too
$db3 = Join-Path $work 'third.sqlite'
Set-Content -LiteralPath $seedPath -Value $seed -Encoding UTF8
[void](Invoke-Native -File $script:Python -Arguments @($seedPath, $db3) -TimeoutSeconds 60)
$injectTicketRef = @'
import sqlite3, sys, json
con = sqlite3.connect(sys.argv[1])
con.execute("INSERT INTO tasks VALUES (?,?,?,?,?,?)", ("orch_dddd77778888", "C:\\other\\project", None, "w9:p9", 1,
  json.dumps({"ID": "orch_dddd77778888", "ProjectKey": "C:\\other\\project", "TicketID": "tkt_aaaa11112222"})))
con.commit()
print("injected")
'@
$injectTicketRefPath = Join-Path $work 'inject_ticket_ref.py'
Set-Content -LiteralPath $injectTicketRefPath -Value $injectTicketRef -Encoding UTF8
$injectedTicketRef = Invoke-Native -File $script:Python -Arguments @($injectTicketRefPath, $db3) -TimeoutSeconds 60
Check 'foreign task linking to our ticket injected' ($injectedTicketRef.Stdout -match 'injected') $injectedTicketRef.Stderr
$blockedByTicketRef = Invoke-Native -File $script:Python -Arguments @($script:StateHelper, $db3, 'delete-scoped', 'C:\temp\run\repo', 'w1:p1,w2:p1') -TimeoutSeconds 60
Check 'delete refuses when a foreign task links to one of our tickets' (($blockedByTicketRef.ExitCode -eq 3) -and ($blockedByTicketRef.Stdout -match 'foreign')) "exit=$($blockedByTicketRef.ExitCode) out=$($blockedByTicketRef.Stdout)"

# a store written before the tickets table existed is an empty table, not a failure
$db4 = Join-Path $work 'before-tickets.sqlite'
Set-Content -LiteralPath $seedPath -Value $seed -Encoding UTF8
[void](Invoke-Native -File $script:Python -Arguments @($seedPath, $db4) -TimeoutSeconds 60)
$dropTickets = @'
import sqlite3, sys
con = sqlite3.connect(sys.argv[1])
con.execute("DROP TABLE tickets")
con.commit()
print("dropped")
'@
$dropTicketsPath = Join-Path $work 'drop_tickets.py'
Set-Content -LiteralPath $dropTicketsPath -Value $dropTickets -Encoding UTF8
[void](Invoke-Native -File $script:Python -Arguments @($dropTicketsPath, $db4) -TimeoutSeconds 60)
$script:StateDb = $db4
$oldDump = Get-StateDump
Check 'a store without a tickets table dumps an empty tickets list and keeps its tasks' ((@($oldDump.tables.tickets).Count -eq 0) -and (@($oldDump.tables.tasks).Count -eq 3)) "tickets=$(@($oldDump.tables.tickets).Count) tasks=$(@($oldDump.tables.tasks).Count)"
Check 'Get-TicketById on a store without the table is null' ($null -eq (Get-TicketById -Id 'tkt_aaaa11112222')) 'old store'
$oldDelete = Invoke-State -Operation 'delete-scoped' -Arguments @('C:\temp\run\repo', 'w1:p1,w2:p1')
Check 'scoped delete works on a store without the tickets table' ((Get-Flag $oldDelete 'ok') -and ((Get-Field $oldDelete 'deleted').tasks -eq 2) -and ((Get-Field $oldDelete 'deleted').tickets -eq 0)) (Get-Json $oldDelete)
# ...but only the tickets table may be missing. A store without another table is not the
# plugin's store (a wrong path becomes an empty SQLite file), and reading it as four
# empty tables would let every "nothing of this run is left behind" check pass vacuously.
$db5 = Join-Path $work 'wrong-store.sqlite'
Set-Content -LiteralPath $seedPath -Value $seed -Encoding UTF8
[void](Invoke-Native -File $script:Python -Arguments @($seedPath, $db5) -TimeoutSeconds 60)
$dropProjects = @'
import sqlite3, sys
con = sqlite3.connect(sys.argv[1])
con.execute("DROP TABLE projects")
con.commit()
print("dropped")
'@
$dropProjectsPath = Join-Path $work 'drop_projects.py'
Set-Content -LiteralPath $dropProjectsPath -Value $dropProjects -Encoding UTF8
[void](Invoke-Native -File $script:Python -Arguments @($dropProjectsPath, $db5) -TimeoutSeconds 60)
$script:StateDb = $db5
$wrongStoreThrew = $false
try { [void](Get-StateDump) } catch { $wrongStoreThrew = $true }
Check 'a store missing a table other than tickets fails the dump instead of reading as empty' $wrongStoreThrew 'projects table missing'
$script:StateDb = $db2

# --- added checks: task projection and handshake materialization -------------
$projectionFails = 0
$fact = [pscustomobject]@{
    ID = 'orch_aaaa11112222'; ProjectKey = 'C:\r'; Title = 'ACCEPT A'; Phase = 'prompt_submitted'
    Branch = 'orch/aaaa11112222'; WorktreePath = 'C:\wt\a'; WorktreeWorkspaceID = 'w2'
    PublicPaneID = 'w2:p1'; LaunchPaneID = 'w2:p1'; CurrentWorkspaceID = 'w2'
    TerminalID = 'term_1'; OMPSessionID = 'C:\s.jsonl'; AgentName = 'orch_aaaa11112222'
    Report = $null; Blocker = ''; Attention = ''; Disposition = ''; Revision = 3; Activity = ''; UpdatedAt = 1
}
$moved = $fact.PSObject.Copy()
$moved.PublicPaneID = 'w9:p1'; $moved.CurrentWorkspaceID = 'w9'; $moved.Revision = 4; $moved.Activity = 'idle'
Check 'projection ignores the move fields' ((Get-TaskProjection -Fact $fact -MoveSensitive) -ceq (Get-TaskProjection -Fact $moved -MoveSensitive)) (Get-TaskProjection -Fact $moved -MoveSensitive)
$mutated = $fact.PSObject.Copy()
$mutated.WorktreePath = 'C:\somewhere-else'
Check 'projection notices a provenance change' ((Get-TaskProjection -Fact $fact -MoveSensitive) -cne (Get-TaskProjection -Fact $mutated -MoveSensitive)) 'provenance'
$reported = $fact.PSObject.Copy()
$reported.Report = [pscustomobject]@{ Kind = 'result'; Summary = 's' }
Check 'projection (stage form) notices a report' ((Get-TaskProjection -Fact $fact) -cne (Get-TaskProjection -Fact $reported)) 'report'
Check 'projection (move form) ignores a report' ((Get-TaskProjection -Fact $fact -MoveSensitive) -ceq (Get-TaskProjection -Fact $reported -MoveSensitive)) 'report/move'
$identityFacts = [pscustomobject]@{
    Project = [pscustomobject]@{ Key = 'repo'; AgentName = 'primary'; LaunchPaneID = 'p1'; OMPSessionID = 's1'; TerminalID = 't1'; RepoWorkspaceID = 'w1' }
    Tasks = @([pscustomobject]@{ fact = [pscustomobject]@{
        ID = 'task1'; ProjectKey = 'repo'; Title = 'task'; Branch = 'branch'; BaseRevision = 'base'
        WorktreePath = 'C:\wt'; WorktreeWorkspaceID = 'w2'; LaunchPaneID = 'p2'; OMPSessionID = 's2'
        Phase = 'running'; Blocker = ''; Attention = ''; Disposition = ''; CurrentWorkspaceID = 'w2'; Report = $null
    } })
    Inbox = @([pscustomobject]@{ id = 'i1'; kind = 'activity'; delivery_state = 'delivered' })
}
$identityProjection = ''
try { $identityProjection = Get-IdentityProjection -Facts $identityFacts } catch { }
Check 'restart identity projection accepts Facts and excludes mutable inbox rows' ($identityProjection -match 'project/Key=repo' -and $identityProjection -notmatch 'inbox/i1') 'project fields only'
$identityFactsAfter = [pscustomobject]@{
    Project = $identityFacts.Project.PSObject.Copy()
    Tasks = @([pscustomobject]@{ fact = $identityFacts.Tasks[0].fact.PSObject.Copy() })
}
$identityFactsAfter.Project.TerminalID = 't2'
$identityFactsAfter.Project.LaunchPaneID = 'p9'
$identityFactsAfter.Tasks[0].fact.LaunchPaneID = 'p8'
$identityProjectionAfter = ''
try { $identityProjectionAfter = Get-IdentityProjection -Facts $identityFactsAfter } catch { }
Check 'restart identity projection permits runtime pane rekeys while preserving session facts' ($identityProjection -ceq $identityProjectionAfter) "before=[$identityProjection] after=[$identityProjectionAfter]"
$boardOrderScreen = @'
Ready for review
[task_b] B
> [task_a] A
Selected task_a | branch branch-a
'@
$boardMoveUp = ''
$boardMoveDown = ''
try { $boardMoveUp = Get-BoardSelectionKey -Screen $boardOrderScreen -CurrentId 'task_a' -TargetId 'task_b' } catch { }
try { $boardMoveDown = Get-BoardSelectionKey -Screen $boardOrderScreen -CurrentId 'task_b' -TargetId 'task_a' } catch { }
Check 'board selector moves upward to a preceding task in the same stage' ($boardMoveUp -eq 'up') "move=$boardMoveUp"
Check 'board selector moves downward to a following task in the same stage' ($boardMoveDown -eq 'down') "move=$boardMoveDown"

$pathHandshake = [pscustomobject]@{ omp_session_file = (Join-Path $script:HarnessRoot 'materialized-probe.txt'); omp_session_on_disk = 1 }
Set-Content -LiteralPath $pathHandshake.omp_session_file -Value 'x'
Check 'handshake materialized: existing path' ((Test-HandshakeMaterialized -Handshake $pathHandshake) -eq $true) 'path exists'
$pathHandshake.omp_session_file = (Join-Path $script:HarnessRoot 'missing-probe.jsonl')
Check 'handshake materialized: missing path' ((Test-HandshakeMaterialized -Handshake $pathHandshake) -eq $false) 'path missing'
$idHandshake = [pscustomobject]@{ omp_session_id = '01a0-abc'; omp_session_file = ''; omp_session_on_disk = 1 }
Check 'handshake materialized: id-only is uncheckable' ($null -eq (Test-HandshakeMaterialized -Handshake $idHandshake)) 'id-only'
Check 'handshake materialized: no handshake' ($null -eq (Test-HandshakeMaterialized -Handshake $null)) 'null'

$err = Invoke-Native -File (Get-Command python).Source -Arguments @('-c', 'import sys,json;print(json.dumps({"error":{"code":"agent_blocked"}}),file=sys.stderr);sys.exit(1)') -TimeoutSeconds 30
Check 'Get-HerdrErrorCode reads stderr envelope' ((Get-HerdrErrorCode -Result $err) -eq 'agent_blocked') (Get-HerdrErrorCode -Result $err)



# --- cleanup-invariant projection (C7 refusal comparison) -------------------
$ci = Get-TaskProjection -Fact $fact -CleanupInvariant
foreach ($fld in @('Branch','WorktreePath','Report','Disposition','LaunchPaneID','PublicPaneID','TerminalID','CurrentWorkspaceID','WorktreeWorkspaceID','AgentName','OMPSessionID')) {
    Check "cleanup invariant includes $fld" ($ci -match [regex]::Escape("$fld=")) $ci
}
foreach ($fld in @('Activity','ActivitySeenAt','Revision','UpdatedAt','CreatedAt')) {
    Check "cleanup invariant excludes $fld" (-not ($ci -match [regex]::Escape("$fld="))) $ci
}
$movedId = $fact.PSObject.Copy(); $movedId.PublicPaneID = 'w9:p1'
Check 'cleanup invariant notices a changed live identity' ((Get-TaskProjection -Fact $movedId -CleanupInvariant) -cne $ci) 'public pane'
$movedActivity = $fact.PSObject.Copy(); $movedActivity.Activity = 'idle'; $movedActivity.Revision = 9; $movedActivity.UpdatedAt = 99
Check 'cleanup invariant ignores activity/revision refreshes' ((Get-TaskProjection -Fact $movedActivity -CleanupInvariant) -ceq $ci) (Get-TaskProjection -Fact $movedActivity -CleanupInvariant)

# --- expected-extension rendering matches the installed file -----------------
# The renderer is validated structurally (the installed file may legitimately
# predate the current source until a setup run refreshes it).
$rendered = Get-ExpectedExtensionContent -Executable 'C:\x\herdr-omp-orchestrator.exe' -StateDir 'C:\Users\a\herdr\plugins\user.omp-orchestrator' -HerdrBinary 'C:\y\herdr.exe'
Check 'renderer output is a string, not an array' ($rendered -is [string]) $rendered.GetType().Name
Check 'renderer left no placeholder behind' (-not ($rendered -match '@@OMP_ORCH_')) ($rendered -match '@@OMP_ORCH_[A-Z_]+@@')
Check 'renderer output starts with the managed marker' ($rendered.StartsWith('// herdr-omp-orchestrator managed OMP extension')) $rendered.Substring(0, 60)
$renderFile = Join-Path $script:HarnessRoot 'render-probe.ts'
Set-Content -LiteralPath $renderFile -Value $rendered -Encoding UTF8 -NoNewline
$parsed = Get-ExtensionConstants -Path $renderFile
Check 'rendered executable round-trips' ([string]$parsed['PLUGIN_EXECUTABLE'] -eq 'C:\x\herdr-omp-orchestrator.exe') ([string]$parsed['PLUGIN_EXECUTABLE'])
Check 'rendered state dir round-trips' ([string]$parsed['PLUGIN_STATE_DIR'] -eq 'C:\Users\a\herdr\plugins\user.omp-orchestrator') ([string]$parsed['PLUGIN_STATE_DIR'])
Check 'rendered herdr binary round-trips' ([string]$parsed['HERDR_BINARY'] -eq 'C:\y\herdr.exe') ([string]$parsed['HERDR_BINARY'])
Remove-Item -LiteralPath $renderFile -Force -ErrorAction SilentlyContinue
# --- bridge envelope shape rules --------------------------------------------
function Test-EnvelopeShape([string]$json) {
    # Exercises the SCRIPT's own rule, so the harness cannot drift from it.
    return (Test-BridgeEnvelopeShape -Answer ($json | ConvertFrom-Json))
}
Check 'envelope: success shape accepted' (Test-EnvelopeShape '{"ok":true,"result":{}}') 'ok+result'
Check 'envelope: coded error shape accepted' (Test-EnvelopeShape '{"ok":false,"error":{"code":"x","message":"y"}}') 'ok+error'
Check 'envelope: empty object rejected' (-not (Test-EnvelopeShape '{}')) 'empty'
Check 'envelope: unrelated object rejected' (-not (Test-EnvelopeShape '{"other":1}')) 'unrelated'
Check 'envelope: ok without result rejected' (-not (Test-EnvelopeShape '{"ok":true}')) 'ok only'
Check 'envelope: error without code rejected' (-not (Test-EnvelopeShape '{"ok":false,"error":{"message":"y"}}')) 'error no code'
Check 'envelope: non-boolean string ok=false rejected' (-not (Test-EnvelopeShape '{"ok":"false","result":{}}')) 'string ok'
Check 'envelope: non-boolean string ok=true rejected' (-not (Test-EnvelopeShape '{"ok":"true","result":{}}')) 'string ok true'
Check 'envelope: numeric ok rejected' (-not (Test-EnvelopeShape '{"ok":1,"result":{}}')) 'numeric ok'
Check 'envelope: null ok rejected' (-not (Test-EnvelopeShape '{"ok":null,"result":{}}')) 'null ok'
Check 'envelope: real boolean false with coded error accepted' (Test-EnvelopeShape '{"ok":false,"error":{"code":"x","message":"y"}}') 'bool false'

# --- strict JSON boolean reader (strings/numbers/arrays/null) ---------------
$boolCases = @(
    @{ n = 'true';     j = '{"v":true}';    e = $true },
    @{ n = 'false';    j = '{"v":false}';   e = $false },
    @{ n = 'string false'; j = '{"v":"false"}'; e = $null },
    @{ n = 'string true';  j = '{"v":"true"}';  e = $null },
    @{ n = 'number 1'; j = '{"v":1}';       e = $null },
    @{ n = 'number 0'; j = '{"v":0}';       e = $null },
    @{ n = 'null';     j = '{"v":null}';    e = $null },
    @{ n = 'absent';   j = '{}';            e = $null },
    @{ n = 'array';    j = '{"v":[1,2]}';   e = $null },
    @{ n = 'nested object'; j = '{"v":{"a":1}}'; e = $null }
)
foreach ($case in $boolCases) {
    $obj = $case.j | ConvertFrom-Json
    Check ("Test-JsonBool " + $case.n) ((Test-JsonBool -Object $obj -Name 'v') -eq $case.e) ("got=" + (Test-JsonBool -Object $obj -Name 'v'))
}
$arr = '[1,2]' | ConvertFrom-Json
Check 'Test-JsonBool on a parsed array is null, not a throw' ((Test-JsonBool -Object $arr -Name 'v') -eq $null) 'array root'
$num = '5' | ConvertFrom-Json
Check 'Test-JsonBool on a parsed number is null' ((Test-JsonBool -Object $num -Name 'v') -eq $null) 'number root'
$str = '"text"' | ConvertFrom-Json
Check 'Test-JsonBool on a parsed string is null' ((Test-JsonBool -Object $str -Name 'v') -eq $null) 'string root'
Check 'Test-JsonBool on null object is null' ((Test-JsonBool -Object $null -Name 'v') -eq $null) 'null object'
$nullBool = [pscustomobject]@{ ok = $null }
Check 'Test-JsonBool null value is null' ((Test-JsonBool -Object $nullBool -Name 'ok') -eq $null) 'null value'
Check 'Assert-JsonTrue records PASS for a real true' ((Assert-JsonTrue -Id 'H.1' -What 'bool true' -Object ([pscustomobject]@{ ok = $true }) -Name 'ok').status -eq 'PASS') 'true'
Check 'Assert-JsonTrue records FAIL for the string "false"' ((Assert-JsonTrue -Id 'H.2' -What 'bool string' -Object ([pscustomobject]@{ ok = 'false' }) -Name 'ok').status -eq 'FAIL') 'string false'
Check 'Assert-JsonTrue records FAIL for a missing value' ((Assert-JsonTrue -Id 'H.3' -What 'bool absent' -Object ([pscustomobject]@{}) -Name 'ok').status -eq 'FAIL') 'absent'
Check 'Assert-JsonFalse records PASS for a real false' ((Assert-JsonFalse -Id 'H.4' -What 'bool false' -Object ([pscustomobject]@{ ok = $false }) -Name 'ok').status -eq 'PASS') 'false'
Check 'Assert-JsonFalse records FAIL for a number' ((Assert-JsonFalse -Id 'H.5' -What 'bool number' -Object ([pscustomobject]@{ ok = 1 }) -Name 'ok').status -eq 'FAIL') 'number'
# Evidence-enriched assertion calls must not throw while formatting a failure.
$withNonEmptyEvidence = $null
try {
    $withNonEmptyEvidence = Assert-NonEmpty -Id 'H.6' -What 'non-empty details' -Value 'pane-1' -Evidence 'context=board-pane'
} catch { }
Check 'Assert-NonEmpty accepts and records caller evidence' ($null -ne $withNonEmptyEvidence -and $withNonEmptyEvidence.evidence -match 'context=board-pane') 'evidence'
$withBoolEvidence = $null
try {
    $withBoolEvidence = Assert-JsonTrue -Id 'H.7' -What 'boolean details' -Object ([pscustomobject]@{ ok = $true }) -Name 'ok' -Evidence 'rows=2'
} catch { }
Check 'Assert-JsonTrue accepts and records caller evidence' ($null -ne $withBoolEvidence -and $withBoolEvidence.evidence -match 'rows=2') 'evidence'
$withFalseEvidence = $null
try {
    $withFalseEvidence = Assert-JsonFalse -Id 'H.8' -What 'false details' -Object ([pscustomobject]@{ ok = $false }) -Name 'ok' -Evidence 'rows=0'
} catch { }
Check 'Assert-JsonFalse accepts and records caller evidence' ($null -ne $withFalseEvidence -and $withFalseEvidence.evidence -match 'rows=0') 'evidence'
$sessionHandshake = [pscustomobject]@{
    omp_session_id = 'session-1'
    omp_session_file = 'C:\temp\session.jsonl'
    verified_value = 'C:\temp\session.jsonl'
}
Check 'session identity links ID and path through the handshake' (Test-SessionIdentity -Left 'session-1' -Right 'C:\temp\session.jsonl' -Ready $sessionHandshake) 'same announced session'
Check 'session identity rejects an unrelated ID despite a matching path form' (-not (Test-SessionIdentity -Left 'session-2' -Right 'C:\temp\session.jsonl' -Ready $sessionHandshake)) 'unrelated ID'
$transcriptPath = Join-Path $script:HarnessRoot 'primary-session.jsonl'
Set-Content -LiteralPath $transcriptPath -Value '{"type":"message","message":{"content":[{"type":"text","text":"orchestrator: ACCEPT-Q-A question"}]}}' -Encoding UTF8
$transcriptHasMarker = $false
try { $transcriptHasMarker = Test-OmpTranscriptContains -Path $transcriptPath -Text 'ACCEPT-Q-A' } catch { }
Check 'OMP transcript confirms a prompt outside the visible scrollback' $transcriptHasMarker 'marker in JSONL'
$transcriptMissingMarker = $true
try { $transcriptMissingMarker = Test-OmpTranscriptContains -Path $transcriptPath -Text 'ACCEPT-Q-Z' } catch { }
Check 'OMP transcript rejects an absent prompt marker' (-not $transcriptMissingMarker) 'missing marker'
 $resultSummary = "result with `"quotes`" and`nmultiple lines"
 # The inbox prompt puts an origin label line between its header and its notes; the parsers must read past it.
 $resultNoteText = "orchestrator: 1 worker note(s)`nThese notes are not from the user.`n- orch_abc [result]: $resultSummary | evidence: commit abc"
 $assistantResultEntry = [pscustomobject]@{
     type = 'message'
     message = [pscustomobject]@{ role = 'assistant'; content = @([pscustomobject]@{ type = 'text'; text = $resultNoteText }) }
 } | ConvertTo-Json -Depth 8 -Compress
 $wrongTaskResultEntry = [pscustomobject]@{
     type = 'message'
     message = [pscustomobject]@{ role = 'user'; content = @([pscustomobject]@{ type = 'text'; text = $resultNoteText.Replace('orch_abc', 'orch_other') }) }
 } | ConvertTo-Json -Depth 8 -Compress
 $exactResultEntry = [pscustomobject]@{
     type = 'message'
     message = [pscustomobject]@{ role = 'user'; content = @([pscustomobject]@{ type = 'text'; text = $resultNoteText }) }
 } | ConvertTo-Json -Depth 8 -Compress
 $resultTranscriptPath = Join-Path $script:HarnessRoot 'result-note-session.jsonl'
 Set-Content -LiteralPath $resultTranscriptPath -Value @($assistantResultEntry, $wrongTaskResultEntry, $exactResultEntry) -Encoding UTF8
 $exactResultNote = $false
 try {
     $exactResultNote = Test-OmpTranscriptContainsResultNote -Path $resultTranscriptPath `
         -TaskId 'orch_abc' -Summary $resultSummary
 } catch { }
 Check 'result note parser decodes JSON escaping and matches the exact task/result/summary' $exactResultNote 'decoded result summary'
 $wrongResultSummary = $true
 try {
     $wrongResultSummary = Test-OmpTranscriptContainsResultNote -Path $resultTranscriptPath `
         -TaskId 'orch_abc' -Summary 'a different summary'
 } catch { }
 Check 'result note parser rejects a different summary' (-not $wrongResultSummary) 'wrong summary'
$longResultSummary = 'x' * 500
$longResultTaskId = 'orch_123456789abc'
$longResultLine = "$longResultTaskId [result]: $longResultSummary | evidence: commit abc"
$truncatedResultLine = $longResultLine.Substring(0, 400)
$truncatedResultEntry = [pscustomobject]@{
    type = 'message'
    message = [pscustomobject]@{
        role = 'user'
        content = @([pscustomobject]@{ type = 'text'; text = "orchestrator: 1 worker note(s)`n- $truncatedResultLine" })
    }
} | ConvertTo-Json -Depth 8 -Compress
$truncatedResultPath = Join-Path $script:HarnessRoot 'truncated-result-note.jsonl'
Set-Content -LiteralPath $truncatedResultPath -Value $truncatedResultEntry -Encoding UTF8
$truncatedResultDelivered = $false
try {
    $truncatedResultDelivered = Test-OmpTranscriptContainsResultNote -Path $truncatedResultPath `
        -TaskId $longResultTaskId -Summary $longResultSummary
} catch { }
Check 'result note parser accepts the exact summary prefix preserved by the inbox line bound' `
    $truncatedResultDelivered 'truncated summary prefix'
$astralSummary = ('x' * 127) + [char]::ConvertFromUtf32(0x1F600) + ('y' * 500)
$astralTaskId = 'orch_123456789abd'
$astralLine = "$astralTaskId [result]: $astralSummary | evidence: commit abc"
$astralEntry = [pscustomobject]@{
    type = 'message'
    message = [pscustomobject]@{
        role = 'user'
        content = @([pscustomobject]@{ type = 'text'; text = "orchestrator: 1 worker note(s)`n- $($astralLine.Substring(0, 400))" })
    }
} | ConvertTo-Json -Depth 8 -Compress
$astralPath = Join-Path $script:HarnessRoot 'astral-result-note.jsonl'
Set-Content -LiteralPath $astralPath -Value $astralEntry -Encoding UTF8
$astralDelivered = $false
try {
    $astralDelivered = Test-OmpTranscriptContainsResultNote -Path $astralPath `
        -TaskId $astralTaskId -Summary $astralSummary
} catch { }
Check 'result note parser keeps an astral scalar intact at the 128-rune boundary' $astralDelivered 'astral summary prefix'
$differentAstralSummary = ('x' * 127) + [char]::ConvertFromUtf32(0x1F601) + ('y' * 500)
$differentAstralLine = "$astralTaskId [result]: $differentAstralSummary | evidence: commit abc"
$differentAstralEntry = [pscustomobject]@{
    type = 'message'
    message = [pscustomobject]@{
        role = 'user'
        content = @([pscustomobject]@{ type = 'text'; text = "orchestrator: 1 worker note(s)`n- $($differentAstralLine.Substring(0, 400))" })
    }
} | ConvertTo-Json -Depth 8 -Compress
$differentAstralPath = Join-Path $script:HarnessRoot 'different-astral-result-note.jsonl'
Set-Content -LiteralPath $differentAstralPath -Value $differentAstralEntry -Encoding UTF8
$differentAstralAccepted = $true
try {
    $differentAstralAccepted = Test-OmpTranscriptContainsResultNote -Path $differentAstralPath `
        -TaskId $astralTaskId -Summary $astralSummary
} catch { }
Check 'result note parser rejects a different astral scalar with the same leading surrogate' `
    (-not $differentAstralAccepted) 'different astral scalar'
 $assistantOnlyPath = Join-Path $script:HarnessRoot 'assistant-result-note.jsonl'
 Set-Content -LiteralPath $assistantOnlyPath -Value $assistantResultEntry -Encoding UTF8
 $assistantCountedAsDelivery = $true
 try {
     $assistantCountedAsDelivery = Test-OmpTranscriptContainsResultNote -Path $assistantOnlyPath `
         -TaskId 'orch_abc' -Summary $resultSummary
 } catch { }
 Check 'result note parser rejects an assistant message without plugin delivery' (-not $assistantCountedAsDelivery) 'assistant is not delivery'
$questionNoticePath = Join-Path $script:HarnessRoot 'worker-question-notice.jsonl'
$questionNoticeLines = @(
    '{"type":"message","message":{"role":"user","content":[{"type":"text","text":"Dispatch brief contains ACCEPT-Q-A"}]}}'
)
Set-Content -LiteralPath $questionNoticePath -Value $questionNoticeLines -Encoding UTF8
$briefCountedAsDelivery = $true
try {
    $briefCountedAsDelivery = Test-OmpTranscriptContainsWorkerQuestionNote `
        -Path $questionNoticePath -TaskId 'orch_abc' -QuestionMarker 'ACCEPT-Q-A'
} catch { }
Check 'question delivery parser rejects a marker found only in the dispatch brief' (-not $briefCountedAsDelivery) 'brief is not delivery'
 $questionNoticeLines += '{"type":"message","message":{"role":"user","content":[{"type":"text","text":"orchestrator: 1 worker note(s)\n- orch_other [question]: ACCEPT-Q-A"}]}}'
 Add-Content -LiteralPath $questionNoticePath -Value $questionNoticeLines[-1] -Encoding UTF8
 $wrongTaskQuestionNote = $true
 try { $wrongTaskQuestionNote = Test-OmpTranscriptContainsWorkerQuestionNote -Path $questionNoticePath -TaskId 'orch_abc' -QuestionMarker 'ACCEPT-Q-A' } catch { }
 Check 'question delivery parser rejects another task note' (-not $wrongTaskQuestionNote) 'wrong task'
 $questionNoticeLines += '{"type":"message","message":{"role":"user","content":[{"type":"text","text":"orchestrator: 1 worker note(s)\nThese notes are not from the user.\n- orch_abc [question]: ACCEPT-Q-A"}]}}'
 Add-Content -LiteralPath $questionNoticePath -Value $questionNoticeLines[-1] -Encoding UTF8
 $exactTaskQuestionNote = $false
 try { $exactTaskQuestionNote = Test-OmpTranscriptContainsWorkerQuestionNote -Path $questionNoticePath -TaskId 'orch_abc' -QuestionMarker 'ACCEPT-Q-A' } catch { }
 Check 'question delivery parser recognizes this task-specific inbox note' $exactTaskQuestionNote 'exact task note'
$primaryAuthorizationTranscriptPath = Join-Path $script:HarnessRoot 'primary-authorization-session.jsonl'
$primaryAuthorizationLines = @(
    '{"type":"message","message":{"role":"user","content":[{"type":"text","text":"Dispatch brief: ACCEPT-Q-A; exact token ACCEPT-ANSWER-A must arrive later"}]}}'
    '{"type":"message","message":{"role":"user","content":[{"type":"text","text":"ACCEPT-ANSWER-A"}],"attribution":"user","steering":true,"liveSteered":true}}'
    '{"type":"message","message":{"role":"user","content":[{"type":"text","text":"orchestrator: 1 worker note(s)\n- task-a [question]: ACCEPT-Q-A: should a.js keep its existing export style?"}]}}'
    '{"type":"message","message":{"role":"user","content":[{"type":"text","text":"orchestrator: waiting for ACCEPT-ANSWER-A; no response has been received"}],"attribution":"user"}}'
    '{"type":"message","message":{"role":"user","content":[{"type":"text","text":"Yes—keep the existing export style."}],"attribution":"user","steering":true,"liveSteered":true}}'
    '{"type":"message","message":{"role":"assistant","content":[{"type":"toolCall","name":"orch_answer","arguments":{"taskId":"task-a","text":"ACCEPT-ANSWER-A"}}]}}'
)
Set-Content -LiteralPath $primaryAuthorizationTranscriptPath -Value $primaryAuthorizationLines -Encoding UTF8
$preNoteAuthorization = $true
try {
    $preNoteAuthorization = Test-OmpTranscriptContainsLaterUserAuthorization -Path $primaryAuthorizationTranscriptPath `
        -TaskId 'task-a' -QuestionMarker 'ACCEPT-Q-A' -Token 'ACCEPT-ANSWER-A'
} catch { }
Check 'a direct token before the task-specific inbox note does not authorize reporting' (-not $preNoteAuthorization) 'pre-note token'
$preAuthorizationRelay = $true
try {
    $preAuthorizationRelay = Test-OmpAnswerCallAfterUserAuthorization -Path $primaryAuthorizationTranscriptPath `
        -QuestionMarker 'ACCEPT-Q-A' -Token 'ACCEPT-ANSWER-A' -TaskId 'task-a'
} catch { }
Check 'orch_answer before a direct post-note user token is rejected' (-not $preAuthorizationRelay) 'relay before authorization'
$primaryAuthorizationLines += '{"type":"message","message":{"role":"user","content":[{"type":"text","text":"ACCEPT-ANSWER-A"}],"attribution":"user","steering":true,"liveSteered":true}}'
Add-Content -LiteralPath $primaryAuthorizationTranscriptPath -Value $primaryAuthorizationLines[-1] -Encoding UTF8
$primaryAuthorizationLines += '{"type":"message","message":{"role":"assistant","content":[{"type":"toolCall","name":"orch_answer","arguments":{"taskId":"task-a","text":"ACCEPT-ANSWER-A"}}]}}'
Add-Content -LiteralPath $primaryAuthorizationTranscriptPath -Value $primaryAuthorizationLines[-1] -Encoding UTF8
$laterUserAuthorization = $false
try {
    $laterUserAuthorization = Test-OmpTranscriptContainsLaterUserAuthorization -Path $primaryAuthorizationTranscriptPath `
        -TaskId 'task-a' -QuestionMarker 'ACCEPT-Q-A' -Token 'ACCEPT-ANSWER-A'
} catch { }
Check 'a direct token after the task-specific note authorizes reporting' $laterUserAuthorization 'post-note token'
$authorizedAnswerRelay = $false
try {
    $authorizedAnswerRelay = Test-OmpAnswerCallAfterUserAuthorization -Path $primaryAuthorizationTranscriptPath `
        -QuestionMarker 'ACCEPT-Q-A' -Token 'ACCEPT-ANSWER-A' -TaskId 'task-a'
} catch { }
Check 'orch_answer is accepted only after the exact direct post-note token and for the bound task' $authorizedAnswerRelay 'authorized relay'
$wrongTaskAnswerRelay = $true
try {
    $wrongTaskAnswerRelay = Test-OmpAnswerCallAfterUserAuthorization -Path $primaryAuthorizationTranscriptPath `
        -QuestionMarker 'ACCEPT-Q-A' -Token 'ACCEPT-ANSWER-A' -TaskId 'task-b'
} catch { }
Check 'orch_answer for a different task is rejected after user authorization' (-not $wrongTaskAnswerRelay) 'wrong task relay'
$authorizedAnswerText = Get-OmpAnswerCallAfterUserAuthorization -Path $primaryAuthorizationTranscriptPath `
    -QuestionMarker 'ACCEPT-Q-A' -Token 'ACCEPT-ANSWER-A' -TaskId 'task-a'
Check 'the authorized orch_answer text is the exact direct user message, for the answer dialog to match' ($authorizedAnswerText -ceq 'ACCEPT-ANSWER-A') ([string]$authorizedAnswerText)
Check 'no authorized answer text is returned for another task' ($null -eq (Get-OmpAnswerCallAfterUserAuthorization -Path $primaryAuthorizationTranscriptPath -QuestionMarker 'ACCEPT-Q-A' -Token 'ACCEPT-ANSWER-A' -TaskId 'task-b')) 'wrong task'
$reportAuthorizationWorkerPath = Join-Path $script:HarnessRoot 'report-authorization-worker.jsonl'
$reportAuthorizationWorkerLines = @(
    '{"type":"message","message":{"role":"user","content":[{"type":"text","text":"ACCEPT-TASK-B: wait for a later user message with ACCEPT-REPORT-B"}]}}'
    '{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"I will wait for ACCEPT-REPORT-B"}]}}'
)
Set-Content -LiteralPath $reportAuthorizationWorkerPath -Value $reportAuthorizationWorkerLines -Encoding UTF8
$reportTokenFromBrief = $true
try {
    $reportTokenFromBrief = Test-OmpTranscriptContainsExactUserMessage `
        -Path $reportAuthorizationWorkerPath -Text 'ACCEPT-REPORT-B'
} catch { }
Check 'a token mention in the dispatch brief is not proof of worker delivery' (-not $reportTokenFromBrief) 'brief mention'
$reportAuthorizationWorkerLines += '{"type":"message","message":{"role":"user","content":[{"type":"text","text":"ACCEPT-REPORT-B"}]}}'
Add-Content -LiteralPath $reportAuthorizationWorkerPath -Value $reportAuthorizationWorkerLines[-1] -Encoding UTF8
$reportTokenDelivered = $false
try {
    $reportTokenDelivered = Test-OmpTranscriptContainsExactUserMessage `
        -Path $reportAuthorizationWorkerPath -Text 'ACCEPT-REPORT-B'
} catch { }
Check 'an exact later user message proves the worker received the report token' $reportTokenDelivered 'exact user message'

$reportAuthorizationPrimaryPath = Join-Path $script:HarnessRoot 'report-authorization-primary.jsonl'
$reportAuthorizationPrimaryLines = @(
    '{"type":"message","message":{"role":"assistant","content":[{"type":"toolCall","name":"orch_answer","arguments":{"taskId":"task-other","text":"ACCEPT-REPORT-B"}}]}}'
)
Set-Content -LiteralPath $reportAuthorizationPrimaryPath -Value $reportAuthorizationPrimaryLines -Encoding UTF8
$wrongTaskReportAnswer = $true
try {
    $wrongTaskReportAnswer = Test-OmpTranscriptContainsAnswerToolCall `
        -Path $reportAuthorizationPrimaryPath -TaskId 'task-b' -Text 'ACCEPT-REPORT-B'
} catch { }
Check 'a report-token orch_answer for another task does not prove delivery' (-not $wrongTaskReportAnswer) 'wrong task'
$reportAuthorizationPrimaryLines += '{"type":"message","message":{"role":"assistant","content":[{"type":"toolCall","name":"orch_answer","arguments":{"taskId":"task-b","text":"ACCEPT-REPORT-B"}}]}}'
Add-Content -LiteralPath $reportAuthorizationPrimaryPath -Value $reportAuthorizationPrimaryLines[-1] -Encoding UTF8
$exactReportAnswerCall = $false
try {
    $exactReportAnswerCall = Test-OmpTranscriptContainsAnswerToolCall `
        -Path $reportAuthorizationPrimaryPath -TaskId 'task-b' -Text 'ACCEPT-REPORT-B'
} catch { }
Check 'the exact task-specific orch_answer call proves the primary relayed the token' $exactReportAnswerCall 'exact tool call'

$unreviewedCleanupFact = [pscustomobject]@{ Report = $null; Disposition = ''; Attention = '' }
$unreviewedWithReportFact = [pscustomobject]@{ Report = [pscustomobject]@{ Kind = 'result' }; Disposition = ''; Attention = '' }
$partialReportWithDispositionFact = [pscustomobject]@{ Report = [pscustomobject]@{ Kind = 'partial' }; Disposition = 'Accepted'; Attention = '' }
$reviewedDoneFact = [pscustomobject]@{ Report = [pscustomobject]@{ Kind = 'result' }; Disposition = 'Accepted'; Attention = '' }
$noReportReviewed = $true
$reportWithoutDispositionReviewed = $true
$partialReportReviewed = $true
$acceptedReportReviewed = $false
try { $noReportReviewed = Test-TaskIsReviewedDone -Fact $unreviewedCleanupFact } catch { }
try { $reportWithoutDispositionReviewed = Test-TaskIsReviewedDone -Fact $unreviewedWithReportFact } catch { }
try { $partialReportReviewed = Test-TaskIsReviewedDone -Fact $partialReportWithDispositionFact } catch { }
try { $acceptedReportReviewed = Test-TaskIsReviewedDone -Fact $reviewedDoneFact } catch { }
Check 'cleanup refusal suite requires a reviewed final result, not a partial report or mere disposition' `
    ((-not $noReportReviewed) -and (-not $reportWithoutDispositionReviewed) -and (-not $partialReportReviewed) -and $acceptedReportReviewed) 'review prerequisite'
$exactBlockedK0 = $false
$unrelatedBlockedK0 = $true
$wrongPhaseBlockedK0 = $true
try { $exactBlockedK0 = Test-IsBlockedPrimaryDispositionPrerequisite -PhaseId 'K0' -Message 'a primary disposition Ask blocked worker-dialog approval' } catch { }
try { $unrelatedBlockedK0 = Test-IsBlockedPrimaryDispositionPrerequisite -PhaseId 'K0' -Message 'worktree move failed' } catch { }
try { $wrongPhaseBlockedK0 = Test-IsBlockedPrimaryDispositionPrerequisite -PhaseId 'V0' -Message 'a primary disposition Ask blocked worker-dialog approval' } catch { }
Check 'K0 disposition-Ask block is a known blocked prerequisite' $exactBlockedK0 'exact K0 prerequisite'
Check 'K0 does not suppress an unrelated failure as blocked' (-not $unrelatedBlockedK0) 'unrelated K0 failure'
Check 'the same disposition-Ask message in another phase is not suppressed' (-not $wrongPhaseBlockedK0) 'wrong phase'
$approvalTranscriptPath = Join-Path $script:HarnessRoot 'approval-session.jsonl'
Set-Content -LiteralPath $approvalTranscriptPath -Value '{"type":"message","message":{"role":"assistant","content":[{"type":"toolCall","name":"orch_approve_dialog","arguments":{"taskId":"orch_abc","keys":["enter"]}}]}}' -Encoding UTF8
$approvalToolCallMatches = $false
try { $approvalToolCallMatches = Test-OmpApprovalToolCall -Path $approvalTranscriptPath -TaskId 'orch_abc' -Keys @('enter') } catch { }
Check 'primary transcript proves the exact approval task and key call' $approvalToolCallMatches 'task/key toolCall'
$wrongApprovalToolTask = $true
try { $wrongApprovalToolTask = Test-OmpApprovalToolCall -Path $approvalTranscriptPath -TaskId 'orch_other' -Keys @('enter') } catch { }
Check 'primary transcript refuses an approval tool call for another task' (-not $wrongApprovalToolTask) 'wrong task'
$wrongApprovalToolKeys = $true
try { $wrongApprovalToolKeys = Test-OmpApprovalToolCall -Path $approvalTranscriptPath -TaskId 'orch_abc' -Keys @('escape') } catch { }
Check 'primary transcript refuses a different approval key set' (-not $wrongApprovalToolKeys) 'wrong keys'
$reviewTranscriptPath = Join-Path $script:HarnessRoot 'review-session.jsonl'
Set-Content -LiteralPath $reviewTranscriptPath -Value '{"type":"message","message":{"role":"assistant","content":[{"type":"toolCall","name":"orch_review","arguments":{"taskId":"orch_abc","outcome":"integrated","disposition":"Accepted the reported result."}}]}}' -Encoding UTF8
Check 'review transcript ties confirmation to the exact tool call' (Test-OmpReviewToolCall -Path $reviewTranscriptPath -TaskId 'orch_abc' -Disposition 'Accepted the reported result.') 'task/decision'
Check 'review transcript refuses a different outcome' (-not (Test-OmpReviewToolCall -Path $reviewTranscriptPath -TaskId 'orch_abc' -Disposition 'Accepted the reported result.' -Outcome 'rejected')) 'wrong outcome'
$noOutcomeTranscriptPath = Join-Path $script:HarnessRoot 'review-session-no-outcome.jsonl'
Set-Content -LiteralPath $noOutcomeTranscriptPath -Value '{"type":"message","message":{"role":"assistant","content":[{"type":"toolCall","name":"orch_review","arguments":{"taskId":"orch_abc","disposition":"Accepted the reported result."}}]}}' -Encoding UTF8
Check 'review transcript refuses a call with no outcome' (-not (Test-OmpReviewToolCall -Path $noOutcomeTranscriptPath -TaskId 'orch_abc' -Disposition 'Accepted the reported result.' -Outcome 'integrated')) 'missing outcome'
Check 'review transcript refuses a different disposition' (-not (Test-OmpReviewToolCall -Path $reviewTranscriptPath -TaskId 'orch_abc' -Disposition 'Rejected the result.')) 'wrong decision'
Check 'id-only restored session proves refusal from its pane' (Test-RestoredReviewRefusal -TaskId 'orch_abc' -Disposition 'RESTORED-123' -Screen 'orch_review RESTORED-123 -> already_decided') 'id-only proof'
Check 'id-only restored session refuses a screen with only the submitted prompt' (-not (Test-RestoredReviewRefusal -TaskId 'orch_abc' -Disposition 'RESTORED-123' -Screen 'orch_review RESTORED-123')) 'no error'
Check 'id-only restored session refuses an unrelated decision' (-not (Test-RestoredReviewRefusal -TaskId 'orch_abc' -Disposition 'RESTORED-123' -Screen 'orch_review OTHER -> already_decided')) 'wrong decision'

$reviewScreen = @'
Earlier confirmation: Record review disposition
Task orch_old; Accepted the reported result.
Record review disposition
Confirm that you reviewed the worker's final report for task orch_abc and chose this outcome: integrated
Note: Accepted the reported result.
Stored report (revision 7):
Summary: "Added subtract to a.js"
Branch: "orch/abc"
Evidence: ["commit 0123456789abcdef"]
Checks: ["go test ./internal/coord ok"]
Risk: "No known risk"
This closes the task and may start queued dependent work. Decline if you have not reviewed the report.
Yes
No
'@
Check 'review modal matches the active task, revision and disposition' (Test-PrimaryReviewConfirmScreen -Screen $reviewScreen -TaskId 'orch_abc' -Revision 7 -Disposition 'Accepted the reported result.') 'review modal'
# A report that states outcomes previews one line per check ("name: outcome (detail)"), which the
# dialog prints in the same Checks list; the matcher stops at the "Stored report" line, so that
# line's content is data.
$statedChecksScreen = $reviewScreen -replace 'Checks: \["go test \./internal/coord ok"\]', 'Checks: ["go test ./internal/coord: passed","go vet ./...: failed (2 findings)"]'
Check 'review modal accepts a stored report whose checks state outcomes' ($statedChecksScreen -ne $reviewScreen -and (Test-PrimaryReviewConfirmScreen -Screen $statedChecksScreen -TaskId 'orch_abc' -Revision 7 -Disposition 'Accepted the reported result.')) 'stated checks'
Check 'review modal rejects a different task' (-not (Test-PrimaryReviewConfirmScreen -Screen $reviewScreen -TaskId 'orch_other' -Revision 7 -Disposition 'Accepted the reported result.')) 'wrong task'
Check 'review modal rejects a different report revision' (-not (Test-PrimaryReviewConfirmScreen -Screen $reviewScreen -TaskId 'orch_abc' -Revision 8 -Disposition 'Accepted the reported result.')) 'wrong revision'
Check 'review modal rejects a different decision' (-not (Test-PrimaryReviewConfirmScreen -Screen $reviewScreen -TaskId 'orch_abc' -Revision 7 -Disposition 'Rejected the result.')) 'wrong disposition'
Check 'review modal rejects a longer different disposition that contains the expected text' (-not (Test-PrimaryReviewConfirmScreen -Screen ($reviewScreen -replace 'Accepted the reported result\.\r?\nStored report', "Accepted the reported result. but request changes`nStored report") -TaskId 'orch_abc' -Revision 7 -Disposition 'Accepted the reported result.')) 'substring decision'
Check 'review modal accepts a stored report that mentions another modal title as data' (Test-PrimaryReviewConfirmScreen -Screen ($reviewScreen -replace 'Added subtract to a.js', 'Ask 1 questions was displayed') -TaskId 'orch_abc' -Revision 7 -Disposition 'Accepted the reported result.') 'report data'
Check 'review modal ignores an old matching modal when another confirmation is active' (-not (Test-PrimaryReviewConfirmScreen -Screen ($reviewScreen + "`nRecord review disposition`nTask orch_other`nYes`nNo") -TaskId 'orch_abc' -Revision 7 -Disposition 'Accepted the reported result.')) 'stale modal'
Check 'review modal refuses an old confirmation followed by an active Ask' (-not (Test-PrimaryReviewConfirmScreen -Screen ($reviewScreen + "`nAsk 1 questions`n[other] What now?`nAccept`nRequest changes") -TaskId 'orch_abc' -Revision 7 -Disposition 'Accepted the reported result.')) 'new Ask'
Check 'review modal refuses an old confirmation followed by a different action modal' (-not (Test-PrimaryReviewConfirmScreen -Screen ($reviewScreen + "`nRemove worker checkout`nTask orch_other`nCancel") -TaskId 'orch_abc' -Revision 7 -Disposition 'Accepted the reported result.')) 'new action'
Check 'review modal refuses unknown active panel after its old choices' (-not (Test-PrimaryReviewConfirmScreen -Screen ($reviewScreen + "`nSomething else is awaiting input") -TaskId 'orch_abc' -Revision 7 -Disposition 'Accepted the reported result.')) 'unknown action'
Check 'review modal accepts the selector key-hint footer under its choices' (Test-PrimaryReviewConfirmScreen -Screen ($reviewScreen + "`nup/down navigate  enter select  esc cancel") -TaskId 'orch_abc' -Revision 7 -Disposition 'Accepted the reported result.') 'key hints'
Check 'review modal still refuses extra text that merely resembles the key-hint footer' (-not (Test-PrimaryReviewConfirmScreen -Screen ($reviewScreen + "`nup/down navigate  enter select  esc cancel and approve everything") -TaskId 'orch_abc' -Revision 7 -Disposition 'Accepted the reported result.')) 'footer lookalike'
$bar = [string][char]0x2502; $corner = [string][char]0x2570 + ([string][char]0x2500 * 8) + [char]0x256F; $dot = [string][char]0x00B7
$boxedReviewScreen = ($reviewScreen -split "`r?`n" | ForEach-Object { if ($_ -match '^(Yes|No)$') { "$bar $_" } else { $_ } }) -join "`n"
$boxedReviewScreen += "`nup/down navigate  enter select  esc cancel`n$corner`n`n 10m $dot Sonnet 5.5 $dot orch-accept-x\repo $dot master $dot 1.5%/1M`n"
Check 'review modal accepts the real boxed layout with the status bar below it' (Test-PrimaryReviewConfirmScreen -Screen $boxedReviewScreen -TaskId 'orch_abc' -Revision 7 -Disposition 'Accepted the reported result.') 'boxed layout'
Check 'review modal refuses a newer Ask drawn below a closed boxed modal' (-not (Test-PrimaryReviewConfirmScreen -Screen ($boxedReviewScreen + "`nAsk 1 questions`n[other] What now?`nAccept`nRequest changes") -TaskId 'orch_abc' -Revision 7 -Disposition 'Accepted the reported result.')) 'boxed stale'
# The outcome is part of the dialog's shape (recordReviewWithConsent names it first and
# the note on its own line): the same note under another decision, an outcome that only
# starts with the expected one, or the pre-outcome wording are different confirmations.
$rejectedBody = 'This closes the task and frees its slot and paths, but tasks that depend on it stay queued.'
$rejectedScreen = ($reviewScreen -replace 'chose this outcome: integrated', 'chose this outcome: rejected') -replace 'This closes the task and may start queued dependent work\.', $rejectedBody
Check 'review modal refuses a dialog whose outcome is not the expected one' (-not (Test-PrimaryReviewConfirmScreen -Screen $rejectedScreen -TaskId 'orch_abc' -Revision 7 -Disposition 'Accepted the reported result.')) 'wrong outcome'
Check 'review modal accepts a non-accepting outcome with its own closing sentence' (Test-PrimaryReviewConfirmScreen -Screen $rejectedScreen -TaskId 'orch_abc' -Revision 7 -Disposition 'Accepted the reported result.' -Outcome 'rejected') 'rejected outcome'
Check 'review modal refuses a non-accepting outcome that still says it releases dependents' (-not (Test-PrimaryReviewConfirmScreen -Screen ($reviewScreen -replace 'chose this outcome: integrated', 'chose this outcome: rejected') -TaskId 'orch_abc' -Revision 7 -Disposition 'Accepted the reported result.' -Outcome 'rejected')) 'release claim'
Check 'review modal refuses an accepting outcome that says dependents stay queued' (-not (Test-PrimaryReviewConfirmScreen -Screen ($reviewScreen -replace 'This closes the task and may start queued dependent work\.', $rejectedBody) -TaskId 'orch_abc' -Revision 7 -Disposition 'Accepted the reported result.')) 'queued claim'
Check 'review modal refuses an outcome that merely starts with the expected one' (-not (Test-PrimaryReviewConfirmScreen -Screen ($reviewScreen -replace 'chose this outcome: integrated', 'chose this outcome: integrated-later') -TaskId 'orch_abc' -Revision 7 -Disposition 'Accepted the reported result.')) 'prefix outcome'
Check 'review modal refuses the old wording that has no outcome' (-not (Test-PrimaryReviewConfirmScreen -Screen ($reviewScreen -replace 'chose this outcome: integrated\r?\nNote: ', 'chose this disposition: ') -TaskId 'orch_abc' -Revision 7 -Disposition 'Accepted the reported result.')) 'old wording'
Check 'review modal refuses a note that is the outcome word in disguise' (-not (Test-PrimaryReviewConfirmScreen -Screen ($reviewScreen -replace 'Note: Accepted the reported result\.', 'Note: integrated') -TaskId 'orch_abc' -Revision 7 -Disposition 'Accepted the reported result.')) 'wrong note'

# --- the ticket confirmation modals: the exact ticket, revision and decision ----
$ticketTitle = 'ACCEPT TICKET: add search'
$ticketMoveScreen = @'
Earlier confirmation: Move ticket to todo
Confirm moving ticket tkt_old to todo.
Move ticket to todo
Confirm moving ticket tkt_abc123 to todo.
Title: "ACCEPT TICKET: add search"
Now: backlog (stage backlog), revision 4
In this version a move only records the ticket as ready for work; it starts nothing. No task, worktree or agent is created, and dispatching work for the ticket is a separate step.
Yes
No
'@
$moveArgs = @{ Kind = 'move'; TicketId = 'tkt_abc123'; TicketTitle = $ticketTitle; Revision = 4 }
Check 'ticket move modal matches the active ticket, title and revision' (Test-PrimaryTicketConfirmScreen -Screen $ticketMoveScreen @moveArgs) 'move modal'
Check 'ticket move modal rejects a different ticket' (-not (Test-PrimaryTicketConfirmScreen -Screen $ticketMoveScreen -Kind 'move' -TicketId 'tkt_other' -TicketTitle $ticketTitle -Revision 4)) 'wrong ticket'
Check 'ticket move modal rejects a different revision' (-not (Test-PrimaryTicketConfirmScreen -Screen $ticketMoveScreen -Kind 'move' -TicketId 'tkt_abc123' -TicketTitle $ticketTitle -Revision 5)) 'wrong revision'
Check 'ticket move modal rejects a different title' (-not (Test-PrimaryTicketConfirmScreen -Screen $ticketMoveScreen -Kind 'move' -TicketId 'tkt_abc123' -TicketTitle 'something else' -Revision 4)) 'wrong title'
Check 'ticket move modal is not the ticket decision modal' (-not (Test-PrimaryTicketConfirmScreen -Screen $ticketMoveScreen -Kind 'review' -TicketId 'tkt_abc123' -TicketTitle $ticketTitle -Revision 4 -Outcome 'integrated' -Disposition 'x' -LinkedTaskIds @('orch_a'))) 'wrong kind'
Check 'ticket move modal refuses a newer Ask below it' (-not (Test-PrimaryTicketConfirmScreen -Screen ($ticketMoveScreen + "`nAsk 1 questions`n[other] What now?`nAccept`nRequest changes") @moveArgs)) 'new Ask'
Check 'ticket move modal refuses a later different action modal' (-not (Test-PrimaryTicketConfirmScreen -Screen ($ticketMoveScreen + "`nRemove worker checkout`nTask orch_other`nCancel") @moveArgs)) 'new action'
Check 'ticket move modal ignores an old matching modal when another is active' (-not (Test-PrimaryTicketConfirmScreen -Screen ($ticketMoveScreen + "`nMove ticket to todo`nConfirm moving ticket tkt_other to todo.`nYes`nNo") @moveArgs)) 'stale modal'
$ticketBoxedScreen = ($ticketMoveScreen -split "`r?`n" | ForEach-Object { if ($_ -match '^(Yes|No)$') { "$bar $_" } else { $_ } }) -join "`n"
$ticketBoxedScreen += "`nup/down navigate  enter select  esc cancel`n$corner`n`n 10m $dot Sonnet 5.5 $dot orch-accept-x\repo $dot master $dot 1.5%/1M`n"
Check 'ticket move modal accepts the real boxed layout with the status bar below it' (Test-PrimaryTicketConfirmScreen -Screen $ticketBoxedScreen @moveArgs) 'boxed layout'
Check 'ticket move modal refuses a newer Ask drawn below a closed boxed modal' (-not (Test-PrimaryTicketConfirmScreen -Screen ($ticketBoxedScreen + "`nAsk 1 questions`n[other] What now?`nAccept`nRequest changes") @moveArgs)) 'boxed stale'
Check 'ticket modal refuses an empty ticket id' (-not (Test-PrimaryTicketConfirmScreen -Screen $ticketMoveScreen -Kind 'move' -TicketId '' -TicketTitle $ticketTitle -Revision 4)) 'empty id'
Check 'ticket modal refuses a zero revision' (-not (Test-PrimaryTicketConfirmScreen -Screen $ticketMoveScreen -Kind 'move' -TicketId 'tkt_abc123' -TicketTitle $ticketTitle -Revision 0)) 'zero revision'

$ticketReviewScreen = @'
Record ticket decision
Confirm closing ticket tkt_abc123 with this outcome: integrated
Title: "ACCEPT TICKET: add search"
Note: Accepted the reported result.
Ticket: revision 5, stage ready_for_review; linked tasks: orch_aaaa11112222
This records your decision on the ticket only: it changes no task and merges nothing. A ticket cannot be closed while any of its linked tasks is still open.
Yes
No
'@
$ticketReviewArgs = @{ Kind = 'review'; TicketId = 'tkt_abc123'; TicketTitle = $ticketTitle; Revision = 5; Outcome = 'integrated'; Disposition = 'Accepted the reported result.'; LinkedTaskIds = @('orch_aaaa11112222') }
Check 'ticket decision modal matches the ticket, revision, outcome, note and linked task' (Test-PrimaryTicketConfirmScreen -Screen $ticketReviewScreen @ticketReviewArgs) 'review modal'
Check 'ticket decision modal rejects a different outcome' (-not (Test-PrimaryTicketConfirmScreen -Screen $ticketReviewScreen -Kind 'review' -TicketId 'tkt_abc123' -TicketTitle $ticketTitle -Revision 5 -Outcome 'rejected' -Disposition 'Accepted the reported result.' -LinkedTaskIds @('orch_aaaa11112222'))) 'wrong outcome'
Check 'ticket decision modal rejects a different note' (-not (Test-PrimaryTicketConfirmScreen -Screen $ticketReviewScreen -Kind 'review' -TicketId 'tkt_abc123' -TicketTitle $ticketTitle -Revision 5 -Outcome 'integrated' -Disposition 'Rejected.' -LinkedTaskIds @('orch_aaaa11112222'))) 'wrong note'
Check 'ticket decision modal rejects a different linked task' (-not (Test-PrimaryTicketConfirmScreen -Screen $ticketReviewScreen -Kind 'review' -TicketId 'tkt_abc123' -TicketTitle $ticketTitle -Revision 5 -Outcome 'integrated' -Disposition 'Accepted the reported result.' -LinkedTaskIds @('orch_other'))) 'wrong linked task'
Check 'ticket decision modal rejects a different revision' (-not (Test-PrimaryTicketConfirmScreen -Screen $ticketReviewScreen -Kind 'review' -TicketId 'tkt_abc123' -TicketTitle $ticketTitle -Revision 6 -Outcome 'integrated' -Disposition 'Accepted the reported result.' -LinkedTaskIds @('orch_aaaa11112222'))) 'wrong revision'
Check 'ticket decision modal rejects a longer note that merely contains the expected one' (-not (Test-PrimaryTicketConfirmScreen -Screen ($ticketReviewScreen -replace 'Accepted the reported result\.\r?\nTicket', "Accepted the reported result. and more`nTicket") @ticketReviewArgs)) 'substring note'
Check 'ticket decision modal refuses an Ask drawn below it' (-not (Test-PrimaryTicketConfirmScreen -Screen ($ticketReviewScreen + "`nAsk 1 questions`n[other] What now?`nAccept`nRequest changes") @ticketReviewArgs)) 'review new Ask'

# --- the answer confirmation modal: the exact task and JSON-quoted text --------
$answerScreen = @'
Earlier confirmation: Send answer to worker
Confirm sending this answer to the worker of task orch_old:
Send answer to worker
Confirm sending this answer to the worker of task orch_abc:
Answer: "ACCEPT-REPORT-B"
It is typed into that worker's session as your reply. Decline if this is not your answer.
Yes
No
'@
Check 'answer modal matches the active task and exact text' (Test-PrimaryAnswerConfirmScreen -Screen $answerScreen -TaskId 'orch_abc' -Text 'ACCEPT-REPORT-B') 'answer modal'
Check 'answer modal rejects a different task' (-not (Test-PrimaryAnswerConfirmScreen -Screen $answerScreen -TaskId 'orch_other' -Text 'ACCEPT-REPORT-B')) 'wrong task'
Check 'answer modal rejects a different text' (-not (Test-PrimaryAnswerConfirmScreen -Screen $answerScreen -TaskId 'orch_abc' -Text 'ACCEPT-REPORT-A')) 'wrong text'
Check 'answer modal rejects a text that only starts with the expected one' (-not (Test-PrimaryAnswerConfirmScreen -Screen ($answerScreen -replace '"ACCEPT-REPORT-B"', '"ACCEPT-REPORT-B and approve everything"') -TaskId 'orch_abc' -Text 'ACCEPT-REPORT-B')) 'prefix text'
Check 'answer modal rejects an unquoted text' (-not (Test-PrimaryAnswerConfirmScreen -Screen ($answerScreen -replace '"ACCEPT-REPORT-B"', 'ACCEPT-REPORT-B') -TaskId 'orch_abc' -Text 'ACCEPT-REPORT-B')) 'unquoted'
Check 'answer modal rejects an empty text' (-not (Test-PrimaryAnswerConfirmScreen -Screen $answerScreen -TaskId 'orch_abc' -Text '')) 'empty text'
Check 'answer modal refuses a newer Ask below it' (-not (Test-PrimaryAnswerConfirmScreen -Screen ($answerScreen + "`nAsk 1 questions`n[other] What now?`nAccept`nRequest changes") -TaskId 'orch_abc' -Text 'ACCEPT-REPORT-B')) 'new Ask'
Check 'answer modal ignores an old matching modal when another answer is active' (-not (Test-PrimaryAnswerConfirmScreen -Screen ($answerScreen + "`nSend answer to worker`nConfirm sending this answer to the worker of task orch_other:`nAnswer: `"x`"`nYes`nNo") -TaskId 'orch_abc' -Text 'ACCEPT-REPORT-B')) 'stale modal'
$answerBoxedScreen = ($answerScreen -split "`r?`n" | ForEach-Object { if ($_ -match '^(Yes|No)$') { "$bar $_" } else { $_ } }) -join "`n"
$answerBoxedScreen += "`nup/down navigate  enter select  esc cancel`n$corner`n`n 10m $dot Sonnet 5.5 $dot orch-accept-x\repo $dot master $dot 1.5%/1M`n"
Check 'answer modal accepts the real boxed layout with the status bar below it' (Test-PrimaryAnswerConfirmScreen -Screen $answerBoxedScreen -TaskId 'orch_abc' -Text 'ACCEPT-REPORT-B') 'boxed layout'
# The dialog quotes with JSON.stringify, so a quote, a backslash and a newline are
# shown escaped; the matcher predicts that exact form.
$specialAnswer = "Keep the `"old`" API.`nPath: C:\repo"
$specialQuoted = ConvertTo-JsonStringLiteral -Text $specialAnswer
Check 'JSON quoting matches JSON.stringify for quotes, backslashes and newlines' ($specialQuoted -ceq '"Keep the \"old\" API.\nPath: C:\\repo"') $specialQuoted
Check 'JSON quoting escapes other control characters as \u escapes' ((ConvertTo-JsonStringLiteral -Text ("a" + [char]1 + "b`t")) -ceq '"a\u0001b\t"') (ConvertTo-JsonStringLiteral -Text ("a" + [char]1 + "b`t"))
$specialAnswerScreen = $answerScreen.Replace('"ACCEPT-REPORT-B"', $specialQuoted)
Check 'answer modal matches a JSON-quoted text with escapes' (Test-PrimaryAnswerConfirmScreen -Screen $specialAnswerScreen -TaskId 'orch_abc' -Text $specialAnswer) 'escaped text'
Check 'answer modal refuses the raw text where the dialog shows it escaped' (-not (Test-PrimaryAnswerConfirmScreen -Screen ($answerScreen.Replace('"ACCEPT-REPORT-B"', '"' + $specialAnswer + '"')) -TaskId 'orch_abc' -Text $specialAnswer)) 'raw text'
# A pipe inside the answer is text, not border chrome: it must be shown exactly.
$pipeAnswer = 'use a|b'
$pipeScreen = $answerScreen.Replace('"ACCEPT-REPORT-B"', '"use a|b"')
Check 'answer modal matches an answer that contains a literal pipe' (Test-PrimaryAnswerConfirmScreen -Screen $pipeScreen -TaskId 'orch_abc' -Text $pipeAnswer) 'pipe text'
Check 'answer modal refuses a dialog that shows a space where the answer has a pipe' (-not (Test-PrimaryAnswerConfirmScreen -Screen ($answerScreen.Replace('"ACCEPT-REPORT-B"', '"use a b"')) -TaskId 'orch_abc' -Text $pipeAnswer)) 'pipe dropped'
Check 'answer modal refuses a dialog that shows a pipe the answer does not have' (-not (Test-PrimaryAnswerConfirmScreen -Screen $pipeScreen -TaskId 'orch_abc' -Text 'use a b')) 'pipe added'
Check 'answer modal refuses a dialog that shows extra pipes inside the answer' (-not (Test-PrimaryAnswerConfirmScreen -Screen ($answerScreen.Replace('"ACCEPT-REPORT-B"', '"use a||b"')) -TaskId 'orch_abc' -Text $pipeAnswer)) 'pipe doubled'
$pipeBoxedScreen = ($pipeScreen -split "`r?`n" | ForEach-Object { "$bar $_ $bar" }) -join "`n"
Check 'answer modal matches a pipe-containing answer inside the boxed border' (Test-PrimaryAnswerConfirmScreen -Screen $pipeBoxedScreen -TaskId 'orch_abc' -Text $pipeAnswer) 'boxed pipe'
# The layout a live OMP session drew (acceptance run ebe42b49): the title sits in
# the top border between box-drawing runs, every body line is framed by vertical
# bars, and the key hints use an arrow pair and icon glyphs instead of words.
$dash = [string][char]0x2500
$topLeft = [string][char]0x256D; $topRight = [string][char]0x256E; $bottomLeft = [string][char]0x2570; $bottomRight = [string][char]0x256F
$liveFooter = [string][char]0x2191 + '/' + [string][char]0x2193 + ' navigate  ' + [char]::ConvertFromUtf32(0xF0311) + ' select  ' + [char]::ConvertFromUtf32(0xF12B7) + ' cancel'
$liveChoice = [string][char]0xF054
function New-LiveAnswerScreen {
    param([string]$TaskId, [string]$QuotedText)
    $framed = @('', "Confirm sending this answer to the worker of task ${TaskId}:", "Answer: $QuotedText",
        "It is typed into that worker's session as your reply. Decline if this is not your answer.", '',
        " $liveChoice Yes", '   No', '', $liveFooter, '') | ForEach-Object { "$bar $($_.PadRight(100)) $bar" }
    return (@(
        ' Answer worker',
        "  taskId=`"$TaskId`", text=$QuotedText",
        '',
        "$topLeft$dash Send answer to worker $($dash * 80)$topRight"
    ) + $framed + @(
        "$bottomLeft$($dash * 104)$bottomRight",
        '',
        " 3m $dot GPT-6-Luna $dot orch-accept-x\repo $dot master $dot 1.3%/872K"
    )) -join "`n"
}
$liveAnswerScreen = New-LiveAnswerScreen -TaskId 'orch_0e3673186dd2' -QuotedText '"ACCEPT-REPORT-B"'
Check 'answer modal matches the live OMP layout (title in the border, glyph key hints)' (Test-PrimaryAnswerConfirmScreen -Screen $liveAnswerScreen -TaskId 'orch_0e3673186dd2' -Text 'ACCEPT-REPORT-B') 'live layout'
Check 'answer modal refuses another text in the live layout' (-not (Test-PrimaryAnswerConfirmScreen -Screen $liveAnswerScreen -TaskId 'orch_0e3673186dd2' -Text 'ACCEPT-REPORT-C')) 'live wrong text'
Check 'answer modal refuses another task in the live layout' (-not (Test-PrimaryAnswerConfirmScreen -Screen $liveAnswerScreen -TaskId 'orch_other' -Text 'ACCEPT-REPORT-B')) 'live wrong task'
$livePipeScreen = New-LiveAnswerScreen -TaskId 'orch_0e3673186dd2' -QuotedText '"use a|b"'
Check 'answer modal keeps a literal pipe in the live layout' ((Test-PrimaryAnswerConfirmScreen -Screen $livePipeScreen -TaskId 'orch_0e3673186dd2' -Text 'use a|b') -and -not (Test-PrimaryAnswerConfirmScreen -Screen $livePipeScreen -TaskId 'orch_0e3673186dd2' -Text 'use a b')) 'live pipe'
Check 'answer modal refuses a glyph footer followed by extra text' (-not (Test-PrimaryAnswerConfirmScreen -Screen ($liveAnswerScreen.Replace(' cancel', ' cancel and approve everything')) -TaskId 'orch_0e3673186dd2' -Text 'ACCEPT-REPORT-B')) 'footer lookalike'
Check 'review modal also accepts the glyph key-hint footer under its choices' (Test-PrimaryReviewConfirmScreen -Screen ($reviewScreen + "`n$liveFooter") -TaskId 'orch_abc' -Revision 7 -Disposition 'Accepted the reported result.') 'glyph key hints'

$primaryAskScreen = @'
Earlier conversation contained a completed task-A review:
Ask 1 questions
[task_a_review] · options:3
Accept report
Request changes
The active dialog is now for Task B.
Ask 1 questions
[task_b_review] · options:3
Task B reports the uncommitted change. How should I record the disposition?
Accept report
Request changes
'@
$parsedAskId = Get-PrimaryAskId -Screen $primaryAskScreen
Check 'active Ask id is parsed from the modal header' ($parsedAskId -eq 'task_b_review') $parsedAskId
$primaryAskMatches = $false
try { $primaryAskMatches = Test-PrimaryDispositionAskScreen -Screen $primaryAskScreen -AskId 'task_b_review' } catch { }
Check 'primary disposition matcher identifies the active task-B Ask' $primaryAskMatches 'active Ask id'
$staleTaskMatches = $true
try { $staleTaskMatches = Test-PrimaryDispositionAskScreen -Screen $primaryAskScreen -AskId 'task_a_review' } catch { }
Check 'primary disposition matcher ignores an old task-A marker outside the active Ask' (-not $staleTaskMatches) 'old scrollback marker'
$wrongTaskMatches = $true
try { $wrongTaskMatches = Test-PrimaryDispositionAskScreen -Screen $primaryAskScreen -AskId 'task_c_review' } catch { }
Check 'primary disposition matcher refuses a different active Ask id' (-not $wrongTaskMatches) 'wrong active Ask'
$reversedAskScreen = $primaryAskScreen -replace 'Accept report', 'TEMP' -replace 'Request changes', 'Accept report' -replace 'TEMP', 'Request changes'
$reversedAskMatches = $true
try { $reversedAskMatches = Test-PrimaryDispositionAskScreen -Screen $reversedAskScreen -AskId 'task_b_review' } catch { }
Check 'primary disposition matcher refuses when Accept is not the first choice' (-not $reversedAskMatches) 'wrong choice order'
$primaryWorkerQuestionAskScreen = @'
Older scrollback mentions another worker question.
Ask 1 questions
[answer_a] · options:2
Should a.js keep its existing export style? The worker reports only after the exact token ACCEPT-ANSWER-A.
Yes — ACCEPT-ANSWER-A
Authorize reporting and keep the existing export style.
No — do not authorize
Do not report.
'@
$workerQuestionAskMatches = $false
try { $workerQuestionAskMatches = Test-PrimaryWorkerQuestionAskScreen -Screen $primaryWorkerQuestionAskScreen -AskId 'answer_a' -QuestionText 'Should a.js keep its existing export style?' -AnswerToken 'ACCEPT-ANSWER-A' } catch { }
Check 'worker-question matcher recognizes only the exact active answer Ask' $workerQuestionAskMatches 'answer_a + exact token'
$wrongWorkerQuestionAskMatches = $true
try { $wrongWorkerQuestionAskMatches = Test-PrimaryWorkerQuestionAskScreen -Screen $primaryWorkerQuestionAskScreen -AskId 'answer_b' -QuestionText 'Should a.js keep its existing export style?' -AnswerToken 'ACCEPT-ANSWER-A' } catch { }
Check 'worker-question matcher refuses a different active Ask id' (-not $wrongWorkerQuestionAskMatches) 'wrong Ask id'
$wrongWorkerQuestionTokenMatches = $true
try { $wrongWorkerQuestionTokenMatches = Test-PrimaryWorkerQuestionAskScreen -Screen $primaryWorkerQuestionAskScreen -AskId 'answer_a' -QuestionText 'Should a.js keep its existing export style?' -AnswerToken 'ACCEPT-ANSWER-B' } catch { }
Check 'worker-question matcher refuses a different authorization token' (-not $wrongWorkerQuestionTokenMatches) 'wrong token'
$reversedWorkerQuestionAskScreen = $primaryWorkerQuestionAskScreen -replace 'Yes — ACCEPT-ANSWER-A', 'TEMP' -replace 'No — do not authorize', 'Yes — ACCEPT-ANSWER-A' -replace 'TEMP', 'No — do not authorize'
$reversedWorkerQuestionAskMatches = $true
try { $reversedWorkerQuestionAskMatches = Test-PrimaryWorkerQuestionAskScreen -Screen $reversedWorkerQuestionAskScreen -AskId 'answer_a' -QuestionText 'Should a.js keep its existing export style?' -AnswerToken 'ACCEPT-ANSWER-A' } catch { }
Check 'worker-question matcher refuses when the authorized token is not the first option' (-not $reversedWorkerQuestionAskMatches) 'reversed authorization choice'
$primaryWorkerAnswerChoiceAskScreen = @'
Ask 1 questions
[export-style] · options:2
How should I answer the worker's question about a.js export style?
Yes, keep the existing style
Preserve current exports and follow their pattern for subtract.
No, use a different style
Give the worker a different export style instruction.
Other (type your own)
'@
$workerAnswerChoiceMatches = $false
try { $workerAnswerChoiceMatches = Test-PrimaryWorkerAnswerChoiceAskScreen -Screen $primaryWorkerAnswerChoiceAskScreen -AskId 'export-style' -QuestionText 'Should a.js keep its existing export style?' } catch { }
Check 'worker-answer matcher accepts the exact primary answer-choice Ask with a dynamic id' $workerAnswerChoiceMatches 'export-style + exact yes/no options'
$wrongWorkerAnswerChoiceMatches = $true
try { $wrongWorkerAnswerChoiceMatches = Test-PrimaryWorkerAnswerChoiceAskScreen -Screen $primaryWorkerAnswerChoiceAskScreen -AskId 'export-style' -QuestionText 'Should b.js keep its existing export style?' } catch { }
Check 'worker-answer matcher refuses a choice Ask for a different worker question' (-not $wrongWorkerAnswerChoiceMatches) 'wrong worker question'
$reversedWorkerAnswerChoiceScreen = $primaryWorkerAnswerChoiceAskScreen -replace 'Yes, keep the existing style', 'TEMP' -replace 'No, use a different style', 'Yes, keep the existing style' -replace 'TEMP', 'No, use a different style'
$reversedWorkerAnswerChoiceMatches = $true
try { $reversedWorkerAnswerChoiceMatches = Test-PrimaryWorkerAnswerChoiceAskScreen -Screen $reversedWorkerAnswerChoiceScreen -AskId 'export-style' -QuestionText 'Should a.js keep its existing export style?' } catch { }
Check 'worker-answer matcher refuses when the authorized style choice is not first' (-not $reversedWorkerAnswerChoiceMatches) 'reversed worker answer choices'
$primaryWorkerExactQuestionAskScreen = @'
Ask 1 questions
[style] · options:2
Should a.js keep its existing export style?
Yes, keep it
Preserve the current style while adding subtract.
No, change it
Have the worker change to another export style.
Other (type your own)
'@
$exactWorkerQuestionAskMatches = $false
try { $exactWorkerQuestionAskMatches = Test-PrimaryWorkerAnswerChoiceAskScreen -Screen $primaryWorkerExactQuestionAskScreen -AskId 'style' -QuestionText 'Should a.js keep its existing export style?' } catch { }
Check 'worker-answer matcher accepts the exact question with the actual style/no-style options' $exactWorkerQuestionAskMatches 'style + exact question and options'
$wrongExactWorkerQuestionAskMatches = $true
try { $wrongExactWorkerQuestionAskMatches = Test-PrimaryWorkerAnswerChoiceAskScreen -Screen $primaryWorkerExactQuestionAskScreen -AskId 'style' -QuestionText 'Should b.js keep its existing export style?' } catch { }
Check 'worker-answer matcher refuses the exact-question Ask for another file' (-not $wrongExactWorkerQuestionAskMatches) 'wrong file question'
$approvalConfirmScreen = @'
Open acceptance dialog as instructed
Ask
ACCEPT-Q-DIALOG: which contract should b.js use?
keep the current contract
change the contract
Keys: enter
Yes
No
'@
$approvalConfirmMatches = $false
try { $approvalConfirmMatches = Test-PrimaryApprovalConfirmScreen -Screen $approvalConfirmScreen -DialogMarker 'ACCEPT-Q-DIALOG' -ExpectedQuestion 'ACCEPT-Q-DIALOG: which contract should b.js use?' -Keys @('enter') } catch { }
Check 'approval modal matches the exact dialog, key and confirmation buttons' $approvalConfirmMatches 'exact confirmation'
$wrongApprovalKeyMatches = $true
try { $wrongApprovalKeyMatches = Test-PrimaryApprovalConfirmScreen -Screen $approvalConfirmScreen -DialogMarker 'ACCEPT-Q-DIALOG' -ExpectedQuestion 'ACCEPT-Q-DIALOG: which contract should b.js use?' -Keys @('escape') } catch { }
Check 'approval modal refuses a different key set' (-not $wrongApprovalKeyMatches) 'wrong keys'
$wrongApprovalDialogMatches = $true
try { $wrongApprovalDialogMatches = Test-PrimaryApprovalConfirmScreen -Screen $approvalConfirmScreen -DialogMarker 'UNRELATED' -ExpectedQuestion 'ACCEPT-Q-DIALOG: which contract should b.js use?' -Keys @('enter') } catch { }
Check 'approval modal refuses a different dialog' (-not $wrongApprovalDialogMatches) 'wrong dialog'
$reversedApprovalChoices = $approvalConfirmScreen -replace 'keep the current contract', 'TEMP' -replace 'change the contract', 'keep the current contract' -replace 'TEMP', 'change the contract'
$reversedApprovalMatches = $true
try { $reversedApprovalMatches = Test-PrimaryApprovalConfirmScreen -Screen $reversedApprovalChoices -DialogMarker 'ACCEPT-Q-DIALOG' -ExpectedQuestion 'ACCEPT-Q-DIALOG: which contract should b.js use?' -Keys @('enter') } catch { }
Check 'approval modal refuses reversed worker-choice options' (-not $reversedApprovalMatches) 'wrong worker option order'
$missingConfirmButtons = $true
try { $missingConfirmButtons = Test-PrimaryApprovalConfirmScreen -Screen ($approvalConfirmScreen -replace 'Yes\r?\nNo', '') -DialogMarker 'ACCEPT-Q-DIALOG' -ExpectedQuestion 'ACCEPT-Q-DIALOG: which contract should b.js use?' -Keys @('enter') } catch { }
Check 'approval modal refuses when its yes/no confirmation buttons are absent' (-not $missingConfirmButtons) 'missing buttons'
$changedApprovalQuestion = $approvalConfirmScreen -replace 'which contract should b\.js use\?', 'which namespace should b.js use?'
$wrongApprovalQuestionMatches = $true
try { $wrongApprovalQuestionMatches = Test-PrimaryApprovalConfirmScreen -Screen $changedApprovalQuestion -DialogMarker 'ACCEPT-Q-DIALOG' -ExpectedQuestion 'ACCEPT-Q-DIALOG: which contract should b.js use?' -Keys @('enter') } catch { }
Check 'approval modal refuses a different question even when its marker and options are unchanged' (-not $wrongApprovalQuestionMatches) 'wrong exact question'
$verticalBorderForTest = [string][char]0x2502
$nestedBorderQuestion = $verticalBorderForTest + ' ' + $verticalBorderForTest + ' ACCEPT-Q-DIALOG: which contract should b.js use? ' + $verticalBorderForTest
$nestedBorderScreen = $approvalConfirmScreen -replace 'ACCEPT-Q-DIALOG: which contract should b\.js use\?', $nestedBorderQuestion
$nestedBorderMatches = $false
try { $nestedBorderMatches = Test-PrimaryApprovalConfirmScreen -Screen $nestedBorderScreen -DialogMarker 'ACCEPT-Q-DIALOG' -ExpectedQuestion 'ACCEPT-Q-DIALOG: which contract should b.js use?' -Keys @('enter') } catch { }
Check 'approval matcher accepts an exact question inside nested terminal borders' $nestedBorderMatches 'double box border'
$inboxBefore = @(
    [pscustomobject]@{ id = 'i1'; project_key = 'repo'; task_id = 't1'; kind = 'question'; summary = 'Need input'; delivery = 'worker question'; delivery_state = 'sending'; delivery_detail = '' }
)
$inboxAfter = @(
    [pscustomobject]@{ id = 'i1'; project_key = 'repo'; task_id = 't1'; kind = 'question'; summary = 'Need input'; delivery = 'worker question'; delivery_state = 'uncertain'; delivery_detail = 'recovered after restart' },
    [pscustomobject]@{ id = 'i2'; project_key = 'repo'; task_id = 't2'; kind = 'activity'; summary = 'worker resumed'; delivery = 'worker activity'; delivery_state = 'delivered'; delivery_detail = '' }
)
$inboxSurvived = $false
try { $inboxSurvived = Test-InboxRowsPreserved -Before $inboxBefore -After $inboxAfter } catch { }
Check 'restart preserves old inbox content while permitting delivery recovery and new notes' $inboxSurvived 'preserved row and one new note'
$inboxMissing = $true
try { $inboxMissing = Test-InboxRowsPreserved -Before $inboxBefore -After @() } catch { }
Check 'restart inbox preservation rejects a missing prior note' (-not $inboxMissing) 'missing row'
$inboxChanged = $true
$changedInbox = @([pscustomobject]@{ id = 'i1'; project_key = 'repo'; task_id = 't1'; kind = 'question'; summary = 'Changed'; delivery = 'worker question'; delivery_state = 'uncertain'; delivery_detail = '' })
try { $inboxChanged = Test-InboxRowsPreserved -Before $inboxBefore -After $changedInbox } catch { }
Check 'restart inbox preservation rejects changed note content' (-not $inboxChanged) 'changed summary'
$startupLogs = @(
    [pscustomobject]@{ event = $null; command = @('./herdr-omp-orchestrator.exe', 'startup'); started_unix_ms = 90; status = 'succeeded' },
    [pscustomobject]@{ event = 'pane.moved'; command = @('./herdr-omp-orchestrator.exe', 'event'); started_unix_ms = 150; status = 'succeeded' },
    [pscustomobject]@{ event = $null; command = @('./herdr-omp-orchestrator.exe', 'startup'); started_unix_ms = 200; status = 'succeeded' }
)
$newStartupLogs = @()
try { $newStartupLogs = @(Get-StartupLogEntries -Logs $startupLogs -AfterUnixMs 100) } catch { }
Check 'restart startup log is recognized from argv after the time watermark' ($newStartupLogs.Count -eq 1 -and $newStartupLogs[0].started_unix_ms -eq 200) 'one new startup'


# --- resume-hint implication (T10.1) ----------------------------------------
Check 'hint: unmaterialized with no resume passes' ((Test-ResumeHintImplication -Hint ([pscustomobject]@{ onDisk = $false; resume = '' })) -eq $true) 'false+empty'
Check 'hint: materialized with a resume passes' ((Test-ResumeHintImplication -Hint ([pscustomobject]@{ onDisk = $true; resume = 'omp --resume=x' })) -eq $true) 'true+resume'
Check 'hint: unmaterialized but offering a resume fails' ((Test-ResumeHintImplication -Hint ([pscustomobject]@{ onDisk = $false; resume = 'omp --resume=x' })) -eq $false) 'false+resume'
Check 'hint: materialized with no resume fails' ((Test-ResumeHintImplication -Hint ([pscustomobject]@{ onDisk = $true; resume = '' })) -eq $false) 'true+empty'
Check 'hint: string flag fails' ((Test-ResumeHintImplication -Hint ([pscustomobject]@{ onDisk = 'false'; resume = '' })) -eq $false) 'string'
Check 'hint: absent flag fails' ((Test-ResumeHintImplication -Hint ([pscustomobject]@{ resume = '' })) -eq $false) 'absent'

# A blocked primary is already settled for prompt dispatch purposes: preserve the
# visible UI and refuse immediately instead of waiting for an idle state.
$script:PrimaryAgentStatusForTest = 'blocked'
$script:PrimaryScreenForTest = 'Ask dialog is waiting for an answer'
$script:PrimaryPromptCallsForTest = 0
function Wait-Until {
    param([double]$TimeoutSeconds, [double]$IntervalSeconds, [string]$What, [scriptblock]$Probe)
    $value = & $Probe
    return [pscustomobject]@{ Ok = ($null -ne $value); Value = $value }
}
function Get-LiveAgent {
    param([string]$Target)
    return [pscustomobject]@{ Found = $true; Agent = [pscustomobject]@{ agent_status = $script:PrimaryAgentStatusForTest } }
}
function Get-PaneScreen {
    param([string]$PaneId, [string]$Source, [int]$Lines)
    return $script:PrimaryScreenForTest
}
function Invoke-AgentPrompt {
    param([string]$PaneId, [string]$Text, [double]$TimeoutSeconds, [switch]$NoWait)
    $script:PrimaryPromptCallsForTest++
    $script:PrimaryNoWaitObservedForTest = $NoWait.IsPresent
    $script:PrimaryPromptTextForTest = $Text
    return [pscustomobject]@{ ExitCode = 0; Stderr = ''; Stdout = ''; CommandLine = 'herdr agent prompt -NoWait' }
}
$blockedPrimary = Invoke-PrimaryPrompt -PaneId 'w1:p1' -Text 'do not type into a modal'
Check 'Invoke-PrimaryPrompt refuses a blocked primary with a typed code' ($blockedPrimary.Blocked -and (Get-Field $blockedPrimary 'Code') -eq 'agent_blocked') 'blocked result'
Check 'Invoke-PrimaryPrompt captures the blocked screen without sending a prompt' ((Get-Field $blockedPrimary 'Screen') -eq $script:PrimaryScreenForTest -and $script:PrimaryPromptCallsForTest -eq 0) 'screen/calls'
Check 'Invoke-PrimaryPrompt proves its blocked prompt was not submitted' ($blockedPrimary.PSObject.Properties['Submitted'] -and $blockedPrimary.Submitted -eq $false) 'submitted=false'

$script:PrimaryNoWaitObservedForTest = $false
$script:PrimaryPromptTextForTest = ''
function Resolve-PendingPrimaryDispositionAsks {
    param([string]$AssertionPrefix, [double]$TimeoutSeconds = 60)
    return [pscustomobject]@{ Resolved = $true; Observed = $false; Screen = '' }
}
$script:PrimaryAgentStatusForTest = 'idle'
$script:PrimaryPromptCallsForTest = 0
$script:PrimaryNoWaitObservedForTest = $true
$script:PrimaryPromptCallsForTest = 0
$syncPrimary = $null
try { $syncPrimary = Invoke-PrimaryPromptAfterDispositionAsks -PaneId 'w1:p1' -Text 'approval submission must observe blocked state' -TimeoutSeconds 30 } catch { }
Check 'primary approval prompt uses the state-aware wait path' ($null -ne $syncPrimary -and $syncPrimary.Submitted -and $syncPrimary.ExitCode -eq 0 -and -not $script:PrimaryNoWaitObservedForTest -and $script:PrimaryPromptTextForTest -eq 'approval submission must observe blocked state') 'wait path'
$script:PrimaryNoWaitObservedForTest = $false
$script:PrimaryPromptCallsForTest = 0
$asyncPrimary = $null
try { $asyncPrimary = Invoke-PrimaryPromptNoWaitAfterDispositionAsks -PaneId 'w1:p1' -Text 'approve exact dialog' -TimeoutSeconds 30 } catch { }
Check 'primary approval prompt is submitted without waiting for its user confirmation' ($null -ne $asyncPrimary -and $asyncPrimary.Submitted -and $asyncPrimary.ExitCode -eq 0 -and $script:PrimaryNoWaitObservedForTest -and $script:PrimaryPromptTextForTest -eq 'approve exact dialog') 'no-wait submission'
$script:PrimaryAgentStatusForTest = 'blocked'
$script:PrimaryPromptCallsForTest = 0
$blockedAsyncPrimary = $null
try { $blockedAsyncPrimary = Invoke-PrimaryPromptNoWaitAfterDispositionAsks -PaneId 'w1:p1' -Text 'must not type into active UI' -TimeoutSeconds 30 } catch { }
Check 'no-wait primary prompt refuses an already blocked session without submitting' ($null -ne $blockedAsyncPrimary -and $blockedAsyncPrimary.Blocked -and -not $blockedAsyncPrimary.Submitted -and $script:PrimaryPromptCallsForTest -eq 0) 'blocked refusal'
# A primary that Herdr reports blocked only briefly (no dialog was open) is tried
# once more after its pending disposition Asks are resolved, as the --wait variant
# does; the first attempt still types nothing.
$script:ResolvePendingCallsForTest = 0
function Resolve-PendingPrimaryDispositionAsks {
    param([string]$AssertionPrefix, [double]$TimeoutSeconds = 60)
    $script:ResolvePendingCallsForTest++
    if ($script:ResolvePendingCallsForTest -ge 2) { $script:PrimaryAgentStatusForTest = 'idle' }
    return [pscustomobject]@{ Resolved = $true; Observed = $false; Screen = '' }
}
$script:PrimaryAgentStatusForTest = 'blocked'
$script:PrimaryPromptCallsForTest = 0
$transientAsyncPrimary = $null
try { $transientAsyncPrimary = Invoke-PrimaryPromptNoWaitAfterDispositionAsks -PaneId 'w1:p1' -Text 'answer after a transient block' -TimeoutSeconds 30 } catch { }
Check 'no-wait primary prompt retries once after a transient blocked state and submits exactly once' ($null -ne $transientAsyncPrimary -and $transientAsyncPrimary.Submitted -and $script:PrimaryPromptCallsForTest -eq 1 -and $script:PrimaryPromptTextForTest -eq 'answer after a transient block') ("calls=$($script:PrimaryPromptCallsForTest) resolves=$($script:ResolvePendingCallsForTest)")
function Resolve-PendingPrimaryDispositionAsks {
    param([string]$AssertionPrefix, [double]$TimeoutSeconds = 60)
    return [pscustomobject]@{ Resolved = $true; Observed = $false; Screen = '' }
}
$script:PrimaryAgentStatusForTest = 'blocked'
$savedKeepResources = [bool]$script:KeepResources
$script:KeepResources = $false
if (Get-Command Preserve-ResourcesForPendingTool -ErrorAction SilentlyContinue) {
    Preserve-ResourcesForPendingTool -AssertionId 'TEST.preserve-resources' -Reason 'approval may still be active'
}
$preservedResources = ([bool]$script:KeepResources -eq $true)
Check 'pending approval switches final cleanup to preserve resources' $preservedResources 'KeepResources=true'
$script:KeepResources = $savedKeepResources
$script:PrimarySendKeysUsed = @()
$script:PrimaryScreenForTest = $primaryAskScreen
$script:ResolveWaitForTest = 0
function Wait-Until {
    param([double]$TimeoutSeconds, [double]$IntervalSeconds, [string]$What, [scriptblock]$Probe)
    $script:ResolveWaitForTest++
    if ($script:ResolveWaitForTest -eq 1) {
        return [pscustomobject]@{ Ok = $true; Value = [pscustomobject]@{ Kind = 'ask'; Screen = $script:PrimaryScreenForTest } }
    }
    if ($script:ResolveWaitForTest -eq 2) { return [pscustomobject]@{ Ok = $true; Value = $true } }
    return [pscustomobject]@{ Ok = $true; Value = 'disposed' }
}
function Send-AgentKeys {
    param([string]$PaneId, [string[]]$Keys)
    return [pscustomobject]@{ ExitCode = 0; CommandLine = "send-keys $PaneId $($Keys -join ' ')" }
}
function Get-TaskById {
    param([string]$Id)
    return [pscustomobject]@{ fact = [pscustomobject]@{ Revision = 7; Disposition = ''; Report = [pscustomobject]@{ Summary = 'result' } } }
}
$script:WorkerDispositionAskTaskForTest = [pscustomobject]@{
    fact = [pscustomobject]@{
        ID = 'task-a'
        Title = 'ACCEPT A: helper in a.js'
        ReservedPaths = @('a.js')
        Report = [pscustomobject]@{
            Summary = 'ACCEPT-A: a.js now also exports subtract'
            Evidence = @('commit 0123456789abcdef0123456789abcdef01234567')
        }
    }
}
function Get-TaskById {
    param([string]$Id)
    if ($Id -eq 'task-a') { return $script:WorkerDispositionAskTaskForTest }
    return [pscustomobject]@{ fact = [pscustomobject]@{ Revision = 7; Disposition = ''; Report = [pscustomobject]@{ Summary = 'result' } } }
}
$workerDispositionAskScreen = @'
Ask 1 questions
[worker_a_disposition] · options:2
Worker A reports commit 0123456789abcdef0123456789abcdef01234567 adding subtract to a.js. What disposition should I record?
Accept
Record the reported result as accepted; no merge or cleanup is performed.
Request changes
Keep the result unaccepted and specify what should change.
Other (type your own)
'@
$workerDispositionAskMatches = $false
try { $workerDispositionAskMatches = Test-PrimaryDispositionAskScreen -Screen $workerDispositionAskScreen -AskId 'worker_a_disposition' -TaskId 'task-a' } catch { }
Check 'disposition matcher ties the active worker-A Ask to its reported commit and choices' $workerDispositionAskMatches 'exact task report + Accept first'
$wrongWorkerDispositionCommit = $workerDispositionAskScreen -replace '0123456789abcdef0123456789abcdef01234567', '1123456789abcdef0123456789abcdef01234567'
$wrongWorkerDispositionMatches = $true
try { $wrongWorkerDispositionMatches = Test-PrimaryDispositionAskScreen -Screen $wrongWorkerDispositionCommit -AskId 'worker_a_disposition' -TaskId 'task-a' } catch { }
Check 'disposition matcher refuses a worker Ask naming a different report commit' (-not $wrongWorkerDispositionMatches) 'different report commit'
$reversedWorkerDispositionAsk = $workerDispositionAskScreen -replace 'Accept', 'TEMP' -replace 'Request changes', 'Accept' -replace 'TEMP', 'Request changes'
$reversedWorkerDispositionMatches = $true
try { $reversedWorkerDispositionMatches = Test-PrimaryDispositionAskScreen -Screen $reversedWorkerDispositionAsk -AskId 'worker_a_disposition' -TaskId 'task-a' } catch { }
Check 'disposition matcher refuses when Accept is not the first choice' (-not $reversedWorkerDispositionMatches) 'reversed disposition choices'
$normalizedDispositionAskScreen = $workerDispositionAskScreen -replace 'worker_a_disposition', 'disposition_a'
$normalizedDispositionAskId = Get-PrimaryAskId -Screen $normalizedDispositionAskScreen
Check 'alternate OMP disposition Ask ids normalize to the report-bound worker id' ($normalizedDispositionAskId -eq 'worker_a_disposition') $normalizedDispositionAskId
$normalizedDispositionAskMatches = $false
try { $normalizedDispositionAskMatches = Test-PrimaryDispositionAskScreen -Screen $normalizedDispositionAskScreen -AskId 'worker_a_disposition' -TaskId 'task-a' } catch { }
Check 'alternate OMP disposition Ask ids still require the exact report and choice content' $normalizedDispositionAskMatches 'report-bound alternate id'
$script:PrimaryPane = 'w1:p1'
$script:Project = [pscustomobject]@{ LaunchPaneID = 'w1:p1' }
$savedReviewModal = (Get-Command Confirm-PrimaryReviewModal).ScriptBlock
$script:ReviewModalTaskForTest = ''
function Confirm-PrimaryReviewModal {
    param([string]$PaneId, [string]$LaunchPaneId, [string]$TaskId, [string]$Disposition)
    $script:ReviewModalTaskForTest = $TaskId
    return $true
}
$resolverOutput = @(Resolve-PrimaryDispositionAsk -Marker 'ACCEPT-B' -AskId 'task_b_review' -TaskId 'task-b' -AssertionId 'TEST.askB')
Check 'primary Ask resolver emits exactly one result object after review confirmation' ($resolverOutput.Count -eq 1 -and $null -ne $resolverOutput[0].PSObject.Properties['Resolved'] -and $script:ReviewModalTaskForTest -eq 'task-b') ("count=$($resolverOutput.Count)")
$script:ResolveWaitForTest = 0
$script:PrimaryAgentStatusForTest = 'blocked'
$script:PrimaryScreenForTest = $workerDispositionAskScreen
$script:WorkerDispositionKeysForTest = @()
function Send-AgentKeys {
    param([string]$PaneId, [string[]]$Keys)
    $script:WorkerDispositionKeysForTest += ($Keys -join ' ')
    return [pscustomobject]@{ ExitCode = 0; CommandLine = "send-keys $PaneId $($Keys -join ' ')" }
}
$workerDispositionResolution = $null
try { $workerDispositionResolution = Resolve-PrimaryDispositionAsk -Marker 'ACCEPT-A' -AskId 'worker_a_disposition' -TaskId 'task-a' -AssertionId 'TEST.workerDisposition' } catch { }
Check 'disposition resolver selects Accept only for the exact active worker report Ask' ($null -ne $workerDispositionResolution -and $workerDispositionResolution.Resolved -and $workerDispositionResolution.Observed -and $script:WorkerDispositionKeysForTest.Count -eq 1 -and $script:WorkerDispositionKeysForTest[0] -eq 'enter') ("result=$(Get-Json $workerDispositionResolution); keys=$($script:WorkerDispositionKeysForTest -join ',')")
Set-Item -Path Function:\Confirm-PrimaryReviewModal -Value $savedReviewModal
function Wait-Until {
    param([double]$TimeoutSeconds, [double]$IntervalSeconds, [string]$What, [scriptblock]$Probe)
    $value = & $Probe
    return [pscustomobject]@{ Ok = ($null -ne $value); Value = $value }
}
function Send-AgentKeys {
    param([string]$PaneId, [string[]]$Keys)
    $script:SentKeysCallsForTest += ($Keys -join ' ')
    return [pscustomobject]@{ ExitCode = 0; CommandLine = "send-keys $PaneId $($Keys -join ' ')" }
}
$script:SentKeysCallsForTest = @()
$script:PrimaryScreenForTest = $approvalConfirmScreen
$script:PrimarySendKeysUsed = @()
$confirmedModal = $null
try { $confirmedModal = Confirm-PrimaryModal -TaskId 'orch_abc' -DialogMarker 'ACCEPT-Q-DIALOG' -ExpectedQuestion 'ACCEPT-Q-DIALOG: which contract should b.js use?' -ExpectedKeys @('enter') -TranscriptPath $approvalTranscriptPath -TimeoutSeconds 1 } catch { }
Check 'confirmation sends the user key only after transcript and visible modal both match' ($null -ne $confirmedModal -and $confirmedModal.Observed -and $script:SentKeysCallsForTest.Count -eq 1 -and $script:SentKeysCallsForTest[0] -eq 'enter' -and $script:PrimarySendKeysUsed.Count -eq 1) ("result=$(Get-Json $confirmedModal); send_calls=$($script:SentKeysCallsForTest -join ',')")
$script:FinalModalPaneReadsForTest = 0
$script:FinalModalReadsAtSendForTest = -1
function Get-PaneScreen {
    param([string]$PaneId, [string]$Source, [int]$Lines)
    $script:FinalModalPaneReadsForTest++
    return $script:PrimaryScreenForTest
}
function Send-AgentKeys {
    param([string]$PaneId, [string[]]$Keys)
    $script:SentKeysCallsForTest += ($Keys -join ' ')
    $script:FinalModalReadsAtSendForTest = $script:FinalModalPaneReadsForTest
    return [pscustomobject]@{ ExitCode = 0; CommandLine = "send-keys $PaneId $($Keys -join ' ')" }
}
$script:FinalModalPaneReadsForTest = 0
$script:FinalModalReadsAtSendForTest = -1
$noPostInputModal = $null
try { $noPostInputModal = Confirm-PrimaryModal -TaskId 'orch_abc' -DialogMarker 'ACCEPT-Q-DIALOG' -ExpectedQuestion 'ACCEPT-Q-DIALOG: which contract should b.js use?' -ExpectedKeys @('enter') -TranscriptPath $approvalTranscriptPath -TimeoutSeconds 1 } catch { }
Check 'confirmation performs no pane I/O after sending the verified key' ($null -ne $noPostInputModal -and $noPostInputModal.Observed -and $script:FinalModalReadsAtSendForTest -eq $script:FinalModalPaneReadsForTest) ("reads_at_send=$($script:FinalModalReadsAtSendForTest) final_reads=$($script:FinalModalPaneReadsForTest)")
$script:PrimaryScreenForTest = 'Ask: unrelated approval'
$script:SentKeysCallsForTest = @()
$script:PrimarySendKeysUsed = @()
$mismatchedModal = $null
try { $mismatchedModal = Confirm-PrimaryModal -TaskId 'orch_abc' -DialogMarker 'ACCEPT-Q-DIALOG' -ExpectedQuestion 'ACCEPT-Q-DIALOG: which contract should b.js use?' -ExpectedKeys @('enter') -TranscriptPath $approvalTranscriptPath -TimeoutSeconds 1 } catch { }
Check 'confirmation sends no key when the visible modal differs from the expected dialog' ($null -ne $mismatchedModal -and -not $mismatchedModal.Observed -and $script:SentKeysCallsForTest.Count -eq 0) 'mismatched modal refusal'
$script:PrimaryScreenForTest = $approvalConfirmScreen
$script:SentKeysCallsForTest = @()
$script:PrimarySendKeysUsed = @()
$wrongTaskModal = $null
try { $wrongTaskModal = Confirm-PrimaryModal -TaskId 'orch_other' -DialogMarker 'ACCEPT-Q-DIALOG' -ExpectedQuestion 'ACCEPT-Q-DIALOG: which contract should b.js use?' -ExpectedKeys @('enter') -TranscriptPath $approvalTranscriptPath -TimeoutSeconds 1 } catch { }
Check 'confirmation sends no key when transcript task identity differs' ($null -ne $wrongTaskModal -and -not $wrongTaskModal.Observed -and $script:SentKeysCallsForTest.Count -eq 0) 'wrong task refusal'
$script:PrimaryScreenReadsForTest = 0
function Get-PaneScreen {
    param([string]$PaneId, [string]$Source, [int]$Lines)
    $script:PrimaryScreenReadsForTest++
    if ($script:PrimaryScreenReadsForTest -eq 1) { return $approvalConfirmScreen }
    return 'Ask: replacement modal appeared after the confirmation snapshot'
}
$script:PrimaryScreenForTest = $approvalConfirmScreen
$script:SentKeysCallsForTest = @()
$script:PrimarySendKeysUsed = @()
$replacedModal = $null
try { $replacedModal = Confirm-PrimaryModal -TaskId 'orch_abc' -DialogMarker 'ACCEPT-Q-DIALOG' -ExpectedQuestion 'ACCEPT-Q-DIALOG: which contract should b.js use?' -ExpectedKeys @('enter') -TranscriptPath $approvalTranscriptPath -TimeoutSeconds 1 } catch { }
Check 'confirmation re-reads the active modal immediately before sending keys' ($null -ne $replacedModal -and -not $replacedModal.Observed -and $script:SentKeysCallsForTest.Count -eq 0 -and $script:PrimaryScreenReadsForTest -ge 2) 'modal changed after snapshot'
$script:ApprovalTranscriptReadsForTest = 0
function Test-OmpApprovalToolCall {
    param([string]$Path, [string]$TaskId, [string[]]$Keys)
    $script:ApprovalTranscriptReadsForTest++
    if ($script:ApprovalTranscriptReadsForTest -eq 2) {
        $script:PrimaryScreenForTest = 'Ask: replacement modal appeared during transcript verification'
    }
    return $true
}
function Get-PaneScreen {
    param([string]$PaneId, [string]$Source, [int]$Lines)
    return $script:PrimaryScreenForTest
}
$script:ApprovalTranscriptReadsForTest = 0
$script:PrimaryScreenForTest = $approvalConfirmScreen
$script:SentKeysCallsForTest = @()
$script:PrimarySendKeysUsed = @()
$lateReplacementModal = $null
try { $lateReplacementModal = Confirm-PrimaryModal -TaskId 'orch_abc' -DialogMarker 'ACCEPT-Q-DIALOG' -ExpectedQuestion 'ACCEPT-Q-DIALOG: which contract should b.js use?' -ExpectedKeys @('enter') -TranscriptPath $approvalTranscriptPath -TimeoutSeconds 1 } catch { }
Check 'confirmation reads the pane after the final transcript check and refuses a replacement modal' ($null -ne $lateReplacementModal -and -not $lateReplacementModal.Observed -and $script:SentKeysCallsForTest.Count -eq 0 -and $script:ApprovalTranscriptReadsForTest -ge 2) 'screen changed during transcript verification'
$savedReadySession = (Get-Command Get-ReadySession).ScriptBlock
$savedProject = $script:Project
$script:Project = [pscustomobject]@{ LaunchPaneID = 'w1:p1' }
$script:RequestedReadyPaneForTest = ''
function Get-ReadySession {
    param([string]$LaunchPaneId)
    $script:RequestedReadyPaneForTest = $LaunchPaneId
    return [pscustomobject]@{ omp_session_file = $reviewTranscriptPath }
}
$script:PrimaryScreenForTest = $reviewScreen
$script:PrimaryAgentStatusForTest = 'blocked'
$script:SentKeysCallsForTest = @()
$script:PrimarySendKeysUsed = @()
$reviewConfirmed = Confirm-PrimaryReviewModal -PaneId 'w1:p1' -LaunchPaneId 'w9:p9' -TaskId 'orch_abc' -Disposition 'Accepted the reported result.'
Check 'review resolver uses restored launch-pane handshake rather than stale project' ($script:RequestedReadyPaneForTest -eq 'w9:p9') $script:RequestedReadyPaneForTest
Check 'review resolver sends Enter only for a transcript-linked exact modal' ($reviewConfirmed -and $script:SentKeysCallsForTest.Count -eq 1 -and $script:SentKeysCallsForTest[0] -eq 'enter') 'review confirmed'
$script:PrimaryScreenForTest = $reviewScreen -replace 'orch_abc', 'orch_other'
$script:SentKeysCallsForTest = @()
$wrongReview = Confirm-PrimaryReviewModal -PaneId 'w1:p1' -LaunchPaneId 'w9:p9' -TaskId 'orch_abc' -Disposition 'Accepted the reported result.'
Check 'review resolver refuses a modal for another task without sending keys' (-not $wrongReview -and $script:SentKeysCallsForTest.Count -eq 0) 'wrong review'
Set-Item -Path Function:\Get-ReadySession -Value $savedReadySession
$script:Project = $savedProject

# --- the answer confirmation: Enter only on the exact task/text, never twice ---
$answerPrimaryTranscriptPath = Join-Path $script:HarnessRoot 'answer-primary.jsonl'
Set-Content -LiteralPath $answerPrimaryTranscriptPath -Encoding UTF8 -Value '{"type":"message","message":{"role":"assistant","content":[{"type":"toolCall","name":"orch_answer","arguments":{"taskId":"orch_abc","text":"ACCEPT-REPORT-B"}}]}}'
$answerWorkerTranscriptPath = Join-Path $script:HarnessRoot 'answer-worker.jsonl'
Set-Content -LiteralPath $answerWorkerTranscriptPath -Encoding UTF8 -Value '{"type":"message","message":{"role":"user","content":[{"type":"text","text":"ACCEPT-TASK-B brief"}]}}'
$script:PrimaryAgentStatusForTest = 'blocked'
$script:PrimaryScreenForTest = $answerScreen
$script:SentKeysCallsForTest = @()
$script:PrimarySendKeysUsed = @()
$answerConfirmed = Confirm-PrimaryAnswerModal -TaskId 'orch_abc' -Text 'ACCEPT-REPORT-B' -TranscriptPath $answerPrimaryTranscriptPath -WorkerTranscriptPath $answerWorkerTranscriptPath -TimeoutSeconds 1
Check 'answer confirmer sends Enter once for the transcript-linked exact dialog' ($answerConfirmed.Observed -and $answerConfirmed.Confirmed -and -not $answerConfirmed.DeliveredBeforeConfirm -and $script:SentKeysCallsForTest.Count -eq 1 -and $script:SentKeysCallsForTest[0] -eq 'enter' -and $script:PrimarySendKeysUsed.Count -eq 1) ("result=$(Get-Json $answerConfirmed); keys=$($script:SentKeysCallsForTest -join ',')")
$script:SentKeysCallsForTest = @()
$answerOtherTask = Confirm-PrimaryAnswerModal -TaskId 'orch_other' -Text 'ACCEPT-REPORT-B' -TranscriptPath $answerPrimaryTranscriptPath -TimeoutSeconds 1
Check 'answer confirmer refuses a dialog for another task without sending keys' (-not $answerOtherTask.Confirmed -and $script:SentKeysCallsForTest.Count -eq 0) (Get-Json $answerOtherTask)
$answerOtherText = Confirm-PrimaryAnswerModal -TaskId 'orch_abc' -Text 'ACCEPT-REPORT-C' -TranscriptPath $answerPrimaryTranscriptPath -TimeoutSeconds 1
Check 'answer confirmer refuses a dialog showing another text without sending keys' (-not $answerOtherText.Confirmed -and $script:SentKeysCallsForTest.Count -eq 0) (Get-Json $answerOtherText)
$answerNoCall = Confirm-PrimaryAnswerModal -TaskId 'orch_abc' -Text 'ACCEPT-REPORT-B' -TranscriptPath $answerWorkerTranscriptPath -TimeoutSeconds 1
Check 'answer confirmer refuses a dialog the primary transcript does not hold an orch_answer call for' (-not $answerNoCall.Confirmed -and $script:SentKeysCallsForTest.Count -eq 0) (Get-Json $answerNoCall)
$script:PrimaryAgentStatusForTest = 'working'
$answerNotBlocked = Confirm-PrimaryAnswerModal -TaskId 'orch_abc' -Text 'ACCEPT-REPORT-B' -TranscriptPath $answerPrimaryTranscriptPath -TimeoutSeconds 1
Check 'answer confirmer sends nothing while the primary is not blocked on a dialog' (-not $answerNotBlocked.Observed -and $script:SentKeysCallsForTest.Count -eq 0) (Get-Json $answerNotBlocked)
$script:PrimaryAgentStatusForTest = 'blocked'
Add-Content -LiteralPath $answerWorkerTranscriptPath -Encoding UTF8 -Value '{"type":"message","message":{"role":"user","content":[{"type":"text","text":"ACCEPT-REPORT-B"}]}}'
$answerAlreadyDelivered = Confirm-PrimaryAnswerModal -TaskId 'orch_abc' -Text 'ACCEPT-REPORT-B' -TranscriptPath $answerPrimaryTranscriptPath -WorkerTranscriptPath $answerWorkerTranscriptPath -TimeoutSeconds 1
Check 'answer confirmer reports a text that reached the worker before confirmation and sends nothing' ($answerAlreadyDelivered.Observed -and $answerAlreadyDelivered.DeliveredBeforeConfirm -and -not $answerAlreadyDelivered.Confirmed -and $script:SentKeysCallsForTest.Count -eq 0) (Get-Json $answerAlreadyDelivered)

$script:DispositionWaitCallsForTest = 0
$script:ReplacementDispositionScreen = $primaryAskScreen -replace 'task_b_review', 'task_a_review'
function Wait-Until {
    param([double]$TimeoutSeconds, [double]$IntervalSeconds, [string]$What, [scriptblock]$Probe)
    $script:DispositionWaitCallsForTest++
    if ($script:DispositionWaitCallsForTest -eq 1) {
        return [pscustomobject]@{ Ok = $true; Value = [pscustomobject]@{ Kind = 'ask'; Screen = $primaryAskScreen } }
    }
    $value = & $Probe
    return [pscustomobject]@{ Ok = ($null -ne $value); Value = $value }
}
function Get-PaneScreen {
    param([string]$PaneId, [string]$Source, [int]$Lines)
    return $script:ReplacementDispositionScreen
}
$script:PrimaryAgentStatusForTest = 'blocked'
$script:SentKeysCallsForTest = @()
$replacedDisposition = $null
try { $replacedDisposition = Resolve-PrimaryDispositionAsk -Marker 'ACCEPT-B' -AskId 'task_b_review' -TaskId 'task-b' -AssertionId 'TEST.askB-race' } catch { }
Check 'disposition resolver refuses to send Enter when the active Ask changes after polling' ($null -ne $replacedDisposition -and -not $replacedDisposition.Resolved -and $script:SentKeysCallsForTest.Count -eq 0) 'replacement Ask'
$script:DispositionWaitCallsForTest = 0
$script:DispositionScreenForTest = $primaryAskScreen
$script:DispositionTaskReadsForTest = 0
function Wait-Until {
    param([double]$TimeoutSeconds, [double]$IntervalSeconds, [string]$What, [scriptblock]$Probe)
    $script:DispositionWaitCallsForTest++
    if ($script:DispositionWaitCallsForTest -eq 1) {
        return [pscustomobject]@{ Ok = $true; Value = [pscustomobject]@{ Kind = 'ask'; Screen = $primaryAskScreen } }
    }
    $value = & $Probe
    return [pscustomobject]@{ Ok = ($null -ne $value); Value = $value }
}
function Get-PaneScreen {
    param([string]$PaneId, [string]$Source, [int]$Lines)
    return $script:DispositionScreenForTest
}
function Get-TaskById {
    param([string]$Id)
    $script:DispositionTaskReadsForTest++
    $disposition = if ($script:DispositionTaskReadsForTest -ge 2) { 'accepted' } else { '' }
    return [pscustomobject]@{ fact = [pscustomobject]@{ Disposition = $disposition; Report = [pscustomobject]@{ Summary = 'result' } } }
}
$script:SentKeysCallsForTest = @()
$changedDisposition = $null
try { $changedDisposition = Resolve-PrimaryDispositionAsk -Marker 'ACCEPT-B' -AskId 'task_b_review' -TaskId 'task-b' -AssertionId 'TEST.askB-state-race' } catch { }
Check 'disposition resolver refuses Enter when the task is already disposed before input' ($null -ne $changedDisposition -and -not $changedDisposition.Resolved -and $script:SentKeysCallsForTest.Count -eq 0) 'disposition changed before Enter'
$script:PrimaryAgentStatusForTest = 'idle'
$script:DispositionWaitCallsForTest = 0
function Wait-Until {
    param([double]$TimeoutSeconds, [double]$IntervalSeconds, [string]$What, [scriptblock]$Probe)
    $value = & $Probe
    return [pscustomobject]@{ Ok = ($null -ne $value); Value = $value }
}
function Get-TaskById {
    param([string]$Id)
    return [pscustomobject]@{ fact = [pscustomobject]@{ Disposition = ''; Report = [pscustomobject]@{ Summary = 'result' } } }
}
$script:SentKeysCallsForTest = @()
$idleUndisposedAsk = $null
try { $idleUndisposedAsk = Resolve-PrimaryDispositionAsk -Marker 'ACCEPT-A' -AskId 'worker_a_disposition' -TaskId 'task-a' -AssertionId 'TEST.ask-idle-undisposed' } catch { }
Check 'disposition resolver does not treat an idle primary as resolved while its report is undisposed' ($null -ne $idleUndisposedAsk -and -not $idleUndisposedAsk.Resolved -and $script:SentKeysCallsForTest.Count -eq 0) 'idle with pending report'
$script:QuestionAskWaitCallsForTest = 0
$script:QuestionAskTaskReadsForTest = 0
$script:QuestionAskScreenForTest = $primaryWorkerQuestionAskScreen
$script:PrimaryAgentStatusForTest = 'blocked'
function Wait-Until {
    param([double]$TimeoutSeconds, [double]$IntervalSeconds, [string]$What, [scriptblock]$Probe)
    $script:QuestionAskWaitCallsForTest++
    if ($script:QuestionAskWaitCallsForTest -eq 1) {
        return [pscustomobject]@{ Ok = $true; Value = [pscustomobject]@{ Kind = 'ask'; AskId = 'answer_a'; Screen = $primaryWorkerQuestionAskScreen } }
    }
    $value = & $Probe
    return [pscustomobject]@{ Ok = ($null -ne $value); Value = $value }
}
function Get-PaneScreen {
    param([string]$PaneId, [string]$Source, [int]$Lines)
    return $script:QuestionAskScreenForTest
}
function Get-TaskById {
    param([string]$Id)
    $script:QuestionAskTaskReadsForTest++
    return [pscustomobject]@{ fact = [pscustomobject]@{ ID = $Id; Attention = 'worker question: ACCEPT-Q-A: should a.js keep its existing export style?'; Disposition = ''; Report = $null } }
}
function Send-AgentKeys {
    param([string]$PaneId, [string[]]$Keys)
    $script:SentKeysCallsForTest += ($Keys -join ' ')
    $script:PrimaryAgentStatusForTest = 'idle'
    $script:QuestionAskScreenForTest = 'The user selected Yes — ACCEPT-ANSWER-A'
    return [pscustomobject]@{ ExitCode = 0; CommandLine = "send-keys $PaneId $($Keys -join ' ')" }
}
$script:SentKeysCallsForTest = @()
$workerQuestionAskResult = $null
try { $workerQuestionAskResult = Resolve-PrimaryWorkerQuestionAsk -QuestionMarker 'ACCEPT-Q-A' -QuestionText 'Should a.js keep its existing export style?' -TaskId 'task-a' -AnswerToken 'ACCEPT-ANSWER-A' -AssertionId 'TEST.question-answer' } catch { }
Check 'worker-question resolver enters only the exact active Ask and selects its authorized token' ($null -ne $workerQuestionAskResult -and $workerQuestionAskResult.Resolved -and $script:SentKeysCallsForTest.Count -eq 1 -and $script:SentKeysCallsForTest[0] -eq 'enter') 'exact worker Ask'
$script:QuestionAskWaitCallsForTest = 0
$script:PrimaryAgentStatusForTest = 'blocked'
$script:QuestionAskScreenForTest = $primaryWorkerAnswerChoiceAskScreen
function Wait-Until {
    param([double]$TimeoutSeconds, [double]$IntervalSeconds, [string]$What, [scriptblock]$Probe)
    $script:QuestionAskWaitCallsForTest++
    if ($script:QuestionAskWaitCallsForTest -eq 1) {
        return [pscustomobject]@{ Ok = $true; Value = [pscustomobject]@{ Kind = 'ask'; AskId = 'export-style'; Screen = $primaryWorkerAnswerChoiceAskScreen } }
    }
    $value = & $Probe
    return [pscustomobject]@{ Ok = ($null -ne $value); Value = $value }
}
$script:SentKeysCallsForTest = @()
$workerAnswerChoiceResolution = $null
try { $workerAnswerChoiceResolution = Resolve-PrimaryWorkerQuestionAsk -QuestionMarker 'ACCEPT-Q-A' -QuestionText 'Should a.js keep its existing export style?' -TaskId 'task-a' -AnswerToken 'ACCEPT-ANSWER-A' -AssertionId 'TEST.question-answer-choice' } catch { }
Check 'worker-question resolver selects only the exact active dynamic-id answer-choice Ask' ($null -ne $workerAnswerChoiceResolution -and $workerAnswerChoiceResolution.Resolved -and $script:SentKeysCallsForTest.Count -eq 1 -and $script:SentKeysCallsForTest[0] -eq 'enter') 'export-style answer choice'
$script:QuestionAskWaitCallsForTest = 0
$script:PrimaryAgentStatusForTest = 'blocked'
$script:QuestionAskScreenForTest = $primaryWorkerQuestionAskScreen -replace 'ACCEPT-ANSWER-A', 'ACCEPT-ANSWER-B'
$script:SentKeysCallsForTest = @()
$wrongWorkerQuestionAskResult = $null
try { $wrongWorkerQuestionAskResult = Resolve-PrimaryWorkerQuestionAsk -QuestionMarker 'ACCEPT-Q-A' -QuestionText 'Should a.js keep its existing export style?' -TaskId 'task-a' -AnswerToken 'ACCEPT-ANSWER-A' -AssertionId 'TEST.question-answer-wrong' } catch { }
Check 'worker-question resolver refuses an Ask with a different authorization token without typing' ($null -ne $wrongWorkerQuestionAskResult -and -not $wrongWorkerQuestionAskResult.Resolved -and $script:SentKeysCallsForTest.Count -eq 0) 'wrong worker Ask'
$script:PrimaryPane = 'w1:p1'
$script:PrimaryAgentStatusForTest = 'idle'
$script:TaskA = [pscustomobject]@{ ID = 'task-a' }
$script:TaskB = $null
$script:PendingDispositionTaskForTest = [pscustomobject]@{
    fact = [pscustomobject]@{
        ID = 'task-a'
        Title = 'ACCEPT A: helper in a.js'
        ReservedPaths = @('a.js')
        Disposition = ''
        Report = [pscustomobject]@{
            Summary = 'ACCEPT-A: a.js now also exports subtract'
            Evidence = @('commit 0123456789abcdef0123456789abcdef01234567')
        }
    }
}
function Get-TaskById {
    param([string]$Id)
    return $script:PendingDispositionTaskForTest
}
$script:PendingDispositionAskScreen = $workerDispositionAskScreen
$script:PendingDispositionPromptTextForTest = ''
$script:PendingDispositionResolvedAskForTest = $null
function Get-PaneScreen {
    param([string]$PaneId, [string]$Source, [int]$Lines)
    return $script:PendingDispositionAskScreen
}
function Invoke-PrimaryPrompt {
    param([string]$PaneId, [string]$Text, [double]$TimeoutSeconds = 300)
    $script:PendingDispositionPromptTextForTest = $Text
    return [pscustomobject]@{ Submitted = $true; ExitCode = 0; Code = ''; CommandLine = 'herdr agent prompt --wait'; Screen = '' }
}
function Wait-Until {
    param([double]$TimeoutSeconds, [double]$IntervalSeconds, [string]$What, [scriptblock]$Probe)
    return [pscustomobject]@{ Ok = $true; Value = [pscustomobject]@{ Kind = 'ask'; Screen = $script:PendingDispositionAskScreen } }
}
function Resolve-PrimaryDispositionAsk {
    param([string]$Marker, [string]$AskId, [string]$TaskId, [string]$AssertionId)
    $script:PendingDispositionResolvedAskForTest = [pscustomobject]@{ Marker = $Marker; AskId = $AskId; TaskId = $TaskId }
    return [pscustomobject]@{ Resolved = $true; Observed = $true; Screen = $script:PendingDispositionAskScreen }
}
$pendingDispositionAskResult = $null
try { $pendingDispositionAskResult = Invoke-PrimaryDispositionAskForPendingReport -AssertionPrefix 'TEST.pending-report' } catch { }
Check 'pending report gets one exact OMP disposition Ask before its first option is selected' ($null -ne $pendingDispositionAskResult -and $pendingDispositionAskResult.Resolved -and $pendingDispositionAskResult.Observed -and $script:PendingDispositionResolvedAskForTest.AskId -eq 'worker_a_disposition' -and $script:PendingDispositionResolvedAskForTest.TaskId -eq 'task-a' -and $script:PendingDispositionPromptTextForTest.Contains('0123456789abcdef0123456789abcdef01234567') -and $script:PendingDispositionPromptTextForTest.Contains('Do not call orch_review until the user chooses')) 'exact pending report Ask'

# --- preflight cleanup refuses live/ambiguous sessions and scoped artifacts ---
$oldRunId = 'abcdef12'
$oldRoot = Join-Path $script:HarnessRoot "orch-accept-$oldRunId"
$oldRepo = Join-Path $oldRoot 'repo'
$sessionBase = Join-Path $script:HarnessRoot 'herdr-sessions'
$oldSession = [pscustomobject]@{
    name = "orch-acc-$oldRunId"; running = $false
    session_dir = (Join-Path $sessionBase "orch-acc-$oldRunId")
}
$stoppedSafe = $false
try { $stoppedSafe = (Get-StaleAcceptanceSessionDisposition -RunId $oldRunId -Sessions @($oldSession) -SessionsRoot $sessionBase) -eq 'stopped' } catch { }
Check 'preflight recognizes only an exact stopped acceptance session' $stoppedSafe 'stopped session'
$oldSession.running = $true
$liveDetected = $false
try { $liveDetected = (Get-StaleAcceptanceSessionDisposition -RunId $oldRunId -Sessions @($oldSession) -SessionsRoot $sessionBase) -eq 'running' } catch { }
Check 'preflight identifies a live acceptance session for explicit confirmation' $liveDetected 'live session'
$absentDisposition = (Get-StaleAcceptanceSessionDisposition -RunId $oldRunId -Sessions @() -SessionsRoot $sessionBase)
Check 'preflight recognizes an already absent acceptance session' ($absentDisposition -eq 'absent') $absentDisposition
$oldSession.running = $false
$oldSession.session_dir = Join-Path $script:HarnessRoot 'other-session'
$wrongDirRefused = $false
try { [void](Get-StaleAcceptanceSessionDisposition -RunId $oldRunId -Sessions @($oldSession) -SessionsRoot $sessionBase) } catch { $wrongDirRefused = $true }
Check 'preflight refuses a session outside the exact session directory' $wrongDirRefused 'foreign directory'

New-Item -ItemType Directory -Path $oldRepo -Force | Out-Null
Set-Content -LiteralPath (Join-Path $oldRepo 'owned.txt') -Value 'acceptance fixture'
@{ runId = $oldRunId; session = "orch-acc-$oldRunId"; projectKey = $oldRepo } |
    ConvertTo-Json | Set-Content -LiteralPath (Join-Path $oldRoot 'report.json')
$rootRemoved = $false
$rootError = ''
try { Remove-StaleAcceptanceRunRoot -RunId $oldRunId -ProjectKey $oldRepo -TempRoot $script:HarnessRoot; $rootRemoved = -not (Test-Path -LiteralPath $oldRoot) } catch { $rootError = $_.Exception.Message }
Check 'preflight removes only the verified old run root after state cleanup' $rootRemoved "root=$oldRoot error=$rootError"
New-Item -ItemType Directory -Path $oldRoot -Force | Out-Null
@{ runId = '99999999'; session = "orch-acc-$oldRunId"; projectKey = $oldRepo } |
    ConvertTo-Json | Set-Content -LiteralPath (Join-Path $oldRoot 'report.json')
$mismatchRefused = $false
try { Remove-StaleAcceptanceRunRoot -RunId $oldRunId -ProjectKey $oldRepo -TempRoot $script:HarnessRoot } catch { $mismatchRefused = $true }
Check 'preflight preserves a run root whose report identity disagrees' ($mismatchRefused -and (Test-Path -LiteralPath $oldRoot)) 'mismatched report'
$script:RunArchiveRoot = Join-Path $script:HarnessRoot 'run-archive'
$manifestRunId = 'ccdd0011'
$manifestRoot = Join-Path $script:HarnessRoot "orch-accept-$manifestRunId"
$manifestRepo = Join-Path $manifestRoot 'repo'
$manifestRecordDir = Join-Path $script:RunArchiveRoot $manifestRunId
New-Item -ItemType Directory -Path $manifestRepo -Force | Out-Null
$centralManifest = [pscustomobject]@{
    schemaVersion = 1
    runId = $manifestRunId
    session = "orch-acc-$manifestRunId"
    projectKey = $manifestRepo
    workRoot = $manifestRoot
    status = 'interrupted'
    pendingUserInteraction = $false
    resourcesPreserved = $false
    keepResourcesRequested = $false
}
Write-AtomicRunManifest -Manifest $centralManifest -Path (Join-Path $manifestRecordDir 'manifest.json')
$manifestRootRemoved = $false
$manifestRootError = ''
try {
    Remove-StaleAcceptanceRunRoot -RunId $manifestRunId -ProjectKey $manifestRepo -TempRoot $script:HarnessRoot
    $manifestRootRemoved = (-not (Test-Path -LiteralPath $manifestRoot)) -and
        (Test-Path -LiteralPath (Join-Path $manifestRecordDir 'manifest.json'))
} catch { $manifestRootError = $_.Exception.Message }
Check 'preflight verifies and removes a stale work root from its durable manifest without deleting the journal' `
    $manifestRootRemoved "root=$manifestRoot error=$manifestRootError"
$legacyRunId = 'eeff2233'
$legacyRoot = Join-Path $script:HarnessRoot "orch-accept-$legacyRunId"
$legacyRepo = Join-Path $legacyRoot 'repo'
New-Item -ItemType Directory -Path $legacyRepo -Force | Out-Null
$script:RetireStaleAcceptanceRunId = ''
$legacyMissingRefused = $false
try { [void](Read-AcceptanceRunIdentity -RunId $legacyRunId -ProjectKey $legacyRepo -TempRoot $script:HarnessRoot) } catch { $legacyMissingRefused = $true }
Check 'legacy missing-report identity is refused without exact run-id retirement' $legacyMissingRefused 'missing report must not authorize itself'
$script:RetireStaleAcceptanceRunId = $legacyRunId
$legacyIdentity = $null
try { $legacyIdentity = Read-AcceptanceRunIdentity -RunId $legacyRunId -ProjectKey $legacyRepo -TempRoot $script:HarnessRoot } catch { }
Check 'exact-ID legacy retirement uses only database-bound identity and forces consent' `
    ($null -ne $legacyIdentity -and
     (Get-Field $legacyIdentity 'runId') -ceq $legacyRunId -and
     (Test-JsonBool -Object $legacyIdentity -Name 'pendingUserInteraction') -eq $true -and
     (Test-JsonBool -Object $legacyIdentity -Name 'resourcesPreserved') -eq $true) `
    (Get-Json $legacyIdentity)
$script:RetireStaleAcceptanceRunId = ''
$cleanupFixtureId = 'aabb0011'
$cleanupFixtureRoot = Join-Path $script:HarnessRoot "orch-accept-$cleanupFixtureId"
$cleanupFixtureRepo = Join-Path $cleanupFixtureRoot 'repo'
$cleanupFixtureRecord = Join-Path $script:RunArchiveRoot $cleanupFixtureId
New-Item -ItemType Directory -Path $cleanupFixtureRepo -Force | Out-Null
$cleanupFixtureIdentity = [pscustomobject]@{
    runId = $cleanupFixtureId
    session = "orch-acc-$cleanupFixtureId"
    projectKey = $cleanupFixtureRepo
    workRoot = $cleanupFixtureRoot
    ownerPid = 2147483647
    ownerStartedAt = '2000-01-01T00:00:00.0000000Z'
    status = 'preserved'
    pendingUserInteraction = $true
    resourcesPreserved = $true
    keepResourcesRequested = $true
}
$cleanupLegacyReport = [pscustomobject]@{
    runId = $cleanupFixtureId
    session = "orch-acc-$cleanupFixtureId"
    projectKey = $cleanupFixtureRepo
    passed = 4
    failed = 0
    blocked = 1
    keepResourcesRequested = $true
    resourcesPreserved = $true
}
$cleanupLegacyReport | ConvertTo-Json | Set-Content -LiteralPath (Join-Path $cleanupFixtureRoot 'report.json') -Encoding UTF8
Set-Content -LiteralPath (Join-Path $cleanupFixtureRoot 'report.txt') -Value 'legacy acceptance report'
$cleanupStart = $null
try {
    $cleanupStart = Start-AcceptanceRunCleanup -RunId $cleanupFixtureId -ProjectKey $cleanupFixtureRepo `
        -Identity $cleanupFixtureIdentity -RetirementAuthorized $true -TempRoot $script:HarnessRoot
} catch { }
$cleanupManifestPath = Join-Path $cleanupFixtureRecord 'manifest.json'
$cleanupStarted = ($null -ne $cleanupStart -and (Test-Path -LiteralPath $cleanupManifestPath) -and
    (Test-Path -LiteralPath (Join-Path $cleanupFixtureRecord 'report.json')) -and
    ((Get-Field (Get-Content -LiteralPath $cleanupManifestPath -Raw | ConvertFrom-Json) 'retirementAuthorized') -eq $true))
Check 'retirement journals consent and archives legacy reports before touching the temp root' $cleanupStarted 'cleanup journal missing'
$cleanupCompleted = $false
try {
    Complete-AcceptanceRunCleanup -RunId $cleanupFixtureId
    $completedManifest = Get-Content -LiteralPath $cleanupManifestPath -Raw | ConvertFrom-Json
    $cleanupCompleted = ((Get-Field $completedManifest 'status') -ceq 'complete' -and
        (Test-JsonBool -Object $completedManifest -Name 'pendingUserInteraction') -eq $false -and
        (Test-JsonBool -Object $completedManifest -Name 'resourcesPreserved') -eq $false -and
        (Test-Path -LiteralPath $cleanupFixtureRoot))
} catch { }
Check 'completed retirement finalizes the central manifest while preserving its report' $cleanupCompleted 'completion journal'
$legacyReportTemp = Join-Path $script:HarnessRoot 'legacy-report-temp'
$legacyReportId = '1122aa33'
$legacyReportRoot = Join-Path $legacyReportTemp "orch-accept-$legacyReportId"
$legacyReportRecord = Join-Path $script:RunArchiveRoot $legacyReportId
New-Item -ItemType Directory -Path $legacyReportRoot -Force | Out-Null
@{
    runId = $legacyReportId
    session = "orch-acc-$legacyReportId"
    projectKey = ''
    keepResourcesRequested = $false
    resourcesPreserved = $false
    passed = 3
    failed = 1
    blocked = 0
} | ConvertTo-Json | Set-Content -LiteralPath (Join-Path $legacyReportRoot 'report.json') -Encoding UTF8
Set-Content -LiteralPath (Join-Path $legacyReportRoot 'report.txt') -Value 'legacy report-only run'
$emptyRunState = [pscustomobject]@{ tables = [pscustomobject]@{ projects = @(); tasks = @(); inbox = @(); ready_sessions = @() } }
$legacyReportMigrated = $false
$legacyMigrationError = ''
try {
    $legacyReportMigrated = Move-LegacyAcceptanceReportRootToArchive -RunId $legacyReportId -TempRoot $legacyReportTemp `
        -ArchiveRoot $script:RunArchiveRoot -StateDump $emptyRunState -Sessions @() -DefaultProjection ''
} catch { $legacyMigrationError = $_.Exception.Message }
$legacyManifestPath = Join-Path $legacyReportRecord 'manifest.json'
$legacyReportMigrated = $legacyReportMigrated -and
    (-not (Test-Path -LiteralPath $legacyReportRoot)) -and
    (Test-Path -LiteralPath (Join-Path $legacyReportRecord 'report.json')) -and
    ((Get-Field (Get-Content -LiteralPath $legacyManifestPath -Raw | ConvertFrom-Json) 'status') -eq 'complete')
Check 'report-only legacy temp root moves its evidence into the bounded archive and is removed' $legacyReportMigrated `
    ("root=$legacyReportRoot migrationError=$legacyMigrationError")
$legacyBlockedId = '2233aa44'
$legacyBlockedRoot = Join-Path $legacyReportTemp "orch-accept-$legacyBlockedId"
New-Item -ItemType Directory -Path $legacyBlockedRoot -Force | Out-Null
@{ runId = $legacyBlockedId; session = "orch-acc-$legacyBlockedId"; projectKey = ''; keepResourcesRequested = $false; resourcesPreserved = $false } |
    ConvertTo-Json | Set-Content -LiteralPath (Join-Path $legacyBlockedRoot 'report.json') -Encoding UTF8
Set-Content -LiteralPath (Join-Path $legacyBlockedRoot 'unowned.dat') -Value 'do not delete'
$legacyExtraPreserved = $false
try {
    $legacyExtraPreserved = -not (Move-LegacyAcceptanceReportRootToArchive -RunId $legacyBlockedId -TempRoot $legacyReportTemp `
        -ArchiveRoot $script:RunArchiveRoot -StateDump $emptyRunState -Sessions @() -DefaultProjection '')
} catch { $legacyExtraPreserved = $true }
Check 'legacy root with anything beyond its report files is never swept as report-only' `
    ($legacyExtraPreserved -and (Test-Path -LiteralPath $legacyBlockedRoot)) 'unexpected cleanup'
# A tickets row for the run's project blocks the report-only sweep exactly like a task row:
# the same root is refused while the row is present and swept once it is gone, so the row
# alone caused the refusal.
$legacyTicketId = '3344aa55'
$legacyTicketRoot = Join-Path $legacyReportTemp "orch-accept-$legacyTicketId"
$legacyTicketRepo = Join-Path $legacyTicketRoot 'repo'
New-Item -ItemType Directory -Path $legacyTicketRoot -Force | Out-Null
@{ runId = $legacyTicketId; session = "orch-acc-$legacyTicketId"; projectKey = $legacyTicketRepo; keepResourcesRequested = $false; resourcesPreserved = $false } |
    ConvertTo-Json | Set-Content -LiteralPath (Join-Path $legacyTicketRoot 'report.json') -Encoding UTF8
Set-Content -LiteralPath (Join-Path $legacyTicketRoot 'report.txt') -Value 'legacy report-only run with a ticket row'
$legacyTicketState = [pscustomobject]@{ tables = [pscustomobject]@{
    projects = @(); tasks = @(); inbox = @(); ready_sessions = @()
    tickets = @([pscustomobject]@{ id = 'tkt_leftover00001'; project_key = $legacyTicketRepo })
} }
$legacyTicketSwept = $true
try {
    $legacyTicketSwept = Move-LegacyAcceptanceReportRootToArchive -RunId $legacyTicketId -TempRoot $legacyReportTemp `
        -ArchiveRoot $script:RunArchiveRoot -StateDump $legacyTicketState -Sessions @() -DefaultProjection ''
} catch { $legacyTicketSwept = $false }
Check 'legacy report-only sweep is refused while a tickets row still references the run project' `
    ((-not $legacyTicketSwept) -and (Test-Path -LiteralPath (Join-Path $legacyTicketRoot 'report.json'))) 'the ticket row was ignored'
$legacyTicketSweptAfter = $false
try {
    $legacyTicketSweptAfter = Move-LegacyAcceptanceReportRootToArchive -RunId $legacyTicketId -TempRoot $legacyReportTemp `
        -ArchiveRoot $script:RunArchiveRoot -StateDump $emptyRunState -Sessions @() -DefaultProjection ''
} catch { }
Check 'the same legacy root is swept once no ticket row references it' `
    ($legacyTicketSweptAfter -and -not (Test-Path -LiteralPath $legacyTicketRoot)) 'control: the refusal was not caused by the ticket row alone'
$manifestOnlyTemp = Join-Path $script:HarnessRoot 'manifest-only-temp'
$manifestOnlyId = '44556677'
$manifestOnlyRoot = Join-Path $manifestOnlyTemp "orch-accept-$manifestOnlyId"
$manifestOnlyRepo = Join-Path $manifestOnlyRoot 'repo'
$manifestOnlyRecord = Join-Path $script:RunArchiveRoot $manifestOnlyId
New-Item -ItemType Directory -Path $manifestOnlyRepo -Force | Out-Null
$manifestOnlyDb = Join-Path $script:HarnessRoot 'manifest-only.sqlite'
[void](Invoke-Native -File $script:Python -Arguments @($seedPath, $manifestOnlyDb) -TimeoutSeconds 60)
$savedLockStore = $script:RunLockStateDb
$script:RunLockStateDb = $manifestOnlyDb
$manifestOnlyRecordData = [pscustomobject]@{
    schemaVersion = 2
    runId = $manifestOnlyId
    session = "orch-acc-$manifestOnlyId"
    projectKey = $manifestOnlyRepo
    workRoot = $manifestOnlyRoot
    stateDb = $manifestOnlyDb
    ownerPid = 2147483647
    ownerStartedAt = '2000-01-01T00:00:00.0000000Z'
    serverPid = 2147483647
    serverStartedAt = '2000-01-01T00:00:00.0000000Z'
    serverExecutable = (Get-Command herdr).Source
    serverStartAttempted = $true
    sessionDir = ''
    status = 'active'
    pendingUserInteraction = $false
    resourcesPreserved = $false
    keepResourcesRequested = $false
    createdWorkspaces = @()
    createdPanes = @()
}
Write-AtomicRunManifest -Manifest $manifestOnlyRecordData -Path (Join-Path $manifestOnlyRecord 'manifest.json')
$defaultRecordForRecovery = [pscustomobject]@{ name = 'default'; session_dir = (Join-Path $script:HarnessRoot 'sessions'); running = $true }
$recoveredManifestOnly = $false
try {
    $recoveredManifestOnly = Recover-ManifestOnlyAcceptanceRun -RunId $manifestOnlyId -TempRoot $manifestOnlyTemp `
        -StateDump (Get-StateDump) -Sessions @($defaultRecordForRecovery) -SessionsRoot (Join-Path $defaultRecordForRecovery.session_dir 'sessions') `
        -DefaultProjection ''
} catch { }
$manifestOnlyFinal = Get-Content -LiteralPath (Join-Path $manifestOnlyRecord 'manifest.json') -Raw | ConvertFrom-Json
Check 'abandoned pre-bind manifest with absent session removes only its exact temp root and finalizes the journal' `
    ($recoveredManifestOnly -and -not (Test-Path -LiteralPath $manifestOnlyRoot) -and
     (Get-Field $manifestOnlyFinal 'status') -ceq 'complete') 'manifest-only recovery'
$pendingOnlyId = '44556678'
$pendingOnlyRoot = Join-Path $manifestOnlyTemp "orch-accept-$pendingOnlyId"
$pendingOnlyRepo = Join-Path $pendingOnlyRoot 'repo'
New-Item -ItemType Directory -Path $pendingOnlyRepo -Force | Out-Null
$pendingOnlyManifest = $manifestOnlyRecordData.PSObject.Copy()
$pendingOnlyManifest.runId = $pendingOnlyId
$pendingOnlyManifest.session = "orch-acc-$pendingOnlyId"
$pendingOnlyManifest.projectKey = $pendingOnlyRepo
$pendingOnlyManifest.workRoot = $pendingOnlyRoot
$pendingOnlyManifest.status = 'preserved'
    $pendingOnlyManifest.pendingUserInteraction = $true
    $pendingOnlyManifest.resourcesPreserved = $true
    $pendingOnlyManifest.keepResourcesRequested = $true
    $pendingOnlyRecord = Join-Path $script:RunArchiveRoot $pendingOnlyId
    Write-AtomicRunManifest -Manifest $pendingOnlyManifest -Path (Join-Path $pendingOnlyRecord 'manifest.json')
    $pendingRecoverySucceeded = $false
    try {
        $pendingRecoverySucceeded = Recover-ManifestOnlyAcceptanceRun -RunId $pendingOnlyId -TempRoot $manifestOnlyTemp `
            -StateDump (Get-StateDump) -Sessions @($defaultRecordForRecovery) -SessionsRoot (Join-Path $defaultRecordForRecovery.session_dir 'sessions') `
            -DefaultProjection ''
    } catch { }
    $pendingOnlyFinal = Get-Content -LiteralPath (Join-Path $pendingOnlyRecord 'manifest.json') -Raw | ConvertFrom-Json
    Check 'new pre-run cleanup retires a preserved pending KeepResources run with a dead owner' `
        ($pendingRecoverySucceeded -and -not (Test-Path -LiteralPath $pendingOnlyRoot) -and
         (Get-Field $pendingOnlyFinal 'status') -ceq 'complete') (Get-Json $pendingOnlyFinal)
    # A tickets row for the run's project blocks manifest-only recovery as a task row does;
    # with the row gone the same run recovers, so the row alone caused the refusal.
    $ticketBlockedId = '44556679'
    $ticketBlockedRoot = Join-Path $manifestOnlyTemp "orch-accept-$ticketBlockedId"
    $ticketBlockedRepo = Join-Path $ticketBlockedRoot 'repo'
    New-Item -ItemType Directory -Path $ticketBlockedRepo -Force | Out-Null
    $ticketBlockedManifest = $manifestOnlyRecordData.PSObject.Copy()
    $ticketBlockedManifest.runId = $ticketBlockedId
    $ticketBlockedManifest.session = "orch-acc-$ticketBlockedId"
    $ticketBlockedManifest.projectKey = $ticketBlockedRepo
    $ticketBlockedManifest.workRoot = $ticketBlockedRoot
    Write-AtomicRunManifest -Manifest $ticketBlockedManifest -Path (Join-Path (Join-Path $script:RunArchiveRoot $ticketBlockedId) 'manifest.json')
    $ticketBlockedDump = [pscustomobject]@{ tables = [pscustomobject]@{
        projects = @(); tasks = @(); inbox = @(); ready_sessions = @()
        tickets = @([pscustomobject]@{ id = 'tkt_leftover00002'; project_key = $ticketBlockedRepo })
    } }
    $ticketBlockedError = ''
    try {
        [void](Recover-ManifestOnlyAcceptanceRun -RunId $ticketBlockedId -TempRoot $manifestOnlyTemp `
            -StateDump $ticketBlockedDump -Sessions @($defaultRecordForRecovery) `
            -SessionsRoot (Join-Path $defaultRecordForRecovery.session_dir 'sessions') -DefaultProjection '')
    } catch { $ticketBlockedError = $_.Exception.Message }
    Check 'manifest-only recovery refuses while a tickets row still references the run project' `
        (($ticketBlockedError -match 'durable tickets rows still reference') -and (Test-Path -LiteralPath $ticketBlockedRoot)) $ticketBlockedError
    $ticketFreedRecovered = $false
    try {
        $ticketFreedRecovered = Recover-ManifestOnlyAcceptanceRun -RunId $ticketBlockedId -TempRoot $manifestOnlyTemp `
            -StateDump (Get-StateDump) -Sessions @($defaultRecordForRecovery) `
            -SessionsRoot (Join-Path $defaultRecordForRecovery.session_dir 'sessions') -DefaultProjection ''
    } catch { }
    Check 'the same manifest-only run recovers once no ticket row references it' `
        ($ticketFreedRecovered -and -not (Test-Path -LiteralPath $ticketBlockedRoot)) 'control: the refusal was not caused by the ticket row alone'
$script:RunLockStateDb = $savedLockStore
$interruptedRunId = 'eeddbb11'
$interruptedRoot = Join-Path $script:HarnessRoot "orch-accept-$interruptedRunId"
$interruptedRepo = Join-Path $interruptedRoot 'repo'
New-Item -ItemType Directory -Path $interruptedRepo -Force | Out-Null
$savedIdentity = [pscustomobject]@{
    RunId = $script:RunId; SessionName = $script:SessionName; RepoPath = $script:RepoPath
    ReportJson = $script:ReportJson; ReportText = $script:ReportText; WorkRoot = $script:WorkRoot
    Project = $script:Project; StartedAt = $script:StartedAt
    PrimaryPane = $script:PrimaryPane; PrimaryWorkspace = $script:PrimaryWorkspace; PrimaryTab = $script:PrimaryTab
    KeepResourcesRequested = $script:KeepResourcesRequested
}
$script:RunId = $interruptedRunId
$script:SessionName = "orch-acc-$interruptedRunId"
$script:RepoPath = $interruptedRepo
$script:WorkRoot = $interruptedRoot
$script:ReportJson = Join-Path $interruptedRoot 'report.json'
$script:ReportText = Join-Path $interruptedRoot 'report.txt'
$script:RunArchiveRoot = Join-Path $script:HarnessRoot 'run-archive'
$script:RunRecordDir = Join-Path $script:RunArchiveRoot $interruptedRunId
$script:RunManifestPath = Join-Path $script:RunRecordDir 'manifest.json'
$script:RunManifest = $null
$script:SessionDir = ''
$script:OwnerProcessStartedAt = (Get-Process -Id $PID).StartTime.ToUniversalTime().ToString('o')
$script:StartedAt = '2026-01-01T00:00:00.0000000Z'
$script:PrimaryPane = 'w9:p9'; $script:PrimaryWorkspace = 'w9'; $script:PrimaryTab = 'w9:t9'
$script:Project = $null
$script:KeepResourcesRequested = $true
# The bind action is the run's first Herdr side effect, so the identity the next
# preflight needs must already be on disk when it is invoked. The stub captures
# both the compatibility report and durable lifecycle manifest.
$script:IdentityAtBindInvocation = $null
$script:ManifestAtBindInvocation = $null
function Invoke-PluginAction {
    param(
        [Parameter(Mandatory)][string]$ActionId,
        [hashtable]$PaneContext = @{},
        [double]$TimeoutSeconds = 180,
        [switch]$NoSessionScope
    )
    $script:IdentityAtBindInvocation = if (Test-Path -LiteralPath $script:ReportJson) {
        Get-Content -LiteralPath $script:ReportJson -Raw | ConvertFrom-Json
    } else { $null }
    $script:ManifestAtBindInvocation = if (Test-Path -LiteralPath $script:RunManifestPath) {
        Get-Content -LiteralPath $script:RunManifestPath -Raw | ConvertFrom-Json
    } else { $null }
    throw 'stub: the helper harness never starts Herdr'
}
try { Invoke-Bind } catch { }
$identityAtBind = $script:IdentityAtBindInvocation
Check 'an interrupted run records a verifiable project identity before the bind action' `
    ($null -ne $identityAtBind -and
     (Get-Field $identityAtBind 'runId') -ceq $interruptedRunId -and
     (Get-Field $identityAtBind 'session') -ceq "orch-acc-$interruptedRunId" -and
     (Test-SamePath -A (Get-Field $identityAtBind 'projectKey') -B $interruptedRepo)) `
    'report identity at the bind invocation'
$manifestAtBind = $script:ManifestAtBindInvocation
Check 'a durable run manifest exists before the first bind side effect' `
    ($null -ne $manifestAtBind -and
     (Get-Field $manifestAtBind 'runId') -ceq $interruptedRunId -and
     (Get-Field $manifestAtBind 'session') -ceq "orch-acc-$interruptedRunId" -and
     (Test-SamePath -A (Get-Field $manifestAtBind 'projectKey') -B $interruptedRepo) -and
     (Get-Field $manifestAtBind 'phase') -eq 'bind') `
    'run manifest at bind'
$finalFallbackKept = $false
$finalKeepIntent = $false
try {
    [void](Write-Report)
    $finalReport = Get-Content -LiteralPath $script:ReportJson -Raw | ConvertFrom-Json
    $finalFallbackKept = ((Get-Field $finalReport 'runId') -ceq $interruptedRunId -and
        (Test-SamePath -A (Get-Field $finalReport 'projectKey') -B $interruptedRepo))
    $finalKeepIntent = (Test-JsonBool -Object $finalReport -Name 'keepResourcesRequested') -eq $true
} catch { }
Check 'the final report keeps the run repository key when bind returned no project' $finalFallbackKept 'final report project key'
Check 'the final report records explicit KeepResources intent' $finalKeepIntent 'keepResourcesRequested'
$script:RunId = $savedIdentity.RunId
$script:SessionName = $savedIdentity.SessionName
$script:RepoPath = $savedIdentity.RepoPath
$script:ReportJson = $savedIdentity.ReportJson
$script:ReportText = $savedIdentity.ReportText
$script:WorkRoot = $savedIdentity.WorkRoot
$script:Project = $savedIdentity.Project
$script:StartedAt = $savedIdentity.StartedAt
$script:PrimaryPane = $savedIdentity.PrimaryPane
$script:PrimaryWorkspace = $savedIdentity.PrimaryWorkspace
$script:PrimaryTab = $savedIdentity.PrimaryTab
$script:KeepResourcesRequested = $savedIdentity.KeepResourcesRequested
$runArchiveRoot = Join-Path $script:HarnessRoot 'run-archive'
$runRoot = Join-Path $script:HarnessRoot 'active-run'
$runManifestPath = Join-Path $runArchiveRoot 'a1b2c3d4\manifest.json'
New-Item -ItemType Directory -Path $runRoot -Force | Out-Null
$runManifest = [pscustomobject]@{
    schemaVersion = 1
    runId = 'a1b2c3d4'
    session = 'orch-acc-a1b2c3d4'
    projectKey = Join-Path $runRoot 'repo'
    workRoot = $runRoot
    ownerPid = $PID
    ownerStartedAt = (Get-Process -Id $PID).StartTime.ToUniversalTime().ToString('o')
    status = 'active'
    pendingUserInteraction = $false
    resourcesPreserved = $false
    finishedAt = ''
}
$atomicManifestWrite = $false
try {
    Write-AtomicRunManifest -Manifest $runManifest -Path $runManifestPath
    $savedManifest = Get-Content -LiteralPath $runManifestPath -Raw | ConvertFrom-Json
    $atomicManifestWrite = ((Get-Field $savedManifest 'runId') -ceq 'a1b2c3d4' -and
        (Get-Field $savedManifest 'session') -ceq 'orch-acc-a1b2c3d4' -and
        (Get-Field $savedManifest 'status') -ceq 'active' -and
        @(Get-ChildItem -LiteralPath (Split-Path $runManifestPath) -Filter '*.tmp').Count -eq 0)
} catch { }
Check 'run manifest is atomically persisted with exact run identity and no temp residue' $atomicManifestWrite $runManifestPath
$runManifest.status = 'cleanup'
$atomicManifestReplacement = $false
try {
    Write-AtomicRunManifest -Manifest $runManifest -Path $runManifestPath
    $replacedManifest = Get-Content -LiteralPath $runManifestPath -Raw | ConvertFrom-Json
    $residualManifestTemp = @(Get-ChildItem -LiteralPath (Split-Path $runManifestPath) -Force | Where-Object { $_.Name -match '\.(tmp|bak)(\.|$)' })
    $atomicManifestReplacement = ((Get-Field $replacedManifest 'status') -ceq 'cleanup' -and $residualManifestTemp.Count -eq 0)
} catch { }
Check 'manifest replacement is atomic and cleans its backup/temp files' $atomicManifestReplacement $runManifestPath
$ownerIsCurrent = $false
try {
    $ownerIsCurrent = Test-RunOwnerAlive -OwnerPid $PID -OwnerStartedAt $runManifest.ownerStartedAt
} catch { }
Check 'run owner identity recognizes the current process' $ownerIsCurrent 'current PID/start-time pair'
$ownerIsAbsent = $true
try { $ownerIsAbsent = Test-RunOwnerAlive -OwnerPid 2147483647 -OwnerStartedAt '2000-01-01T00:00:00.0000000Z' } catch { }
Check 'run owner identity treats a missing process as dead' (-not $ownerIsAbsent) 'missing PID'
$processIdentityCommand = Get-Command Get-AcceptanceProcessDisposition -ErrorAction SilentlyContinue
Check 'acceptance server process identity helper exists' ($null -ne $processIdentityCommand) 'missing process identity helper'
if ($processIdentityCommand) {
    $currentProcessStart = (Get-Process -Id $PID).StartTime.ToUniversalTime().ToString('o')
    $liveProcessMode = Get-AcceptanceProcessDisposition -ProcessId $PID -StartedAt $currentProcessStart
    $reusedPidMode = Get-AcceptanceProcessDisposition -ProcessId $PID -StartedAt '2000-01-01T00:00:00.0000000Z'
    $missingProcessMode = Get-AcceptanceProcessDisposition -ProcessId 2147483647 -StartedAt '2000-01-01T00:00:00.0000000Z'
    Check 'server process identity recognizes an exact live PID/start pair' ($liveProcessMode -eq 'active') $liveProcessMode
    Check 'server process identity treats a reused PID as the old process exited' ($reusedPidMode -eq 'dead') $reusedPidMode
    Check 'server process identity recognizes a missing PID as exited' ($missingProcessMode -eq 'dead') $missingProcessMode
}
$serverStopCommand = Get-Command Stop-VerifiedAcceptanceServerProcess -ErrorAction SilentlyContinue
Check 'verified acceptance server stop helper exists' ($null -ne $serverStopCommand) 'missing server stop helper'
if ($serverStopCommand) {
    $liveServerManifest = [pscustomobject]@{
        runId = '12345678'
        session = 'orch-acc-12345678'
        serverPid = $PID
        serverStartedAt = (Get-Process -Id $PID).StartTime.ToUniversalTime().ToString('o')
        serverExecutable = (Get-Command powershell.exe).Source
    }
    $liveServerRefused = $false
    try { [void](Stop-VerifiedAcceptanceServerProcess -RunId '12345678' -Manifest $liveServerManifest -SessionDisposition 'stopped') } catch {
        $liveServerRefused = $true
    }
    Check 'stale-server cleanup refuses to kill the current PowerShell process' `
        ($liveServerRefused -and (Get-Process -Id $PID -ErrorAction SilentlyContinue)) 'current process was not protected'

    $reusedServerManifest = [pscustomobject]@{
        runId = '12345678'
        session = 'orch-acc-12345678'
        serverPid = $PID
        serverStartedAt = '2000-01-01T00:00:00.0000000Z'
        serverExecutable = (Get-Command powershell.exe).Source
    }
    $reusedServerResult = Stop-VerifiedAcceptanceServerProcess -RunId '12345678' `
        -Manifest $reusedServerManifest -SessionDisposition 'stopped'
    Check 'stale-server cleanup never kills a reused PID' `
        ($reusedServerResult -eq 'already-exited' -and (Get-Process -Id $PID -ErrorAction SilentlyContinue)) $reusedServerResult
    $neverStartedManifest = [pscustomobject]@{
        schemaVersion = 2
        runId = '12345678'
        session = 'orch-acc-12345678'
        serverPid = 0
        serverStartedAt = ''
        serverExecutable = (Get-Command herdr).Source
        serverStartAttempted = $false
        sessionDir = ''
        phase = 'temp-root-created'
    }
    $notRecordedResult = Stop-VerifiedAcceptanceServerProcess -RunId '12345678' `
        -Manifest $neverStartedManifest -SessionDisposition 'absent'
    Check 'a missing server PID is accepted only when a version-2 manifest proves it never started' `
        ($notRecordedResult -eq 'not-recorded') $notRecordedResult
    $uncertainStartManifest = $neverStartedManifest.PSObject.Copy()
    $uncertainStartManifest.phase = 'server-starting'
    $uncertainStartManifest.serverStartAttempted = $true
    $uncertainStartRefused = $false
    try { [void](Stop-VerifiedAcceptanceServerProcess -RunId '12345678' -Manifest $uncertainStartManifest -SessionDisposition 'absent') } catch {
        $uncertainStartRefused = $true
    }
    Check 'a missing server PID after launch was attempted blocks cleanup' `
        $uncertainStartRefused 'server start attempt was treated as no process'
}

$safeInterruptedManifest = [pscustomobject]@{
    ownerPid = 2147483647
    ownerStartedAt = '2000-01-01T00:00:00.0000000Z'
    status = 'active'
    pendingUserInteraction = $false
    resourcesPreserved = $false
    keepResourcesRequested = $false
}
$safeRecoveryMode = 'unknown'
try { $safeRecoveryMode = Get-AcceptanceRecoveryDisposition -Manifest $safeInterruptedManifest -SessionDisposition 'running' } catch { }
Check 'dead non-interactive run is eligible for exact-session recovery' ($safeRecoveryMode -eq 'recover') $safeRecoveryMode
$pendingRecoveryManifest = $safeInterruptedManifest.PSObject.Copy()
$absentRecoveryMode = 'unknown'
try { $absentRecoveryMode = Get-AcceptanceRecoveryDisposition -Manifest $safeInterruptedManifest -SessionDisposition 'absent' } catch { }
Check 'dead owner with an absent session is eligible for cleanup recovery' ($absentRecoveryMode -eq 'recover') $absentRecoveryMode
$unknownOwnerManifest = $safeInterruptedManifest.PSObject.Copy()
$unknownOwnerManifest.ownerPid = 0
$unknownRecoveryMode = 'unknown'
try { $unknownRecoveryMode = Get-AcceptanceRecoveryDisposition -Manifest $unknownOwnerManifest -SessionDisposition 'absent' } catch { }
Check 'absent session with unknown owner metadata is not eligible for automatic cleanup' ($unknownRecoveryMode -eq 'unverified') $unknownRecoveryMode
    $pendingRecoveryManifest.pendingUserInteraction = $true
    $pendingRecoveryMode = 'unknown'
    try { $pendingRecoveryMode = Get-AcceptanceRecoveryDisposition -Manifest $pendingRecoveryManifest -SessionDisposition 'running' } catch { }
    Check 'dead acceptance run with a pending interaction is recoverable under the pre-run cleanup instruction' `
        ($pendingRecoveryMode -eq 'recover') $pendingRecoveryMode
    $keptRecoveryManifest = $pendingRecoveryManifest.PSObject.Copy()
    $keptRecoveryManifest.keepResourcesRequested = $true
    $keptRecoveryMode = 'unknown'
    try { $keptRecoveryMode = Get-AcceptanceRecoveryDisposition -Manifest $keptRecoveryManifest -SessionDisposition 'running' } catch { }
    Check 'a dead owner with an explicit KeepResources request remains eligible for the next-run cleanup' `
        ($keptRecoveryMode -eq 'recover') $keptRecoveryMode
$authorizedCleanupManifest = $pendingRecoveryManifest.PSObject.Copy()
$authorizedCleanupManifest.status = 'cleanup'
Add-Member -InputObject $authorizedCleanupManifest -NotePropertyName retirementAuthorized -NotePropertyValue $true
$authorizedCleanupMode = 'unknown'
try { $authorizedCleanupMode = Get-AcceptanceRecoveryDisposition -Manifest $authorizedCleanupManifest -SessionDisposition 'running' } catch { }
Check 'a persisted exact-ID retirement resumes idempotently after an interrupted cleanup' `
    ($authorizedCleanupMode -eq 'recover') $authorizedCleanupMode
$liveAuthorizedCleanup = $authorizedCleanupManifest.PSObject.Copy()
$liveAuthorizedCleanup.ownerPid = $PID
$liveAuthorizedCleanup.ownerStartedAt = (Get-Process -Id $PID).StartTime.ToUniversalTime().ToString('o')
$liveCleanupModes = @()
foreach ($sessionMode in @('running', 'stopped', 'absent')) {
    $mode = 'unknown'
    try { $mode = Get-AcceptanceRecoveryDisposition -Manifest $liveAuthorizedCleanup -SessionDisposition $sessionMode } catch { }
    $liveCleanupModes += $mode
}
Check 'cleanup retirement flag never overrides a live owner, regardless of session listing' `
    (@($liveCleanupModes | Where-Object { $_ -ne 'active' }).Count -eq 0) ($liveCleanupModes -join ',')
$unknownAuthorizedCleanup = $pendingRecoveryManifest.PSObject.Copy()
$unknownAuthorizedCleanup.ownerPid = 0
$unknownAuthorizedCleanup.status = 'cleanup'
Add-Member -InputObject $unknownAuthorizedCleanup -NotePropertyName retirementAuthorized -NotePropertyValue $true
$unknownAuthorizedMode = 'unknown'
try { $unknownAuthorizedMode = Get-AcceptanceRecoveryDisposition -Manifest $unknownAuthorizedCleanup -SessionDisposition 'absent' } catch { }
Check 'persisted cleanup authorization never overrides a missing owner identity' ($unknownAuthorizedMode -eq 'unverified') $unknownAuthorizedMode
$liveRecoveryManifest = $safeInterruptedManifest.PSObject.Copy()
$liveRecoveryManifest.ownerPid = $PID
$liveRecoveryManifest.ownerStartedAt = (Get-Process -Id $PID).StartTime.ToUniversalTime().ToString('o')
$liveRecoveryMode = 'unknown'
try { $liveRecoveryMode = Get-AcceptanceRecoveryDisposition -Manifest $liveRecoveryManifest -SessionDisposition 'running' } catch { }
Check 'live run owner is never classified as abandoned' ($liveRecoveryMode -eq 'active') $liveRecoveryMode
$runLockPath = Join-Path $script:HarnessRoot 'acceptance.lock'
$runLock = $null
$duplicateRunLock = $null
try {
    $runLock = Enter-AcceptanceRunLock -Path $runLockPath
    $duplicateRunLock = Enter-AcceptanceRunLock -Path $runLockPath
} catch { }
Check 'only one runner acquires the same exclusive lock' ($null -ne $runLock -and $null -eq $duplicateRunLock) 'duplicate acquisition'
if ($duplicateRunLock) { Exit-AcceptanceRunLock -Lock $duplicateRunLock }
if ($runLock) { Exit-AcceptanceRunLock -Lock $runLock }
$releasedRunLock = $null
try { $releasedRunLock = Enter-AcceptanceRunLock -Path $runLockPath } catch { }
Check 'the run lock becomes available after its owner releases it' ($null -ne $releasedRunLock) 'lock remained held'
if ($releasedRunLock) { Exit-AcceptanceRunLock -Lock $releasedRunLock }
$storeLockA = Get-AcceptanceRunLockPath -StateDb 'C:\Herdr\plugins\orch\orchestrator.sqlite' -Root $script:HarnessRoot
$storeLockSame = Get-AcceptanceRunLockPath -StateDb 'c:/herdr/plugins/orch/orchestrator.sqlite' -Root $script:HarnessRoot
$storeLockOther = Get-AcceptanceRunLockPath -StateDb 'C:\Herdr\plugins\other\orchestrator.sqlite' -Root $script:HarnessRoot
Check 'run lock keys the canonical shared SQLite store rather than repository path' `
    (($storeLockA -ceq $storeLockSame) -and ($storeLockA -cne $storeLockOther)) `
    "same=[$storeLockSame] other=[$storeLockOther]"
$crossProcessLockPath = Join-Path $script:HarnessRoot 'cross-process.lock'
$readyPath = Join-Path $script:HarnessRoot 'cross-process.ready'
$releasePath = Join-Path $script:HarnessRoot 'cross-process.release'
$holderPath = Join-Path $script:HarnessRoot 'lock-holder.ps1'
@'
param([string]$LockPath, [string]$ReadyPath, [string]$ReleasePath)
$directory = Split-Path -Parent $LockPath
[void](New-Item -ItemType Directory -Path $directory -Force)
$stream = [System.IO.File]::Open($LockPath, [System.IO.FileMode]::OpenOrCreate, [System.IO.FileAccess]::ReadWrite, [System.IO.FileShare]::None)
try {
    [System.IO.File]::WriteAllText($ReadyPath, 'ready')
    while (-not (Test-Path -LiteralPath $ReleasePath)) { Start-Sleep -Milliseconds 50 }
} finally { $stream.Dispose() }
'@ | Set-Content -LiteralPath $holderPath -Encoding UTF8
$child = $null
$crossProcessBlocked = $false
$crossProcessReleased = $false
try {
    $psi = New-Object System.Diagnostics.ProcessStartInfo
    $psi.FileName = (Get-Command powershell.exe).Source
    $psi.Arguments = (@('-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', $holderPath, $crossProcessLockPath, $readyPath, $releasePath) |
        ForEach-Object { ConvertTo-ArgumentLiteral $_ }) -join ' '
    $psi.UseShellExecute = $false
    $psi.CreateNoWindow = $true
    $child = New-Object System.Diagnostics.Process
    $child.StartInfo = $psi
    [void]$child.Start()
    for ($waitIndex = 0; $waitIndex -lt 100 -and -not (Test-Path -LiteralPath $readyPath); $waitIndex++) {
        Start-Sleep -Milliseconds 50
    }
    if (Test-Path -LiteralPath $readyPath) {
        $contendedLock = Enter-AcceptanceRunLock -Path $crossProcessLockPath
        $crossProcessBlocked = ($null -eq $contendedLock)
        if ($contendedLock) { Exit-AcceptanceRunLock -Lock $contendedLock }
        $child.Kill()
        $crossProcessReleased = $child.WaitForExit(10000)
        if ($crossProcessReleased) {
            $afterChildExit = Enter-AcceptanceRunLock -Path $crossProcessLockPath
            $crossProcessReleased = ($null -ne $afterChildExit)
            if ($afterChildExit) { Exit-AcceptanceRunLock -Lock $afterChildExit }
        }
    }
} catch {
    $crossProcessBlocked = $false
    $crossProcessReleased = $false
} finally {
    if (-not (Test-Path -LiteralPath $releasePath)) { Set-Content -LiteralPath $releasePath -Value 'release' }
    if ($child) {
        if (-not $child.HasExited) { try { $child.Kill() } catch { } }
        $child.Dispose()
    }
}
Check 'a separate PowerShell process cannot take the same store lock and OS release frees it after force termination' `
    ($crossProcessBlocked -and $crossProcessReleased) "blocked=$crossProcessBlocked releasedAfterKill=$crossProcessReleased"
$preflightWorkRoot = Join-Path $script:HarnessRoot 'must-not-exist-before-preflight'
$preflightRecordDir = Join-Path $script:HarnessRoot 'preflight-record'
$preflightLockPath = Join-Path $script:HarnessRoot 'preflight.lock'
$preflightManifestPath = Join-Path $preflightRecordDir 'manifest.json'
$preflightManifest = [pscustomobject]@{
    schemaVersion = 1
    runId = 'aabbccdd'
    session = 'orch-acc-aabbccdd'
    projectKey = Join-Path $preflightWorkRoot 'repo'
    workRoot = $preflightWorkRoot
    ownerPid = $PID
    ownerStartedAt = (Get-Process -Id $PID).StartTime.ToUniversalTime().ToString('o')
    status = 'active'
    pendingUserInteraction = $false
    resourcesPreserved = $false
    keepResourcesRequested = $false
}
$initializedRunLock = $null
try {
    $initializedRunLock = Initialize-AcceptanceRun -LockPath $preflightLockPath `
        -RecordDir $preflightRecordDir -Manifest $preflightManifest
} catch { }
$preflightBeforeTempRoot = ($null -ne $initializedRunLock -and
    (Test-Path -LiteralPath $preflightManifestPath) -and
    (-not (Test-Path -LiteralPath $preflightWorkRoot)) -and
    ((Get-Field (Get-Content -LiteralPath $preflightManifestPath -Raw | ConvertFrom-Json) 'status') -eq 'active'))
Check 'preflight creates durable journal under the lock without creating the working temp root' $preflightBeforeTempRoot 'temp root created too early'
if ($initializedRunLock) { Exit-AcceptanceRunLock -Lock $initializedRunLock }
$retentionRoot = Join-Path $script:HarnessRoot 'retention-archive'
$archiveNow = [DateTime]::UtcNow
foreach ($archiveRun in @(
    @{ Id = '00000001'; Age = 0; Status = 'complete'; Preserved = $false }
    @{ Id = '00000002'; Age = 1; Status = 'complete'; Preserved = $false }
    @{ Id = '00000003'; Age = 2; Status = 'complete'; Preserved = $false }
    @{ Id = '00000004'; Age = 100; Status = 'preserved'; Preserved = $true }
    @{ Id = '00000005'; Age = 0; Status = 'active'; Preserved = $false }
)) {
$archivePath = Join-Path $retentionRoot "$($archiveRun.Id)"
    New-Item -ItemType Directory -Path $archivePath -Force | Out-Null
    [pscustomobject]@{
        schemaVersion = 1
        runId = $archiveRun.Id
        session = "orch-acc-$($archiveRun.Id)"
        projectKey = ''
        workRoot = ''
        ownerPid = 2147483647
        ownerStartedAt = '2000-01-01T00:00:00.0000000Z'
        status = $archiveRun.Status
        pendingUserInteraction = $false
        resourcesPreserved = $archiveRun.Preserved
        keepResourcesRequested = $archiveRun.Preserved
        finishedAt = $archiveNow.AddDays(-$archiveRun.Age).ToString('o')
    } | ConvertTo-Json | Set-Content -LiteralPath (Join-Path $archivePath 'manifest.json') -Encoding UTF8
    if ($archiveRun.Id -in @('00000001', '00000004', '00000005')) { Set-Content -LiteralPath (Join-Path $archivePath 'state_query.py') -Value 'temporary' }
}
$archivePruned = $false
try {
    Prune-RunReportArchive -Root $retentionRoot -KeepCount 2 -RetentionDays 30 -Now $archiveNow
$archivePruned = (-not (Test-Path -LiteralPath (Join-Path $retentionRoot '00000003'))) -and
    (Test-Path -LiteralPath (Join-Path $retentionRoot '00000001')) -and
    (Test-Path -LiteralPath (Join-Path $retentionRoot '00000002')) -and
    (Test-Path -LiteralPath (Join-Path $retentionRoot '00000004')) -and
    (Test-Path -LiteralPath (Join-Path $retentionRoot '00000005')) -and
    (-not (Test-Path -LiteralPath (Join-Path $retentionRoot '00000001\state_query.py'))) -and
    (Test-Path -LiteralPath (Join-Path $retentionRoot '00000004\state_query.py')) -and
    (Test-Path -LiteralPath (Join-Path $retentionRoot '00000005\state_query.py'))
} catch { }
Check 'archive pruning keeps the newest completed reports and never prunes preserved runs' $archivePruned 'archive retention'

$script:RunArchiveRoot = Join-Path $script:HarnessRoot 'run-archive'
$script:RunRecordDir = Join-Path $script:RunArchiveRoot $script:RunId
$script:RunManifestPath = Join-Path $script:RunRecordDir 'manifest.json'
$script:RunLockStream = $null
$script:RunManifest = $null
$script:LifecyclePhase = $null
$script:PendingUserInteraction = $false
$script:OwnerProcessStartedAt = (Get-Process -Id $PID).StartTime.ToUniversalTime().ToString('o')
# Exercise the actual preflight path against a synthetic store and session list.
$savedTemp = $env:TEMP
$savedDb = $script:StateDb
$preflightTempRoot = Join-Path $script:HarnessRoot 'preflight-temp'
New-Item -ItemType Directory -Path $preflightTempRoot -Force | Out-Null
$env:TEMP = $preflightTempRoot
try {
    $script:PreflightServerProcess = $null
    $script:StateDb = Join-Path $script:HarnessRoot 'preflight.sqlite'
    [void](Invoke-Native -File $script:Python -Arguments @($seedPath, $script:StateDb) -TimeoutSeconds 60)
    $preflightRunId = 'fedcba12'
$preflightRoot = Join-Path $env:TEMP "orch-accept-$preflightRunId"
    $preflightRepo = Join-Path $preflightRoot 'repo'
    New-Item -ItemType Directory -Path $preflightRepo -Force | Out-Null
    @{ runId = $preflightRunId; session = "orch-acc-$preflightRunId"; projectKey = $preflightRepo; keepResourcesRequested = $true } |
        ConvertTo-Json | Set-Content -LiteralPath (Join-Path $preflightRoot 'report.json')
    $preflightSeed = @'
import json, sqlite3, sys
con = sqlite3.connect(sys.argv[1])
key = sys.argv[2]
run = sys.argv[3]
con.execute("INSERT INTO projects VALUES (?,?)", (key, json.dumps({"Key": key, "LaunchPaneID": "w8:p8", "RepoWorkspaceID": "w8"})))
con.execute("INSERT INTO tasks VALUES (?,?,?,?,?,?)", ("orch_fedcba12abcd", key, "w8:p9", "w8:p9", 1, json.dumps({"ID": "orch_fedcba12abcd", "WorktreePath": key[:-4] + "worktrees\\repo\\orch_fedcba12abcd", "WorktreeWorkspaceID": "w9", "CurrentWorkspaceID": "w10", "LaunchPaneID": "w8:p9", "PublicPaneID": "w10:p1"})))
con.execute("INSERT INTO inbox VALUES (?,?,?,?,?,?,?,?)", ("preflight-note", key, "orch_fedcba12abcd", "question", "x", "worker question", "pending", ""))
con.execute("INSERT INTO ready_sessions VALUES (?,?,?,?,?,?,?,?,?)", ("w8:p9", "", "C:\\omp\\orch-accept-" + run + "\\session.jsonl", 1, 1, 1, "path", "x", 1))
con.commit()
'@
    $preflightSeedPath = Join-Path $script:HarnessRoot 'preflight_seed.py'
    Set-Content -LiteralPath $preflightSeedPath -Value $preflightSeed
    $seedResult = Invoke-Native -File $script:Python -Arguments @($preflightSeedPath, $script:StateDb, $preflightRepo, $preflightRunId) -TimeoutSeconds 60
    Check 'preflight fixture contains scoped state' ($seedResult.ExitCode -eq 0) $seedResult.Stderr
    $script:PreflightRunId = $preflightRunId
    $script:PreflightSessionForTest = [pscustomobject]@{
        name = "orch-acc-$preflightRunId"; running = $true
        session_dir = (Join-Path (Join-Path $script:HarnessRoot 'sessions') "orch-acc-$preflightRunId")
    }
    $script:PreflightStopTarget = ''
    $script:PreflightStopArguments = @()
    $script:PreflightCloseArguments = @()
    $script:PreflightCloseCalls = New-Object System.Collections.ArrayList
    $script:PreflightRootClosed = $false
    $script:PreflightDetachedWorkspaceClosed = $false
    $script:PreflightCallOrder = @()
    $script:PreflightDeleteCalls = 0
    $script:PreflightUntrackedWorkspaces = $false
    function Get-Sessions {
        $default = [pscustomobject]@{ name = 'default'; session_dir = $script:HarnessRoot; running = $true }
        if ($script:PreflightSessionForTest) { return @($default, $script:PreflightSessionForTest) }
        return @($default)
    }
    function Invoke-HerdrForSession {
        param([string]$SessionName, [string[]]$Arguments, [double]$TimeoutSeconds)
        if ($SessionName -cne $script:PreflightSessionForTest.name) { throw "wrong explicit test session target [$SessionName]" }
        if ($Arguments.Count -ge 2 -and $Arguments[0] -ceq 'workspace' -and $Arguments[1] -ceq 'close') {
            $script:PreflightCloseArguments = @($Arguments)
            [void]$script:PreflightCloseCalls.Add(@($Arguments) -join ' ')
            $script:PreflightCallOrder += 'workspace close'
            if ($Arguments.Count -ne 4 -or $Arguments[3] -cne '--group') {
                throw 'workspace close was not scoped to an exact run workspace group'
            }
            if ($Arguments[2] -ceq 'w8') {
                $script:PreflightRootClosed = $true
            } elseif ($Arguments[2] -ceq 'w10') {
                $script:PreflightDetachedWorkspaceClosed = $true
            } else {
                throw "workspace close targeted unowned workspace $($Arguments[2])"
            }
            return [pscustomobject]@{ ExitCode = 0; Stdout = ''; Stderr = '' }
        }
        if ($Arguments.Count -eq 2 -and $Arguments[0] -ceq 'workspace' -and $Arguments[1] -ceq 'list') {
            $script:PreflightCallOrder += 'workspace list'
            if ($script:PreflightUntrackedWorkspaces) {
                $workspacesJson = '[{"workspace_id":"w99","worktree":{"checkout_path":"C:\\unowned"}}]'
            } elseif ($script:PreflightRootClosed -and -not $script:PreflightDetachedWorkspaceClosed) {
                $checkout = Join-Path (Join-Path (Join-Path (Join-Path $env:TEMP "orch-accept-$($script:PreflightRunId)") 'worktrees') 'repo') 'orch_fedcba12abcd'
                $workspacesJson = (@(@{ workspace_id = 'w10' }) | ConvertTo-Json -Compress)
            } else {
                $workspacesJson = '[]'
            }
            return [pscustomobject]@{ ExitCode = 0; Stdout = "{`"result`":{`"workspaces`":$workspacesJson}}"; Stderr = '' }
        }
        if ($Arguments.Count -eq 2 -and $Arguments[0] -ceq 'api' -and $Arguments[1] -ceq 'snapshot') {
            $checkout = Join-Path (Join-Path (Join-Path (Join-Path $env:TEMP "orch-accept-$($script:PreflightRunId)") 'worktrees') 'repo') 'orch_fedcba12abcd'
            $snapshot = @{ panes = @(@{ pane_id = 'w10:p1'; workspace_id = 'w10'; cwd = $checkout }) }
            $response = @{ result = @{ snapshot = $snapshot } } | ConvertTo-Json -Depth 6 -Compress
            return [pscustomobject]@{ ExitCode = 0; Stdout = $response; Stderr = '' }
        }
        if ($Arguments.Count -eq 2 -and $Arguments[0] -ceq 'server' -and $Arguments[1] -ceq 'stop') {
            $script:PreflightStopTarget = $SessionName
            $script:PreflightStopArguments = @($Arguments)
            $script:PreflightCallOrder += 'server stop'
            $script:PreflightSessionForTest.running = $false
            return [pscustomobject]@{ ExitCode = 0; Stdout = ''; Stderr = '' }
        }
        throw "unexpected scoped Herdr call during synthetic preflight: $($Arguments -join ' ')"
    }
    function Invoke-Herdr {
        param([string[]]$Arguments, [switch]$NoSessionScope, [double]$TimeoutSeconds)
        if ($Arguments[0] -eq 'session' -and $Arguments[1] -eq 'delete') {
            $script:PreflightDeleteCalls++
            $script:PreflightCallOrder += 'session delete'
            if ($script:PreflightSessionForTest -and $Arguments[2] -eq $script:PreflightSessionForTest.name) {
                $script:PreflightSessionForTest = $null
                return [pscustomobject]@{ ExitCode = 0 }
            }
            throw 'unexpected acceptance session delete target during synthetic preflight'
        }
        throw 'unexpected unscoped Herdr call during synthetic preflight'
    }
    $script:RetireStaleAcceptanceRunId = ''
    $liveError = ''
    try { Clear-StaleAcceptanceState } catch { $liveError = $_.Exception.Message }
    Check 'preflight refuses stale acceptance state whose owner identity is unverifiable' `
        ($liveError -match 'cannot be verified' -and $script:PreflightSessionForTest.running -and
         (Test-Path -LiteralPath $preflightRoot) -and @((Get-StateDump).tables.projects | Where-Object { $_.key -eq $preflightRepo }).Count -eq 1) $liveError
    $script:RetireStaleAcceptanceRunId = $preflightRunId
    $unverifiedAuthorizationError = ''
    try { Clear-StaleAcceptanceState } catch { $unverifiedAuthorizationError = $_.Exception.Message }
    Check 'an exact run-id opt-in cannot override unverifiable owner identity' `
        ($unverifiedAuthorizationError -match 'cannot be verified' -and $script:PreflightSessionForTest.running -and
         -not $script:PreflightStopTarget -and (Test-Path -LiteralPath $preflightRoot)) $unverifiedAuthorizationError
    $script:RetireStaleAcceptanceRunId = 'ffffffff'
    $wrongAuthorizationError = ''
    try { Clear-StaleAcceptanceState } catch { $wrongAuthorizationError = $_.Exception.Message }
    Check 'live preflight rejects authorization for a different run id' `
        ($wrongAuthorizationError -match 'RetireStaleAcceptanceRunId' -and $script:PreflightSessionForTest.running -and
         (Test-Path -LiteralPath $preflightRoot)) $wrongAuthorizationError
    $script:PreflightServerProcess = Start-Process -FilePath (Get-Command powershell.exe).Source `
        -ArgumentList @('-NoProfile', '-Command', 'Start-Sleep -Seconds 120') -PassThru -WindowStyle Hidden
    $wrongExecutableManifest = [pscustomobject]@{
        runId = $preflightRunId
        session = "orch-acc-$preflightRunId"
        serverPid = $script:PreflightServerProcess.Id
        serverStartedAt = $script:PreflightServerProcess.StartTime.ToUniversalTime().ToString('o')
        serverExecutable = (Get-Command cmd.exe).Source
    }
    $wrongExecutableRefused = $false
    try { [void](Stop-VerifiedAcceptanceServerProcess -RunId $preflightRunId -Manifest $wrongExecutableManifest -SessionDisposition 'stopped') } catch {
        $wrongExecutableRefused = $true
    }
    Check 'verified PID with a different executable path is never killed' `
        ($wrongExecutableRefused -and -not $script:PreflightServerProcess.HasExited) 'wrong executable path was accepted'
    $preservedManifest = [pscustomobject]@{
        schemaVersion = 2
        runId = $preflightRunId
        session = "orch-acc-$preflightRunId"
        projectKey = $preflightRepo
        workRoot = $preflightRoot
        stateDb = $script:StateDb
        ownerPid = 2147483647
        ownerStartedAt = '2000-01-01T00:00:00.0000000Z'
        sessionDir = $script:PreflightSessionForTest.session_dir
        serverPid = $script:PreflightServerProcess.Id
        serverStartedAt = $script:PreflightServerProcess.StartTime.ToUniversalTime().ToString('o')
        serverExecutable = (Get-Command powershell.exe).Source
        serverStartAttempted = $true
        phase = 'finished'
        status = 'preserved'
        pendingUserInteraction = $true
        resourcesPreserved = $true
        keepResourcesRequested = $true
        createdWorkspaces = @('w8', 'w9', 'w10')
        createdPanes = @('w8:p8', 'w8:p9', 'w10:p1')
    }
    Write-AtomicRunManifest -Manifest $preservedManifest -Path (Join-Path (Join-Path $script:RunArchiveRoot $preflightRunId) 'manifest.json')
    $liveOwnerManifest = $preservedManifest.PSObject.Copy()
    $liveOwnerManifest.ownerPid = $PID
    $liveOwnerManifest.ownerStartedAt = (Get-Process -Id $PID).StartTime.ToUniversalTime().ToString('o')
    $preservedManifestPath = Join-Path (Join-Path $script:RunArchiveRoot $preflightRunId) 'manifest.json'
    Write-AtomicRunManifest -Manifest $liveOwnerManifest -Path $preservedManifestPath
    $script:RetireStaleAcceptanceRunId = $preflightRunId
    $liveOwnerError = ''
    try { Clear-StaleAcceptanceState } catch { $liveOwnerError = $_.Exception.Message }
    Check 'even an exact run-id opt-in never retires a live acceptance owner' `
        ($liveOwnerError -match 'owner process is live' -and $script:PreflightSessionForTest.running -and
         -not $script:PreflightStopTarget -and (Test-Path -LiteralPath $preflightRoot)) $liveOwnerError
    Write-AtomicRunManifest -Manifest $preservedManifest -Path $preservedManifestPath
    $script:RetireStaleAcceptanceRunId = $preflightRunId
    $script:PreflightUntrackedWorkspaces = $true
    $untrackedError = ''
    try { Clear-StaleAcceptanceState } catch { $untrackedError = $_.Exception.Message }
    Check 'retirement refuses to stop while any unexpected workspace remains in the stale session' `
        ($untrackedError -and $script:PreflightSessionForTest.running -and
         -not $script:PreflightStopTarget -and (Test-Path -LiteralPath $preflightRoot) -and
         (@($script:PreflightCallOrder) -join '|') -ceq 'workspace close|workspace list') $untrackedError
    $script:PreflightRootClosed = $false
    $script:PreflightDetachedWorkspaceClosed = $false
    $script:PreflightCloseCalls.Clear()
    $script:PreflightUntrackedWorkspaces = $false
    $script:PreflightStopTarget = ''
    $script:PreflightStopArguments = @()
    $script:PreflightCloseArguments = @()
    $script:PreflightCallOrder = @()
    $interruptedCleanupManifestPath = Join-Path (Join-Path $script:RunArchiveRoot $preflightRunId) 'manifest.json'
    $interruptedCleanupManifest = Get-Content -LiteralPath $interruptedCleanupManifestPath -Raw | ConvertFrom-Json
    $interruptedCleanupManifest.ownerPid = 2147483647
    $interruptedCleanupManifest.ownerStartedAt = '2000-01-01T00:00:00.0000000Z'
    Write-AtomicRunManifest -Manifest $interruptedCleanupManifest -Path $interruptedCleanupManifestPath
    $script:RetireStaleAcceptanceRunId = ''
    $cleanupError = ''
    try { Clear-StaleAcceptanceState } catch { $cleanupError = $_.Exception.Message }
    $afterPreflight = Get-StateDump
    Check 'preflight automatically retires the verified preserved acceptance run and its pending interaction' `
        (-not $cleanupError -and $script:PreflightStopTarget -eq "orch-acc-$preflightRunId" -and
         (@($script:PreflightCloseCalls) -join '|') -ceq 'workspace close w8 --group|workspace close w10 --group' -and
         (@($script:PreflightStopArguments) -join ' ') -ceq 'server stop' -and
         (@($script:PreflightCallOrder) -join '|') -ceq 'workspace close|workspace list|workspace close|workspace list|server stop|session delete' -and
         -not $script:PreflightSessionForTest -and -not (Test-Path -LiteralPath $preflightRoot) -and
         $script:PreflightServerProcess.HasExited -and
         @($afterPreflight.tables.projects).Count -eq 2 -and @($afterPreflight.tables.tasks).Count -eq 3 -and
         @($afterPreflight.tables.inbox).Count -eq 2 -and @($afterPreflight.tables.ready_sessions).Count -eq 2) `
        ("error=$cleanupError serverProcessExited=$($script:PreflightServerProcess.HasExited) closeCalls=$(@($script:PreflightCloseCalls) -join '|') stopTarget=$($script:PreflightStopTarget) stopArgs=$(@($script:PreflightStopArguments) -join ' ') order=$(@($script:PreflightCallOrder) -join '|') projects=$(@($afterPreflight.tables.projects).Count)")
    $script:RetireStaleAcceptanceRunId = ''
    $absentRunId = '11223344'
    $script:RetireStaleAcceptanceRunId = $absentRunId
$absentRoot = Join-Path $env:TEMP "orch-accept-$absentRunId"
    $absentRepo = Join-Path $absentRoot 'repo'
    New-Item -ItemType Directory -Path $absentRepo -Force | Out-Null
    @{ runId = $absentRunId; session = "orch-acc-$absentRunId"; projectKey = $absentRepo; keepResourcesRequested = $false; resourcesPreserved = $false } |
        ConvertTo-Json | Set-Content -LiteralPath (Join-Path $absentRoot 'report.json')
    $absentManifest = [pscustomobject]@{
        schemaVersion = 1
        runId = $absentRunId
        session = "orch-acc-$absentRunId"
        projectKey = $absentRepo
        workRoot = $absentRoot
        stateDb = $script:StateDb
        ownerPid = 2147483647
        ownerStartedAt = '2000-01-01T00:00:00.0000000Z'
        status = 'active'
        pendingUserInteraction = $false
        serverPid = 2147483647
        serverStartedAt = '2000-01-01T00:00:00.0000000Z'
        serverExecutable = (Get-Command herdr).Source
        serverStartAttempted = $true
        resourcesPreserved = $false
        keepResourcesRequested = $false
    }
    Write-AtomicRunManifest -Manifest $absentManifest -Path (Join-Path (Join-Path $script:RunArchiveRoot $absentRunId) 'manifest.json')
    $absentWorktree = Join-Path $absentRoot 'worktrees\repo\orch-fake11223344'
    New-Item -ItemType Directory -Path $absentWorktree -Force | Out-Null
    $lockedFile = Join-Path $absentWorktree 'held-open.txt'
    Set-Content -LiteralPath $lockedFile -Value 'synthetic lock'
    $addAbsentProject = @'
import json, sqlite3, sys
con = sqlite3.connect(sys.argv[1])
key = sys.argv[2]
con.execute("INSERT INTO projects VALUES (?,?)", (key, json.dumps({"Key": key, "LaunchPaneID": "w8:p10"})))
con.commit()
'@
    $addAbsentPath = Join-Path $script:HarnessRoot 'add_absent_project.py'
    Set-Content -LiteralPath $addAbsentPath -Value $addAbsentProject
    $addAbsentResult = Invoke-Native -File $script:Python -Arguments @($addAbsentPath, $script:StateDb, $absentRepo) -TimeoutSeconds 60
    $script:PreflightDeleteCalls = 0
    $heldFile = $null
    $lockedRemovalError = ''
    $afterLockedAttempt = $null
    try {
        $heldFile = [System.IO.File]::Open($lockedFile, [System.IO.FileMode]::Open, [System.IO.FileAccess]::ReadWrite, [System.IO.FileShare]::None)
        try { Clear-StaleAcceptanceState } catch { $lockedRemovalError = $_.Exception.Message }
        $afterLockedAttempt = Get-StateDump
    } finally {
        if ($heldFile) { $heldFile.Dispose() }
    }
    Check 'locked stale root leaves scoped state intact for a safe retry' `
        ($lockedRemovalError -and (Test-Path -LiteralPath $absentRoot) -and
         @($afterLockedAttempt.tables.projects | Where-Object { $_.key -eq $absentRepo }).Count -eq 1) `
        ("error=$lockedRemovalError projects=$(@($afterLockedAttempt.tables.projects).Count)")
    $absentManifestPath = Join-Path (Join-Path $script:RunArchiveRoot $absentRunId) 'manifest.json'
    if (Test-Path -LiteralPath $absentManifestPath) {
        $interruptedCleanup = Get-Content -LiteralPath $absentManifestPath -Raw | ConvertFrom-Json
        $interruptedCleanup.ownerPid = 2147483647
        $interruptedCleanup.ownerStartedAt = '2000-01-01T00:00:00.0000000Z'
        Write-AtomicRunManifest -Manifest $interruptedCleanup -Path $absentManifestPath
    }
    $script:RetireStaleAcceptanceRunId = ''
    $absentCleanupError = ''
    try { Clear-StaleAcceptanceState } catch { $absentCleanupError = $_.Exception.Message }
    $afterAbsentCleanup = Get-StateDump
    Check 'absent-session cleanup retries and deletes only its verified run after the lock is released' `
        (-not $absentCleanupError -and $addAbsentResult.ExitCode -eq 0 -and $script:PreflightDeleteCalls -eq 0 -and
         -not (Test-Path -LiteralPath $absentRoot) -and @($afterAbsentCleanup.tables.projects).Count -eq 2) `
        ("error=$absentCleanupError deleteCalls=$($script:PreflightDeleteCalls) projects=$(@($afterAbsentCleanup.tables.projects).Count)")
    $orphanRunId = '99887766'
$orphanRoot = Join-Path $env:TEMP "orch-accept-$orphanRunId"
    $orphanRepo = Join-Path $orphanRoot 'repo'
    New-Item -ItemType Directory -Path $orphanRepo -Force | Out-Null
    @{ runId = $orphanRunId; session = "orch-acc-$orphanRunId"; projectKey = $orphanRepo; keepResourcesRequested = $true; resourcesPreserved = $true } |
        ConvertTo-Json | Set-Content -LiteralPath (Join-Path $orphanRoot 'report.json')
    $siblingRunId = '88776655'
    $orphanWorktree = Join-Path $orphanRoot 'worktrees\repo\worker'
    New-Item -ItemType Directory -Path $orphanWorktree -Force | Out-Null
    $orphanLockedFile = Join-Path $orphanWorktree 'held-open.txt'
    Set-Content -LiteralPath $orphanLockedFile -Value 'synthetic orphan lock'
$siblingRoot = Join-Path $env:TEMP "orch-accept-$siblingRunId"
    $siblingRepo = Join-Path $siblingRoot 'repo'
    New-Item -ItemType Directory -Path $siblingRepo -Force | Out-Null
    @{ runId = $siblingRunId; session = "orch-acc-$siblingRunId"; projectKey = $siblingRepo; keepResourcesRequested = $true; resourcesPreserved = $true } |
        ConvertTo-Json | Set-Content -LiteralPath (Join-Path $siblingRoot 'report.json')
    function Get-HerdrJson {
        param([string[]]$Arguments, [switch]$NoSessionScope)
        return [pscustomobject]@{ Result = [pscustomobject]@{ snapshot = [pscustomobject]@{} } }
    }
    $manifestOnlyRunId = '66778899'
    $manifestOnlyRunRoot = Join-Path $env:TEMP "orch-accept-$manifestOnlyRunId"
    $manifestOnlyRunRepo = Join-Path $manifestOnlyRunRoot 'repo'
    New-Item -ItemType Directory -Path $manifestOnlyRunRepo -Force | Out-Null
    $script:RunLockStateDb = $script:StateDb
    $manifestOnlyRunManifestSeed = [pscustomobject]@{
        schemaVersion = 2
        runId = $manifestOnlyRunId
        session = "orch-acc-$manifestOnlyRunId"
        projectKey = $manifestOnlyRunRepo
        workRoot = $manifestOnlyRunRoot
        stateDb = $script:StateDb
        ownerPid = 2147483647
        ownerStartedAt = '2000-01-01T00:00:00.0000000Z'
        serverPid = 0
        serverStartedAt = ''
        serverExecutable = (Get-Command herdr).Source
        serverStartAttempted = $false
        sessionDir = ''
        phase = 'temp-root-created'
        status = 'active'
        pendingUserInteraction = $false
        resourcesPreserved = $false
        keepResourcesRequested = $false
    }
Write-AtomicRunManifest -Manifest $manifestOnlyRunManifestSeed `
    -Path (Join-Path (Join-Path $script:RunArchiveRoot $manifestOnlyRunId) 'manifest.json')
    $script:RetireStaleAcceptanceRunId = ''
    [void](Clear-StaleAcceptanceState)
    $manifestOnlyRunManifest = Get-Content -LiteralPath (Join-Path (Join-Path $script:RunArchiveRoot $manifestOnlyRunId) 'manifest.json') -Raw | ConvertFrom-Json
    Check 'preflight discovers and retires an abandoned pre-bind run with no database project row' `
        ((-not (Test-Path -LiteralPath $manifestOnlyRunRoot)) -and (Get-Field $manifestOnlyRunManifest 'status') -ceq 'complete') `
        (Get-Json $manifestOnlyRunManifest)
    $script:RunLockStateDb = ''
    function Get-FocusProjection {
        param([object]$Snapshot)
        return ''
    }
    $script:RetireStaleAcceptanceRunId = ''
    [void](Clear-StaleAcceptanceState)
    Check 'orphan acceptance roots remain untouched without a run-specific authorization' `
        ((Test-Path -LiteralPath $orphanRoot) -and (Test-Path -LiteralPath $siblingRoot)) 'orphan roots were removed without opt-in'
    $script:RetireStaleAcceptanceRunId = $orphanRunId
    $orphanHeldFile = $null
    $orphanLockError = ''
    try {
        $orphanHeldFile = [System.IO.File]::Open($orphanLockedFile, [System.IO.FileMode]::Open, [System.IO.FileAccess]::ReadWrite, [System.IO.FileShare]::None)
        try { Clear-StaleAcceptanceState } catch { $orphanLockError = $_.Exception.Message }
        Check 'locked exact orphan cleanup preserves its report identity for retry' `
            ($orphanLockError -and (Test-Path -LiteralPath $orphanRoot) -and
             (Test-Path -LiteralPath (Join-Path $orphanRoot 'report.json')) -and (Test-Path -LiteralPath $siblingRoot)) $orphanLockError
    } finally {
        if ($orphanHeldFile) { $orphanHeldFile.Dispose() }
    }
    # A tickets row for the orphan's project blocks its cleanup like a task row, and its root
    # and report survive the refusal; the exact retry below is the control that cleans it once
    # the row is gone.
    $orphanTicketScript = @'
import json, sqlite3, sys
con = sqlite3.connect(sys.argv[1])
key, action = sys.argv[2], sys.argv[3]
if action == "add":
    con.execute("INSERT INTO tickets VALUES (?,?,?,?)", ("tkt_orphan0000001", key, 1, json.dumps({"ID": "tkt_orphan0000001", "ProjectKey": key})))
else:
    con.execute("DELETE FROM tickets WHERE id = ?", ("tkt_orphan0000001",))
con.commit()
'@
    $orphanTicketScriptPath = Join-Path $script:HarnessRoot 'orphan_ticket_row.py'
    Set-Content -LiteralPath $orphanTicketScriptPath -Value $orphanTicketScript
    [void](Invoke-Native -File $script:Python -Arguments @($orphanTicketScriptPath, $script:StateDb, $orphanRepo, 'add') -TimeoutSeconds 60)
    $orphanTicketError = ''
    try { Clear-StaleAcceptanceState } catch { $orphanTicketError = $_.Exception.Message }
    Check 'orphan cleanup is refused while a tickets row still references its project' `
        (($orphanTicketError -match 'state rows still reference') -and (Test-Path -LiteralPath (Join-Path $orphanRoot 'report.json')) -and
         (Test-Path -LiteralPath $siblingRoot)) $orphanTicketError
    [void](Invoke-Native -File $script:Python -Arguments @($orphanTicketScriptPath, $script:StateDb, $orphanRepo, 'remove') -TimeoutSeconds 60)
    $orphanCleanupError = ''
    try { Clear-StaleAcceptanceState } catch { $orphanCleanupError = $_.Exception.Message }
    Check 'exact run-id recovery retries and removes only its report-verified orphan root' `
        (-not $orphanCleanupError -and -not (Test-Path -LiteralPath $orphanRoot) -and (Test-Path -LiteralPath $siblingRoot)) $orphanCleanupError
} finally {
    if ($script:PreflightServerProcess -and -not $script:PreflightServerProcess.HasExited) {
        try { $script:PreflightServerProcess.Kill(); [void]$script:PreflightServerProcess.WaitForExit(5000) } catch { }
    }
    if ($script:PreflightServerProcess) { $script:PreflightServerProcess.Dispose() }
    $env:TEMP = $savedTemp
    $script:StateDb = $savedDb
}

# --- OMP model pinning (-Model) ----------------------------------------------
foreach ($accepted in @('openai-codex/gpt-6-luna', 'openai-codex/gpt-6-luna:high', 'nanogpt/openai/gpt-6-luna', 'anthropic/claude-sonnet-5-5:xhigh')) {
    Check "model spec accepted: $accepted" ($accepted -match $script:OmpModelPattern) $accepted
}
foreach ($rejected in @('', 'gpt-6-luna', 'openai-codex/', '/gpt-6-luna', 'openai-codex/gpt-6-luna:turbo', 'openai-codex/gpt 6', 'openai-codex/gpt-6-luna"', "openai-codex/gpt-6-luna`nmodelRoles:", "openai-codex/gpt-6-luna`n")) {
    Check "model spec rejected: [$($rejected -replace "`n", '\n')]" (-not ($rejected -match $script:OmpModelPattern)) $rejected
}
Check 'overlay pins only the default model role, quoted' `
    ((Get-OmpModelOverlayText -Model 'openai-codex/gpt-6-luna') -ceq "modelRoles:`n  default: `"openai-codex/gpt-6-luna`"`n") 'overlay text'
$overlayRefused = $false
try { [void](Get-OmpModelOverlayText -Model "x/y`nmodelRoles: {}") } catch { $overlayRefused = $true }
Check 'overlay refuses a spec that could inject YAML' $overlayRefused 'injection'

$pathSeparator = [string][System.IO.Path]::PathSeparator
$overlayFile = 'C:\run\omp-model-overlay.yml'
Check 'config files: with nothing listed the value is just the overlay' `
    ((Join-OmpConfigFiles -Existing '' -Overlay $overlayFile) -ceq $overlayFile) 'empty list'
Check 'config files: the invoking user list is kept and the overlay goes last so it wins' `
    ((Join-OmpConfigFiles -Existing 'C:\u\one.yml' -Overlay $overlayFile) -ceq ('C:\u\one.yml' + $pathSeparator + $overlayFile)) 'order'
Check 'config files: blanks, repeats and an earlier copy of the overlay collapse to one trailing overlay' `
    ((Join-OmpConfigFiles -Existing ($pathSeparator + $overlayFile + $pathSeparator + 'C:\u\one.yml' + $pathSeparator + 'C:\u\one.yml') -Overlay $overlayFile) -ceq ('C:\u\one.yml' + $pathSeparator + $overlayFile)) 'dedupe'

$modelTranscript = Join-Path $script:HarnessRoot 'model-session.jsonl'
Set-Content -LiteralPath $modelTranscript -Encoding UTF8 -Value @(
    '{"type":"title","v":1,"title":"x"}'
    '{"type":"message","message":{"role":"user","content":[{"type":"text","text":"hi"}],"provider":"anthropic","model":"claude-sonnet-5-5"}}'
    '{"type":"message","message":{"role":"assistant","provider":"openai-codex","model":"gpt-6-luna","content":[]}}'
    '{"type":"message","message":{"role":"assistant","provider":"openai-codex","model":"gpt-6-luna","content":[]}}'
    '{"type":"message","message":{"role":"assistant","provid'
)
$modelsSeen = @(Get-OmpTranscriptModels -Path $modelTranscript)
Check 'transcript models: only distinct assistant provider/model pairs (a user line and a half-written line add nothing)' `
    ($modelsSeen.Count -eq 1 -and $modelsSeen[0] -ceq 'openai-codex/gpt-6-luna') ($modelsSeen -join ',')
Check 'transcript models: a session that ran only on the pinned model passes' `
    (Test-OmpTranscriptModels -Path $modelTranscript -Model 'openai-codex/gpt-6-luna') 'pinned model'
Check 'transcript models: the :thinking suffix is a request, not part of the recorded model id' `
    (Test-OmpTranscriptModels -Path $modelTranscript -Model 'openai-codex/gpt-6-luna:high') 'suffix'
Check 'transcript models: a different pinned model fails' `
    (-not (Test-OmpTranscriptModels -Path $modelTranscript -Model 'anthropic/claude-sonnet-5-5')) 'other model'

$mixedTranscript = Join-Path $script:HarnessRoot 'model-mixed-session.jsonl'
Set-Content -LiteralPath $mixedTranscript -Encoding UTF8 -Value @(
    '{"type":"message","message":{"role":"assistant","provider":"openai-codex","model":"gpt-6-luna","content":[]}}'
    '{"type":"message","message":{"role":"assistant","provider":"anthropic","model":"claude-sonnet-5-5","content":[]}}'
)
Check 'transcript models: one turn on another model (a fallback) fails the whole session' `
    (-not (Test-OmpTranscriptModels -Path $mixedTranscript -Model 'openai-codex/gpt-6-luna')) 'mixed models'

$userOnlyTranscript = Join-Path $script:HarnessRoot 'model-user-only-session.jsonl'
Set-Content -LiteralPath $userOnlyTranscript -Encoding UTF8 -Value '{"type":"message","message":{"role":"user","content":[],"provider":"openai-codex","model":"gpt-6-luna"}}'
Check 'transcript models: a user message naming the model is not an assistant turn' `
    (-not (Test-OmpTranscriptModels -Path $userOnlyTranscript -Model 'openai-codex/gpt-6-luna')) 'user only'
Check 'transcript models: a missing transcript proves nothing' `
    (-not (Test-OmpTranscriptModels -Path (Join-Path $script:HarnessRoot 'no-such-session.jsonl') -Model 'openai-codex/gpt-6-luna')) 'missing file'

$nestedTranscript = Join-Path $script:HarnessRoot 'model-nested-session.jsonl'
Set-Content -LiteralPath $nestedTranscript -Encoding UTF8 -Value '{"type":"message","message":{"role":"assistant","provider":"nanogpt","model":"openai/gpt-6-luna","content":[]}}'
Check 'transcript models: a model id that itself contains a slash matches its provider/model spelling' `
    (Test-OmpTranscriptModels -Path $nestedTranscript -Model 'nanogpt/openai/gpt-6-luna') 'nested id'

# Assert-OmpSessionModel is the runtime gate that stops a pinned run passing without
# proof. Stub only the handshake lookup, record what the gate asserts, then restore
# the real function and the run counters so nothing leaks into other checks.
$realGetReadySession = ${function:Get-ReadySession}
$savedModel = $script:OmpModel
$savedAssertions = $script:Assertions
$savedCounts = @($script:Passed, $script:Failures, $script:Blocked)
$script:Assertions = New-Object System.Collections.ArrayList
$script:StubHandshake = $null
function Get-ReadySession { param([string]$LaunchPaneId) return $script:StubHandshake }
function Get-ModelGateStatus {
    param([string]$Model, $Handshake)
    $script:OmpModel = $Model
    $script:StubHandshake = $Handshake
    $before = $script:Assertions.Count
    Assert-OmpSessionModel -Id 'T.model' -What 'probe' -LaunchPaneId 'w1:p1' *> $null
    return [string]$script:Assertions[$before].status
}
try {
    $pinned = 'openai-codex/gpt-6-luna'
    $idOnly = [pscustomobject]@{ omp_session_id = '01a0-abc'; omp_session_file = '' }
    Check 'model gate: pinned with no readiness handshake fails' ((Get-ModelGateStatus -Model $pinned -Handshake $null) -ceq 'FAIL') 'no handshake'
    Check 'model gate: pinned with an id-only session (no transcript path) fails instead of passing on faith' ((Get-ModelGateStatus -Model $pinned -Handshake $idOnly) -ceq 'FAIL') 'id only'
    Check 'model gate: pinned and every turn on the pinned model passes' `
        ((Get-ModelGateStatus -Model $pinned -Handshake ([pscustomobject]@{ omp_session_file = $modelTranscript })) -ceq 'PASS') 'right model'
    Check 'model gate: pinned but one turn on another model fails' `
        ((Get-ModelGateStatus -Model $pinned -Handshake ([pscustomobject]@{ omp_session_file = $mixedTranscript })) -ceq 'FAIL') 'mixed models'
    Check 'model gate: nothing pinned only records what OMP chose (INFO), even without a transcript path' `
        ((Get-ModelGateStatus -Model '' -Handshake $idOnly) -ceq 'INFO') 'unpinned id only'
    Check 'model gate: nothing pinned records a readable transcript as INFO, not a verdict' `
        ((Get-ModelGateStatus -Model '' -Handshake ([pscustomobject]@{ omp_session_file = $modelTranscript })) -ceq 'INFO') 'unpinned file'
} finally {
    ${function:Get-ReadySession} = $realGetReadySession
    Remove-Item -LiteralPath Function:\Get-ModelGateStatus -ErrorAction SilentlyContinue
    Remove-Variable -Name StubHandshake -Scope Script -ErrorAction SilentlyContinue
    $script:OmpModel = $savedModel
    $script:Assertions = $savedAssertions
    $script:Passed, $script:Failures, $script:Blocked = $savedCounts
}

} finally {
    # One owned directory holds every path this run created, so cleanup is a single
    # removal that cannot touch anything the harness did not create.
    Remove-Item -LiteralPath $script:HarnessRoot -Recurse -Force -ErrorAction SilentlyContinue
    Set-Location -LiteralPath $previousLocation
}


# --- resume availability (N6.2): exactly one of resume/resumeUnavailable -----
Check 'availability: materialized (resume set, no reason) passes' ((Test-ResumeHintAvailability -Hint ([pscustomobject]@{ resume = 'omp --resume=x'; resumeUnavailable = '' })) -eq $true) 'materialized'
Check 'availability: unmaterialized (no resume, reason set) passes' ((Test-ResumeHintAvailability -Hint ([pscustomobject]@{ resume = ''; resumeUnavailable = 'not materialized yet' })) -eq $true) 'unmaterialized'
Check 'availability: both present fails' ((Test-ResumeHintAvailability -Hint ([pscustomobject]@{ resume = 'omp --resume=x'; resumeUnavailable = 'y' })) -eq $false) 'both'
Check 'availability: neither present fails' ((Test-ResumeHintAvailability -Hint ([pscustomobject]@{ resume = ''; resumeUnavailable = '' })) -eq $false) 'neither'
Check 'N6 rule: a string onDisk cannot pass even with a resume offered' ((Test-ResumeHintImplication -Hint ([pscustomobject]@{ onDisk = 'true'; resume = 'omp --resume=x'; resumeUnavailable = '' })) -eq $false) 'string flag + resume'
Check 'N6 rule: a missing onDisk cannot pass even with a resume offered' ((Test-ResumeHintImplication -Hint ([pscustomobject]@{ resume = 'omp --resume=x'; resumeUnavailable = '' })) -eq $false) 'absent flag + resume'
$exitCode = $fails + $projectionFails
Write-Host ''
Write-Host "helper check failures: $exitCode of $checks checks"
exit $exitCode
