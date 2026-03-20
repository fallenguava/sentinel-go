package database

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"

	_ "github.com/lib/pq"
)

// DB holds the database connection
type DB struct {
	conn *sql.DB
}

// AuthDuration is how long authorization lasts
const AuthDuration = 7 * 24 * time.Hour // 7 days

// AuthorizedUser represents an authorized user in the database
type AuthorizedUser struct {
	ID        int64
	ChatID    int64
	Name      string
	CreatedAt time.Time
	ExpiresAt time.Time
	LastSeen  time.Time
}

// AuthState represents the authentication state of a user
type AuthState struct {
	ChatID    int64
	Step      int    // 0 = not started, 1 = asked name, 2 = asked security question
	Name      string // Name provided in step 1
	Attempts  int    // Failed attempts
	UpdatedAt time.Time
}

// New creates a new database connection
func New(host, port, user, password, dbname string) (*DB, error) {
	connStr := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		host, port, user, password, dbname)

	conn, err := sql.Open("postgres", connStr)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Test the connection
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := conn.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	db := &DB{conn: conn}

	// Initialize tables
	if err := db.initTables(ctx); err != nil {
		return nil, fmt.Errorf("failed to initialize tables: %w", err)
	}

	log.Println("[DATABASE] Connected successfully")
	return db, nil
}

// initTables creates the necessary tables if they don't exist
func (db *DB) initTables(ctx context.Context) error {
	queries := []string{
		`CREATE TABLE IF NOT EXISTS authorized_users (
			id SERIAL PRIMARY KEY,
			chat_id BIGINT UNIQUE NOT NULL,
			name VARCHAR(100) NOT NULL,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			expires_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP + INTERVAL '7 days',
			last_seen TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		// Add expires_at column if it doesn't exist (for migration)
		`DO $$ BEGIN
			ALTER TABLE authorized_users ADD COLUMN expires_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP + INTERVAL '7 days';
		EXCEPTION WHEN duplicate_column THEN END $$`,
		`CREATE TABLE IF NOT EXISTS auth_states (
			chat_id BIGINT PRIMARY KEY,
			step INTEGER DEFAULT 0,
			name VARCHAR(100) DEFAULT '',
			attempts INTEGER DEFAULT 0,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_authorized_users_chat_id ON authorized_users(chat_id)`,
	}

	for _, query := range queries {
		if _, err := db.conn.ExecContext(ctx, query); err != nil {
			return fmt.Errorf("failed to execute query: %w", err)
		}
	}

	log.Println("[DATABASE] Tables initialized")
	return nil
}

// IsAuthorized checks if a chat ID is authorized and not expired
func (db *DB) IsAuthorized(ctx context.Context, chatID int64) (bool, error) {
	var exists bool
	err := db.conn.QueryRowContext(ctx,
		"SELECT EXISTS(SELECT 1 FROM authorized_users WHERE chat_id = $1 AND expires_at > CURRENT_TIMESTAMP)",
		chatID).Scan(&exists)
	if err != nil {
		return false, err
	}
	return exists, nil
}

// IsExpired checks if a user's authorization has expired
func (db *DB) IsExpired(ctx context.Context, chatID int64) (bool, *AuthorizedUser, error) {
	user := &AuthorizedUser{}
	err := db.conn.QueryRowContext(ctx,
		"SELECT id, chat_id, name, created_at, expires_at, last_seen FROM authorized_users WHERE chat_id = $1",
		chatID).Scan(&user.ID, &user.ChatID, &user.Name, &user.CreatedAt, &user.ExpiresAt, &user.LastSeen)
	if err == sql.ErrNoRows {
		return false, nil, nil // User doesn't exist, not expired
	}
	if err != nil {
		return false, nil, err
	}
	// User exists, check if expired
	return time.Now().After(user.ExpiresAt), user, nil
}

// AddAuthorizedUser adds a new authorized user with 7-day expiry
func (db *DB) AddAuthorizedUser(ctx context.Context, chatID int64, name string) error {
	_, err := db.conn.ExecContext(ctx,
		`INSERT INTO authorized_users (chat_id, name, created_at, expires_at, last_seen) 
		 VALUES ($1, $2, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP + INTERVAL '7 days', CURRENT_TIMESTAMP)
		 ON CONFLICT (chat_id) DO UPDATE SET 
		 name = $2, expires_at = CURRENT_TIMESTAMP + INTERVAL '7 days', last_seen = CURRENT_TIMESTAMP`,
		chatID, name)
	return err
}

// RenewAuthorization extends the user's authorization by 7 days
func (db *DB) RenewAuthorization(ctx context.Context, chatID int64) error {
	_, err := db.conn.ExecContext(ctx,
		"UPDATE authorized_users SET expires_at = CURRENT_TIMESTAMP + INTERVAL '7 days', last_seen = CURRENT_TIMESTAMP WHERE chat_id = $1",
		chatID)
	return err
}

// UpdateLastSeen updates the last seen timestamp for a user
func (db *DB) UpdateLastSeen(ctx context.Context, chatID int64) error {
	_, err := db.conn.ExecContext(ctx,
		"UPDATE authorized_users SET last_seen = CURRENT_TIMESTAMP WHERE chat_id = $1",
		chatID)
	return err
}

// GetAuthState gets the current authentication state for a chat
func (db *DB) GetAuthState(ctx context.Context, chatID int64) (*AuthState, error) {
	state := &AuthState{ChatID: chatID}
	err := db.conn.QueryRowContext(ctx,
		"SELECT step, name, attempts, updated_at FROM auth_states WHERE chat_id = $1",
		chatID).Scan(&state.Step, &state.Name, &state.Attempts, &state.UpdatedAt)

	if err == sql.ErrNoRows {
		// Create new state
		_, err = db.conn.ExecContext(ctx,
			"INSERT INTO auth_states (chat_id, step, name, attempts) VALUES ($1, 0, '', 0)",
			chatID)
		if err != nil {
			return nil, err
		}
		state.Step = 0
		state.Name = ""
		state.Attempts = 0
		state.UpdatedAt = time.Now()
		return state, nil
	}

	if err != nil {
		return nil, err
	}
	return state, nil
}

// UpdateAuthState updates the authentication state
func (db *DB) UpdateAuthState(ctx context.Context, state *AuthState) error {
	_, err := db.conn.ExecContext(ctx,
		`INSERT INTO auth_states (chat_id, step, name, attempts, updated_at) 
		 VALUES ($1, $2, $3, $4, CURRENT_TIMESTAMP)
		 ON CONFLICT (chat_id) DO UPDATE SET 
		 step = $2, name = $3, attempts = $4, updated_at = CURRENT_TIMESTAMP`,
		state.ChatID, state.Step, state.Name, state.Attempts)
	return err
}

// ResetAuthState resets the authentication state for a chat
func (db *DB) ResetAuthState(ctx context.Context, chatID int64) error {
	_, err := db.conn.ExecContext(ctx,
		"UPDATE auth_states SET step = 0, name = '', attempts = 0, updated_at = CURRENT_TIMESTAMP WHERE chat_id = $1",
		chatID)
	return err
}

// GetAuthorizedUser gets an authorized user by chat ID
func (db *DB) GetAuthorizedUser(ctx context.Context, chatID int64) (*AuthorizedUser, error) {
	user := &AuthorizedUser{}
	err := db.conn.QueryRowContext(ctx,
		"SELECT id, chat_id, name, created_at, expires_at, last_seen FROM authorized_users WHERE chat_id = $1",
		chatID).Scan(&user.ID, &user.ChatID, &user.Name, &user.CreatedAt, &user.ExpiresAt, &user.LastSeen)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return user, nil
}

// RemoveAuthorizedUser removes an authorized user
func (db *DB) RemoveAuthorizedUser(ctx context.Context, chatID int64) error {
	_, err := db.conn.ExecContext(ctx,
		"DELETE FROM authorized_users WHERE chat_id = $1", chatID)
	return err
}

// GetAllAuthorizedUsers returns all authorized users (not expired)
func (db *DB) GetAllAuthorizedUsers(ctx context.Context) ([]*AuthorizedUser, error) {
	rows, err := db.conn.QueryContext(ctx,
		"SELECT id, chat_id, name, created_at, expires_at, last_seen FROM authorized_users WHERE expires_at > CURRENT_TIMESTAMP ORDER BY created_at")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []*AuthorizedUser
	for rows.Next() {
		user := &AuthorizedUser{}
		if err := rows.Scan(&user.ID, &user.ChatID, &user.Name, &user.CreatedAt, &user.ExpiresAt, &user.LastSeen); err != nil {
			return nil, err
		}
		users = append(users, user)
	}
	return users, nil
}

// Close closes the database connection
func (db *DB) Close() error {
	return db.conn.Close()
}
