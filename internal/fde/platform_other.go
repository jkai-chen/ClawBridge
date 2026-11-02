//go:build !darwin && !linux && !windows

package fde

import "context"

func detectPlatform(ctx context.Context, dir string) Result {
	return unknown("none", "full-disk encryption detection isn't supported on this OS")
}
