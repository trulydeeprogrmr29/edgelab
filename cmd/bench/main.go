package main

import (
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"
)

type benchmarkConfig struct {
	URL                   string
	Requests              int
	Concurrency           int
	ClientTimeout         time.Duration
	ForceAttemptHTTP2     bool
	MaxIdleConns          int
	MaxIdleConnsPerHost   int
	MaxConnsPerHost       int
	IdleConnTimeout       time.Duration
	DialTimeout           time.Duration
	KeepAlive             time.Duration
	TLSHandshakeTimeout   time.Duration
	ResponseHeaderTimeout time.Duration
	ExpectContinueTimeout time.Duration
	ResponseReadChunkSize int
	ResponseReadDelay     time.Duration
}

type requestResult struct {
	latency     time.Duration
	clientErr   bool
	bodyReadErr bool
	statusErr   bool
}

type benchmarkResult struct {
	latencies    []time.Duration
	Requests     int
	Concurrency  int
	Successful   int
	Errors       int
	ClientErrs   int
	BodyReadErrs int
	StatusErrs   int
	Elapsed      time.Duration
}

func main() {
	cfg, err := parseConfig(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	client := newHTTPClient(cfg)
	defer client.Transport.(*http.Transport).CloseIdleConnections()
	result := runBenchmark(cfg, client)
	printResult(os.Stdout, cfg, result)
}

func parseConfig(args []string) (benchmarkConfig, error) {
	cfg := benchmarkConfig{}
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&cfg.URL, "url", "http://localhost:18080/hello", "URL to request")
	fs.IntVar(&cfg.Requests, "n", 10000, "total requests")
	fs.IntVar(&cfg.Concurrency, "c", 50, "concurrent workers")
	fs.DurationVar(&cfg.ClientTimeout, "timeout", 5*time.Second, "per-request client timeout")
	fs.BoolVar(&cfg.ForceAttemptHTTP2, "force-http2", true, "attempt HTTP/2 for HTTPS connections")
	fs.IntVar(&cfg.MaxIdleConns, "max-idle-conns", 100, "maximum idle connections across all hosts")
	fs.IntVar(&cfg.MaxIdleConnsPerHost, "max-idle-conns-per-host", 2, "maximum idle connections per host")
	fs.IntVar(&cfg.MaxConnsPerHost, "max-conns-per-host", 0, "maximum connections per host (0 means unlimited)")
	fs.DurationVar(&cfg.IdleConnTimeout, "idle-conn-timeout", 90*time.Second, "maximum idle connection duration")
	fs.DurationVar(&cfg.DialTimeout, "dial-timeout", 30*time.Second, "connection establishment timeout")
	fs.DurationVar(&cfg.KeepAlive, "keep-alive", 30*time.Second, "TCP keep-alive period")
	fs.DurationVar(&cfg.TLSHandshakeTimeout, "tls-handshake-timeout", 10*time.Second, "TLS handshake timeout")
	fs.DurationVar(&cfg.ResponseHeaderTimeout, "response-header-timeout", 0, "response header timeout (0 disables it)")
	fs.DurationVar(&cfg.ExpectContinueTimeout, "expect-continue-timeout", time.Second, "100-continue response timeout")
	fs.IntVar(&cfg.ResponseReadChunkSize, "read-chunk-size", 1024, "response read chunk size")
	fs.DurationVar(&cfg.ResponseReadDelay, "read-delay", 0, "delay between response reads (slow-downstream simulation)")
	if err := fs.Parse(args); err != nil {
		return benchmarkConfig{}, err
	}
	if fs.NArg() != 0 {
		return benchmarkConfig{}, fmt.Errorf("unexpected positional arguments: %v", fs.Args())
	}
	if cfg.Requests <= 0 || cfg.Concurrency <= 0 {
		return benchmarkConfig{}, fmt.Errorf("-n and -c must be positive")
	}
	if cfg.ResponseReadChunkSize <= 0 || cfg.ResponseReadDelay < 0 {
		return benchmarkConfig{}, fmt.Errorf("-read-chunk-size must be positive and -read-delay must not be negative")
	}
	if cfg.ClientTimeout <= 0 || cfg.DialTimeout <= 0 {
		return benchmarkConfig{}, fmt.Errorf("-timeout and -dial-timeout must be positive")
	}
	if cfg.MaxIdleConns < 0 || cfg.MaxIdleConnsPerHost < 0 || cfg.MaxConnsPerHost < 0 {
		return benchmarkConfig{}, fmt.Errorf("connection limits must not be negative")
	}
	if cfg.IdleConnTimeout < 0 || cfg.KeepAlive < 0 || cfg.TLSHandshakeTimeout < 0 ||
		cfg.ResponseHeaderTimeout < 0 || cfg.ExpectContinueTimeout < 0 {
		return benchmarkConfig{}, fmt.Errorf("transport timeouts must not be negative")
	}
	parsedURL, err := http.NewRequest(http.MethodGet, cfg.URL, nil)
	if err != nil || (parsedURL.URL.Scheme != "http" && parsedURL.URL.Scheme != "https") || parsedURL.URL.Host == "" {
		return benchmarkConfig{}, fmt.Errorf("-url must be a valid http or https URL")
	}
	return cfg, nil
}

func newHTTPClient(cfg benchmarkConfig) *http.Client {
	dialer := &net.Dialer{
		Timeout:   cfg.DialTimeout,
		KeepAlive: cfg.KeepAlive,
	}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     cfg.ForceAttemptHTTP2,
		MaxIdleConns:          cfg.MaxIdleConns,
		MaxIdleConnsPerHost:   cfg.MaxIdleConnsPerHost,
		MaxConnsPerHost:       cfg.MaxConnsPerHost,
		IdleConnTimeout:       cfg.IdleConnTimeout,
		TLSHandshakeTimeout:   cfg.TLSHandshakeTimeout,
		ResponseHeaderTimeout: cfg.ResponseHeaderTimeout,
		ExpectContinueTimeout: cfg.ExpectContinueTimeout,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   cfg.ClientTimeout,
	}
}

func runBenchmark(cfg benchmarkConfig, client *http.Client) benchmarkResult {
	results := make([]requestResult, cfg.Requests)
	jobs := make(chan int)
	workers := cfg.Concurrency
	if workers > cfg.Requests {
		workers = cfg.Requests
	}

	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				results[idx] = makeRequest(client, cfg.URL, cfg.ResponseReadChunkSize, cfg.ResponseReadDelay)
			}
		}()
	}
	for i := 0; i < cfg.Requests; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	elapsed := time.Since(start)

	result := benchmarkResult{
		latencies:   make([]time.Duration, len(results)),
		Requests:    cfg.Requests,
		Concurrency: workers,
		Elapsed:     elapsed,
	}
	for i, request := range results {
		result.latencies[i] = request.latency
		failed := request.clientErr || request.bodyReadErr || request.statusErr
		if failed {
			result.Errors++
		} else {
			result.Successful++
		}
		if request.clientErr {
			result.ClientErrs++
		}
		if request.bodyReadErr {
			result.BodyReadErrs++
		}
		if request.statusErr {
			result.StatusErrs++
		}
	}
	sort.Slice(result.latencies, func(i, j int) bool {
		return result.latencies[i] < result.latencies[j]
	})
	return result
}

func makeRequest(client *http.Client, requestURL string, readChunkSize int, readDelay time.Duration) requestResult {
	start := time.Now()
	resp, err := client.Get(requestURL)
	result := requestResult{clientErr: err != nil}
	if resp != nil {
		var readErr error
		if readDelay == 0 {
			_, readErr = io.Copy(io.Discard, resp.Body)
		} else {
			readErr = consumeSlowly(resp.Body, readChunkSize, readDelay)
		}
		if readErr != nil {
			result.bodyReadErr = true
		}
		if closeErr := resp.Body.Close(); closeErr != nil {
			result.bodyReadErr = true
		}
		if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusBadRequest {
			result.statusErr = true
		}
	}
	result.latency = time.Since(start)
	return result
}

func consumeSlowly(body io.Reader, chunkSize int, delay time.Duration) error {
	buffer := make([]byte, chunkSize)
	for {
		n, err := body.Read(buffer)
		if n > 0 {
			time.Sleep(delay)
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

func percentile(sortedLatencies []time.Duration, p float64) time.Duration {
	if len(sortedLatencies) == 0 {
		return 0
	}
	idx := int(float64(len(sortedLatencies)-1) * p)
	return sortedLatencies[idx]
}

func printResult(w io.Writer, cfg benchmarkConfig, result benchmarkResult) {
	fmt.Fprintf(w, "URL:                       %s\n", cfg.URL)
	fmt.Fprintf(w, "Requests:                  %d\n", result.Requests)
	fmt.Fprintf(w, "Concurrency:               %d\n", result.Concurrency)
	fmt.Fprintf(w, "Client timeout:            %s\n", cfg.ClientTimeout)
	fmt.Fprintf(w, "Proxy setting:             environment\n")
	fmt.Fprintf(w, "Force HTTP/2:              %t\n", cfg.ForceAttemptHTTP2)
	fmt.Fprintf(w, "Max idle connections:      %d\n", cfg.MaxIdleConns)
	fmt.Fprintf(w, "Max idle connections/host: %d\n", cfg.MaxIdleConnsPerHost)
	if cfg.MaxConnsPerHost == 0 {
		fmt.Fprintln(w, "Max connections/host:      unlimited")
	} else {
		fmt.Fprintf(w, "Max connections/host:      %d\n", cfg.MaxConnsPerHost)
	}
	fmt.Fprintf(w, "Idle connection timeout:   %s\n", cfg.IdleConnTimeout)
	fmt.Fprintf(w, "Dial timeout:              %s\n", cfg.DialTimeout)
	fmt.Fprintf(w, "TCP keep-alive:            %s\n", cfg.KeepAlive)
	fmt.Fprintf(w, "TLS handshake timeout:     %s\n", cfg.TLSHandshakeTimeout)
	fmt.Fprintf(w, "Response header timeout:   %s\n", cfg.ResponseHeaderTimeout)
	fmt.Fprintf(w, "Expect-continue timeout:   %s\n", cfg.ExpectContinueTimeout)
	fmt.Fprintf(w, "Response read chunk size:  %d\n", cfg.ResponseReadChunkSize)
	fmt.Fprintf(w, "Response read delay:       %s\n", cfg.ResponseReadDelay)
	fmt.Fprintf(w, "Duration:                  %s\n", result.Elapsed)
	fmt.Fprintf(w, "Throughput:                %.2f req/s\n", float64(result.Requests)/result.Elapsed.Seconds())
	fmt.Fprintf(w, "Successful:                %d\n", result.Successful)
	fmt.Fprintf(w, "Errors:                    %d\n", result.Errors)
	fmt.Fprintf(w, "Client request errors:     %d\n", result.ClientErrs)
	fmt.Fprintf(w, "Body read/close errors:    %d\n", result.BodyReadErrs)
	fmt.Fprintf(w, "HTTP status errors:        %d\n", result.StatusErrs)
	fmt.Fprintf(w, "p50 latency:               %s\n", percentile(result.latencies, 0.50))
	fmt.Fprintf(w, "p95 latency:               %s\n", percentile(result.latencies, 0.95))
	fmt.Fprintf(w, "p99 latency:               %s\n", percentile(result.latencies, 0.99))
}
