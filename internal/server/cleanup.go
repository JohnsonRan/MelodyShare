package server

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"melodyshare/internal/storage"
	"melodyshare/internal/store"
)

// Unfinished uploads are kept 72h so interrupted transfers can be resumed.
const staleUploadAge = 72 * time.Hour

// Cleanup deletes expired/exhausted files, aborts abandoned uploads, purges
// expired pastes, and removes local storage objects with no DB row.
func (s *Server) Cleanup(ctx context.Context) error {
	var errs []error

	expired, err := s.st.ListExpiredFiles(time.Now().Unix())
	if err != nil {
		errs = append(errs, err)
	}
	for _, f := range expired {
		if err := s.deleteStoredFile(ctx, f, "expired"); err != nil {
			errs = append(errs, err)
		}
	}

	exhausted, err := s.st.ListExhaustedFiles()
	if err != nil {
		errs = append(errs, err)
	}
	for _, f := range exhausted {
		if err := s.deleteStoredFile(ctx, f, "exhausted"); err != nil {
			errs = append(errs, err)
		}
	}

	stale, err := s.st.ListStaleUploads(staleUploadAge)
	if err != nil {
		errs = append(errs, err)
	}
	for _, up := range stale {
		if err := s.storageFor(up.Storage).Abort(ctx, up.Slug, up.ProviderID); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := s.st.DeleteUpload(up.ID); err != nil {
			errs = append(errs, err)
			continue
		}
		slog.Info("cleanup aborted stale upload", "id", up.ID, "name", up.Name)
	}

	if n, err := s.st.DeleteExpiredPastes(time.Now().Unix()); err != nil {
		errs = append(errs, err)
	} else if n > 0 {
		slog.Info("cleanup removed expired pastes", "count", n)
	}

	if err := s.reconcileLocalOrphans(ctx); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

func (s *Server) deleteStoredFile(ctx context.Context, f *store.File, reason string) error {
	if err := s.storageFor(f.Storage).Delete(ctx, f.Slug); err != nil {
		return err
	}
	if err := s.st.DeleteFile(f.ID); err != nil {
		return err
	}
	slog.Info("cleanup removed file", "reason", reason, "slug", f.Slug, "name", f.Name)
	return nil
}

// reconcileLocalOrphans deletes finished local objects that have neither a
// files row nor an in-progress uploads row (e.g. metadata write failed after
// storage.Complete and the best-effort rollback also failed).
func (s *Server) reconcileLocalOrphans(ctx context.Context) error {
	lister, ok := s.local.(storage.ObjectLister)
	if !ok {
		return nil
	}
	keys, err := lister.ListKeys(ctx)
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		return nil
	}
	known, err := s.st.KnownObjectSlugs()
	if err != nil {
		return err
	}
	var errs []error
	for _, key := range keys {
		if _, ok := known[key]; ok {
			continue
		}
		if err := s.local.Delete(ctx, key); err != nil {
			errs = append(errs, err)
			continue
		}
		slog.Info("cleanup removed orphan local object", "key", key)
	}
	return errors.Join(errs...)
}
