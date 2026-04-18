package server

import (
	"net/http"
	"strconv"
	"time"
)

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status": "ok",
		"time":   time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *Server) handleSystemBalances(w http.ResponseWriter, r *http.Request) {
	rollups, err := s.queries.GetSystemRollups(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": rollups})
}

// --- Reconciliation ---

func (s *Server) handleReconcileGlobal(w http.ResponseWriter, r *http.Request) {
	result, err := s.reconciler.CheckAccountingEquation(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "reconcile_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

type reconcileAccountRequest struct {
	Holder     int64 `json:"holder"`
	CurrencyID int64 `json:"currency_id"`
}

func (s *Server) handleReconcileAccount(w http.ResponseWriter, r *http.Request) {
	var req reconcileAccountRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if req.Holder == 0 || req.CurrencyID == 0 {
		writeError(w, http.StatusBadRequest, "invalid_params", "holder and currency_id required")
		return
	}

	result, err := s.reconciler.ReconcileAccount(r.Context(), req.Holder, req.CurrencyID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "reconcile_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// --- Snapshots ---

func (s *Server) handleListSnapshots(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	holder, err := strconv.ParseInt(q.Get("holder"), 10, 64)
	if err != nil || holder == 0 {
		writeError(w, http.StatusBadRequest, "invalid_params", "holder is required")
		return
	}
	currencyID, err := strconv.ParseInt(q.Get("currency_id"), 10, 64)
	if err != nil || currencyID == 0 {
		writeError(w, http.StatusBadRequest, "invalid_params", "currency_id is required")
		return
	}

	startStr := q.Get("start")
	endStr := q.Get("end")
	if startStr == "" || endStr == "" {
		writeError(w, http.StatusBadRequest, "invalid_params", "start and end date required (YYYY-MM-DD)")
		return
	}
	start, err := time.Parse("2006-01-02", startStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_params", "invalid start date format")
		return
	}
	end, err := time.Parse("2006-01-02", endStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_params", "invalid end date format")
		return
	}

	snapshots, err := s.queries.ListSnapshotsByDateRange(r.Context(), holder, currencyID, start, end)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	type snapshotResp struct {
		AccountHolder    int64  `json:"account_holder"`
		CurrencyID       int64  `json:"currency_id"`
		ClassificationID int64  `json:"classification_id"`
		SnapshotDate     string `json:"snapshot_date"`
		Balance          string `json:"balance"`
	}
	data := make([]snapshotResp, len(snapshots))
	for i, s := range snapshots {
		data[i] = snapshotResp{
			AccountHolder:    s.AccountHolder,
			CurrencyID:       s.CurrencyID,
			ClassificationID: s.ClassificationID,
			SnapshotDate:     s.SnapshotDate.Format("2006-01-02"),
			Balance:          s.Balance.String(),
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data})
}
