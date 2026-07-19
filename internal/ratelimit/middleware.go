package ratelimit

import (
	"errors"
	"net/http"
)

// Middleware returns net/http middleware that reserves cost units of quota
// for the API key in apiKeyHeader (default "X-API-Key") before calling next,
// then commits the reservation if next responds with a non-5xx status or
// voids it otherwise. Use this to embed quota enforcement directly in a Go
// service's own handler chain; services in other languages should call the
// HTTP endpoints in package httpapi instead.
func Middleware(ledger *Ledger, cost uint64, apiKeyHeader string) func(http.Handler) http.Handler {
	if apiKeyHeader == "" {
		apiKeyHeader = "X-API-Key"
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			apiKey := r.Header.Get(apiKeyHeader)
			if apiKey == "" {
				http.Error(w, "missing API key", http.StatusUnauthorized)
				return
			}

			result, err := ledger.Reserve(apiKey, cost)
			if err != nil {
				if errors.Is(err, ErrUnknownAPIKey) {
					http.Error(w, "unknown API key", http.StatusForbidden)
					return
				}
				http.Error(w, "rate limiter unavailable", http.StatusServiceUnavailable)
				return
			}
			if !result.Allowed {
				w.Header().Set("Retry-After", "1")
				http.Error(w, "quota exceeded", http.StatusTooManyRequests)
				return
			}

			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)

			if rec.status >= 500 {
				_ = ledger.Void(result.ReservationID)
			} else {
				_ = ledger.Commit(result.ReservationID, nil)
			}
		})
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusRecorder) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}
