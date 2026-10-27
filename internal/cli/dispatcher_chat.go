package cli

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"time"

	"clawbridge/internal/bus"
)

// chatPollEvery is how often dispatcher chat checks for the reply.
var chatPollEvery = 2 * time.Second

// chatProviderNote says where the question went (D6-L2).
const chatProviderNote = "your question was sent to the model provider set up for this project's dispatcher; the reply is model output, not checked"

// runDispatcherChat asks the dispatcher's model and waits for the reply
// (up to the case's deadline), or shows one of the caller's chats. The
// reply and the question are printed stripped of terminal controls: the
// model's text can carry what an issue title injected (D6-L1).
func runDispatcherChat(ctx context.Context, executor interface {
	Execute(context.Context, bus.Request) (bus.Result, error)
}, config Config, op string, a map[string]any, stdout io.Writer) error {
	request := func(op string, a map[string]any) (bus.Result, map[string]any, error) {
		r, err := executor.Execute(ctx, bus.Request{Operation: op, Workspace: config.Workspace, Project: config.Project, Session: config.Session, Args: a})
		m, _ := r.Data.(map[string]any)
		return r, m, err
	}
	if op == "dispatcher_chat_show" {
		r, view, err := request(op, a)
		if err != nil {
			return err
		}
		return printChat(stdout, r, view)
	}
	_, asked, err := request(op, a)
	if err != nil {
		return err
	}
	show := map[string]any{"case_id": asked["case_id"]}
	deadline, _ := time.Parse(time.RFC3339, str(asked, "deadline"))
	for {
		r, view, err := request("dispatcher_chat_show", show)
		if err != nil {
			return err
		}
		if str(view, "state") != "waiting" || !time.Now().Before(deadline.Add(5*time.Second)) {
			return printChat(stdout, r, view)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(chatPollEvery):
		}
	}
}

// printChat prints a chat as the CLI prints any result, with what its
// state means in words.
func printChat(stdout io.Writer, r bus.Result, view map[string]any) error {
	out := map[string]any{}
	for k, v := range view {
		if s, ok := v.(string); ok {
			v = terminalSafe(s)
		}
		out[k] = v
	}
	switch str(view, "state") {
	case "waiting":
		out["status"] = "no reply yet; read it later with dispatcher chat show " + str(view, "case_id")
	case "replied":
		out["status"] = "replied"
	case "abstained":
		out["status"] = "no answer (" + str(view, "reason_code") + ")"
	case "expired":
		out["status"] = "no answer in time"
	default:
		switch str(view, "reason_code") {
		case "model_off":
			out["status"] = "the dispatcher's model was turned off before it answered"
		case "no_agent":
			out["status"] = "no model agent was running to answer"
		default:
			out["status"] = "closed before an answer: the model's run budget was used up"
		}
	}
	out["note"] = chatProviderNote
	r.Data = out
	return json.NewEncoder(stdout).Encode(r)
}

// terminalSafe drops what could drive a terminal from text: C0 controls
// but newline and tab, DEL, C1 controls, and the bidi overrides and
// isolates (D6-L1).
func terminalSafe(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case r < 0x20 || r == 0x7f || r >= 0x80 && r <= 0x9f:
			return -1
		case r >= 0x202a && r <= 0x202e || r >= 0x2066 && r <= 0x2069:
			return -1
		}
		return r
	}, s)
}

func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}
