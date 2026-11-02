package fde

import (
	"context"
	"os"
	"path/filepath"
)

// existingAncestor returns the nearest existing directory at or above p, so
// a state directory that is not created yet can still be located.
func existingAncestor(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	for {
		if _, err := os.Stat(p); err == nil {
			return p
		}
		parent := filepath.Dir(p)
		if parent == p {
			return p
		}
		p = parent
	}
}

func detectPlatform(ctx context.Context, dir string) Result {
	return detectLinuxOrWSL(ctx, existingAncestor(dir))
}
