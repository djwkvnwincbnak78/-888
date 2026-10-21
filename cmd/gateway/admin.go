package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/djwkvnwincbnak78/nexus/internal/authz"
	"github.com/djwkvnwincbnak78/nexus/internal/model"
)

// adminAuth authenticates X-Admin-Key and enforces a role permission.
// Admin traffic does not use the end-user limiter (different credential), but
// it is still wrapped by the metrics middleware.
func adminAuth(store authz.Store, action authz.Action, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimSpace(r.Header.Get("X-Admin-Key"))
		if key == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized", "X-Admin-Key required")
			return
		}
		principal, err := store.ResolveAdminKey(r.Context(), authz.HashKey(key))
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid admin key")
			return
		}
		if !authz.Can(principal.Role, action) {
			writeError(w, http.StatusForbidden, "forbidden",
				fmt.Sprintf("role %s cannot %s", principal.Role, action))
			return
		}
		next(w, r.WithContext(authz.WithPrincipal(r.Context(), principal)))
	}
}

func mapAuthzError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, authz.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "resource not found")
	case errors.Is(err, authz.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", "resource is not in a valid state for this operation")
	case errors.Is(err, authz.ErrForbidden):
		writeError(w, http.StatusForbidden, "forbidden", "not allowed")
	default:
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
	}
}

// decodeAmount accepts both "12.5" and 12.5 and keeps 10-digit precision.
func decodeAmount(raw json.RawMessage) (model.NUMERIC, error) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return 0, fmt.Errorf("amount is required")
	}
	s = strings.Trim(s, `"`)
	return model.ParseNUMERIC(s)
}

// POST /admin/api/orders  {amount, currency}
func (s *server) adminCreateOrder(w http.ResponseWriter, r *http.Request) {
	p, ok := authz.FromContext(r.Context())
	if !ok {
		writeError(w, http.StatusInternalServerError, "no_principal", "missing principal")
		return
	}
	var body struct {
		Amount   json.RawMessage `json:"amount"`
		Currency string          `json:"currency"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	amount, err := decodeAmount(body.Amount)
	if err != nil || amount <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "amount must be a positive decimal")
		return
	}
	order, err := s.admin.CreateOrder(r.Context(), p.TenantID, amount, strings.ToUpper(body.Currency))
	if err != nil {
		mapAuthzError(w, err)
		return
	}
	s.metrics.Inc("gateway_admin_orders_total", map[string]string{"status": "created"})
	writeJSON(w, http.StatusCreated, order)
}

// GET /admin/api/orders/{id}
func (s *server) adminGetOrder(w http.ResponseWriter, r *http.Request) {
	p, _ := authz.FromContext(r.Context())
	order, err := s.admin.GetOrder(r.Context(), p.TenantID, r.PathValue("id"))
	if err != nil {
		mapAuthzError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, order)
}

// POST /admin/api/orders/{id}/pay — marks paid and credits the tenant account.
func (s *server) adminPayOrder(w http.ResponseWriter, r *http.Request) {
	p, _ := authz.FromContext(r.Context())
	order, err := s.admin.MarkOrderPaid(r.Context(), p.TenantID, r.PathValue("id"))
	if err != nil {
		mapAuthzError(w, err)
		return
	}
	if err := s.ledger.Credit(r.Context(), p.TenantID, order.Amount, "order "+order.ID); err != nil {
		// The order is already paid; surface the credit failure loudly rather
		// than pretending the money landed.
		slog.Error("order_credit_failed", "order", order.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "credit_failed", err.Error())
		return
	}
	s.metrics.Inc("gateway_admin_orders_total", map[string]string{"status": "paid"})
	writeJSON(w, http.StatusOK, order)
}

// POST /admin/api/redeem {amount, count}
func (s *server) adminCreateRedeem(w http.ResponseWriter, r *http.Request) {
	p, _ := authz.FromContext(r.Context())
	var body struct {
		Amount json.RawMessage `json:"amount"`
		Count  int             `json:"count"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	amount, err := decodeAmount(body.Amount)
	if err != nil || amount <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "amount must be a positive decimal")
		return
	}
	if body.Count <= 0 || body.Count > 1000 {
		writeError(w, http.StatusBadRequest, "invalid_request", "count must be 1..1000")
		return
	}
	codes, err := s.admin.CreateRedeemCodes(r.Context(), p.TenantID, amount, body.Count)
	if err != nil {
		mapAuthzError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"object": "list", "data": codes})
}

// POST /admin/api/redeem/claim {code}
func (s *server) adminClaimRedeem(w http.ResponseWriter, r *http.Request) {
	p, _ := authz.FromContext(r.Context())
	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	code := strings.TrimSpace(body.Code)
	if code == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "code is required")
		return
	}
	claimed, err := s.admin.ClaimRedeem(r.Context(), p.TenantID, p.TenantID, code)
	if err != nil {
		mapAuthzError(w, err)
		return
	}
	if err := s.ledger.Credit(r.Context(), p.TenantID, claimed.Amount, "redeem "+code); err != nil {
		slog.Error("redeem_credit_failed", "code", code, "err", err)
		writeError(w, http.StatusInternalServerError, "credit_failed", err.Error())
		return
	}
	s.metrics.Inc("gateway_admin_redeem_total", map[string]string{"status": "claimed"})
	writeJSON(w, http.StatusOK, map[string]any{
		"code": claimed.Code, "amount": claimed.Amount, "status": claimed.Status,
	})
}
