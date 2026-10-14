# EdgeLab

A measurable HTTP reverse-proxy engineering lab. The first milestone is deliberately small:
a working proxy with explicit timeouts, request/response accounting, and a repeatable load
generator. We will optimize only after establishing a baseline.

## Requirements
- Go 1.22+
- A local HTTP server to act as the upstream (the included demo server is provided)

## Run locally (PowerShell or Bash)

Both servers bind to IPv4 loopback by default (`127.0.0.1:18080` for the proxy and
`127.0.0.1:19090` for the demo upstream). The upstream failure-control endpoint is for local
experiments only; the proxy returns 404 for that reserved path rather than forwarding it, and the
upstream itself accepts control requests only from loopback clients.

Terminal 1 — start the demo upstream:
```sh
go run ./cmd/upstream
```

Terminal 2 — start EdgeLab:
```sh
go run ./cmd/edgelab
```

Terminal 3 — send a request through the proxy:
```sh
curl http://localhost:18080/hello
```

## Run a benchmark

```sh
go run ./cmd/bench -url http://localhost:18080/hello -n 10000 -c 50
```

The benchmark prints its effective client and transport configuration with the results. Defaults
preserve Go's standard HTTP transport pool settings: 100 idle connections total, 2 idle connections
per host, no per-host active connection limit, and a 90-second idle connection timeout. Other
defaults are a 5-second whole-request timeout, 30-second dial timeout, 30-second TCP keep-alive,
10-second TLS handshake timeout, no response-header timeout, and a 1-second
`Expect: 100-continue` timeout. Configure these independently with `-timeout`, `-dial-timeout`,
`-keep-alive`, `-tls-handshake-timeout`, `-response-header-timeout`, `-expect-continue-timeout`,
`-idle-conn-timeout`, `-max-idle-conns`, `-max-idle-conns-per-host`, `-max-conns-per-host`, and
`-force-http2`.
These are benchmark-client settings; they do not change the proxy's separately configured
upstream transport. The benchmark's default idle pool (100 total / 2 per host) intentionally
matches Go's default client transport, not the proxy's configured pool (1000 total / 100 per host).
To match the proxy pool for a controlled experiment, pass
`-max-idle-conns 1000 -max-idle-conns-per-host 100`.

### Measurement boundaries

- Per-request latency begins when a worker starts a request, after receiving its job. It includes
  the HTTP client request, connection acquisition/setup as needed, response headers, consuming the
  complete response body, and closing the response body. It excludes time waiting in the job
  channel and time the producer spends dispatching other jobs.
- Total benchmark duration starts before worker creation and job dispatch, and ends after all
  workers finish. It includes worker setup, dispatch/backpressure, request execution, scheduling,
  and waiting for worker completion. It excludes argument parsing and client/transport construction.
- Latencies are sorted and the percentile is selected at zero-based index
  `floor((N - 1) * p)`. This lower-index convention can differ from nearest-rank for some sample
  counts; for example, with four observations p50 selects index 1 (the second value).
- `Errors` counts failed requests once each. Errors returned by `http.Client.Get` (including
  transport errors), body-read/close errors, and HTTP status errors (status >= 400) are also
  reported by category. A request can belong to more than one category. Every available response
  body is read to EOF and closed.

The earlier concurrency-1 run that printed p50 as `0s` was not reproduced by the subsequent
single-worker repeats. A printed `0s` represents a zero duration, not a sub-second duration rounded
to seconds. Its cause is unknown; retain the raw output and rerun the same command before drawing
conclusions. Run comparisons with identical URLs, flags, environment, and server state.

## Experiment 1: proxy-to-upstream connection reuse

The proxy accepts `-upstream-keep-alives=true|false` (default `true`, preserving the existing
behavior). `true` leaves the explicit upstream transport pool unchanged. `false` changes only
`http.Transport.DisableKeepAlives` from `false` to `true`; the transport still has
`MaxIdleConns=1000`, `MaxIdleConnsPerHost=100`, `MaxConnsPerHost=0`, `IdleConnTimeout=90s`,
`TLSHandshakeTimeout=5s`, `ResponseHeaderTimeout=2s`, and `ExpectContinueTimeout=1s`. Go then
does not reuse persistent upstream connections. This option controls proxy-to-upstream traffic,
not benchmark-client connections.

The demo upstream counts accepted TCP connections in `ConnState` and exposes the cumulative count
at `/stats`. `go test ./cmd/edgelab -run TestUpstreamKeepAliveControlsConnectionReuse -count=1`
checks five requests per mode: keep-alives enabled accepts one connection; disabled accepts five.
The experiment measures the accepted-connection delta during each interval separately from its
100-request warm-up. The stats reader's own newly accepted TCP connection is subtracted from each
delta.

### Local experiment run (2026-10-12)

Both server binaries and the benchmark binary were built once and reused. Each mode used the same
upstream process, `/hello` handler, `-n 1000 -c 10`, benchmark-client defaults listed above, and a
100-request / concurrency-10 warm-up before every measured repetition. No load tests ran
simultaneously. Repetitions were grouped by mode rather than interleaved; this leaves possible
time/order effects. The isolated proxy listened on port 18081 and the instrumented upstream on
19091; the application path and all other settings were identical. Existing listeners on the
default ports were left untouched.

Raw repetitions (latencies are the benchmark's reported percentile values):

| Reuse | Rep | Throughput req/s | p50 | p95 | p99 | Success | Errors (client/body/status) | Warm-up accepted | New accepted during measurement |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| enabled | 1 | 5494.26 | 1.3221 ms | 3.9984 ms | 6.2054 ms | 1000 | 0 (0/0/0) | 10 | 0 |
| enabled | 2 | 4506.59 | 1.7316 ms | 5.1300 ms | 8.8899 ms | 1000 | 0 (0/0/0) | 0 | 0 |
| enabled | 3 | 3828.58 | 1.9635 ms | 5.9934 ms | 8.8835 ms | 1000 | 0 (0/0/0) | 0 | 0 |
| enabled | 4 | 4097.81 | 1.8601 ms | 5.5682 ms | 8.7280 ms | 1000 | 0 (0/0/0) | 0 | 0 |
| enabled | 5 | 3867.15 | 1.8210 ms | 5.7122 ms | 12.5369 ms | 1000 | 0 (0/0/0) | 0 | 0 |
| disabled | 1 | 1771.63 | 5.0508 ms | 9.4187 ms | 15.6631 ms | 1000 | 0 (0/0/0) | 100 | 1000 |
| disabled | 2 | 1703.95 | 5.3673 ms | 10.0984 ms | 13.8125 ms | 1000 | 0 (0/0/0) | 100 | 1000 |
| disabled | 3 | 1539.01 | 5.9396 ms | 11.1741 ms | 18.1999 ms | 1000 | 0 (0/0/0) | 100 | 1000 |
| disabled | 4 | 1867.03 | 4.8336 ms | 9.2858 ms | 12.4125 ms | 1000 | 0 (0/0/0) | 100 | 1000 |
| disabled | 5 | 1849.43 | 5.0770 ms | 9.0625 ms | 12.3260 ms | 1000 | 0 (0/0/0) | 100 | 1000 |

| Metric | Enabled median | Disabled median | Disabled vs enabled |
|---|---:|---:|---:|
| Throughput | 4097.81 req/s | 1771.63 req/s | -56.77% |
| p50 | 1.8210 ms | 5.0770 ms | +178.80% |
| p95 | 5.5682 ms | 9.4187 ms | +69.15% |
| p99 | 8.8835 ms | 13.8125 ms | +55.48% |
| Newly accepted upstream connections per measured 1000 requests | 0 | 1000 | Not a meaningful percentage comparison |

Each measured request succeeded and all client, body, and HTTP-status error counts were zero.
The observed accepted-connection counts verify that the toggle changes upstream connection reuse.
They do not by themselves establish why latency/throughput differ; this local single-machine result
is one experiment, with mode order grouped and no interleaved repetitions. Windows process CPU
accounting reported zero at its available resolution for these short runs, so CPU is omitted.
Working-set snapshots were about 22.9–24.6 MB for the enabled proxy and 16.3–18.7 MB for the
disabled proxy; these are sampled values, not peak memory, and are not evidence that disabling
reuse reduces memory.

The raw benchmark output, repetitions, summaries, and relevant server logs are included in
[`measurements/exp1-connection-reuse/`](./measurements/exp1-connection-reuse/).

## Experiment 2: upstream concurrency limits

### Hypothesis and implementation

Go's `http.Transport.MaxConnsPerHost` limits the total connections per host across dialing,
active, and idle states. When all connection slots are occupied, additional RoundTrip requests
wait for a slot; the transport does not reject them merely because the limit was reached. The
proxy exposes this one setting as `-upstream-max-conns-per-host=N`. `N=0` retains Go's existing
unlimited behavior and is still the default. Negative values are rejected. Upstream keep-alives
remain enabled for every configuration; the separate `-upstream-keep-alives` option remains
unchanged.

Automated coverage verifies that the configured `MaxConnsPerHost` reaches the transport, a
negative value is rejected, default 0 and keep-alives enabled are preserved, and concurrent
requests finish successfully while a local upstream's observed peak active handler count reaches
but never exceeds a limit of 2.

### Method

The expected behavior is queueing/backpressure at low limits: with fixed client concurrency 50,
the upstream `/delay?ms=100` endpoint holds each active request for 100 ms. Limits 5, 10, 25,
and 100 expose progressively more upstream parallelism; limit 0 is included as the unchanged
unlimited default reference. `/hello` is used after the delayed workload to sample recovery with
the same limit still configured.

For every configuration, the same built upstream, proxy, and benchmark binaries were used with
one upstream process and the same paths. Proxy instances listened on isolated ports but forwarded
to the same upstream at `http://localhost:19100`. Every measured and recovery benchmark used 300
requests, concurrency 50, a 5-second client timeout, the same benchmark-client transport settings,
and a 50-request/concurrency-50 slow-path warm-up before each measured repetition. Five
measured slow-path repetitions were run sequentially per setting, followed by one 300-request
`/hello` recovery run. The configuration order was 0, 5, 10, 25, 100; repetitions were not
interleaved, so order/time effects remain possible. No load tests ran simultaneously.

The proxy transport settings other than `MaxConnsPerHost` were fixed: keep-alives on,
`MaxIdleConns=1000`, `MaxIdleConnsPerHost=100`, `IdleConnTimeout=90s`,
`TLSHandshakeTimeout=5s`, `ResponseHeaderTimeout=2s`, and `ExpectContinueTimeout=1s`.
Benchmark-client settings were fixed at `ProxyFromEnvironment`, HTTP/2 attempts on, maximum idle
connections 100 total / 2 per host, `MaxConnsPerHost=0`, idle timeout 90s, dial and TCP
keep-alive 30s, TLS handshake timeout 10s, no response-header timeout, expect-continue timeout
1s, and 5s whole-request timeout.

The binaries were built once and reused:

```powershell
go build -o $env:TEMP\edgelab-upstream-limit-20261013\upstream.exe .\cmd\upstream
go build -o $env:TEMP\edgelab-upstream-limit-20261013\edgelab.exe .\cmd\edgelab
go build -o $env:TEMP\edgelab-upstream-limit-20261013\bench.exe .\cmd\bench
```

The proxy invocation template was
`edgelab.exe -listen=:PORT -upstream=http://localhost:19100 -upstream-keep-alives=true -upstream-max-conns-per-host=LIMIT`.
Limits `0`, `5`, `10`, `25`, and `100` each ran on their own proxy port, all forwarding to the
same upstream process. Before each warm-up and measured interval, `POST
http://localhost:19100/stats/reset` reset instrumentation. The benchmark command template was:

```powershell
bench.exe -url http://localhost:PORT/delay?ms=100 -n 300 -c 50 -timeout=5s `
  -force-http2=true -max-idle-conns=100 -max-idle-conns-per-host=2 `
  -max-conns-per-host=0 -idle-conn-timeout=90s -dial-timeout=30s `
  -keep-alive=30s -tls-handshake-timeout=10s `
  -response-header-timeout=0s -expect-continue-timeout=1s
```

For each repetition, the same command was run first with URL
`http://localhost:PORT/delay?ms=100` and `-n 50` as warm-up, then `-n 300` as the measured run.
Recovery used the same flags with URL `http://localhost:PORT/hello` and `-n 300`. The HTTP path was
changed only for the recovery sample; connection limit and all client flags remained fixed.

The demo upstream records total accepted TCP connections, current active delay requests, and peak
active delay requests at `/stats`. `POST /stats/reset` resets the accepted counter and delay peak
between measured intervals without closing the proxy's pooled connections. The stats query itself
opens one TCP connection; accepted-connection interval counts below subtract that stats probe.
Warm-up accepts are reported separately. Peak active delay requests are reset per interval.

### Raw measured slow-path results (2026-10-13)

Each row is one actual 300-request run. Error columns are total failed requests and the
client/body/status categories. All rows completed with 300 successes and zero errors in every
category.

| Limit | Rep | Throughput req/s | p50 | p95 | p99 | Success / errors (client/body/status) | Warm-up accepted | Measured accepted | Peak active delay requests |
|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| 0 (unlimited) | 1 | 427.27 | 108.3813 ms | 135.5789 ms | 138.4047 ms | 300 / 0 (0/0/0) | 50 | 0 | 50 |
| 0 (unlimited) | 2 | 429.04 | 106.7485 ms | 145.3968 ms | 145.3968 ms | 300 / 0 (0/0/0) | 0 | 0 | 50 |
| 0 (unlimited) | 3 | 420.69 | 111.4161 ms | 138.4762 ms | 140.2136 ms | 300 / 0 (0/0/0) | 0 | 0 | 50 |
| 0 (unlimited) | 4 | 435.88 | 103.3464 ms | 137.2898 ms | 138.8693 ms | 300 / 0 (0/0/0) | 0 | 0 | 50 |
| 0 (unlimited) | 5 | 409.75 | 104.9029 ms | 146.5979 ms | 148.3572 ms | 300 / 0 (0/0/0) | 0 | 0 | 50 |
| 5 | 1 | 48.82 | 1.016533 s | 1.0205938 s | 1.0558134 s | 300 / 0 (0/0/0) | 5 | 0 | 5 |
| 5 | 2 | 48.79 | 1.0194492 s | 1.0241313 s | 1.0477628 s | 300 / 0 (0/0/0) | 0 | 0 | 5 |
| 5 | 3 | 48.94 | 1.0163347 s | 1.0218365 s | 1.0443784 s | 300 / 0 (0/0/0) | 0 | 0 | 5 |
| 5 | 4 | 48.92 | 1.014628 s | 1.0414382 s | 1.043202 s | 300 / 0 (0/0/0) | 0 | 0 | 5 |
| 5 | 5 | 48.95 | 1.0146352 s | 1.022855 s | 1.0449795 s | 300 / 0 (0/0/0) | 0 | 0 | 5 |
| 10 | 1 | 97.32 | 508.0479 ms | 512.7386 ms | 537.1803 ms | 300 / 0 (0/0/0) | 10 | 0 | 10 |
| 10 | 2 | 97.15 | 508.2779 ms | 516.5164 ms | 531.6188 ms | 300 / 0 (0/0/0) | 0 | 0 | 10 |
| 10 | 3 | 96.43 | 507.8365 ms | 528.4861 ms | 561.2538 ms | 300 / 0 (0/0/0) | 0 | 0 | 10 |
| 10 | 4 | 97.57 | 508.6703 ms | 514.5585 ms | 524.8544 ms | 300 / 0 (0/0/0) | 0 | 0 | 10 |
| 10 | 5 | 97.31 | 509.8367 ms | 517.3083 ms | 530.7182 ms | 300 / 0 (0/0/0) | 0 | 0 | 10 |
| 25 | 1 | 234.10 | 203.3761 ms | 238.0757 ms | 241.3864 ms | 300 / 0 (0/0/0) | 25 | 0 | 25 |
| 25 | 2 | 236.91 | 204.0662 ms | 227.8784 ms | 227.8784 ms | 300 / 0 (0/0/0) | 0 | 0 | 25 |
| 25 | 3 | 236.54 | 203.7381 ms | 231.8967 ms | 236.0650 ms | 300 / 0 (0/0/0) | 0 | 0 | 25 |
| 25 | 4 | 235.76 | 203.6649 ms | 234.7495 ms | 237.4160 ms | 300 / 0 (0/0/0) | 0 | 0 | 25 |
| 25 | 5 | 237.44 | 203.6848 ms | 233.2446 ms | 239.2490 ms | 300 / 0 (0/0/0) | 0 | 0 | 25 |
| 100 | 1 | 414.85 | 103.3282 ms | 175.6443 ms | 175.6443 ms | 300 / 0 (0/0/0) | 50 | 0 | 50 |
| 100 | 2 | 407.69 | 105.3317 ms | 150.1258 ms | 159.3948 ms | 300 / 0 (0/0/0) | 0 | 0 | 50 |
| 100 | 3 | 426.80 | 104.0423 ms | 137.8754 ms | 145.5470 ms | 300 / 0 (0/0/0) | 0 | 0 | 50 |
| 100 | 4 | 424.44 | 108.3560 ms | 129.5148 ms | 134.5578 ms | 300 / 0 (0/0/0) | 0 | 0 | 50 |
| 100 | 5 | 427.78 | 105.7741 ms | 145.9339 ms | 145.9339 ms | 300 / 0 (0/0/0) | 0 | 0 | 50 |

### Medians and differences from the unchanged unlimited default

Percent differences are relative to limit 0's median: throughput is
`(limited - unlimited) / unlimited`; latency is calculated the same way. These are observed
differences in this local setup, not universal performance claims.

| Limit | Median throughput req/s | vs unlimited | Median p50 | vs unlimited | Median p95 | vs unlimited | Median p99 | vs unlimited | Median peak active |
|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| 0 (unlimited) | 427.27 | baseline | 106.7485 ms | baseline | 138.4762 ms | baseline | 140.2136 ms | baseline | 50 |
| 5 | 48.92 | -88.55% | 1016.3347 ms | +852.31% | 1022.8550 ms | +638.98% | 1044.9795 ms | +645.43% | 5 |
| 10 | 97.31 | -77.23% | 508.2779 ms | +376.25% | 516.5164 ms | +272.99% | 531.6188 ms | +279.13% | 10 |
| 25 | 236.54 | -44.64% | 203.6848 ms | +90.81% | 233.2446 ms | +68.43% | 237.4160 ms | +69.33% | 25 |
| 100 | 424.44 | -0.66% | 105.3317 ms | -1.33% | 145.9339 ms | +5.38% | 145.9339 ms | +4.08% | 50 |

### Recovery samples

These are one recovery run per configured limit, 300 requests at concurrency 50 to `/hello`
after the delayed repetitions. They are samples, not medians.

| Limit | Throughput req/s | p50 | p95 | p99 | Success / errors (client/body/status) |
|---:|---:|---:|---:|---:|---:|
| 0 (unlimited) | 4715.81 | 4.0759 ms | 26.5828 ms | 30.8000 ms | 300 / 0 (0/0/0) |
| 5 | 4029.53 | 3.9506 ms | 41.7877 ms | 46.9141 ms | 300 / 0 (0/0/0) |
| 10 | 4223.10 | 2.8843 ms | 34.6365 ms | 44.1078 ms | 300 / 0 (0/0/0) |
| 25 | 3227.64 | 3.4698 ms | 45.3209 ms | 53.6936 ms | 300 / 0 (0/0/0) |
| 100 | 3406.72 | 2.8401 ms | 51.8443 ms | 58.7049 ms | 300 / 0 (0/0/0) |

All sampled proxy-process CPU percentages rounded to 0% at the available process CPU-time
resolution; CPU is not usable for comparison here. Proxy working-set snapshots during slow runs ranged from
17.2 to 20.2 MB across samples and are not peak-memory measurements. The low-limit results show
queueing under this fixed 100 ms service-time workload and concurrency 50; connection counts and
peak active requests verify the bound was exercised. The one-shot recovery tails were noisy and
do not support a ranking of limits for recovery. Results are specific to this local Windows
machine and small workload.

Raw benchmark outputs, summaries, and the final proxy/upstream logs are included in
[`measurements/exp2-upstream-limits/`](./measurements/exp2-upstream-limits/).

## Initial architecture

- `cmd/upstream`: deterministic local HTTP origin server.
- `cmd/edgelab`: reverse proxy with upstream timeout, basic counters, configurable upstream
  keep-alive reuse, and a configurable active upstream connection limit.
- `cmd/bench`: concurrent load generator with latency percentiles.

## Experiment 3: bounded response buffering vs. streaming

### Implementation and trade-offs

The default response path is Go's `httputil.ReverseProxy` streaming copy path. With
`-response-buffer-limit=0` (the default), EdgeLab does not buffer the upstream response body.
The opt-in `-response-buffer-limit=1048576` mode reads responses up to 1 MiB completely before
forwarding them. For larger or unknown-length responses, it reads a 1 MiB prefix plus one
overflow-detection byte, then replays that prefix and streams the remainder. Status, headers,
body bytes, and request-context cancellation are preserved. The response data retained before
downstream copying is bounded to the configured limit plus the one-byte overflow sentinel; this
does not cap aggregate memory across concurrent requests.

Small responses incur full-body read latency before the downstream response begins, but the
upstream response is no longer coupled to a slow downstream once buffering completes. Oversized
responses still stream after the bounded prefix, so this mode does not decouple their remaining
upstream transfer from a slow downstream. Buffering is opt-in and does not change either proxy
transport configuration or the existing Experiment 1/2 settings.

Automated tests cover a response exactly at the cap (status, custom header, content length, and
body), an over-limit response (cap-plus-one pre-read, status/header/content-length preservation,
body integrity, and upstream body close), cancellation while buffering, and downstream
disconnection while streaming. `go test ./cmd/edgelab -run TestBoundedResponseBuffer -count=10`
was also run to repeat the cancellation and body-handling tests.

### Method and settings

The same built `upstream.exe`, `edgelab.exe`, and `bench.exe` were reused for all measurements.
One instrumented upstream process ran at `http://localhost:19110`; the streaming proxy listened
on port 18091 and the bounded-buffer proxy on 18092. Both used the same upstream URL,
`-upstream-keep-alives=true`, `-upstream-max-conns-per-host=0`, and all other proxy transport
defaults unchanged. The only proxy configuration difference was
`-response-buffer-limit=0` versus `-response-buffer-limit=1048576`.

Each sample used `-n 10 -c 4 -timeout=15s`, with benchmark client settings fixed to
`-force-http2=true -max-idle-conns=100 -max-idle-conns-per-host=2 -max-conns-per-host=0
-idle-conn-timeout=90s -dial-timeout=30s -keep-alive=30s -tls-handshake-timeout=10s
-response-header-timeout=0s -expect-continue-timeout=1s`. The read chunk size was 1024 bytes.
Scenarios were `/payload/64k`, `/payload/4m`, and `/payload/64k` with a 250 µs delay after each
read to simulate a slow downstream consumer. That delay is imposed by the benchmark client and
is included in request latency.

Every measured repetition had an identical 8-request, concurrency-4 warm-up on the same proxy
and scenario. Five measured repetitions were run sequentially for every scenario/configuration.
Within each scenario, streaming ran first on odd repetitions and buffering first on even
repetitions. No benchmark loads ran simultaneously. The upstream accepted-connection counter was
reset after warm-up; it read as 1 after each measured run because the subsequent `/stats` probe
itself opened one TCP connection. Thus the measured workload opened no new upstream connection
after warm-up. The upstream's recorded peak active payload requests was 3–4 for these runs.

Latency percentiles use the benchmark's existing lower-index rule,
`floor((N - 1) * p)`. With only 10 requests per repetition, p95 and p99 select the same
observation (index 8); these samples therefore provide weak tail-latency evidence. The observed
medians are descriptive of this local Windows laptop and small workload, not a general performance
claim.

### Raw measured results

All rows had 10 successful requests, 0 total errors, and 0 client / body-read-or-close / HTTP
status errors. The peak column is the upstream's observed active payload-handler count.

| Scenario | Mode | Rep | Throughput req/s | p50 | p95 | p99 | Peak active |
|---|---|---:|---:|---:|---:|---:|---:|
| 64 KiB | stream | 1 | 455.35 | 3.0176 ms | 16.5719 ms | 16.5719 ms | 4 |
| 64 KiB | stream | 2 | 581.55 | 2.6636 ms | 13.4089 ms | 13.4089 ms | 4 |
| 64 KiB | stream | 3 | 759.20 | 0.5393 ms | 12.0999 ms | 12.0999 ms | 3 |
| 64 KiB | stream | 4 | 806.51 | 1.0967 ms | 10.2044 ms | 10.2044 ms | 4 |
| 64 KiB | stream | 5 | 513.69 | 1.8076 ms | 16.0565 ms | 16.0565 ms | 4 |
| 64 KiB | buffer 1 MiB | 1 | 468.47 | 1.6432 ms | 17.9004 ms | 17.9004 ms | 3 |
| 64 KiB | buffer 1 MiB | 2 | 820.51 | 1.0877 ms | 10.5769 ms | 10.5769 ms | 4 |
| 64 KiB | buffer 1 MiB | 3 | 483.39 | 2.6099 ms | 16.7558 ms | 16.7558 ms | 3 |
| 64 KiB | buffer 1 MiB | 4 | 785.25 | 1.6326 ms | 10.5674 ms | 10.5674 ms | 4 |
| 64 KiB | buffer 1 MiB | 5 | 463.96 | 2.3877 ms | 18.0891 ms | 18.0891 ms | 4 |
| 4 MiB | stream | 1 | 165.01 | 13.1870 ms | 27.7297 ms | 27.7297 ms | 4 |
| 4 MiB | stream | 2 | 132.90 | 23.2840 ms | 42.6738 ms | 42.6738 ms | 4 |
| 4 MiB | stream | 3 | 235.91 | 11.4481 ms | 23.5097 ms | 23.5097 ms | 4 |
| 4 MiB | stream | 4 | 133.60 | 17.4465 ms | 31.0201 ms | 31.0201 ms | 4 |
| 4 MiB | stream | 5 | 227.93 | 15.6576 ms | 23.5018 ms | 23.5018 ms | 4 |
| 4 MiB | buffer 1 MiB | 1 | 181.43 | 17.1891 ms | 27.1800 ms | 27.1800 ms | 4 |
| 4 MiB | buffer 1 MiB | 2 | 145.80 | 20.2652 ms | 31.2990 ms | 31.2990 ms | 4 |
| 4 MiB | buffer 1 MiB | 3 | 197.73 | 15.3572 ms | 27.7497 ms | 27.7497 ms | 4 |
| 4 MiB | buffer 1 MiB | 4 | 148.68 | 19.8912 ms | 32.6628 ms | 32.6628 ms | 4 |
| 4 MiB | buffer 1 MiB | 5 | 131.55 | 17.0118 ms | 49.4855 ms | 49.4855 ms | 4 |
| 64 KiB, slow reader | stream | 1 | 62.59 | 46.8842 ms | 66.8838 ms | 66.8838 ms | 4 |
| 64 KiB, slow reader | stream | 2 | 63.01 | 52.7414 ms | 56.9510 ms | 56.9510 ms | 4 |
| 64 KiB, slow reader | stream | 3 | 66.91 | 48.7634 ms | 56.1030 ms | 56.1030 ms | 4 |
| 64 KiB, slow reader | stream | 4 | 70.17 | 44.0593 ms | 56.7325 ms | 56.7325 ms | 4 |
| 64 KiB, slow reader | stream | 5 | 70.11 | 46.9886 ms | 52.0296 ms | 52.0296 ms | 4 |
| 64 KiB, slow reader | buffer 1 MiB | 1 | 66.22 | 50.4938 ms | 57.0217 ms | 57.0217 ms | 3 |
| 64 KiB, slow reader | buffer 1 MiB | 2 | 70.56 | 45.7127 ms | 52.7909 ms | 52.7909 ms | 3 |
| 64 KiB, slow reader | buffer 1 MiB | 3 | 70.88 | 45.2819 ms | 54.9528 ms | 54.9528 ms | 4 |
| 64 KiB, slow reader | buffer 1 MiB | 4 | 67.39 | 41.6536 ms | 69.8637 ms | 69.8637 ms | 4 |
| 64 KiB, slow reader | buffer 1 MiB | 5 | 66.51 | 50.2775 ms | 56.8481 ms | 56.8481 ms | 4 |

### Medians

Percentage differences compare buffer 1 MiB against streaming, calculated from the displayed
medians. They are observed results, not evidence that buffering is generally faster or slower.

| Scenario | Mode | Throughput median | p50 median | p95 median | p99 median |
|---|---|---:|---:|---:|---:|
| 64 KiB | stream | 581.55 req/s | 1.8076 ms | 13.4089 ms | 13.4089 ms |
| 64 KiB | buffer 1 MiB | 483.39 req/s (-16.88%) | 1.6432 ms (-9.09%) | 16.7558 ms (+24.96%) | 16.7558 ms (+24.96%) |
| 4 MiB | stream | 165.01 req/s | 15.6576 ms | 27.7297 ms | 27.7297 ms |
| 4 MiB | buffer 1 MiB | 148.68 req/s (-9.90%) | 17.1891 ms (+9.78%) | 31.2990 ms (+12.87%) | 31.2990 ms (+12.87%) |
| 64 KiB, slow reader | stream | 66.91 req/s | 46.9886 ms | 56.7325 ms | 56.7325 ms |
| 64 KiB, slow reader | buffer 1 MiB | 67.39 req/s (+0.72%) | 45.7127 ms (-2.72%) | 56.8481 ms (+0.20%) | 56.8481 ms (+0.20%) |

The slow-reader results are close and do not distinguish the modes reliably. In these samples,
buffering's median throughput was lower for the 64 KiB and 4 MiB normal-reader scenarios; the
small sample size and run-to-run variation prevent a strong conclusion. No peak-memory measurement
was available, so memory impact is not quantified.

Body-integrity checks downloaded each payload from the upstream and both proxy modes. All three
copies matched by size and SHA-256: 64 KiB was 65,536 bytes
(`D8B2AF1C85CDB4588E978AED5875E12CBFD20F68009823CF914B40CF41D8E4CE`); 4 MiB was 4,194,304 bytes
(`3CAB7815C40973B8FB036C7A2A69B3649882622A4A0A2F1A980EFA4EF8545577`). Slow-client disconnect
and cancellation tests passed; no dedicated throughput claim is made for disconnect behavior.

The complete benchmark command output is included at
[`measurements/exp3-response-buffering/raw-benchmarks-final.txt`](./measurements/exp3-response-buffering/raw-benchmarks-final.txt).
This is a copy of the original `%TEMP%\edgelab-response-buffer-exp3\raw-benchmarks-final.txt`.
Process peak memory was not measured, and the per-repetition 10-request sample makes p95/p99
estimates particularly coarse.

## Measurement discipline
1. Run the benchmark at concurrency 1, 10, 50, and 100.
2. Record throughput, p50/p95/p99, and errors.
3. Change one variable at a time.
4. Do not claim an optimization unless repeated runs support it.
5. This is a local lab, not a production security proxy.

## Next milestones
- Add `/metrics` and structured access logs.
- Extend the local upstream fault injector with additional failure modes (for example, timeouts).
- Compare default transport vs tuned connection pooling.
- Add graceful shutdown and tests for timeout propagation.
- Publish raw benchmark output and a short results report.

## Experiment measurement instrumentation

The following instrumentation and wrapper are for upcoming Go experiments. They do not alter
the historical measurements above.

### Proxy connection and error counters

`http.Server.ConnState` now tracks these separate values at `/stats`:

- `inbound_connections_accepted_total` is the lifetime number of accepted proxy TCP connections.
  It is not a count of connections currently open or requests handled.
- `inbound_connections_open` is the number of accepted connections not yet closed or hijacked.
- `inbound_connections_peak` is the maximum open count since startup or the last interval reset.
- `POST /stats/reset` resets request/error interval counters and sets the open-connection peak to
  the current open count. It preserves accepted-total and current-open values.

Close and hijack transitions each remove a connection from the tracked set at most once. This
prevents a hijack followed by a close notification from decrementing current-open twice. A
persistent stats connection remains an open inbound connection; the measurement wrapper opens and
reuses one observer connection, records its baseline, and reports the peak above that baseline.

The existing reverse-proxy `ErrorHandler` receives both `RoundTrip` failures and response
processing errors, so the old aggregate alone could not classify them. It now retains that
aggregate and partitions its error events into `proxy_timeout_errors` (a wrapped `net.Error`
whose `Timeout()` is true), `proxy_other_upstream_errors` (a non-timeout `*url.Error` from
RoundTrip), and `proxy_response_processing_errors` (other proxy response-path errors). The
original error log is unchanged. These proxy event counters are not additional unique failed
requests and must not be added to the benchmark's `Errors`; the benchmark counts each failed
request once even when it has multiple categories.

### PowerShell phase wrapper and JSONL

[`scripts/measure-benchmark.ps1`](./scripts/measure-benchmark.ps1) executes an optional warm-up
and one measured command sequentially. It writes complete stdout/stderr files and one
versioned JSONL object per phase, conforming to
[`scripts/measurement.schema.json`](./scripts/measurement.schema.json). The script keeps persistent HTTP observer connections to the
proxy/upstream stats endpoints; reset and snapshots use those connections so monitoring does not
create a fresh accepted connection for each probe. It resets stats after warm-up, before the
measured phase. During phases longer than 30 seconds it refreshes both observers on the same
connections to avoid the server's 60-second idle timeout. This small stats traffic is included in
proxy/upstream process CPU deltas, but does not create another connection or enter benchmark
request counts.

Example for an isolated proxy on port 18081 and upstream on 19091:

```powershell
$clientFlags = @(
  '-force-http2=true', '-max-idle-conns=100', '-max-idle-conns-per-host=2',
  '-max-conns-per-host=0', '-idle-conn-timeout=90s', '-dial-timeout=30s',
  '-keep-alive=30s', '-tls-handshake-timeout=10s',
  '-response-header-timeout=0s', '-expect-continue-timeout=1s'
)
$warmup = @('-url', 'http://localhost:18081/hello', '-n', '1000', '-c', '10',
  '-timeout=5s') + $clientFlags
$measured = @('-url', 'http://localhost:18081/hello', '-n', '20000', '-c', '10',
  '-timeout=5s') + $clientFlags

.\scripts\measure-benchmark.ps1 `
  -RunId 'baseline-c10-rep1' -Experiment 'go-baseline' -ConfigurationId 'c10' `
  -BenchmarkExecutable '.\bench.exe' -WarmupArguments $warmup `
  -BenchmarkArguments $measured -ProxyPid 1234 -UpstreamPid 5678 `
  -ProxyStatsUrl 'http://127.0.0.1:18081/stats' `
  -ProxyStatsResetUrl 'http://127.0.0.1:18081/stats/reset' `
  -UpstreamStatsUrl 'http://127.0.0.1:19091/stats' `
  -UpstreamStatsResetUrl 'http://127.0.0.1:19091/stats/reset' `
  -OutputDirectory '.\measurements'
```

Replace the example PIDs with the exact proxy and upstream process IDs. Run one wrapper invocation
at a time. The script enforces a 600-second per-command timeout by default and records a timed-out
phase's output and JSONL result before returning an error. Warm-up and measured phases have
separate process CPU deltas and output files; no warm-up result is included in the measured
benchmark result.

Successful phase records retain the existing `schema_version=1` format. A collection failure
instead appends exactly one `record_type="collection_failure"` object for that invocation, with
run/experiment/configuration IDs, the timestamp, known phase, a `failure_stage`, error type/message,
known server and benchmark process IDs, any available binary hashes and command, and a `metrics`
object. Unavailable metrics are explicit `null`; metrics already captured for a completed child
phase are retained even if a later stats snapshot fails. This collection-failure record is
separate from an ordinary benchmark nonzero exit or timeout, which continues to emit the normal
phase record with its exit/timeout status and null summary metrics if none were printed.

Collection-failure stages include `preflight`, `initial_stats`, `warmup_phase`,
`warmup_post_stats`, `stats_reset`, `measured_pre_stats`, `measured_phase`, and
`measured_post_stats`. Failures before an output directory/run file can be established cannot be
recorded there. The wrapper refuses to append to an existing run ID file and guards against a
duplicate collection-failure record for the same run ID.

Each successful JSONL line has `schema_version=1` and the following fields:

- Identity/time/command: `run_id`, `experiment`, `configuration_id`, `phase`,
  `timestamp_start_utc`, `timestamp_end_utc`, and `exact_command` (absolute executable, argument
  array, and Windows command-line rendering), `server_processes` (proxy/upstream PID, executable,
  and Windows command line), `binary_sha256` (benchmark/proxy/upstream executable hashes),
  `exit_code`, and `timed_out`.
- Timing/result: wrapper `elapsed_seconds`/`elapsed_nanoseconds`, parsed benchmark requests,
  effective worker count, throughput, successes, unique failed requests, client/body/status error
  categories, p50/p95/p99 in nanoseconds, and the percentile convention. If a benchmark process
  exits before emitting its summary (for example, invalid CLI arguments), these result values are
  explicitly `null`, not omitted or represented as zero. The process exit code and `timed_out`
  flag distinguish an incomplete run from a successful zero-request result.
- Processes: proxy, upstream, and benchmark process IDs; CPU seconds before/after and delta;
  phase wall seconds; one-core-equivalent CPU percentage
  (`cpu_delta_seconds / wall_seconds * 100`); logical-processor-normalized machine percentage
  (one-core percentage divided by logical processor count); working-set snapshot before and
  `working_set_observed_bytes` with a scope label (phase-end snapshot for persistent servers;
  last nonzero live 10-ms sample for the short-lived benchmark process);
  OS `PeakWorkingSet64` labelled as a process-lifetime high-water mark. The short-lived
  benchmark process is sampled every 10 ms while running; its observed working set is the last
  nonzero live sample (the short-lived process may no longer expose a meaningful working set after
  exit), its recorded peak is the maximum OS-reported high-water value observed, and the last
  sample time is included. Durations expressed by Go in microseconds are converted to
  nanoseconds using a Unicode-codepoint match compatible with Windows PowerShell 5.1.
- Instrumentation/environment/artifacts: proxy accepted/open/peak counters and categorized proxy
  error counters; upstream accepted-connection delta and active/peak handler counters; Windows
  version/build, CPU model/core counts, visible physical memory, PowerShell/Go versions, and paths
  to full stdout/stderr.

UTC timestamps are wall-clock markers immediately before launch and after exit/output collection;
elapsed durations come from .NET `Stopwatch` (monotonic) and include child launch, execution,
exit, and output draining. Exact benchmark executable/argument arrays and server executable paths,
PIDs, command lines, and hashes are recorded. Proxy/upstream CPU deltas bracket each command
phase; benchmark CPU time is measured from the fresh child process's start. The
proxy/upstream process-lifetime peak working set includes their startup and all activity before
the phase, including warm-up; it is not a phase-only peak. Since each benchmark command uses a
fresh process, its OS process-lifetime peak covers that command's startup and phase. The wrapper
polls the short-lived process every 10 ms because Windows process memory properties may no longer
be available after exit; very short phases can end between polls, so review the recorded final
sample timestamp and treat their peak as a sampled OS high-water mark. Sampled current
working-set values are not substitutes for the OS high-water value.

The persistent stats observers reuse one HTTP connection per configured stats endpoint. They do
not increment the proxy's `requests_received` handler counter, but their TCP connections can
appear in accepted/open/peak connection statistics. Those observer connections are established
before the warm-up/reset baseline and remain open across snapshots; stats reset and polling CPU
cost is included in the proxy/upstream process CPU deltas. Benchmark request/success/error totals
reconcile when the child emits its complete summary; for a timeout or early CLI failure, all
benchmark summary values are `null` while proxy/upstream counters and the child exit/timeout state
remain available.

If a stats endpoint is unreachable before a phase, or a server exits before the post-phase process
and stats snapshots can be collected, the wrapper throws after appending one collection-failure
record. This is distinct from a benchmark child failure; use the error stage and retained child
stdout/stderr paths to diagnose collection failures after launch. A successful stats HTTP response
must also include every expected counter; a malformed/incomplete stats body is treated as a
collection failure rather than silently becoming missing or zero-valued counters.

The wrapper captures exact benchmark arguments and obtains the proxy/upstream launch command lines
from `Win32_Process`; it hashes the three executable files with SHA-256 before running phases.
PowerShell and Windows process counters provide local process attribution, but background OS
activity and the co-located upstream/client can still affect results. Do not run benchmark
workloads simultaneously.

## Experiment 4: load scaling and resource consumption

### Method and reproduction

The sequential runner [`scripts/run-experiments-4-5.ps1`](./scripts/run-experiments-4-5.ps1) builds
the upstream, proxy, and benchmark executables once per run, starts them on dedicated loopback
ports, invokes the existing measurement wrapper once per condition, and stops only the processes
it started in a `finally` block. The output directory must be new or empty. It retains the binaries,
their SHA-256 values and launch command lines, per-phase JSONL records, full benchmark stdout and
stderr, server logs, and the Experiment 5 recovery-probe CSV.

The low-load pilot was run first:

```powershell
.\scripts\run-experiments-4-5.ps1 -OutputDirectory .\measurements\exp4-5-pilot-20261014 -Pilot
```

The full run used three sequential repetitions at each concurrency:

```powershell
.\scripts\run-experiments-4-5.ps1 -OutputDirectory .\measurements\exp4-5-20261014 -Repetitions 3
```

For a repeat, supply a different new output directory; the runner refuses to mix or overwrite
existing artifacts. It serves the same 64 KiB `/payload/64k` body through the same proxy/upstream
binaries for all Experiment 4 repetitions. Proxy-to-upstream keep-alives remained enabled, the
proxy used its defaults (`MaxIdleConns=1000`, `MaxIdleConnsPerHost=100`, unlimited active
connections, no response buffering), and the benchmark kept its default HTTP connection reuse.
The runner explicitly fixed the client pool at 1000/100, active connections unlimited, 90-second
idle timeout, 5-second request timeout, 30-second dial/TCP keep-alive, 10-second TLS handshake,
no response-header timeout, and 1-second expect-continue timeout. Each measured run had a
20-request concurrency-matched warm-up followed by 200 measured requests. Concurrency levels were
1, 5, 10, 25, and 50; no workloads ran concurrently.

### Results

The table reports the median of three repetitions for each metric independently. Request latency
percentiles use the benchmark's documented floor-index convention; displayed latency values are
milliseconds. To reproduce the aggregation, parse each per-run JSONL file, select its `measured`
record (not `warmup`), group by concurrency/phase, sort each metric's three values independently,
and select index `floor((3 - 1) / 2)`, i.e. the middle value. No run was excluded from these
medians. The PowerShell median helper used for the tables is:

```powershell
function Get-Median($Values) {
  $ordered = @($Values | Sort-Object)
  return $ordered[[int][Math]::Floor(($ordered.Count - 1) / 2)]
}
```

Apply it independently to `benchmark.throughput_requests_per_second`,
`benchmark.p50_ns`, `benchmark.p95_ns`, and `benchmark.p99_ns` for the selected measured records;
divide latency values by 1,000,000 to display milliseconds. Experiment 5's table uses the same
three-repetition, independently sorted median calculation.

| Client concurrency | Requests per repetition | Throughput (req/s) | p50 (ms) | p95 (ms) | p99 (ms) | Successes | Errors |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 1 | 200 | 1,040.79 | 0.599 | 2.423 | 3.610 | 200 | 0 |
| 5 | 200 | 3,111.32 | 1.249 | 3.881 | 5.658 | 200 | 0 |
| 10 | 200 | 2,333.66 | 3.801 | 8.804 | 11.507 | 200 | 0 |
| 25 | 200 | 2,233.92 | 11.535 | 20.110 | 27.769 | 200 | 0 |
| 50 | 200 | 1,962.41 | 15.999 | 55.846 | 72.921 | 200 | 0 |

Throughput increased about threefold from one to five clients, then the measured medians declined
at higher concurrency while latency—especially the tail—rose. This local sample suggests that
more concurrency did not help this workload beyond five clients, but it does not establish a
general proxy saturation threshold: each run contained only 200 requests, repetition variability
was substantial, and the client, proxy, and upstream shared one laptop.

The wrapper's process CPU deltas were too coarse/noisy for a defensible CPU comparison in these
short phases (including zero deltas in some phases and stats-observer work in the process totals).
Working-set values are phase-end snapshots for the long-lived servers; the recorded OS peak is
process-lifetime, not phase peak. The benchmark process's 10 ms samples can miss short peaks.
CPU and memory are therefore unavailable as reliable Experiment 4 findings and are not included
in the results table; do not interpret the wrapper's raw diagnostic fields as peak-resource
measurements.

## Experiment 5: upstream slowdown, failure, and recovery

The same runner uses three repetitions of the following sequential phases at concurrency 10. Each
phase has a 10-request warm-up before its measured run. A is 100 requests to `/hello`; B is 40
requests to `/delay?ms=200`; C is 40 requests to `/hello` while the upstream is in failure mode;
D is 100 requests to `/hello` after restoring the upstream. The delay handler's peak active
requests was 10 in every measured B repetition, confirming the intended concurrency and delay.

The upstream provides a loopback-only `POST /__control/failure?enabled=true|false` control for this
experiment. While enabled, it closes proxied request connections without sending an HTTP response,
except for the stats and control endpoints, so the measurement wrapper can continue observing the
same upstream PID. The existing proxy converts these failures to HTTP 502 responses. The benchmark
counts status codes >=400 as request errors, separately from client transport errors; consequently
C reports HTTP-status failures rather than falsely counting the returned 502 as a success.
Restoration is a `POST ...enabled=false` followed by a probe through the same proxy.

| Phase | Upstream condition | Requests; concurrency | Throughput (req/s) | p50 (ms) | p95 (ms) | p99 (ms) | Successes | Errors (HTTP status) |
|---|---|---:|---:|---:|---:|---:|---:|---:|
| A — healthy | Normal `/hello` | 100; 10 | 4,925.84 | 1.280 | 5.744 | 6.290 | 100 | 0 |
| B — slow | 200 ms `/delay` | 40; 10 | 49.04 | 202.831 | 207.695 | 207.695 | 40 | 0 |
| C — failure | Upstream drops all proxied request connections | 40; 10 | 1,054.59 | 7.633 | 18.279 | 18.518 | 0 | 40 |
| D — recovery | Failure disabled; normal `/hello` | 100; 10 | 3,456.02 | 1.571 | 6.287 | 7.696 | 100 | 0 |

Recovery time is defined narrowly as the monotonic elapsed time starting immediately before the
upstream restore POST and ending when the first subsequent proxy `/hello` probe returns HTTP 200.
The three values were 47.259 ms, 39.334 ms, and 36.909 ms (median 39.334 ms). This is a
time-to-first-successful-probe, not proof of sustained or full-system recovery. The separate D
benchmark then verified 100/100 successful requests in every repetition; its throughput and
latencies are reported separately. Phase C produced 40/40 unique benchmark errors and 40 HTTP
status errors in each repetition; client-transport and response-body error counts were zero because
the proxy returned a 502 response.

The results are local to this single Windows 11 Home laptop (12th Gen Intel Core i3-1215U, 6
physical cores / 8 logical processors, about 7.68 GiB visible RAM). The small sequential samples
do not represent multi-host production load. No benchmark or collection failures occurred in the
full run; the expected Phase C request failures are retained as ordinary measured records, not
discarded collection failures.

Raw full-run artifacts are under
[`measurements/exp4-5-20261014/`](./measurements/exp4-5-20261014/): 27 per-run JSONL files (54
warm-up/measured records), 54 matching stdout/stderr pairs, the recovery CSV, two server logs, and
the three binaries. The low-load pilot and its artifacts are under
[`measurements/exp4-5-pilot-20261014/`](./measurements/exp4-5-pilot-20261014/). All 64 pilot and
full-run JSONL records validated against the existing Draft 2020-12
[`measurement schema`](./scripts/measurement.schema.json); there were no `collection_failure`
records. The wrapper's existing collection-failure behavior and schema were not changed.
