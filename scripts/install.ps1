[CmdletBinding()]
param(
    [string]$Version = '',
    [string]$InstallDir = '',
    [switch]$NoSkill,
    [switch]$NoModifyPath,
    [switch]$AllowDowngrade,
    [switch]$Help
)
Set-StrictMode -Version 2.0
$ErrorActionPreference = 'Stop'

function Test-ClawBridgeVersion([string]$Value) {
    return $Value -cmatch '\Av(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\z'
}

# IMAGE_FILE_MACHINE values: 0x8664 x64, 0xAA64 ARM64, 0x14c x86. 0 means unknown.
# kernel32 IsWow64Process2 reports the machine Windows itself runs on, even for an
# x64 or x86 process emulated on ARM64, where the environment variables only
# describe the emulated process. It exists from Windows 10 1709; elsewhere (or if
# it cannot be compiled) this returns 0 and the caller falls back to the variables.
function Get-ClawBridgeNativeMachine {
    try {
        if (-not ('ClawBridge.NativeMachine' -as [type])) {
            Add-Type -Namespace ClawBridge -Name NativeMachine -MemberDefinition @'
[System.Runtime.InteropServices.DllImport("kernel32.dll")]
public static extern System.IntPtr GetCurrentProcess();
[System.Runtime.InteropServices.DllImport("kernel32.dll", SetLastError = true)]
[return: System.Runtime.InteropServices.MarshalAs(System.Runtime.InteropServices.UnmanagedType.Bool)]
public static extern bool IsWow64Process2(System.IntPtr process, out ushort processMachine, out ushort nativeMachine);
'@
        }
        [uint16]$processMachine = 0
        [uint16]$nativeMachine = 0
        if (-not [ClawBridge.NativeMachine]::IsWow64Process2([ClawBridge.NativeMachine]::GetCurrentProcess(), [ref]$processMachine, [ref]$nativeMachine)) { return 0 }
        return [int]$nativeMachine
    } catch {
        return 0
    }
}

function Get-ClawBridgeEnvironmentMachine {
    $nativeArchitecture = $env:PROCESSOR_ARCHITEW6432
    if ([string]::IsNullOrEmpty($nativeArchitecture)) { $nativeArchitecture = $env:PROCESSOR_ARCHITECTURE }
    switch ($nativeArchitecture) {
        'AMD64' { return 0x8664 }
        'ARM64' { return 0xAA64 }
        'x86' { return 0x14c }
        default { return 0 }
    }
}

function ConvertTo-ClawBridgePlatform([int]$Machine) {
    switch ($Machine) {
        0x8664 { return 'windows_amd64' }
        0xAA64 { return 'windows_arm64' }
        default { throw 'Only native Windows x64 and ARM64 are supported' }
    }
}

function Get-ClawBridgePlatform {
    if ([Environment]::OSVersion.Platform -ne [PlatformID]::Win32NT) { throw 'Windows x64 or ARM64 is required' }
    $machine = Get-ClawBridgeNativeMachine
    if ($machine -eq 0) { $machine = Get-ClawBridgeEnvironmentMachine }
    return ConvertTo-ClawBridgePlatform $machine
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

# Windows PowerShell 5.1 may default to protocols GitHub refuses; add TLS 1.2
# (and 1.3 where the OS supports it) without removing any enabled protocol.
function Enable-ClawBridgeTls {
    $protocols = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
    if ([Enum]::GetNames([Net.SecurityProtocolType]) -contains 'Tls13' -and [Environment]::OSVersion.Version.Build -ge 20348) {
        $protocols = $protocols -bor [Net.SecurityProtocolType]::Tls13
    }
    [Net.ServicePointManager]::SecurityProtocol = $protocols
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

# Runs the installed, checksum-verified binary once to install the agent skill.
function Invoke-ClawBridgeSkillInstall([string]$Binary) {
    $null = & $Binary skill install
    if ($LASTEXITCODE -ne 0) { throw ('skill install exited ' + $LASTEXITCODE) }
}

function Format-ClawBridgePathEntry([string]$Entry) {
    return $Entry.Trim().Trim('"').TrimEnd([char[]]'\/')
}

# Matches case-insensitively, ignoring trailing slashes, and also after
# expanding %VARIABLES%, as Windows itself resolves PATH entries.
function Test-ClawBridgePathContains([string]$PathValue, [string]$Directory) {
    $wanted = Format-ClawBridgePathEntry $Directory
    foreach ($entry in $PathValue.Split(';')) {
        if ($entry.Trim() -eq '') { continue }
        foreach ($candidate in @($entry, [Environment]::ExpandEnvironmentVariables($entry))) {
            if ([string]::Equals((Format-ClawBridgePathEntry $candidate), $wanted, [StringComparison]::OrdinalIgnoreCase)) { return $true }
        }
    }
    return $false
}

function Join-ClawBridgePath([string]$PathValue, [string]$Directory) {
    if ($PathValue -eq '') { return $Directory }
    if ($PathValue.EndsWith(';')) { return $PathValue + $Directory }
    return $PathValue + ';' + $Directory
}

# Appends Directory to the per-user Path under HKCU\<RegistryKey>; returns
# $false when it is already there. The raw value is read unexpanded and written
# back with its kind, so %VARIABLES% and REG_EXPAND_SZ survive.
function Add-ClawBridgeUserPath([string]$Directory, [string]$RegistryKey = 'Environment') {
    $key = [Microsoft.Win32.Registry]::CurrentUser.CreateSubKey($RegistryKey)
    try {
        $kind = [Microsoft.Win32.RegistryValueKind]::ExpandString
        $current = ''
        if ($key.GetValueNames() -contains 'Path') {
            $kind = $key.GetValueKind('Path')
            if ($kind -ne [Microsoft.Win32.RegistryValueKind]::ExpandString -and $kind -ne [Microsoft.Win32.RegistryValueKind]::String) { throw ('the user Path value has unexpected type ' + $kind) }
            $current = [string]$key.GetValue('Path', '', [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
        }
        if (Test-ClawBridgePathContains $current $Directory) { return $false }
        $key.SetValue('Path', (Join-ClawBridgePath $current $Directory), $kind)
        return $true
    } finally {
        $key.Dispose()
    }
}

# Tells Explorer and other listeners to reload the environment, so terminals
# opened afterwards see the new Path without signing out.
function Send-ClawBridgeEnvironmentChange {
    if (-not ('ClawBridgeInstaller.NativeMethods' -as [type])) {
        Add-Type -Namespace ClawBridgeInstaller -Name NativeMethods -MemberDefinition @'
[DllImport("user32.dll", SetLastError = true, CharSet = CharSet.Unicode)]
public static extern IntPtr SendMessageTimeout(IntPtr hWnd, uint Msg, UIntPtr wParam, string lParam, uint fuFlags, uint uTimeout, out UIntPtr lpdwResult);
'@
    }
    $result = [UIntPtr]::Zero
    # HWND_BROADCAST, WM_SETTINGCHANGE, SMTO_ABORTIFHUNG, 5 seconds.
    $null = [ClawBridgeInstaller.NativeMethods]::SendMessageTimeout([IntPtr]0xffff, 0x1A, [UIntPtr]::Zero, 'Environment', 2, 5000, [ref]$result)
}

# The previous binary is renamed to this path. A running `clawbridge mcp` keeps
# it mapped, so it may outlive the install: it goes to %TEMP% where possible
# (renames need the same volume) and otherwise beside the binary, never as .exe.
function Get-ClawBridgeBackupPath([string]$InstallDir) {
    $name = 'clawbridge-previous-' + [Guid]::NewGuid().ToString('N') + '.exe.old'
    $temp = [IO.Path]::GetTempPath()
    if ([string]::Equals([IO.Path]::GetPathRoot($temp), [IO.Path]::GetPathRoot($InstallDir), [StringComparison]::OrdinalIgnoreCase)) {
        return (Join-Path $temp $name)
    }
    return (Join-Path $InstallDir ('.' + $name))
}

# The one file-move primitive of an install, separate so fixtures can fail it.
function Move-ClawBridgeFile([string]$Source, [string]$Destination) {
    [IO.File]::Move($Source, $Destination)
}

# Best effort: a leftover must never fail an install that already succeeded.
function Remove-ClawBridgeLeftover([string]$Path) {
    try {
        if ([IO.Directory]::Exists($Path)) { [IO.Directory]::Delete($Path, $true) }
        elseif ([IO.File]::Exists($Path)) { [IO.File]::Delete($Path) }
        return $true
    } catch {
        return $false
    }
}

# The installed binary's release version, or $null when it doesn't say one
# (a dev build, or no runnable binary): the downgrade guard asks it.
function Get-ClawBridgeInstalledVersion([string]$Path) {
    if (-not [IO.File]::Exists($Path)) { return $null }
    try {
        $text = (& $Path version 2>$null | Out-String)
        if ($text -cmatch '"version":"(v(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*))"') { return $Matches[1] }
    } catch {
        Write-Verbose ('The installed binary did not report its version: ' + $_.Exception.Message)
    }
    return $null
}

# Whether release version A is older than B; both are canonical.
function Test-ClawBridgeOlder([string]$A, [string]$B) {
    return [Version]$A.Substring(1) -lt [Version]$B.Substring(1)
}

# The GitHub CLI, for CLAWBRIDGE_VERIFY=required, or $null.
function Get-ClawBridgeGh {
    $command = Get-Command -Name 'gh' -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($null -eq $command) { return $null }
    return $command.Path
}

# Checks the binary's build provenance (G9-10): the release workflow of this
# repository built it, for this tag.
function Test-ClawBridgeProvenance([string]$Gh, [string]$Binary, [string]$Version) {
    & $Gh attestation verify $Binary --repo jkai-chen/ClawBridge --signer-workflow jkai-chen/ClawBridge/.github/workflows/release.yml --source-ref ('refs/tags/' + $Version) *> $null
    return $LASTEXITCODE -eq 0
}

function Get-ClawBridgeResolvedCommand {
    $command = Get-Command -Name 'clawbridge' -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($null -eq $command) { return $null }
    return $command.Path
}

function Install-ClawBridge([string]$Version = '', [string]$InstallDir = '', [switch]$NoSkill, [switch]$NoModifyPath, [switch]$AllowDowngrade, [string]$PathRegistryKey = 'Environment') {
    if ($Version -ne '' -and -not (Test-ClawBridgeVersion $Version)) { throw 'Invalid canonical release version' }
    # Build provenance is optional, and required only when asked for, since it
    # needs the GitHub CLI. The checksum alone proves the bytes match the
    # release's manifest, not who built them.
    $gh = $null
    $verify = [string]$env:CLAWBRIDGE_VERIFY
    if ($verify -ceq 'required') {
        $gh = Get-ClawBridgeGh
        if ($null -eq $gh) { throw 'CLAWBRIDGE_VERIFY=required needs the GitHub CLI (gh) on PATH' }
    } elseif ($verify -ne '') {
        throw 'CLAWBRIDGE_VERIFY must be unset or "required"'
    }
    $platform = Get-ClawBridgePlatform
    if (@('windows_amd64', 'windows_arm64') -cnotcontains $platform) { throw 'Unsupported platform' }
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
        Enable-ClawBridgeTls
        $releaseBase = 'https://github.com/jkai-chen/ClawBridge/releases'
        if ($Version -eq '') {
            $latest = Invoke-ClawBridgeDownload -Uri ($releaseBase + '/latest') -OutFile (Join-Path $temporary 'latest') -MaxBytes 1048576
            $prefix = $releaseBase + '/tag/'
            if (-not $latest.StartsWith($prefix, [StringComparison]::Ordinal)) { throw 'Unexpected latest release location' }
            $Version = $latest.Substring($prefix.Length)
            if (-not (Test-ClawBridgeVersion $Version)) { throw 'Latest release is not a canonical stable version' }
        }
        # Never silently older than what is installed: a replaced "latest" must
        # not roll an install back.
        if (-not $AllowDowngrade) {
            $current = Get-ClawBridgeInstalledVersion $destination
            if ($null -ne $current -and (Test-ClawBridgeOlder $Version $current)) {
                throw ($Version + ' is older than the installed ' + $current + '; pass -AllowDowngrade to install it anyway')
            }
        }
        $asset = 'clawbridge_' + $Version + '_' + $platform + '.exe'
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
        if ($null -eq $selected) {
            # Releases before Windows on ARM have no windows_arm64 asset.
            if ($platform -ceq 'windows_arm64') { throw ('ClawBridge ' + $Version + ' has no Windows ARM64 build; install a later version') }
            throw 'Selected checksum entry is absent'
        }
        $binary = Join-Path $temporary 'binary'
        $null = Invoke-ClawBridgeDownload -Uri ($releaseUrl + '/' + $asset) -OutFile $binary -MaxBytes 536870912
        if ((Get-Item -LiteralPath $binary).Length -eq 0) { throw 'Downloaded binary is empty' }
        if ((Get-FileHash -LiteralPath $binary -Algorithm SHA256).Hash.ToLowerInvariant() -cne $selected) { throw 'Binary checksum mismatch' }
        if ($null -ne $gh) {
            if (-not (Test-ClawBridgeProvenance $gh $binary $Version)) { throw ('The build provenance of ' + $Version + ' could not be verified (gh attestation verify); nothing was installed') }
            Write-Output ('Verified the build provenance of ' + $Version)
        }
        Assert-ClawBridgePath $destination
        if ([IO.Directory]::Exists($destination)) { throw 'Destination must not be a directory' }
        $null = [IO.Directory]::CreateDirectory($InstallDir)
        $stage = New-ClawBridgePrivateDirectory $InstallDir
        $stagedBinary = Join-Path $stage 'clawbridge.exe'
        [IO.File]::Copy($binary, $stagedBinary, $false)
        Assert-ClawBridgePath $destination
        $backup = $null
        if ([IO.File]::Exists($destination)) {
            $backup = Get-ClawBridgeBackupPath $InstallDir
            $inUse = 'Could not replace ' + $destination + ', which is in use. Close the agents running `clawbridge mcp` and any other clawbridge process, then run the installer again.'
            # File.Replace refuses a running image, but Windows allows renaming
            # it, so move the old binary aside, then move the new one in.
            try {
                Move-ClawBridgeFile $destination $backup
            } catch {
                throw ($inUse + ' (' + $_.Exception.Message + ')')
            }
            try {
                Move-ClawBridgeFile $stagedBinary $destination
            } catch {
                $reason = $_.Exception.Message
                try {
                    Move-ClawBridgeFile $backup $destination
                } catch {
                    throw ('Could not install ' + $destination + ' or restore the previous binary, which is now at ' + $backup + '; move it back to ' + $destination + '. (' + $reason + ')')
                }
                throw ($inUse + ' (' + $reason + ')')
            }
        } else {
            Move-ClawBridgeFile $stagedBinary $destination
        }
        Write-Output ('Installed ' + $Version + ' at ' + $destination)
        if ($null -ne $backup -and -not (Remove-ClawBridgeLeftover $backup)) {
            Write-Output ('The previous version is still running; its old copy at ' + $backup + ' can be deleted once it exits.')
        }
        # The agent skill is a convenience: a failure warns and never fails the install.
        if (-not $NoSkill) {
            try {
                Invoke-ClawBridgeSkillInstall $destination
                Write-Output 'Installed the ClawBridge agent skill (see: clawbridge skill status).'
            } catch {
                Write-Warning 'The agent skill was not installed; run `clawbridge skill install --agent claude-code|codex|all` later.'
            }
        }
        $sessionStep = '$env:Path = ' + "'" + $InstallDir.Replace("'", "''") + ";' + " + '$env:Path'
        # Looked up before this session's Path changes, so it shows what PATH
        # already resolves: an older install or a package manager's shim.
        $resolved = Get-ClawBridgeResolvedCommand
        $shadowed = $null -ne $resolved -and -not [string]::Equals([IO.Path]::GetFullPath($resolved), $destination, [StringComparison]::OrdinalIgnoreCase)
        if ($shadowed) {
            Write-Warning ('`clawbridge` on your PATH is ' + $resolved + ', not the binary just installed at ' + $destination + '. New terminals will keep running the other one until you remove it or move ' + $InstallDir + ' ahead of it in PATH.')
        }
        # PATH separates entries with ';' and Windows does not reliably honour
        # quoting, so such a directory would be broken and re-added every time.
        if ($InstallDir.IndexOfAny([char[]]';"') -ge 0) {
            Write-Warning ('PATH was not changed: ' + $InstallDir + ' contains ; or ", which PATH cannot hold. Run ' + $destination + ' by its full path, or reinstall with another -InstallDir.')
            return
        }
        if ($NoModifyPath) {
            Write-Output ('PATH was not changed. For this terminal: ' + $sessionStep)
            Write-Output 'For future terminals, add that directory through your per-user Environment Variables settings.'
            return
        }
        # A PATH failure is a warning: the binary is already installed.
        try {
            if (Add-ClawBridgeUserPath -Directory $InstallDir -RegistryKey $PathRegistryKey) {
                try { Send-ClawBridgeEnvironmentChange } catch { Write-Verbose ('Environment change broadcast failed: ' + $_.Exception.Message) }
                Write-Output ('Added ' + $InstallDir + ' to your user PATH for new terminals.')
            } else {
                Write-Output ($InstallDir + ' is already on your user PATH.')
            }
        } catch {
            Write-Warning ('Your user PATH was not updated (' + $_.Exception.Message + '); add ' + $InstallDir + ' through your per-user Environment Variables settings.')
        }
        # This session puts the new binary first; the persisted Path only appends.
        if ($shadowed -or -not (Test-ClawBridgePathContains ([string]$env:Path) $InstallDir)) {
            $env:Path = $InstallDir + ';' + [string]$env:Path
        }
        Write-Output ('Terminals that were already open need: ' + $sessionStep)
    } finally {
        if ($null -ne $stage -and -not (Remove-ClawBridgeLeftover $stage)) { Write-Warning ('Could not remove the temporary directory ' + $stage) }
        if ($null -ne $temporary -and -not (Remove-ClawBridgeLeftover $temporary)) { Write-Warning ('Could not remove the temporary directory ' + $temporary) }
    }
}

# Dot-source to import testable functions without installation or network work.
if ($MyInvocation.InvocationName -eq '.') { return }
if ($Help) {
    Write-Output 'Usage: powershell -NoProfile -ExecutionPolicy Bypass -File install.ps1 [-Version vX.Y.Z] [-InstallDir ABSOLUTE_PATH] [-NoSkill] [-NoModifyPath] [-AllowDowngrade] [-Help]'
    Write-Output 'An older version than the installed one needs -AllowDowngrade. $env:CLAWBRIDGE_VERIFY = ''required'' also checks the build provenance with the GitHub CLI.'
    Write-Output 'After installing, runs `clawbridge skill install` for the agents configured here unless -NoSkill,'
    Write-Output 'and appends the install directory to your user PATH unless -NoModifyPath.'
    return
}
Install-ClawBridge -Version $Version -InstallDir $InstallDir -NoSkill:$NoSkill -NoModifyPath:$NoModifyPath -AllowDowngrade:$AllowDowngrade
