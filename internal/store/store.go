// Package store persists gateway state without retaining inference content.
package store

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gmoigneu/openaisub-gateway/internal/openaiauth"
	_ "modernc.org/sqlite"
)

type Store struct {
	db   *sql.DB
	aead cipher.AEAD
}
type Key struct {
	ID        string
	Name      string
	Prefix    string
	CreatedAt string
	Revoked   bool
}

func Open(dir string, key []byte) (*Store, error) {
	if len(key) != 32 {
		return nil, errors.New("encryption key must contain 32 bytes")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "gateway.db")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	if err = os.Chmod(path, 0600); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, aead: aead}
	_, err = db.Exec(`PRAGMA busy_timeout=5000; PRAGMA journal_mode=WAL;
 CREATE TABLE IF NOT EXISTS metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS credentials (id INTEGER PRIMARY KEY CHECK(id=1), revision INTEGER NOT NULL, sealed BLOB NOT NULL);
 CREATE TABLE IF NOT EXISTS client_keys (id TEXT PRIMARY KEY,name TEXT NOT NULL,prefix TEXT NOT NULL,digest BLOB UNIQUE NOT NULL,created_at TEXT NOT NULL,revoked INTEGER NOT NULL DEFAULT 0);`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("initialize database: %w", err)
	}
	// Decrypt on startup so a replaced secret fails before serving requests.
	if _, err = s.LoadCredentials(context.Background()); err != nil && !errors.Is(err, openaiauth.ErrNotConnected) {
		db.Close()
		return nil, err
	}
	return s, nil
}
func (s *Store) Close() error { return s.db.Close() }
func (s *Store) HostID(ctx context.Context) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	id := fmt.Sprintf("urn:uuid:%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
	if _, err := s.db.ExecContext(ctx, "INSERT OR IGNORE INTO metadata(key,value) VALUES('host_id',?)", id); err != nil {
		return "", err
	}
	err := s.db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='host_id'").Scan(&id)
	return id, err
}
func (s *Store) LoadCredentials(ctx context.Context) (openaiauth.Credentials, error) {
	var c openaiauth.Credentials
	var sealed []byte
	var revision int64
	err := s.db.QueryRowContext(ctx, "SELECT revision,sealed FROM credentials WHERE id=1").Scan(&revision, &sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return c, openaiauth.ErrNotConnected
	}
	if err != nil {
		return c, err
	}
	if len(sealed) < s.aead.NonceSize() {
		return c, errors.New("invalid encrypted credentials")
	}
	plain, err := s.aead.Open(nil, sealed[:s.aead.NonceSize()], sealed[s.aead.NonceSize():], []byte("credentials:v1"))
	if err != nil {
		return c, errors.New("cannot decrypt credentials; restore the original encryption key")
	}
	if err = json.Unmarshal(plain, &c); err != nil {
		return c, errors.New("invalid credential record")
	}
	c.Revision = revision
	return c, nil
}
func (s *Store) SaveCredentials(ctx context.Context, c openaiauth.Credentials) error {
	plain, err := json.Marshal(c)
	if err != nil {
		return err
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return err
	}
	sealed := s.aead.Seal(nonce, nonce, plain, []byte("credentials:v1"))
	var result sql.Result
	if c.Revision == 0 {
		result, err = s.db.ExecContext(ctx, "INSERT OR IGNORE INTO credentials(id,revision,sealed) VALUES(1,1,?)", sealed)
	} else {
		result, err = s.db.ExecContext(ctx, "UPDATE credentials SET revision=revision+1,sealed=? WHERE id=1 AND revision=?", sealed, c.Revision)
	}
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return openaiauth.ErrConflict
	}
	return nil
}
func (s *Store) ClearCredentials(ctx context.Context, revision int64) error {
	c, err := s.LoadCredentials(ctx)
	if err != nil {
		return err
	}
	if c.Revision != revision {
		return openaiauth.ErrConflict
	}
	c.AccessToken = ""
	c.RefreshToken = ""
	c.IDToken = ""
	c.Scopes = nil
	c.ExpiresAt = time.Time{}
	return s.SaveCredentials(ctx, c)
}
func (s *Store) CreateKey(ctx context.Context, name string) (Key, string, error) {
	var k Key
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return k, "", err
	}
	token := "osk_" + base64.RawURLEncoding.EncodeToString(b)
	digest := sha256.Sum256([]byte(token))
	id := hex.EncodeToString(b[:8])
	k = Key{ID: id, Name: name, Prefix: token[:12], CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	_, err := s.db.ExecContext(ctx, "INSERT INTO client_keys(id,name,prefix,digest,created_at) VALUES(?,?,?,?,?)", k.ID, k.Name, k.Prefix, digest[:], k.CreatedAt)
	if err != nil {
		return Key{}, "", err
	}
	return k, token, nil
}
func (s *Store) Authenticate(ctx context.Context, token string) bool {
	if len(token) != 47 {
		return false
	}
	digest := sha256.Sum256([]byte(token))
	var count int
	return s.db.QueryRowContext(ctx, "SELECT count(*) FROM client_keys WHERE digest=? AND revoked=0", digest[:]).Scan(&count) == nil && count == 1
}
func (s *Store) Keys(ctx context.Context) ([]Key, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id,name,prefix,created_at,revoked FROM client_keys ORDER BY created_at DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := []Key{}
	for rows.Next() {
		var k Key
		if err = rows.Scan(&k.ID, &k.Name, &k.Prefix, &k.CreatedAt, &k.Revoked); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}
func (s *Store) RevokeKey(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE client_keys SET revoked=1 WHERE id=?", id)
	return err
}
