package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"
)

func main() {
	listenAddr := flag.String("listen", "127.0.0.1:19090", "upstream listen address")
	flag.Parse()

	var acceptedConnections atomic.Uint64
	var activeDelayRequests atomic.Int64
	var peakDelayRequests atomic.Int64
	var activePayloadRequests atomic.Int64
	var peakPayloadRequests atomic.Int64
	var failureMode atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("POST /__control/failure", func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
			http.Error(w, "control endpoint is loopback-only", http.StatusForbidden)
			return
		}
		enabled, err := strconv.ParseBool(r.URL.Query().Get("enabled"))
		if err != nil {
			http.Error(w, "enabled must be true or false", http.StatusBadRequest)
			return
		}
		failureMode.Store(enabled)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/hello", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "hello from upstream\n")
	})
	mux.HandleFunc("/delay", func(w http.ResponseWriter, r *http.Request) {
		active := activeDelayRequests.Add(1)
		defer activeDelayRequests.Add(-1)
		updatePeak(&peakDelayRequests, active)

		delay := 100 * time.Millisecond
		if raw := r.URL.Query().Get("ms"); raw != "" {
			if parsed, err := time.ParseDuration(raw + "ms"); err == nil && parsed >= 0 && parsed <= 10*time.Second {
				delay = parsed
			}
		}
		time.Sleep(delay)
		fmt.Fprintf(w, "delayed %s\n", delay)
	})
	mux.HandleFunc("/payload/", func(w http.ResponseWriter, r *http.Request) {
		active := activePayloadRequests.Add(1)
		defer activePayloadRequests.Add(-1)
		updatePeak(&peakPayloadRequests, active)

		var size int64
		slow := false
		switch r.URL.Path {
		case "/payload/64k":
			size = 64 << 10
		case "/payload/1m":
			size = 1 << 20
		case "/payload/4m":
			size = 4 << 20
		case "/payload/slow":
			size = 1 << 20
			slow = true
		default:
			http.NotFound(w, r)
			return
		}
		chunkSize := 32 << 10
		delay := time.Duration(0)
		if slow {
			chunkSize = 1024
			delay = time.Millisecond
		}
		if err := writePayload(w, size, chunkSize, delay, slow); err != nil {
			log.Printf("payload_write_error path=%s error=%q", r.URL.Path, err)
		}
	})
	mux.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "accepted_connections %d\nactive_delay_requests %d\npeak_delay_requests %d\n"+
			"active_payload_requests %d\npeak_payload_requests %d\n",
			acceptedConnections.Load(), activeDelayRequests.Load(), peakDelayRequests.Load(),
			activePayloadRequests.Load(), peakPayloadRequests.Load())
	})
	mux.HandleFunc("POST /stats/reset", func(w http.ResponseWriter, _ *http.Request) {
		acceptedConnections.Store(0)
		peakDelayRequests.Store(activeDelayRequests.Load())
		peakPayloadRequests.Store(activePayloadRequests.Load())
		w.WriteHeader(http.StatusNoContent)
	})
	server := &http.Server{
		Addr: *listenAddr,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if failureMode.Load() && r.URL.Path != "/__control/failure" &&
				r.URL.Path != "/stats" && r.URL.Path != "/stats/reset" {
				hijacker, ok := w.(http.Hijacker)
				if !ok {
					http.Error(w, "connection hijacking unavailable", http.StatusInternalServerError)
					return
				}
				connection, _, err := hijacker.Hijack()
				if err != nil {
					log.Printf("failure_mode_hijack_error error=%q", err)
					return
				}
				_ = connection.Close()
				return
			}
			mux.ServeHTTP(w, r)
		}),
		ReadHeaderTimeout: 3 * time.Second,
		IdleTimeout:       30 * time.Second,
		ConnState: func(_ net.Conn, state http.ConnState) {
			if state == http.StateNew {
				acceptedConnections.Add(1)
			}
		},
	}
	log.Printf("demo upstream listening on %s", server.Addr)
	log.Fatal(server.ListenAndServe())
}

func updatePeak(peak *atomic.Int64, current int64) {
	for previous := peak.Load(); current > previous; previous = peak.Load() {
		if peak.CompareAndSwap(previous, current) {
			return
		}
	}
}

func writePayload(w http.ResponseWriter, size int64, chunkSize int, delay time.Duration, flush bool) error {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	chunk := bytes.Repeat([]byte("0123456789abcdef"), (chunkSize+15)/16)[:chunkSize]
	remaining := size
	for remaining > 0 {
		n := int64(len(chunk))
		if n > remaining {
			n = remaining
		}
		if delay > 0 {
			time.Sleep(delay)
		}
		if _, err := io.CopyN(w, bytes.NewReader(chunk[:n]), n); err != nil {
			return err
		}
		remaining -= n
		if flush {
			if err := http.NewResponseController(w).Flush(); err != nil {
				return err
			}
		}
	}
	return nil
}
