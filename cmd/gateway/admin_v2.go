package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/djwkvnwincbnak78/nexus/internal/admin"
	"github.com/djwkvnwincbnak78/nexus/internal/authz"
	"github.com/djwkvnwincbnak78/nexus/internal/model"
)

// adminAuthV2 accepts either the legacy X-Admin-Key or a console JWT.
func (s *server) adminAuthV2(action authz.Action, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		if key := strings.TrimSpace(r.Header.Get("X-Admin-Key")); key != "" && s.admin != nil {
			if p, err := s.admin.ResolveAdminKey(ctx, authz.HashKey(key)); err == nil {
				if !authz.Can(p.Role, action) {
					writeError(w, http.StatusForbidden, "forbidden", "role cannot "+string(action))
					return
				}
				p.UserID = p.TenantID
				next(w, r.WithContext(authz.WithPrincipal(ctx, p)))
				return
			}
		}

		auth := strings.TrimSpace(r.Header.Get("Authorization"))
		if strings.HasPrefix(auth, "Bearer ") {
			if s.adminSvc == nil {
				writeError(w, http.StatusForbidden, "forbidden", "admin API is not configured")
				return
			}
			claims, err := admin.Parse(s.adminSvc.JWTSecret, strings.TrimSpace(strings.TrimPrefix(auth, "Bearer ")))
			if err == nil {
				p := &authz.Principal{TenantID: claims.Tenant, UserID: claims.Sub, Role: authz.Role(claims.Role)}
				if !authz.Can(p.Role, action) {
					writeError(w, http.StatusForbidden, "forbidden", "role cannot "+string(action))
					return
				}
				next(w, r.WithContext(authz.WithPrincipal(ctx, p)))
				return
			}
		}
		writeError(w, http.StatusUnauthorized, "unauthorized", "login required")
	}
}

// POST /admin/api/login
func (s *server) adminLogin(w http.ResponseWriter, r *http.Request) {
	if s.adminSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "no_database", "control plane requires PostgreSQL")
		return
	}
	var in struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	tok, claims, err := s.adminSvc.Login(r.Context(), in.Email, in.Password)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "invalid email or password")
		return
	}
	s.metrics.Inc("gateway_admin_logins_total", map[string]string{"result": "ok"})
	writeJSON(w, http.StatusOK, map[string]any{
		"token": tok, "email": claims.Email, "role": claims.Role, "tenant": claims.Tenant,
	})
}

// GET /admin/api/dashboard
func (s *server) adminDashboard(w http.ResponseWriter, r *http.Request) {
	if s.adminSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "no_database", "control plane requires PostgreSQL")
		return
	}
	d, err := s.adminSvc.Dashboard(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "dashboard_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// GET/POST /admin/api/channels
func (s *server) adminChannels(w http.ResponseWriter, r *http.Request) {
	if s.adminSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "no_database", "control plane requires PostgreSQL")
		return
	}
	switch r.Method {
	case http.MethodGet:
		list, err := s.adminSvc.ListChannels(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "list_failed", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": list})
	case http.MethodPost:
		var in admin.CreateChannelInput
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		ch, err := s.adminSvc.CreateChannel(r.Context(), in)
		if err != nil {
			writeError(w, http.StatusBadRequest, "create_failed", err.Error())
			return
		}
		s.reloadCatalog(r)
		writeJSON(w, http.StatusCreated, ch)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "GET or POST")
	}
}

// PATCH/DELETE /admin/api/channels/{id}
func (s *server) adminChannelByID(w http.ResponseWriter, r *http.Request) {
	if s.adminSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "no_database", "control plane requires PostgreSQL")
		return
	}
	id64, err := strconv.ParseUint(r.PathValue("id"), 10, 32)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "bad channel id")
		return
	}
	id := uint(id64)

	switch r.Method {
	case http.MethodPatch:
		var in struct {
			Enabled *bool `json:"enabled"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil || in.Enabled == nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "enabled is required")
			return
		}
		if err := s.adminSvc.SetChannelEnabled(r.Context(), id, *in.Enabled); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeError(w, http.StatusNotFound, "not_found", "channel not found")
				return
			}
			writeError(w, http.StatusInternalServerError, "update_failed", err.Error())
			return
		}
		s.reloadCatalog(r)
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "enabled": *in.Enabled})
	case http.MethodDelete:
		if err := s.adminSvc.DeleteChannel(r.Context(), id); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeError(w, http.StatusNotFound, "not_found", "channel not found")
				return
			}
			writeError(w, http.StatusInternalServerError, "delete_failed", err.Error())
			return
		}
		s.reloadCatalog(r)
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "deleted": true})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "PATCH or DELETE")
	}
}

// GET/POST /admin/api/tokens
func (s *server) adminTokens(w http.ResponseWriter, r *http.Request) {
	if s.adminSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "no_database", "control plane requires PostgreSQL")
		return
	}
	p, _ := authz.FromContext(r.Context())
	userID := p.UserID
	if userID == 0 {
		userID = p.TenantID
	}

	switch r.Method {
	case http.MethodGet:
		list, err := s.adminSvc.ListTokens(r.Context(), userID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "list_failed", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": list})
	case http.MethodPost:
		var in struct {
			Name  string          `json:"name"`
			Quota json.RawMessage `json:"quota"`
			Days  int             `json:"expires_days"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		quota := model.NUMERIC(0)
		if len(in.Quota) > 0 && string(in.Quota) != "null" {
			q, err := decodeAmount(in.Quota)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid_request", "quota must be a positive decimal")
				return
			}
			quota = q
		}
		tok, plain, err := s.adminSvc.CreateToken(r.Context(), userID, in.Name, quota, in.Days)
		if err != nil {
			writeError(w, http.StatusBadRequest, "create_failed", err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{
			"token": plain, "id": tok.ID, "prefix": tok.Prefix, "name": tok.Name,
			"quota": tok.Quota, "warning": "store this token now; it is never shown again",
		})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "GET or POST")
	}
}

// DELETE /admin/api/tokens/{id}
func (s *server) adminTokenByID(w http.ResponseWriter, r *http.Request) {
	if s.adminSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "no_database", "control plane requires PostgreSQL")
		return
	}
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method", "DELETE")
		return
	}
	p, _ := authz.FromContext(r.Context())
	userID := p.UserID
	if userID == 0 {
		userID = p.TenantID
	}
	id64, err := strconv.ParseUint(r.PathValue("id"), 10, 32)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "bad token id")
		return
	}
	if err := s.adminSvc.DeleteToken(r.Context(), userID, uint(id64)); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "not_found", "token not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "revoke_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id64, "status": "disabled"})
}
