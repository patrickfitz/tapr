// Package fake provides a fake tape drive implementation.
package fake // import "hpt.space/tapr/store/tape/drive/fake"

import (
	"hpt.space/tapr/store/tape/drive"
)

func init() {
	drive.Register("fake", New)
}

type impl struct {
}

var _ drive.Drive = (*impl)(nil)

// New returns a new fake tape drive implementation.
func New(opts map[string]interface{}) (drive.Drive, error) {
	const op = "drive/scsi.New"

	return &impl{}, nil
}