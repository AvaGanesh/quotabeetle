// Package httpapi exposes internal/ratelimit as an HTTP service so that
// services in any language can check-and-decrement quota over the network:
//
//	POST /v1/keys                                  provision/top-up a key's quota
//	GET  /v1/keys/{apiKey}/balance                 inspect current balance
//	POST /v1/quota/reserve                         reserve capacity (two-phase pending transfer)
//	POST /v1/quota/reservations/{id}/commit        finalize a reservation
//	POST /v1/quota/reservations/{id}/void          release a reservation
package httpapi

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"

	tb "github.com/tigerbeetle/tigerbeetle-go"

	"github.com/avaganesh/api-ratelimiter/internal/ratelimit"
)

type Server struct {
	ledger *ratelimit.Ledger
	mux    *http.ServeMux
}

func New(ledger *ratelimit.Ledger) *Server {
	s := &Server{ledger: ledger, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealth)
	s.mux.HandleFunc("POST /v1/keys", s.handleCreateKey)
	s.mux.HandleFunc("GET /v1/keys/{apiKey}/balance", s.handleBalance)
	s.mux.HandleFunc("POST /v1/quota/reserve", s.handleReserve)
	s.mux.HandleFunc("POST /v1/quota/reservations/{id}/commit", s.handleCommit)
	s.mux.HandleFunc("POST /v1/quota/reservations/{id}/void", s.handleVoid)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	var req createKeyRequest
	if !decode(w, r, &req) {
		return
	}
	if req.APIKey == "" || req.Quota == 0 {
		writeError(w, http.StatusBadRequest, "api_key and a non-zero quota are required")
		return
	}

	if _, err := s.ledger.Grant(req.APIKey, req.Quota); err != nil {
		log.Printf("grant failed: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to grant quota")
		return
	}
	writeJSON(w, http.StatusCreated, createKeyResponse{APIKey: req.APIKey, Granted: req.Quota})
}

func (s *Server) handleBalance(w http.ResponseWriter, r *http.Request) {
	apiKey := r.PathValue("apiKey")

	balance, err := s.ledger.GetBalance(apiKey)
	if err != nil {
		if errors.Is(err, ratelimit.ErrUnknownAPIKey) {
			writeError(w, http.StatusNotFound, "unknown api key")
			return
		}
		log.Printf("balance lookup failed: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to look up balance")
		return
	}

	writeJSON(w, http.StatusOK, balanceResponse{
		APIKey:    apiKey,
		Granted:   balance.Granted,
		Used:      balance.Used,
		Pending:   balance.Pending,
		Available: balance.Available,
	})
}

func (s *Server) handleReserve(w http.ResponseWriter, r *http.Request) {
	var req reserveRequest
	if !decode(w, r, &req) {
		return
	}
	if req.APIKey == "" {
		writeError(w, http.StatusBadRequest, "api_key is required")
		return
	}
	if req.Cost == 0 {
		req.Cost = 1
	}

	result, err := s.ledger.Reserve(req.APIKey, req.Cost)
	if err != nil {
		if errors.Is(err, ratelimit.ErrUnknownAPIKey) {
			writeError(w, http.StatusNotFound, "unknown api key")
			return
		}
		log.Printf("reserve failed: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to reserve quota")
		return
	}

	if !result.Allowed {
		writeJSON(w, http.StatusTooManyRequests, reserveResponse{Allowed: false, Reason: "quota_exceeded"})
		return
	}
	writeJSON(w, http.StatusOK, reserveResponse{
		Allowed:        true,
		ReservationID:  result.ReservationID.String(),
		TimeoutSeconds: result.TimeoutSeconds,
	})
}

func (s *Server) handleCommit(w http.ResponseWriter, r *http.Request) { s.settle(w, r, true) }
func (s *Server) handleVoid(w http.ResponseWriter, r *http.Request)   { s.settle(w, r, false) }

func (s *Server) settle(w http.ResponseWriter, r *http.Request, isCommit bool) {
	reservationID, err := tb.HexStringToUint128(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid reservation id")
		return
	}

	var amount *uint64
	if isCommit && r.ContentLength != 0 {
		var req settleRequest
		if !decode(w, r, &req) {
			return
		}
		amount = req.Amount
	}

	var settleErr error
	if isCommit {
		settleErr = s.ledger.Commit(reservationID, amount)
	} else {
		settleErr = s.ledger.Void(reservationID)
	}

	if settleErr != nil {
		switch {
		case errors.Is(settleErr, ratelimit.ErrReservationNotFound):
			writeError(w, http.StatusNotFound, "reservation not found")
		case errors.Is(settleErr, ratelimit.ErrReservationExpired):
			writeError(w, http.StatusConflict, "reservation expired")
		case errors.Is(settleErr, ratelimit.ErrAlreadySettled):
			writeError(w, http.StatusConflict, "reservation already settled")
		case errors.Is(settleErr, ratelimit.ErrAmountExceedsReservation):
			writeError(w, http.StatusBadRequest, "amount exceeds reservation")
		default:
			log.Printf("settle failed: %v", settleErr)
			writeError(w, http.StatusInternalServerError, "failed to settle reservation")
		}
		return
	}

	status := "voided"
	if isCommit {
		status = "committed"
	}
	writeJSON(w, http.StatusOK, settleResponse{Status: status})
}

func decode(w http.ResponseWriter, r *http.Request, v interface{}) bool {
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
}
