package fde

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// unknownMaxAge caps how long an unknown result is reused, whatever maxAge
// says: it is slow to recompute but the cause is often transient.
const unknownMaxAge = time.Hour

// detect is the detector Cached and Refresh run. Tests replace it so the
// cache is exercised independently of the host platform.
var detect = Detect

func validState(s string) bool {
	return s == StateDetected || s == StateNotDetected || s == StateUnknown
}

// Cached returns the result stored at cachePath when it is younger than
// maxAge (an unknown result: younger than one hour at most). Otherwise it
// runs the detector and stores the outcome.
func Cached(ctx context.Context, dir, cachePath string, maxAge time.Duration) Result {
	if r, ok := readCache(cachePath); ok {
		limit := maxAge
		if r.State == StateUnknown && limit > unknownMaxAge {
			limit = unknownMaxAge
		}
		age := now().Sub(r.At)
		if age >= 0 && age < limit {
			return r
		}
	}
	return Refresh(ctx, dir, cachePath)
}

// Refresh detects again, ignoring any stored result, and stores the outcome.
func Refresh(ctx context.Context, dir, cachePath string) Result {
	r := detect(ctx, dir)
	writeCache(cachePath, r)
	return r
}

func readCache(path string) (Result, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Result{}, false
	}
	var r Result
	if json.Unmarshal(b, &r) != nil || !validState(r.State) || r.At.IsZero() {
		return Result{}, false
	}
	return r, true
}

// writeCache stores r with mode 0600 through a temp file and a rename. A
// failure is ignored: the cache is only an optimisation.
func writeCache(path string, r Result) {
	b, err := json.Marshal(r)
	if err != nil {
		return
	}
	dir := filepath.Dir(path)
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	f, err := os.CreateTemp(dir, ".fde-*.tmp")
	if err != nil {
		return
	}
	tmp := f.Name()
	ok := f.Chmod(0o600) == nil
	if _, err := f.Write(b); err != nil {
		ok = false
	}
	if f.Close() != nil {
		ok = false
	}
	if !ok || os.Rename(tmp, path) != nil {
		os.Remove(tmp)
	}
}
