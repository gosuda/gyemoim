// First-start bootstrap. When the users table holds no management user, one
// "admin" user is created with a generated random password that must be changed
// at first login. The generated password is returned once to the caller (main
// prints it to stderr) and is never persisted, logged, or stored in any form
// other than its argon2id hash.
package config

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

// BootstrapUsername is the fixed name of the auto-created first management user.
const BootstrapUsername = "admin"

// bootstrapPasswordEntropyBytes yields 128 bits of entropy, which base64url
// encodes to 22 unambiguous characters.
const bootstrapPasswordEntropyBytes = 16

// EnsureBootstrapAdmin creates the initial "admin" user when no management user
// exists and returns the generated plaintext password alongside whether the
// user was created. The caller must print the password to stderr exactly once;
// this function neither logs nor stores it.
//
// The check and the insert run in one transaction, so a database can never end
// up with two bootstrap users even if this were ever called outside the
// single-process lock that the process lock file guarantees per data directory.
//
// Bootstrap deliberately repeats: an empty users table always means "unmanaged
// install", so if every user is deleted later, the next start creates a fresh
// admin and prints a new password again.
func (s *Store) EnsureBootstrapAdmin(ctx context.Context) (password string, created bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", false, fmt.Errorf("begin bootstrap user transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&count); err != nil {
		return "", false, fmt.Errorf("count users for bootstrap: %w", err)
	}
	if count > 0 {
		return "", false, nil
	}
	password, err = generateBootstrapPassword()
	if err != nil {
		return "", false, err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return "", false, err
	}
	id, err := newUUID()
	if err != nil {
		return "", false, err
	}
	_, timestamp := nowText()
	if _, err := tx.ExecContext(ctx, `INSERT INTO users(id, username, password_hash, must_change_password, disabled_at, created_at, updated_at) VALUES (?, ?, ?, 1, NULL, ?, ?)`,
		id, BootstrapUsername, hash, timestamp, timestamp); err != nil {
		if isUniqueConstraint(err) {
			// Another writer inserted the name first; keep their user and print nothing.
			return "", false, nil
		}
		return "", false, fmt.Errorf("create bootstrap user: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", false, fmt.Errorf("commit bootstrap user: %w", err)
	}
	return password, true, nil
}

func generateBootstrapPassword() (string, error) {
	var entropy [bootstrapPasswordEntropyBytes]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", fmt.Errorf("generate bootstrap password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(entropy[:]), nil
}
