package update

import (
	"fmt"
	"io"

	"github.com/minio/selfupdate"
)

// Applier swaps the running (or target) binary for the bytes read from r.
type Applier interface {
	Apply(r io.Reader) error
}

// SelfUpdateApplier performs an atomic in-place replacement via minio/selfupdate,
// which handles same-filesystem rename, permission preservation, the Windows
// can't-replace-a-running-exe dance, and rollback on a failed apply. TargetPath
// is empty for the real command (defaults to the running executable); tests set
// it to a throwaway file.
type SelfUpdateApplier struct {
	TargetPath string
}

func (a SelfUpdateApplier) Apply(r io.Reader) error {
	err := selfupdate.Apply(r, selfupdate.Options{TargetPath: a.TargetPath})
	if err != nil {
		if rerr := selfupdate.RollbackError(err); rerr != nil {
			return fmt.Errorf("apply failed and rollback also failed: %v (rollback: %v)", err, rerr)
		}
		return fmt.Errorf("apply update: %w", err)
	}
	return nil
}
