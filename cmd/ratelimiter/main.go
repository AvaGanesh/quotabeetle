package main

import (
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	tb "github.com/tigerbeetle/tigerbeetle-go"

	"github.com/avaganesh/api-ratelimiter/internal/httpapi"
	"github.com/avaganesh/api-ratelimiter/internal/ratelimit"
)

func main() {
	clusterID := envUint64("TB_CLUSTER_ID", 0)
	address := envString("TB_ADDRESS", "127.0.0.1:3000")
	port := envString("PORT", "8080")
	reservationTimeout := envDuration("RESERVATION_TIMEOUT", 60*time.Second)
	refillInterval := envDuration("REFILL_INTERVAL", 1*time.Second)

	client, err := tb.NewClient(tb.ToUint128(clusterID), []string{address})
	if err != nil {
		log.Fatalf("connect to tigerbeetle at %s: %v", address, err)
	}
	defer client.Close()

	ledger := ratelimit.New(client, reservationTimeout)
	if err := ledger.Bootstrap(); err != nil {
		log.Fatalf("bootstrap ledger: %v", err)
	}

	go runRefillLoop(ledger, refillInterval)

	server := httpapi.New(ledger)
	log.Printf("api-ratelimiter listening on :%s (tigerbeetle at %s, refill every %s)", port, address, refillInterval)
	if err := http.ListenAndServe(":"+port, server); err != nil {
		log.Fatal(err)
	}
}

// runRefillLoop drives token-bucket replenishment: every interval, it tops
// up each API key's bucket by its configured refill amount, capped at its
// capacity. This is what turns the static per-key balance into a steady
// request rate with bursts allowed up to capacity.
func runRefillLoop(ledger *ratelimit.Ledger, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		if n, err := ledger.Refill(); err != nil {
			log.Printf("refill: %v", err)
		} else if n > 0 {
			log.Printf("refill: topped up %d bucket(s)", n)
		}
	}
}

func envString(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envUint64(key string, def uint64) uint64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
