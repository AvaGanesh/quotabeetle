// Package ratelimit implements a token-bucket API quota ledger on top of
// TigerBeetle.
//
// Each API key is a TigerBeetle account with DebitsMustNotExceedCredits set,
// so its available balance (tokens granted minus tokens used minus tokens
// pending) can never go negative under any amount of concurrency -
// TigerBeetle processes transfers against a single account strictly one at a
// time, which is what rules out double-spend without any locking on our
// side. Every request reserves capacity with a pending (two-phase) transfer;
// callers then post (commit) or void (release) it once they know whether the
// downstream work succeeded.
//
// The bucket's capacity and per-tick refill amount are stored on the
// account itself (UserData64 and UserData32) at creation time. A periodic
// call to Refill tops up every key's bucket by its refill amount, capped at
// its capacity - the classic token bucket: requests drain the bucket,
// Refill replenishes it at a steady rate, and bursts up to capacity are
// allowed whenever the bucket is full.
package ratelimit

import (
	"fmt"
	"math"
	"time"

	tb "github.com/tigerbeetle/tigerbeetle-go"
)

const (
	ledgerID = 1

	issuerCode = 1
	sinkCode   = 2
	apiKeyCode = 3

	grantCode = 1
	usageCode = 2
)

var (
	// issuerAccountID is the source account that quota grants are debited
	// from. It carries no balance constraints, so it can fund any number
	// of API key accounts.
	issuerAccountID = tb.ToUint128(1)

	// sinkAccountID is the destination for every usage transfer. Its
	// posted balance is effectively a running total of settled request
	// cost across all keys; it isn't otherwise used by this service.
	sinkAccountID = tb.ToUint128(2)
)

type Ledger struct {
	client                    tb.Client
	defaultReservationTimeout uint32 // seconds
}

// New wraps a TigerBeetle client with quota-ledger operations. defaultReservationTimeout
// is how long a reservation may stay pending before TigerBeetle auto-voids it.
func New(client tb.Client, defaultReservationTimeout time.Duration) *Ledger {
	return &Ledger{
		client:                    client,
		defaultReservationTimeout: uint32(defaultReservationTimeout.Seconds()),
	}
}

// Bootstrap idempotently creates the system accounts used internally. Safe
// to call on every startup.
func (l *Ledger) Bootstrap() error {
	results, err := l.client.CreateAccounts([]tb.Account{
		{ID: issuerAccountID, Ledger: ledgerID, Code: issuerCode},
		{ID: sinkAccountID, Ledger: ledgerID, Code: sinkCode},
	})
	if err != nil {
		return fmt.Errorf("bootstrap accounts: %w", err)
	}
	for i, r := range results {
		if !accountOK(r.Status) {
			return fmt.Errorf("bootstrap account %d: %s", i, r.Status)
		}
	}
	return nil
}

func accountOK(status tb.CreateAccountStatus) bool {
	return status == tb.AccountCreated || status == tb.AccountExists
}

// queryBatchMax is TigerBeetle's per-request batch limit; it bounds both how
// many accounts a single QueryAccounts call returns and how many transfers a
// single CreateTransfers call accepts.
const queryBatchMax = 8189

// CreateKey provisions an API key's bucket: capacity is its burst size (the
// most tokens it can ever hold) and refillPerInterval is how many tokens
// Refill adds to it each tick. The bucket starts full. capacity and
// refillPerInterval are fixed at creation - calling CreateKey again for the
// same key is a no-op that reports created=false rather than resetting or
// re-filling the bucket; use a different key to change either value. A
// refillPerInterval of 0 disables automatic refill, leaving a static quota.
func (l *Ledger) CreateKey(apiKey string, capacity uint64, refillPerInterval uint32) (created bool, err error) {
	id := DeriveAccountID(apiKey)

	results, err := l.client.CreateAccounts([]tb.Account{
		{
			ID:         id,
			Ledger:     ledgerID,
			Code:       apiKeyCode,
			UserData64: capacity,
			UserData32: refillPerInterval,
			Flags:      tb.AccountFlags{DebitsMustNotExceedCredits: true}.ToUint16(),
		},
	})
	if err != nil {
		return false, fmt.Errorf("create key account: %w", err)
	}

	switch results[0].Status {
	case tb.AccountCreated:
		created = true
	case tb.AccountExists:
		return false, nil
	case tb.AccountExistsWithDifferentUserData64, tb.AccountExistsWithDifferentUserData32:
		return false, ErrKeyConfigMismatch
	default:
		return false, fmt.Errorf("create key account: %s", results[0].Status)
	}

	if capacity == 0 {
		return created, nil
	}

	transferResults, err := l.client.CreateTransfers([]tb.Transfer{
		{
			ID:              tb.ID(),
			DebitAccountID:  issuerAccountID,
			CreditAccountID: id,
			Amount:          tb.ToUint128(capacity),
			Ledger:          ledgerID,
			Code:            grantCode,
		},
	})
	if err != nil {
		return created, fmt.Errorf("initial fill transfer: %w", err)
	}
	if status := transferResults[0].Status; status != tb.TransferCreated && status != tb.TransferExists {
		return created, fmt.Errorf("initial fill transfer: %s", status)
	}
	return created, nil
}

// Refill tops up every API key's bucket by its configured refill amount,
// never exceeding its capacity. Call it on a timer (see cmd/ratelimiter) to
// get standard token-bucket behavior: a steady replenishment rate with
// bursts up to each key's capacity. It returns how many buckets were
// topped up.
func (l *Ledger) Refill() (int, error) {
	var transfers []tb.Transfer

	var timestampMin uint64
	for {
		accounts, err := l.client.QueryAccounts(tb.QueryFilter{
			Ledger:       ledgerID,
			Code:         apiKeyCode,
			TimestampMin: timestampMin,
			Limit:        queryBatchMax,
		})
		if err != nil {
			return 0, fmt.Errorf("query key accounts: %w", err)
		}

		for _, a := range accounts {
			capacity := a.UserData64
			refillAmount := uint64(a.UserData32)
			if refillAmount == 0 {
				continue
			}

			// CreditsPosted is a monotonically increasing total-ever-granted
			// counter, not "tokens currently in the bucket" - it already sits
			// at capacity right after the initial full fill and never drops
			// as the bucket is spent. The quantity that must stay <=
			// capacity is the available balance, so headroom is measured
			// against that, not against CreditsPosted directly.
			posted := toUint64(a.CreditsPosted)
			used := toUint64(a.DebitsPosted)
			pending := toUint64(a.DebitsPending)
			var available uint64
			if posted > used+pending {
				available = posted - used - pending
			}
			if available >= capacity {
				continue
			}

			amount := refillAmount
			if headroom := capacity - available; amount > headroom {
				amount = headroom
			}
			transfers = append(transfers, tb.Transfer{
				ID:              tb.ID(),
				DebitAccountID:  issuerAccountID,
				CreditAccountID: a.ID,
				Amount:          tb.ToUint128(amount),
				Ledger:          ledgerID,
				Code:            grantCode,
			})
		}

		if len(accounts) < queryBatchMax {
			break
		}
		timestampMin = accounts[len(accounts)-1].Timestamp + 1
	}

	refilled := 0
	for start := 0; start < len(transfers); start += queryBatchMax {
		end := start + queryBatchMax
		if end > len(transfers) {
			end = len(transfers)
		}
		chunk := transfers[start:end]

		results, err := l.client.CreateTransfers(chunk)
		if err != nil {
			return refilled, fmt.Errorf("refill transfers: %w", err)
		}
		for i, r := range results {
			if r.Status != tb.TransferCreated && r.Status != tb.TransferExists {
				return refilled, fmt.Errorf("refill transfer for account %s: %s", chunk[i].CreditAccountID, r.Status)
			}
			refilled++
		}
	}
	return refilled, nil
}

type ReserveResult struct {
	Allowed        bool
	ReservationID  tb.Uint128
	TimeoutSeconds uint32
}

// Reserve atomically checks and decrements cost units of quota for apiKey,
// using this Ledger's default reservation timeout. It does not finalize the
// spend - call Commit or Void with the returned ReservationID once the
// downstream request has run.
func (l *Ledger) Reserve(apiKey string, cost uint64) (ReserveResult, error) {
	return l.ReserveWithTimeout(apiKey, cost, l.defaultReservationTimeout)
}

func (l *Ledger) ReserveWithTimeout(apiKey string, cost uint64, timeoutSeconds uint32) (ReserveResult, error) {
	id := DeriveAccountID(apiKey)
	reservationID := tb.ID()

	results, err := l.client.CreateTransfers([]tb.Transfer{
		{
			ID:              reservationID,
			DebitAccountID:  id,
			CreditAccountID: sinkAccountID,
			Amount:          tb.ToUint128(cost),
			Ledger:          ledgerID,
			Code:            usageCode,
			Timeout:         timeoutSeconds,
			Flags:           tb.TransferFlags{Pending: true}.ToUint16(),
		},
	})
	if err != nil {
		return ReserveResult{}, fmt.Errorf("reserve transfer: %w", err)
	}

	switch results[0].Status {
	case tb.TransferCreated:
		return ReserveResult{Allowed: true, ReservationID: reservationID, TimeoutSeconds: timeoutSeconds}, nil
	case tb.TransferExceedsCredits:
		return ReserveResult{Allowed: false}, nil
	case tb.TransferDebitAccountNotFound:
		return ReserveResult{}, ErrUnknownAPIKey
	default:
		return ReserveResult{}, fmt.Errorf("reserve transfer: %s", results[0].Status)
	}
}

// Commit finalizes a reservation, permanently spending its quota. If amount
// is nil the full reserved amount is posted; otherwise it must be <= the
// amount originally reserved, and any unused remainder is released back to
// the account.
func (l *Ledger) Commit(reservationID tb.Uint128, amount *uint64) error {
	amt := tb.AmountMax
	if amount != nil {
		amt = tb.ToUint128(*amount)
	}

	results, err := l.client.CreateTransfers([]tb.Transfer{
		{
			ID:        tb.ID(),
			PendingID: reservationID,
			Amount:    amt,
			Ledger:    ledgerID,
			Code:      usageCode,
			Flags:     tb.TransferFlags{PostPendingTransfer: true}.ToUint16(),
		},
	})
	if err != nil {
		return fmt.Errorf("commit transfer: %w", err)
	}
	return classifySettleStatus(results[0].Status)
}

// Void releases a reservation without spending anything, restoring its
// quota to the account. Use this when the downstream work the quota was
// reserved for failed.
func (l *Ledger) Void(reservationID tb.Uint128) error {
	results, err := l.client.CreateTransfers([]tb.Transfer{
		{
			ID:        tb.ID(),
			PendingID: reservationID,
			Ledger:    ledgerID,
			Code:      usageCode,
			Flags:     tb.TransferFlags{VoidPendingTransfer: true}.ToUint16(),
		},
	})
	if err != nil {
		return fmt.Errorf("void transfer: %w", err)
	}
	return classifySettleStatus(results[0].Status)
}

func classifySettleStatus(status tb.CreateTransferStatus) error {
	switch status {
	case tb.TransferCreated, tb.TransferExists:
		return nil
	case tb.TransferPendingTransferNotFound:
		return ErrReservationNotFound
	case tb.TransferPendingTransferExpired:
		return ErrReservationExpired
	case tb.TransferPendingTransferAlreadyPosted, tb.TransferPendingTransferAlreadyVoided:
		return ErrAlreadySettled
	case tb.TransferExceedsPendingTransferAmount:
		return ErrAmountExceedsReservation
	default:
		return fmt.Errorf("settle transfer: %s", status)
	}
}

type Balance struct {
	Capacity          uint64
	RefillPerInterval uint32
	Tokens            uint64 // currently in the bucket (granted minus none yet spent)
	Used              uint64
	Pending           uint64
	Available         uint64
}

// GetBalance returns the current bucket state for an API key.
func (l *Ledger) GetBalance(apiKey string) (Balance, error) {
	id := DeriveAccountID(apiKey)
	accounts, err := l.client.LookupAccounts([]tb.Uint128{id})
	if err != nil {
		return Balance{}, fmt.Errorf("lookup account: %w", err)
	}
	if len(accounts) == 0 {
		return Balance{}, ErrUnknownAPIKey
	}

	a := accounts[0]
	tokens := toUint64(a.CreditsPosted)
	used := toUint64(a.DebitsPosted)
	pending := toUint64(a.DebitsPending)

	var available uint64
	if tokens > used+pending {
		available = tokens - used - pending
	}

	return Balance{
		Capacity:          a.UserData64,
		RefillPerInterval: a.UserData32,
		Tokens:            tokens,
		Used:              used,
		Pending:           pending,
		Available:         available,
	}, nil
}

func toUint64(v tb.Uint128) uint64 {
	lo, hi := v.Uint64()
	if hi != 0 {
		return math.MaxUint64
	}
	return lo
}
