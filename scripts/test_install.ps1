# Native Windows fixture suite; dot-sources production functions, never downloads.
Set-StrictMode -Version 2.0
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'install.ps1')
if ((Get-ClawBridgePlatform) -cne 'windows_amd64') { throw 'Native x64 fixture expected' }
$fixtureRoot = Join-Path ([IO.Path]::GetTempPath()) ('clawbridge-installer-test-' + [Guid]::NewGuid().ToString('N'))
$null = [IO.Directory]::CreateDirectory($fixtureRoot)
$script:requests = New-Object 'Collections.Generic.List[string]'
$script:mode = 'ok'
$script:payload = [Text.Encoding]::UTF8.GetBytes('A fixture payload, never an executable')
$sha = [Security.Cryptography.SHA256]::Create()
try { $script:digest = ([BitConverter]::ToString($sha.ComputeHash($script:payload))).Replace('-', '').ToLowerInvariant() } finally { $sha.Dispose() }
$script:privateDirectories = New-Object 'Collections.Generic.List[string]'
$originalPrivateDirectory = ${function:New-ClawBridgePrivateDirectory}
function New-ClawBridgePrivateDirectory([string]$Parent) {
    if ($script:mode -eq 'write-failure' -and $Parent -like '*bin with spaces*') { throw 'synthetic staging failure' }
    $created = & $originalPrivateDirectory $Parent
    $script:privateDirectories.Add($created)
    return $created
}
function Invoke-ClawBridgeDownload([Uri]$Uri, [string]$OutFile, [long]$MaxBytes) {
    $url = $Uri.AbsoluteUri
    $script:requests.Add($url)
    $base = 'https://github.com/jkai-chen/ClawBridge/releases'
    if ($url -ceq ($base + '/latest')) {
        [IO.File]::WriteAllText($OutFile, 'latest data')
        if ($script:mode -eq 'bad-latest') { return ($base + '/tag/v01.0.0') }
        return ($base + '/tag/v0.1.0')
    }
    if (-not $url.StartsWith($base + '/download/v0.1.0/', [StringComparison]::Ordinal)) { throw ('unbounded/unpinned URL ' + $url) }
    $asset = 'clawbridge_v0.1.0_windows_amd64.exe'
    if ($url.EndsWith('/SHA256SUMS')) {
        $entry = $script:digest + '  ' + $asset + "`n"
        switch ($script:mode) {
            'duplicate' { $entry += $entry }
            'missing' { $entry = $script:digest + "  other`n" }
            'malformed' { $entry = $script:digest + ' ' + $asset }
            'bad-hash' { $entry = ('Z' * 64) + '  ' + $asset }
            'hostile' { $entry += '$(' + 'throw "must not execute"' + ')' }
            'download-failure' { throw 'synthetic download failure' }
        }
        [IO.File]::WriteAllText($OutFile, $entry, (New-Object Text.UTF8Encoding($false)))
    } else {
        if ($url -cne ($base + '/download/v0.1.0/' + $asset)) { throw 'wrong asset' }
        $bytes = $script:payload
        if ($script:mode -eq 'mismatch') { $bytes = [Text.Encoding]::UTF8.GetBytes('wrong') }
        if ($script:mode -eq 'empty') { $bytes = New-Object byte[] 0 }
        [IO.File]::WriteAllBytes($OutFile, $bytes)
    }
    return $url
}
# PATH writes go to a throwaway HKCU key; the real user Path is never touched.
$testRegistryRoot = 'Software\ClawBridgeTest\' + [Guid]::NewGuid().ToString('N')
$script:registryKey = $testRegistryRoot + '\Environment'
$savedPath = $env:Path
$savedProtocols = [Net.ServicePointManager]::SecurityProtocol
$originalInstall = ${function:Install-ClawBridge}
function Install-ClawBridge([string]$Version = '', [string]$InstallDir = '', [switch]$NoSkill, [switch]$NoModifyPath) {
    & $originalInstall -Version $Version -InstallDir $InstallDir -NoSkill:$NoSkill -NoModifyPath:$NoModifyPath -PathRegistryKey $script:registryKey
}
$script:broadcasts = 0
function Send-ClawBridgeEnvironmentChange { $script:broadcasts++ }
$script:backups = New-Object 'Collections.Generic.List[string]'
$originalBackupPath = ${function:Get-ClawBridgeBackupPath}
function Get-ClawBridgeBackupPath([string]$InstallDir) {
    $path = & $originalBackupPath $InstallDir
    $script:backups.Add($path)
    return $path
}
function Set-TestUserPath([string]$Value, [Microsoft.Win32.RegistryValueKind]$Kind = [Microsoft.Win32.RegistryValueKind]::ExpandString) {
    $key = [Microsoft.Win32.Registry]::CurrentUser.CreateSubKey($script:registryKey)
    try { $key.SetValue('Path', $Value, $Kind) } finally { $key.Dispose() }
}
function Clear-TestUserPath {
    $key = [Microsoft.Win32.Registry]::CurrentUser.CreateSubKey($script:registryKey)
    try { $key.DeleteValue('Path', $false) } finally { $key.Dispose() }
}
function Get-TestUserPath {
    $key = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey($script:registryKey)
    if ($null -eq $key) { return $null }
    try {
        if (-not ($key.GetValueNames() -contains 'Path')) { return $null }
        return [pscustomobject]@{
            Value = $key.GetValue('Path', $null, [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
            Kind = $key.GetValueKind('Path')
        }
    } finally { $key.Dispose() }
}
# Fails the move of the new binary into place, and in 'restore-failure' also
# the move of the previous binary back.
function Move-ClawBridgeFile([string]$Source, [string]$Destination) {
    $staged = $Source -like '*\.clawbridge-install-*'
    $restoring = $Source -like '*clawbridge-previous-*'
    if (($script:mode -eq 'move-in-failure' -and $staged) -or ($script:mode -eq 'restore-failure' -and ($staged -or $restoring))) { throw 'synthetic move failure' }
    [IO.File]::Move($Source, $Destination)
}
function Format-TestUserPath {
    $state = Get-TestUserPath
    if ($null -eq $state) { return '<none>' }
    return [string]$state.Kind + '|' + [string]$state.Value
}
# The installed payload is not an executable, so the installer's skill step is
# wrapped: it records which binary the installer chose, then runs the REAL
# Invoke-ClawBridgeSkillInstall against a fake clawbridge.exe that logs its
# arguments and exits with CB_PROBE_SKILL_EXIT. Built before any test changes
# the environment, since the Go toolchain needs the runner's caches.
$script:fakeSkill = Join-Path $fixtureRoot 'fake-clawbridge.exe'
Push-Location (Split-Path -Parent $PSScriptRoot)
try {
    & go build -o $script:fakeSkill ./integration/release_fixtures/fakeskill
    if ($LASTEXITCODE -ne 0) { throw 'Building the fake skill executable failed' }
} finally { Pop-Location }
$script:skillLog = Join-Path $fixtureRoot 'skill-runs.log'
$savedSkillLog = $env:CB_PROBE_SKILL_LOG
$savedSkillExit = $env:CB_PROBE_SKILL_EXIT
$env:CB_PROBE_SKILL_LOG = $script:skillLog
$env:CB_PROBE_SKILL_EXIT = '0'
function Get-SkillLog {
    if (-not (Test-Path -LiteralPath $script:skillLog)) { return @() }
    return @([IO.File]::ReadAllLines($script:skillLog) | Where-Object { $_ -ne '' })
}
$script:skillRuns = New-Object 'Collections.Generic.List[string]'
$originalSkillInstall = ${function:Invoke-ClawBridgeSkillInstall}
function Invoke-ClawBridgeSkillInstall([string]$Binary) {
    $script:skillRuns.Add($Binary)
    if ($script:mode -eq 'skill-failure') { $env:CB_PROBE_SKILL_EXIT = '3' } else { $env:CB_PROBE_SKILL_EXIT = '0' }
    & $originalSkillInstall $script:fakeSkill
}
# Joins output and warning records by their messages. Out-String would render
# them at the console width and break long paths across lines.
function ConvertTo-RecordText {
    $messages = @(foreach ($record in $input) {
        if ($record -is [Management.Automation.WarningRecord]) { $record.Message } else { [string]$record }
    })
    return ($messages -join "`n")
}
# Failures are collected, not thrown, so one run reports every problem; the
# script fails once at the end with the full list.
$script:failures = New-Object 'Collections.Generic.List[string]'
function Assert-True([bool]$Value, [string]$Message) { if (-not $Value) { $script:failures.Add($Message) } }
function Expect-Failure([scriptblock]$Body) {
    $failed = $false
    try { & $Body | Out-Null } catch { $failed = $true }
    Assert-True $failed 'expected failure did not occur'
}
function Assert-Cleanup {
    foreach ($path in $script:privateDirectories) { Assert-True (-not (Test-Path -LiteralPath $path)) ('temporary leaked: ' + $path) }
    foreach ($path in $script:backups) { Assert-True (-not (Test-Path -LiteralPath $path)) ('backup leaked: ' + $path) }
}
function Reset-TestSession {
    $script:mode = 'ok'
    $env:Path = $savedPath
    [Net.ServicePointManager]::SecurityProtocol = $savedProtocols
    Remove-Item Env:CLAWBRIDGE_TEST_ROOT -ErrorAction SilentlyContinue
    Clear-TestUserPath
}
# Runs one self-contained case from a reset session. An unexpected error ends
# only that case; its failures are prefixed with the case name.
function Invoke-Case([string]$Name, [scriptblock]$Body) {
    Reset-TestSession
    $failuresBefore = $script:failures.Count
    try { & $Body } catch { $script:failures.Add('stopped by an unexpected error: ' + $_.Exception.Message) } finally { Reset-TestSession }
    for ($i = $failuresBefore; $i -lt $script:failures.Count; $i++) { $script:failures[$i] = $Name + ': ' + $script:failures[$i] }
}
try {
    # The real skill step on its own: it runs the binary with exactly
    # "skill install", succeeds on exit 0 and throws with the code otherwise.
    $env:CB_PROBE_SKILL_EXIT = '0'
    & $originalSkillInstall $script:fakeSkill
    $runs = @(Get-SkillLog)
    # The log holds the path the OS reports, which may be the 8.3 form of TEMP, so
    # only the file name and the arguments are compared.
    Assert-True ($runs.Count -eq 1 -and $runs[0].EndsWith("\fake-clawbridge.exe`tskill install", [StringComparison]::OrdinalIgnoreCase)) ('skill step ran with the wrong binary or arguments: ' + ($runs -join ' | '))
    $env:CB_PROBE_SKILL_EXIT = '7'
    $skillError = $null
    try { & $originalSkillInstall $script:fakeSkill } catch { $skillError = $_.Exception.Message }
    Assert-True ($skillError -ceq 'skill install exited 7') ('a failing skill step must throw its exit code, got: ' + [string]$skillError)
    $env:CB_PROBE_SKILL_EXIT = '0'
    [IO.File]::Delete($script:skillLog)
    $destination = Join-Path $fixtureRoot 'bin with spaces'
    Install-ClawBridge -InstallDir $destination | Out-Null
    Assert-True ($script:requests.Count -eq 3) 'latest must be discovered exactly once'
    Assert-True ($script:requests[0].EndsWith('/latest')) 'missing latest discovery'
    $binary = Join-Path $destination 'clawbridge.exe'
    Assert-True ((Get-FileHash -LiteralPath $binary).Hash.ToLowerInvariant() -ceq $script:digest) 'installed digest mismatch'
    Assert-Cleanup
    Assert-True ($script:skillRuns.Count -eq 1 -and $script:skillRuns[0] -ceq $binary) 'the installed binary must install the skill once'
    Assert-True (@(Get-SkillLog).Count -eq 1 -and @(Get-SkillLog)[0].EndsWith("`tskill install")) 'the real skill step must run "skill install" once'
    $userPath = Get-TestUserPath
    Assert-True ($null -ne $userPath -and $userPath.Value -ceq $destination) 'install directory not added to the user Path'
    Assert-True ($userPath.Kind -eq [Microsoft.Win32.RegistryValueKind]::ExpandString) 'a new user Path must be REG_EXPAND_SZ'
    Assert-True ($script:broadcasts -eq 1) 'a Path change must broadcast WM_SETTINGCHANGE once'
    Assert-True (Test-ClawBridgePathContains $env:Path $destination) 'current session Path not updated'
    $script:requests.Clear()
    Install-ClawBridge -Version v0.1.0 -InstallDir $destination -NoSkill | Out-Null
    Assert-True ($script:requests.Count -eq 2) 'explicit version must skip discovery'
    Assert-True ($script:skillRuns.Count -eq 1) '-NoSkill still installed the skill'
    Assert-True ((Get-TestUserPath).Value -ceq $destination) 'a rerun duplicated the user Path entry'
    Assert-True ($script:broadcasts -eq 1) 'an unchanged Path must not broadcast'
    Assert-True (@($env:Path.Split(';') | Where-Object { $_ -eq $destination }).Count -eq 1) 'a rerun duplicated the session Path entry'
    Assert-Cleanup
    $script:mode = 'skill-failure'
    $output = Install-ClawBridge -Version v0.1.0 -InstallDir $destination 3>&1 | ConvertTo-RecordText
    Assert-True ($script:skillRuns.Count -eq 2) 'skill step not attempted'
    Assert-True (@(Get-SkillLog).Count -eq 2) 'the real skill step did not run the failing binary'
    Assert-True ($output.Contains('The agent skill was not installed; run `clawbridge skill install --agent claude-code|codex|all` later.')) ('skill failure must warn with the recovery command, got: ' + $output)
    Assert-True ($output -notlike '*Installed the ClawBridge agent skill*') 'a failed skill step must not claim success'
    Assert-True ((Get-FileHash -LiteralPath $binary).Hash.ToLowerInvariant() -ceq $script:digest) 'skill failure undid the install'
    $script:mode = 'ok'
    $script:skillRuns.Clear()
    foreach ($failure in @('duplicate','missing','malformed','bad-hash','hostile','download-failure','mismatch','empty','write-failure')) {
        $script:mode = $failure
        [IO.File]::WriteAllText($binary, 'old binary')
        Expect-Failure { Install-ClawBridge -Version v0.1.0 -InstallDir $destination }
        Assert-True ([IO.File]::ReadAllText($binary) -ceq 'old binary') ('old binary changed: ' + $failure)
        Assert-True ($script:skillRuns.Count -eq 0) ('a failed install ran a binary: ' + $failure)
        Assert-Cleanup
    }
    $script:mode = 'ok'
    $script:requests.Clear()
    foreach ($invalid in @('v01.0.0','v0.1.0-beta','https://bad','v1.2.3;bad',"v0.1.0`n")) {
        Expect-Failure { Install-ClawBridge -Version $invalid -InstallDir $destination }
    }
    Expect-Failure { Install-ClawBridge -Version v0.1.0 -InstallDir 'relative' }
    Expect-Failure { Install-ClawBridge -Version v0.1.0 -InstallDir (Join-Path $fixtureRoot 'NUL') }
    Assert-True ($script:requests.Count -eq 0) 'invalid input accessed transport'
    $savedNative = $env:PROCESSOR_ARCHITEW6432
    $savedProcess = $env:PROCESSOR_ARCHITECTURE
    try {
        $env:PROCESSOR_ARCHITEW6432 = 'ARM64'
        $env:PROCESSOR_ARCHITECTURE = 'AMD64'
        Expect-Failure { Install-ClawBridge -Version v0.1.0 -InstallDir $destination }
        Assert-True ($script:requests.Count -eq 0) 'unsupported native architecture accessed transport'
        $env:PROCESSOR_ARCHITEW6432 = 'AMD64'
        $env:PROCESSOR_ARCHITECTURE = 'x86'
        Assert-True ((Get-ClawBridgePlatform) -ceq 'windows_amd64') 'WOW64 native x64 detection failed'
    } finally {
        $env:PROCESSOR_ARCHITEW6432 = $savedNative
        $env:PROCESSOR_ARCHITECTURE = $savedProcess
    }
    $script:mode = 'bad-latest'
    Expect-Failure { Install-ClawBridge -InstallDir $destination }
    Assert-True ($script:requests.Count -eq 1) 'bad latest downloaded a payload'
    $script:mode = 'ok'
    $script:requests.Clear()
    # A file held open without sharing cannot even be renamed: it must survive
    # the refused upgrade, with a clear instruction and no success message.
    $lockedOutput = New-Object 'Collections.Generic.List[string]'
    $lockedError = ''
    $locked = [IO.File]::Open($binary, [IO.FileMode]::Open, [IO.FileAccess]::Read, [IO.FileShare]::None)
    try {
        try { Install-ClawBridge -Version v0.1.0 -InstallDir $destination -NoSkill | ForEach-Object { $lockedOutput.Add([string]$_) } } catch { $lockedError = $_.Exception.Message }
    } finally { $locked.Dispose() }
    Assert-True ($lockedError.Contains('Close the agents running `clawbridge mcp`') -and $lockedError.Contains('run the installer again')) ('locked replacement must explain the fix: ' + $lockedError)
    Assert-True (@($lockedOutput | Where-Object { $_ -like 'Installed*' }).Count -eq 0) 'locked replacement claimed success'
    Assert-True ([IO.File]::ReadAllText($binary) -ceq 'old binary') 'locked binary was changed'
    Assert-Cleanup
    # If the new binary cannot move in, the previous one is moved back.
    $script:backups.Clear()
    [IO.File]::WriteAllText($binary, 'old binary')
    $script:mode = 'move-in-failure'
    $moveOutput = New-Object 'Collections.Generic.List[string]'
    $moveError = ''
    try { Install-ClawBridge -Version v0.1.0 -InstallDir $destination -NoSkill | ForEach-Object { $moveOutput.Add([string]$_) } } catch { $moveError = $_.Exception.Message }
    $script:mode = 'ok'
    Assert-True ($moveError.Contains('Close the agents running `clawbridge mcp`') -and $moveError.Contains('run the installer again') -and $moveError.Contains('synthetic move failure')) ('failed move-in must explain the fix: ' + $moveError)
    Assert-True (@($moveOutput | Where-Object { $_ -like 'Installed*' }).Count -eq 0) 'failed move-in claimed success'
    Assert-True ([IO.File]::ReadAllText($binary) -ceq 'old binary') 'failed move-in did not restore the previous binary'
    Assert-True ($script:backups.Count -eq 1) 'the previous binary was not moved aside first'
    Assert-Cleanup
    # If moving it back fails too, the error names where the previous binary is.
    $script:backups.Clear()
    $script:mode = 'restore-failure'
    $moveOutput.Clear()
    $moveError = ''
    try { Install-ClawBridge -Version v0.1.0 -InstallDir $destination -NoSkill | ForEach-Object { $moveOutput.Add([string]$_) } } catch { $moveError = $_.Exception.Message }
    $script:mode = 'ok'
    Assert-True ($script:backups.Count -eq 1) 'the previous binary was not moved aside first'
    $stranded = $script:backups[0]
    Assert-True ($moveError.Contains('restore the previous binary, which is now at ' + $stranded + '; move it back to ' + $binary)) ('double failure must name the backup: ' + $moveError)
    Assert-True (@($moveOutput | Where-Object { $_ -like 'Installed*' }).Count -eq 0) 'double failure claimed success'
    Assert-True (-not [IO.File]::Exists($binary) -and [IO.File]::ReadAllText($stranded) -ceq 'old binary') 'the previous binary is not at the named backup'
    [IO.File]::Move($stranded, $binary)
    Assert-Cleanup
    # A running binary can be renamed but not deleted: the upgrade succeeds and
    # leaves its old copy behind without failing.
    $running = $null
    $script:backups.Clear()
    try {
        [IO.File]::Copy((Join-Path $env:WINDIR 'System32\PING.EXE'), $binary, $true)
        $running = Start-Process -FilePath $binary -ArgumentList '-n', '120', '127.0.0.1' -WindowStyle Hidden -PassThru
        Start-Sleep -Milliseconds 500
        Assert-True (-not $running.HasExited) 'fixture process did not stay running'
        $runningOutput = Install-ClawBridge -Version v0.1.0 -InstallDir $destination -NoSkill 3>&1 | ConvertTo-RecordText
        Assert-True ((Get-FileHash -LiteralPath $binary).Hash.ToLowerInvariant() -ceq $script:digest) 'running binary was not replaced'
        Assert-True ($runningOutput -like '*Installed v0.1.0*') 'running upgrade did not report success'
        Assert-True ($runningOutput -like '*still running*') 'running upgrade did not report its leftover'
        Assert-True ($script:backups.Count -eq 1 -and [IO.File]::Exists($script:backups[0])) 'the running copy must stay at its backup path'
    } finally {
        if ($null -ne $running -and -not $running.HasExited) { $running.Kill(); $running.WaitForExit() }
    }
    foreach ($path in $script:backups) { if ([IO.File]::Exists($path)) { [IO.File]::Delete($path) } }
    Assert-Cleanup
    # The cases below are self-contained: each takes its own baselines right
    # before the action it measures, and Invoke-Case restores the session.
    Invoke-Case 'user Path keeps %VARIABLES% and REG_EXPAND_SZ' {
        Set-TestUserPath '%SystemRoot%\tools;C:\Other'
        $broadcastsBefore = $script:broadcasts
        Install-ClawBridge -Version v0.1.0 -InstallDir $destination -NoSkill | Out-Null
        $state = Format-TestUserPath
        Assert-True ($state -ceq ('ExpandString|%SystemRoot%\tools;C:\Other;' + $destination)) ('variables or kind not preserved: ' + $state)
        Assert-True ($script:broadcasts -eq $broadcastsBefore + 1) 'appending did not broadcast once'
        Assert-Cleanup
    }
    Invoke-Case 'user Path keeps REG_SZ and a trailing separator' {
        Set-TestUserPath 'C:\Other;' ([Microsoft.Win32.RegistryValueKind]::String)
        Install-ClawBridge -Version v0.1.0 -InstallDir $destination -NoSkill | Out-Null
        $state = Format-TestUserPath
        Assert-True ($state -ceq ('String|C:\Other;' + $destination)) ('REG_SZ or trailing separator mishandled: ' + $state)
        Assert-Cleanup
    }
    Invoke-Case 'an existing entry in another spelling is not duplicated' {
        $env:CLAWBRIDGE_TEST_ROOT = $fixtureRoot
        foreach ($present in @(
            ('C:\Other;' + $destination.ToUpperInvariant() + '\'),
            ($destination.ToLowerInvariant() + '/;C:\Other'),
            '%CLAWBRIDGE_TEST_ROOT%\bin with spaces\',
            ('"' + $destination + '"'))) {
            Set-TestUserPath $present
            $broadcastsBefore = $script:broadcasts
            Install-ClawBridge -Version v0.1.0 -InstallDir $destination -NoSkill | Out-Null
            $state = Format-TestUserPath
            Assert-True ($state -ceq ('ExpandString|' + $present)) ('duplicate added to ' + $present + ': ' + $state)
            Assert-True ($script:broadcasts -eq $broadcastsBefore) ('an existing entry broadcast: ' + $present)
        }
        Assert-Cleanup
    }
    Invoke-Case 'a directory with ; is never added to PATH' {
        $semicolon = Join-Path $fixtureRoot 'bin;semicolon'
        Set-TestUserPath 'C:\Other'
        foreach ($attempt in 1..2) {
            $sessionBefore = $env:Path
            $broadcastsBefore = $script:broadcasts
            $refused = Install-ClawBridge -Version v0.1.0 -InstallDir $semicolon -NoSkill 3>&1 | ConvertTo-RecordText
            Assert-True ($refused -like '*PATH was not changed*contains ; or "*') ('a ; directory must warn: ' + $refused)
            $state = Format-TestUserPath
            Assert-True ($state -ceq 'ExpandString|C:\Other') ('a ; directory reached the user Path: ' + $state)
            Assert-True ($env:Path -ceq $sessionBefore) 'a ; directory reached the session Path'
            Assert-True ($script:broadcasts -eq $broadcastsBefore) 'a refused directory broadcast'
        }
        Assert-True ([IO.File]::Exists((Join-Path $semicolon 'clawbridge.exe'))) 'a ; directory must still install the binary'
        Assert-Cleanup
    }
    Invoke-Case 'an older clawbridge earlier on PATH is reported and outranked in this session' {
        $older = Join-Path $fixtureRoot 'older install'
        $null = [IO.Directory]::CreateDirectory($older)
        $olderBinary = Join-Path $older 'clawbridge.exe'
        [IO.File]::WriteAllText($olderBinary, 'older binary')
        Set-TestUserPath 'C:\Other'
        $env:Path = $older + ';' + $savedPath
        $shadowOutput = Install-ClawBridge -Version v0.1.0 -InstallDir $destination -NoSkill 3>&1 | ConvertTo-RecordText
        Assert-True ($shadowOutput.Contains($olderBinary + ', not the binary just installed at ' + $binary)) ('shadowing must warn with both paths: ' + $shadowOutput)
        Assert-True ($env:Path -ceq ($destination + ';' + $older + ';' + $savedPath)) ('the session must put the new binary first: ' + $env:Path)
        $resolved = @(Get-Command -Name clawbridge -CommandType Application -ErrorAction SilentlyContinue)
        Assert-True ($resolved.Count -gt 0 -and $resolved[0].Path -ieq $binary) 'this session does not resolve the new binary'
        $state = Format-TestUserPath
        Assert-True ($state -ceq ('ExpandString|C:\Other;' + $destination)) ('the persisted Path must append: ' + $state)
        $sessionBefore = $env:Path
        $rerun = Install-ClawBridge -Version v0.1.0 -InstallDir $destination -NoSkill 3>&1 | ConvertTo-RecordText
        Assert-True (-not $rerun.Contains('not the binary just installed')) 'a resolved install still warned about shadowing'
        Assert-True ($env:Path -ceq $sessionBefore) 'a rerun changed the session Path again'
        Assert-Cleanup
    }
    Invoke-Case '-NoModifyPath changes no PATH' {
        $sessionBefore = $env:Path
        $broadcastsBefore = $script:broadcasts
        $noModifyOutput = Install-ClawBridge -Version v0.1.0 -InstallDir $destination -NoSkill -NoModifyPath | ConvertTo-RecordText
        $state = Format-TestUserPath
        Assert-True ($state -ceq '<none>') ('-NoModifyPath wrote the user Path: ' + $state)
        Assert-True ($env:Path -ceq $sessionBefore) '-NoModifyPath changed the session Path'
        Assert-True ($script:broadcasts -eq $broadcastsBefore) '-NoModifyPath broadcast'
        Assert-True ($noModifyOutput -like '*PATH was not changed*') '-NoModifyPath must print the manual step'
        Assert-Cleanup
    }
    Invoke-Case 'an unexpected user Path value only warns' {
        $key = [Microsoft.Win32.Registry]::CurrentUser.CreateSubKey($script:registryKey)
        try { $key.SetValue('Path', 7, [Microsoft.Win32.RegistryValueKind]::DWord) } finally { $key.Dispose() }
        [IO.File]::WriteAllText($binary, 'old binary')
        $broadcastsBefore = $script:broadcasts
        $warned = Install-ClawBridge -Version v0.1.0 -InstallDir $destination -NoSkill 3>&1 | ConvertTo-RecordText
        Assert-True ($warned -like '*user PATH was not updated*') ('a registry failure must warn: ' + $warned)
        Assert-True ((Get-FileHash -LiteralPath $binary).Hash.ToLowerInvariant() -ceq $script:digest) 'a registry failure undid the install'
        $state = Format-TestUserPath
        Assert-True ($state -ceq 'DWord|7') ('an unexpected Path value was overwritten: ' + $state)
        Assert-True ($script:broadcasts -eq $broadcastsBefore) 'a failed Path update broadcast'
        Assert-Cleanup
    }
    Invoke-Case 'TLS 1.2 is added to the enabled protocols' {
        [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls11
        Install-ClawBridge -Version v0.1.0 -InstallDir $destination -NoSkill -NoModifyPath | Out-Null
        $protocols = [Net.ServicePointManager]::SecurityProtocol
        Assert-True (($protocols -band [Net.SecurityProtocolType]::Tls12) -eq [Net.SecurityProtocolType]::Tls12) ('TLS 1.2 not enabled: ' + $protocols)
        Assert-True (($protocols -band [Net.SecurityProtocolType]::Tls11) -eq [Net.SecurityProtocolType]::Tls11) ('an enabled protocol was removed: ' + $protocols)
        if ([Enum]::GetNames([Net.SecurityProtocolType]) -contains 'Tls13' -and [Environment]::OSVersion.Version.Build -ge 20348) {
            Assert-True (($protocols -band [Net.SecurityProtocolType]::Tls13) -eq [Net.SecurityProtocolType]::Tls13) ('TLS 1.3 not enabled where supported: ' + $protocols)
        }
        Assert-Cleanup
    }
    Invoke-Case 'the default destination is per-user' {
        $savedLocalAppData = $env:LOCALAPPDATA
        try {
            $env:LOCALAPPDATA = Join-Path $fixtureRoot 'isolated local app data'
            Install-ClawBridge -Version v0.1.0 -NoModifyPath | Out-Null
            Assert-True ([IO.File]::Exists((Join-Path $env:LOCALAPPDATA 'Programs\ClawBridge\clawbridge.exe'))) 'default per-user destination missing'
            Assert-Cleanup
        } finally { $env:LOCALAPPDATA = $savedLocalAppData }
    }
    # Directory junctions work without developer-mode symlink privileges.
    Invoke-Case 'a junction destination is refused before transport' {
        $junction = Join-Path $fixtureRoot 'junction'
        $null = New-Item -ItemType Junction -Path $junction -Target $destination
        try {
            $script:requests.Clear()
            Expect-Failure { Install-ClawBridge -Version v0.1.0 -InstallDir $junction }
            Assert-True ($script:requests.Count -eq 0) 'junction accessed transport'
        } finally { [IO.Directory]::Delete($junction) }
        Assert-Cleanup
    }
    Invoke-Case 'a directory at the binary path is refused before transport' {
        $script:requests.Clear()
        [IO.File]::Delete($binary)
        $null = [IO.Directory]::CreateDirectory($binary)
        Expect-Failure { Install-ClawBridge -Version v0.1.0 -InstallDir $destination }
        Assert-True ($script:requests.Count -eq 0) 'directory destination accessed transport'
        Assert-Cleanup
    }
} catch {
    $script:failures.Add('stopped by an unexpected error: ' + $_.Exception.Message)
} finally {
    $env:Path = $savedPath
    Remove-Item Env:CLAWBRIDGE_TEST_ROOT -ErrorAction SilentlyContinue
    $env:CB_PROBE_SKILL_LOG = $savedSkillLog
    $env:CB_PROBE_SKILL_EXIT = $savedSkillExit
    [Net.ServicePointManager]::SecurityProtocol = $savedProtocols
    [Microsoft.Win32.Registry]::CurrentUser.DeleteSubKeyTree($testRegistryRoot, $false)
    $parent = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Software\ClawBridgeTest')
    if ($null -ne $parent) {
        $empty = $parent.SubKeyCount -eq 0 -and $parent.ValueCount -eq 0
        $parent.Dispose()
        if ($empty) { [Microsoft.Win32.Registry]::CurrentUser.DeleteSubKey('Software\ClawBridgeTest', $false) }
    }
    foreach ($path in $script:backups) { if ([IO.File]::Exists($path)) { [IO.File]::Delete($path) } }
    if (Test-Path -LiteralPath (Join-Path $fixtureRoot 'junction')) { [IO.Directory]::Delete((Join-Path $fixtureRoot 'junction')) }
    [IO.Directory]::Delete($fixtureRoot, $true)
}
if ($script:failures.Count -gt 0) {
    throw ([string]$script:failures.Count + ' installer fixture failure(s):' + "`n- " + ($script:failures -join "`n- "))
}
Write-Output 'PASS: native Windows installer latest/pinning/rename replacement/locked refusal/running upgrade/preservation/validation/reparse/cleanup/skill/user PATH/TLS fixtures'
