package authz

import (
	"context"
	"errors"
	"testing"

	"github.com/djwkvnwincbnak78/nexus/internal/model"
)

func TestPermissionMatrix(t *testing.T) {
	cases := []struct {
		role   Role
		action Action
		want   bool
	}{
		{RoleOwner, ActionOrdersWrite, true},
		{RoleAdmin, ActionOrdersWrite, true},
		{RoleMember, ActionOrdersWrite, false},
		{RoleMember, ActionRedeemClaim, true},
		{RoleMember, ActionOrdersRead, true},
		{RoleViewer, ActionOrdersRead, true},
		{RoleViewer, ActionRedeemClaim, false},
		{Role("god"), ActionOrdersRead, false},
	}
	for _, tc := range cases {
		if got := Can(tc.role, tc.action); got != tc.want {
			t.Fatalf("Can(%s,%s)=%v want %v", tc.role, tc.action, got, tc.want)
		}
	}
}

func TestHashKeyIsStableSHA256(t *testing.T) {
	a := HashKey("secret")
	if len(a) != 64 {
		t.Fatalf("hash length=%d", len(a))
	}
	if a != HashKey("secret") || a == HashKey("secret2") {
		t.Fatal("hash is not deterministic/unique")
	}
}

func TestMemoryStoreAdminKeyAndTenantIsolation(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	if err := s.EnsureAdminKey(ctx, HashKey("k"), 1, RoleAdmin); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	p, err := s.ResolveAdminKey(ctx, HashKey("k"))
	if err != nil || p.TenantID != 1 || p.Role != RoleAdmin {
		t.Fatalf("principal=%+v err=%v", p, err)
	}
	if _, err := s.ResolveAdminKey(ctx, HashKey("nope")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown key err=%v", err)
	}

	order, err := s.CreateOrder(ctx, 1, model.FromFloat(5), "USD")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	if _, err := s.GetOrder(ctx, 2, order.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant read err=%v", err)
	}
	if _, err := s.MarkOrderPaid(ctx, 2, order.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant pay err=%v", err)
	}
	paid, err := s.MarkOrderPaid(ctx, 1, order.ID)
	if err != nil || paid.Status != "paid" || paid.PaidAt == nil {
		t.Fatalf("paid=%+v err=%v", paid, err)
	}
	if _, err := s.MarkOrderPaid(ctx, 1, order.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("double pay err=%v", err)
	}
}

func TestMemoryStoreRedeemLifecycle(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	codes, err := s.CreateRedeemCodes(ctx, 1, model.FromFloat(2.5), 3)
	if err != nil || len(codes) != 3 {
		t.Fatalf("codes=%+v err=%v", codes, err)
	}
	claimed, err := s.ClaimRedeem(ctx, 1, 7, codes[0].Code)
	if err != nil || claimed.Status != "claimed" || claimed.ClaimedBy != 7 {
		t.Fatalf("claimed=%+v err=%v", claimed, err)
	}
	if _, err := s.ClaimRedeem(ctx, 1, 7, codes[0].Code); !errors.Is(err, ErrConflict) {
		t.Fatalf("double claim err=%v", err)
	}
	if _, err := s.ClaimRedeem(ctx, 2, 7, codes[1].Code); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant claim err=%v", err)
	}
	if _, err := s.CreateRedeemCodes(ctx, 1, model.FromFloat(0), 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("zero amount err=%v", err)
	}
	if _, err := s.CreateRedeemCodes(ctx, 1, model.FromFloat(1), 0); !errors.Is(err, ErrConflict) {
		t.Fatalf("zero count err=%v", err)
	}
}
