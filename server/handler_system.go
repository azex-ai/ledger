package server

import (
	"net/http"
	"strconv"
	"time"

	"github.com/azex-ai/ledger/pkg/httpx"
)

type healthResponse struct {
	Status                  string `json:"status"`
	RollupQueueDepth        int64  `json:"rollup_queue_depth"`
	CheckpointMaxAgeSeconds int    `json:"checkpoint_max_age_seconds"`
	ActiveReservations      int64  `json:"active_reservations"`
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	metrics, err := s.queries.GetHealthMetrics(r.Context())
	if err != nil {
		httpx.OK(w, healthResponse{Status: "degraded"})
		return
	}
	httpx.OK(w, healthResponse{
		Status:                  "ok",
		RollupQueueDepth:        metrics.RollupQueueDepth,
		CheckpointMaxAgeSeconds: metrics.CheckpointMaxAgeSeconds,
		ActiveReservations:      metrics.ActiveReservations,
	})
}

func (s *Server) handleSystemBalances(w http.ResponseWriter, r *http.Request) {
	rollups, err := s.queries.GetSystemRollups(r.Context())
	if err != nil {
		httpx.Error(w, err)
		return
	}
	type systemBalanceResp struct {
		CurrencyID       int64  `json:"currency_id"`
		ClassificationID int64  `json:"classification_id"`
		TotalBalance     string `json:"total_balance"`
		UpdatedAt        string `json:"updated_at"`
	}
	data := make([]systemBalanceResp, len(rollups))
	for i, r := range rollups {
		data[i] = systemBalanceResp{
			CurrencyID:       r.CurrencyID,
			ClassificationID: r.ClassificationID,
			TotalBalance:     r.TotalBalance.String(),
			UpdatedAt:        r.UpdatedAt.Format(time.RFC3339),
		}
	}
	httpx.OK(w, data)
}

// --- Reconciliation ---

type reconcileDetailResponse struct {
	AccountHolder    int64  `json:"account_holder"`
	CurrencyID       int64  `json:"currency_id"`
	ClassificationID int64  `json:"classification_id"`
	Expected         string `json:"expected"`
	Actual           string `json:"actual"`
	Drift            string `json:"drift"`
}

type reconcileResponse struct {
	Balanced  bool                      `json:"balanced"`
	Gap       string                    `json:"gap"`
	Details   []reconcileDetailResponse `json:"details"`
	CheckedAt time.Time                 `json:"checked_at"`
}

func (s *Server) handleReconcileGlobal(w http.ResponseWriter, r *http.Request) {
	result, err := s.reconciler.CheckAccountingEquation(r.Context())
	if err != nil {
		httpx.Error(w, err)
		return
	}
	details := make([]reconcileDetailResponse, len(result.Details))
	for i, d := range result.Details {
		details[i] = reconcileDetailResponse{
			AccountHolder:    d.AccountHolder,
			CurrencyID:       d.CurrencyID,
			ClassificationID: d.ClassificationID,
			Expected:         d.Expected.String(),
			Actual:           d.Actual.String(),
			Drift:            d.Drift.String(),
		}
	}
	httpx.OK(w, reconcileResponse{
		Balanced:  result.Balanced,
		Gap:       result.Gap.String(),
		Details:   details,
		CheckedAt: result.CheckedAt,
	})
}

type reconcileAccountRequest struct {
	Holder     int64 `json:"holder"`
	CurrencyID int64 `json:"currency_id"`
}

func (s *Server) handleReconcileAccount(w http.ResponseWriter, r *http.Request) {
	req, err := httpx.Decode[reconcileAccountRequest](r)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	if req.Holder == 0 || req.CurrencyID == 0 {
		httpx.Error(w, httpx.ErrBadRequest("holder and currency_id required"))
		return
	}

	result, err := s.reconciler.ReconcileAccount(r.Context(), req.Holder, req.CurrencyID)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	details := make([]reconcileDetailResponse, len(result.Details))
	for i, d := range result.Details {
		details[i] = reconcileDetailResponse{
			AccountHolder:    d.AccountHolder,
			CurrencyID:       d.CurrencyID,
			ClassificationID: d.ClassificationID,
			Expected:         d.Expected.String(),
			Actual:           d.Actual.String(),
			Drift:            d.Drift.String(),
		}
	}
	httpx.OK(w, reconcileResponse{
		Balanced:  result.Balanced,
		Gap:       result.Gap.String(),
		Details:   details,
		CheckedAt: result.CheckedAt,
	})
}

// --- Snapshots ---

func (s *Server) handleListSnapshots(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	holder, err := strconv.ParseInt(q.Get("holder"), 10, 64)
	if err != nil || holder == 0 {
		httpx.Error(w, httpx.ErrBadRequest("holder is required"))
		return
	}
	currencyID, err := strconv.ParseInt(q.Get("currency_id"), 10, 64)
	if err != nil || currencyID == 0 {
		httpx.Error(w, httpx.ErrBadRequest("currency_id is required"))
		return
	}

	startStr := q.Get("start")
	endStr := q.Get("end")
	if startStr == "" || endStr == "" {
		httpx.Error(w, httpx.ErrBadRequest("start and end date required (YYYY-MM-DD)"))
		return
	}
	start, err := time.Parse("2006-01-02", startStr)
	if err != nil {
		httpx.Error(w, httpx.ErrBadRequest("invalid start date format"))
		return
	}
	end, err := time.Parse("2006-01-02", endStr)
	if err != nil {
		httpx.Error(w, httpx.ErrBadRequest("invalid end date format"))
		return
	}

	snapshots, err := s.queries.ListSnapshotsByDateRange(r.Context(), holder, currencyID, start, end)
	if err != nil {
		httpx.Error(w, err)
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
	httpx.OK(w, data)
}
