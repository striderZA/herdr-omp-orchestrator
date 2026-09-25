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

# --- resume-hint implication (T10.1) ----------------------------------------
Check 'hint: unmaterialized with no resume passes' ((Test-ResumeHintImplication -Hint ([pscustomobject]@{ onDisk = $false; resume = '' })) -eq $true) 'false+empty'
Check 'hint: materialized with a resume passes' ((Test-ResumeHintImplication -Hint ([pscustomobject]@{ onDisk = $true; resume = 'omp --resume=x' })) -eq $true) 'true+resume'
Check 'hint: unmaterialized but offering a resume fails' ((Test-ResumeHintImplication -Hint ([pscustomobject]@{ onDisk = $false; resume = 'omp --resume=x' })) -eq $false) 'false+resume'
Check 'hint: materialized with no resume fails' ((Test-ResumeHintImplication -Hint ([pscustomobject]@{ onDisk = $true; resume = '' })) -eq $false) 'true+empty'
Check 'hint: string flag fails' ((Test-ResumeHintImplication -Hint ([pscustomobject]@{ onDisk = 'false'; resume = '' })) -eq $false) 'string'
Check 'hint: absent flag fails' ((Test-ResumeHintImplication -Hint ([pscustomobject]@{ resume = '' })) -eq $false) 'absent'

$exitCode = $fails + $projectionFails
# One owned directory holds every path this run created, so cleanup is one removal
# and cannot touch anything the harness did not create.
Remove-Item -LiteralPath $script:HarnessRoot -Recurse -Force -ErrorAction SilentlyContinue
Set-Location -LiteralPath $previousLocation
Write-Host ''
Write-Host "helper check failures: $exitCode of $checks checks"
exit $exitCode
