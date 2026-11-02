package fde

import "context"

func detectPlatform(ctx context.Context, dir string) Result {
	return windowsPolicy(ctx)
}
