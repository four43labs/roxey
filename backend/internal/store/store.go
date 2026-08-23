package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"time"

	_ "modernc.org/sqlite"
)

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
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS api_keys (
		id TEXT PRIMARY KEY,
		label TEXT NOT NULL,
		hash TEXT NOT NULL UNIQUE,
		created_at TEXT NOT NULL,
		last_used_at TEXT
	)`); err != nil {
		return nil, err
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS tunnel_events (
		id TEXT PRIMARY KEY,
		service TEXT NOT NULL,
		path TEXT NOT NULL,
		remote TEXT,
		connected_at TEXT NOT NULL,
		disconnected_at TEXT
	)`); err != nil {
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

// CreateAPIKey generates and stores a new key, returning the plaintext once (never persisted).
func (s *Store) CreateAPIKey(label string) (plain string, key APIKey, err error) {
	plain = "rxy_" + randomHex(24)
	id := randomHex(16)
	now := time.Now().UTC().Format(time.RFC3339)
	if label == "" {
		label = "unnamed"
	}
	if _, err = s.db.Exec(`INSERT INTO api_keys (id, label, hash, created_at) VALUES (?, ?, ?, ?)`,
		id, label, hashKey(plain), now); err != nil {
		return "", APIKey{}, err
	}
	return plain, APIKey{ID: id, Label: label, CreatedAt: now}, nil
}

func (s *Store) ListAPIKeys() ([]APIKey, error) {
	rows, err := s.db.Query(`SELECT id, label, created_at, last_used_at FROM api_keys ORDER BY created_at DESC`)
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

func (s *Store) RevokeAPIKey(id string) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM api_keys WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ValidateAPIKey checks the key and, if valid, bumps last_used_at.
func (s *Store) ValidateAPIKey(plain string) bool {
	if plain == "" {
		return false
	}
	res, err := s.db.Exec(`UPDATE api_keys SET last_used_at = ? WHERE hash = ?`,
		time.Now().UTC().Format(time.RFC3339), hashKey(plain))
	if err != nil {
		return false
	}
	n, _ := res.RowsAffected()
	return n > 0
}

type TunnelEvent struct {
	ID             string  `json:"id"`
	Service        string  `json:"service"`
	Path           string  `json:"path"`
	Remote         string  `json:"remote"`
	ConnectedAt    string  `json:"connectedAt"`
	DisconnectedAt *string `json:"disconnectedAt"`
}

// RecordConnect logs a tunnel registration; the returned id is passed to
// RecordDisconnect when that same websocket closes. This is a history log
// only — the live routing table (which holds the actual connection) stays
// in-memory in the relay package and is not reconstructed from this.
func (s *Store) RecordConnect(service, path, remote string) (string, error) {
	id := randomHex(16)
	_, err := s.db.Exec(`INSERT INTO tunnel_events (id, service, path, remote, connected_at) VALUES (?, ?, ?, ?, ?)`,
		id, service, path, remote, time.Now().UTC().Format(time.RFC3339))
	return id, err
}

func (s *Store) RecordDisconnect(id string) error {
	_, err := s.db.Exec(`UPDATE tunnel_events SET disconnected_at = ? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339), id)
	return err
}

func (s *Store) ListTunnelEvents(limit int) ([]TunnelEvent, error) {
	rows, err := s.db.Query(`SELECT id, service, path, remote, connected_at, disconnected_at
		FROM tunnel_events ORDER BY connected_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []TunnelEvent{}
	for rows.Next() {
		var e TunnelEvent
		var disconnected sql.NullString
		if err := rows.Scan(&e.ID, &e.Service, &e.Path, &e.Remote, &e.ConnectedAt, &disconnected); err != nil {
			return nil, err
		}
		if disconnected.Valid {
			e.DisconnectedAt = &disconnected.String
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
