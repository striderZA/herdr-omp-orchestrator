#requires -Version 5.1
<#
.SYNOPSIS
    Branch tests for install.ps1, run against stub herdr and go executables.

.DESCRIPTION
    Each scenario copies the repository's install.ps1 and herdr-plugin.toml into a
    fresh folder under a directory this run owns, puts stub herdr and go executables
    first on PATH, and runs the copy under Windows PowerShell. The stubs answer from
    a state file and log every call, so each scenario checks the exit status, the
    message the user reads, and which Herdr and Go commands ran, in order. That is
    how a scenario proves where the script stops.

    Nothing here starts Herdr or builds the plugin for real, and nothing reads or
    writes a real Herdr, Go or OMP installation. The PATH the script sees holds only
    the stubs and the Windows shell folders, so a real herdr or go on this machine
    cannot be reached. The repository's own files are never modified, and the run's
    folder is removed on exit, even after a failure.

.EXAMPLE
    powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\tests\install_tests.ps1
    -> prints one "ok" or "FAIL" line per scenario, then
       "install script check failures: 0 of 19 scenarios" and exits 0.
#>
[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$script:HarnessPath = if ($PSCommandPath) { $PSCommandPath } elseif ($MyInvocation.MyCommand.Path) { $MyInvocation.MyCommand.Path } else { '' }
if (-not $script:HarnessPath) { throw 'run this file with -File so its location is known' }
$script:TestsDir = Split-Path -Parent $script:HarnessPath
$script:PluginRoot = Split-Path -Parent $script:TestsDir
if (-not (Test-Path -LiteralPath (Join-Path $script:PluginRoot 'install.ps1')) -or
    -not (Test-Path -LiteralPath (Join-Path $script:PluginRoot 'herdr-plugin.toml'))) {
    throw "install.ps1 and herdr-plugin.toml must sit in $($script:PluginRoot)"
}

$script:PowerShellExe = Join-Path $env:SystemRoot 'System32\WindowsPowerShell\v1.0\powershell.exe'
$script:ShellDirs = @(
    (Join-Path $env:SystemRoot 'System32'),
    (Join-Path $env:SystemRoot 'System32\WindowsPowerShell\v1.0')
)

# Everything this run writes lives under one directory it creates and removes, so
# runs never collide and cleanup is one removal, even when a scenario throws.
$script:RunRoot = Join-Path $env:TEMP ('install-tests-' + ([guid]::NewGuid().ToString('N')).Substring(0, 8))
$script:WorkRoot = Join-Path $script:RunRoot 'work'
$script:StatePath = Join-Path $script:WorkRoot 'state.json'
$script:LogPath = Join-Path $script:WorkRoot 'calls.log'
$script:ArgvPath = Join-Path $script:WorkRoot 'argv.log'
$script:FakeRepo = ''

# The stand-ins. herdr.cmd and go.cmd forward to the PowerShell stubs, so the script
# finds them as ordinary commands on PATH.
$script:HerdrShim = @'
@echo off
powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0herdr-stub.ps1" %*
exit /b %ERRORLEVEL%
'@

$script:GoShim = @'
@echo off
powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0go-stub.ps1" %*
exit /b %ERRORLEVEL%
'@

# Herdr answers from STUB_STATE. Each call is appended to STUB_LOG as "herdr <args>"
# and to STUB_ARGV as one bracketed token per argument, so argument boundaries show.
$script:HerdrStub = @'
$ErrorActionPreference = 'Stop'
$argv = @($args)
Add-Content -LiteralPath $env:STUB_LOG -Value ('herdr ' + ($argv -join ' '))
Add-Content -LiteralPath $env:STUB_ARGV -Value (($argv | ForEach-Object { '[' + $_ + ']' }) -join '')
$state = Get-Content -LiteralPath $env:STUB_STATE -Raw | ConvertFrom-Json

function Save-State {
    ($state | ConvertTo-Json -Depth 8) | Set-Content -LiteralPath $env:STUB_STATE -Encoding UTF8
}

function Write-Json {
    param($Value)
    Write-Output (ConvertTo-Json -InputObject $Value -Depth 8 -Compress)
}

if ($argv[0] -eq 'plugin' -and $argv[1] -eq 'list') {
    if ([int]$state.listExit -ne 0) {
        [Console]::Error.WriteLine('stub: herdr is not answering')
        exit [int]$state.listExit
    }
    $plugins = @()
    if ($state.registered) {
        $entry = [ordered]@{
            plugin_id   = 'user.omp-orchestrator'
            plugin_root = [string]$state.root
            enabled     = [bool]$state.enabled
            actions     = @([ordered]@{ id = 'setup' }, [ordered]@{ id = 'bind' }, [ordered]@{ id = 'board' })
        }
        $warnings = @($state.warnings | Where-Object { $null -ne $_ })
        if ($warnings.Count -gt 0) { $entry['warnings'] = $warnings }
        $plugins = @($entry)
    }
    Write-Json ([ordered]@{ id = 'cli:plugin'; result = [ordered]@{ plugins = $plugins } })
    exit 0
}

if ($argv[0] -eq 'plugin' -and $argv[1] -eq 'link') {
    if ([int]$state.linkExit -ne 0) {
        [Console]::Error.WriteLine([string]$state.linkStderr)
        exit [int]$state.linkExit
    }
    if (-not $state.registered) { $state.enabled = $true }
    $state.registered = $true
    $state.root = [string]$argv[2]
    Save-State
    Write-Json ([ordered]@{ id = 'cli:plugin'; result = [ordered]@{ linked = [string]$argv[2] } })
    exit 0
}

if ($argv[0] -eq 'plugin' -and $argv[1] -eq 'enable') {
    if ([int]$state.enableExit -ne 0) { [Console]::Error.WriteLine('enable failed'); exit [int]$state.enableExit }
    $state.enabled = $true
    Save-State
    exit 0
}

if ($argv[0] -eq 'plugin' -and $argv[1] -eq 'action' -and $argv[2] -eq 'invoke') {
    if ([int]$state.invokeExit -ne 0) { [Console]::Error.WriteLine('invoke failed'); exit [int]$state.invokeExit }
    $state.polls = 0
    Save-State
    Write-Json ([ordered]@{ id = 'cli:plugin'; result = [ordered]@{ log = [ordered]@{ log_id = 'plugin-log-901' } } })
    exit 0
}

if ($argv[0] -eq 'plugin' -and $argv[1] -eq 'log') {
    $state.polls = [int]$state.polls + 1
    Save-State
    $status = 'succeeded'
    if ([int]$state.polls -le [int]$state.runningPolls) { $status = 'running' }
    $entry = [ordered]@{
        log_id    = 'plugin-log-901'
        status    = $status
        exit_code = [int]$state.setupExit
        stdout    = [string]$state.setupStdout
        stderr    = ''
    }
    Write-Json ([ordered]@{ id = 'cli:plugin'; result = [ordered]@{ logs = @($entry) } })
    exit 0
}

if ($argv[0] -eq 'integration' -and $argv[1] -eq 'status') {
    $dir = 'C:\Users\test\.omp\agent\extensions'
    Write-Output 'pi: not installed (C:\Users\test\.pi\agent\extensions\herdr-agent-state.ts)'
    switch ([string]$state.omp) {
        'current'  { Write-Output "omp: current (v10) ($dir\herdr-omp-agent-state.ts)" }
        'outdated' { Write-Output "omp: outdated (v9) ($dir\herdr-omp-agent-state.ts)" }
        default    { Write-Output "omp: not installed ($dir\herdr-omp-agent-state.ts)" }
    }
    exit 0
}

if ($argv[0] -eq 'integration' -and $argv[1] -eq 'install') {
    if ([int]$state.ompInstallExit -ne 0) { [Console]::Error.WriteLine('integration install failed'); exit [int]$state.ompInstallExit }
    if (-not $state.ompStaysMissing) { $state.omp = 'current' }
    Save-State
    exit 0
}

[Console]::Error.WriteLine('stub: unexpected herdr call: ' + ($argv -join ' '))
exit 99
'@

# go records the call with CGO_ENABLED, then fails when GO_EXIT is set, or writes a
# placeholder herdr-omp-orchestrator.exe into the working folder.
$script:GoStub = @'
$argv = @($args)
Add-Content -LiteralPath $env:STUB_LOG -Value ('go CGO_ENABLED=[' + $env:CGO_ENABLED + '] ' + ($argv -join ' '))
if ($env:GO_EXIT -and [int]$env:GO_EXIT -ne 0) {
    [Console]::Error.WriteLine([string]$env:GO_STDERR)
    exit [int]$env:GO_EXIT
}
Set-Content -LiteralPath (Join-Path (Get-Location).Path 'herdr-omp-orchestrator.exe') -Value 'placeholder'
exit 0
'@

$script:BaseState = @{
    registered      = $false
    root            = ''
    enabled         = $true
    warnings        = @()
    omp             = 'missing'
    ompInstallExit  = 0
    ompStaysMissing = $false
    listExit        = 0
    linkExit        = 0
    linkStderr      = ''
    enableExit      = 0
    invokeExit      = 0
    runningPolls    = 0
    setupExit       = 0
    setupStdout     = '{"ok":true,"extension":"C:\\Users\\test\\.omp\\agent\\extensions\\herdr-orchestrator.ts","protocol":1}'
    polls           = 0
}

$goBuild = '^go CGO_ENABLED=\[0\] build -o herdr-omp-orchestrator\.exe \./cmd/herdr-omp-orchestrator$'
$invokeSetup = '^herdr plugin action invoke setup --plugin user\.omp-orchestrator$'

# Each scenario names the state it starts from, the result it must produce, and the
# Herdr and go calls it must make (Calls), must not make (NoCalls), or must make in
# an exact argument shape (ExactArgv). {fakeRepo} stands for the scenario's folder.
$scenarios = @(
    @{ Name = 'fresh install runs every step'
       State = @{}
       Exit = 0
       Out = @('Installed\.', 'extension: C:\\Users')
       Calls = @($goBuild, '^herdr integration install omp$', '^herdr plugin link ', $invokeSetup)
       NoCalls = @('plugin unlink', 'plugin enable') },
    @{ Name = 'already linked and installed: no reinstall, no unlink'
       State = @{ registered = $true; root = '{fakeRepo}'; omp = 'current' }
       Exit = 0
       Out = @('Installed\.')
       Calls = @('^herdr plugin link ', '^herdr plugin action invoke setup')
       NoCalls = @('integration install', 'plugin unlink') },
    @{ Name = 'manifest path with the prefix and another case is the same folder'
       State = @{ registered = $true; root = '{fakeRepoVerbatim}'; omp = 'current' }
       Exit = 0
       Out = @('Installed\.')
       NoCalls = @('plugin unlink') },
    @{ Name = 'an outdated integration counts as installed'
       State = @{ omp = 'outdated' }
       Exit = 0
       Out = @('Installed\.')
       NoCalls = @('integration install') },
    @{ Name = 'a link to another folder stops before any change'
       State = @{ registered = $true; root = 'C:\Elsewhere\checkout' }
       Exit = 1
       Out = @('herdr plugin unlink user\.omp-orchestrator')
       NoCalls = @('^go ', 'integration install', 'plugin link', 'action invoke', 'plugin enable') },
    @{ Name = 'a disabled plugin is enabled'
       State = @{ registered = $true; root = '{fakeRepo}'; enabled = $false; omp = 'current' }
       Exit = 0
       Out = @('Installed\.')
       Calls = @('^herdr plugin enable user\.omp-orchestrator$')
       NoCalls = @('plugin unlink') },
    @{ Name = 'manifest warnings stop before setup'
       State = @{ warnings = @('unknown event pane.frobnicated') }
       Exit = 1
       Out = @('manifest warnings')
       NoCalls = @('action invoke') },
    @{ Name = 'a setup refusal names its code'
       State = @{ omp = 'current'; setupExit = 1; setupStdout = '{"ok":false,"error":{"code":"integration_missing","message":"Herdr reports its OMP integration is not installed"}}' }
       Exit = 1
       Out = @('setup refused: integration_missing') },
    @{ Name = 'a setup action still running is awaited'
       State = @{ omp = 'current'; runningPolls = 2 }
       Exit = 0
       Out = @('Installed\.')
       MinCount = @{ Pattern = '^herdr plugin log list'; Min = 3 } },
    @{ Name = 'a build failure stops before Herdr is changed'
       State = @{ omp = 'missing' }
       Vars = @{ GO_EXIT = '1'; GO_STDERR = 'boom: cannot compile' }
       Exit = 1
       Out = @('go build failed', 'boom: cannot compile')
       NoCalls = @('plugin link', 'integration install', 'action invoke') },
    @{ Name = 'a build blocked by a running executable names the task board'
       State = @{ omp = 'current' }
       Vars = @{ GO_EXIT = '1'; GO_STDERR = 'The process cannot access the file because it is being used by another process.' }
       Exit = 1
       Out = @('close the task board pane') },
    @{ Name = 'herdr missing from PATH stops at once'
       Bin = 'no-herdr'
       Exit = 1
       Out = @('herdr was not found on PATH')
       NoCalls = @('^herdr ', '^go ') },
    @{ Name = 'go missing from PATH stops at once'
       Bin = 'no-go'
       Exit = 1
       Out = @('go was not found on PATH')
       NoCalls = @('^herdr ', '^go ') },
    @{ Name = 'a failing herdr plugin list stops before anything is built'
       State = @{ listExit = 1 }
       Exit = 1
       Out = @('herdr plugin list failed')
       NoCalls = @('^go ', 'integration install', 'plugin link', 'action invoke') },
    @{ Name = 'a link failure stops before setup'
       State = @{ omp = 'current'; linkExit = 1; linkStderr = 'plugin requires a newer Herdr' }
       Exit = 1
       Out = @('herdr plugin link failed', 'requires a newer Herdr')
       NoCalls = @('action invoke') },
    @{ Name = 'an integration install failure stops before the link'
       State = @{ omp = 'missing'; ompInstallExit = 1 }
       Exit = 1
       Out = @('herdr integration install omp failed')
       NoCalls = @('plugin link', 'action invoke') },
    @{ Name = 'an integration still missing after install stops'
       State = @{ omp = 'missing'; ompStaysMissing = $true }
       Exit = 1
       Out = @('still does not report')
       NoCalls = @('plugin link', 'action invoke') },
    @{ Name = 'a folder without the manifest stops before Herdr is asked'
       OmitManifest = $true
       Exit = 1
       Out = @('herdr-plugin.toml is not in')
       NoCalls = @('^herdr ', '^go ') },
    @{ Name = 'a repository path with spaces reaches herdr as one argument'
       FolderName = 'fake repo'
       State = @{ omp = 'current' }
       Exit = 0
       Out = @('Installed\.')
       ExactArgv = @('[plugin][link][{fakeRepo}]')
       Calls = @($invokeSetup) }
)

$script:checks = 0
$script:fails = 0
function Check {
    param([string]$Name, [bool]$Ok, [string]$Detail = '')
    $script:checks++
    if ($Ok) { Write-Host "ok   $Name" } else { Write-Host "FAIL $Name :: $Detail" -ForegroundColor Red; $script:fails++ }
}

function Write-StubFile {
    param([string]$Path, [string]$Text)
    $crlf = ($Text -replace '\r?\n', "`r`n") + "`r`n"
    [System.IO.File]::WriteAllText($Path, $crlf, [System.Text.Encoding]::ASCII)
}

function New-Bin {
    param([string]$Folder, [string[]]$Names)
    New-Item -ItemType Directory -Path $Folder -Force | Out-Null
    foreach ($name in $Names) {
        $text = switch ($name) {
            'herdr.cmd'      { $script:HerdrShim }
            'go.cmd'         { $script:GoShim }
            'herdr-stub.ps1' { $script:HerdrStub }
            'go-stub.ps1'    { $script:GoStub }
        }
        Write-StubFile -Path (Join-Path $Folder $name) -Text $text
    }
}

function Reset-Scenario {
    param([string]$FolderName, [bool]$OmitManifest)
    if (Test-Path -LiteralPath $script:WorkRoot) { Remove-Item -LiteralPath $script:WorkRoot -Recurse -Force }
    $script:FakeRepo = Join-Path $script:WorkRoot $FolderName
    New-Item -ItemType Directory -Path $script:FakeRepo -Force | Out-Null
    Copy-Item -LiteralPath (Join-Path $script:PluginRoot 'install.ps1') -Destination $script:FakeRepo
    if (-not $OmitManifest) {
        Copy-Item -LiteralPath (Join-Path $script:PluginRoot 'herdr-plugin.toml') -Destination $script:FakeRepo
    }
    New-Bin -Folder (Join-Path $script:WorkRoot 'bin-full') -Names @('herdr.cmd', 'herdr-stub.ps1', 'go.cmd', 'go-stub.ps1')
    New-Bin -Folder (Join-Path $script:WorkRoot 'bin-no-herdr') -Names @('go.cmd', 'go-stub.ps1')
    New-Bin -Folder (Join-Path $script:WorkRoot 'bin-no-go') -Names @('herdr.cmd', 'herdr-stub.ps1')
}

function Invoke-Install {
    param([string]$BinFolder, [hashtable]$Vars)
    $names = @('PATH', 'STUB_STATE', 'STUB_LOG', 'STUB_ARGV', 'GO_EXIT', 'GO_STDERR')
    $saved = @{}
    foreach ($name in $names) { $saved[$name] = [Environment]::GetEnvironmentVariable($name) }
    try {
        # Native stderr from the child is merged in, so it arrives as ErrorRecord
        # objects below; the file-redirect form wraps each line in error text.
        $ErrorActionPreference = 'Continue'
        [Environment]::SetEnvironmentVariable('PATH', (@($BinFolder) + $script:ShellDirs) -join ';')
        [Environment]::SetEnvironmentVariable('STUB_STATE', $script:StatePath)
        [Environment]::SetEnvironmentVariable('STUB_LOG', $script:LogPath)
        [Environment]::SetEnvironmentVariable('STUB_ARGV', $script:ArgvPath)
        [Environment]::SetEnvironmentVariable('GO_EXIT', [string]$Vars['GO_EXIT'])
        [Environment]::SetEnvironmentVariable('GO_STDERR', [string]$Vars['GO_STDERR'])
        $merged = @(& $script:PowerShellExe -NoProfile -ExecutionPolicy Bypass -File (Join-Path $script:FakeRepo 'install.ps1') 2>&1)
        $code = $LASTEXITCODE
    } finally {
        foreach ($name in $names) { [Environment]::SetEnvironmentVariable($name, $saved[$name]) }
    }
    $output = (@($merged | ForEach-Object {
        if ($_ -is [System.Management.Automation.ErrorRecord]) { [string]$_.Exception.Message } else { [string]$_ }
    })) -join "`n"
    $calls = @()
    if (Test-Path -LiteralPath $script:LogPath) { $calls = @(Get-Content -LiteralPath $script:LogPath) }
    $argvLines = @()
    if (Test-Path -LiteralPath $script:ArgvPath) { $argvLines = @(Get-Content -LiteralPath $script:ArgvPath) }
    return [pscustomobject]@{ Exit = $code; Output = $output; Calls = $calls; Argv = $argvLines }
}

function Get-Problems {
    param($Scenario, $Result, [string[]]$ExactArgv)
    $problems = New-Object System.Collections.ArrayList
    if ($Result.Exit -ne $Scenario['Exit']) { [void]$problems.Add("exit $($Result.Exit), want $($Scenario['Exit'])") }
    foreach ($pattern in @($Scenario['Out'] | Where-Object { $null -ne $_ })) {
        if ($Result.Output -notmatch $pattern) { [void]$problems.Add("output lacks /$pattern/") }
    }
    foreach ($pattern in @($Scenario['Calls'] | Where-Object { $null -ne $_ })) {
        if (@($Result.Calls | Where-Object { $_ -match $pattern }).Count -eq 0) { [void]$problems.Add("no call matches /$pattern/") }
    }
    foreach ($pattern in @($Scenario['NoCalls'] | Where-Object { $null -ne $_ })) {
        $hits = @($Result.Calls | Where-Object { $_ -match $pattern })
        if ($hits.Count -gt 0) { [void]$problems.Add("unexpected call matches /$pattern/: $($hits[0])") }
    }
    foreach ($line in $ExactArgv) {
        if (@($Result.Argv | Where-Object { $_ -ceq $line }).Count -eq 0) { [void]$problems.Add("no call received exactly $line") }
    }
    if ($Scenario['MinCount']) {
        $count = @($Result.Calls | Where-Object { $_ -match $Scenario['MinCount']['Pattern'] }).Count
        if ($count -lt $Scenario['MinCount']['Min']) {
            [void]$problems.Add("matched $count calls to /$($Scenario['MinCount']['Pattern'])/, want at least $($Scenario['MinCount']['Min'])")
        }
    }
    return , $problems.ToArray()
}

function Format-Short {
    param([string]$Text)
    $flat = ([string]$Text).Trim() -replace '\r?\n', ' | '
    if ($flat.Length -gt 1200) { $flat = $flat.Substring(0, 1200) + '...' }
    return $flat
}

try {
    foreach ($scenario in $scenarios) {
        $folderName = 'fake-repo'
        if ($scenario['FolderName']) { $folderName = $scenario['FolderName'] }
        Reset-Scenario -FolderName $folderName -OmitManifest ([bool]$scenario['OmitManifest'])

        $state = @{}
        foreach ($key in $script:BaseState.Keys) { $state[$key] = $script:BaseState[$key] }
        if ($scenario['State']) {
            foreach ($key in $scenario['State'].Keys) { $state[$key] = $scenario['State'][$key] }
        }
        if ($state['root'] -eq '{fakeRepo}') { $state['root'] = $script:FakeRepo }
        if ($state['root'] -eq '{fakeRepoVerbatim}') { $state['root'] = '\\?\' + $script:FakeRepo.ToUpperInvariant() }
        $state | ConvertTo-Json -Depth 6 | Set-Content -LiteralPath $script:StatePath -Encoding UTF8

        $binName = 'full'
        if ($scenario['Bin']) { $binName = $scenario['Bin'] }
        $vars = @{}
        if ($scenario['Vars']) { $vars = $scenario['Vars'] }
        $result = Invoke-Install -BinFolder (Join-Path $script:WorkRoot ('bin-' + $binName)) -Vars $vars

        $exact = @($scenario['ExactArgv'] | Where-Object { $null -ne $_ } | ForEach-Object { $_.Replace('{fakeRepo}', $script:FakeRepo) })
        $problems = Get-Problems -Scenario $scenario -Result $result -ExactArgv $exact
        $detail = $problems -join '; '
        if ($problems.Count -gt 0) {
            $detail += " | output: $(Format-Short -Text $result.Output) | calls: $($result.Calls -join ' | ')"
        }
        Check -Name $scenario['Name'] -Ok ($problems.Count -eq 0) -Detail $detail
    }
} finally {
    Remove-Item -LiteralPath $script:RunRoot -Recurse -Force -ErrorAction SilentlyContinue
}

Write-Host ''
Write-Host "install script check failures: $($script:fails) of $($script:checks) scenarios"
if ($script:fails -gt 0) { exit 1 }
exit 0
