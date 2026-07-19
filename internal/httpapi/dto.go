package httpapi

type createKeyRequest struct {
	APIKey string `json:"api_key"`
	Quota  uint64 `json:"quota"`
}

type createKeyResponse struct {
	APIKey  string `json:"api_key"`
	Granted uint64 `json:"granted"`
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
	APIKey    string `json:"api_key"`
	Granted   uint64 `json:"granted"`
	Used      uint64 `json:"used"`
	Pending   uint64 `json:"pending"`
	Available uint64 `json:"available"`
}

type errorResponse struct {
	Error string `json:"error"`
}
