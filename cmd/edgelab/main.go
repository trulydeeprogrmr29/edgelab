package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

type counters struct {
	requests                     atomic.Uint64
	errors                       atomic.Uint64
	upstreamTimeoutErrors        atomic.Uint64
	upstreamOtherTransportErrors atomic.Uint64
	responseProcessingErrors     atomic.Uint64
	connMu                       sync.Mutex
	connections                  map[net.Conn]struct{}
	acceptedConnections          uint64
	openConnections              uint64
	peakOpenConnections          uint64
}

type connectionSnapshot struct {
	accepted uint64
	open     uint64
	peak     uint64
}

func (c *counters) connectionState(conn net.Conn, state http.ConnState) {
	c.connMu.Lock()
	defer c.connMu.Unlock()

	switch state {
	case http.StateNew:
		if c.connections == nil {
			c.connections = make(map[net.Conn]struct{})
		}
		if _, exists := c.connections[conn]; exists {
			return
		}
		c.connections[conn] = struct{}{}
		c.acceptedConnections++
		c.openConnections++
		if c.openConnections > c.peakOpenConnections {
			c.peakOpenConnections = c.openConnections
		}
	case http.StateClosed, http.StateHijacked:
		if _, exists := c.connections[conn]; !exists {
			return
		}
		delete(c.connections, conn)
		c.openConnections--
	}
}

func (c *counters) connectionSnapshot() connectionSnapshot {
	c.connMu.Lock()
	defer c.connMu.Unlock()
	return connectionSnapshot{
		accepted: c.acceptedConnections,
		open:     c.openConnections,
		peak:     c.peakOpenConnections,
	}
}

func (c *counters) resetInterval() {
	c.requests.Store(0)
	c.errors.Store(0)
	c.upstreamTimeoutErrors.Store(0)
	c.upstreamOtherTransportErrors.Store(0)
	c.responseProcessingErrors.Store(0)

	c.connMu.Lock()
	c.peakOpenConnections = c.openConnections
	c.connMu.Unlock()
}

func (c *counters) recordUpstreamError(err error) {
	c.errors.Add(1)
	if isTimeoutError(err) {
		c.upstreamTimeoutErrors.Add(1)
		return
	}
	var requestError *url.Error
	if errors.As(err, &requestError) {
		c.upstreamOtherTransportErrors.Add(1)
		return
	}
	c.responseProcessingErrors.Add(1)
}

func isTimeoutError(err error) bool {
	var networkError net.Error
	return errors.As(err, &networkError) && networkError.Timeout()
}

func main() {
	config, err := parseProxyConfig(os.Args[1:])
	if err != nil {
		log.Print(err)
		os.Exit(2)
	}

	target, err := url.Parse(config.upstreamURL)
	if err != nil {
		log.Fatal(err)
	}

	stats := &counters{}
	handler := newProxyHandler(target, config, stats)

	server := &http.Server{
		Addr:              config.listenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		ConnState:         stats.connectionState,
	}
	log.Printf("EdgeLab proxy listening on %s -> %s (upstream keep-alives=%t max-conns-per-host=%d response-buffer-limit=%d)",
		server.Addr, target, config.upstreamKeepAlives, config.upstreamMaxConnsPerHost, config.responseBufferLimit)
	log.Fatal(server.ListenAndServe())
}

func newProxyHandler(target *url.URL, config proxyConfig, stats *counters) http.Handler {
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = newUpstreamTransport(config.upstreamKeepAlives, config.upstreamMaxConnsPerHost)
	if config.responseBufferLimit > 0 {
		proxy.ModifyResponse = boundedResponseBuffer(config.responseBufferLimit)
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		stats.recordUpstreamError(err)
		log.Printf("proxy_error method=%s path=%s error=%q", r.Method, r.URL.Path, err)
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		connections := stats.connectionSnapshot()
		_, _ = w.Write([]byte(
			"requests_received " + itoa(stats.requests.Load()) + "\n" +
				"proxy_errors " + itoa(stats.errors.Load()) + "\n" +
				"proxy_timeout_errors " + itoa(stats.upstreamTimeoutErrors.Load()) + "\n" +
				"proxy_other_upstream_errors " + itoa(stats.upstreamOtherTransportErrors.Load()) + "\n" +
				"proxy_response_processing_errors " + itoa(stats.responseProcessingErrors.Load()) + "\n" +
				"inbound_connections_accepted_total " + itoa(connections.accepted) + "\n" +
				"inbound_connections_open " + itoa(connections.open) + "\n" +
				"inbound_connections_peak " + itoa(connections.peak) + "\n"))
	})
	mux.HandleFunc("POST /stats/reset", func(w http.ResponseWriter, _ *http.Request) {
		stats.resetInterval()
		w.WriteHeader(http.StatusNoContent)
	})
	controlUnavailable := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	mux.Handle("/__control/failure", controlUnavailable)
	mux.Handle("/__control/failure/", controlUnavailable)
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stats.requests.Add(1)
		proxy.ServeHTTP(w, r)
	}))
	return mux
}

type proxyConfig struct {
	listenAddr              string
	upstreamURL             string
	upstreamKeepAlives      bool
	upstreamMaxConnsPerHost int
	responseBufferLimit     int64
}

func parseProxyConfig(args []string) (proxyConfig, error) {
	config := proxyConfig{}
	flags := flag.NewFlagSet("edgelab", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&config.listenAddr, "listen", "127.0.0.1:18080", "proxy listen address")
	flags.StringVar(&config.upstreamURL, "upstream", "http://127.0.0.1:19090", "upstream base URL")
	flags.BoolVar(&config.upstreamKeepAlives, "upstream-keep-alives", true, "reuse upstream TCP connections")
	flags.IntVar(&config.upstreamMaxConnsPerHost, "upstream-max-conns-per-host", 0,
		"maximum active upstream connections per host (0 means unlimited)")
	flags.Int64Var(&config.responseBufferLimit, "response-buffer-limit", 0,
		"maximum bytes to buffer from an upstream response (0 disables buffering)")
	if err := flags.Parse(args); err != nil {
		return proxyConfig{}, err
	}
	if flags.NArg() > 0 {
		return proxyConfig{}, fmt.Errorf("unexpected positional arguments: %v", flags.Args())
	}
	if config.upstreamMaxConnsPerHost < 0 {
		return proxyConfig{}, fmt.Errorf("-upstream-max-conns-per-host must not be negative")
	}
	if config.responseBufferLimit < 0 || config.responseBufferLimit == math.MaxInt64 {
		return proxyConfig{}, fmt.Errorf("-response-buffer-limit must be between 0 and %d", int64(math.MaxInt64)-1)
	}
	upstream, err := url.Parse(config.upstreamURL)
	if err != nil {
		return proxyConfig{}, fmt.Errorf("-upstream must be a valid URL: %w", err)
	}
	if (upstream.Scheme != "http" && upstream.Scheme != "https") || upstream.Host == "" {
		return proxyConfig{}, fmt.Errorf("-upstream must be an absolute http or https URL")
	}
	return config, nil
}

func boundedResponseBuffer(maxBytes int64) func(*http.Response) error {
	if maxBytes <= 0 {
		return nil
	}
	return func(response *http.Response) error {
		if response.Body == nil {
			return nil
		}

		body, err := io.ReadAll(io.LimitReader(response.Body, maxBytes+1))
		if err != nil {
			closeErr := response.Body.Close()
			return errors.Join(fmt.Errorf("buffer upstream response: %w", err), closeErr)
		}
		if int64(len(body)) <= maxBytes {
			if err := response.Body.Close(); err != nil {
				return fmt.Errorf("close buffered upstream response: %w", err)
			}
			response.Body = io.NopCloser(bytes.NewReader(body))
			return nil
		}

		response.Body = &prefixedReadCloser{
			Reader: io.MultiReader(bytes.NewReader(body), response.Body),
			closer: response.Body,
		}
		return nil
	}
}

type prefixedReadCloser struct {
	io.Reader
	closer io.Closer
}

func (body *prefixedReadCloser) Close() error {
	return body.closer.Close()
}

func newUpstreamTransport(keepAlives bool, maxConnsPerHost int) *http.Transport {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          1000,
		MaxIdleConnsPerHost:   100,
		MaxConnsPerHost:       maxConnsPerHost,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 2 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableKeepAlives:     !keepAlives,
	}
}

func itoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
