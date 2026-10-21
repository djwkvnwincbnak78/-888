// Package admin implements the commercial control plane: JWT auth, channels,
// tokens and dashboard aggregates backed by PostgreSQL.
package admin

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Claims is the JWT payload for the admin console.
type Claims struct {
	Sub    uint   `json:"sub"`
	Email  string `json:"email"`
	Role   string `json:"role"`
	Tenant uint   `json:"tenant"`
	Iat    int64  `json:"iat"`
	Exp    int64  `json:"exp"`
}

var jwtHeader = base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))

// Sign issues an HS256 JWT valid for ttl.
func Sign(secret []byte, c Claims) (string, error) {
	if len(secret) < 16 {
		return "", errors.New("admin: JWT secret too short")
	}
	now := time.Now()
	c.Iat, c.Exp = now.Unix(), now.Add(12*time.Hour).Unix()
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	body := jwtHeader + "." + base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// Parse verifies signature and expiry.
func Parse(secret []byte, token string) (*Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("admin: malformed token")
	}
	body := parts[0] + "." + parts[1]
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(body))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(parts[2])) {
		return nil, errors.New("admin: bad signature")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var c Claims
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, err
	}
	if time.Now().Unix() > c.Exp {
		return nil, fmt.Errorf("admin: token expired")
	}
	return &c, nil
}
