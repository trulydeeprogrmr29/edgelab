package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestUpstreamKeepAliveControlsConnectionReuse(t *testing.T) {
	for _, test := range []struct {
		name       string
		keepAlives bool
		wantConns  uint64
	}{
		{name: "enabled reuses one connection", keepAlives: true, wantConns: 1},
		{name: "disabled opens a connection per request", keepAlives: false, wantConns: 5},
	} {
		t.Run(test.name, func(t *testing.T) {
			var accepted atomic.Uint64
			upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = fmt.Fprintln(w, "ok")
			}))
			upstream.Config.ConnState = func(_ net.Conn, state http.ConnState) {
				if state == http.StateNew {
					accepted.Add(1)
				}
			}
			upstream.Start()
			defer upstream.Close()

			transport := newUpstreamTransport(test.keepAlives, 0)
			defer transport.CloseIdleConnections()
			proxy := httputil.NewSingleHostReverseProxy(mustParseURL(t, upstream.URL))
			proxy.Transport = transport

			for i := 0; i < 5; i++ {
				recorder := httptest.NewRecorder()
				request := httptest.NewRequest(http.MethodGet, "http://proxy.test/hello", nil)
				proxy.ServeHTTP(recorder, request)
				if recorder.Code != http.StatusOK {
					t.Fatalf("request %d status = %d, want %d", i+1, recorder.Code, http.StatusOK)
				}
			}

			if got := accepted.Load(); got != test.wantConns {
				t.Fatalf("accepted upstream connections = %d, want %d", got, test.wantConns)
			}
		})
	}
}

func TestNewUpstreamTransportPreservesPoolSettings(t *testing.T) {
	transport := newUpstreamTransport(true, 0)
	if transport.MaxIdleConns != 1000 || transport.MaxIdleConnsPerHost != 100 ||
		transport.MaxConnsPerHost != 0 || transport.IdleConnTimeout.String() != "1m30s" ||
		transport.DisableKeepAlives {
		t.Fatalf("unexpected keep-alive transport configuration: %+v", transport)
	}

	disabled := newUpstreamTransport(false, 0)
	if !disabled.DisableKeepAlives {
		t.Fatal("DisableKeepAlives is false when upstream keep-alives are disabled")
	}
}

func TestParseProxyConfigMaxConnsPerHost(t *testing.T) {
	config, err := parseProxyConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	if config.listenAddr != "127.0.0.1:18080" || config.upstreamURL != "http://127.0.0.1:19090" ||
		config.upstreamMaxConnsPerHost != 0 || !config.upstreamKeepAlives || config.responseBufferLimit != 0 {
		t.Fatalf("default configuration changed: %+v", config)
	}

	config, err = parseProxyConfig([]string{"-upstream-max-conns-per-host=7", "-response-buffer-limit=4096"})
	if err != nil {
		t.Fatal(err)
	}
	if config.upstreamMaxConnsPerHost != 7 || !config.upstreamKeepAlives || config.responseBufferLimit != 4096 {
		t.Fatalf("configured values or keep-alive default incorrect: %+v", config)
	}
	for _, args := range [][]string{
		{"-upstream-max-conns-per-host=-1"},
		{"-upstream=ftp://localhost:19090"},
		{"-response-buffer-limit=-1"},
		{"-response-buffer-limit=9223372036854775807"},
	} {
		if _, err := parseProxyConfig(args); err == nil {
			t.Fatalf("invalid arguments %v were accepted", args)
		}
	}
}

func TestFailureControlCannotBeReachedThroughProxy(t *testing.T) {
	var failureMode atomic.Bool
	var controlCalls atomic.Uint64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/__control/failure" && r.Method == http.MethodPost {
			controlCalls.Add(1)
			enabled, err := strconv.ParseBool(r.URL.Query().Get("enabled"))
			if err != nil {
				http.Error(w, "invalid enabled value", http.StatusBadRequest)
				return
			}
			failureMode.Store(enabled)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if failureMode.Load() {
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	proxyHandler := newProxyHandler(mustParseURL(t, upstream.URL), proxyConfig{
		upstreamKeepAlives: true,
	}, &counters{})
	proxy := httptest.NewServer(proxyHandler)
	defer proxy.Close()

	send := func(client *http.Client, baseURL, enabled string) *http.Response {
		t.Helper()
		response, err := client.Post(baseURL+"/__control/failure?enabled="+enabled, "", nil)
		if err != nil {
			t.Fatalf("POST failure control enabled=%s: %v", enabled, err)
		}
		return response
	}
	assertProxiedStatus := func(want int) {
		t.Helper()
		response, err := proxy.Client().Get(proxy.URL + "/hello")
		if err != nil {
			t.Fatalf("GET through proxy: %v", err)
		}
		_ = response.Body.Close()
		if response.StatusCode != want {
			t.Fatalf("proxied /hello status = %d, want %d", response.StatusCode, want)
		}
	}

	response := send(proxy.Client(), proxy.URL, "true")
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("proxied enable-control status = %d, want %d", response.StatusCode, http.StatusNotFound)
	}
	_ = response.Body.Close()
	if failureMode.Load() {
		t.Fatal("ordinary proxy forwarding activated upstream failure mode")
	}
	assertProxiedStatus(http.StatusOK)

	response = send(upstream.Client(), upstream.URL, "true")
	if response.StatusCode != http.StatusNoContent {
		_ = response.Body.Close()
		t.Fatalf("direct enable-control status = %d, want %d", response.StatusCode, http.StatusNoContent)
	}
	_ = response.Body.Close()
	if !failureMode.Load() {
		t.Fatal("direct upstream control did not activate failure mode")
	}

	response = send(proxy.Client(), proxy.URL, "false")
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("proxied disable-control status = %d, want %d", response.StatusCode, http.StatusNotFound)
	}
	_ = response.Body.Close()
	if !failureMode.Load() {
		t.Fatal("ordinary proxy forwarding deactivated upstream failure mode")
	}
	assertProxiedStatus(http.StatusBadGateway)

	response = send(upstream.Client(), upstream.URL, "false")
	if response.StatusCode != http.StatusNoContent {
		_ = response.Body.Close()
		t.Fatalf("direct disable-control status = %d, want %d", response.StatusCode, http.StatusNoContent)
	}
	_ = response.Body.Close()
	if failureMode.Load() {
		t.Fatal("direct upstream control did not restore failure mode")
	}
	if got := controlCalls.Load(); got != 2 {
		t.Fatalf("upstream control handler calls = %d, want 2 direct calls only", got)
	}
}

func TestConnectionCountersTrackLifecycleAndReset(t *testing.T) {
	stats := &counters{}
	first, firstPeer := net.Pipe()
	defer first.Close()
	defer firstPeer.Close()
	second, secondPeer := net.Pipe()
	defer second.Close()
	defer secondPeer.Close()

	stats.connectionState(first, http.StateNew)
	stats.connectionState(first, http.StateActive)
	stats.requests.Add(3)
	stats.recordUpstreamError(testTimeoutError("reset this interval error"))
	stats.recordUpstreamError(&url.Error{Op: "Get", URL: "http://upstream.test/", Err: errors.New("reset this interval error")})
	stats.recordUpstreamError(errors.New("reset this response processing error"))
	snapshot := stats.connectionSnapshot()
	if snapshot.accepted != 1 || snapshot.open != 1 || snapshot.peak != 1 {
		t.Fatalf("after first connection: got %+v, want accepted=1 open=1 peak=1", snapshot)
	}

	stats.resetInterval()
	snapshot = stats.connectionSnapshot()
	if snapshot.accepted != 1 || snapshot.open != 1 || snapshot.peak != 1 {
		t.Fatalf("reset changed lifetime/current connection counts: %+v", snapshot)
	}
	if stats.requests.Load() != 0 || stats.errors.Load() != 0 ||
		stats.upstreamTimeoutErrors.Load() != 0 ||
		stats.upstreamOtherTransportErrors.Load() != 0 ||
		stats.responseProcessingErrors.Load() != 0 {
		t.Fatal("interval request/error counters were not reset")
	}

	stats.connectionState(second, http.StateNew)
	snapshot = stats.connectionSnapshot()
	if snapshot.accepted != 2 || snapshot.open != 2 || snapshot.peak != 2 {
		t.Fatalf("after second connection: got %+v, want accepted=2 open=2 peak=2", snapshot)
	}

	stats.connectionState(first, http.StateHijacked)
	stats.connectionState(first, http.StateClosed)
	snapshot = stats.connectionSnapshot()
	if snapshot.accepted != 2 || snapshot.open != 1 || snapshot.peak != 2 {
		t.Fatalf("hijacked connection was not decremented exactly once: %+v", snapshot)
	}

	stats.connectionState(second, http.StateClosed)
	stats.connectionState(second, http.StateClosed)
	snapshot = stats.connectionSnapshot()
	if snapshot.accepted != 2 || snapshot.open != 0 || snapshot.peak != 2 {
		t.Fatalf("closed connection was not decremented exactly once: %+v", snapshot)
	}

	stats.resetInterval()
	snapshot = stats.connectionSnapshot()
	if snapshot.accepted != 2 || snapshot.open != 0 || snapshot.peak != 0 {
		t.Fatalf("reset at zero open connections returned incorrect snapshot: %+v", snapshot)
	}
}

func TestUpstreamErrorClassification(t *testing.T) {
	timeoutErr := errors.Join(errors.New("wrapped"), testTimeoutError("upstream timed out"))
	if !isTimeoutError(timeoutErr) {
		t.Fatal("wrapped timeout error was classified as non-timeout")
	}

	otherErr := &url.Error{Op: "Get", URL: "http://upstream.test/", Err: errors.New("connection refused")}
	if isTimeoutError(otherErr) {
		t.Fatal("non-timeout transport error was classified as timeout")
	}

	stats := &counters{}
	stats.recordUpstreamError(timeoutErr)
	stats.recordUpstreamError(otherErr)
	stats.recordUpstreamError(errors.New("buffer upstream response: read failed"))
	if got := stats.errors.Load(); got != 3 {
		t.Fatalf("proxy error total = %d, want 3", got)
	}
	if got := stats.upstreamTimeoutErrors.Load(); got != 1 {
		t.Fatalf("proxy timeout errors = %d, want 1", got)
	}
	if got := stats.upstreamOtherTransportErrors.Load(); got != 1 {
		t.Fatalf("other upstream transport errors = %d, want 1", got)
	}
	if got := stats.responseProcessingErrors.Load(); got != 1 {
		t.Fatalf("response processing errors = %d, want 1", got)
	}
}

type testTimeoutError string

func (err testTimeoutError) Error() string { return string(err) }
func (testTimeoutError) Timeout() bool     { return true }
func (testTimeoutError) Temporary() bool   { return true }

func TestUpstreamConnectionLimitBoundsConcurrentRequests(t *testing.T) {
	var active atomic.Int64
	var peak atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		current := active.Add(1)
		defer active.Add(-1)
		for previous := peak.Load(); current > previous; previous = peak.Load() {
			if peak.CompareAndSwap(previous, current) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		_, _ = fmt.Fprintln(w, "ok")
	}))
	defer upstream.Close()

	const limit = 2
	transport := newUpstreamTransport(true, limit)
	defer transport.CloseIdleConnections()
	if transport.MaxConnsPerHost != limit {
		t.Fatalf("MaxConnsPerHost = %d, want %d", transport.MaxConnsPerHost, limit)
	}
	proxy := httputil.NewSingleHostReverseProxy(mustParseURL(t, upstream.URL))
	proxy.Transport = transport

	const requests = 12
	var wg sync.WaitGroup
	statuses := make(chan int, requests)
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "http://proxy.test/delay", nil)
			proxy.ServeHTTP(recorder, request)
			statuses <- recorder.Code
		}()
	}
	wg.Wait()
	close(statuses)
	for status := range statuses {
		if status != http.StatusOK {
			t.Fatalf("proxied request status = %d, want %d", status, http.StatusOK)
		}
	}
	if got := peak.Load(); got != limit {
		t.Fatalf("peak active upstream requests = %d, want limit %d", got, limit)
	}
}

func TestBoundedResponseBufferPreservesSmallResponse(t *testing.T) {
	payload := bytes.Repeat([]byte("small response"), 100)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Upstream-Test", "preserved")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(payload)
	}))
	defer upstream.Close()

	proxy := httputil.NewSingleHostReverseProxy(mustParseURL(t, upstream.URL))
	proxy.ModifyResponse = boundedResponseBuffer(int64(len(payload)))
	server := httptest.NewServer(proxy)
	defer server.Close()

	response, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusCreated || response.Header.Get("X-Upstream-Test") != "preserved" {
		t.Fatalf("response metadata changed: status=%d header=%q", response.StatusCode, response.Header.Get("X-Upstream-Test"))
	}
	if response.ContentLength != int64(len(payload)) {
		t.Fatalf("Content-Length = %d, want %d", response.ContentLength, len(payload))
	}
	if !bytes.Equal(body, payload) {
		t.Fatal("buffered response body differs from upstream body")
	}
}

func TestBoundedResponseBufferStreamsAfterLimit(t *testing.T) {
	payload := bytes.Repeat([]byte("large response"), 1000)
	source := &countingReadCloser{Reader: bytes.NewReader(payload)}
	response := &http.Response{
		StatusCode:    http.StatusPartialContent,
		Header:        http.Header{"X-Upstream-Test": []string{"preserved"}},
		ContentLength: int64(len(payload)),
		Body:          source,
	}
	const limit = 1024
	if err := boundedResponseBuffer(limit)(response); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusPartialContent ||
		response.Header.Get("X-Upstream-Test") != "preserved" ||
		response.ContentLength != int64(len(payload)) {
		t.Fatalf("overflow response metadata changed: status=%d header=%q length=%d",
			response.StatusCode, response.Header.Get("X-Upstream-Test"), response.ContentLength)
	}
	if source.read != limit+1 {
		t.Fatalf("pre-read %d bytes before streaming, want cap plus one overflow byte (%d)", source.read, limit+1)
	}

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, payload) {
		t.Fatal("streamed response body differs from upstream body")
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if !source.closed {
		t.Fatal("overflow response did not close upstream body")
	}
}

func TestBoundedResponseBufferCancellationReachesUpstream(t *testing.T) {
	upstreamStarted := make(chan struct{})
	upstreamCanceled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "partial")
		w.(http.Flusher).Flush()
		close(upstreamStarted)
		<-r.Context().Done()
		close(upstreamCanceled)
	}))
	defer upstream.Close()

	proxy := httputil.NewSingleHostReverseProxy(mustParseURL(t, upstream.URL))
	proxy.Transport = newUpstreamTransport(true, 0)
	defer proxy.Transport.(*http.Transport).CloseIdleConnections()
	proxy.ModifyResponse = boundedResponseBuffer(1024)
	server := httptest.NewServer(proxy)
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
		if err != nil {
			done <- err
			return
		}
		response, err := http.DefaultClient.Do(request)
		if response != nil {
			_ = response.Body.Close()
		}
		done <- err
	}()

	select {
	case <-upstreamStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream request did not start")
	}
	cancel()
	select {
	case <-upstreamCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream did not observe downstream cancellation during buffering")
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("request unexpectedly completed after downstream cancellation")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("downstream request did not return after cancellation")
	}
}

func TestBoundedResponseBufferDisconnectClosesStreamingBody(t *testing.T) {
	upstreamStarted := make(chan struct{})
	upstreamCanceled := make(chan struct{})
	payload := bytes.Repeat([]byte("x"), 1<<20)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
		w.(http.Flusher).Flush()
		close(upstreamStarted)
		<-r.Context().Done()
		close(upstreamCanceled)
	}))
	defer upstream.Close()

	proxy := httputil.NewSingleHostReverseProxy(mustParseURL(t, upstream.URL))
	proxy.Transport = newUpstreamTransport(true, 0)
	defer proxy.Transport.(*http.Transport).CloseIdleConnections()
	proxy.ModifyResponse = boundedResponseBuffer(1024)
	server := httptest.NewServer(proxy)
	defer server.Close()

	response, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.CopyN(io.Discard, response.Body, 128)
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-upstreamStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream did not start streaming")
	}
	select {
	case <-upstreamCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream did not observe downstream disconnect")
	}
}

type countingReadCloser struct {
	io.Reader
	read   int64
	closed bool
}

func (body *countingReadCloser) Read(p []byte) (int, error) {
	n, err := body.Reader.Read(p)
	body.read += int64(n)
	return n, err
}

func (body *countingReadCloser) Close() error {
	body.closed = true
	return nil
}

func mustParseURL(t *testing.T, rawURL string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}
