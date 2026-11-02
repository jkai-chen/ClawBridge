package sessions

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// The per-project lock is a file, <records dir>/.lock, created with O_EXCL
// and holding the holder's PID, start time, time and a random token. It is
// stale only when its file parses and the holder is provably gone (PID and
// start time, C3), or when the file does not parse and is older than
// lockStale. A lock whose holder is alive, or can't be checked, is never
// stale, however old, so a slow holder is never robbed. The next taker removes
// a stale lock, so a crash costs at most the wait.

var (
	lockStale = 30 * time.Second
	// lockStale is the age after which an unparseable lock file is stale.
	// lockWait is how long WithProjectLock waits for a live holder.
	lockWait = 15 * time.Second
	// lockPollEvery is the wait between tries.
	lockPollEvery = 5 * time.Millisecond
)

// ErrLockTimeout is a project lock that stayed held by a live process.
var ErrLockTimeout = errors.New("the project's session records are locked by another process")

// lockTimeout is ErrLockTimeout with the holder's PID, for doctor.
type lockTimeout struct{ pid int }

func (e lockTimeout) Error() string {
	if e.pid > 0 {
		return fmt.Sprintf("%s (held by pid %d)", ErrLockTimeout, e.pid)
	}
	return ErrLockTimeout.Error()
}

func (e lockTimeout) Is(target error) bool { return target == ErrLockTimeout }

type lockHolder struct {
	PID   int    `json:"pid"`
	Start int64  `json:"start"`
	At    int64  `json:"at"` // Unix nanoseconds
	Token string `json:"token"`
}

// WithProjectLock runs fn with the project's lock held. fn must not call
// another function that takes the lock (Claim, Alias, and so on), which
// would wait for itself until the timeout. dir is created 0700.
func WithProjectLock(dir string, fn func() error) (err error) {
	if err := fsys.MkdirPrivate(dir); err != nil {
		return err
	}
	path := filepath.Join(dir, ".lock")
	var tok [16]byte
	if _, err := rand.Read(tok[:]); err != nil {
		return err
	}
	me := lockHolder{Token: hex.EncodeToString(tok[:])}
	self := Self()
	me.PID, me.Start = self.PID, self.Start

	deadline := now().Add(lockWait)
	holderPID := 0
	for {
		me.At = now().UnixNano()
		data, _ := json.Marshal(me)
		cerr := fsys.CreateExclusive(path, data)
		if cerr == nil {
			break
		}
		// On Windows a lock being removed keeps its name for a moment, and
		// then refuses a create with an access error, not "exists".
		if !errors.Is(cerr, os.ErrExist) && !transient(cerr) {
			return cerr
		}
		if pid := breakIfStale(path); pid > 0 {
			holderPID = pid
		}
		if now().After(deadline) {
			return lockTimeout{pid: holderPID}
		}
		time.Sleep(lockPollEvery)
	}
	defer func() {
		if rerr := releaseLock(path, me.Token); rerr != nil {
			err = errors.Join(err, rerr)
		}
	}()
	return fn()
}

// readHolder reads the lock file; ok is false when it can't be parsed (a
// creator may be mid-write), with its age from the file time.
func readHolder(path string) (h lockHolder, raw []byte, ok bool, err error) {
	raw, err = fsys.ReadFile(path, 4<<10)
	if err != nil {
		return lockHolder{}, nil, false, err
	}
	if json.Unmarshal(raw, &h) != nil || h.PID <= 0 {
		return lockHolder{}, raw, false, nil
	}
	return h, raw, true, nil
}

// breakIfStale removes the lock file when it is stale (see the top of this
// file) and returns the holder's PID when it could be read, else 0. The file is first moved aside by an atomic rename, so that two
// breakers can't both remove a lock the other has just made; what was moved
// is checked to be the stale content, and put back if it is not.
func breakIfStale(path string) (pid int) {
	h, raw, ok, err := readHolder(path)
	if err != nil {
		return 0
	}
	stale := false
	if ok {
		pid = h.PID
		stale = gone(ProcID{PID: h.PID, Start: h.Start})
	} else if fi, err := os.Stat(path); err == nil {
		stale = now().Sub(fi.ModTime()) > lockStale
	}
	if !stale {
		return pid
	}
	var n [8]byte
	rand.Read(n[:])
	aside := path + ".stale-" + hex.EncodeToString(n[:])
	if renameRetry(path, aside) != nil {
		return pid
	}
	if moved, err := fsys.ReadFile(aside, 4<<10); err == nil && bytes.Equal(moved, raw) {
		removeRetry(aside)
		return pid
	}
	// We moved a lock that was not the stale one: put it back, or drop our
	// copy when someone else has taken the name meanwhile.
	if os.Link(aside, path) == nil {
		removeRetry(aside)
		return pid
	}
	removeRetry(aside)
	return pid
}

// releaseLock removes the lock only while it still holds our token. A remove
// that keeps failing is returned, never swallowed: a lock left behind would
// block the project until our PID is gone.
func releaseLock(path, token string) error {
	h, _, ok, err := readHolder(path)
	if err != nil || !ok || h.Token != token {
		return nil
	}
	if err := removeRetry(path); err != nil {
		return fmt.Errorf("release the project lock: %w", err)
	}
	return nil
}
