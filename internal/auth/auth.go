// Package auth defines instance-wide permissions and opaque bearer credentials.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"slices"
	"time"
)

var ErrUnauthorized = errors.New("unauthorized")

const (
	Ingest    = "ingest"
	Inspect   = "inspect"
	Endpoints = "endpoints"
	Replay    = "replay"
	Metrics   = "metrics"
)

type Principal struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Kind        string   `json:"kind"`
	Permissions []string `json:"permissions"`
}

func (p Principal) Allows(permission string) bool { return slices.Contains(p.Permissions, permission) }

func ValidPermissions(permissions []string) bool {
	if len(permissions) == 0 {
		return false
	}
	for _, p := range permissions {
		switch p {
		case Ingest, Inspect, Endpoints, Replay, Metrics:
		default:
			return false
		}
	}
	return true
}

// Generate returns a 256-bit random credential. Only its hash belongs in storage.
func Generate() (string, error) {
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return "", err
	}
	return "wr_" + base64.RawURLEncoding.EncodeToString(secret[:]), nil
}

func Hash(token string) ([]byte, error) {
	if len(token) != 46 || token[:3] != "wr_" {
		return nil, ErrUnauthorized
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(token[3:])
	if err != nil || len(decoded) != 32 {
		return nil, ErrUnauthorized
	}
	sum := sha256.Sum256([]byte(token))
	return sum[:], nil
}

type Authenticator interface {
	Authenticate(context.Context, []byte, time.Time) (Principal, error)
}

type principalKey struct{}

func WithPrincipal(ctx context.Context, principal Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, principal)
}

func FromContext(ctx context.Context) Principal {
	principal, _ := ctx.Value(principalKey{}).(Principal)
	return principal
}
