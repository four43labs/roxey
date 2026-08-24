package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// schemaVersion is bumped whenever the on-disk layout changes in a way
// that cannot be migrated in place. Open refuses older databases with a
// clear message rather than silently misreading them.
const schemaVersion = 2

type User struct {
	ID        string `json:"id"`
	Email     string `json:"email"`
	CreatedAt string `json:"createdAt"`
}

type APIKey struct {
	ID         string  `json:"id"`
	Label      string  `json:"label"`
	CreatedAt  string  `json:"createdAt"`
	LastUsedAt *string `json:"lastUsedAt"`
}

type Store struct{ db *sql.DB }

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

func Open(path string) (*Store, error) {
	// WAL + busy timeout so concurrent tunnel handshakes (each validating
	// its API key with an UPDATE) don't fail with SQLITE_BUSY.
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}

	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		db.Close()
		return nil, err
	}
	if version > 0 && version < schemaVersion {
		db.Close()
		return nil, fmt.Errorf(
			"database at %s uses schema v%d but this relay needs v%d; accounts were introduced in v2 and old databases cannot be migrated — delete the file and register again",
			path, version, schemaVersion)
	}
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
		db.Close()
		return nil, err
	}

	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS users (
		id TEXT PRIMARY KEY,
		email TEXT NOT NULL UNIQUE,
		password_hash TEXT NOT NULL,
		created_at TEXT NOT NULL
	)`); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS api_keys (
		id TEXT PRIMARY KEY,
		user_id TEXT NOT NULL,
		label TEXT NOT NULL,
		hash TEXT NOT NULL UNIQUE,
		created_at TEXT NOT NULL,
		last_used_at TEXT
	)`); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS tunnel_events (
		id TEXT PRIMARY KEY,
		user_id TEXT NOT NULL,
		service TEXT NOT NULL,
		host TEXT NOT NULL,
		path TEXT NOT NULL,
		remote TEXT,
		connected_at TEXT NOT NULL,
		disconnected_at TEXT
	)`); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func hashKey(k string) string {
	sum := sha256.Sum256([]byte(k))
	return hex.EncodeToString(sum[:])
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ── users ────────────────────────────────────────────────────────────────

// CreateUser registers a new account; email uniqueness is enforced by the DB.
func (s *Store) CreateUser(email, passwordHash string) (User, error) {
	id := randomHex(16)
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.Exec(`INSERT INTO users (id, email, password_hash, created_at) VALUES (?, ?, ?, ?)`,
		id, email, passwordHash, now)
	if err != nil {
		return User{}, err
	}
	return User{ID: id, Email: email, CreatedAt: now}, nil
}

// GetUserByEmail returns the account for email, or ok=false if absent.
func (s *Store) GetUserByEmail(email string) (User, string, bool, error) {
	var u User
	var hash string
	err := s.db.QueryRow(`SELECT id, email, created_at, password_hash FROM users WHERE email = ?`, email).
		Scan(&u.ID, &u.Email, &u.CreatedAt, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, "", false, nil
	}
	if err != nil {
		return User{}, "", false, err
	}
	return u, hash, true, nil
}

// GetUserByID returns the account for id, or ok=false if absent.
func (s *Store) GetUserByID(id string) (User, bool, error) {
	var u User
	err := s.db.QueryRow(`SELECT id, email, created_at FROM users WHERE id = ?`, id).
		Scan(&u.ID, &u.Email, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, false, nil
	}
	if err != nil {
		return User{}, false, err
	}
	return u, true, nil
}

// SeedSingleUser upserts the SINGLE_USER admin account by email: the row is
// inserted if missing, otherwise its password hash is refreshed from env so
// regenerated CLI credentials always win. Env is the source of truth.
func (s *Store) SeedSingleUser(email, passwordHash string) error {
	u, _, exists, err := s.GetUserByEmail(email)
	if err != nil {
		return err
	}
	if !exists {
		_, err = s.CreateUser(email, passwordHash)
		return err
	}
	_, err = s.db.Exec(`UPDATE users SET password_hash = ? WHERE id = ?`, passwordHash, u.ID)
	return err
}

// ── api keys ─────────────────────────────────────────────────────────────

// CreateAPIKey generates and stores a new key for userID, returning the
// plaintext once (never persisted).
func (s *Store) CreateAPIKey(userID, label string) (plain string, key APIKey, err error) {
	plain = "rxy_" + randomHex(24)
	id := randomHex(16)
	now := time.Now().UTC().Format(time.RFC3339)
	if label == "" {
		label = "unnamed"
	}
	if _, err = s.db.Exec(`INSERT INTO api_keys (id, user_id, label, hash, created_at) VALUES (?, ?, ?, ?, ?)`,
		id, userID, label, hashKey(plain), now); err != nil {
		return "", APIKey{}, err
	}
	return plain, APIKey{ID: id, Label: label, CreatedAt: now}, nil
}

func (s *Store) ListAPIKeys(userID string) ([]APIKey, error) {
	rows, err := s.db.Query(`SELECT id, label, created_at, last_used_at FROM api_keys WHERE user_id = ? ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []APIKey{}
	for rows.Next() {
		var k APIKey
		var lastUsed sql.NullString
		if err := rows.Scan(&k.ID, &k.Label, &k.CreatedAt, &lastUsed); err != nil {
			return nil, err
		}
		if lastUsed.Valid {
			k.LastUsedAt = &lastUsed.String
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// RevokeAPIKey deletes a key owned by userID; reports whether it existed.
func (s *Store) RevokeAPIKey(userID, id string) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM api_keys WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ValidateAPIKey checks the key and, if valid, bumps last_used_at and
// returns the owning user's ID.
func (s *Store) ValidateAPIKey(plain string) (userID string, ok bool) {
	if plain == "" {
		return "", false
	}
	var owner string
	err := s.db.QueryRow(`SELECT user_id FROM api_keys WHERE hash = ?`, hashKey(plain)).Scan(&owner)
	if err != nil {
		return "", false
	}
	_, err = s.db.Exec(`UPDATE api_keys SET last_used_at = ? WHERE hash = ?`,
		time.Now().UTC().Format(time.RFC3339), hashKey(plain))
	if err != nil {
		return "", false
	}
	return owner, true
}

// ── tunnel events ────────────────────────────────────────────────────────

type TunnelEvent struct {
	ID             string  `json:"id"`
	Service        string  `json:"service"`
	Host           string  `json:"host"`
	Path           string  `json:"path"`
	Remote         string  `json:"remote"`
	ConnectedAt    string  `json:"connectedAt"`
	DisconnectedAt *string `json:"disconnectedAt"`
}

// RecordConnect logs a tunnel registration for userID; the returned id is
// passed to RecordDisconnect when that same websocket closes. This is a
// history log only — the live routing table (which holds the actual
// connection) stays in-memory in the relay package and is not reconstructed
// from this.
func (s *Store) RecordConnect(userID, service, host, path, remote string) (string, error) {
	id := randomHex(16)
	_, err := s.db.Exec(`INSERT INTO tunnel_events (id, user_id, service, host, path, remote, connected_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, userID, service, host, path, remote, time.Now().UTC().Format(time.RFC3339))
	return id, err
}

func (s *Store) RecordDisconnect(id string) error {
	_, err := s.db.Exec(`UPDATE tunnel_events SET disconnected_at = ? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339), id)
	return err
}

func (s *Store) ListTunnelEvents(userID string, limit int) ([]TunnelEvent, error) {
	rows, err := s.db.Query(`SELECT id, service, host, path, remote, connected_at, disconnected_at
		FROM tunnel_events WHERE user_id = ? ORDER BY connected_at DESC LIMIT ?`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []TunnelEvent{}
	for rows.Next() {
		var e TunnelEvent
		var disconnected sql.NullString
		if err := rows.Scan(&e.ID, &e.Service, &e.Host, &e.Path, &e.Remote, &e.ConnectedAt, &disconnected); err != nil {
			return nil, err
		}
		if disconnected.Valid {
			e.DisconnectedAt = &disconnected.String
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
