package bus

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
)

// Coordination describes conversation context; it never grants task ownership
// or human approval. Assignment revisions are opaque coordinator references.
type Coordination struct {
	IssueID            string `json:"issue_id"`
	AssignmentRevision string `json:"assignment_revision"`
	Intent             string `json:"intent"`
	RequestID          string `json:"request_id,omitempty"`
	SourceCommit       string `json:"source_commit,omitempty"`
}

var coordinationUUID = regexp.MustCompile(`\A[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\z`)
var coordinationID = regexp.MustCompile(`\A[A-Za-z0-9._-]{1,128}\z`)
var coordinationCommit = regexp.MustCompile(`\A(?:[0-9a-f]{40}|[0-9a-f]{64})\z`)

// ParseCoordination validates a present object, including explicit optional nulls.
func ParseCoordination(value any) (Coordination, error) {
	var c Coordination
	data, err := json.Marshal(value)
	if err != nil {
		return c, errors.New("coordination must be a bounded object")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || fields == nil {
		return c, errors.New("coordination must be an object")
	}
	values := map[string]string{}
	for k, v := range fields {
		switch k {
		case "issue_id", "assignment_revision", "intent", "request_id", "source_commit":
		default:
			return c, errors.New("unknown coordination field")
		}
		var text string
		if string(v) == "null" || json.Unmarshal(v, &text) != nil || text == "" {
			return c, errors.New("coordination fields must be nonempty strings")
		}
		values[k] = text
	}
	c = Coordination{values["issue_id"], values["assignment_revision"], values["intent"], values["request_id"], values["source_commit"]}
	if !coordinationUUID.MatchString(c.IssueID) || len(c.AssignmentRevision) < 1 || len(c.AssignmentRevision) > 128 {
		return Coordination{}, errors.New("invalid coordination issue or revision")
	}
	for _, ch := range []byte(c.AssignmentRevision) {
		if ch < 32 || ch > 126 {
			return Coordination{}, errors.New("revision requires printable ASCII")
		}
	}
	for _, re := range secretPatterns {
		if re.MatchString(c.AssignmentRevision) {
			return Coordination{}, errors.New("coordination revision rejected: possible secret")
		}
	}
	switch c.Intent {
	case "request", "accept", "decline", "clarify", "progress", "ready", "blocked", "handoff":
	default:
		return Coordination{}, errors.New("invalid coordination intent")
	}
	if (c.RequestID != "" && !coordinationID.MatchString(c.RequestID)) || (c.SourceCommit != "" && !coordinationCommit.MatchString(c.SourceCommit)) {
		return Coordination{}, errors.New("invalid coordination request or commit")
	}
	if (c.Intent == "accept" || c.Intent == "decline" || c.Intent == "clarify") && c.RequestID == "" {
		return Coordination{}, errors.New("acknowledgement requires request_id")
	}
	canonical, _ := json.Marshal(c)
	if len(canonical) > 1024 {
		return Coordination{}, errors.New("coordination exceeds 1024 bytes")
	}
	return c, nil
}
func (s *Store) correlateCoordination(ctx context.Context, tx *transaction, pid, sid string, c Coordination, r Request) error {
	if c.RequestID == "" {
		return nil
	}
	if r.Operation == "reply" && c.RequestID != str(r.Args, "message_id") {
		return errors.New("coordination request_id must match reply message_id")
	}
	parent, err := s.participatingMessage(ctx, tx, pid, sid, c.RequestID)
	if err != nil {
		return err
	}
	original, err := ParseCoordination(parent["coordination"])
	if err != nil || original.IssueID != c.IssueID || original.AssignmentRevision != c.AssignmentRevision {
		return errors.New("coordination request issue/revision mismatch")
	}
	return nil
}
