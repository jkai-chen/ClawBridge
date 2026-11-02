package linearboard

import (
	"context"
	"log"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

// intakeCheckTimeout bounds the text check of an issue read.
const intakeCheckTimeout = 10 * time.Second

// WithIntakeText marks an issue read with whether the issue's text changed
// since the trusted signal that queued it (FR57). It asks the bus for the
// server's stored verdict and sends no hashes: this read is the person's or
// the agent's own profile's, which Linear renders differently from the
// app's, so only the server's app reads compare text. Only an issue the
// queue tracks gains the field. A check that fails never fails the read:
// the field is left off.
func (s *Service) WithIntakeText(ctx context.Context, scope bus.Request, issue *linearprovider.IssueV2) {
	if issue == nil || s.cfg.Executor == nil || !linearprovider.ValidID(issue.ID) {
		return
	}
	args := map[string]any{"issue_uuid": issue.ID}
	if instance, _ := scope.Args["instance"].(string); instance != "" {
		args["instance"] = instance
	}
	// The check is its own short call: the read's deadline may be nearly
	// spent, and the read is already done.
	check, cancel := context.WithTimeout(context.WithoutCancel(ctx), intakeCheckTimeout)
	defer cancel()
	result, err := s.cfg.Executor.Execute(check, bus.Request{Workspace: scope.Workspace, Project: scope.Project, Session: scope.Session, Operation: "intake_text_status", Args: args})
	if err != nil {
		log.Print("clawbridge: intake text check failed; the read goes without it")
		return
	}
	data, _ := result.Data.(map[string]any)
	if tracked, _ := data["tracked"].(bool); !tracked {
		return
	}
	changed, _ := data["changed"].(bool)
	maybe, _ := data["maybe"].(bool)
	signalAt, _ := data["signal_at"].(string)
	issue.Intake = &linearprovider.IntakeText{TextChangedSinceSignal: changed, Maybe: maybe, SignalAt: signalAt}
}
