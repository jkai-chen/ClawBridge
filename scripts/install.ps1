[CmdletBinding()]
param(
    [string]$Version = '',
    [string]$InstallDir = '',
    [switch]$Help
)
Set-StrictMode -Version 2.0
$ErrorActionPreference = 'Stop'

function Test-ClawBridgeVersion([string]$Value) {
    return $Value -cmatch '\Av(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\z'
}

function Get-ClawBridgePlatform {
    if ([Environment]::OSVersion.Platform -ne [PlatformID]::Win32NT) { throw 'Windows x64 is required' }
    $nativeArchitecture = $env:PROCESSOR_ARCHITEW6432
    if ([string]::IsNullOrEmpty($nativeArchitecture)) { $nativeArchitecture = $env:PROCESSOR_ARCHITECTURE }
    if ($nativeArchitecture -ne 'AMD64') { throw 'Only native Windows x64 is supported' }
    return 'windows_amd64'
}

function Assert-ClawBridgePath([string]$Path) {
    # Check existing ancestors too: do not write through junctions or symlinks.
    $part = $Path
    while (-not [string]::IsNullOrEmpty($part)) {
        $item = Get-Item -LiteralPath $part -Force -ErrorAction SilentlyContinue
        if ($null -ne $item -and ($item.Attributes -band [IO.FileAttributes]::ReparsePoint)) {
            throw 'Destination paths must not contain reparse points'
        }
        $parent = [IO.Path]::GetDirectoryName($part)
        if ($parent -eq $part) { break }
        $part = $parent
    }
}

function New-ClawBridgePrivateDirectory([string]$Parent) {
    $path = Join-Path $Parent ('.clawbridge-install-' + [Guid]::NewGuid().ToString('N'))
    if (Test-Path -LiteralPath $path) { throw 'Temporary directory collision' }
    $null = [IO.Directory]::CreateDirectory($path)
    try {
        $identity = [Security.Principal.WindowsIdentity]::GetCurrent().User
        $acl = New-Object Security.AccessControl.DirectorySecurity
        $acl.SetAccessRuleProtection($true, $false)
        $acl.SetOwner($identity)
        $rule = New-Object Security.AccessControl.FileSystemAccessRule($identity, 'FullControl', 'ContainerInherit,ObjectInherit', 'None', 'Allow')
        $acl.AddAccessRule($rule)
        Set-Acl -LiteralPath $path -AclObject $acl
        return $path
    } catch {
        [IO.Directory]::Delete($path, $true)
        throw
    }
}

function Invoke-ClawBridgeDownload([Uri]$Uri, [string]$OutFile, [long]$MaxBytes) {
    # Explicit redirects ensure an HTTPS response cannot downgrade to HTTP.
    $deadline = [Diagnostics.Stopwatch]::StartNew()
    $current = $Uri
    for ($redirect = 0; $redirect -le 5; $redirect++) {
        if ($current.Scheme -cne 'https') { throw 'Only HTTPS downloads are permitted' }
        $remaining = 120000 - [int]$deadline.ElapsedMilliseconds
        if ($remaining -le 0) { throw 'Download deadline exceeded' }
        $request = [Net.HttpWebRequest]::Create($current)
        $request.AllowAutoRedirect = $false
        $request.Timeout = [Math]::Min(10000, $remaining)
        $request.ReadWriteTimeout = [Math]::Min(10000, $remaining)
        $request.UserAgent = 'ClawBridge-installer'
        $response = $null
        try {
            $response = $request.GetResponse()
            $status = [int]$response.StatusCode
            if ($status -in @(301, 302, 303, 307, 308)) {
                if ($redirect -eq 5 -or [string]::IsNullOrEmpty($response.Headers['Location'])) { throw 'Invalid/excessive redirect' }
                $current = New-Object Uri($current, $response.Headers['Location'])
                continue
            }
            if ($status -ne 200) { throw ('Unexpected HTTP status ' + $status) }
            if ($response.ContentLength -gt $MaxBytes) { throw 'Download too large' }
            $inputStream = $response.GetResponseStream()
            $outputStream = $null
            try {
                $outputStream = [IO.File]::Open($OutFile, [IO.FileMode]::CreateNew, [IO.FileAccess]::Write, [IO.FileShare]::None)
                $buffer = New-Object byte[] 65536
                $total = 0L
                while (($read = $inputStream.Read($buffer, 0, $buffer.Length)) -gt 0) {
                    $total += $read
                    if ($total -gt $MaxBytes -or $deadline.ElapsedMilliseconds -gt 120000) { throw 'Download size/deadline exceeded' }
                    $outputStream.Write($buffer, 0, $read)
                }
            } finally {
                if ($null -ne $outputStream) { $outputStream.Dispose() }
                $inputStream.Dispose()
            }
            return $current.AbsoluteUri
        } finally {
            if ($null -ne $response) { $response.Dispose() }
            $request.Abort()
        }
    }
    throw 'Redirect limit exceeded'
}

function Install-ClawBridge([string]$Version = '', [string]$InstallDir = '') {
    if ($Version -ne '' -and -not (Test-ClawBridgeVersion $Version)) { throw 'Invalid canonical release version' }
    $platform = Get-ClawBridgePlatform
    if ($platform -cne 'windows_amd64') { throw 'Unsupported platform' }
    if ($InstallDir -eq '') {
        if ([string]::IsNullOrEmpty($env:LOCALAPPDATA)) { throw 'LOCALAPPDATA is required' }
        $InstallDir = Join-Path $env:LOCALAPPDATA 'Programs\ClawBridge'
    }
    if ($InstallDir -notmatch '^(?:[A-Za-z]:[\\/]|\\\\[^\\/]+[\\/][^\\/]+(?:[\\/]|$))') { throw 'InstallDir must be an absolute Windows path' }
    $InstallDir = [IO.Path]::GetFullPath($InstallDir)
    # Exclude alternate streams, device paths and names normalized differently by
    # Windows before transport or directory creation.
    if ($InstallDir.StartsWith('\\?\') -or $InstallDir.StartsWith('\\.\') -or $InstallDir.Substring(2).IndexOf(':') -ge 0) { throw 'Unsupported Windows destination path' }
    foreach ($segment in $InstallDir.Split([char[]]'\/')) {
        if ($segment -match '[<>"|?*\x00-\x1f]' -or $segment -match '[. ]$' -or $segment -match '^(?i:CON|PRN|AUX|NUL|COM[0-9]|LPT[0-9])(?:\.|$)') { throw 'Invalid Windows destination component' }
    }
    Assert-ClawBridgePath $InstallDir
    if ([IO.File]::Exists($InstallDir)) { throw 'InstallDir must be a directory' }
    $destination = Join-Path $InstallDir 'clawbridge.exe'
    Assert-ClawBridgePath $destination
    if ([IO.Directory]::Exists($destination)) { throw 'Destination must not be a directory' }
    $temporary = $null
    $stage = $null
    try {
        $temporary = New-ClawBridgePrivateDirectory ([IO.Path]::GetTempPath())
        $releaseBase = 'https://github.com/jkai-chen/ClawBridge/releases'
        if ($Version -eq '') {
            $latest = Invoke-ClawBridgeDownload -Uri ($releaseBase + '/latest') -OutFile (Join-Path $temporary 'latest') -MaxBytes 1048576
            $prefix = $releaseBase + '/tag/'
            if (-not $latest.StartsWith($prefix, [StringComparison]::Ordinal)) { throw 'Unexpected latest release location' }
            $Version = $latest.Substring($prefix.Length)
            if (-not (Test-ClawBridgeVersion $Version)) { throw 'Latest release is not a canonical stable version' }
        }
        $asset = 'clawbridge_' + $Version + '_windows_amd64.exe'
        $releaseUrl = $releaseBase + '/download/' + $Version
        $manifest = Join-Path $temporary 'SHA256SUMS'
        $null = Invoke-ClawBridgeDownload -Uri ($releaseUrl + '/SHA256SUMS') -OutFile $manifest -MaxBytes 1048576
        $selected = $null
        $seen = New-Object 'Collections.Generic.HashSet[string]' ([StringComparer]::Ordinal)
        foreach ($line in [IO.File]::ReadAllLines($manifest)) {
            if ($line -cnotmatch '\A([0-9a-f]{64})  ([A-Za-z0-9_.-]+)\z') { throw 'Malformed checksum manifest' }
            $digest = $Matches[1]
            $name = $Matches[2]
            if (-not $seen.Add($name)) { throw 'Duplicate checksum entry' }
            if ($name -ceq $asset) { $selected = $digest }
        }
        if ($null -eq $selected) { throw 'Selected checksum entry is absent' }
        $binary = Join-Path $temporary 'binary'
        $null = Invoke-ClawBridgeDownload -Uri ($releaseUrl + '/' + $asset) -OutFile $binary -MaxBytes 536870912
        if ((Get-Item -LiteralPath $binary).Length -eq 0) { throw 'Downloaded binary is empty' }
        if ((Get-FileHash -LiteralPath $binary -Algorithm SHA256).Hash.ToLowerInvariant() -cne $selected) { throw 'Binary checksum mismatch' }
        Assert-ClawBridgePath $destination
        if ([IO.Directory]::Exists($destination)) { throw 'Destination must not be a directory' }
        $null = [IO.Directory]::CreateDirectory($InstallDir)
        $stage = New-ClawBridgePrivateDirectory $InstallDir
        $stagedBinary = Join-Path $stage 'clawbridge.exe'
        [IO.File]::Copy($binary, $stagedBinary, $false)
        Assert-ClawBridgePath $destination
        if ([IO.File]::Exists($destination)) {
            # Replace is atomic on the same volume; backup stays in private stage.
            [IO.File]::Replace($stagedBinary, $destination, (Join-Path $stage 'previous.exe'))
        } else {
            [IO.File]::Move($stagedBinary, $destination)
        }
        Write-Output ('Installed ' + $Version + ' at ' + $destination)
        Write-Output ('$env:PATH = ' + "'" + $InstallDir.Replace("'", "''") + ";' + " + '$env:PATH')
        Write-Output 'For future shells, add that directory through your per-user Environment Variables settings.'
    } finally {
        if ($null -ne $stage) { [IO.Directory]::Delete($stage, $true) }
        if ($null -ne $temporary) { [IO.Directory]::Delete($temporary, $true) }
    }
}

# Dot-source to import testable functions without installation or network work.
if ($MyInvocation.InvocationName -eq '.') { return }
if ($Help) {
    Write-Output 'Usage: .\install.ps1 [-Version vX.Y.Z] [-InstallDir ABSOLUTE_PATH] [-Help]'
    return
}
Install-ClawBridge -Version $Version -InstallDir $InstallDir
