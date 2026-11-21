// Package core holds ZeroOS's configuration, database and small shared helpers.
package core

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Version is set at build time with -ldflags "-X github.com/MightyX3N/zeroos/internal/core.Version=..."
var Version = "dev"

// Config holds where ZeroOS keeps its things and how it listens.
type Config struct {
	DataDir   string // on the SSD: database, app data, store cache
	MountRoot string // where NAS shares are mounted
	Listen    string // web UI address, e.g. ":80"
	TLSListen string // encrypted web UI address, used for reaching ZeroOS from outside
	Socket    string // local socket for the command-line tool
}

// DefaultConfig reads ZEROOS_* environment variables, with sensible defaults.
func DefaultConfig() Config {
	c := Config{
		DataDir:   "/var/lib/zeroos",
		MountRoot: "/mnt/zeroos",
		Listen:    ":80",
		TLSListen: ":8443",
		Socket:    "/run/zeroos/zeroos.sock",
	}
	if v := os.Getenv("ZEROOS_DATA"); v != "" {
		c.DataDir = v
	}
	if v := os.Getenv("ZEROOS_MOUNTS"); v != "" {
		c.MountRoot = v
	}
	if v := os.Getenv("ZEROOS_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("ZEROOS_TLS_LISTEN"); v != "" {
		c.TLSListen = v
	}
	if v := os.Getenv("ZEROOS_SOCKET"); v != "" {
		c.Socket = v
	}
	return c
}

func (c Config) AppsDir() string  { return filepath.Join(c.DataDir, "apps") }
func (c Config) StoreDir() string { return filepath.Join(c.DataDir, "store") }
func (c Config) MediaDir() string { return filepath.Join(c.DataDir, "media") }

// Core is shared by every part of ZeroOS.
type Core struct {
	Cfg Config
	DB  *sql.DB
	key []byte
	mu  sync.Mutex
}

// Open creates the data folders, opens the database and runs migrations.
func Open(cfg Config) (*Core, error) {
	for _, d := range []string{cfg.DataDir, cfg.AppsDir(), cfg.StoreDir(), cfg.MediaDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("create %s: %w", d, err)
		}
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(cfg.DataDir, "zeroos.db")+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	c := &Core{Cfg: cfg, DB: db}
	if err := c.migrate(); err != nil {
		return nil, fmt.Errorf("database migration: %w", err)
	}
	if err := c.loadKey(); err != nil {
		return nil, err
	}
	return c, nil
}

var migrations = []string{
	`CREATE TABLE roles (
		id INTEGER PRIMARY KEY,
		name TEXT NOT NULL UNIQUE,
		perms TEXT NOT NULL DEFAULT '[]',
		app_perms TEXT NOT NULL DEFAULT '[]',
		builtin INTEGER NOT NULL DEFAULT 0
	);
	CREATE TABLE users (
		id INTEGER PRIMARY KEY,
		username TEXT NOT NULL UNIQUE COLLATE NOCASE,
		pass_hash TEXT NOT NULL,
		role_id INTEGER REFERENCES roles(id),
		is_owner INTEGER NOT NULL DEFAULT 0,
		extra_perms TEXT NOT NULL DEFAULT '[]',
		removed_perms TEXT NOT NULL DEFAULT '[]',
		quota_apps INTEGER NOT NULL DEFAULT 0,
		quota_tunnels INTEGER NOT NULL DEFAULT 0,
		totp_secret TEXT NOT NULL DEFAULT '',
		failed_logins INTEGER NOT NULL DEFAULT 0,
		locked_until INTEGER NOT NULL DEFAULT 0,
		last_seen INTEGER NOT NULL DEFAULT 0,
		created INTEGER NOT NULL
	);
	CREATE TABLE sessions (
		token_hash TEXT PRIMARY KEY,
		user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		created INTEGER NOT NULL,
		expires INTEGER NOT NULL,
		agent TEXT NOT NULL DEFAULT ''
	);
	CREATE TABLE apps (
		id TEXT PRIMARY KEY,
		store_id TEXT NOT NULL,
		catalog TEXT NOT NULL,
		name TEXT NOT NULL,
		port INTEGER NOT NULL DEFAULT 0,
		state TEXT NOT NULL DEFAULT 'installing',
		settings TEXT NOT NULL DEFAULT '{}',
		folders TEXT NOT NULL DEFAULT '{}',
		shares TEXT NOT NULL DEFAULT '[]',
		installed_by INTEGER,
		created INTEGER NOT NULL
	);
	CREATE TABLE app_perms (
		user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		app_id TEXT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
		perm TEXT NOT NULL,
		PRIMARY KEY (user_id, app_id, perm)
	);
	CREATE TABLE nas_shares (
		id INTEGER PRIMARY KEY,
		name TEXT NOT NULL UNIQUE,
		host TEXT NOT NULL,
		share TEXT NOT NULL,
		username TEXT NOT NULL DEFAULT '',
		password_enc TEXT NOT NULL DEFAULT '',
		created INTEGER NOT NULL
	);
	CREATE TABLE tunnels (
		id INTEGER PRIMARY KEY,
		kind TEXT NOT NULL,
		app_id TEXT,
		name TEXT NOT NULL,
		local_port INTEGER NOT NULL,
		proto TEXT NOT NULL,
		playit_id TEXT NOT NULL DEFAULT '',
		address TEXT NOT NULL DEFAULT '',
		created_by INTEGER,
		created INTEGER NOT NULL
	);
	CREATE TABLE settings (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL
	);
	CREATE TABLE activity (
		id INTEGER PRIMARY KEY,
		ts INTEGER NOT NULL,
		user TEXT NOT NULL,
		action TEXT NOT NULL,
		target TEXT NOT NULL DEFAULT '',
		app_id TEXT NOT NULL DEFAULT '',
		ok INTEGER NOT NULL DEFAULT 1,
		detail TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX activity_ts ON activity(ts);`,
}

func (c *Core) migrate() error {
	if _, err := c.DB.Exec(`CREATE TABLE IF NOT EXISTS schema_version (v INTEGER NOT NULL)`); err != nil {
		return err
	}
	var v int
	if err := c.DB.QueryRow(`SELECT COALESCE(MAX(v),0) FROM schema_version`).Scan(&v); err != nil {
		return err
	}
	for i := v; i < len(migrations); i++ {
		tx, err := c.DB.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return err
		}
		if _, err := tx.Exec(`INSERT INTO schema_version(v) VALUES (?)`, i+1); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Setting reads a stored setting, or "" when it isn't set.
func (c *Core) Setting(key string) string {
	var v string
	_ = c.DB.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	return v
}

// SetSetting stores a setting.
func (c *Core) SetSetting(key, value string) error {
	_, err := c.DB.Exec(`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// --- secrets at rest -------------------------------------------------------

func (c *Core) loadKey() error {
	p := filepath.Join(c.Cfg.DataDir, "secret.key")
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		k := make([]byte, 32)
		if _, err := rand.Read(k); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(hex.EncodeToString(k)), 0o600); err != nil {
			return err
		}
		c.key = k
		return nil
	}
	if err != nil {
		return err
	}
	k, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(k) != 32 {
		return errors.New("secret.key is damaged")
	}
	c.key = k
	return nil
}

// Encrypt protects a secret (playit key, NAS password) before it is stored.
func (c *Core) Encrypt(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	block, err := aes.NewCipher(c.key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(gcm.Seal(nonce, nonce, []byte(plain), nil)), nil
}

// Decrypt reverses Encrypt.
func (c *Core) Decrypt(enc string) (string, error) {
	if enc == "" {
		return "", nil
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(c.key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("secret too short")
	}
	out, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	return string(out), err
}

// --- activity log ----------------------------------------------------------

// Log records one action in the activity log.
func (c *Core) Log(user, action, target, appID string, ok bool, detail string) {
	okInt := 0
	if ok {
		okInt = 1
	}
	_, _ = c.DB.Exec(`INSERT INTO activity(ts,user,action,target,app_id,ok,detail) VALUES(?,?,?,?,?,?,?)`,
		time.Now().Unix(), user, action, target, appID, okInt, detail)
}

// RandomHex returns n random bytes as hex.
func RandomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// RandomString returns a random password-like string of the given length.
func RandomString(n int) string {
	const chars = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, n)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = chars[int(b[i])%len(chars)]
	}
	return string(b)
}
