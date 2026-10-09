// Management user administration endpoints. Every authenticated management user
// has full rights over other users (decision 1 — no roles); the only limits are
// that a user cannot disable or delete themselves and cannot delete the last
// remaining user, because either would make the interface unreachable.
package httpapi

import (
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/gosuda/gyemoim/internal/config"
)

// maxUsernameLength caps new usernames. Usernames are trimmed of surrounding
// whitespace, must not contain whitespace inside, and may hold at most this many
// Unicode characters. The trimmed value is stored and login compares against it.
const maxUsernameLength = 64

// users lists existing management users (hash-free) and creates new ones. New
// users always start with must_change_password set: the creator chose their
// password, so hygiene forces a change at first sign-in.
func (api *managementAPI) users(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		users, err := api.store.ListUsers(r.Context())
		if err != nil {
			writeManagementFailure(w, err)
			return
		}
		writeJSON(w, http.StatusOK, users)
	case http.MethodPost:
		var input struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if !decodeRequiredJSON(w, r, &input) {
			return
		}
		username, ok := validateUsername(w, input.Username)
		if !ok {
			return
		}
		if utf8.RuneCountInString(input.Password) < minPasswordLength {
			writeManagementError(w, http.StatusBadRequest, "password must contain at least 12 characters", "invalid_request")
			return
		}
		hash, err := config.HashPassword(input.Password)
		if err != nil {
			writeManagementFailure(w, err)
			return
		}
		user, err := api.store.CreateUser(r.Context(), config.User{
			Username: username, PasswordHash: hash, MustChangePassword: true,
		})
		if err != nil {
			writeManagementFailure(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, user)
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPost)
	}
}

// user deletes a management user; their sessions cascade away with them.
func (api *managementAPI) user(w http.ResponseWriter, r *http.Request, id string, actor config.User) {
	if r.Method != http.MethodDelete {
		methodNotAllowed(w, http.MethodDelete)
		return
	}
	if id == actor.ID {
		writeManagementError(w, http.StatusBadRequest, "you cannot delete the account you are signed in with", "invalid_request")
		return
	}
	users, err := api.store.ListUsers(r.Context())
	if err != nil {
		writeManagementFailure(w, err)
		return
	}
	if len(users) <= 1 {
		writeManagementError(w, http.StatusBadRequest, "the last remaining user cannot be deleted", "invalid_request")
		return
	}
	if err := api.store.DeleteUser(r.Context(), id); err != nil {
		writeManagementFailure(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// setUserEnabled disables or re-enables a management user. Disabling takes
// effect immediately: session resolution rejects any session of a disabled user.
func (api *managementAPI) setUserEnabled(w http.ResponseWriter, r *http.Request, id string, actor config.User, disable bool) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if !decodeOptionalEmptyJSON(w, r) {
		return
	}
	if disable && id == actor.ID {
		writeManagementError(w, http.StatusBadRequest, "you cannot disable the account you are signed in with", "invalid_request")
		return
	}
	if err := api.store.UpdateUserDisabled(r.Context(), id, disable); err != nil {
		writeManagementFailure(w, err)
		return
	}
	user, err := api.store.GetUser(r.Context(), id)
	if err != nil {
		writeManagementFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, user)
}

// resetUserPassword sets another user's password, forces a change at their next
// sign-in, and revokes all of their sessions so every browser must sign in
// again with the reset password.
func (api *managementAPI) resetUserPassword(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	var input struct {
		NewPassword string `json:"newPassword"`
	}
	if !decodeRequiredJSON(w, r, &input) {
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
	if err := api.store.UpdateUserPassword(r.Context(), id, hash, true); err != nil {
		writeManagementFailure(w, err)
		return
	}
	if err := api.store.DeleteAllUserSessions(r.Context(), id); err != nil {
		writeManagementFailure(w, err)
		return
	}
	user, err := api.store.GetUser(r.Context(), id)
	if err != nil {
		writeManagementFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, user)
}

func validateUsername(w http.ResponseWriter, raw string) (string, bool) {
	username := strings.TrimSpace(raw)
	if username == "" {
		writeManagementError(w, http.StatusBadRequest, "username is required", "invalid_request")
		return "", false
	}
	if utf8.RuneCountInString(username) > maxUsernameLength {
		writeManagementError(w, http.StatusBadRequest, "username must contain at most 64 Unicode characters", "invalid_request")
		return "", false
	}
	if strings.IndexFunc(username, unicode.IsSpace) >= 0 {
		writeManagementError(w, http.StatusBadRequest, "username must not contain whitespace", "invalid_request")
		return "", false
	}
	return username, true
}
