[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$RunId,
    [Parameter(Mandatory = $true)]
    [string]$Experiment,
    [Parameter(Mandatory = $true)]
    [string]$ConfigurationId,
    [Parameter(Mandatory = $true)]
    [string]$BenchmarkExecutable,
    [Parameter(Mandatory = $true)]
    [string[]]$BenchmarkArguments,
    [string[]]$WarmupArguments,
    [Parameter(Mandatory = $true)]
    [int]$ProxyPid,
    [Parameter(Mandatory = $true)]
    [int]$UpstreamPid,
    [string]$ProxyStatsUrl = 'http://127.0.0.1:18080/stats',
    [string]$ProxyStatsResetUrl = 'http://127.0.0.1:18080/stats/reset',
    [string]$UpstreamStatsUrl = 'http://127.0.0.1:19090/stats',
    [string]$UpstreamStatsResetUrl = 'http://127.0.0.1:19090/stats/reset',
    [Parameter(Mandatory = $true)]
    [string]$OutputDirectory,
    [ValidateRange(1, 3600)]
    [int]$TimeoutSeconds = 600
)

$ErrorActionPreference = 'Stop'

function ConvertTo-WindowsArgument {
    param([string]$Value)

    if ($Value.Length -gt 0 -and $Value -notmatch '[\s"]') {
        return $Value
    }

    $builder = New-Object System.Text.StringBuilder
    [void]$builder.Append('"')
    $slashes = 0
    foreach ($character in $Value.ToCharArray()) {
        if ($character -eq '\') {
            $slashes++
            continue
        }
        if ($character -eq '"') {
            [void]$builder.Append(('\' * (2 * $slashes + 1)))
            [void]$builder.Append('"')
            $slashes = 0
            continue
        }
        if ($slashes -gt 0) {
            [void]$builder.Append(('\' * $slashes))
            $slashes = 0
        }
        [void]$builder.Append($character)
    }
    if ($slashes -gt 0) {
        [void]$builder.Append(('\' * (2 * $slashes)))
    }
    [void]$builder.Append('"')
    return $builder.ToString()
}

function Get-ProcessSnapshot {
    param([int]$Id)

    $process = [System.Diagnostics.Process]::GetProcessById($Id)
    try {
        $process.Refresh()
        return [ordered]@{
            pid = $Id
            cpu_seconds = $process.TotalProcessorTime.TotalSeconds
            working_set_bytes = [int64]$process.WorkingSet64
            os_peak_working_set_bytes_process_lifetime = [int64]$process.PeakWorkingSet64
        }
    }
    finally {
        $process.Dispose()
    }
}

function Get-ChildProcessSnapshot {
    param([System.Diagnostics.Process]$Process)

    $Process.Refresh()
    return [ordered]@{
        pid = $Process.Id
        cpu_seconds = $Process.TotalProcessorTime.TotalSeconds
        working_set_bytes = [int64]$Process.WorkingSet64
        os_peak_working_set_bytes_process_lifetime = [int64]$Process.PeakWorkingSet64
    }
}

function ConvertFrom-StatsText {
    param([string]$Text)

    $values = @{}
    foreach ($line in ($Text -split "`r?`n")) {
        if ($line -match '^([a-z_]+)\s+(\d+)$') {
            $values[$Matches[1]] = [int64]$Matches[2]
        }
    }
    return $values
}

function Get-StatsSnapshot {
    param(
        [string]$Url,
        [string]$Name,
        [string[]]$RequiredCounters
    )

    $stats = ConvertFrom-StatsText (Get-HttpText $Url)
    $missing = @($RequiredCounters | Where-Object { -not $stats.ContainsKey($_) })
    if ($missing.Count -gt 0) {
        throw "$Name stats response from $Url is missing required counters: $($missing -join ', ')"
    }
    return $stats
}

function Get-StatValue {
    param(
        [hashtable]$Stats,
        [string]$Name
    )
    if ($Stats.ContainsKey($Name)) {
        return [int64]$Stats[$Name]
    }
    return $null
}

function ConvertTo-Nanoseconds {
    param([string]$Duration)

    if ($Duration -match '^([0-9.]+)(ns|ms|us|s|[\u00B5]s)$') {
        $value = [double]$Matches[1]
        $unit = $Matches[2]
        if ($unit -eq 'ns') {
            return [int64]$value
        }
        if ($unit -eq 'us' -or ($unit.Length -eq 2 -and $unit[0] -eq [char]0x00B5)) {
            return [int64]($value * 1000)
        }
        if ($unit -eq 'ms') {
            return [int64]($value * 1000000)
        }
        if ($unit -eq 's') {
            return [int64]($value * 1000000000)
        }
    }
    return $null
}

function Get-BenchmarkMetrics {
    param([string]$Text)

    $metrics = [ordered]@{
        requests = $null
        effective_workers = $null
        throughput_requests_per_second = $null
        successes = $null
        unique_failed_requests = $null
        client_transport_errors = $null
        body_read_or_close_errors = $null
        http_status_errors = $null
        p50_ns = $null
        p95_ns = $null
        p99_ns = $null
        benchmark_elapsed_ns = $null
    }
    $patterns = [ordered]@{
        requests = '^Requests:\s+(\d+)$'
        effective_workers = '^Concurrency:\s+(\d+)$'
        throughput_requests_per_second = '^Throughput:\s+([0-9.]+)\s+req/s$'
        successes = '^Successful:\s+(\d+)$'
        unique_failed_requests = '^Errors:\s+(\d+)$'
        client_transport_errors = '^Client request errors:\s+(\d+)$'
        body_read_or_close_errors = '^Body read/close errors:\s+(\d+)$'
        http_status_errors = '^HTTP status errors:\s+(\d+)$'
        p50 = '^p50 latency:\s+(\S+)$'
        p95 = '^p95 latency:\s+(\S+)$'
        p99 = '^p99 latency:\s+(\S+)$'
        benchmark_elapsed = '^Duration:\s+(\S+)$'
    }
    foreach ($line in ($Text -split "`r?`n")) {
        foreach ($name in $patterns.Keys) {
            if ($line -match $patterns[$name]) {
                $value = $Matches[1]
                switch ($name) {
                    { $_ -in 'requests', 'effective_workers', 'successes', 'unique_failed_requests',
                        'client_transport_errors', 'body_read_or_close_errors', 'http_status_errors' } {
                        $metrics[$name] = [int64]$value
                    }
                    'throughput_requests_per_second' {
                        $metrics[$name] = [double]$value
                    }
                    { $_ -in 'p50', 'p95', 'p99', 'benchmark_elapsed' } {
                        $metrics[($_ + '_ns')] = ConvertTo-Nanoseconds $value
                    }
                }
            }
        }
    }
    return $metrics
}

function Get-CommandLine {
    param([string]$Executable, [string[]]$Arguments)

    $parts = @((ConvertTo-WindowsArgument $Executable))
    foreach ($argument in $Arguments) {
        $parts += ConvertTo-WindowsArgument $argument
    }
    return $parts -join ' '
}

function Invoke-BenchmarkPhase {
    param(
        [string]$Phase,
        [string[]]$Arguments
    )

    $commandLine = Get-CommandLine $BenchmarkExecutable $Arguments
    $stdoutPath = Join-Path $OutputDirectory ($RunId + '-' + $Phase + '.stdout.txt')
    $stderrPath = Join-Path $OutputDirectory ($RunId + '-' + $Phase + '.stderr.txt')
    $proxyBefore = Get-ProcessSnapshot $ProxyPid
    $upstreamBefore = Get-ProcessSnapshot $UpstreamPid
    $startedUtc = [DateTime]::UtcNow
    $stopwatch = [System.Diagnostics.Stopwatch]::StartNew()

    $startInfo = New-Object System.Diagnostics.ProcessStartInfo
    $startInfo.FileName = $BenchmarkExecutable
    $startInfo.Arguments = ($Arguments | ForEach-Object { ConvertTo-WindowsArgument $_ }) -join ' '
    $startInfo.UseShellExecute = $false
    $startInfo.CreateNoWindow = $true
    $startInfo.RedirectStandardOutput = $true
    $startInfo.RedirectStandardError = $true
    $startInfo.StandardOutputEncoding = [System.Text.Encoding]::UTF8
    $startInfo.StandardErrorEncoding = [System.Text.Encoding]::UTF8
    $process = New-Object System.Diagnostics.Process
    $process.StartInfo = $startInfo
    if (-not $process.Start()) {
        throw "Could not start benchmark command: $commandLine"
    }

    $benchmarkBefore = Get-ChildProcessSnapshot $process
    $benchmarkBefore.cpu_seconds = 0.0
    $stdoutTask = $process.StandardOutput.ReadToEndAsync()
    $stderrTask = $process.StandardError.ReadToEndAsync()
    $benchmarkPeakWorkingSet = [int64]$benchmarkBefore.os_peak_working_set_bytes_process_lifetime
    $benchmarkWorkingSetAfter = [int64]$benchmarkBefore.working_set_bytes
    $benchmarkPeakSampleUtc = [DateTime]::UtcNow
    $lastObserverRefreshSeconds = 0.0
    $timedOut = $false
    while (-not $process.WaitForExit(10)) {
        if ($stopwatch.Elapsed.TotalSeconds -ge $TimeoutSeconds) {
            if ($process.HasExited) {
                break
            }
            $timedOut = $true
            $process.Kill()
            $process.WaitForExit()
            break
        }
        if (-not $process.HasExited) {
            $sample = Get-ChildProcessSnapshot $process
            if ($sample.working_set_bytes -gt 0) {
                $benchmarkWorkingSetAfter = [int64]$sample.working_set_bytes
            }
            if ($sample.os_peak_working_set_bytes_process_lifetime -gt $benchmarkPeakWorkingSet) {
                $benchmarkPeakWorkingSet = [int64]$sample.os_peak_working_set_bytes_process_lifetime
            }
            $benchmarkPeakSampleUtc = [DateTime]::UtcNow
        }
        if ($stopwatch.Elapsed.TotalSeconds - $lastObserverRefreshSeconds -ge 30) {
            $null = Get-HttpText $ProxyStatsUrl
            $null = Get-HttpText $UpstreamStatsUrl
            $lastObserverRefreshSeconds = $stopwatch.Elapsed.TotalSeconds
        }
    }
    $process.WaitForExit()
    $stdout = $stdoutTask.GetAwaiter().GetResult()
    $stderr = $stderrTask.GetAwaiter().GetResult()
    $stopwatch.Stop()
    $endedUtc = [DateTime]::UtcNow
    $proxyAfter = Get-ProcessSnapshot $ProxyPid
    $upstreamAfter = Get-ProcessSnapshot $UpstreamPid
    $benchmarkAfter = Get-ChildProcessSnapshot $process
    $benchmarkAfter.working_set_bytes = $benchmarkWorkingSetAfter
    $benchmarkAfter.os_peak_working_set_bytes_process_lifetime = $benchmarkPeakWorkingSet
    $exitCode = if ($timedOut) { -1 } else { $process.ExitCode }
    $process.Dispose()

    [System.IO.File]::WriteAllText($stdoutPath, $stdout, (New-Object System.Text.UTF8Encoding($false)))
    [System.IO.File]::WriteAllText($stderrPath, $stderr, (New-Object System.Text.UTF8Encoding($false)))

    $wallSeconds = $stopwatch.Elapsed.TotalSeconds
    $logicalProcessors = [int]$script:environment.logical_processors
    $processMeasurements = [ordered]@{}
    foreach ($entry in @(
        @{ name = 'proxy'; before = $proxyBefore; after = $proxyAfter },
        @{ name = 'upstream'; before = $upstreamBefore; after = $upstreamAfter },
        @{ name = 'benchmark'; before = $benchmarkBefore; after = $benchmarkAfter }
    )) {
        $cpuBefore = [double]$entry.before.cpu_seconds
        $cpuAfter = [double]$entry.after.cpu_seconds
        if ($cpuAfter -lt $cpuBefore) {
            throw "CPU time decreased for $($entry.name): before=$cpuBefore after=$cpuAfter"
        }
        $cpuDelta = $cpuAfter - $cpuBefore
        $processMeasurements[$entry.name] = [ordered]@{
            pid = $entry.before.pid
            cpu_seconds_before = $cpuBefore
            cpu_seconds_after = $cpuAfter
            cpu_delta_seconds = $cpuDelta
            wall_seconds = $wallSeconds
            cpu_one_core_percent = if ($wallSeconds -gt 0) { $cpuDelta / $wallSeconds * 100 } else { $null }
            cpu_machine_percent_normalized = if ($wallSeconds -gt 0 -and $logicalProcessors -gt 0) {
                $cpuDelta / $wallSeconds * 100 / $logicalProcessors
            } else { $null }
            working_set_before_bytes = $entry.before.working_set_bytes
            working_set_observed_bytes = $entry.after.working_set_bytes
            working_set_observation_scope = if ($entry.name -eq 'benchmark') {
                'Last nonzero live working-set sample; sampled every 10 ms before process exit'
            } else {
                'Process working-set snapshot at phase end'
            }
            os_peak_working_set_bytes_process_lifetime = $entry.after.os_peak_working_set_bytes_process_lifetime
            peak_scope = 'OS process-lifetime high-water mark; includes startup and any earlier activity'
        }
    }

    $record = [ordered]@{
        schema_version = 1
        run_id = $RunId
        experiment = $Experiment
        configuration_id = $ConfigurationId
        phase = $Phase
        timestamp_start_utc = $startedUtc.ToString('o')
        timestamp_end_utc = $endedUtc.ToString('o')
        exact_command = [ordered]@{
            executable = [System.IO.Path]::GetFullPath($BenchmarkExecutable)
            arguments = @($Arguments)
            windows_command_line = $commandLine
        }
        server_processes = $script:serverProcesses
        binary_sha256 = $script:binaryHashes
        exit_code = $exitCode
        timed_out = $timedOut
        elapsed_seconds = $wallSeconds
        elapsed_nanoseconds = [int64]($stopwatch.ElapsedTicks * 1000000000.0 / [System.Diagnostics.Stopwatch]::Frequency)
        percentile_convention = 'floor((N - 1) * p), zero-based index'
        benchmark = Get-BenchmarkMetrics $stdout
        processes = $processMeasurements
        proxy_connections = $null
        upstream_activity = $null
        environment = $script:environment
        stdout_path = $stdoutPath
        stderr_path = $stderrPath
        cpu_measurement_scope = 'Proxy/upstream deltas span only this command phase; benchmark process begins at launch.'
        benchmark_peak_working_set_last_sample_utc = $benchmarkPeakSampleUtc.ToString('o')
        benchmark_peak_working_set_poll_interval_ms = 10
    }

    return $record
}

function Append-JsonLine {
    param([object]$Record)

    $json = ConvertTo-Json -InputObject $Record -Depth 16 -Compress
    [System.IO.File]::AppendAllText($script:jsonlPath, $json + [Environment]::NewLine,
        (New-Object System.Text.UTF8Encoding($false)))
}

function Write-CollectionFailure {
    param(
        [System.Exception]$Exception,
        [string]$FailureStage
    )

    if ($script:collectionFailureWritten) {
        return
    }
    if (Test-Path -LiteralPath $script:jsonlPath) {
        foreach ($line in [System.IO.File]::ReadAllLines($script:jsonlPath)) {
            try {
                $existing = ConvertFrom-Json -InputObject $line -ErrorAction Stop
                if ($existing.record_type -eq 'collection_failure' -and $existing.run_id -eq $RunId) {
                    $script:collectionFailureWritten = $true
                    return
                }
            }
            catch {
                # An incomplete trailing line must not suppress the failure record.
            }
        }
    }

    $phaseRecord = $script:partialPhaseRecord
    $phase = if ($script:currentPhase) { $script:currentPhase } else { $null }
    $benchmarkPid = if ($phaseRecord -and $phaseRecord.processes.benchmark) {
        $phaseRecord.processes.benchmark.pid
    } else { $null }
    $metrics = [ordered]@{
        elapsed_seconds = if ($phaseRecord) { $phaseRecord.elapsed_seconds } else { $null }
        elapsed_nanoseconds = if ($phaseRecord) { $phaseRecord.elapsed_nanoseconds } else { $null }
        benchmark = if ($phaseRecord) { $phaseRecord.benchmark } else { $null }
        processes = if ($phaseRecord) { $phaseRecord.processes } else { $null }
        proxy_connections = $null
        upstream_activity = $null
    }
    $record = [ordered]@{
        record_type = 'collection_failure'
        run_id = $RunId
        experiment = $Experiment
        configuration_id = $ConfigurationId
        timestamp_utc = [DateTime]::UtcNow.ToString('o')
        phase = $phase
        failure_stage = $FailureStage
        error = [ordered]@{
            type = $Exception.GetType().FullName
            message = $Exception.Message
        }
        process_ids = [ordered]@{
            proxy = if ($ProxyPid -gt 0) { $ProxyPid } else { $null }
            upstream = if ($UpstreamPid -gt 0) { $UpstreamPid } else { $null }
            benchmark = $benchmarkPid
        }
        server_processes = $script:serverProcesses
        binary_sha256 = $script:binaryHashes
        benchmark_command = if ($phaseRecord) {
            $phaseRecord.exact_command
        } elseif ($phase -eq 'warmup') {
            [ordered]@{
                executable = $script:benchmarkPath
                arguments = @($WarmupArguments)
            }
        } elseif ($phase -eq 'measured') {
            [ordered]@{
                executable = $script:benchmarkPath
                arguments = @($BenchmarkArguments)
            }
        } else { $null }
        metrics = $metrics
        stdout_path = if ($phaseRecord) { $phaseRecord.stdout_path } else { $null }
        stderr_path = if ($phaseRecord) { $phaseRecord.stderr_path } else { $null }
    }
    Append-JsonLine $record
    $script:collectionFailureWritten = $true
}

function Get-HttpText {
    param([string]$Url)

    return $script:httpClient.GetStringAsync($Url).GetAwaiter().GetResult()
}

function Reset-HttpStats {
    param([string]$Url)

    $content = New-Object -TypeName System.Net.Http.StringContent -ArgumentList ''
    try {
        $response = $script:httpClient.PostAsync($Url, $content).GetAwaiter().GetResult()
        try {
            if (-not $response.IsSuccessStatusCode) {
                throw "Stats reset returned HTTP $([int]$response.StatusCode) for $Url"
            }
        }
        finally {
            $response.Dispose()
        }
    }
    finally {
        $content.Dispose()
    }
}

function Add-StatsToRecord {
    param(
        [object]$Record,
        [hashtable]$ProxyBefore,
        [hashtable]$ProxyAfter,
        [hashtable]$UpstreamBefore,
        [hashtable]$UpstreamAfter
    )

    $proxyBaselineOpen = Get-StatValue $ProxyBefore 'inbound_connections_open'
    $proxyPeak = Get-StatValue $ProxyAfter 'inbound_connections_peak'
    $Record.proxy_connections = [ordered]@{
        accepted_total_before = Get-StatValue $ProxyBefore 'inbound_connections_accepted_total'
        accepted_total_after = Get-StatValue $ProxyAfter 'inbound_connections_accepted_total'
        accepted_delta = (Get-StatValue $ProxyAfter 'inbound_connections_accepted_total') -
            (Get-StatValue $ProxyBefore 'inbound_connections_accepted_total')
        open_before = $proxyBaselineOpen
        open_after = Get-StatValue $ProxyAfter 'inbound_connections_open'
        peak_open_including_persistent_stats_observer = $proxyPeak
        peak_open_above_baseline = if ($null -ne $proxyPeak -and $null -ne $proxyBaselineOpen) {
            [math]::Max(0, $proxyPeak - $proxyBaselineOpen)
        } else { $null }
        requests_received = Get-StatValue $ProxyAfter 'requests_received'
        proxy_errors = Get-StatValue $ProxyAfter 'proxy_errors'
        timeout_errors = Get-StatValue $ProxyAfter 'proxy_timeout_errors'
        other_upstream_errors = Get-StatValue $ProxyAfter 'proxy_other_upstream_errors'
        response_processing_errors = Get-StatValue $ProxyAfter 'proxy_response_processing_errors'
    }
    $Record.upstream_activity = [ordered]@{
        accepted_connections_before = Get-StatValue $UpstreamBefore 'accepted_connections'
        accepted_connections_after = Get-StatValue $UpstreamAfter 'accepted_connections'
        accepted_connections_delta = (Get-StatValue $UpstreamAfter 'accepted_connections') -
            (Get-StatValue $UpstreamBefore 'accepted_connections')
        active_delay_requests_after = Get-StatValue $UpstreamAfter 'active_delay_requests'
        peak_delay_requests = Get-StatValue $UpstreamAfter 'peak_delay_requests'
        active_payload_requests_after = Get-StatValue $UpstreamAfter 'active_payload_requests'
        peak_payload_requests = Get-StatValue $UpstreamAfter 'peak_payload_requests'
    }
}

if (-not (Test-Path -LiteralPath $BenchmarkExecutable -PathType Leaf)) {
    throw "Benchmark executable not found: $BenchmarkExecutable"
}
if (-not (Test-Path -LiteralPath $OutputDirectory -PathType Container)) {
    [void](New-Item -ItemType Directory -Path $OutputDirectory -Force)
}
$BenchmarkExecutable = [System.IO.Path]::GetFullPath($BenchmarkExecutable)
$script:jsonlPath = Join-Path $OutputDirectory ($RunId + '.jsonl')
if (Test-Path -LiteralPath $script:jsonlPath) {
    throw "Refusing to append to existing run file: $script:jsonlPath"
}

$script:benchmarkPath = $BenchmarkExecutable
$script:serverProcesses = [ordered]@{
    proxy = [ordered]@{ pid = if ($ProxyPid -gt 0) { $ProxyPid } else { $null }; executable = $null; command_line = $null }
    upstream = [ordered]@{ pid = if ($UpstreamPid -gt 0) { $UpstreamPid } else { $null }; executable = $null; command_line = $null }
}
$script:binaryHashes = [ordered]@{
    benchmark_sha256 = $null
    proxy_sha256 = $null
    upstream_sha256 = $null
}
$script:currentPhase = $null
$script:currentFailureStage = 'preflight'
$script:partialPhaseRecord = $null
$script:collectionFailureWritten = $false
$script:httpClient = $null
$handler = $null

try {
$proxyProcess = Get-CimInstance Win32_Process -Filter ("ProcessId = {0}" -f $ProxyPid)
if (-not $proxyProcess) {
    throw "Could not read proxy process metadata for PID $ProxyPid from Win32_Process"
}
$script:serverProcesses.proxy = [ordered]@{
    pid = $ProxyPid
    executable = $proxyProcess.ExecutablePath
    command_line = $proxyProcess.CommandLine
}
$script:binaryHashes.benchmark_sha256 = (Get-FileHash -LiteralPath $BenchmarkExecutable -Algorithm SHA256).Hash
$script:binaryHashes.proxy_sha256 = (Get-FileHash -LiteralPath $proxyProcess.ExecutablePath -Algorithm SHA256).Hash

$upstreamProcess = Get-CimInstance Win32_Process -Filter ("ProcessId = {0}" -f $UpstreamPid)
if (-not $upstreamProcess) {
    throw "Could not read upstream process metadata for PID $UpstreamPid from Win32_Process"
}
$script:serverProcesses.upstream = [ordered]@{
    pid = $UpstreamPid
    executable = $upstreamProcess.ExecutablePath
    command_line = $upstreamProcess.CommandLine
}
$script:binaryHashes.upstream_sha256 = (Get-FileHash -LiteralPath $upstreamProcess.ExecutablePath -Algorithm SHA256).Hash

$os = Get-CimInstance Win32_OperatingSystem
$processor = Get-CimInstance Win32_Processor | Select-Object -First 1
$script:environment = [ordered]@{
    windows_caption = $os.Caption
    windows_version = $os.Version
    windows_build = $os.BuildNumber
    cpu_model = $processor.Name
    logical_processors = [int]$processor.NumberOfLogicalProcessors
    physical_cores = [int]$processor.NumberOfCores
    physical_memory_bytes = [int64]$os.TotalVisibleMemorySize * 1024
    powershell_version = $PSVersionTable.PSVersion.ToString()
    go_version = (& go version 2>&1 | Out-String).Trim()
}

Add-Type -AssemblyName System.Net.Http
$handler = New-Object System.Net.Http.HttpClientHandler
$handler.UseProxy = $false
$script:httpClient = New-Object -TypeName System.Net.Http.HttpClient -ArgumentList (,$handler)
$script:httpClient.Timeout = [TimeSpan]::FromSeconds(10)
    $script:currentFailureStage = 'initial_stats'
    $proxyCounters = @(
        'requests_received', 'proxy_errors', 'proxy_timeout_errors',
        'proxy_other_upstream_errors', 'proxy_response_processing_errors',
        'inbound_connections_accepted_total', 'inbound_connections_open',
        'inbound_connections_peak'
    )
    $upstreamCounters = @(
        'accepted_connections', 'active_delay_requests', 'peak_delay_requests',
        'active_payload_requests', 'peak_payload_requests'
    )
    $proxyInitial = Get-StatsSnapshot $ProxyStatsUrl 'proxy' $proxyCounters
    $upstreamInitial = Get-StatsSnapshot $UpstreamStatsUrl 'upstream' $upstreamCounters

    if ($WarmupArguments -and $WarmupArguments.Count -gt 0) {
        $script:currentPhase = 'warmup'
        $script:currentFailureStage = 'warmup_phase'
        $warmupRecord = Invoke-BenchmarkPhase 'warmup' $WarmupArguments
        $script:partialPhaseRecord = $warmupRecord
        $script:currentFailureStage = 'warmup_post_stats'
        $proxyWarmup = Get-StatsSnapshot $ProxyStatsUrl 'proxy' $proxyCounters
        $upstreamWarmup = Get-StatsSnapshot $UpstreamStatsUrl 'upstream' $upstreamCounters
        Add-StatsToRecord $warmupRecord $proxyInitial $proxyWarmup $upstreamInitial $upstreamWarmup
        Append-JsonLine $warmupRecord
        $script:partialPhaseRecord = $null
        if ($warmupRecord.exit_code -ne 0) {
            $script:currentFailureStage = 'benchmark_exit'
            throw "Warm-up failed with exit code $($warmupRecord.exit_code). See $($warmupRecord.stdout_path) and $($warmupRecord.stderr_path)"
        }
    }

    $script:currentPhase = 'measured'
    $script:currentFailureStage = 'stats_reset'
    Reset-HttpStats $ProxyStatsResetUrl
    Reset-HttpStats $UpstreamStatsResetUrl
    $script:currentFailureStage = 'measured_pre_stats'
    $proxyBefore = Get-StatsSnapshot $ProxyStatsUrl 'proxy' $proxyCounters
    $upstreamBefore = Get-StatsSnapshot $UpstreamStatsUrl 'upstream' $upstreamCounters
    $script:currentFailureStage = 'measured_phase'
    $measurementRecord = Invoke-BenchmarkPhase 'measured' $BenchmarkArguments
    $script:partialPhaseRecord = $measurementRecord
    $script:currentFailureStage = 'measured_post_stats'
    $proxyAfter = Get-StatsSnapshot $ProxyStatsUrl 'proxy' $proxyCounters
    $upstreamAfter = Get-StatsSnapshot $UpstreamStatsUrl 'upstream' $upstreamCounters
    Add-StatsToRecord $measurementRecord $proxyBefore $proxyAfter $upstreamBefore $upstreamAfter
    Append-JsonLine $measurementRecord
    $script:partialPhaseRecord = $null
    if ($measurementRecord.exit_code -ne 0) {
        $script:currentFailureStage = 'benchmark_exit'
        throw "Measured phase failed with exit code $($measurementRecord.exit_code). See $($measurementRecord.stdout_path) and $($measurementRecord.stderr_path)"
    }

    Write-Output ("JSONL: " + $script:jsonlPath)
    Write-Output ("Measured stdout: " + $measurementRecord.stdout_path)
}
catch {
    $caughtError = $_.Exception
    if ($script:currentFailureStage -ne 'benchmark_exit') {
        Write-CollectionFailure $caughtError $script:currentFailureStage
    }
    throw
}
finally {
    if ($script:httpClient) {
        $script:httpClient.Dispose()
    }
    if ($handler) {
        $handler.Dispose()
    }
}
