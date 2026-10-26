package linearboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"

	"clawbridge/internal/bus"
)

// DocPolicyV2 reads the project's document policy: any member may, the
// deny patterns' regexes shown to an owner or lead only.
func (s *Service) DocPolicyV2(ctx context.Context, scope bus.Request) (any, error) {
	scope.Operation = "doc_policy_get"
	scope.Args = instanceArgs(scope.Args)
	r, e := s.cfg.Executor.Execute(ctx, scope)
	return r.Data, e
}

// SetDocPolicyV2 sets the project's document policy from the owner's file
// at expected, the revision the owner last read.
func (s *Service) SetDocPolicyV2(ctx context.Context, scope bus.Request, raw []byte, expected int64) (any, error) {
	if e := uniqueKeys(raw); e != nil {
		return nil, e
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	if dec.Decode(&v) != nil || dec.Decode(new(any)) != io.EOF {
		return nil, errors.New("the document policy file must hold one JSON object")
	}
	policy, e := bus.ParseDocPolicy(v)
	if e != nil {
		return nil, e
	}
	scope.Operation = "doc_policy_set"
	scope.Args = instanceArgs(scope.Args)
	scope.Args["expected_revision"] = expected
	scope.Args["doc_policy"] = policy
	r, e := s.cfg.Executor.Execute(ctx, scope)
	return r.Data, e
}
