package youtube

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/HolyArrs/holyarrs/app/backend/internal/core/jobs"
)

// ForgetPerson removes everything the YouTube library keeps for a deleted
// person: their waiting and running jobs are stopped, their follow and
// video rows go, and so does their "<id>-<name>" folder in the YouTube
// folder. Only entries directly inside the YouTube folder are removed; a
// link there is removed as a link, never followed. It carries on past a
// failed step and returns every failure.
func (s *Service) ForgetPerson(ctx context.Context, userID int64) error {
	var errs []error
	ids, err := jobs.LiveIDsWithKeyPrefix(ctx, s.Store.Read(), fmt.Sprintf("youtube:%d:", userID))
	if err != nil {
		errs = append(errs, fmt.Errorf("youtube: jobs of user %d: %w", userID, err))
	}
	for _, id := range ids {
		if err := s.Queue.Cancel(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("youtube: cancel job %d: %w", id, err))
		}
	}
	if err := s.Store.Write(ctx, func(tx *sql.Tx) error { return DeletePersonRows(tx, userID) }); err != nil {
		errs = append(errs, fmt.Errorf("youtube: rows of user %d: %w", userID, err))
	}
	if err := removePersonFolder(LoadSettings(ctx, s.Store.Read()).Folder, userID); err != nil {
		errs = append(errs, fmt.Errorf("youtube: folder of user %d: %w", userID, err))
	}
	return errors.Join(errs...)
}

// removePersonFolder removes every "<id>-" folder or link directly inside
// root. Nothing happens when root is not set or not there.
func removePersonFolder(root string, userID int64) error {
	if root == "" {
		return nil
	}
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	prefix := fmt.Sprintf("%d-", userID)
	var errs []error
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), prefix) || !(e.IsDir() || e.Type()&os.ModeSymlink != 0) {
			continue
		}
		// RemoveAll unlinks a link without following it, and never
		// follows links found inside the folder.
		if err := os.RemoveAll(filepath.Join(root, e.Name())); err != nil {
			errs = append(errs, withoutPath(err))
		}
	}
	return errors.Join(errs...)
}

// withoutPath drops the file path from err: paths under a person's folder
// carry video titles, which never go in a log line.
func withoutPath(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return fmt.Errorf("%s: %w", pe.Op, pe.Err)
	}
	return err
}
