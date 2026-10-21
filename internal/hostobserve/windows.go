//go:build windows

package hostobserve

import "context"

func Open(context.Context, Binding) (Observer, error) { return nil, Unsupported() }
