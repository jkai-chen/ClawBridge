package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os/user"
	"path/filepath"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/notify"
	"clawbridge/internal/remote"
)

var newNotifyExecutor = func(c Config) (notify.Executor, func(), error) {
	if c.Mode == "cloud" {
		e, err := remote.New(remoteConfig(c))
		return e, func() {}, err
	}
	e, err := bus.Open(c.DB)
	return e, func() {
		if e != nil {
			_ = e.Close()
		}
	}, err
}
var runNotifyWatcher = notify.Run

func runNotify(ctx context.Context, flags Config, args []string, stdout, stderr io.Writer) error {
	var adapter, thread, state, command string
	var interval time.Duration
	var once bool
	f, err := commandFlags("notify", args, stderr, func(f *flag.FlagSet) {
		f.StringVar(&adapter, "adapter", "", "json or codex (explicit opt-in)")
		f.StringVar(&thread, "thread", "", "explicit canonical Codex thread UUID")
		f.StringVar(&state, "state", "", "absolute private checkpoint path")
		f.StringVar(&command, "codex-command", "", "absolute Codex executable override")
		f.DurationVar(&interval, "interval", 5*time.Second, "authority poll interval (1s–300s)")
		f.BoolVar(&once, "once", false, "at most one status poll and hint attempt")
	})
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	if adapter != "json" && adapter != "codex" {
		return errors.New("notify requires --adapter json or codex")
	}
	if !filepath.IsAbs(state) || interval < time.Second || interval > 300*time.Second {
		return errors.New("notify requires absolute --state and interval 1s–300s")
	}
	if adapter == "codex" && (!notify.ValidThread(thread) || (command != "" && !filepath.IsAbs(command))) {
		return errors.New("Codex adapter requires canonical UUID --thread and an absolute executable override")
	}
	if adapter == "json" {
		bad := false
		f.Visit(func(fl *flag.Flag) {
			if fl.Name == "thread" || fl.Name == "codex-command" {
				bad = true
			}
		})
		if bad {
			return errors.New("JSON adapter rejects Codex-only flags")
		}
	}
	c, _, err := resolveConfig(ctx, flags)
	if err != nil {
		return err
	}
	if c.Workspace == "" || c.Project == "" || c.Session == "" || c.Instance == "" {
		return errors.New("notify requires existing workspace/project/session and exact instance; never registers")
	}
	principal := c.Principal
	var route any
	if c.Mode == "cloud" {
		if principal == "" {
			principal = c.Account
		}
		if principal == "" {
			principal = "oauth:" + c.OAuthClientID
		}
		cloud := []string{"cloud", c.Server, c.Audience, c.CredentialSource, c.Principal, c.Account, c.ImpersonateServiceAccount, c.OAuthClientID, c.OAuthKeyringService, c.CredentialsFile}
		if c.CredentialSource == "device" {
			// Appended only for a device route, so every other route's
			// state binding is unchanged.
			cloud = append(cloud, c.DeviceID)
		}
		route = cloud
	} else {
		u, err := user.Current()
		if err != nil {
			return err
		}
		principal = "local:" + u.Uid + ":" + u.Username
		db, err := filepath.Abs(c.DB)
		if err != nil {
			return err
		}
		if resolved, e := filepath.EvalSymlinks(db); e == nil {
			db = resolved
		}
		route = []string{"local", db, principal}
	}
	raw, _ := json.Marshal(route)
	digest := sha256.Sum256(raw)
	executor, closeExecutor, err := newNotifyExecutor(c)
	if err != nil {
		return err
	}
	defer closeExecutor()
	return runNotifyWatcher(ctx, notify.Config{Binding: notify.Binding{Authority: hex.EncodeToString(digest[:]), Principal: principal, Workspace: c.Workspace, Project: c.Project, Session: c.Session, Instance: c.Instance, Adapter: adapter, Thread: thread}, Executor: executor, StatePath: state, Interval: interval, Once: once, Output: stdout, Diagnostics: stderr, CodexCommand: command})
}
