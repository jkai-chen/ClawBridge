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
function Assert-True([bool]$Value, [string]$Message) { if (-not $Value) { throw $Message } }
function Expect-Failure([scriptblock]$Body) {
    $failed = $false
    try { & $Body | Out-Null } catch { $failed = $true }
    Assert-True $failed 'expected failure did not occur'
}
function Assert-Cleanup {
    foreach ($path in $script:privateDirectories) { Assert-True (-not (Test-Path -LiteralPath $path)) ('temporary leaked: ' + $path) }
}
try {
    $destination = Join-Path $fixtureRoot 'bin with spaces'
    Install-ClawBridge -InstallDir $destination | Out-Null
    Assert-True ($script:requests.Count -eq 3) 'latest must be discovered exactly once'
    Assert-True ($script:requests[0].EndsWith('/latest')) 'missing latest discovery'
    $binary = Join-Path $destination 'clawbridge.exe'
    Assert-True ((Get-FileHash -LiteralPath $binary).Hash.ToLowerInvariant() -ceq $script:digest) 'installed digest mismatch'
    Assert-Cleanup
    $script:requests.Clear()
    Install-ClawBridge -Version v0.1.0 -InstallDir $destination | Out-Null
    Assert-True ($script:requests.Count -eq 2) 'explicit version must skip discovery'
    foreach ($failure in @('duplicate','missing','malformed','bad-hash','hostile','download-failure','mismatch','empty','write-failure')) {
        $script:mode = $failure
        [IO.File]::WriteAllText($binary, 'old binary')
        Expect-Failure { Install-ClawBridge -Version v0.1.0 -InstallDir $destination }
        Assert-True ([IO.File]::ReadAllText($binary) -ceq 'old binary') ('old binary changed: ' + $failure)
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
    # A locked existing executable must survive atomic replacement failure.
    $locked = [IO.File]::Open($binary, [IO.FileMode]::Open, [IO.FileAccess]::Read, [IO.FileShare]::None)
    try { Expect-Failure { Install-ClawBridge -Version v0.1.0 -InstallDir $destination } } finally { $locked.Dispose() }
    Assert-True ([IO.File]::ReadAllText($binary) -ceq 'old binary') 'locked binary was changed'
    Assert-Cleanup
    $savedLocalAppData = $env:LOCALAPPDATA
    try {
        $env:LOCALAPPDATA = Join-Path $fixtureRoot 'isolated local app data'
        Install-ClawBridge -Version v0.1.0 | Out-Null
        Assert-True ([IO.File]::Exists((Join-Path $env:LOCALAPPDATA 'Programs\ClawBridge\clawbridge.exe'))) 'default per-user destination missing'
        Assert-Cleanup
    } finally { $env:LOCALAPPDATA = $savedLocalAppData }
    # Directory junctions work without developer-mode symlink privileges.
    $junction = Join-Path $fixtureRoot 'junction'
    $null = New-Item -ItemType Junction -Path $junction -Target $destination
    $script:requests.Clear()
    Expect-Failure { Install-ClawBridge -Version v0.1.0 -InstallDir $junction }
    Assert-True ($script:requests.Count -eq 0) 'junction accessed transport'
    [IO.Directory]::Delete($junction)
    $script:requests.Clear()
    [IO.File]::Delete($binary)
    $null = [IO.Directory]::CreateDirectory($binary)
    Expect-Failure { Install-ClawBridge -Version v0.1.0 -InstallDir $destination }
    Assert-True ($script:requests.Count -eq 0) 'directory destination accessed transport'
    Assert-Cleanup
    Write-Output 'PASS: native Windows installer latest/pinning/atomic replacement/preservation/validation/reparse/cleanup fixtures'
} finally {
    if (Test-Path -LiteralPath (Join-Path $fixtureRoot 'junction')) { [IO.Directory]::Delete((Join-Path $fixtureRoot 'junction')) }
    [IO.Directory]::Delete($fixtureRoot, $true)
}
