#requires -Version 5.1
<#
.SYNOPSIS
    Installs this checkout as the OMP Orchestrator Herdr plugin on this machine.

.DESCRIPTION
    Checks the environment first, then runs the install steps in order and
    stops at the first one that fails:

      Preflight  Finds herdr and go on PATH and reads the plugin's current Herdr
                 link. If Herdr links the plugin from another folder, it stops
                 here, before anything is built or written.
      [1/4]      Builds herdr-omp-orchestrator.exe in this folder with
                 CGO_ENABLED=0, the build command the plugin manifest declares.
      [2/4]      Checks Herdr's OMP integration. It runs
                 `herdr integration install omp` only when
                 `herdr integration status` reports omp as not installed.
      [3/4]      Links this folder as a Herdr plugin, enables it if it is
                 disabled, and checks that Herdr reports no manifest warnings
                 and lists the setup action.
      [4/4]      Runs the plugin's setup action, which writes the OMP extension
                 for the active profile, then reads the answer from Herdr's
                 plugin log.

    Every step is safe to repeat. The script never unlinks or uninstalls the
    plugin. Its writes go through Herdr's own commands and the plugin's setup
    action: the built executable in this folder, the plugin link, Herdr's OMP
    integration only when Herdr reports it missing, and the OMP extension file.
    It installs the checkout that holds this file, whatever folder it is started
    from.

.EXAMPLE
    powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\install.ps1
#>
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

$PluginId = 'user.omp-orchestrator'
$ExecutableName = 'herdr-omp-orchestrator.exe'
$SetupLogTimeoutSeconds = 120
$RepoRoot = $PSScriptRoot
$script:Herdr = $null

function Get-Prop {
    # Reads a property from an object parsed from Herdr's JSON. A missing
    # property reads as $null, so StrictMode does not stop the script.
    param($Object, [string]$Name)
    if ($null -eq $Object) { return $null }
    $property = $Object.PSObject.Properties[$Name]
    if ($null -eq $property) { return $null }
    return $property.Value
}

function Format-Detail {
    # Trims captured output for an error message and keeps it short.
    param([string]$Text)
    $clean = ([string]$Text).Trim()
    if ($clean.Length -gt 800) { $clean = $clean.Substring(0, 800) + '...' }
    return $clean
}

function Invoke-Tool {
    # Runs one executable with an argv array and captures stdout and stderr
    # separately. Nothing is handed to a shell. Native stderr is merged in and
    # arrives as ErrorRecord objects whose Exception.Message is the raw line; a
    # file redirect would wrap each line in PowerShell's own error text instead.
    param([Parameter(Mandatory)][string]$File, [string[]]$Arguments = @())
    # Native stderr is output to report, not a PowerShell error to raise.
    $ErrorActionPreference = 'Continue'
    $merged = @(& $File @Arguments 2>&1)
    $exitCode = $LASTEXITCODE
    $stdout = @($merged | Where-Object { $_ -isnot [System.Management.Automation.ErrorRecord] } | ForEach-Object { [string]$_ })
    $stderr = @($merged | Where-Object { $_ -is [System.Management.Automation.ErrorRecord] } | ForEach-Object { [string]$_.Exception.Message })
    return [pscustomobject]@{
        ExitCode = $exitCode
        Stdout   = ($stdout -join "`n")
        Stderr   = ($stderr -join "`n")
    }
}

function Resolve-Tool {
    param([string]$Name)
    $command = Get-Command -Name $Name -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($null -eq $command) { return $null }
    return [string]$command.Source
}

function ConvertFrom-HerdrJson {
    # Herdr's JSON commands print one object shaped {"id": ..., "result": {...}}.
    # Text that is not JSON is a failure here, never a guess.
    param([string]$Text, [string]$What)
    $parsed = $null
    if (-not [string]::IsNullOrWhiteSpace($Text)) {
        try { $parsed = $Text | ConvertFrom-Json } catch { $parsed = $null }
    }
    if ($null -eq $parsed) { throw "$What printed no JSON answer: $(Format-Detail $Text)" }
    return $parsed
}

function Get-PathKey {
    param([string]$Path)
    $clean = [string]$Path
    if ($clean.StartsWith('\\?\')) { $clean = $clean.Substring(4) }
    return [System.IO.Path]::GetFullPath($clean).TrimEnd('\')
}

function Test-SamePath {
    param([string]$Left, [string]$Right)
    if ([string]::IsNullOrWhiteSpace($Left) -or [string]::IsNullOrWhiteSpace($Right)) { return $false }
    return [string]::Equals((Get-PathKey $Left), (Get-PathKey $Right), [System.StringComparison]::OrdinalIgnoreCase)
}

function Get-Registration {
    # This plugin's entry in `herdr plugin list --json`, or $null when Herdr
    # holds no link for it. Read-only.
    $list = Invoke-Tool -File $script:Herdr -Arguments @('plugin', 'list', '--json')
    if ($list.ExitCode -ne 0) {
        throw "herdr plugin list failed (exit $($list.ExitCode)): $(Format-Detail $list.Stderr)"
    }
    $answer = ConvertFrom-HerdrJson -Text $list.Stdout -What 'herdr plugin list'
    $plugins = @(Get-Prop (Get-Prop $answer 'result') 'plugins' | Where-Object { $null -ne $_ })
    $found = @($plugins | Where-Object { (Get-Prop $_ 'plugin_id') -eq $PluginId })
    if ($found.Count -gt 1) { throw "herdr plugin list shows $PluginId $($found.Count) times" }
    if ($found.Count -eq 0) { return $null }
    return $found[0]
}

function Assert-NoOtherLink {
    # Herdr keeps one link per plugin id. A link to another folder belongs to
    # the user to release, so this script refuses rather than re-pointing it.
    param($Registration)
    if ($null -eq $Registration) { return }
    $linkedFrom = [string](Get-Prop $Registration 'plugin_root')
    if (Test-SamePath $linkedFrom $RepoRoot) { return }
    throw ("Herdr already links {0} from '{1}', not from this folder '{2}'. If that folder is gone or this checkout moved, run 'herdr plugin unlink {0}' and then run this script again." -f $PluginId, $linkedFrom, $RepoRoot)
}

function Get-OmpIntegrationLine {
    # The omp: line of `herdr integration status`, or $null when there is none.
    $status = Invoke-Tool -File $script:Herdr -Arguments @('integration', 'status')
    if ($status.ExitCode -ne 0) {
        throw "herdr integration status failed (exit $($status.ExitCode)): $(Format-Detail $status.Stderr)"
    }
    foreach ($line in ($status.Stdout -split "`n")) {
        $trimmed = $line.Trim()
        if ($trimmed.StartsWith('omp:')) { return $trimmed }
    }
    return $null
}

function Test-OmpIntegrationInstalled {
    # The rule internal/setup applies: an omp: line that does not say "not
    # installed" counts as installed, outdated included. No omp: line proves
    # nothing.
    param([string]$Line)
    return ($null -ne $Line) -and (-not ($Line -match '(?i)not installed'))
}

function Wait-PluginLog {
    # Polls Herdr's plugin log until the action's entry is finished. Herdr
    # records the action's own stdout there, which is the answer setup gave.
    param([string]$LogId)
    $deadline = (Get-Date).AddSeconds($SetupLogTimeoutSeconds)
    while ($true) {
        $list = Invoke-Tool -File $script:Herdr -Arguments @('plugin', 'log', 'list', '--plugin', $PluginId, '--limit', '100')
        if ($list.ExitCode -ne 0) {
            throw "herdr plugin log list failed (exit $($list.ExitCode)): $(Format-Detail $list.Stderr)"
        }
        $answer = ConvertFrom-HerdrJson -Text $list.Stdout -What 'herdr plugin log list'
        $logs = @(Get-Prop (Get-Prop $answer 'result') 'logs' | Where-Object { $null -ne $_ })
        $found = @($logs | Where-Object { (Get-Prop $_ 'log_id') -eq $LogId })
        if ($found.Count -gt 0 -and (Get-Prop ($found[0]) 'status') -ne 'running') {
            return $found[0]
        }
        if ((Get-Date) -gt $deadline) {
            throw "the setup action (log $LogId) did not finish within $SetupLogTimeoutSeconds seconds; see 'herdr plugin log list --plugin $PluginId'."
        }
        Start-Sleep -Seconds 1
    }
}

function Install-Plugin {
    if (-not $RepoRoot) { throw 'run this script as a file (-File), so its folder is known' }
    $script:Herdr = Resolve-Tool -Name 'herdr'
    if (-not $script:Herdr) {
        throw 'herdr was not found on PATH. Install Herdr 0.9.0 or newer, open a new terminal, and run this script again.'
    }
    $go = Resolve-Tool -Name 'go'
    if (-not $go) {
        throw 'go was not found on PATH. Install Go 1.27.1 (the version go.mod names), open a new terminal, and run this script again.'
    }
    if (-not (Test-Path -LiteralPath (Join-Path $RepoRoot 'herdr-plugin.toml'))) {
        throw "herdr-plugin.toml is not in '$RepoRoot'. Run this script from the plugin repository."
    }
    Write-Host "Herdr: $($script:Herdr)"
    Write-Host "Go:    $go"
    Assert-NoOtherLink -Registration (Get-Registration)

    Write-Host "[1/4] Building $ExecutableName (CGO_ENABLED=0, the manifest build command)"
    $previousCgo = $env:CGO_ENABLED
    $env:CGO_ENABLED = '0'
    Push-Location -LiteralPath $RepoRoot
    try {
        $build = Invoke-Tool -File $go -Arguments @('build', '-o', $ExecutableName, './cmd/herdr-omp-orchestrator')
    } finally {
        Pop-Location
        $env:CGO_ENABLED = $previousCgo
    }
    if ($build.ExitCode -ne 0) {
        $hint = ''
        if ($build.Stderr -match '(?i)being used by another process|access is denied') {
            $hint = " $ExecutableName is in use; close the task board pane and run this script again."
        }
        throw "go build failed (exit $($build.ExitCode)): $(Format-Detail $build.Stderr)$hint"
    }
    $exe = Join-Path $RepoRoot $ExecutableName
    if (-not (Test-Path -LiteralPath $exe)) { throw "go build succeeded but $exe does not exist." }
    Write-Host "      built $exe"

    Write-Host "[2/4] Checking Herdr's OMP integration"
    $integration = Get-OmpIntegrationLine
    if (-not (Test-OmpIntegrationInstalled -Line $integration)) {
        Write-Host '      not installed: running herdr integration install omp'
        $install = Invoke-Tool -File $script:Herdr -Arguments @('integration', 'install', 'omp')
        if ($install.ExitCode -ne 0) {
            throw "herdr integration install omp failed (exit $($install.ExitCode)): $(Format-Detail $install.Stderr)"
        }
        $integration = Get-OmpIntegrationLine
        if (-not (Test-OmpIntegrationInstalled -Line $integration)) {
            throw "Herdr still does not report its OMP integration as installed after 'herdr integration install omp' (status line: '$integration')."
        }
    }
    Write-Host "      $integration"

    Write-Host "[3/4] Linking this folder as a Herdr plugin"
    $link = Invoke-Tool -File $script:Herdr -Arguments @('plugin', 'link', $RepoRoot)
    if ($link.ExitCode -ne 0) {
        throw "herdr plugin link failed (exit $($link.ExitCode)): $(Format-Detail $link.Stderr)"
    }
    $registration = Get-Registration
    if ($null -eq $registration) { throw "Herdr does not list $PluginId after 'herdr plugin link'." }
    Assert-NoOtherLink -Registration $registration
    if (-not [bool](Get-Prop $registration 'enabled')) {
        Write-Host '      disabled: running herdr plugin enable'
        $enable = Invoke-Tool -File $script:Herdr -Arguments @('plugin', 'enable', $PluginId)
        if ($enable.ExitCode -ne 0) {
            throw "herdr plugin enable failed (exit $($enable.ExitCode)): $(Format-Detail $enable.Stderr)"
        }
        $registration = Get-Registration
        if ($null -eq $registration -or -not [bool](Get-Prop $registration 'enabled')) {
            throw "Herdr still reports $PluginId as disabled after 'herdr plugin enable'."
        }
    }
    $warnings = @(Get-Prop $registration 'warnings' | Where-Object { $null -ne $_ })
    if ($warnings.Count -gt 0) {
        throw "Herdr reports manifest warnings for ${PluginId}: $(Format-Detail (ConvertTo-Json -InputObject $warnings -Compress -Depth 5)). A warning means part of herdr-plugin.toml did not load."
    }
    $actions = @(Get-Prop $registration 'actions' | Where-Object { $null -ne $_ })
    if (-not ($actions | Where-Object { (Get-Prop $_ 'id') -eq 'setup' })) {
        throw "Herdr lists no setup action for $PluginId."
    }
    Write-Host "      linked $PluginId from $(Get-Prop $registration 'plugin_root') (enabled, no warnings)"

    Write-Host "[4/4] Running the plugin's setup action (writes the OMP extension for the active profile)"
    $invoke = Invoke-Tool -File $script:Herdr -Arguments @('plugin', 'action', 'invoke', 'setup', '--plugin', $PluginId)
    if ($invoke.ExitCode -ne 0) {
        throw "herdr plugin action invoke setup failed (exit $($invoke.ExitCode)): $(Format-Detail $invoke.Stderr)"
    }
    $invokeAnswer = ConvertFrom-HerdrJson -Text $invoke.Stdout -What 'herdr plugin action invoke'
    $logId = [string](Get-Prop (Get-Prop (Get-Prop $invokeAnswer 'result') 'log') 'log_id')
    if ([string]::IsNullOrWhiteSpace($logId)) {
        throw 'herdr plugin action invoke gave no log id for the setup action.'
    }
    $entry = Wait-PluginLog -LogId $logId
    $stdoutText = [string](Get-Prop $entry 'stdout')
    $answer = $null
    if (-not [string]::IsNullOrWhiteSpace($stdoutText)) {
        try { $answer = $stdoutText | ConvertFrom-Json } catch { $answer = $null }
    }
    $exitCode = Get-Prop $entry 'exit_code'
    if ($null -ne $answer -and $exitCode -eq 0 -and (Get-Prop $answer 'ok') -eq $true) {
        Write-Host "      extension: $(Get-Prop $answer 'extension')"
    } elseif ($null -ne (Get-Prop $answer 'error')) {
        $failure = Get-Prop $answer 'error'
        throw "setup refused: $(Get-Prop $failure 'code'): $(Get-Prop $failure 'message')"
    } else {
        throw ("the setup action exited {0} without a setup answer. stdout: {1}. stderr: {2}" -f $exitCode, (Format-Detail $stdoutText), (Format-Detail ([string](Get-Prop $entry 'stderr'))))
    }

    Write-Host ''
    Write-Host 'Installed. Use the plugin actions in Herdr:'
    Write-Host '  bind   Bind this repository as an orchestrated project (run from a pane in the repository)'
    Write-Host '  board  Open the task board'
}

try {
    Install-Plugin
    exit 0
} catch {
    [Console]::Error.WriteLine("install failed: $($_.Exception.Message)")
    exit 1
}
