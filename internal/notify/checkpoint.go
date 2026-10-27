package notify

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
)

type checkpoint struct {
	Version int     `json:"version"`
	Binding Binding `json:"binding"`
	Scope   Scope   `json:"scope"`
	After   int64   `json:"after"`
}
type checkpointFile struct {
	path     string
	lock     *os.File
	lockInfo os.FileInfo
}

func regularPrivate(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("state and lock must be regular, nonsymlink files")
	}
	if err = checkPrivate(path, info); err != nil {
		return nil, err
	}
	return info, nil
}
func openCheckpoint(path string, b Binding) (*checkpointFile, checkpoint, error) {
	state := checkpoint{Version: 1, Binding: b}
	if !filepath.IsAbs(path) {
		return nil, state, errors.New("state path must be absolute")
	}
	parent := filepath.Dir(path)
	_, statErr := os.Lstat(parent)
	if os.IsNotExist(statErr) {
		if err := os.MkdirAll(parent, 0700); err != nil {
			return nil, state, err
		}
		if err := makePrivate(parent); err != nil {
			return nil, state, err
		}
	}
	info, err := os.Lstat(parent)
	if err != nil {
		return nil, state, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, state, errors.New("state directory must be private and not a symlink")
	}
	if err = checkPrivate(parent, info); err != nil {
		return nil, state, err
	}
	lock, err := os.OpenFile(path+".lock", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, state, fmt.Errorf("notification state is locked or unsafe; stop the prior watcher, then manually inspect/remove %s.lock if it crashed", path)
	}
	f := &checkpointFile{path: path, lock: lock}
	f.lockInfo, _ = lock.Stat()
	if err = makePrivate(path + ".lock"); err != nil {
		f.close()
		return nil, state, err
	}
	info, err = regularPrivate(path)
	if os.IsNotExist(err) {
		return f, state, nil
	}
	if err != nil {
		f.close()
		return nil, state, err
	}
	file, err := os.Open(path)
	if err != nil {
		f.close()
		return nil, state, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		f.close()
		return nil, state, errors.New("state changed during safe open")
	}
	data, err := io.ReadAll(io.LimitReader(file, 32769))
	if err != nil || len(data) > 32768 {
		f.close()
		return nil, state, errors.New("state exceeds bounded read")
	}
	// Decode persisted state independently of the fresh-state defaults. Missing
	// or null cursor fields must never become zero and replay accepted hints.
	var fields map[string]json.RawMessage
	validFields := json.Unmarshal(data, &fields) == nil && len(fields) == 4
	for _, key := range []string{"version", "binding", "scope", "after"} {
		value := bytes.TrimSpace(fields[key])
		if len(value) == 0 || bytes.Equal(value, []byte("null")) {
			validFields = false
		}
	}
	var persisted checkpoint
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var extra any
	if !validFields || dec.Decode(&persisted) != nil || dec.Decode(&extra) != io.EOF || persisted.Version != 1 || !reflect.DeepEqual(persisted.Binding, b) || !persisted.Scope.valid() || persisted.After < 0 || persisted.After > MaxSequence {
		f.close()
		return nil, state, errors.New("corrupt or mismatched notification state; inspect it rather than retargeting")
	}
	return f, persisted, nil
}
func (f *checkpointFile) close() {
	if f.lock == nil {
		return
	}
	f.lock.Close()
	if info, err := os.Lstat(f.path + ".lock"); err == nil && os.SameFile(info, f.lockInfo) {
		_ = os.Remove(f.path + ".lock")
	}
	f.lock = nil
}
func (f *checkpointFile) save(s checkpoint) error {
	if _, err := regularPrivate(f.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(f.path), ".clawbridge-notify-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err = makePrivate(name); err == nil {
		err = json.NewEncoder(tmp).Encode(s)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if e := tmp.Close(); err == nil {
		err = e
	}
	if err != nil {
		return err
	}
	if _, err = regularPrivate(f.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err = os.Rename(name, f.path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(f.path))
}
