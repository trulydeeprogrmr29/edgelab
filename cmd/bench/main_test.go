package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestPercentile(t *testing.T) {
	tests := []struct {
		name   string
		values []time.Duration
		p      float64
		want   time.Duration
	}{
		{name: "p50 of five", values: []time.Duration{time.Nanosecond, 2, 3, 4, 5}, p: 0.50, want: 3},
		{name: "p95 of five uses floor index", values: []time.Duration{time.Nanosecond, 2, 3, 4, 5}, p: 0.95, want: 4},
		{name: "p50 of four uses floor index", values: []time.Duration{time.Nanosecond, 2, 3, 4}, p: 0.50, want: 2},
		{name: "single sample", values: []time.Duration{17}, p: 0.99, want: 17},
		{name: "empty samples", values: nil, p: 0.50, want: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := percentile(test.values, test.p); got != test.want {
				t.Fatalf("percentile(%v, %v) = %v, want %v", test.values, test.p, got, test.want)
			}
		})
	}
}

func TestParseConfig(t *testing.T) {
	cfg, err := parseConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxIdleConns != 100 || cfg.MaxIdleConnsPerHost != 2 ||
		cfg.MaxConnsPerHost != 0 || cfg.ClientTimeout != 5*time.Second ||
		!cfg.ForceAttemptHTTP2 ||
		cfg.DialTimeout != 30*time.Second || cfg.KeepAlive != 30*time.Second ||
		cfg.IdleConnTimeout != 90*time.Second || cfg.TLSHandshakeTimeout != 10*time.Second ||
		cfg.ResponseHeaderTimeout != 0 || cfg.ExpectContinueTimeout != time.Second {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}

	invalidArgs := [][]string{
		{"-n=0"},
		{"-c=0"},
		{"-timeout=0s"},
		{"-dial-timeout=0s"},
		{"-max-idle-conns=-1"},
		{"-max-idle-conns-per-host=-1"},
		{"-max-conns-per-host=-1"},
		{"-idle-conn-timeout=-1s"},
		{"-response-header-timeout=-1s"},
		{"-url=ftp://localhost/path"},
		{"-url=not-a-url"},
		{"-unknown"},
	}
	for _, args := range invalidArgs {
		t.Run(fmt.Sprint(args), func(t *testing.T) {
			if _, err := parseConfig(args); err == nil {
				t.Fatalf("parseConfig(%v) unexpectedly succeeded", args)
			}
		})
	}
}

func TestRunBenchmarkSuccessAndRequestAccounting(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()

	cfg := benchmarkConfig{URL: server.URL, Requests: 12, Concurrency: 4}
	result := runBenchmark(cfg, server.Client())
	if got := requests.Load(); got != int64(cfg.Requests) {
		t.Fatalf("server received %d requests, want %d", got, cfg.Requests)
	}
	if result.Requests != cfg.Requests || result.Successful != cfg.Requests ||
		result.Errors != 0 || result.ClientErrs != 0 ||
		result.BodyReadErrs != 0 || result.StatusErrs != 0 {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestRunBenchmarkCapsWorkersAndConsumesBody(t *testing.T) {
	body := &trackedBody{reader: bytes.NewReader([]byte("response body"))}
	client := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       body,
			Header:     make(http.Header),
		}, nil
	})}
	cfg := benchmarkConfig{URL: "http://example.test/", Requests: 1, Concurrency: 10}

	result := runBenchmark(cfg, client)
	if result.Concurrency != 1 {
		t.Fatalf("effective concurrency = %d, want 1", result.Concurrency)
	}
	if body.readBytes != int64(len("response body")) || !body.closed {
		t.Fatalf("body read %d bytes and closed=%v; want all bytes and closed", body.readBytes, body.closed)
	}
}

func TestRunBenchmarkHTTPStatusError(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "failure", http.StatusInternalServerError)
	}))
	defer server.Close()

	cfg := benchmarkConfig{URL: server.URL, Requests: 3, Concurrency: 2}
	result := runBenchmark(cfg, server.Client())
	if got := requests.Load(); got != int64(cfg.Requests) {
		t.Fatalf("server received %d requests, want %d", got, cfg.Requests)
	}
	if result.Errors != cfg.Requests || result.Successful != 0 || result.StatusErrs != cfg.Requests {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestRunBenchmarkTransportError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := server.URL
	server.Close()

	cfg := benchmarkConfig{URL: url, Requests: 2, Concurrency: 2}
	result := runBenchmark(cfg, &http.Client{Timeout: time.Second})
	if result.Errors != cfg.Requests || result.Successful != 0 || result.ClientErrs != cfg.Requests {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestRunBenchmarkBodyReadErrorAndClose(t *testing.T) {
	body := &failingBody{}
	client := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       body,
			Header:     make(http.Header),
		}, nil
	})}
	cfg := benchmarkConfig{URL: "http://example.test/", Requests: 1, Concurrency: 1}

	result := runBenchmark(cfg, client)
	if result.Errors != 1 || result.BodyReadErrs != 1 || result.Successful != 0 {
		t.Fatalf("unexpected result: %+v", result)
	}
	if !body.closed {
		t.Fatal("response body was not closed after read failure")
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

type failingBody struct {
	closed bool
}

func (b *failingBody) Read([]byte) (int, error) {
	return 0, errors.New("body read failed")
}

func (b *failingBody) Close() error {
	b.closed = true
	return nil
}

type trackedBody struct {
	reader    *bytes.Reader
	readBytes int64
	closed    bool
}

func (b *trackedBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	b.readBytes += int64(n)
	return n, err
}

func (b *trackedBody) Close() error {
	b.closed = true
	return nil
}
