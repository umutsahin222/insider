// Command mockprovider is a local stand-in for webhook.site: it accepts the provider request
// format and answers 202 with a messageId, or fails on purpose to exercise retries.
//
// GET /stats reports accepted and failed requests and, per channel, the most requests that
// arrived within any one second (peak_per_second): the rate limit as the provider sees it.
//
// Environment:
//
//	HTTP_ADDR     listen address (default :8090)
//	FAILURE_RATE  fraction of requests that fail, 0..1 (default 0)
//	LATENCY       base response latency, e.g. 50ms (default 20ms); up to +100% jitter is added
package main

import (
	"encoding/json"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

type request struct {
	To      string `json:"to"`
	Channel string `json:"channel"`
	Content string `json:"content"`
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "mockprovider")
	addr := getenv("HTTP_ADDR", ":8090")
	failureRate, _ := strconv.ParseFloat(getenv("FAILURE_RATE", "0"), 64)
	latency, err := time.ParseDuration(getenv("LATENCY", "20ms"))
	if err != nil {
		log.Error("invalid LATENCY", "error", err)
		os.Exit(1)
	}

	var accepted, failed atomic.Int64
	peaks := newPeakTracker()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /send", func(w http.ResponseWriter, r *http.Request) {
		arrived := time.Now()
		var req request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.To == "" || req.Content == "" {
			http.Error(w, `{"error":"invalid request"}`, http.StatusBadRequest)
			return
		}
		peaks.observe(req.Channel, arrived)
		if latency > 0 {
			time.Sleep(latency + time.Duration(rand.Int64N(int64(latency))))
		}
		if rand.Float64() < failureRate {
			failed.Add(1)
			// A mix of transient failures the worker should retry.
			switch rand.IntN(3) {
			case 0:
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusTooManyRequests)
			case 1:
				w.WriteHeader(http.StatusServiceUnavailable)
			default:
				w.WriteHeader(http.StatusInternalServerError)
			}
			return
		}
		accepted.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"messageId": uuid.NewString(),
			"status":    "accepted",
			"timestamp": time.Now().UTC().Format(time.RFC3339),
		})
		log.Debug("accepted", "channel", req.Channel, "idempotency_key", r.Header.Get("X-Idempotency-Key"))
	})
	mux.HandleFunc("GET /stats", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accepted": accepted.Load(), "failed": failed.Load(), "peak_per_second": peaks.snapshot(),
		})
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	log.Info("listening", "addr", addr, "failure_rate", failureRate, "latency", latency.String())
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		log.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

// peakTracker records, per channel, the most requests that arrived within any one second.
type peakTracker struct {
	mu     sync.Mutex
	recent map[string][]time.Time // arrivals within the last second, oldest first
	peak   map[string]int
}

func newPeakTracker() *peakTracker {
	return &peakTracker{recent: map[string][]time.Time{}, peak: map[string]int{}}
}

func (p *peakTracker) observe(channel string, at time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	q := p.recent[channel]
	i := 0
	for i < len(q) && !q[i].After(at.Add(-time.Second)) {
		i++
	}
	q = append(q[i:], at) // arrivals in (at-1s, at]
	p.recent[channel] = q
	p.peak[channel] = max(p.peak[channel], len(q))
}

func (p *peakTracker) snapshot() map[string]int {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]int, len(p.peak))
	for ch, n := range p.peak {
		out[ch] = n
	}
	return out
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
