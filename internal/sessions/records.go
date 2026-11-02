package sessions

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	recordVersion  = 1
	maxRecordBytes = 64 << 10
)

// Record is one named session in a project, stored in its home's state
// directory. The instance lives here and nowhere else.
type Record struct {
	Name          string   `json:"name"`
	Workspace     string   `json:"workspace,omitempty"`
	Project       string   `json:"project,omitempty"`
	Instance      string   `json:"instance,omitempty"`
	Conversations []string `json:"conversations,omitempty"`
	CWD           string   `json:"cwd,omitempty"`
	AgentKind     string   `json:"agent_kind,omitempty"`
	Channel       bool     `json:"channel,omitempty"`
	Launch        []string `json:"launch,omitempty"`
	CodexThread   string   `json:"codex_thread,omitempty"`
	// Owner is the live agent process, or nil.
	Owner   *ProcID   `json:"owner,omitempty"`
	Home    string    `json:"home,omitempty"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
	// EnvIDDiag is the environment's session ID as the agent saw it. It is
	// diagnostic only and never used to bind (D1).
	EnvIDDiag string `json:"env_id_diag,omitempty"`
	// Notifier is the notifier's state, owned by a later phase.
	Notifier json.RawMessage `json:"notifier,omitempty"`
}

type recordFile struct {
	V int `json:"v"`
	Record
}

// Hint is what a no-link claim can show for the record it names (F6, F10).
type Hint struct {
	// AgentStartName is the name `agent start NAME` gave this process.
	AgentStartName string
	// CodexThread is the Codex thread ID of the current process.
	CodexThread string
}

// Errors of the record operations.
var (
	ErrConversationTaken = errors.New("another session already has this conversation")
	ErrBelongsToAnother  = errors.New("the session belongs to another conversation")
	ErrNotFound          = errors.New("no such session")
	ErrNotOwner          = errors.New("this agent does not own the session")
	ErrInstanceSet       = errors.New("the session already has a different instance")
	ErrCorrupt           = errors.New("unreadable session record")
	// ErrNameCaseCollision is a new name that differs from an existing
	// session's only in case. It is refused on every OS, so that behaviour
	// doesn't depend on whether the filesystem folds case.
	ErrNameCaseCollision = errors.New("a session whose name differs only in case already exists")
)

// ErrInUse is a session whose owner is another live agent process.
type ErrInUse struct{ PID int }

func (e ErrInUse) Error() string {
	return fmt.Sprintf("session is in use by another running agent (pid %d)", e.PID)
}

func recordPath(dir, name string) string { return filepath.Join(dir, name+".json") }

func readRecord(dir, name string) (Record, error) {
	if err := checkSegment("session name", name); err != nil {
		return Record{}, err
	}
	path := recordPath(dir, name)
	data, err := fsys.ReadFile(path, maxRecordBytes)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Record{}, ErrNotFound
		}
		return Record{}, fmt.Errorf("%w: %s: %v", ErrCorrupt, filepath.Base(path), err)
	}
	var f recordFile
	if err := decodeStrict(data, &f); err != nil || f.V != recordVersion || f.Name != name {
		if err == nil {
			err = errors.New("wrong version or name")
		}
		return Record{}, fmt.Errorf("%w: %s: %v", ErrCorrupt, filepath.Base(path), err)
	}
	for _, c := range f.Conversations {
		if checkConv(c) != nil {
			return Record{}, fmt.Errorf("%w: %s: bad conversation id", ErrCorrupt, filepath.Base(path))
		}
	}
	return f.Record, nil
}

// writeRecord stores r, bumping Updated.
func writeRecord(dir string, r Record) error {
	if err := checkSegment("session name", r.Name); err != nil {
		return err
	}
	r.Updated = now().UTC()
	data, err := json.Marshal(recordFile{V: recordVersion, Record: r})
	if err != nil {
		return err
	}
	if len(data) > maxRecordBytes {
		return ErrTooLarge
	}
	return fsys.WriteFile(recordPath(dir, r.Name), data)
}

// listRecords reads every record in dir. A record that can't be read stops
// the listing: the conversation checks must not pass over a file they can't
// see into.
func listRecords(dir string) ([]Record, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []Record
	for _, e := range entries {
		n, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || e.IsDir() || strings.HasPrefix(n, ".") {
			continue
		}
		r, err := readRecord(dir, n)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b Record) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// List is the project's records, sorted by name.
func List(dir string) ([]Record, error) { return listRecords(dir) }

// Get is one record.
func Get(dir, name string) (Record, error) { return readRecord(dir, name) }

// FindByConversation is the record listing conv.
func FindByConversation(dir, conv string) (Record, bool) {
	conv, err := normConv(conv)
	if err != nil {
		return Record{}, false
	}
	recs, err := listRecords(dir)
	if err != nil {
		return Record{}, false
	}
	for _, r := range recs {
		if slices.Contains(r.Conversations, conv) {
			return r, true
		}
	}
	return Record{}, false
}

// Delete removes the record (`agent remove`).
func Delete(dir, name string) error {
	if err := checkSegment("session name", name); err != nil {
		return err
	}
	return WithProjectLock(dir, func() error {
		if err := os.Remove(recordPath(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	})
}

// takenElsewhere reports whether a record other than name lists conv (F8).
func takenElsewhere(recs []Record, name, conv string) bool {
	for _, r := range recs {
		if r.Name != name && slices.Contains(r.Conversations, conv) {
			return true
		}
	}
	return false
}

// liveOther is the PID of an owner that is not agent and still runs.
func liveOther(r Record, agent ProcID) (int, bool) {
	// Only a provably gone owner is dead: a failed lookup counts as busy.
	if r.Owner != nil && *r.Owner != agent && !gone(*r.Owner) {
		return r.Owner.PID, true
	}
	return 0, false
}

func checkAgent(agent ProcID) error {
	if agent.PID <= 0 {
		return errors.New("the agent process is not identified")
	}
	return nil
}

// Claim binds the session name to agent. The record is created, or an
// existing one is adopted when it has no live owner other than agent. All
// under the project lock, so of concurrent claims one wins and the rest get
// ErrInUse.
//
// conv, when not empty, is added to the record's conversations; it is refused
// with ErrConversationTaken if another record lists it (F8). instance is
// stored when the record is new or has none; a stored instance is never
// replaced.
//
// For an existing record whose owner is not a live other agent: a conv it
// already lists is a resume; a record with no conversations is adoptable; a
// record with other conversations is ErrBelongsToAnother unless
// hint.AgentStartName is name (the person ran `agent start NAME`). The
// caller words that refusal: resume that conversation, or run `clawbridge
// agent takeover NAME`. A live owner is checked first, and only a provably
// gone one counts as dead.
func Claim(dir, name, conv string, agent ProcID, instance string, hint Hint) (Record, error) {
	if err := checkSegment("session name", name); err != nil {
		return Record{}, err
	}
	if err := checkAgent(agent); err != nil {
		return Record{}, err
	}
	if conv != "" {
		var err error
		if conv, err = normConv(conv); err != nil {
			return Record{}, err
		}
	}
	var out Record
	err := WithProjectLock(dir, func() error {
		recs, err := listRecords(dir)
		if err != nil {
			return err
		}
		r, found := findByName(recs, name)
		if found {
			if pid, busy := liveOther(r, agent); busy {
				return ErrInUse{PID: pid}
			}
		}
		// A claim with no conversation is the no-link path: it may not take a
		// record that belongs to conversations (ClaimByNameNoLink decides).
		if found && conv == "" && len(r.Conversations) > 0 {
			return ErrBelongsToAnother
		}
		if conv != "" && takenElsewhere(recs, name, conv) {
			return ErrConversationTaken
		}
		// A new conversation may not take a session that belongs to others by
		// guessing its name; only `agent start NAME` (the hint) may.
		if found && conv != "" && len(r.Conversations) > 0 && !slices.Contains(r.Conversations, conv) &&
			hint.AgentStartName != name {
			return ErrBelongsToAnother
		}
		if !found {
			if caseCollision(recs, name) {
				return ErrNameCaseCollision
			}
			ws, proj := dirNames(dir)
			t := now().UTC()
			r = Record{Name: name, Workspace: ws, Project: proj, Created: t}
		}
		if r.Instance == "" {
			r.Instance = instance
		}
		if conv != "" && !slices.Contains(r.Conversations, conv) {
			r.Conversations = append(r.Conversations, conv)
		}
		owner := agent
		r.Owner = &owner
		if err := writeRecord(dir, r); err != nil {
			return err
		}
		out, err = readRecord(dir, name)
		return err
	})
	return out, err
}

// caseCollision reports whether a record's name equals name when case is
// ignored but not byte for byte.
func caseCollision(recs []Record, name string) bool {
	for _, r := range recs {
		if r.Name != name && strings.EqualFold(r.Name, name) {
			return true
		}
	}
	return false
}

func findByName(recs []Record, name string) (Record, bool) {
	for _, r := range recs {
		if r.Name == name {
			return r, true
		}
	}
	return Record{}, false
}

// dirNames is the workspace and project of a records directory
// (<state>/sessions/<workspace>/<project>), or "" for a part that isn't a
// valid name.
func dirNames(dir string) (workspace, project string) {
	project = filepath.Base(dir)
	workspace = filepath.Base(filepath.Dir(dir))
	if checkSegment("", project) != nil {
		project = ""
	}
	if checkSegment("", workspace) != nil {
		workspace = ""
	}
	return workspace, project
}

// Alias adds newConv to the session's conversations. It is for `/clear` (F5,
// which the caller must have decided from the link's source) and is allowed
// only to the live owner, and only when no other record lists newConv (F8).
func Alias(dir, name, newConv string, agent ProcID) error {
	newConv, err := normConv(newConv)
	if err != nil {
		return err
	}
	return WithProjectLock(dir, func() error {
		recs, err := listRecords(dir)
		if err != nil {
			return err
		}
		r, found := findByName(recs, name)
		if !found {
			return ErrNotFound
		}
		if r.Owner == nil || *r.Owner != agent || gone(agent) {
			return ErrNotOwner
		}
		if takenElsewhere(recs, name, newConv) {
			return ErrConversationTaken
		}
		if slices.Contains(r.Conversations, newConv) {
			return nil
		}
		r.Conversations = append(r.Conversations, newConv)
		return writeRecord(dir, r)
	})
}

// Takeover clears the session's owner, whoever it is and whether or not it
// runs, so that the next claim succeeds. This package does not check that a
// person asked: refusing under an agent environment and requiring a TTY is
// `agent takeover`'s job (design §2.4). by is the process that took over and
// only has to be identified.
func Takeover(dir, name string, by ProcID) error {
	if err := checkAgent(by); err != nil {
		return err
	}
	return WithProjectLock(dir, func() error {
		r, err := readRecord(dir, name)
		if err != nil {
			return err
		}
		r.Owner = nil
		return writeRecord(dir, r)
	})
}

// Release clears the owner if it is agent, as when the agent ends; another
// owner is left alone.
func Release(dir, name string, agent ProcID) error {
	return WithProjectLock(dir, func() error {
		r, err := readRecord(dir, name)
		if err != nil {
			return err
		}
		if r.Owner == nil || *r.Owner != agent {
			return nil
		}
		r.Owner = nil
		return writeRecord(dir, r)
	})
}

// ClaimByNameNoLink is the claim when there is no agent link and so no
// conversation ID (F6, F10): the record is bound to the agent's PID only.
// A record that does not exist is created. An existing one may be taken only
// when its owner is gone or none, and either it has no conversations (it was
// itself made without a link), or hint shows the claimer is the right one:
// the process was started by `agent start NAME`, or its Codex thread is the
// one the record stores. Otherwise ErrBelongsToAnother. A live owner other
// than agent is ErrInUse.
func ClaimByNameNoLink(dir, name string, agent ProcID, hint Hint) (Record, error) {
	if err := checkSegment("session name", name); err != nil {
		return Record{}, err
	}
	if err := checkAgent(agent); err != nil {
		return Record{}, err
	}
	var out Record
	err := WithProjectLock(dir, func() error {
		recs, err := listRecords(dir)
		if err != nil {
			return err
		}
		r, found := findByName(recs, name)
		switch {
		case !found:
			if caseCollision(recs, name) {
				return ErrNameCaseCollision
			}
			ws, proj := dirNames(dir)
			r = Record{Name: name, Workspace: ws, Project: proj, Created: now().UTC()}
		default:
			if pid, busy := liveOther(r, agent); busy {
				return ErrInUse{PID: pid}
			}
			if len(r.Conversations) > 0 && !hint.matches(r) {
				return ErrBelongsToAnother
			}
		}
		owner := agent
		r.Owner = &owner
		if err := writeRecord(dir, r); err != nil {
			return err
		}
		out, err = readRecord(dir, name)
		return err
	})
	return out, err
}

func (h Hint) matches(r Record) bool {
	if h.AgentStartName != "" && h.AgentStartName == r.Name {
		return true
	}
	return h.CodexThread != "" && h.CodexThread == r.CodexThread
}

// Update changes the fields of a record that Claim and the others don't
// manage (CWD, Launch, Home, Channel, CodexThread, Notifier, Instance when
// empty and so on) under the project lock. Name, Workspace, Project, Created
// and Owner are restored after fn; a changed Conversations list is checked
// against F8; a stored Instance can't be replaced.
func Update(dir, name string, fn func(*Record)) (Record, error) {
	var out Record
	err := WithProjectLock(dir, func() error {
		recs, err := listRecords(dir)
		if err != nil {
			return err
		}
		orig, found := findByName(recs, name)
		if !found {
			return ErrNotFound
		}
		r := orig
		r.Conversations = slices.Clone(orig.Conversations)
		fn(&r)
		r.Name, r.Workspace, r.Project, r.Created, r.Owner = orig.Name, orig.Workspace, orig.Project, orig.Created, orig.Owner
		if orig.Instance != "" && r.Instance != orig.Instance {
			return ErrInstanceSet
		}
		for i, c := range r.Conversations {
			c, err := normConv(c)
			if err != nil {
				return err
			}
			r.Conversations[i] = c
			if takenElsewhere(recs, name, c) {
				return ErrConversationTaken
			}
		}
		if err := writeRecord(dir, r); err != nil {
			return err
		}
		out, err = readRecord(dir, name)
		return err
	})
	return out, err
}
