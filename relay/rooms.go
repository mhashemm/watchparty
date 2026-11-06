package main

import (
	"crypto/sha256"
	"database/sql"
	"errors"
	"time"

	_ "modernc.org/sqlite"
)

const forgetAfter = 30 * 24 * time.Hour

// remembered keeps the rooms key holders opened, so a member can bring one back
// without the key after its last member left. Only a hash of the secret is stored.
type remembered struct {
	db     *sql.DB
	forget time.Duration
}

type known struct {
	secret [sha256.Size]byte
	file   string
}

func openRemembered(path string) (*remembered, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// one connection: sqlite has one writer, and ":memory:" is per connection
	db.SetMaxOpenConns(1)
	for _, q := range []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA temp_store=MEMORY`,
		`CREATE TABLE IF NOT EXISTS rooms (name TEXT PRIMARY KEY, secret BLOB NOT NULL, file TEXT NOT NULL, used INTEGER NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS rooms_used ON rooms (used)`,
	} {
		if _, err := db.Exec(q); err != nil {
			db.Close()
			return nil, err
		}
	}
	return &remembered{db: db, forget: forgetAfter}, nil
}

func (s *remembered) remember(name string, secret []byte, file string) error {
	now := time.Now().Unix()
	sum := sha256.Sum256(secret)
	_, err := s.db.Exec(`INSERT INTO rooms (name, secret, file, used) VALUES (?, ?, ?, ?)
		ON CONFLICT (name) DO UPDATE SET secret = excluded.secret, file = excluded.file, used = excluded.used`, name, sum[:], file, now)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`DELETE FROM rooms WHERE used < ?`, now-int64(s.forget.Seconds()))
	return err
}

func (s *remembered) touch(name string) error {
	_, err := s.db.Exec(`UPDATE rooms SET used = ? WHERE name = ?`, time.Now().Unix(), name)
	return err
}

// lookup returns nil for a room never opened, or forgotten.
func (s *remembered) lookup(name string) (*known, error) {
	var secret []byte
	k := &known{}
	err := s.db.QueryRow(`SELECT secret, file FROM rooms WHERE name = ? AND used >= ?`, name, time.Now().Add(-s.forget).Unix()).Scan(&secret, &k.file)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	copy(k.secret[:], secret)
	return k, nil
}
