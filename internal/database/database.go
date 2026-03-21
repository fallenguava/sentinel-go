package database

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"

	_ "github.com/lib/pq"
)

const (
	sessionDuration       = 72 * time.Hour
	servicePINValidWindow = 10 * time.Minute
)

// DB holds the database connection.
type DB struct {
	conn *sql.DB
}

// User stores registration and role details.
type User struct {
	ChatID     int64
	Name       string
	PinHash    string
	Role       string
	Status     string
	CreatedAt  time.Time
	ApprovedAt sql.NullTime
}

// Session stores login and admin service PIN confirmation state.
type Session struct {
	ChatID         int64
	LoggedInAt     time.Time
	ExpiresAt      time.Time
	PinConfirmedAt sql.NullTime
}

// New creates a new database connection.
func New(host, port, user, password, dbname string) (*DB, error) {
	connStr := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		host, port, user, password, dbname)

	conn, err := sql.Open("postgres", connStr)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := conn.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	db := &DB{conn: conn}
	if err := db.initTables(ctx); err != nil {
		return nil, fmt.Errorf("failed to initialize tables: %w", err)
	}

	log.Println("[DATABASE] Connected successfully")
	return db, nil
}

func (db *DB) initTables(ctx context.Context) error {
	queries := []string{
		`CREATE TABLE IF NOT EXISTS users (
			chat_id BIGINT PRIMARY KEY,
			name VARCHAR(100) NOT NULL,
			pin_hash VARCHAR(255) NOT NULL,
			role VARCHAR(20) NOT NULL DEFAULT 'user',
			status VARCHAR(20) NOT NULL DEFAULT 'pending',
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			approved_at TIMESTAMP NULL,
			CHECK (role IN ('admin', 'user')),
			CHECK (status IN ('pending', 'approved', 'rejected'))
		)`,
		`CREATE TABLE IF NOT EXISTS sessions (
			chat_id BIGINT PRIMARY KEY,
			logged_in_at TIMESTAMP NOT NULL,
			expires_at TIMESTAMP NOT NULL,
			pin_confirmed_at TIMESTAMP NULL,
			CONSTRAINT fk_sessions_user FOREIGN KEY (chat_id)
			REFERENCES users(chat_id) ON DELETE CASCADE
		)`,
		`CREATE INDEX IF NOT EXISTS idx_users_status ON users(status)`,
		`CREATE INDEX IF NOT EXISTS idx_users_role ON users(role)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_expires_at ON sessions(expires_at)`,
	}

	for _, query := range queries {
		if _, err := db.conn.ExecContext(ctx, query); err != nil {
			return fmt.Errorf("failed to execute query: %w", err)
		}
	}

	log.Println("[DATABASE] Tables initialized")
	return nil
}

// UpsertUser stores registration details and status.
func (db *DB) UpsertUser(ctx context.Context, chatID int64, name, pinHash, role, status string) error {
	if role != "admin" {
		role = "user"
	}
	if status != "approved" && status != "rejected" {
		status = "pending"
	}

	approvedAtExpr := "NULL"
	if status == "approved" {
		approvedAtExpr = "CURRENT_TIMESTAMP"
	}

	query := fmt.Sprintf(`INSERT INTO users (chat_id, name, pin_hash, role, status, created_at, approved_at)
		VALUES ($1, $2, $3, $4, $5, CURRENT_TIMESTAMP, %s)
		ON CONFLICT (chat_id) DO UPDATE SET
		name = EXCLUDED.name,
		pin_hash = EXCLUDED.pin_hash,
		role = EXCLUDED.role,
		status = EXCLUDED.status,
		approved_at = CASE WHEN EXCLUDED.status = 'approved' THEN CURRENT_TIMESTAMP ELSE NULL END`, approvedAtExpr)

	_, err := db.conn.ExecContext(ctx, query, chatID, name, pinHash, role, status)
	return err
}

// GetUser returns a user record by chat ID.
func (db *DB) GetUser(ctx context.Context, chatID int64) (*User, error) {
	u := &User{}
	err := db.conn.QueryRowContext(ctx,
		`SELECT chat_id, name, pin_hash, role, status, created_at, approved_at
		 FROM users WHERE chat_id = $1`,
		chatID,
	).Scan(&u.ChatID, &u.Name, &u.PinHash, &u.Role, &u.Status, &u.CreatedAt, &u.ApprovedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}

// ApproveUser approves a pending user.
func (db *DB) ApproveUser(ctx context.Context, chatID int64) (bool, error) {
	res, err := db.conn.ExecContext(ctx,
		`UPDATE users
		 SET status = 'approved', approved_at = CURRENT_TIMESTAMP
		 WHERE chat_id = $1`,
		chatID,
	)
	if err != nil {
		return false, err
	}
	affected, _ := res.RowsAffected()
	return affected > 0, nil
}

// RejectUser rejects a user request.
func (db *DB) RejectUser(ctx context.Context, chatID int64) (bool, error) {
	res, err := db.conn.ExecContext(ctx,
		`UPDATE users
		 SET status = 'rejected', approved_at = NULL
		 WHERE chat_id = $1`,
		chatID,
	)
	if err != nil {
		return false, err
	}
	affected, _ := res.RowsAffected()
	if affected > 0 {
		_, _ = db.conn.ExecContext(ctx, "DELETE FROM sessions WHERE chat_id = $1", chatID)
	}
	return affected > 0, nil
}

// PromoteUser sets a user role to admin.
func (db *DB) PromoteUser(ctx context.Context, chatID int64) (bool, error) {
	res, err := db.conn.ExecContext(ctx,
		`UPDATE users
		 SET role = 'admin', status = 'approved', approved_at = COALESCE(approved_at, CURRENT_TIMESTAMP)
		 WHERE chat_id = $1`,
		chatID,
	)
	if err != nil {
		return false, err
	}
	affected, _ := res.RowsAffected()
	return affected > 0, nil
}

// RevokeUser removes a user and active session.
func (db *DB) RevokeUser(ctx context.Context, chatID int64) (bool, error) {
	res, err := db.conn.ExecContext(ctx, "DELETE FROM users WHERE chat_id = $1", chatID)
	if err != nil {
		return false, err
	}
	affected, _ := res.RowsAffected()
	return affected > 0, nil
}

// ListApprovedUsers returns all approved users.
func (db *DB) ListApprovedUsers(ctx context.Context) ([]*User, error) {
	rows, err := db.conn.QueryContext(ctx,
		`SELECT chat_id, name, pin_hash, role, status, created_at, approved_at
		 FROM users
		 WHERE status = 'approved'
		 ORDER BY created_at`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	users := make([]*User, 0)
	for rows.Next() {
		u := &User{}
		if err := rows.Scan(&u.ChatID, &u.Name, &u.PinHash, &u.Role, &u.Status, &u.CreatedAt, &u.ApprovedAt); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, nil
}

// CreateOrRefreshSession resets login session for a user.
func (db *DB) CreateOrRefreshSession(ctx context.Context, chatID int64) error {
	_, err := db.conn.ExecContext(ctx,
		`INSERT INTO sessions (chat_id, logged_in_at, expires_at, pin_confirmed_at)
		 VALUES ($1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP + INTERVAL '3 days', NULL)
		 ON CONFLICT (chat_id) DO UPDATE SET
		 logged_in_at = CURRENT_TIMESTAMP,
		 expires_at = CURRENT_TIMESTAMP + INTERVAL '3 days',
		 pin_confirmed_at = NULL`,
		chatID,
	)
	return err
}

// TouchSession extends session expiry based on activity.
func (db *DB) TouchSession(ctx context.Context, chatID int64) error {
	_, err := db.conn.ExecContext(ctx,
		`UPDATE sessions
		 SET expires_at = CURRENT_TIMESTAMP + INTERVAL '3 days'
		 WHERE chat_id = $1`,
		chatID,
	)
	return err
}

// IsSessionActive checks whether a valid session exists.
func (db *DB) IsSessionActive(ctx context.Context, chatID int64) (bool, error) {
	var exists bool
	err := db.conn.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM sessions WHERE chat_id = $1 AND expires_at > CURRENT_TIMESTAMP)`,
		chatID,
	).Scan(&exists)
	if err != nil {
		return false, err
	}
	return exists, nil
}

// GetSession returns session details.
func (db *DB) GetSession(ctx context.Context, chatID int64) (*Session, error) {
	s := &Session{}
	err := db.conn.QueryRowContext(ctx,
		`SELECT chat_id, logged_in_at, expires_at, pin_confirmed_at
		 FROM sessions WHERE chat_id = $1`,
		chatID,
	).Scan(&s.ChatID, &s.LoggedInAt, &s.ExpiresAt, &s.PinConfirmedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s, nil
}

// SetPinConfirmed marks that admin PIN was confirmed for service commands.
func (db *DB) SetPinConfirmed(ctx context.Context, chatID int64) error {
	_, err := db.conn.ExecContext(ctx,
		`UPDATE sessions SET pin_confirmed_at = CURRENT_TIMESTAMP WHERE chat_id = $1`,
		chatID,
	)
	return err
}

// HasValidServicePINConfirm checks if admin service PIN confirmation is still valid.
func (db *DB) HasValidServicePINConfirm(ctx context.Context, chatID int64) (bool, error) {
	var exists bool
	err := db.conn.QueryRowContext(ctx,
		`SELECT EXISTS(
			SELECT 1 FROM sessions
			WHERE chat_id = $1
			  AND pin_confirmed_at IS NOT NULL
			  AND pin_confirmed_at > CURRENT_TIMESTAMP - INTERVAL '10 minutes'
			  AND expires_at > CURRENT_TIMESTAMP
		)`,
		chatID,
	).Scan(&exists)
	if err != nil {
		return false, err
	}
	return exists, nil
}

// ClearSession removes active session data.
func (db *DB) ClearSession(ctx context.Context, chatID int64) error {
	_, err := db.conn.ExecContext(ctx, "DELETE FROM sessions WHERE chat_id = $1", chatID)
	return err
}

// SessionDuration returns login session lifetime.
func SessionDuration() time.Duration {
	return sessionDuration
}

// ServicePINWindow returns how long service PIN confirmation remains valid.
func ServicePINWindow() time.Duration {
	return servicePINValidWindow
}

// Close closes the database connection.
func (db *DB) Close() error {
	return db.conn.Close()
}
