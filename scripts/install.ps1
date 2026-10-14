param(
    [string]$Version = "latest",
    [string]$BinDir = (Join-Path $env:LOCALAPPDATA "tacit\bin"),
    [string]$ReleaseBaseUrl = "https://github.com/agent-sh/tacit/releases"
)
$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"
if ($Version -eq "latest") {
    $response = Invoke-WebRequest "$ReleaseBaseUrl/latest" -UseBasicParsing
    $uri = $response.BaseResponse.ResponseUri
    if (-not $uri) { $uri = $response.BaseResponse.RequestMessage.RequestUri }
    $Version = [uri]::UnescapeDataString($uri.AbsolutePath.Split('/')[-1])
}
if ($Version -notmatch '^v[0-9]+\.[0-9]+\.[0-9]+(?:-[A-Za-z0-9.-]+)?$') { throw "Invalid release tag" }
$arch = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString().ToLowerInvariant()
switch ($arch) { "x64" { $arch = "amd64" }; "arm64" { }; default { throw "Supported architectures are amd64 and arm64" } }
$task = Join-Path $env:LOCALAPPDATA ("tacit\cache\install-" + [Guid]::NewGuid())
New-Item -ItemType Directory -Force $task | Out-Null
try {
    $name = "tacit-$Version-windows-$arch.zip"
    $archive = Join-Path $task $name
    Invoke-WebRequest -UseBasicParsing "$ReleaseBaseUrl/download/$Version/$name" -OutFile $archive
    $sumFile = Join-Path $task "SHA256SUMS"
    Invoke-WebRequest -UseBasicParsing "$ReleaseBaseUrl/download/$Version/SHA256SUMS" -OutFile $sumFile
    $sums = Get-Content $sumFile -Raw
    $line = @($sums -split "`n" | Where-Object { $_ -match ("^[0-9a-f]{64}  " + [regex]::Escape($name) + "`r?$") })
    if ($line.Count -ne 1) { throw "Missing or invalid archive checksum" }
    $expected = ($line[0] -split '  ')[0]
    if ((Get-FileHash $archive -Algorithm SHA256).Hash.ToLowerInvariant() -ne $expected) { throw "Archive checksum mismatch" }
    Expand-Archive $archive -DestinationPath $task
    $source = Join-Path $task "tacit-$Version-windows-$arch"
    $tacit = Join-Path $source "tacit.exe"
    $scrump = Join-Path $source "scrump.exe"
    if (-not (Test-Path $tacit) -or -not (Test-Path $scrump)) { throw "Archive lacks required executables" }
    $reported = & $tacit --version
    if ($LASTEXITCODE -ne 0 -or $reported -ne "tacit $Version") { throw "Archive version mismatch" }
    $canary = "TACIT_PROBE_TOKEN=fixture-value-12345"
    $masked = $canary | & $scrump --profile text scrub - -o -
    if ($LASTEXITCODE -ne 0 -or $masked -match 'fixture-value-12345') { throw "Bundled scrump lacks working text masking" }
    New-Item -ItemType Directory -Force $BinDir | Out-Null
    # Stage before replacing; locked running executables fail instead of truncating.
    foreach ($exe in @("scrump.exe", "tacit.exe")) {
        $staged = Join-Path $BinDir ("." + $exe + "." + [Guid]::NewGuid())
        try {
            Copy-Item (Join-Path $source $exe) $staged
            $dest = Join-Path $BinDir $exe
            if (Test-Path $dest) {
                # Windows PowerShell coerces a null string argument to an empty
                # backup path. A named backup works on both PowerShell editions.
                $backup = Join-Path $BinDir ("." + $exe + ".backup." + [Guid]::NewGuid())
                [System.IO.File]::Replace($staged, $dest, $backup)
                Remove-Item $backup
            }
            else { [System.IO.File]::Move($staged, $dest) }
        } finally { if (Test-Path $staged) { Remove-Item $staged } }
    }
    Write-Output "Installed tacit $Version and scrump in $BinDir"
    Write-Output "Add $BinDir to PATH, then run: tacit init"
} finally { Remove-Item -Recurse -Force $task }
