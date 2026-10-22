# Independent native Windows fixtures; downloads are bytes in isolated directories.
[CmdletBinding()]
param()
$ErrorActionPreference = 'Stop'
$repository = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..\..'))
. (Join-Path $repository 'scripts\install.ps1')
$nativePlatform = ${function:Get-ClawBridgePlatform}
if ([Environment]::OSVersion.Platform -ne [PlatformID]::Win32NT) { throw 'Native Windows is required for this acceptance probe' }
if ((& $nativePlatform) -ne 'windows_amd64') { throw 'Native Windows x64 is required' }
function Assert-Probe([bool]$Condition, [string]$Message) { if (-not $Condition) { throw $Message } }
$fixture = Join-Path ([IO.Path]::GetTempPath()) ('cb-independent-release-' + [Guid]::NewGuid().ToString('N'))
$oldLocal = $env:LOCALAPPDATA
$oldTemp = $env:TEMP
$oldTmp = $env:TMP
$oldArch = $env:PROCESSOR_ARCHITECTURE
$oldArch64 = $env:PROCESSOR_ARCHITEW6432
$oldPath = $env:PATH
$null = [IO.Directory]::CreateDirectory($fixture)
# The installed payload is a real executable, so the installer's own skill step
# (Invoke-ClawBridgeSkillInstall) runs it. Build it before the environment is
# isolated: the Go toolchain needs the runner's caches.
$fake = Join-Path $fixture 'fake-clawbridge.exe'
Push-Location $repository
try {
    & go build -o $fake ./integration/release_fixtures/fakeskill
    if ($LASTEXITCODE -ne 0) { throw 'Building the fake skill executable failed' }
} finally { Pop-Location }
$skillLog = Join-Path $fixture 'skill-runs.log'
$oldSkillLog = $env:CB_PROBE_SKILL_LOG
$oldSkillExit = $env:CB_PROBE_SKILL_EXIT
# The real per-user Path must never change: these cases pass -NoModifyPath, and
# the PATH cases below write only to a throwaway key.
function Get-UserPathState {
    $key = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Environment')
    if ($null -eq $key) { return '<no key>' }
    try {
        if ($key.GetValueNames() -notcontains 'Path') { return '<no value>' }
        return [string]$key.GetValueKind('Path') + '|' + [string]$key.GetValue('Path', '', [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
    } finally { $key.Dispose() }
}
$realUserPath = Get-UserPathState
$testRoot = 'Software\ClawBridgeTest\' + [Guid]::NewGuid().ToString('N')
try {
    $env:LOCALAPPDATA = Join-Path $fixture 'isolated local'
    $env:TEMP = Join-Path $fixture 'temporary'
    $env:TMP = $env:TEMP
    $null = [IO.Directory]::CreateDirectory($env:LOCALAPPDATA)
    $null = [IO.Directory]::CreateDirectory($env:TEMP)
    $directory = Join-Path $fixture "bin space ' quote ; inert"
    $null = [IO.Directory]::CreateDirectory($directory)
    $destination = Join-Path $directory 'clawbridge.exe'
    $sentinel = Join-Path $env:LOCALAPPDATA 'preserve-config'
    [IO.File]::WriteAllText($sentinel, 'UNCHANGED')
    $script:Calls = New-Object Collections.ArrayList
    $script:Mode = 'ok'
    $script:Payload = [IO.File]::ReadAllBytes($fake)
    [IO.File]::Delete($fake)
    $env:CB_PROBE_SKILL_LOG = $skillLog
    $env:CB_PROBE_SKILL_EXIT = '0'
    $hash = [Security.Cryptography.SHA256]::Create()
    try { $script:Digest = ([BitConverter]::ToString($hash.ComputeHash($script:Payload))).Replace('-', '').ToLowerInvariant() } finally { $hash.Dispose() }
    function Get-ClawBridgePlatform { return 'windows_amd64' }
    function Invoke-ClawBridgeDownload([Uri]$Uri, [string]$OutFile, [long]$MaxBytes) {
        $url = $Uri.AbsoluteUri
        $null = $script:Calls.Add($url)
        $base = 'https://github.com/jkai-chen/ClawBridge/releases'
        if ($url -eq ($base + '/latest')) {
            [IO.File]::WriteAllText($OutFile, 'fake release body')
            if ($script:Mode -eq 'bad-latest') { return 'https://attacker.example/tag/v1.2.3' }
            return ($base + '/tag/v1.2.3')
        }
        if (-not $url.StartsWith($base + '/download/v1.2.3/', [StringComparison]::Ordinal)) { throw 'Unpinned download' }
        if ($script:Mode -eq 'download-failure') { throw 'Injected transport failure' }
        if ($url.EndsWith('/SHA256SUMS')) {
            $line = $script:Digest + '  clawbridge_v1.2.3_windows_amd64.exe' + "`n"
            switch ($script:Mode) {
                'duplicate' { $line += $line }
                'malformed' { $line = ('z' * 64) + '  clawbridge_v1.2.3_windows_amd64.exe' + "`n" }
                'absent' { $line = $script:Digest + '  other.exe' + "`n" }
                'hostile' { $line += '$(Set-Content INJECTED unsafe)' + "`n" }
            }
            [IO.File]::WriteAllText($OutFile, $line, [Text.Encoding]::ASCII)
        } else {
            if (-not $url.EndsWith('/clawbridge_v1.2.3_windows_amd64.exe')) { throw 'Wrong target' }
            $bytes = $script:Payload
            if ($script:Mode -eq 'empty') { $bytes = New-Object byte[] 0 }
            if ($script:Mode -eq 'mismatch') { $bytes = [Text.Encoding]::ASCII.GetBytes('WRONG') }
            [IO.File]::WriteAllBytes($OutFile, $bytes)
        }
        return $url
    }
    # The installer's one post-install step is `clawbridge skill install` on the
    # installed binary, run by the production Invoke-ClawBridgeSkillInstall.
    # The installed fake logs each run; the rule under test is exactly one run,
    # of the destination, with exactly those arguments.
    function Get-SkillRuns {
        if (-not [IO.File]::Exists($skillLog)) { return @() }
        return @([IO.File]::ReadAllLines($skillLog) | Where-Object { $_ -ne '' })
    }
    function Clear-SkillRuns { if ([IO.File]::Exists($skillLog)) { [IO.File]::Delete($skillLog) } }
    function Assert-SkillRan {
        $runs = @(Get-SkillRuns)
        Assert-Probe ($runs.Count -eq 1) 'Successful install did not run the skill step exactly once'
        $fields = $runs[0].Split("`t")
        Assert-Probe ($fields.Count -eq 2) 'Malformed skill run record'
        Assert-Probe ([IO.Path]::GetFullPath($fields[0]) -eq [IO.Path]::GetFullPath($destination)) 'Skill step ran something other than the installed binary'
        Assert-Probe ($fields[1] -ceq 'skill install') ('Skill step passed unexpected arguments: ' + $fields[1])
        Clear-SkillRuns
    }
    function Assert-NoSkillRun([string]$Message) { Assert-Probe (@(Get-SkillRuns).Count -eq 0) $Message }
    function Assert-Cleanup {
        Assert-Probe (@(Get-ChildItem -LiteralPath $env:TEMP -Force).Count -eq 0) 'Private temporary directory leaked'
        Assert-Probe (@(Get-ChildItem -LiteralPath $directory -Force).Count -eq 1) 'Destination stage leaked'
        Assert-Probe ([IO.File]::ReadAllText($sentinel) -eq 'UNCHANGED') 'Other config changed'
        Assert-Probe ($env:PATH -ceq $oldPath) 'Installer modified PATH'
        Assert-Probe (-not (Test-Path -LiteralPath (Join-Path $fixture 'INJECTED'))) 'Hostile text executed'
    }
    function Expect-Failure([string]$Version = 'v1.2.3', [string]$Target = $directory) {
        Clear-SkillRuns
        $failed = $false
        try { $null = Install-ClawBridge -NoModifyPath -Version $Version -InstallDir $Target } catch { $failed = $true }
        Assert-Probe $failed 'Expected installer rejection'
        Assert-Probe ([IO.File]::ReadAllText($destination) -eq 'OLD') 'Failure replaced existing binary'
        Assert-NoSkillRun 'Failed install ran the skill step'
        Assert-Cleanup
    }
    [IO.File]::WriteAllText($destination, 'OLD')
    $null = Install-ClawBridge -NoModifyPath -InstallDir $directory
    Assert-Probe ((Get-FileHash -LiteralPath $destination -Algorithm SHA256).Hash.ToLowerInvariant() -eq $script:Digest) 'Latest replacement mismatch'
    Assert-Probe (@($script:Calls | Where-Object { $_ -eq 'https://github.com/jkai-chen/ClawBridge/releases/latest' }).Count -eq 1) 'Latest not discovered exactly once'
    Assert-Probe ($script:Calls.Count -eq 3) 'Unexpected download count'
    Assert-SkillRan
    Assert-Cleanup
    $script:Calls.Clear()
    $null = Install-ClawBridge -NoModifyPath -Version 'v1.2.3' -InstallDir $directory
    Assert-Probe ($script:Calls.Count -eq 2) 'Explicit version performed discovery'
    Assert-SkillRan
    Assert-Cleanup
    $null = Install-ClawBridge -NoModifyPath -Version 'v1.2.3' -InstallDir $directory -NoSkill
    Assert-NoSkillRun '-NoSkill still ran the skill step'
    Assert-Cleanup
    # A failing skill step (the real executable exits 3) only warns: the
    # install still succeeds, and the warning names the recovery command.
    $env:CB_PROBE_SKILL_EXIT = '3'
    [IO.File]::WriteAllText($destination, 'OLD')
    $records = @(Install-ClawBridge -NoModifyPath -Version 'v1.2.3' -InstallDir $directory 3>&1)
    $env:CB_PROBE_SKILL_EXIT = '0'
    $warnings = @($records | Where-Object { $_ -is [Management.Automation.WarningRecord] })
    Assert-Probe ($warnings.Count -eq 1) 'Skill failure did not warn exactly once'
    Assert-Probe ($warnings[0].Message -like '*agent skill was not installed*clawbridge skill install*') ('Skill failure warning lacks the recovery command: ' + $warnings[0].Message)
    Assert-Probe ((Get-FileHash -LiteralPath $destination -Algorithm SHA256).Hash.ToLowerInvariant() -eq $script:Digest) 'Skill failure undid the install'
    Assert-SkillRan
    Assert-Cleanup
    foreach ($mode in @('duplicate', 'malformed', 'absent', 'hostile', 'empty', 'mismatch', 'download-failure')) {
        $script:Mode = $mode
        [IO.File]::WriteAllText($destination, 'OLD')
        Expect-Failure
    }
    $script:Mode = 'ok'
    foreach ($invalid in @('v01.2.3', 'v1.2.3-rc1', 'https://attacker.example', 'v1.2.3;unsafe', "v1.2.3`n")) {
        $script:Calls.Clear()
        Expect-Failure -Version $invalid
        Assert-Probe ($script:Calls.Count -eq 0) 'Invalid version reached network'
    }
    $script:Calls.Clear()
    Expect-Failure -Target 'relative-path'
    Assert-Probe ($script:Calls.Count -eq 0) 'Invalid path reached network'
    # Actual native file-in-use behavior, not a mocked Replace implementation.
    $locked = [IO.File]::Open($destination, [IO.FileMode]::Open, [IO.FileAccess]::Read, [IO.FileShare]::None)
    Clear-SkillRuns
    try {
        $failed = $false
        try { $null = Install-ClawBridge -NoModifyPath -Version 'v1.2.3' -InstallDir $directory } catch { $failed = $true }
        Assert-Probe $failed 'Locked existing executable replacement succeeded'
    } finally { $locked.Dispose() }
    Assert-Probe ([IO.File]::ReadAllText($destination) -eq 'OLD') 'Locked binary changed'
    Assert-NoSkillRun 'Failed locked install ran the skill step'
    Assert-Cleanup
    # Junction refusal exercises native reparse-point handling without symlink privilege.
    $junction = Join-Path $fixture 'junction'
    $null = New-Item -ItemType Junction -Path $junction -Target $directory
    try {
        $script:Calls.Clear()
        Expect-Failure -Target $junction
        Assert-Probe ($script:Calls.Count -eq 0) 'Reparse destination reached network'
    } finally { [IO.Directory]::Delete($junction) }
    # The default PATH step, against a throwaway key standing in for
    # HKCU\Environment, then a fresh shell whose PATH is built the way Windows
    # builds a new terminal's (machine Path, then the expanded user Path).
    $pathKey = $testRoot + '\Environment'
    $pathDir = Join-Path $fixture 'path bin'
    $pathExe = Join-Path $pathDir 'clawbridge.exe'
    $null = [IO.Directory]::CreateDirectory($pathDir)
    $seed = '%SystemRoot%\cb-probe-existing;C:\cb-probe-other'
    # $Value is untyped: a [string] parameter would turn $null into ''.
    function Set-TestPath($Value, [Microsoft.Win32.RegistryValueKind]$Kind) {
        $key = [Microsoft.Win32.Registry]::CurrentUser.CreateSubKey($pathKey)
        try {
            if ($null -eq $Value) { if ($key.GetValueNames() -contains 'Path') { $key.DeleteValue('Path') } }
            else { $key.SetValue('Path', $Value, $Kind) }
        } finally { $key.Dispose() }
    }
    function Get-TestPath {
        $key = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey($pathKey)
        try {
            return @{ Kind = $key.GetValueKind('Path'); Raw = [string]$key.GetValue('Path', '', [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames) }
        } finally { $key.Dispose() }
    }
    function Install-PathCase([string]$Target, [switch]$NoModifyPath) {
        $sessionPath = $env:PATH
        try {
            return @(Install-ClawBridge -Version 'v1.2.3' -InstallDir $Target -NoSkill -NoModifyPath:$NoModifyPath -PathRegistryKey $pathKey)
        } finally { $env:PATH = $sessionPath }
    }
    # Runs `clawbridge path-lookup` in a new shell; returns its exit code. The
    # fake logs the executable that PATH resolution actually started.
    function Invoke-FreshShell {
        $machine = [Environment]::GetEnvironmentVariable('Path', 'Machine')
        $user = [Environment]::ExpandEnvironmentVariables((Get-TestPath).Raw)
        $start = New-Object Diagnostics.ProcessStartInfo
        $start.FileName = (Get-Process -Id $PID).Path
        $start.Arguments = '-NoProfile -NonInteractive -Command "& clawbridge path-lookup; exit $LASTEXITCODE"'
        $start.UseShellExecute = $false
        $start.RedirectStandardOutput = $true
        $start.RedirectStandardError = $true
        $start.EnvironmentVariables['PATH'] = $machine + ';' + $user
        $process = [Diagnostics.Process]::Start($start)
        $null = $process.StandardOutput.ReadToEnd()
        $null = $process.StandardError.ReadToEnd()
        $process.WaitForExit()
        return $process.ExitCode
    }
    Set-TestPath $seed ([Microsoft.Win32.RegistryValueKind]::ExpandString)
    # Control: before the install, a fresh shell finds no clawbridge to run.
    Clear-SkillRuns
    Assert-Probe ((Invoke-FreshShell) -ne 0) 'A clawbridge was already resolvable before the install; the lookup check would prove nothing'
    Assert-NoSkillRun 'The control shell ran a clawbridge'
    $out = Install-PathCase $pathDir
    Assert-Probe (@($out | Where-Object { $_ -like 'Added *to your user PATH*' }).Count -eq 1) 'Install did not report adding the PATH entry'
    $state = Get-TestPath
    Assert-Probe ($state.Kind -eq [Microsoft.Win32.RegistryValueKind]::ExpandString) 'The user Path lost its REG_EXPAND_SZ kind'
    Assert-Probe ($state.Raw -ceq ($seed + ';' + $pathDir)) ('The user Path was not the seed plus the install directory, unexpanded: ' + $state.Raw)
    Assert-Probe ((Invoke-FreshShell) -eq 0) 'A fresh shell could not run clawbridge from the persisted PATH'
    $runs = @(Get-SkillRuns)
    Assert-Probe ($runs.Count -eq 1) 'The fresh shell did not run clawbridge exactly once'
    $fields = $runs[0].Split("`t")
    Assert-Probe ([IO.Path]::GetFullPath($fields[0]) -eq [IO.Path]::GetFullPath($pathExe)) ('The fresh shell resolved another clawbridge: ' + $fields[0])
    Assert-Probe ($fields[1] -ceq 'path-lookup') 'The fresh shell passed unexpected arguments'
    Clear-SkillRuns
    # Idempotent, including a trailing separator and different case.
    $out = Install-PathCase ($pathDir.ToUpperInvariant() + '\')
    Assert-Probe ((Get-TestPath).Raw -ceq ($seed + ';' + $pathDir)) 'A reinstall duplicated the PATH entry'
    Assert-Probe (@($out | Where-Object { $_ -like '*already on your user PATH*' }).Count -eq 1) 'A reinstall did not report the existing PATH entry'
    # -NoModifyPath leaves the value byte-identical.
    $other = Join-Path $fixture 'other bin'
    $null = [IO.Directory]::CreateDirectory($other)
    $null = Install-PathCase $other -NoModifyPath
    Assert-Probe ((Get-TestPath).Raw -ceq ($seed + ';' + $pathDir)) '-NoModifyPath changed the user Path'
    # A REG_SZ value stays REG_SZ; an absent value is created as REG_EXPAND_SZ.
    Set-TestPath 'C:\cb-probe-other' ([Microsoft.Win32.RegistryValueKind]::String)
    $null = Install-PathCase $pathDir
    $state = Get-TestPath
    Assert-Probe ($state.Kind -eq [Microsoft.Win32.RegistryValueKind]::String -and $state.Raw -ceq ('C:\cb-probe-other;' + $pathDir)) 'A REG_SZ user Path changed kind or content unexpectedly'
    Set-TestPath $null ([Microsoft.Win32.RegistryValueKind]::String)
    $null = Install-PathCase $pathDir
    $state = Get-TestPath
    Assert-Probe ($state.Kind -eq [Microsoft.Win32.RegistryValueKind]::ExpandString -and $state.Raw -ceq $pathDir) 'An absent user Path was not created as the install directory'
    Assert-NoSkillRun 'A -NoSkill PATH install ran the skill step'
    # Architecture mapping test uses original production function, then restores env.
    $env:PROCESSOR_ARCHITECTURE = 'x86'
    $env:PROCESSOR_ARCHITEW6432 = 'AMD64'
    Assert-Probe ((& $nativePlatform) -eq 'windows_amd64') '32-bit process x64 mapping failed'
    Assert-Probe ((Get-UserPathState) -ceq $realUserPath) 'The real per-user Path changed'
    Write-Output 'PASS independent native Windows installer fixtures; platform mapping, latest pinning, preservation, real skill step once/never/warn-only, locked file and junction checks, persisted user PATH (kind, unexpanded, idempotent, opt-out) found by a fresh shell'
} finally {
    $env:LOCALAPPDATA = $oldLocal
    $env:TEMP = $oldTemp
    $env:TMP = $oldTmp
    $env:PROCESSOR_ARCHITECTURE = $oldArch
    $env:PROCESSOR_ARCHITEW6432 = $oldArch64
    $env:PATH = $oldPath
    $env:CB_PROBE_SKILL_LOG = $oldSkillLog
    $env:CB_PROBE_SKILL_EXIT = $oldSkillExit
    [Microsoft.Win32.Registry]::CurrentUser.DeleteSubKeyTree($testRoot, $false)
    $parent = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Software\ClawBridgeTest')
    if ($null -ne $parent) {
        $empty = $parent.SubKeyCount -eq 0 -and $parent.ValueCount -eq 0
        $parent.Dispose()
        if ($empty) { [Microsoft.Win32.Registry]::CurrentUser.DeleteSubKey('Software\ClawBridgeTest', $false) }
    }
    if (Test-Path -LiteralPath $fixture) { [IO.Directory]::Delete($fixture, $true) }
}
