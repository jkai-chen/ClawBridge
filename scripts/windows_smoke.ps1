# Release smoke on native Windows (.github/workflows/windows-smoke.yml): runs an
# installed or downloaded clawbridge.exe through the paths a new user takes
# before any server is involved. Every state it writes is under a throwaway
# CLAWBRIDGE_HOME and skills directory; the runner's own profile is untouched.
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$Exe,
    [Parameter(Mandatory = $true)][string]$Expect
)
Set-StrictMode -Version 2.0
$ErrorActionPreference = 'Stop'
function Assert-Smoke([bool]$Condition, [string]$Message) { if (-not $Condition) { throw $Message } }
# A Go panic or runtime fault is never an acceptable answer, refusal or not.
function Assert-NoPanic([string]$Output, [string]$Step) {
    Assert-Smoke (-not ($Output -match 'panic:|goroutine [0-9]+ \[')) ($Step + ' panicked: ' + $Output)
}
Assert-Smoke (Test-Path -LiteralPath $Exe -PathType Leaf) ('No binary at ' + $Exe)
$root = Join-Path ([IO.Path]::GetTempPath()) ('cb-smoke-' + [Guid]::NewGuid().ToString('N'))
$null = [IO.Directory]::CreateDirectory($root)
$oldHome = $env:CLAWBRIDGE_HOME
$env:CLAWBRIDGE_HOME = Join-Path $root 'home'
try {
    # version: the stamped release, as JSON.
    $out = (& $Exe version 2>&1 | Out-String)
    Assert-Smoke ($LASTEXITCODE -eq 0) ('version failed: ' + $out)
    $reported = ($out | ConvertFrom-Json).data.version
    Assert-Smoke ($reported -ceq $Expect) ('version reports ' + $reported + ', expected ' + $Expect)
    Write-Output ('version: ' + $reported)

    $out = (& $Exe --help 2>&1 | Out-String)
    Assert-Smoke ($LASTEXITCODE -eq 0) ('--help failed: ' + $out)
    Assert-Smoke ($out -match 'join') '--help does not mention join'

    # skill install, then status, into a throwaway skills directory.
    $skills = Join-Path $root 'skills'
    $out = (& $Exe skill install --agent claude-code --dir $skills 2>&1 | Out-String)
    Assert-Smoke ($LASTEXITCODE -eq 0) ('skill install failed: ' + $out)
    $installed = @(Get-ChildItem -LiteralPath $skills -Recurse -File -Filter 'SKILL.md')
    Assert-Smoke ($installed.Count -ge 1) 'skill install wrote no SKILL.md'
    $out = (& $Exe skill status --agent claude-code --dir $skills 2>&1 | Out-String)
    Assert-Smoke ($LASTEXITCODE -eq 0) ('skill status failed: ' + $out)
    Write-Output ('skill: ' + $installed.Count + ' SKILL.md installed')

    # join refuses a link that is not one, before any network call, and keeps
    # no key or configuration.
    $out = ('not-an-invite-link' | & $Exe join --yes --key-store file 2>&1 | Out-String)
    Assert-Smoke ($LASTEXITCODE -ne 0) ('join accepted a bad link: ' + $out)
    Assert-NoPanic $out 'join'
    $kept = @(Get-ChildItem -LiteralPath $env:CLAWBRIDGE_HOME -Recurse -File -ErrorAction SilentlyContinue | Where-Object { $_.Name -match 'key|config' })
    Assert-Smoke ($kept.Count -eq 0) ('join kept files after refusing: ' + (($kept | ForEach-Object { $_.FullName }) -join ', '))
    Write-Output ('join: refused (' + $out.Trim() + ')')

    # doctor on a machine that was never set up: it reports, it does not crash.
    $out = (& $Exe doctor 2>&1 | Out-String)
    Assert-NoPanic $out 'doctor'
    Assert-Smoke ($out.Trim() -ne '') 'doctor printed nothing'
    Write-Output ('doctor: exit ' + $LASTEXITCODE)
} finally {
    $env:CLAWBRIDGE_HOME = $oldHome
    Remove-Item -LiteralPath $root -Recurse -Force -ErrorAction SilentlyContinue
}
