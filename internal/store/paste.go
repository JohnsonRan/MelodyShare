package store

import (
	"crypto/rand"
	"database/sql"
	"errors"
)

// Paste is an anonymous clipboard entry. Unlike files, content lives in the
// database and expiry is mandatory.
type Paste struct {
	ID        int64  `json:"id"`
	Slug      string `json:"slug"`
	Content   string `json:"content"`
	CreatedAt int64  `json:"createdAt"`
	ExpiresAt int64  `json:"expiresAt"` // unix seconds, always > 0
}

// Paste slugs are meant to be read off one screen and typed into another, so
// the alphabet is lowercase-only and drops lookalikes (0/o, 1/l/i).
const pasteSlugChars = "abcdefghjkmnpqrstuvwxyz23456789"

func NewPasteSlug() string {
	b := make([]byte, 5)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	for i := range b {
		b[i] = pasteSlugChars[int(b[i])%len(pasteSlugChars)]
	}
	return string(b)
}

func (s *Store) CreatePaste(p *Paste) error {
	res, err := s.db.Exec(
		`INSERT INTO pastes (slug, content, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		p.Slug, p.Content, p.CreatedAt, p.ExpiresAt)
	if err != nil {
		return err
	}
	p.ID, err = res.LastInsertId()
	return err
}

func scanPaste(row interface{ Scan(...any) error }) (*Paste, error) {
	var p Paste
	err := row.Scan(&p.ID, &p.Slug, &p.Content, &p.CreatedAt, &p.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

const pasteCols = `id, slug, content, created_at, expires_at`

func (s *Store) GetPasteBySlug(slug string) (*Paste, error) {
	return scanPaste(s.db.QueryRow(`SELECT `+pasteCols+` FROM pastes WHERE slug = ?`, slug))
}

func (s *Store) GetPasteByID(id int64) (*Paste, error) {
	return scanPaste(s.db.QueryRow(`SELECT `+pasteCols+` FROM pastes WHERE id = ?`, id))
}

func (s *Store) ListPastes() ([]*Paste, error) {
	rows, err := s.db.Query(`SELECT ` + pasteCols + ` FROM pastes ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	pastes := []*Paste{}
	for rows.Next() {
		p, err := scanPaste(rows)
		if err != nil {
			return nil, err
		}
		pastes = append(pastes, p)
	}
	return pastes, rows.Err()
}

func (s *Store) DeletePaste(id int64) error {
	_, err := s.db.Exec(`DELETE FROM pastes WHERE id = ?`, id)
	return err
}

// DeleteExpiredPastes removes pastes past their expiry and reports how many.
func (s *Store) DeleteExpiredPastes(now int64) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM pastes WHERE expires_at < ?`, now)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
