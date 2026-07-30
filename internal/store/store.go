package store

import (
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type File struct {
	ID           int64  `json:"id"`
	Slug         string `json:"slug"`
	Name         string `json:"name"`
	Size         int64  `json:"size"`
	Storage      string `json:"storage"`
	ContentType  string `json:"contentType"`
	PasswordHash string `json:"-"`
	HasPassword  bool   `json:"hasPassword"`
	CreatedAt    int64  `json:"createdAt"`
	ExpiresAt    int64  `json:"expiresAt"`    // unix seconds, 0 = never
	Downloads    int64  `json:"downloads"`    // completed download count
	MaxDownloads int64  `json:"maxDownloads"` // 0 = unlimited
}

type Upload struct {
	ID           string
	Slug         string
	Name         string
	Size         int64
	ChunkSize    int64
	TotalChunks  int
	Storage      string
	ProviderID   string
	PasswordHash string
	ExpiresIn    int64 // seconds after completion, 0 = never
	MaxDownloads int64
	CreatedAt    int64
}

type Part struct {
	Idx  int
	ETag string
}

type Store struct {
	db *sql.DB
}

var ErrNotFound = errors.New("not found")

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	// Column additions for databases created by older versions.
	for _, alter := range []string{
		`ALTER TABLE files ADD COLUMN downloads INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE files ADD COLUMN max_downloads INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE uploads ADD COLUMN max_downloads INTEGER NOT NULL DEFAULT 0`,
	} {
		if _, err := db.Exec(alter); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			db.Close()
			return nil, fmt.Errorf("migrate: %w", err)
		}
	}
	for _, idx := range []string{
		`CREATE INDEX IF NOT EXISTS idx_files_expires ON files(expires_at)`,
		`CREATE INDEX IF NOT EXISTS idx_files_exhausted ON files(max_downloads, downloads)`,
		`CREATE INDEX IF NOT EXISTS idx_uploads_created ON uploads(created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_pastes_expires ON pastes(expires_at)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_expires ON sessions(expires_at)`,
	} {
		if _, err := db.Exec(idx); err != nil {
			db.Close()
			return nil, fmt.Errorf("migrate index: %w", err)
		}
	}
	return &Store{db: db}, nil
}

// IsUniqueViolation reports whether err came from a UNIQUE constraint (e.g. a
// taken slug).
func IsUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

const schema = `
CREATE TABLE IF NOT EXISTS files (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	slug TEXT NOT NULL UNIQUE,
	name TEXT NOT NULL,
	size INTEGER NOT NULL,
	storage TEXT NOT NULL,
	content_type TEXT NOT NULL DEFAULT '',
	password_hash TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL DEFAULT 0,
	downloads INTEGER NOT NULL DEFAULT 0,
	max_downloads INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS sessions (
	token_hash TEXT PRIMARY KEY,
	expires_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS settings (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS uploads (
	id TEXT PRIMARY KEY,
	slug TEXT NOT NULL UNIQUE,
	name TEXT NOT NULL,
	size INTEGER NOT NULL,
	chunk_size INTEGER NOT NULL,
	total_chunks INTEGER NOT NULL,
	storage TEXT NOT NULL,
	provider_id TEXT NOT NULL DEFAULT '',
	password_hash TEXT NOT NULL DEFAULT '',
	expires_in INTEGER NOT NULL DEFAULT 0,
	max_downloads INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS upload_parts (
	upload_id TEXT NOT NULL REFERENCES uploads(id) ON DELETE CASCADE,
	idx INTEGER NOT NULL,
	etag TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (upload_id, idx)
);
CREATE TABLE IF NOT EXISTS pastes (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	slug TEXT NOT NULL UNIQUE,
	content TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL
);
`

func (s *Store) Close() error { return s.db.Close() }

// Ping checks that the database is reachable.
func (s *Store) Ping() error { return s.db.Ping() }

const slugChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

func NewSlug() string { return randomString(8, slugChars) }

// randomString returns n characters drawn from alphabet using crypto/rand.
func randomString(n int, alphabet string) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

// scanAll iterates rows, applying scan to each, and returns the collected
// slice. rows are always closed. scan is one of the scanX helpers, which also
// work over a single *sql.Row.
func scanAll[T any](rows *sql.Rows, scan func(interface{ Scan(...any) error }) (T, error)) ([]T, error) {
	defer rows.Close()
	out := []T{}
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// --- files ---

func (s *Store) CreateFile(f *File) error {
	f.HasPassword = f.PasswordHash != ""
	res, err := s.db.Exec(
		`INSERT INTO files (slug, name, size, storage, content_type, password_hash, created_at, expires_at, downloads, max_downloads)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		f.Slug, f.Name, f.Size, f.Storage, f.ContentType, f.PasswordHash, f.CreatedAt, f.ExpiresAt, f.Downloads, f.MaxDownloads)
	if err != nil {
		return err
	}
	f.ID, err = res.LastInsertId()
	return err
}

// FinalizeUpload inserts the finished file and removes the in-progress upload
// (and its parts) in one transaction so a crash cannot leave only half of the
// metadata behind.
func (s *Store) FinalizeUpload(f *File, uploadID string) error {
	f.HasPassword = f.PasswordHash != ""
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(
		`INSERT INTO files (slug, name, size, storage, content_type, password_hash, created_at, expires_at, downloads, max_downloads)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		f.Slug, f.Name, f.Size, f.Storage, f.ContentType, f.PasswordHash, f.CreatedAt, f.ExpiresAt, f.Downloads, f.MaxDownloads)
	if err != nil {
		return err
	}
	f.ID, err = res.LastInsertId()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM uploads WHERE id = ?`, uploadID); err != nil {
		return err
	}
	return tx.Commit()
}

func scanFile(row interface{ Scan(...any) error }) (*File, error) {
	var f File
	err := row.Scan(&f.ID, &f.Slug, &f.Name, &f.Size, &f.Storage, &f.ContentType, &f.PasswordHash, &f.CreatedAt, &f.ExpiresAt, &f.Downloads, &f.MaxDownloads)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	f.HasPassword = f.PasswordHash != ""
	return &f, nil
}

const fileCols = `id, slug, name, size, storage, content_type, password_hash, created_at, expires_at, downloads, max_downloads`

func (s *Store) GetFileBySlug(slug string) (*File, error) {
	return scanFile(s.db.QueryRow(`SELECT `+fileCols+` FROM files WHERE slug = ?`, slug))
}

func (s *Store) GetFileByID(id int64) (*File, error) {
	return scanFile(s.db.QueryRow(`SELECT `+fileCols+` FROM files WHERE id = ?`, id))
}

func (s *Store) ListFiles() ([]*File, error) {
	rows, err := s.db.Query(`SELECT ` + fileCols + ` FROM files ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	return scanAll(rows, scanFile)
}

func (s *Store) ListExpiredFiles(now int64) ([]*File, error) {
	rows, err := s.db.Query(`SELECT `+fileCols+` FROM files WHERE expires_at > 0 AND expires_at < ?`, now)
	if err != nil {
		return nil, err
	}
	return scanAll(rows, scanFile)
}

// ListExhaustedFiles returns shares that have used up their download quota.
func (s *Store) ListExhaustedFiles() ([]*File, error) {
	rows, err := s.db.Query(`SELECT ` + fileCols + ` FROM files WHERE max_downloads > 0 AND downloads >= max_downloads`)
	if err != nil {
		return nil, err
	}
	return scanAll(rows, scanFile)
}

func (s *Store) UpdateFile(id int64, expiresAt int64, passwordHash string, maxDownloads int64) error {
	_, err := s.db.Exec(`UPDATE files SET expires_at = ?, password_hash = ?, max_downloads = ? WHERE id = ?`,
		expiresAt, passwordHash, maxDownloads, id)
	return err
}

func (s *Store) IncrementDownloads(id int64) error {
	_, err := s.db.Exec(`UPDATE files SET downloads = downloads + 1 WHERE id = ?`, id)
	return err
}

// TryIncrementDownloads atomically consumes one download slot. It returns
// ok=false when the file is missing or has already hit max_downloads (without
// changing the counter). Unlimited files (max_downloads = 0) always succeed
// when the row exists.
func (s *Store) TryIncrementDownloads(id int64) (ok bool, err error) {
	res, err := s.db.Exec(
		`UPDATE files SET downloads = downloads + 1
		 WHERE id = ? AND (max_downloads = 0 OR downloads < max_downloads)`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// KnownObjectSlugs returns every slug that currently has either a finished
// file row or an in-progress upload — used to detect orphaned storage objects.
func (s *Store) KnownObjectSlugs() (map[string]struct{}, error) {
	out := map[string]struct{}{}
	rows, err := s.db.Query(`SELECT slug FROM files UNION SELECT slug FROM uploads`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			return nil, err
		}
		out[slug] = struct{}{}
	}
	return out, rows.Err()
}

type Usage struct {
	Bytes int64 `json:"bytes"`
	Count int64 `json:"count"`
}

// UsageByStorage returns total stored bytes and file counts per backend.
func (s *Store) UsageByStorage() (map[string]Usage, error) {
	rows, err := s.db.Query(`SELECT storage, COALESCE(SUM(size), 0), COUNT(*) FROM files GROUP BY storage`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	usage := map[string]Usage{}
	for rows.Next() {
		var name string
		var u Usage
		if err := rows.Scan(&name, &u.Bytes, &u.Count); err != nil {
			return nil, err
		}
		usage[name] = u
	}
	return usage, rows.Err()
}

// --- sessions (persisted login sessions) ---

func (s *Store) CreateSession(tokenHash string, expiresAt int64) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO sessions (token_hash, expires_at) VALUES (?, ?)`, tokenHash, expiresAt)
	return err
}

// SessionExpiry returns 0 when the session does not exist.
func (s *Store) SessionExpiry(tokenHash string) (int64, error) {
	var exp int64
	err := s.db.QueryRow(`SELECT expires_at FROM sessions WHERE token_hash = ?`, tokenHash).Scan(&exp)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return exp, err
}

func (s *Store) DeleteSession(tokenHash string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE token_hash = ?`, tokenHash)
	return err
}

func (s *Store) DeleteExpiredSessions(now int64) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE expires_at < ?`, now)
	return err
}

// --- settings (runtime configuration saved from the web UI) ---

// SetSettings upserts the given keys; an empty value deletes the key.
func (s *Store) SetSettings(kv map[string]string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for k, v := range kv {
		if v == "" {
			if _, err := tx.Exec(`DELETE FROM settings WHERE key = ?`, k); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.Exec(
			`INSERT INTO settings (key, value) VALUES (?, ?)
			 ON CONFLICT (key) DO UPDATE SET value = excluded.value`, k, v); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) AllSettings() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	kv := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		kv[k] = v
	}
	return kv, rows.Err()
}

func (s *Store) DeleteFile(id int64) error {
	_, err := s.db.Exec(`DELETE FROM files WHERE id = ?`, id)
	return err
}

// --- uploads ---

func (s *Store) CreateUpload(u *Upload) error {
	_, err := s.db.Exec(
		`INSERT INTO uploads (id, slug, name, size, chunk_size, total_chunks, storage, provider_id, password_hash, expires_in, max_downloads, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		u.ID, u.Slug, u.Name, u.Size, u.ChunkSize, u.TotalChunks, u.Storage, u.ProviderID, u.PasswordHash, u.ExpiresIn, u.MaxDownloads, u.CreatedAt)
	return err
}

const uploadCols = `id, slug, name, size, chunk_size, total_chunks, storage, provider_id, password_hash, expires_in, max_downloads, created_at`

func scanUpload(row interface{ Scan(...any) error }) (*Upload, error) {
	var u Upload
	err := row.Scan(&u.ID, &u.Slug, &u.Name, &u.Size, &u.ChunkSize, &u.TotalChunks, &u.Storage, &u.ProviderID, &u.PasswordHash, &u.ExpiresIn, &u.MaxDownloads, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func (s *Store) GetUpload(id string) (*Upload, error) {
	return scanUpload(s.db.QueryRow(`SELECT `+uploadCols+` FROM uploads WHERE id = ?`, id))
}

func (s *Store) ListStaleUploads(olderThan time.Duration) ([]*Upload, error) {
	cutoff := time.Now().Add(-olderThan).Unix()
	rows, err := s.db.Query(`SELECT `+uploadCols+` FROM uploads WHERE created_at < ?`, cutoff)
	if err != nil {
		return nil, err
	}
	return scanAll(rows, scanUpload)
}

func (s *Store) DeleteUpload(id string) error {
	_, err := s.db.Exec(`DELETE FROM uploads WHERE id = ?`, id)
	return err
}

// --- upload parts ---

func (s *Store) PutPart(uploadID string, idx int, etag string) error {
	_, err := s.db.Exec(
		`INSERT INTO upload_parts (upload_id, idx, etag) VALUES (?, ?, ?)
		 ON CONFLICT (upload_id, idx) DO UPDATE SET etag = excluded.etag`,
		uploadID, idx, etag)
	return err
}

func scanPart(row interface{ Scan(...any) error }) (Part, error) {
	var p Part
	err := row.Scan(&p.Idx, &p.ETag)
	return p, err
}

func (s *Store) ListParts(uploadID string) ([]Part, error) {
	rows, err := s.db.Query(`SELECT idx, etag FROM upload_parts WHERE upload_id = ? ORDER BY idx`, uploadID)
	if err != nil {
		return nil, err
	}
	return scanAll(rows, scanPart)
}
