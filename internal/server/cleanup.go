package server

import (
	"context"
	"errors"
	"log"
	"time"
)

// Unfinished uploads are kept 72h so interrupted transfers can be resumed.
const staleUploadAge = 72 * time.Hour

// Cleanup deletes expired files and aborts uploads that were never completed.
func (s *Server) Cleanup(ctx context.Context) error {
	var errs []error

	expired, err := s.st.ListExpiredFiles(time.Now().Unix())
	if err != nil {
		errs = append(errs, err)
	}
	for _, f := range expired {
		if err := s.storageFor(f.Storage).Delete(ctx, f.Slug); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := s.st.DeleteFile(f.ID); err != nil {
			errs = append(errs, err)
			continue
		}
		log.Printf("cleanup: removed expired file %s (%s)", f.Slug, f.Name)
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
		log.Printf("cleanup: aborted stale upload %s (%s)", up.ID, up.Name)
	}

	if n, err := s.st.DeleteExpiredPastes(time.Now().Unix()); err != nil {
		errs = append(errs, err)
	} else if n > 0 {
		log.Printf("cleanup: removed %d expired paste(s)", n)
	}

	return errors.Join(errs...)
}
