package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
	_ "modernc.org/sqlite"
)

type State struct {
	DB   *sql.DB
	lock *flock.Flock
}

func OpenState(path string) (*State, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, errors.New("cannot create state directory")
	}
	lock := flock.New(path + ".lock")
	locked, err := lock.TryLock()
	if err != nil || !locked {
		return nil, errors.New("state is locked; only one worker may run")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		lock.Close()
		return nil, errors.New("cannot open state file")
	}
	file.Close()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		lock.Close()
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; CREATE TABLE IF NOT EXISTS state (key TEXT PRIMARY KEY, value BLOB NOT NULL);`); err != nil {
		db.Close()
		lock.Close()
		return nil, err
	}
	return &State{db, lock}, nil
}

func (state *State) Close() error { return errors.Join(state.DB.Close(), state.lock.Close()) }

func (state *State) Get(key string, value any) (bool, error) {
	var encoded []byte
	err := state.DB.QueryRow(`SELECT value FROM state WHERE key = ?`, key).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(encoded, value); err != nil {
		return false, errors.New("corrupt state; refusing to reset ownership")
	}
	return true, nil
}

func (state *State) Put(key string, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = state.DB.Exec(`INSERT INTO state(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, encoded)
	return err
}

type BindingState struct {
	SourceDigest string
	Fingerprint  string            `json:"fingerprint"`
	CollectionID string            `json:"collection_id"`
	Creating     bool              `json:"creating"`
	Matches      map[string]string `json:"matches"`
	LastSuccess  time.Time         `json:"last_success"`
}

type CachedDetail struct {
	Item    CatalogEntry `json:"item"`
	Fetched time.Time    `json:"fetched"`
}
