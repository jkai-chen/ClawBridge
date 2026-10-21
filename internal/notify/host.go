package notify

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

type Runner interface {
	Run(context.Context, string, []string) ([]byte, error)
}
type RunnerFunc func(context.Context, string, []string) ([]byte, error)

func (f RunnerFunc) Run(ctx context.Context, path string, args []string) ([]byte, error) {
	return f(ctx, path, args)
}

type boundedOutput struct {
	mu       sync.Mutex
	data     []byte
	overflow bool
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.data)+len(p) > 16384 {
		b.overflow = true
		return 0, errors.New("host output exceeded limit")
	}
	b.data = append(b.data, p...)
	return len(p), nil
}

type processRunner struct{}

func (processRunner) Run(ctx context.Context, path string, args []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.WaitDelay = 250000000
	var output boundedOutput
	cmd.Stdout = &output
	cmd.Stderr = &output
	err := cmd.Run()
	if err != nil || output.overflow {
		return nil, errors.New("Codex queue command failed or timed out; acceptance may be uncertain")
	}
	return output.data, nil
}
func resolveCodex(path string) (string, error) {
	var err error
	if path == "" {
		path, err = exec.LookPath("codex")
	} else if !filepath.IsAbs(path) {
		err = errors.New("codex command override must be absolute")
	}
	if err != nil {
		return "", Permanent(errors.New("Codex executable unavailable; install a CLI supporting queue"))
	}
	path, err = exec.LookPath(path)
	if err != nil {
		return "", Permanent(errors.New("codex command must be executable"))
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", Permanent(errors.New("cannot resolve Codex executable path"))
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", Permanent(errors.New("codex command must be an executable file"))
	}
	return path, nil
}

// Canonicalize storage identifiers without reading credentials or creating any
// host directories. Missing suffixes are resolved against existing ancestors.
func codexHome() (string, error) {
	path := os.Getenv("CODEX_HOME")
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, ".codex")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	var suffix []string
	for {
		resolved, e := filepath.EvalSymlinks(abs)
		if e == nil {
			info, e := os.Stat(resolved)
			if e != nil || !info.IsDir() {
				return "", errors.New("Codex storage must resolve to a directory")
			}
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return resolved, nil
		}
		if !os.IsNotExist(e) {
			return "", errors.New("cannot resolve Codex storage identifier")
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return "", e
		}
		suffix = append(suffix, filepath.Base(abs))
		abs = parent
	}
}
func supportsQueue(output []byte) bool {
	s := string(output)
	return len(output) <= 16384 && strings.Contains(s, "queue") && strings.Contains(s, "--thread") && strings.Contains(s, "--message")
}
