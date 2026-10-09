// Management session enforcement: the embedded UI pages and the JSON management
// API both require a valid login session. /auth/callback and /v1/ sit outside
// this middleware by design.
package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gosuda/gyemoim/internal/config"
)

// sessionCookieName is a plain name (no __Host- prefix, no Secure flag):
// plain-HTTP LAN access is the primary usage mode by explicit owner decision
// (2026-10-09), and __Host-/Secure cookies are dropped by browsers on plain
// HTTP. SameSite=Lax is kept as the baseline cross-site protection.
const sessionCookieName = "gym_session"

// newSessionID returns a fresh 256-bit session ID as base64url (43 characters).
// Only its SHA-256 hash is ever stored; the plaintext ID lives in the cookie.
func newSessionID() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate session ID: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func sessionIDHash(id string) []byte {
	sum := sha256.Sum256([]byte(id))
	return sum[:]
}

func setSessionCookie(w http.ResponseWriter, id string, expires time.Time) {
	// Secure is deliberately not set: the UI is reached over plain HTTP on the
	// LAN (explicit owner decision), and a Secure cookie would never be stored
	// or sent there.
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    id,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// resolveManagementSession validates a presented session ID: it must map to an
// unexpired session whose user still exists and is not disabled. Expiry is
// absolute; sessions are never extended. last_seen_at is left untouched —
// without sliding renewal it carries no security meaning.
func resolveManagementSession(ctx context.Context, store *config.Store, id string) (config.User, config.Session, bool) {
	if id == "" {
		return config.User{}, config.Session{}, false
	}
	session, err := store.GetSession(ctx, sessionIDHash(id))
	if err != nil || !session.ExpiresAt.After(time.Now()) {
		return config.User{}, config.Session{}, false
	}
	user, err := store.GetUser(ctx, session.UserID)
	if err != nil || user.DisabledAt != nil {
		return config.User{}, config.Session{}, false
	}
	return user, session, true
}

// RequireManagementSession wraps the embedded UI handler so page requests need a
// valid management session. Unauthenticated requests redirect to /login; users
// with a pending forced password change are redirected to /change-password. The
// login page and static assets stay reachable without a session.
func RequireManagementSession(store *config.Store, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" || strings.HasPrefix(r.URL.Path, "/assets/") {
			next.ServeHTTP(w, r)
			return
		}
		cookie, err := r.Cookie(sessionCookieName)
		user, _, ok := resolveManagementSession(r.Context(), store, cookieValue(cookie, err))
		if !ok {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if user.MustChangePassword && r.URL.Path != "/change-password" {
			http.Redirect(w, r, "/change-password", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func cookieValue(cookie *http.Cookie, err error) string {
	if err != nil || cookie == nil {
		return ""
	}
	return cookie.Value
}
