package authz

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"github.com/djwkvnwincbnak78/nexus/internal/model"
)

// Role is a tenant-scoped role.
type Role string

// Roles, ordered from most to least privileged.
const (
	RoleOwner  Role = "owner"
	RoleAdmin  Role = "admin"
	RoleMember Role = "member"
	RoleViewer Role = "viewer"
)

// Action is a permission checked by the admin middleware.
type Action string

// Actions exposed by the admin API.
const (
	ActionOrdersRead   Action = "orders:read"
	ActionOrdersWrite  Action = "orders:write"
	ActionRedeemCreate Action = "redeem:create"
	ActionRedeemClaim  Action = "redeem:claim"
)

var permissions = map[Role]map[Action]bool{
	RoleOwner: {
		ActionOrdersRead: true, ActionOrdersWrite: true,
		ActionRedeemCreate: true, ActionRedeemClaim: true,
	},
	RoleAdmin: {
		ActionOrdersRead: true, ActionOrdersWrite: true,
		ActionRedeemCreate: true, ActionRedeemClaim: true,
	},
	RoleMember: {
		ActionOrdersRead: true, ActionRedeemClaim: true,
	},
	RoleViewer: {
		ActionOrdersRead: true,
	},
}

// Can reports whether role may perform action. Unknown roles deny everything.
func Can(role Role, action Action) bool {
	allowed, ok := permissions[role]
	if !ok {
		return false
	}
	return allowed[action]
}

// Principal is the authenticated admin caller.
type Principal struct {
	TenantID uint
	UserID   uint
	Role     Role
	KeyID    string
}

type principalKey struct{}

// WithPrincipal stores the principal in the request context.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// FromContext returns the principal.
func FromContext(ctx context.Context) (*Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(*Principal)
	return p, ok && p != nil
}

// HashKey returns the SHA-256 hex digest used as the storage key. Plaintext
// admin keys are never persisted.
func HashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// Errors returned by stores.
var (
	ErrNotFound  = errors.New("authz: not found")
	ErrConflict  = errors.New("authz: conflict")
	ErrForbidden = errors.New("authz: forbidden")
)

// Order is a top-up order.
type Order struct {
	ID        string        `json:"id"`
	TenantID  uint          `json:"tenant_id"`
	Amount    model.NUMERIC `json:"amount"`
	Currency  string        `json:"currency"`
	Status    string        `json:"status"`
	CreatedAt time.Time     `json:"created_at"`
	PaidAt    *time.Time    `json:"paid_at,omitempty"`
}

// RedeemCode is a single-use credit code.
type RedeemCode struct {
	Code      string        `json:"code"`
	TenantID  uint          `json:"tenant_id"`
	Amount    model.NUMERIC `json:"amount"`
	Status    string        `json:"status"`
	CreatedAt time.Time     `json:"created_at"`
	ClaimedAt *time.Time    `json:"claimed_at,omitempty"`
	ClaimedBy uint          `json:"claimed_by,omitempty"`
}

// Store is the persistence contract for RBAC and commerce.
type Store interface {
	ResolveAdminKey(ctx context.Context, keyHash string) (*Principal, error)
	EnsureAdminKey(ctx context.Context, keyHash string, tenantID uint, role Role) error

	CreateOrder(ctx context.Context, tenantID uint, amount model.NUMERIC, currency string) (*Order, error)
	GetOrder(ctx context.Context, tenantID uint, id string) (*Order, error)
	MarkOrderPaid(ctx context.Context, tenantID uint, id string) (*Order, error)

	CreateRedeemCodes(ctx context.Context, tenantID uint, amount model.NUMERIC, count int) ([]RedeemCode, error)
	ClaimRedeem(ctx context.Context, tenantID, userID uint, code string) (*RedeemCode, error)
}

// ---------------------------------------------------------------------------
// MemoryStore
// ---------------------------------------------------------------------------

// MemoryStore is a process-local Store (tests, DB-less deployments).
type MemoryStore struct {
	mu     sync.Mutex
	keys   map[string]*Principal
	orders map[string]*Order
	codes  map[string]*RedeemCode
	now    func() time.Time
}

// NewMemoryStore builds an empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		keys:   make(map[string]*Principal),
		orders: make(map[string]*Order),
		codes:  make(map[string]*RedeemCode),
		now:    time.Now,
	}
}

// ResolveAdminKey implements Store.
func (m *MemoryStore) ResolveAdminKey(_ context.Context, keyHash string) (*Principal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.keys[keyHash]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *p
	return &cp, nil
}

// EnsureAdminKey implements Store.
func (m *MemoryStore) EnsureAdminKey(_ context.Context, keyHash string, tenantID uint, role Role) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := keyHash
	if len(id) > 8 {
		id = id[:8]
	}
	m.keys[keyHash] = &Principal{TenantID: tenantID, Role: role, KeyID: id}
	return nil
}

// CreateOrder implements Store.
func (m *MemoryStore) CreateOrder(_ context.Context, tenantID uint, amount model.NUMERIC, currency string) (*Order, error) {
	if amount <= 0 {
		return nil, ErrConflict
	}
	if currency == "" {
		currency = "USD"
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	o := &Order{
		ID: newID("ord_"), TenantID: tenantID, Amount: amount,
		Currency: currency, Status: "pending", CreatedAt: m.now(),
	}
	m.orders[o.ID] = o
	cp := *o
	return &cp, nil
}

// GetOrder implements Store.
func (m *MemoryStore) GetOrder(_ context.Context, tenantID uint, id string) (*Order, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.orders[id]
	if !ok || o.TenantID != tenantID {
		return nil, ErrNotFound
	}
	cp := *o
	return &cp, nil
}

// MarkOrderPaid implements Store.
func (m *MemoryStore) MarkOrderPaid(_ context.Context, tenantID uint, id string) (*Order, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.orders[id]
	if !ok || o.TenantID != tenantID {
		return nil, ErrNotFound
	}
	if o.Status == "paid" {
		return nil, ErrConflict
	}
	now := m.now()
	o.Status = "paid"
	o.PaidAt = &now
	cp := *o
	return &cp, nil
}

// CreateRedeemCodes implements Store.
func (m *MemoryStore) CreateRedeemCodes(_ context.Context, tenantID uint, amount model.NUMERIC, count int) ([]RedeemCode, error) {
	if amount <= 0 || count <= 0 || count > 1000 {
		return nil, ErrConflict
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]RedeemCode, 0, count)
	for i := 0; i < count; i++ {
		c := &RedeemCode{
			Code: newID("rc_"), TenantID: tenantID, Amount: amount,
			Status: "active", CreatedAt: m.now(),
		}
		m.codes[c.Code] = c
		out = append(out, *c)
	}
	return out, nil
}

// ClaimRedeem implements Store.
func (m *MemoryStore) ClaimRedeem(_ context.Context, tenantID, userID uint, code string) (*RedeemCode, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.codes[code]
	if !ok || c.TenantID != tenantID {
		return nil, ErrNotFound
	}
	if c.Status != "active" {
		return nil, ErrConflict
	}
	now := m.now()
	c.Status = "claimed"
	c.ClaimedAt = &now
	c.ClaimedBy = userID
	cp := *c
	return &cp, nil
}

func newID(prefix string) string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return prefix + hex.EncodeToString([]byte(time.Now().Format(time.RFC3339Nano)))
	}
	return prefix + hex.EncodeToString(b[:])
}
