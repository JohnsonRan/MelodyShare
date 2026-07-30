package server

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"melodyshare/internal/storage"
	"melodyshare/internal/store"
)

const (
	maxExpiresIn    = 10 * 365 * 24 * 3600 // 10 years
	diskFreeMargin  = 1 << 30              // keep at least 1 GiB free on disk
	maxMaxDownloads = 1_000_000_000

	// Chunk-size bounds: S3/R2's multipart floor and Cloudflare's 100 MB
	// request ceiling. The MB values are the single source of truth — settings
	// validation and the byte sizes below both derive from them.
	minChunkMB      = 5
	maxChunkMB      = 95
	minChunkSize    = minChunkMB << 20
	maxChunkSize    = maxChunkMB << 20
	autoTargetParts = 64
)

// autoChunkSize picks a chunk size for the given file size: 5 MiB for small
// files (fast connection turnover, fine-grained retry/resume), growing in
// whole MiB once the file would exceed autoTargetParts chunks, capped at
// maxChunkSize.
func autoChunkSize(size int64) int64 {
	c := (size + autoTargetParts - 1) / autoTargetParts
	c = (c + (1 << 20) - 1) >> 20 << 20
	return min(max(c, minChunkSize), maxChunkSize)
}

var slugPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{3,64}$`)

func (s *Server) handleUploadInit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name         string `json:"name"`
		Size         int64  `json:"size"`
		Storage      string `json:"storage"`
		Slug         string `json:"slug"`      // optional custom share link
		ExpiresIn    int64  `json:"expiresIn"` // seconds, 0 = never
		Password     string `json:"password"`
		MaxDownloads int64  `json:"maxDownloads"` // 0 = unlimited
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Name == "" || len(req.Name) > 512 {
		jsonError(w, http.StatusBadRequest, "invalid file name")
		return
	}
	if req.Size <= 0 {
		jsonError(w, http.StatusBadRequest, "file size must be greater than zero")
		return
	}
	if req.ExpiresIn < 0 || req.ExpiresIn > maxExpiresIn {
		jsonError(w, http.StatusBadRequest, "invalid expiresIn")
		return
	}
	if req.MaxDownloads < 0 || req.MaxDownloads > maxMaxDownloads {
		jsonError(w, http.StatusBadRequest, "invalid maxDownloads")
		return
	}
	if req.Storage == "" {
		req.Storage = "local"
	}
	backend := s.storageFor(req.Storage)
	if backend == nil {
		jsonError(w, http.StatusBadRequest, "unknown storage backend: "+req.Storage)
		return
	}

	slug := req.Slug
	if slug != "" {
		if !slugPattern.MatchString(slug) {
			jsonError(w, http.StatusBadRequest, "自定义链接只能包含字母、数字、-、_，长度 3–64")
			return
		}
		if _, err := s.st.GetFileBySlug(slug); err == nil {
			jsonError(w, http.StatusConflict, "该链接已被占用")
			return
		}
	} else {
		slug = store.NewSlug()
	}

	// Refuse uploads that would (nearly) fill a space-limited backend instead
	// of failing halfway through a chunk write.
	if sc, ok := backend.(storage.SpaceChecker); ok {
		if free, err := sc.FreeSpace(); err == nil && free < req.Size+diskFreeMargin {
			jsonError(w, http.StatusInsufficientStorage,
				fmt.Sprintf("磁盘空间不足：剩余 %.1f GB，需要 %.1f GB", float64(free)/(1<<30), float64(req.Size+diskFreeMargin)/(1<<30)))
			return
		}
	}

	passwordHash, err := hashPassword(req.Password)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}

	chunkSize := s.rt.ChunkSize()
	if chunkSize == 0 { // auto mode: derive from the file size
		chunkSize = autoChunkSize(req.Size)
	}
	up := &store.Upload{
		ID:           newUploadID(),
		Slug:         slug,
		Name:         req.Name,
		Size:         req.Size,
		ChunkSize:    chunkSize,
		TotalChunks:  int((req.Size + chunkSize - 1) / chunkSize),
		Storage:      req.Storage,
		PasswordHash: passwordHash,
		ExpiresIn:    req.ExpiresIn,
		MaxDownloads: req.MaxDownloads,
		CreatedAt:    time.Now().Unix(),
	}
	providerID, err := backend.Init(r.Context(), up.Slug, up.Size)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "init upload: "+err.Error())
		return
	}
	up.ProviderID = providerID
	if err := s.st.CreateUpload(up); err != nil {
		backend.Abort(r.Context(), up.Slug, providerID)
		if store.IsUniqueViolation(err) {
			jsonError(w, http.StatusConflict, "该链接已被占用（或有进行中的上传使用它）")
			return
		}
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonWrite(w, http.StatusOK, map[string]any{
		"id":          up.ID,
		"slug":        up.Slug,
		"chunkSize":   up.ChunkSize,
		"totalChunks": up.TotalChunks,
	})
}

func (s *Server) getUpload(w http.ResponseWriter, r *http.Request) *store.Upload {
	up, err := s.st.GetUpload(r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		jsonError(w, http.StatusNotFound, "upload not found")
		return nil
	}
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return nil
	}
	return up
}

func (s *Server) handleUploadStatus(w http.ResponseWriter, r *http.Request) {
	up := s.getUpload(w, r)
	if up == nil {
		return
	}
	parts, err := s.st.ListParts(up.ID)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	received := make([]int, len(parts))
	for i, p := range parts {
		received[i] = p.Idx
	}
	jsonWrite(w, http.StatusOK, map[string]any{
		"id": up.ID, "name": up.Name, "size": up.Size, "storage": up.Storage,
		"totalChunks": up.TotalChunks, "chunkSize": up.ChunkSize, "received": received,
	})
}

// chunkIdx validates the {idx} path segment against the upload.
func chunkIdx(w http.ResponseWriter, r *http.Request, up *store.Upload) (int, bool) {
	idx, err := strconv.Atoi(r.PathValue("idx"))
	if err != nil || idx < 0 || idx >= up.TotalChunks {
		jsonError(w, http.StatusBadRequest, "chunk index out of range")
		return 0, false
	}
	return idx, true
}

// handleChunkURL hands out a presigned URL so the browser can upload the
// chunk directly to the backing store (R2), bypassing this server.
func (s *Server) handleChunkURL(w http.ResponseWriter, r *http.Request) {
	up := s.getUpload(w, r)
	if up == nil {
		return
	}
	p, ok := s.storageFor(up.Storage).(storage.Presigner)
	if !ok {
		jsonError(w, http.StatusBadRequest, "storage backend does not support direct upload")
		return
	}
	idx, ok := chunkIdx(w, r, up)
	if !ok {
		return
	}
	u, err := p.PresignPart(r.Context(), up.Slug, up.ProviderID, idx)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "presign: "+err.Error())
		return
	}
	jsonWrite(w, http.StatusOK, map[string]string{"url": u})
}

// handleChunkETag records the etag of a chunk the browser uploaded directly.
func (s *Server) handleChunkETag(w http.ResponseWriter, r *http.Request) {
	up := s.getUpload(w, r)
	if up == nil {
		return
	}
	if _, ok := s.storageFor(up.Storage).(storage.Presigner); !ok {
		jsonError(w, http.StatusBadRequest, "storage backend does not support direct upload")
		return
	}
	idx, ok := chunkIdx(w, r, up)
	if !ok {
		return
	}
	var req struct {
		ETag string `json:"etag"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	etag := strings.Trim(req.ETag, `"`)
	if etag == "" {
		jsonError(w, http.StatusBadRequest, "etag is required")
		return
	}
	if err := s.st.PutPart(up.ID, idx, etag); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w)
}

func (s *Server) handleUploadChunk(w http.ResponseWriter, r *http.Request) {
	up := s.getUpload(w, r)
	if up == nil {
		return
	}
	idx, ok := chunkIdx(w, r, up)
	if !ok {
		return
	}
	want := up.ChunkSize
	if idx == up.TotalChunks-1 {
		want = up.Size - int64(up.TotalChunks-1)*up.ChunkSize
	}
	if r.ContentLength != want {
		jsonError(w, http.StatusBadRequest,
			fmt.Sprintf("chunk %d must be exactly %d bytes, got Content-Length %d", idx, want, r.ContentLength))
		return
	}
	body := http.MaxBytesReader(w, r.Body, want)
	etag, err := s.storageFor(up.Storage).PutChunk(r.Context(), up.Slug, up.ProviderID, idx, up.ChunkSize, body, want)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "store chunk: "+err.Error())
		return
	}
	if err := s.st.PutPart(up.ID, idx, etag); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w)
}

func (s *Server) handleUploadComplete(w http.ResponseWriter, r *http.Request) {
	up := s.getUpload(w, r)
	if up == nil {
		return
	}
	parts, err := s.st.ListParts(up.ID)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(parts) != up.TotalChunks {
		jsonError(w, http.StatusBadRequest,
			fmt.Sprintf("upload incomplete: %d of %d chunks received", len(parts), up.TotalChunks))
		return
	}
	etags := make([]string, len(parts))
	for i, p := range parts {
		if p.Idx != i {
			jsonError(w, http.StatusBadRequest, fmt.Sprintf("missing chunk %d", i))
			return
		}
		etags[i] = p.ETag
	}
	backend := s.storageFor(up.Storage)
	if err := backend.Complete(r.Context(), up.Slug, up.ProviderID, etags); err != nil {
		jsonError(w, http.StatusInternalServerError, "complete upload: "+err.Error())
		return
	}

	contentType := mime.TypeByExtension(filepath.Ext(up.Name))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	now := time.Now().Unix()
	f := &store.File{
		Slug:         up.Slug,
		Name:         up.Name,
		Size:         up.Size,
		Storage:      up.Storage,
		ContentType:  contentType,
		PasswordHash: up.PasswordHash,
		MaxDownloads: up.MaxDownloads,
		CreatedAt:    now,
	}
	if up.ExpiresIn > 0 {
		f.ExpiresAt = now + up.ExpiresIn
	}
	// Create the file row and drop the upload row together. If metadata write
	// fails after storage.Complete, best-effort delete the object so it does
	// not become an orphan (except on slug conflict — another file owns the key).
	if err := s.st.FinalizeUpload(f, up.ID); err != nil {
		if store.IsUniqueViolation(err) {
			jsonError(w, http.StatusConflict, "该链接在上传期间被其他文件占用了")
			return
		}
		if delErr := backend.Delete(r.Context(), up.Slug); delErr != nil {
			slog.Error("orphan after failed finalize", "slug", up.Slug, "err", delErr)
		}
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonWrite(w, http.StatusOK, map[string]any{"file": f, "url": s.fileURL(r, f.Slug)})
}

func (s *Server) handleUploadAbort(w http.ResponseWriter, r *http.Request) {
	up := s.getUpload(w, r)
	if up == nil {
		return
	}
	if err := s.storageFor(up.Storage).Abort(r.Context(), up.Slug, up.ProviderID); err != nil {
		slog.Warn("abort upload", "id", up.ID, "err", err)
	}
	if err := s.st.DeleteUpload(up.ID); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w)
}

func newUploadID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
