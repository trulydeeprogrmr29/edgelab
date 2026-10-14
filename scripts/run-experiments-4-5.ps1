[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$OutputDirectory,
    [switch]$Pilot,
    [ValidateRange(1, 10)]
    [int]$Repetitions = 3
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$outputPath = [System.IO.Path]::GetFullPath((Join-Path (Get-Location).Path $OutputDirectory))
if (Test-Path -LiteralPath $outputPath) {
    if (@(Get-ChildItem -LiteralPath $outputPath -Force).Count -gt 0) {
        throw "Output directory must be new or empty: $outputPath"
    }
} else {
    [void](New-Item -ItemType Directory -Path $outputPath -Force)
}

$binaryPath = Join-Path $outputPath 'bin'
[void](New-Item -ItemType Directory -Path $binaryPath -Force)
$upstreamExecutable = Join-Path $binaryPath 'upstream.exe'
$proxyExecutable = Join-Path $binaryPath 'edgelab.exe'
$benchmarkExecutable = Join-Path $binaryPath 'bench.exe'
Push-Location $root
try {
    & go build -o $upstreamExecutable .\cmd\upstream
    if ($LASTEXITCODE -ne 0) { throw "Building upstream failed with exit code $LASTEXITCODE" }
    & go build -o $proxyExecutable .\cmd\edgelab
    if ($LASTEXITCODE -ne 0) { throw "Building proxy failed with exit code $LASTEXITCODE" }
    & go build -o $benchmarkExecutable .\cmd\bench
    if ($LASTEXITCODE -ne 0) { throw "Building benchmark failed with exit code $LASTEXITCODE" }
}
finally {
    Pop-Location
}

$upstreamLog = Join-Path $outputPath 'upstream.log'
$upstreamErrorLog = Join-Path $outputPath 'upstream.stderr.log'
$proxyLog = Join-Path $outputPath 'proxy.log'
$proxyErrorLog = Join-Path $outputPath 'proxy.stderr.log'
$upstreamProcess = $null
$proxyProcess = $null
$proxyAddress = 'http://127.0.0.1:18180'
$upstreamAddress = 'http://127.0.0.1:19190'
$controlAddress = "$upstreamAddress/__control/failure"
$recoveryPath = Join-Path $outputPath 'experiment-5-recovery.csv'
[System.IO.File]::WriteAllText(
    $recoveryPath,
    "run_id,restore_to_first_successful_probe_ms,probe_status,error`r`n"
)

function Wait-HttpReady {
    param(
        [System.Diagnostics.Process]$Process,
        [string]$Url
    )

    $timer = [System.Diagnostics.Stopwatch]::StartNew()
    while ($timer.Elapsed.TotalSeconds -lt 20) {
        $Process.Refresh()
        if ($Process.HasExited) {
            throw "Server PID $($Process.Id) exited before becoming ready at $Url"
        }
        try {
            $null = Invoke-WebRequest -UseBasicParsing -Uri $Url -TimeoutSec 2
            return
        }
        catch {
            Start-Sleep -Milliseconds 200
        }
    }
    throw "Server did not become ready within 20 seconds: $Url"
}

function Invoke-Phase {
    param(
        [string]$RunId,
        [string]$ConfigurationId,
        [string]$Url,
        [int]$Requests,
        [int]$Concurrency,
        [int]$WarmupRequests
    )

    $clientFlags = @(
        '-timeout=5s', '-force-http2=true', '-max-idle-conns=1000',
        '-max-idle-conns-per-host=100', '-max-conns-per-host=0',
        '-idle-conn-timeout=90s', '-dial-timeout=30s', '-keep-alive=30s',
        '-tls-handshake-timeout=10s', '-response-header-timeout=0s',
        '-expect-continue-timeout=1s', '-read-chunk-size=1024'
    )
    $warmup = @(
        '-url', $Url, '-n', [string]$WarmupRequests, '-c', [string]$Concurrency
    ) + $clientFlags
    $measured = @(
        '-url', $Url, '-n', [string]$Requests, '-c', [string]$Concurrency
    ) + $clientFlags
    $wrapper = Join-Path $PSScriptRoot 'measure-benchmark.ps1'
    & $wrapper `
        -RunId $RunId `
        -Experiment $(if ($RunId -like 'exp4-*') { 'experiment-4-load-scaling' } else { 'experiment-5-upstream-failure-recovery' }) `
        -ConfigurationId $ConfigurationId `
        -BenchmarkExecutable $benchmarkExecutable `
        -WarmupArguments $warmup `
        -BenchmarkArguments $measured `
        -ProxyPid $proxyProcess.Id `
        -UpstreamPid $upstreamProcess.Id `
        -ProxyStatsUrl "$proxyAddress/stats" `
        -ProxyStatsResetUrl "$proxyAddress/stats/reset" `
        -UpstreamStatsUrl "$upstreamAddress/stats" `
        -UpstreamStatsResetUrl "$upstreamAddress/stats/reset" `
        -OutputDirectory $outputPath
}

function Set-UpstreamFailureMode {
    param([bool]$Enabled)

    $uri = "$controlAddress`?enabled=$($Enabled.ToString().ToLowerInvariant())"
    $response = Invoke-WebRequest -UseBasicParsing -Method Post -Uri $uri -TimeoutSec 3
    if ([int]$response.StatusCode -ne 204) {
        throw "Upstream failure control returned HTTP $([int]$response.StatusCode)"
    }
}

function Measure-RecoveryProbe {
    param([string]$RunId)

    $timer = [System.Diagnostics.Stopwatch]::StartNew()
    try {
        Set-UpstreamFailureMode $false
        $response = Invoke-WebRequest -UseBasicParsing -Uri "$proxyAddress/hello" -TimeoutSec 5
        $timer.Stop()
        if ([int]$response.StatusCode -ne 200) {
            throw "Recovery probe returned HTTP $([int]$response.StatusCode)"
        }
        $elapsed = $timer.Elapsed.TotalMilliseconds.ToString(
            'F3', [Globalization.CultureInfo]::InvariantCulture
        )
        [System.IO.File]::AppendAllText($recoveryPath, "$RunId,$elapsed,200,`r`n")
    }
    catch {
        $timer.Stop()
        $message = $_.Exception.Message -replace '"', '""' -replace '[\r\n,]', ' '
        [System.IO.File]::AppendAllText($recoveryPath, "$RunId,,$([int]0),`"$message`"`r`n")
        throw
    }
}

try {
    $upstreamProcess = Start-Process `
        -FilePath $upstreamExecutable `
        -ArgumentList @('-listen=127.0.0.1:19190') `
        -WorkingDirectory $outputPath `
        -RedirectStandardOutput $upstreamLog `
        -RedirectStandardError $upstreamErrorLog `
        -PassThru `
        -WindowStyle Hidden
    Wait-HttpReady $upstreamProcess "$upstreamAddress/stats"

    $proxyProcess = Start-Process `
        -FilePath $proxyExecutable `
        -ArgumentList @('-listen=127.0.0.1:18180', "-upstream=$upstreamAddress") `
        -WorkingDirectory $outputPath `
        -RedirectStandardOutput $proxyLog `
        -RedirectStandardError $proxyErrorLog `
        -PassThru `
        -WindowStyle Hidden
    Wait-HttpReady $proxyProcess "$proxyAddress/healthz"

    if ($Pilot) {
        Invoke-Phase 'exp4-pilot-c1' 'c1-pilot' "$proxyAddress/payload/64k" 25 1 5
        Invoke-Phase 'exp5-pilot-healthy' 'pilot-healthy-c10' "$proxyAddress/hello" 20 10 5
        Invoke-Phase 'exp5-pilot-delay' 'pilot-delay200ms-c10' "$proxyAddress/delay?ms=200" 20 10 5
        Set-UpstreamFailureMode $true
        $failurePhaseComplete = $false
        try {
            Invoke-Phase 'exp5-pilot-failure' 'pilot-upstream-disconnect-c10' "$proxyAddress/hello" 20 10 5
            $failurePhaseComplete = $true
        }
        finally {
            if (-not $failurePhaseComplete) {
                Set-UpstreamFailureMode $false
            }
        }
        Measure-RecoveryProbe 'exp5-pilot'
        Invoke-Phase 'exp5-pilot-recovery' 'pilot-recovery-c10' "$proxyAddress/hello" 20 10 5
    } else {
        foreach ($concurrency in @(1, 5, 10, 25, 50)) {
            for ($repetition = 1; $repetition -le $Repetitions; $repetition++) {
                $runId = "exp4-c$concurrency-rep$repetition"
                Invoke-Phase $runId "c$concurrency" "$proxyAddress/payload/64k" 200 $concurrency 20
            }
        }

        for ($repetition = 1; $repetition -le 3; $repetition++) {
            $suffix = "rep$repetition"
            Invoke-Phase "exp5-healthy-$suffix" "healthy-c10" "$proxyAddress/hello" 100 10 10
            Invoke-Phase "exp5-delay-$suffix" "delay200ms-c10" "$proxyAddress/delay?ms=200" 40 10 10
            Set-UpstreamFailureMode $true
            $failurePhaseComplete = $false
            try {
                Invoke-Phase "exp5-failure-$suffix" "upstream-disconnect-c10" "$proxyAddress/hello" 40 10 10
                $failurePhaseComplete = $true
            }
            finally {
                if (-not $failurePhaseComplete) {
                    Set-UpstreamFailureMode $false
                }
            }
            Measure-RecoveryProbe "exp5-recovery-$suffix"
            Invoke-Phase "exp5-recovery-$suffix" "recovery-c10" "$proxyAddress/hello" 100 10 10
        }
    }
    Write-Output "Raw records and logs: $outputPath"
}
finally {
    if ($proxyProcess) {
        $proxyProcess.Refresh()
        if (-not $proxyProcess.HasExited) {
            Stop-Process -Id $proxyProcess.Id -Force
            $proxyProcess.WaitForExit()
        }
    }
    if ($upstreamProcess) {
        $upstreamProcess.Refresh()
        if (-not $upstreamProcess.HasExited) {
            Stop-Process -Id $upstreamProcess.Id -Force
            $upstreamProcess.WaitForExit()
        }
    }
}
