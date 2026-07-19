//go:build integration

// Run against a live single-replica TigerBeetle cluster:
//
//	scripts/dev-tigerbeetle.sh &
//	go test -tags=integration ./test/...
package test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tb "github.com/tigerbeetle/tigerbeetle-go"

	"github.com/avaganesh/api-ratelimiter/internal/httpapi"
	"github.com/avaganesh/api-ratelimiter/internal/ratelimit"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()

	address := os.Getenv("TB_ADDRESS")
	if address == "" {
		address = "127.0.0.1:3000"
	}

	client, err := tb.NewClient(tb.ToUint128(0), []string{address})
	if err != nil {
		t.Fatalf("connect to tigerbeetle at %s (start it with scripts/dev-tigerbeetle.sh): %v", address, err)
	}
	t.Cleanup(client.Close)

	ledger := ratelimit.New(client, 30*time.Second)
	if err := ledger.Bootstrap(); err != nil {
		t.Fatalf("bootstrap ledger: %v", err)
	}

	srv := httptest.NewServer(httpapi.New(ledger))
	t.Cleanup(srv.Close)
	return srv
}

func uniqueKey(t *testing.T) string {
	return fmt.Sprintf("test-key-%s-%d", t.Name(), time.Now().UnixNano())
}

func postJSON(t *testing.T, client *http.Client, url string, body any) (int, map[string]any) {
	t.Helper()

	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode request body: %v", err)
		}
	}

	resp, err := client.Post(url, "application/json", &buf)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()

	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func getJSON(t *testing.T, client *http.Client, url string) (int, map[string]any) {
	t.Helper()

	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()

	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// TestConcurrentReserve_NoDoubleSpend fires far more concurrent reservation
// requests against a single key than it has quota for, and asserts that
// exactly the granted amount succeeds - proving TigerBeetle's per-account
// serialization prevents double-spend under contention.
func TestConcurrentReserve_NoDoubleSpend(t *testing.T) {
	srv := newTestServer(t)
	client := srv.Client()
	key := uniqueKey(t)

	const quota = 100
	const workers = 300

	if status, body := postJSON(t, client, srv.URL+"/v1/keys", map[string]any{"api_key": key, "quota": quota}); status != http.StatusCreated {
		t.Fatalf("create key: status=%d body=%v", status, body)
	}

	var wg sync.WaitGroup
	var allowed, denied int64
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			status, body := postJSON(t, client, srv.URL+"/v1/quota/reserve", map[string]any{"api_key": key, "cost": 1})
			switch status {
			case http.StatusOK:
				if ok, _ := body["allowed"].(bool); ok {
					atomic.AddInt64(&allowed, 1)
				} else {
					atomic.AddInt64(&denied, 1)
				}
			case http.StatusTooManyRequests:
				atomic.AddInt64(&denied, 1)
			default:
				t.Errorf("unexpected status %d: %v", status, body)
			}
		}()
	}
	wg.Wait()

	if allowed != quota {
		t.Fatalf("expected exactly %d reservations to succeed under concurrency, got %d (denied=%d)", quota, allowed, denied)
	}
	if denied != workers-quota {
		t.Fatalf("expected %d denials, got %d", workers-quota, denied)
	}

	status, balance := getJSON(t, client, srv.URL+"/v1/keys/"+key+"/balance")
	if status != http.StatusOK {
		t.Fatalf("balance: status=%d", status)
	}
	if pending := balance["pending"]; pending != float64(quota) {
		t.Fatalf("expected pending=%d, got %v", quota, pending)
	}
	if used := balance["used"]; used != float64(0) {
		t.Fatalf("expected used=0, got %v", used)
	}
	if available := balance["available"]; available != float64(0) {
		t.Fatalf("expected available=0, got %v", available)
	}
}

// TestConcurrentReserveCommitVoid_ReclaimsCapacity exercises the full
// two-phase lifecycle under concurrency: many goroutines reserve, then
// either commit or void their reservation. It asserts the settled balance
// exactly matches what was committed/voided, and that the capacity freed by
// voids - and only that capacity - becomes available for new reservations,
// again fired concurrently.
func TestConcurrentReserveCommitVoid_ReclaimsCapacity(t *testing.T) {
	srv := newTestServer(t)
	client := srv.Client()
	key := uniqueKey(t)

	const quota = 50

	if status, body := postJSON(t, client, srv.URL+"/v1/keys", map[string]any{"api_key": key, "quota": quota}); status != http.StatusCreated {
		t.Fatalf("create key: status=%d body=%v", status, body)
	}

	var wg sync.WaitGroup
	var committed, voided int64
	wg.Add(quota)
	for i := 0; i < quota; i++ {
		i := i
		go func() {
			defer wg.Done()

			status, body := postJSON(t, client, srv.URL+"/v1/quota/reserve", map[string]any{"api_key": key, "cost": 1})
			if status != http.StatusOK {
				t.Errorf("reserve %d: status=%d body=%v", i, status, body)
				return
			}
			if ok, _ := body["allowed"].(bool); !ok {
				t.Errorf("reserve %d: expected allowed=true, got %v", i, body)
				return
			}
			reservationID, _ := body["reservation_id"].(string)

			if i%2 == 0 {
				s, b := postJSON(t, client, fmt.Sprintf("%s/v1/quota/reservations/%s/commit", srv.URL, reservationID), nil)
				if s != http.StatusOK {
					t.Errorf("commit %d: status=%d body=%v", i, s, b)
					return
				}
				atomic.AddInt64(&committed, 1)
			} else {
				s, b := postJSON(t, client, fmt.Sprintf("%s/v1/quota/reservations/%s/void", srv.URL, reservationID), nil)
				if s != http.StatusOK {
					t.Errorf("void %d: status=%d body=%v", i, s, b)
					return
				}
				atomic.AddInt64(&voided, 1)
			}
		}()
	}
	wg.Wait()

	if committed+voided != quota {
		t.Fatalf("expected %d settled reservations, got committed=%d voided=%d", quota, committed, voided)
	}

	// Only the voided capacity should be reclaimable. Fire more concurrent
	// attempts than that so contention would surface any over-commit.
	attempts := int(voided) + 20
	var wg2 sync.WaitGroup
	var reclaimed int64
	wg2.Add(attempts)
	for i := 0; i < attempts; i++ {
		go func() {
			defer wg2.Done()
			status, body := postJSON(t, client, srv.URL+"/v1/quota/reserve", map[string]any{"api_key": key, "cost": 1})
			if status == http.StatusOK {
				if ok, _ := body["allowed"].(bool); ok {
					atomic.AddInt64(&reclaimed, 1)
				}
			}
		}()
	}
	wg2.Wait()

	if reclaimed != voided {
		t.Fatalf("expected exactly %d reclaimed reservations to succeed, got %d", voided, reclaimed)
	}

	status, balance := getJSON(t, client, srv.URL+"/v1/keys/"+key+"/balance")
	if status != http.StatusOK {
		t.Fatalf("balance: status=%d", status)
	}
	if used := balance["used"]; used != float64(committed) {
		t.Fatalf("expected used=%d, got %v", committed, used)
	}
	// The reclaimed reservations from the second wave are still pending (uncommitted).
	if pending := balance["pending"]; pending != float64(voided) {
		t.Fatalf("expected pending=%d, got %v", voided, pending)
	}
}
