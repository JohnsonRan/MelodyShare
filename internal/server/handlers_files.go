package server

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"golang.org/x/crypto/bcrypt"
	"melodyshare/internal/storage"
	"melodyshare/internal/store"
)

func (s *Server) handleFileList(w http.ResponseWriter, r *http.Request) {
	files, err := s.st.ListFiles()
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	type item struct {
		*store.File
		URL string `json:"url"`
	}
	items := make([]item, len(files))
	for i, f := range files {
		items[i] = item{File: f, URL: s.fileURL(r, f.Slug)}
	}
	jsonWrite(w, http.StatusOK, map[string]any{"files": items})
}

func (s *Server) getFile(w http.ResponseWriter, r *http.Request) *store.File {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		jsonError(w, http.StatusBadRequest, "invalid file id")
		return nil
	}
	f, err := s.st.GetFileByID(id)
	if errors.Is(err, store.ErrNotFound) {
		jsonError(w, http.StatusNotFound, "file not found")
		return nil
	}
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return nil
	}
	return f
}

// handleFileUpdate changes expiry and/or password. Omitted (null) fields are
// left unchanged; expiresIn 0 means never expires; password "" removes it.
func (s *Server) handleFileUpdate(w http.ResponseWriter, r *http.Request) {
	f := s.getFile(w, r)
	if f == nil {
		return
	}
	var req struct {
		ExpiresIn    *int64  `json:"expiresIn"`
		Password     *string `json:"password"`
		MaxDownloads *int64  `json:"maxDownloads"` // 0 = unlimited
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.MaxDownloads != nil {
		if *req.MaxDownloads < 0 || *req.MaxDownloads > maxMaxDownloads {
			jsonError(w, http.StatusBadRequest, "invalid maxDownloads")
			return
		}
		f.MaxDownloads = *req.MaxDownloads
	}
	if req.ExpiresIn != nil {
		if *req.ExpiresIn < 0 || *req.ExpiresIn > maxExpiresIn {
			jsonError(w, http.StatusBadRequest, "invalid expiresIn")
			return
		}
		if *req.ExpiresIn == 0 {
			f.ExpiresAt = 0
		} else {
			f.ExpiresAt = time.Now().Unix() + *req.ExpiresIn
		}
	}
	if req.Password != nil {
		if *req.Password == "" {
			f.PasswordHash = ""
		} else {
			h, err := bcrypt.GenerateFromPassword([]byte(*req.Password), bcrypt.DefaultCost)
			if err != nil {
				jsonError(w, http.StatusInternalServerError, err.Error())
				return
			}
			f.PasswordHash = string(h)
		}
	}
	if err := s.st.UpdateFile(f.ID, f.ExpiresAt, f.PasswordHash, f.MaxDownloads); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	f.HasPassword = f.PasswordHash != ""
	jsonWrite(w, http.StatusOK, map[string]any{"file": f})
}

// handleStats reports per-backend usage and local free disk space.
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	usage, err := s.st.UsageByStorage()
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	free := int64(-1)
	if l, ok := s.local.(*storage.Local); ok {
		if f, err := l.FreeSpace(); err == nil {
			free = f
		}
	}
	jsonWrite(w, http.StatusOK, map[string]any{
		"local":     usage["local"],
		"r2":        usage["r2"],
		"localFree": free,
	})
}

func (s *Server) handleFileDelete(w http.ResponseWriter, r *http.Request) {
	f := s.getFile(w, r)
	if f == nil {
		return
	}
	if err := s.storageFor(f.Storage).Delete(r.Context(), f.Slug); err != nil {
		jsonError(w, http.StatusInternalServerError, "delete file: "+err.Error())
		return
	}
	if err := s.st.DeleteFile(f.ID); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonWrite(w, http.StatusOK, map[string]bool{"ok": true})
}
