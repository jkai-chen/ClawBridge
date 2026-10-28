package ctxwatch

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// QueueRecord is what an agent's MCP server tells its hooks about the
// message queue (MESSAGE-QUEUE-DESIGN §5), at <context dir>/<session>.queue.json.
// The hooks run as their own processes: the Stop hook asks the server for the
// session's unacked count, and `context compacting` gives the held leases
// back. Both need the instance and the leases only the MCP server knows.
// It holds IDs and routing, never a credential or a message, and is private
// to the user.
type QueueRecord struct {
	Version   int    `json:"version"`
	Session   string `json:"session"`
	Workspace string `json:"workspace"`
	Project   string `json:"project"`
	Instance  string `json:"instance"`
	// Server is the hosted server's URL, or DB the local database.
	Server string `json:"server,omitempty"`
	DB     string `json:"db,omitempty"`
	// MCPPID is the MCP server's process, AgentPID its parent: the agent
	// whose hooks may act on the record.
	MCPPID   int `json:"mcp_pid"`
	AgentPID int `json:"agent_pid"`
	// Stream is how the MCP server takes its deliveries: stream, polling,
	// off (no queue on the server), or ended.
	Stream    string       `json:"stream"`
	Leases    []QueueLease `json:"leases"`
	UpdatedAt string       `json:"updated_at"`
}

// QueueLease is one delivery the MCP server holds.
type QueueLease struct {
	MessageID string `json:"message_id"`
	LeaseID   string `json:"lease_id"`
}

// QueueRecordFresh is how old a record may be and still be acted on: the
// MCP server touches it every minute.
const QueueRecordFresh = 3 * time.Minute

func sidePath(dir, session, suffix string) (string, error) {
	path, err := BindingPath(dir, session)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(path, ".json") + suffix, nil
}

// SaveQueueRecord writes r for its session.
func SaveQueueRecord(dir string, r QueueRecord) error {
	path, err := sidePath(dir, r.Session, ".queue.json")
	if err != nil {
		return err
	}
	r.Version = 1
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return writePrivate(dir, path, data)
}

// RemoveQueueRecord removes session's record when it is still the one
// process mcpPID wrote.
func RemoveQueueRecord(dir, session string, mcpPID int) {
	if r, ok, err := LoadQueueRecord(dir, session); err == nil && ok && r.MCPPID == mcpPID {
		if path, err := sidePath(dir, session, ".queue.json"); err == nil {
			_ = os.Remove(path)
		}
	}
}

// LoadQueueRecord reads session's record. A missing one is not an error.
func LoadQueueRecord(dir, session string) (QueueRecord, bool, error) {
	path, err := sidePath(dir, session, ".queue.json")
	if err != nil {
		return QueueRecord{}, false, err
	}
	data, ok, err := readPrivate(path)
	if err != nil || !ok {
		return QueueRecord{}, false, err
	}
	var r QueueRecord
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if dec.Decode(&r) != nil || r.Version != 1 || r.Session != session || r.Instance == "" || (r.Server == "") == (r.DB == "") {
		return QueueRecord{}, false, fmt.Errorf("the queue record %s is not valid", path)
	}
	return r, true, nil
}

// HookQueueRecord finds the record a hook of the agent process agentPID may
// act on: session's, or with no session the one whose agent it is. It must
// be fresh, and its MCP server alive and still the agent's child, so a
// reused process ID never acts on another agent's queue.
func HookQueueRecord(dir, session string, agentPID int, now time.Time) (QueueRecord, bool) {
	if agentPID <= 0 {
		return QueueRecord{}, false
	}
	var candidates []string
	if session != "" {
		candidates = []string{session}
	} else {
		matches, _ := filepath.Glob(filepath.Join(dir, "*.queue.json"))
		for _, m := range matches {
			candidates = append(candidates, strings.TrimSuffix(filepath.Base(m), ".queue.json"))
		}
	}
	for _, s := range candidates {
		r, ok, err := LoadQueueRecord(dir, s)
		if err != nil || !ok || r.AgentPID != agentPID {
			continue
		}
		at, err := time.Parse(time.RFC3339, r.UpdatedAt)
		if err != nil || now.Sub(at) > QueueRecordFresh || at.Sub(now) > time.Minute {
			continue
		}
		if parent, ok := ParentPID(r.MCPPID); !ok || parent != agentPID {
			continue
		}
		return r, true
	}
	return QueueRecord{}, false
}

// CompactionHold is the mark `context compacting` leaves when it holds the
// session's notifications for a compaction: the instance it held and when.
// Only a hold with this mark is ever set back to accept, so a user's own
// hold or refuse is never changed.
type CompactionHold struct {
	Instance string `json:"instance"`
	HeldAt   string `json:"held_at"`
}

// CompactionHoldLimit is how long a compaction's hold may last before it is
// set back to accept: a session that died mid-compaction mustn't hold its
// mail for ever.
const CompactionHoldLimit = 15 * time.Minute

// SaveCompactionHold marks session as held for a compaction.
func SaveCompactionHold(dir, session string, h CompactionHold) error {
	path, err := sidePath(dir, session, ".compaction-hold.json")
	if err != nil {
		return err
	}
	data, err := json.Marshal(h)
	if err != nil {
		return err
	}
	return writePrivate(dir, path, data)
}

// LoadCompactionHold reads session's mark, if any.
func LoadCompactionHold(dir, session string) (CompactionHold, bool) {
	path, err := sidePath(dir, session, ".compaction-hold.json")
	if err != nil {
		return CompactionHold{}, false
	}
	data, ok, err := readPrivate(path)
	if err != nil || !ok {
		return CompactionHold{}, false
	}
	var h CompactionHold
	if json.Unmarshal(data, &h) != nil || h.Instance == "" {
		return CompactionHold{}, false
	}
	return h, true
}

// RemoveCompactionHold removes session's mark.
func RemoveCompactionHold(dir, session string) {
	if path, err := sidePath(dir, session, ".compaction-hold.json"); err == nil {
		_ = os.Remove(path)
	}
}

// StopMark is the newest message the Stop hook already blocked a stop for,
// so it blocks at most once per new message and never loops an agent on a
// message it can't handle.
type StopMark struct {
	Instance  string `json:"instance"`
	LatestSeq int64  `json:"latest_seq"`
}

// LoadStopMark reads session's mark, if any.
func LoadStopMark(dir, session string) (StopMark, bool) {
	path, err := sidePath(dir, session, ".stop.json")
	if err != nil {
		return StopMark{}, false
	}
	data, ok, err := readPrivate(path)
	if err != nil || !ok {
		return StopMark{}, false
	}
	var m StopMark
	if json.Unmarshal(data, &m) != nil {
		return StopMark{}, false
	}
	return m, true
}

// SaveStopMark writes session's mark.
func SaveStopMark(dir, session string, m StopMark) error {
	path, err := sidePath(dir, session, ".stop.json")
	if err != nil {
		return err
	}
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return writePrivate(dir, path, data)
}

// afterStat runs between readPrivate's stat and its open; tests remove or
// replace the file there.
var afterStat = func() {}

// readPrivate reads a small record: a regular file, not a link, owned by
// this user and readable by nobody else.
func readPrivate(path string) ([]byte, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil || !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("%s is not a regular file", path)
	}
	if err := privateToUser(info); err != nil {
		return nil, false, fmt.Errorf("%s %s", path, err)
	}
	afterStat()
	f, err := openNoFollow(path)
	if errors.Is(err, os.ErrNotExist) {
		// Removed since the stat: the MCP server exited.
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("%s is not readable", path)
	}
	defer f.Close()
	// A rename since the stat can put another file there; check the one opened.
	if info, err = f.Stat(); err != nil || !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("%s is not a regular file", path)
	}
	if err := privateToUser(info); err != nil {
		return nil, false, fmt.Errorf("%s %s", path, err)
	}
	data, err := io.ReadAll(io.LimitReader(f, 256<<10))
	if err != nil {
		return nil, false, fmt.Errorf("%s is not readable", path)
	}
	return data, true, nil
}

// writePrivate replaces path with data, 0600, through a rename.
func writePrivate(dir, path string, data []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("the context directory %s could not be created", dir)
	}
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	tmp, err := os.CreateTemp(dir, ".record-*")
	if err != nil {
		return fmt.Errorf("%s could not be written", path)
	}
	name := tmp.Name()
	defer os.Remove(name)
	err = tmp.Chmod(0o600)
	if err == nil {
		_, err = tmp.Write(append(data, '\n'))
	}
	if e := tmp.Close(); err == nil {
		err = e
	}
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		return fmt.Errorf("%s could not be written", path)
	}
	return nil
}
