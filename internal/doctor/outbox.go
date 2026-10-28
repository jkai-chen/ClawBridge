package doctor

import (
	"context"
	"fmt"
	"os"
	"time"

	"clawbridge/internal/outbox"
)

// maintenanceReader is a hosted client that heard the server say it is in
// maintenance.
type maintenanceReader interface {
	MaintenanceUntil() (time.Time, bool)
}

// checkServerMaintenance warns while a deploy drains the server
// (ZERO-LOSS-DEPLOY §2.4). It shows only then.
func checkServerMaintenance(executor Executor, add func(Check)) {
	m, ok := executor.(maintenanceReader)
	if !ok {
		return
	}
	until, on := m.MaintenanceUntil()
	if !on {
		return
	}
	add(Check{ID: "server.maintenance", Title: "Server maintenance", Status: Warn,
		Message: fmt.Sprintf("The server is in maintenance until %s UTC at the latest: a deploy is draining it. Reads answer; writes are refused until it ends, and message writes wait in this machine's outbox.", until.UTC().Format("15:04")),
		Action:  "Nothing to do: the deploy ends it. Retry a refused write with the same operation ID afterwards."})
}

// checkClientOutbox reads this machine's outbox (§1.5): pass when empty;
// warn when a write has waited over 10 minutes or half a bound is used;
// fail when it is full, a write expired, or a dead letter is from the
// last day.
func checkClientOutbox(ctx context.Context, config Config, add func(Check)) {
	if config.Mode != "cloud" || outbox.Disabled() {
		return
	}
	path := outbox.PathFor(config.Cache)
	c := Check{ID: "client.outbox", Title: "Client outbox", Status: Pass, Path: path, Message: "Nothing waits in this machine's outbox."}
	if path == "" {
		return
	}
	if _, err := os.Stat(path); err != nil {
		add(c)
		return
	}
	ob, err := outbox.Open(path)
	if err == nil {
		defer ob.Close()
	}
	var st outbox.Status
	if err == nil {
		st, err = ob.Status(ctx)
	}
	if err != nil {
		c.Status, c.Message = Fail, "This machine's outbox can't be read: "+err.Error()
		c.Action = "Check the file's permissions; while it is unreadable, writes that can't reach the server fail as before."
		add(c)
		return
	}
	waiting := fmt.Sprintf("%d write(s) wait in this machine's outbox, the oldest for %s; they are sent when the server answers.", st.Waiting, st.Oldest.Round(time.Second))
	switch {
	case st.Full():
		c.Status, c.Message = Fail, "This machine's outbox is full: new writes that can't reach the server fail. "+waiting
	case st.Expired:
		c.Status, c.Message = Fail, "A write waited 24 hours without reaching the server and became a dead letter."
	case st.Dead > 0:
		c.Status, c.Message = Fail, fmt.Sprintf("%d write(s) from the last day were refused by the server and are dead letters.", st.Dead)
	case st.Waiting > 0 && (st.Oldest > outbox.WarnAge || st.Waiting >= outbox.MaxItems/2 || st.Bytes >= outbox.MaxBytes/2):
		c.Status, c.Message = Warn, waiting
	case st.Waiting > 0:
		c.Message = waiting
	}
	if c.Status != Pass {
		c.Action = "Run clawbridge outbox list (--dead for the dead letters); outbox flush sends now, retry ID resends a dead letter, drop ID --confirm drop deletes one. Never resend a waiting write under a new operation ID."
	}
	add(c)
}
