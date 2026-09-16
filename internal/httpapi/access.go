package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/KleitonBarone/webhook-redrive/internal/auth"
	"github.com/KleitonBarone/webhook-redrive/internal/destination"
)

type Security struct {
	Authenticator auth.Authenticator
	Destinations  *destination.Policy
}

func (a *API) require(permission string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		values := r.Header.Values("Authorization")
		var token string
		if len(values) == 1 {
			scheme, value, ok := strings.Cut(values[0], " ")
			if ok && strings.EqualFold(scheme, "Bearer") {
				token = value
			}
		}
		hash, err := auth.Hash(token)
		if err != nil {
			unauthorized(w)
			return
		}
		if a.security.Authenticator == nil {
			writeError(w, 503, "authentication unavailable")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		principal, err := a.security.Authenticator.Authenticate(ctx, hash, a.clock.Now())
		if errors.Is(err, auth.ErrUnauthorized) {
			unauthorized(w)
			return
		}
		if err != nil {
			writeError(w, 503, "authentication unavailable")
			return
		}
		if !principal.Allows(permission) {
			writeError(w, 403, "forbidden")
			return
		}
		next(w, r.WithContext(auth.WithPrincipal(r.Context(), principal)))
	}
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="webhook-redrive"`)
	writeError(w, http.StatusUnauthorized, "unauthorized")
}
