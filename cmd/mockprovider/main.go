// Command mockprovider is a local stand-in for webhook.site: it accepts the provider request
// format and answers 202 with a messageId, or fails on purpose to exercise retries.
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
	mux := http.NewServeMux()
	mux.HandleFunc("POST /send", func(w http.ResponseWriter, r *http.Request) {
		var req request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.To == "" || req.Content == "" {
			http.Error(w, `{"error":"invalid request"}`, http.StatusBadRequest)
			return
		}
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
		_ = json.NewEncoder(w).Encode(map[string]int64{"accepted": accepted.Load(), "failed": failed.Load()})
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	log.Info("listening", "addr", addr, "failure_rate", failureRate, "latency", latency.String())
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		log.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
