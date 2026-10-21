package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/djwkvnwincbnak78/nexus/internal/authz"
	"github.com/djwkvnwincbnak78/nexus/internal/settings"
)

func TestAdminSettingsRejectsNonAdminBearer(t *testing.T) {
	srv := &server{settings: settings.New(nil), adminSvc: nil}
	h := srv.adminAuthV2(authz.ActionOrdersRead, srv.adminSettingsGet)

	req := httptest.NewRequest(http.MethodGet, "/admin/api/settings", nil)
	req.Header.Set("Authorization", "Bearer sk-nexus-usertoken-not-an-admin-jwt")
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestAdminSettingsRejectsAnonymous(t *testing.T) {
	srv := &server{settings: settings.New(nil)}
	h := srv.adminAuthV2(authz.ActionOrdersRead, srv.adminSettingsGet)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/admin/api/settings", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}
