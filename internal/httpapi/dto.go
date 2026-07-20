package httpapi

type createKeyRequest struct {
	APIKey            string `json:"api_key"`
	Capacity          uint64 `json:"capacity"`
	RefillPerInterval uint32 `json:"refill_per_interval"`
}

type createKeyResponse struct {
	APIKey            string `json:"api_key"`
	Capacity          uint64 `json:"capacity"`
	RefillPerInterval uint32 `json:"refill_per_interval"`
	Created           bool   `json:"created"`
}

type reserveRequest struct {
	APIKey string `json:"api_key"`
	Cost   uint64 `json:"cost"`
}

type reserveResponse struct {
	Allowed        bool   `json:"allowed"`
	ReservationID  string `json:"reservation_id,omitempty"`
	TimeoutSeconds uint32 `json:"timeout_seconds,omitempty"`
	Reason         string `json:"reason,omitempty"`
}

type settleRequest struct {
	Amount *uint64 `json:"amount,omitempty"`
}

type settleResponse struct {
	Status string `json:"status"`
}

type balanceResponse struct {
	APIKey            string `json:"api_key"`
	Capacity          uint64 `json:"capacity"`
	RefillPerInterval uint32 `json:"refill_per_interval"`
	Tokens            uint64 `json:"tokens"`
	Used              uint64 `json:"used"`
	Pending           uint64 `json:"pending"`
	Available         uint64 `json:"available"`
}

type errorResponse struct {
	Error string `json:"error"`
}
