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
    $script:Payload = [Text.Encoding]::ASCII.GetBytes('MZ-FAKE-BINARY-NOT-EXECUTABLE')
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
    function Assert-Cleanup {
        Assert-Probe (@(Get-ChildItem -LiteralPath $env:TEMP -Force).Count -eq 0) 'Private temporary directory leaked'
        Assert-Probe (@(Get-ChildItem -LiteralPath $directory -Force).Count -eq 1) 'Destination stage leaked'
        Assert-Probe ([IO.File]::ReadAllText($sentinel) -eq 'UNCHANGED') 'Other config changed'
        Assert-Probe ($env:PATH -ceq $oldPath) 'Installer modified PATH'
        Assert-Probe (-not (Test-Path -LiteralPath (Join-Path $fixture 'INJECTED'))) 'Hostile text executed'
    }
    function Expect-Failure([string]$Version = 'v1.2.3', [string]$Target = $directory) {
        $failed = $false
        try { $null = Install-ClawBridge -Version $Version -InstallDir $Target } catch { $failed = $true }
        Assert-Probe $failed 'Expected installer rejection'
        Assert-Probe ([IO.File]::ReadAllText($destination) -eq 'OLD') 'Failure replaced existing binary'
        Assert-Cleanup
    }
    [IO.File]::WriteAllText($destination, 'OLD')
    $null = Install-ClawBridge -InstallDir $directory
    Assert-Probe ((Get-FileHash -LiteralPath $destination -Algorithm SHA256).Hash.ToLowerInvariant() -eq $script:Digest) 'Latest replacement mismatch'
    Assert-Probe (@($script:Calls | Where-Object { $_ -eq 'https://github.com/jkai-chen/ClawBridge/releases/latest' }).Count -eq 1) 'Latest not discovered exactly once'
    Assert-Probe ($script:Calls.Count -eq 3) 'Unexpected download count'
    Assert-Cleanup
    $script:Calls.Clear()
    $null = Install-ClawBridge -Version 'v1.2.3' -InstallDir $directory
    Assert-Probe ($script:Calls.Count -eq 2) 'Explicit version performed discovery'
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
    try {
        $failed = $false
        try { $null = Install-ClawBridge -Version 'v1.2.3' -InstallDir $directory } catch { $failed = $true }
        Assert-Probe $failed 'Locked existing executable replacement succeeded'
    } finally { $locked.Dispose() }
    Assert-Probe ([IO.File]::ReadAllText($destination) -eq 'OLD') 'Locked binary changed'
    Assert-Cleanup
    # Junction refusal exercises native reparse-point handling without symlink privilege.
    $junction = Join-Path $fixture 'junction'
    $null = New-Item -ItemType Junction -Path $junction -Target $directory
    try {
        $script:Calls.Clear()
        Expect-Failure -Target $junction
        Assert-Probe ($script:Calls.Count -eq 0) 'Reparse destination reached network'
    } finally { [IO.Directory]::Delete($junction) }
    # Architecture mapping test uses original production function, then restores env.
    $env:PROCESSOR_ARCHITECTURE = 'x86'
    $env:PROCESSOR_ARCHITEW6432 = 'AMD64'
    Assert-Probe ((& $nativePlatform) -eq 'windows_amd64') '32-bit process x64 mapping failed'
    Write-Output 'PASS independent native Windows installer fixtures; platform mapping, latest pinning, preservation, locked file and junction checks'
} finally {
    $env:LOCALAPPDATA = $oldLocal
    $env:TEMP = $oldTemp
    $env:TMP = $oldTmp
    $env:PROCESSOR_ARCHITECTURE = $oldArch
    $env:PROCESSOR_ARCHITEW6432 = $oldArch64
    $env:PATH = $oldPath
    if (Test-Path -LiteralPath $fixture) { [IO.Directory]::Delete($fixture, $true) }
}
