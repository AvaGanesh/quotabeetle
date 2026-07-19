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

	client, err := tb.NewClient(tb.ToUint128(clusterID), []string{address})
	if err != nil {
		log.Fatalf("connect to tigerbeetle at %s: %v", address, err)
	}
	defer client.Close()

	ledger := ratelimit.New(client, reservationTimeout)
	if err := ledger.Bootstrap(); err != nil {
		log.Fatalf("bootstrap ledger: %v", err)
	}

	server := httpapi.New(ledger)
	log.Printf("api-ratelimiter listening on :%s (tigerbeetle at %s)", port, address)
	if err := http.ListenAndServe(":"+port, server); err != nil {
		log.Fatal(err)
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
