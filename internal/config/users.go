package config

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// CreateUser inserts a User with an already-hashed password and fills its stable
// ID and timestamps. Duplicate usernames report ErrConflict.
func (s *Store) CreateUser(ctx context.Context, user User) (User, error) {
	if strings.TrimSpace(user.Username) == "" {
		return User{}, errors.New("username is required")
	}
	if user.PasswordHash == "" {
		return User{}, errors.New("user password hash is required")
	}
	if user.ID == "" {
		id, err := newUUID()
		if err != nil {
			return User{}, err
		}
		user.ID = id
	}
	now, timestamp := nowText()
	user.CreatedAt, user.UpdatedAt = now, now
	_, err := s.db.ExecContext(ctx, `INSERT INTO users(id, username, password_hash, must_change_password, disabled_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		user.ID, user.Username, user.PasswordHash, boolInt(user.MustChangePassword), nullableTimePtr(user.DisabledAt), timestamp, timestamp)
	if err != nil {
		if isUniqueConstraint(err) {
			return User{}, ErrConflict
		}
		return User{}, fmt.Errorf("create user: %w", err)
	}
	return user, nil
}

// GetUser returns one User including its password hash for login verification.
func (s *Store) GetUser(ctx context.Context, id string) (User, error) {
	user, err := scanUser(s.db.QueryRowContext(ctx, `SELECT id, username, password_hash, must_change_password, disabled_at, created_at, updated_at FROM users WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, notFound("user", id)
	}
	if err != nil {
		return User{}, fmt.Errorf("get user: %w", err)
	}
	return user, nil
}

// GetUserByUsername returns one User including its password hash for login
// verification. Usernames are compared as stored; normalization belongs to callers.
func (s *Store) GetUserByUsername(ctx context.Context, username string) (User, error) {
	user, err := scanUser(s.db.QueryRowContext(ctx, `SELECT id, username, password_hash, must_change_password, disabled_at, created_at, updated_at FROM users WHERE username = ?`, username))
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, notFound("user", username)
	}
	if err != nil {
		return User{}, fmt.Errorf("get user by username: %w", err)
	}
	return user, nil
}

// ListUsers returns all Users in stable creation order. It never returns password
// hashes; login verification reads them through the individual getters.
func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, username, must_change_password, disabled_at, created_at, updated_at FROM users ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()
	users := make([]User, 0)
	for rows.Next() {
		user, err := scanUserWithoutHash(rows)
		if err != nil {
			return nil, fmt.Errorf("read user: %w", err)
		}
		users = append(users, user)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	return users, nil
}

// UpdateUserPassword replaces the stored password hash. The forced-change flag is
// set explicitly so a password change can clear it while bootstrap can leave it set.
// It changes nothing else; callers that must also revoke sessions atomically use
// ChangeUserPassword instead.
func (s *Store) UpdateUserPassword(ctx context.Context, id, passwordHash string, mustChange bool) error {
	if passwordHash == "" {
		return errors.New("user password hash is required")
	}
	_, timestamp := nowText()
	result, err := s.db.ExecContext(ctx, `UPDATE users SET password_hash = ?, must_change_password = ?, updated_at = ? WHERE id = ?`,
		passwordHash, boolInt(mustChange), timestamp, id)
	if err != nil {
		return fmt.Errorf("update user password: %w", err)
	}
	return requireAffected(result, "user", id)
}

// UpdateUserDisabled marks a User disabled (no future logins) or re-enables them.
// Existing sessions are left alone; enforcing the state belongs to callers.
func (s *Store) UpdateUserDisabled(ctx context.Context, id string, disabled bool) error {
	_, timestamp := nowText()
	var result sql.Result
	var err error
	if disabled {
		result, err = s.db.ExecContext(ctx, `UPDATE users SET disabled_at = ?, updated_at = ? WHERE id = ?`, timestamp, timestamp, id)
	} else {
		result, err = s.db.ExecContext(ctx, `UPDATE users SET disabled_at = NULL, updated_at = ? WHERE id = ?`, timestamp, id)
	}
	if err != nil {
		return fmt.Errorf("update user disabled state: %w", err)
	}
	return requireAffected(result, "user", id)
}

// ChangeUserPassword replaces a user's password hash and revokes every other
// session of that user in ONE transaction, so a failure cannot leave the password
// changed while the user's other sessions survive. keepIDHash identifies the one
// session that stays valid (the browser performing the change).
func (s *Store) ChangeUserPassword(ctx context.Context, id, passwordHash string, mustChange bool, keepIDHash []byte) error {
	if passwordHash == "" {
		return errors.New("user password hash is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin user password change: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	_, timestamp := nowText()
	result, err := tx.ExecContext(ctx, `UPDATE users SET password_hash = ?, must_change_password = ?, updated_at = ? WHERE id = ?`,
		passwordHash, boolInt(mustChange), timestamp, id)
	if err != nil {
		return fmt.Errorf("update user password: %w", err)
	}
	if err := requireAffected(result, "user", id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ? AND id_hash <> ?`, id, keepIDHash); err != nil {
		return fmt.Errorf("delete other sessions for user: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit user password change: %w", err)
	}
	return nil
}

// ErrLastUser reports a refused deletion of the last remaining management user.
var ErrLastUser = errors.New("the last remaining management user cannot be deleted")

// DeleteUserGuarded removes a User unless they are the last remaining one. The
// delete and the count run in one transaction, so two concurrent deletes cannot
// race between the check and the act and empty the users table. Sessions cascade
// away with the user via the sessions foreign key. ErrLastUser reports the
// refused last-user delete; ErrNotFound an unknown id.
func (s *Store) DeleteUserGuarded(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin guarded user delete: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `DELETE FROM users WHERE id = ? AND (SELECT COUNT(*) FROM users) > 1`, id)
	if err != nil {
		return fmt.Errorf("delete user: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check user delete: %w", err)
	}
	if affected == 0 {
		// Distinguish an unknown id from the refused last-user delete.
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE id = ?`, id).Scan(&count); err != nil {
			return fmt.Errorf("count deleted user: %w", err)
		}
		if count == 0 {
			return notFound("user", id)
		}
		return ErrLastUser
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit guarded user delete: %w", err)
	}
	return nil
}

// CreateSession stores a new login session keyed by its ID hash. The caller
// generates the random session ID and its hash; the store never sees the ID.
func (s *Store) CreateSession(ctx context.Context, session Session) (Session, error) {
	if len(session.IDHash) == 0 {
		return Session{}, errors.New("session ID hash is required")
	}
	if strings.TrimSpace(session.UserID) == "" {
		return Session{}, errors.New("session user ID is required")
	}
	if session.ExpiresAt.IsZero() {
		return Session{}, errors.New("session expiry is required")
	}
	now, timestamp := nowText()
	// last_seen_at is informational only: it is stamped once at creation and
	// never advanced. Sessions are never extended — expiry is absolute.
	session.CreatedAt, session.LastSeenAt = now, now
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions(id_hash, user_id, created_at, expires_at, last_seen_at) VALUES (?, ?, ?, ?, ?)`,
		append([]byte(nil), session.IDHash...), session.UserID, timestamp, nullableTime(session.ExpiresAt), timestamp)
	if err != nil {
		if isForeignKeyConstraint(err) {
			return Session{}, notFound("user", session.UserID)
		}
		return Session{}, fmt.Errorf("create session: %w", err)
	}
	return session, nil
}

// GetSession returns one Session by its ID hash. Expiry and disabled-user checks
// belong to callers.
func (s *Store) GetSession(ctx context.Context, idHash []byte) (Session, error) {
	session, err := scanSession(s.db.QueryRowContext(ctx, `SELECT id_hash, user_id, created_at, expires_at, last_seen_at FROM sessions WHERE id_hash = ?`, idHash))
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, fmt.Errorf("get session: %w", err)
	}
	return session, nil
}

// DeleteSession removes one Session by its ID hash, for logout.
func (s *Store) DeleteSession(ctx context.Context, idHash []byte) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id_hash = ?`, idHash)
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check session delete: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteAllUserSessions revokes every Session of a User with no exception, as
// required when an administrator resets a password: the affected user must sign
// in again everywhere with the reset password. (The self-service password
// change uses ChangeUserPassword instead, which keeps the acting session; a
// plain "delete others" variant would need a non-NULL keep-hash sentinel and
// was removed as dead code.)
func (s *Store) DeleteAllUserSessions(ctx context.Context, userID string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("delete all sessions for user: %w", err)
	}
	return nil
}

// DeleteExpiredSessions prunes Sessions past their expiry and returns how many
// rows were removed; callers run it opportunistically. The RFC3339Nano TEXT
// timestamps are fixed-width, so lexicographic comparison matches time order.
func (s *Store) DeleteExpiredSessions(ctx context.Context) (int64, error) {
	_, timestamp := nowText()
	result, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, timestamp)
	if err != nil {
		return 0, fmt.Errorf("delete expired sessions: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("check expired session delete: %w", err)
	}
	return affected, nil
}

func scanUser(row scanner) (User, error) {
	var user User
	var mustChange int
	var disabledAt sql.NullString
	var createdAt, updatedAt string
	err := row.Scan(&user.ID, &user.Username, &user.PasswordHash, &mustChange, &disabledAt, &createdAt, &updatedAt)
	if err != nil {
		return User{}, err
	}
	if err := fillUser(&user, mustChange, disabledAt, createdAt, updatedAt); err != nil {
		return User{}, err
	}
	return user, nil
}

func scanUserWithoutHash(row scanner) (User, error) {
	var user User
	var mustChange int
	var disabledAt sql.NullString
	var createdAt, updatedAt string
	err := row.Scan(&user.ID, &user.Username, &mustChange, &disabledAt, &createdAt, &updatedAt)
	if err != nil {
		return User{}, err
	}
	if err := fillUser(&user, mustChange, disabledAt, createdAt, updatedAt); err != nil {
		return User{}, err
	}
	return user, nil
}

func fillUser(user *User, mustChange int, disabledAt sql.NullString, createdAt, updatedAt string) error {
	user.MustChangePassword = mustChange != 0
	if disabledAt.Valid {
		t, err := readTime(disabledAt.String)
		if err != nil {
			return err
		}
		user.DisabledAt = &t
	}
	var err error
	if user.CreatedAt, err = readTime(createdAt); err != nil {
		return err
	}
	if user.UpdatedAt, err = readTime(updatedAt); err != nil {
		return err
	}
	return nil
}

func scanSession(row scanner) (Session, error) {
	var session Session
	var createdAt, expiresAt, lastSeenAt string
	err := row.Scan(&session.IDHash, &session.UserID, &createdAt, &expiresAt, &lastSeenAt)
	if err != nil {
		return Session{}, err
	}
	if session.CreatedAt, err = readTime(createdAt); err != nil {
		return Session{}, err
	}
	if session.ExpiresAt, err = readTime(expiresAt); err != nil {
		return Session{}, err
	}
	if session.LastSeenAt, err = readTime(lastSeenAt); err != nil {
		return Session{}, err
	}
	return session, nil
}

func nullableTimePtr(value *time.Time) any {
	if value == nil {
		return nil
	}
	return nullableTime(*value)
}
