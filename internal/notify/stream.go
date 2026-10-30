package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"clawbridge/internal/remote"
	"clawbridge/internal/secretenv"
)

// Streamer is an executor that can follow a session's stream: the hosted
// client. With one, notify is woken by the server's observe stream instead
// of polling every interval (MESSAGE-QUEUE-DESIGN §5).
type Streamer interface {
	Stream(context.Context, remote.StreamScope, remote.StreamOptions, func(remote.StreamEvent) error) error
}

// streamSafety is how often notify still checks while its stream is open,
// in case a wake was missed.
const streamSafety = 60 * time.Second

// waker turns the observe stream into wakes for the poll loop. Only the
// loop writes diagnostics; the stream's end comes back on ended.
type waker struct {
	wake  chan struct{}
	ended chan error
	live  bool
	// open is set by the stream goroutine on each connect.
	open chan struct{}
}

func startWaker(ctx context.Context, s Streamer, b Binding) *waker {
	w := &waker{wake: make(chan struct{}, 1), ended: make(chan error, 1), open: make(chan struct{}, 1)}
	poke := func(ch chan struct{}) {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	scope := remote.StreamScope{Workspace: b.Workspace, Project: b.Project, Session: b.Session, Instance: b.Instance, Mode: "observe"}
	go func() {
		w.ended <- s.Stream(ctx, scope, remote.StreamOptions{OnConnect: func() { poke(w.open); poke(w.wake) }}, func(e remote.StreamEvent) error {
			if e.Name == "status" {
				poke(w.wake)
			}
			return nil
		})
	}()
	return w
}

// wait sleeps for the poll interval, or the safety interval while the
// stream is open, and returns early on a wake. A stream that ended is
// reported once, and notify polls from then on.
func (w *waker) wait(ctx context.Context, c Config) error {
	if w == nil {
		return c.Wait(ctx, c.Interval)
	}
	d := c.Interval
	if w.live {
		d = max(c.Interval, streamSafety)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			return nil
		case <-w.wake:
			return nil
		case <-w.open:
			w.live = true
		case err := <-w.ended:
			w.live, w.ended = false, nil
			kind := "stream_ended"
			if errors.Is(err, remote.ErrStreamUnsupported) {
				kind = "stream_unavailable"
			}
			if ctx.Err() == nil {
				if e := diagnostic(c.Diagnostics, kind, nil); e != nil {
					return e
				}
			}
			return nil
		}
	}
}

// NoticeText is the only text the command adapter is ever given to show or
// type (M6), and the only content the channel server pushes (FR26): fixed
// words and a count, never a sender, subject or body, so no member can type
// into an idle agent's terminal or prompt it.
func NoticeText(waiting int64) string {
	if waiting == 1 {
		return "1 ClawBridge message is waiting; call inbox"
	}
	return fmt.Sprintf("%d ClawBridge messages are waiting; call inbox", waiting)
}

// TextPlaceholder in a command's arguments is replaced by the notice text.
const TextPlaceholder = "{text}"

// CommandRunner runs the command adapter's argv, never through a shell,
// with env added and stdin given.
type CommandRunner func(ctx context.Context, argv, env []string, stdin []byte) error

func runCommand(ctx context.Context, argv, env []string, stdin []byte) error {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	// G9 F3: the command gets the environment without ClawBridge's secrets.
	cmd.Env = append(secretenv.Scrub(os.Environ()), env...)
	cmd.WaitDelay = 250 * time.Millisecond
	cmd.Stdin = strings.NewReader(string(stdin))
	var output boundedOutput
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Run(); err != nil || output.overflow {
		return errors.New("notify command failed or timed out; checkpoint not advanced")
	}
	return nil
}

// resolveCommand checks the command adapter's argv and makes its program
// an absolute path, once, before anything runs.
func resolveCommand(argv []string) ([]string, error) {
	if len(argv) == 0 || len(argv) > 64 {
		return nil, Permanent(errors.New("the command adapter needs a command after --, at most 64 arguments"))
	}
	if argv[0] == "" {
		return nil, Permanent(errors.New("the command adapter needs a command after --"))
	}
	for _, a := range argv {
		if len(a) > 4096 || strings.ContainsRune(a, 0) {
			return nil, Permanent(errors.New("command arguments must be text of at most 4096 bytes"))
		}
	}
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return nil, Permanent(errors.New("notify command is not an executable"))
	}
	if path, err = filepath.Abs(path); err != nil {
		return nil, Permanent(errors.New("cannot resolve the notify command's path"))
	}
	return append([]string{path}, argv[1:]...), nil
}

// commandHint hands one notice to the command adapter: the fixed text in
// place of each {text} argument and in CLAWBRIDGE_NOTICE_TEXT, the count in
// CLAWBRIDGE_NOTICE_COUNT, and the event's IDs as JSON on stdin.
func commandHint(ctx context.Context, c Config, e Event) error {
	text := NoticeText(e.UnreadCount)
	argv := make([]string, len(c.Binding.Command))
	for i, a := range c.Binding.Command {
		argv[i] = strings.ReplaceAll(a, TextPlaceholder, text)
	}
	stdin, err := json.Marshal(e)
	if err != nil {
		return Permanent(err)
	}
	env := []string{"CLAWBRIDGE_NOTICE_TEXT=" + text, "CLAWBRIDGE_NOTICE_COUNT=" + strconv.FormatInt(e.UnreadCount, 10)}
	bounded, cancel := context.WithTimeout(ctx, c.HostTimeout)
	defer cancel()
	return c.CommandRunner(bounded, argv, env, append(stdin, '\n'))
}
