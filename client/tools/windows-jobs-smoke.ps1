param([Parameter(Mandatory = $true)][string]$Binary)

# Run on Windows with Task Scheduler available. Uses a disposable profile and
# registers jobs only; InteractiveToken execution is checked in a logged-in VM.
$ErrorActionPreference = 'Stop'
$exe = (Resolve-Path $Binary).Path
$root = Join-Path ([System.IO.Path]::GetTempPath()) ('tacit-jobs-' + [guid]::NewGuid() + " O'Brien")
$saved = @{}
foreach ($key in @('USERPROFILE', 'TACIT_SETTINGS', 'TACIT_STORE')) {
    $saved[$key] = [Environment]::GetEnvironmentVariable($key, 'Process')
}
$tasks = @()
try {
    New-Item -ItemType Directory -Path $root | Out-Null
    $env:USERPROFILE = $root
    $env:TACIT_SETTINGS = Join-Path $root 'config.yaml'
    $env:TACIT_STORE = Join-Path $root 'My Facts'
    '{}' | Set-Content -Encoding UTF8 $env:TACIT_SETTINGS
    $envFile = Join-Path $root 'provider.env'
    '' | Set-Content -Encoding UTF8 $envFile
    & $exe init
    if ($LASTEXITCODE -ne 0) { throw "init exited $LASTEXITCODE" }
    foreach ($target in @('dream', 'curate')) {
        & $exe install $target --env-file $envFile
        if ($LASTEXITCODE -ne 0) { throw "install $target exited $LASTEXITCODE" }
        $xmlPath = Join-Path $root ".config\tacit\tasks\tacit-$target.xml"
        $name = 'TacitPortable-' + [guid]::NewGuid()
        $user = [System.Security.Principal.WindowsIdentity]::GetCurrent().Name
        Register-ScheduledTask -TaskName $name -Xml (Get-Content -Raw $xmlPath) -User $user | Out-Null
        $tasks += $name
        $task = Get-ScheduledTask -TaskName $name
        if ($task.Principal.LogonType -ne 'Interactive') {
            throw "Unexpected logon type: $($task.Principal.LogonType)"
        }
    }
}
finally {
    $cleanupErrors = @()
    foreach ($name in $tasks) {
        try { Unregister-ScheduledTask -TaskName $name -Confirm:$false -ErrorAction Stop }
        catch { $cleanupErrors += "$name`: $($_.Exception.Message)" }
    }
    foreach ($key in $saved.Keys) {
        [Environment]::SetEnvironmentVariable($key, $saved[$key], 'Process')
    }
    if ($cleanupErrors.Count -gt 0) {
        throw "Task cleanup failed; profile preserved at $root`: $($cleanupErrors -join '; ')"
    }
    if (Test-Path $root) { Remove-Item -LiteralPath $root -Recurse -Force }
}
Write-Output 'PASS: native Windows dream and curator task registration and cleanup'
