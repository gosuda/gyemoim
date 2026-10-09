// Management authentication endpoints: login, logout, and self-service password
// change. They run under guard.Management, so Origin and CSRF checks apply to
// them like every other mutation; the login form uses the CSRF token injected
// into the served HTML. Session middleware for the remaining management surface
// lives in session.go.
package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gosuda/gyemoim/internal/config"
)

const (
	sessionLifetime   = 24 * time.Hour
	minPasswordLength = 12
)

// writeInvalidCredentials answers every failed login and current-password check
// with the same generic message so unknown usernames are indistinguishable from
// wrong passwords.
func writeInvalidCredentials(w http.ResponseWriter) {
	writeManagementError(w, http.StatusUnauthorized, "invalid credentials", "invalid_credentials")
}

// timingEqualizerHash is a fixed valid argon2id hash with the standard
// parameters. Unknown usernames verify a password against it so failed logins
// cost the same argon2id work whether or not the username exists.
const timingEqualizerHash = `$argon2id$v=19$m=65536,t=3,p=1$TfhvhFGJdOBDMh1n5gYHqw$iv6KIOfoHeP9G3vSPp2KunT4Sfvs6cN3utyxJOpDbDQ`

// login verifies credentials and issues a fresh session. Usernames are trimmed
// of surrounding whitespace and must not be empty; they are otherwise compared
// exactly as stored (normalization beyond that belongs to callers). Disabled
// accounts receive the same generic 401 as unknown users and wrong passwords.
func (api *managementAPI) login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeRequiredJSON(w, r, &input) {
		return
	}
	username := strings.TrimSpace(input.Username)
	if username == "" || input.Password == "" {
		writeInvalidCredentials(w)
		return
	}
	// Brute-force backoff (decision 8) is checked before any store lookup or
	// argon2id work, so a username inside a backoff window gets the same
	// generic 401 almost for free. Rejected attempts are not counted as
	// failures — windows decay naturally instead of being extended by spam.
	if api.backoff.blocked(username, time.Now()) {
		writeInvalidCredentials(w)
		return
	}
	user, err := api.store.GetUserByUsername(r.Context(), username)
	if err != nil {
		if errors.Is(err, config.ErrNotFound) {
			// Equalize timing with the wrong-password path before the generic 401.
			_ = config.VerifyPassword(input.Password, timingEqualizerHash)
			api.backoff.recordFailure(username, time.Now())
			writeInvalidCredentials(w)
			return
		}
		writeManagementFailure(w, err)
		return
	}
	// Verify before the disabled check so a disabled account also costs one
	// argon2id verification, keeping every 401 path equally expensive.
	validPassword := config.VerifyPassword(input.Password, user.PasswordHash)
	if user.DisabledAt != nil || !validPassword {
		api.backoff.recordFailure(username, time.Now())
		writeInvalidCredentials(w)
		return
	}
	sessionID, err := newSessionID()
	if err != nil {
		writeManagementFailure(w, err)
		return
	}
	expires := time.Now().UTC().Add(sessionLifetime)
	// A fresh random ID at every login defends against session fixation.
	if _, err := api.store.CreateSession(r.Context(), config.Session{
		IDHash:    sessionIDHash(sessionID),
		UserID:    user.ID,
		ExpiresAt: expires,
	}); err != nil {
		writeManagementFailure(w, err)
		return
	}
	// The login succeeded, so the username's failure state is cleared.
	api.backoff.recordSuccess(username)
	// Opportunistic housekeeping; a failed prune must not fail the login.
	_, _ = api.store.DeleteExpiredSessions(r.Context())
	setSessionCookie(w, sessionID, expires)
	writeJSON(w, http.StatusOK, user)
}

// logout deletes the presented session and clears the cookie. It succeeds even
// when the session was already invalid or expired, so stale browsers can clean up.
func (api *managementAPI) logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		_ = api.store.DeleteSession(r.Context(), sessionIDHash(cookie.Value))
	}
	clearSessionCookie(w)
	writeJSON(w, http.StatusOK, struct{}{})
}

// changePassword requires a valid session (resolved by ServeHTTP), verifies the
// current password, updates the hash, clears the forced-change flag, and revokes
// every other session of the user. The current session stays valid.
func (api *managementAPI) changePassword(w http.ResponseWriter, r *http.Request, session config.Session, user config.User) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	var input struct {
		CurrentPassword string `json:"currentPassword"`
		NewPassword     string `json:"newPassword"`
	}
	if !decodeRequiredJSON(w, r, &input) {
		return
	}
	if !config.VerifyPassword(input.CurrentPassword, user.PasswordHash) {
		writeInvalidCredentials(w)
		return
	}
	if utf8.RuneCountInString(input.NewPassword) < minPasswordLength {
		writeManagementError(w, http.StatusBadRequest, "new password must contain at least 12 characters", "invalid_request")
		return
	}
	hash, err := config.HashPassword(input.NewPassword)
	if err != nil {
		writeManagementFailure(w, err)
		return
	}
	if err := api.store.UpdateUserPassword(r.Context(), user.ID, hash, false); err != nil {
		writeManagementFailure(w, err)
		return
	}
	if err := api.store.DeleteOtherUserSessions(r.Context(), user.ID, session.IDHash); err != nil {
		writeManagementFailure(w, err)
		return
	}
	updated, err := api.store.GetUser(r.Context(), user.ID)
	if err != nil {
		writeManagementFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// authenticate resolves the session for every management path except the auth
// endpoints that opt out, writing the JSON error response itself on failure.
func (api *managementAPI) authenticate(w http.ResponseWriter, r *http.Request) (config.User, config.Session, bool) {
	cookie, err := r.Cookie(sessionCookieName)
	user, session, ok := resolveManagementSession(r.Context(), api.store, cookieValue(cookie, err))
	if !ok {
		writeManagementError(w, http.StatusUnauthorized, "management session is missing, invalid, or expired", "unauthenticated")
		return config.User{}, config.Session{}, false
	}
	return user, session, true
}
