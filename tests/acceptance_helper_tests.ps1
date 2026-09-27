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

# --- paths ------------------------------------------------------------------
Check 'Test-SamePath mixed slashes' (Test-SamePath -A 'C:/Users/Jaco/x' -B 'C:\Users\Jaco\x') 'mixed'
Check 'Test-SamePath case/backslash' (Test-SamePath -A 'C:\Users\JACO\x\' -B 'c:/users/jaco/x') 'case'
Check 'Test-SamePath rejects sibling' (-not (Test-SamePath -A 'C:\a\b' -B 'C:\a\bc')) 'sibling'
Check 'Test-Below child' (Test-Below -Root 'C:\a\b' -Path 'C:\a\b\c\d') 'child'
Check 'Test-Below rejects prefix sibling' (-not (Test-Below -Root 'C:\a\b' -Path 'C:\a\bc')) 'prefix'
Check 'ConvertTo-NormalPath trims trailing' ((ConvertTo-NormalPath 'C:/x/y/') -eq 'C:\x\y') (ConvertTo-NormalPath 'C:/x/y/')

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
""")
key = "C:\\temp\\run\\repo"
cur.execute("INSERT INTO projects VALUES (?,?)", (key, json.dumps({"Key": key, "AgentName": "orch_p_x", "OMPSessionID": "sess-1"})))
cur.execute("INSERT INTO tasks VALUES (?,?,?,?,?,?)", ("orch_aaaa11112222", key, "w1:p1", "w1:p1", 3,
  json.dumps({"ID": "orch_aaaa11112222", "ProjectKey": key, "Branch": "orch/aaaa11112222", "Report": {"Kind": "result"}})))
cur.execute("INSERT INTO tasks VALUES (?,?,?,?,?,?)", ("orch_bbbb33334444", key, "w2:p1", "w2:p1", 1,
  json.dumps({"ID": "orch_bbbb33334444", "ProjectKey": key, "Branch": "orch/bbbb33334444"})))
cur.execute("INSERT INTO inbox VALUES (?,?,?,?,?,?,?,?)", ("q1", key, "orch_aaaa11112222", "question", "hi", "worker question", "delivered", "ok"))
cur.execute("INSERT INTO ready_sessions VALUES (?,?,?,?,?,?,?,?,?)", ("w1:p1", "sess-1", "", 0, 1, 1, "path", "C:\\s.jsonl", 1))
# a foreign row that must survive untouched
other = "C:\\other\\project"
cur.execute("INSERT INTO projects VALUES (?,?)", (other, json.dumps({"Key": other})))
cur.execute("INSERT INTO tasks VALUES (?,?,?,?,?,?)", ("orch_cccc55556666", other, None, "w9:p1", 1,
  json.dumps({"ID": "orch_cccc55556666", "ProjectKey": other})))
cur.execute("INSERT INTO inbox VALUES (?,?,?,?,?,?,?,?)", ("q9", other, "orch_cccc55556666", "progress", "x", "worker progress", "pending", ""))
cur.execute("INSERT INTO ready_sessions VALUES (?,?,?,?,?,?,?,?,?)", ("w9:p1", "sess-9", "", 0, 1, 1, "id", "sess-9", 1))
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

$plan = Invoke-State -Operation 'plan-scoped' -Arguments @('C:\temp\run\repo', 'w1:p1,w2:p1')
Check 'plan counts ours' ((Get-Field $plan 'counts').tasks -eq 2) (Get-Json $plan)
Check 'plan sees no foreign reference' (@($plan.foreign).Count -eq 0) (Get-Json $plan.foreign)

$del = Invoke-State -Operation 'delete-scoped' -Arguments @('C:\temp\run\repo', 'w1:p1,w2:p1')
Check 'delete reports ok' ((Get-Flag $del 'ok')) (Get-Json $del)
Check 'delete removed exactly our rows' (((Get-Field $del 'deleted').tasks -eq 2) -and ((Get-Field $del 'deleted').projects -eq 1) -and ((Get-Field $del 'deleted').inbox -eq 1) -and ((Get-Field $del 'deleted').ready_sessions -eq 1)) (Get-Json $del)
Check 'delete left other rows identical' ((Get-Flag $del 'others_identical')) (Get-Json $del)
Check 'delete left nothing of ours' ((Get-Field $del 'ours_remaining') -eq 0) (Get-Json $del)
$after = Get-StateDump
Check 'foreign rows survive' ((@($after.tables.projects).Count -eq 1) -and (@($after.tables.tasks).Count -eq 1) -and (@($after.tables.ready_sessions).Count -eq 1)) `
    ("p=$(@($after.tables.projects).Count) t=$(@($after.tables.tasks).Count) r=$(@($after.tables.ready_sessions).Count)")

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
    Tasks = @()
    Inbox = @([pscustomobject]@{ id = 'i1'; kind = 'activity'; delivery_state = 'delivered' })
}
$identityProjection = ''
try { $identityProjection = Get-IdentityProjection -Facts $identityFacts } catch { }
Check 'restart identity projection accepts Facts and excludes mutable inbox rows' ($identityProjection -match 'project/Key=repo' -and $identityProjection -notmatch 'inbox/i1') 'project fields only'
$identityFactsAfter = [pscustomobject]@{
    Project = $identityFacts.Project.PSObject.Copy()
    Tasks = @()
    Inbox = @()
}
$identityFactsAfter.Project.TerminalID = 't2'
$identityProjectionAfter = ''
try { $identityProjectionAfter = Get-IdentityProjection -Facts $identityFactsAfter } catch { }
Check 'restart identity projection ignores the terminal id that Herdr reassigns' ($identityProjection -ceq $identityProjectionAfter) "before=[$identityProjection] after=[$identityProjectionAfter]"
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
Set-Content -LiteralPath $reviewTranscriptPath -Value '{"type":"message","message":{"role":"assistant","content":[{"type":"toolCall","name":"orch_review","arguments":{"taskId":"orch_abc","disposition":"Accepted the reported result."}}]}}' -Encoding UTF8
Check 'review transcript ties confirmation to the exact tool call' (Test-OmpReviewToolCall -Path $reviewTranscriptPath -TaskId 'orch_abc' -Disposition 'Accepted the reported result.') 'task/decision'
Check 'review transcript refuses a different disposition' (-not (Test-OmpReviewToolCall -Path $reviewTranscriptPath -TaskId 'orch_abc' -Disposition 'Rejected the result.')) 'wrong decision'
Check 'id-only restored session proves refusal from its pane' (Test-RestoredReviewRefusal -TaskId 'orch_abc' -Disposition 'RESTORED-123' -Screen 'orch_review RESTORED-123 -> already_decided') 'id-only proof'
Check 'id-only restored session refuses a screen with only the submitted prompt' (-not (Test-RestoredReviewRefusal -TaskId 'orch_abc' -Disposition 'RESTORED-123' -Screen 'orch_review RESTORED-123')) 'no error'
Check 'id-only restored session refuses an unrelated decision' (-not (Test-RestoredReviewRefusal -TaskId 'orch_abc' -Disposition 'RESTORED-123' -Screen 'orch_review OTHER -> already_decided')) 'wrong decision'

$reviewScreen = @'
Earlier confirmation: Record review disposition
Task orch_old; Accepted the reported result.
Record review disposition
Confirm that you reviewed the worker's final report for task orch_abc and chose this disposition:
Accepted the reported result.
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
Check 'review modal rejects a different task' (-not (Test-PrimaryReviewConfirmScreen -Screen $reviewScreen -TaskId 'orch_other' -Revision 7 -Disposition 'Accepted the reported result.')) 'wrong task'
Check 'review modal rejects a different report revision' (-not (Test-PrimaryReviewConfirmScreen -Screen $reviewScreen -TaskId 'orch_abc' -Revision 8 -Disposition 'Accepted the reported result.')) 'wrong revision'
Check 'review modal rejects a different decision' (-not (Test-PrimaryReviewConfirmScreen -Screen $reviewScreen -TaskId 'orch_abc' -Revision 7 -Disposition 'Rejected the result.')) 'wrong disposition'
Check 'review modal rejects a longer different disposition that contains the expected text' (-not (Test-PrimaryReviewConfirmScreen -Screen ($reviewScreen -replace 'Accepted the reported result\.\r?\nStored report', "Accepted the reported result. but request changes`nStored report") -TaskId 'orch_abc' -Revision 7 -Disposition 'Accepted the reported result.')) 'substring decision'
Check 'review modal accepts a stored report that mentions another modal title as data' (Test-PrimaryReviewConfirmScreen -Screen ($reviewScreen -replace 'Added subtract to a.js', 'Ask 1 questions was displayed') -TaskId 'orch_abc' -Revision 7 -Disposition 'Accepted the reported result.') 'report data'
Check 'review modal ignores an old matching modal when another confirmation is active' (-not (Test-PrimaryReviewConfirmScreen -Screen ($reviewScreen + "`nRecord review disposition`nTask orch_other`nYes`nNo") -TaskId 'orch_abc' -Revision 7 -Disposition 'Accepted the reported result.')) 'stale modal'
Check 'review modal refuses an old confirmation followed by an active Ask' (-not (Test-PrimaryReviewConfirmScreen -Screen ($reviewScreen + "`nAsk 1 questions`n[other] What now?`nAccept`nRequest changes") -TaskId 'orch_abc' -Revision 7 -Disposition 'Accepted the reported result.')) 'new Ask'
Check 'review modal refuses an old confirmation followed by a different action modal' (-not (Test-PrimaryReviewConfirmScreen -Screen ($reviewScreen + "`nRemove worker checkout`nTask orch_other`nCancel") -TaskId 'orch_abc' -Revision 7 -Disposition 'Accepted the reported result.')) 'new action'
Check 'review modal refuses unknown active panel after its old choices' (-not (Test-PrimaryReviewConfirmScreen -Screen ($reviewScreen + "`nSomething else is awaiting input") -TaskId 'orch_abc' -Revision 7 -Disposition 'Accepted the reported result.')) 'unknown action'

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
