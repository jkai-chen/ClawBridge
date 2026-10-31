package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"clawbridge/internal/buildinfo"
	"clawbridge/internal/guibackend"
	"clawbridge/internal/webui"
)

// uiOpener opens a browser; tests replace it.
var uiOpener = func(platform string) webui.Opener { return webui.SystemOpener(platform) }

// runUI serves the local web UI (live/GUI-DESIGN.md §12): this client, in
// the person's terminal, behind a loopback listener. It stops when the tab
// closes, on Ctrl-C or on SIGTERM.
func runUI(ctx context.Context, flags Config, args []string, stdout, stderr io.Writer) error {
	var noOpen bool
	var port int
	_, err := commandFlags("ui", args, stderr, func(f *flag.FlagSet) {
		f.BoolVar(&noOpen, "no-open", false, "print the link instead of opening a browser")
		f.IntVar(&port, "port", 0, "listen on this port of 127.0.0.1 (default: a random one)")
	})
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	// C10: parity with the CLI's person-only commands. An agent that clears
	// its environment still can, as it can run the CLI.
	if flags.Session != "" {
		return errors.New("the UI is never opened with --session; run it yourself in your own terminal")
	}
	if marker := agentMarker(closesEnv()); marker != "" {
		return fmt.Errorf("the UI is a person's interface, and %s says an agent runs this command; run it yourself in your own terminal", marker)
	}
	home, err := stateHome()
	if err != nil {
		return err
	}
	// One UI per profile: the state directory is keyed by the profile's path.
	_, profile, err := resolveConfig(ctx, flags)
	if err != nil {
		return err
	}
	stateDir := filepath.Join(home, "ui")
	platform := guibackend.Platform()
	backend := &guibackend.Server{}
	srv, err := webui.New(webui.Config{
		Backend:  backend,
		Profile:  profile,
		StateDir: stateDir,
		Port:     port,
		Platform: platform,
		Opener:   uiOpener(platform),
		Log:      stderr,
	})
	var running *webui.AlreadyRunning
	if errors.As(err, &running) {
		return handOffUI(ctx, stateDir, profile, platform, noOpen, stdout, stderr)
	}
	if err != nil {
		return err
	}
	defer srv.Close()

	in, err := openUIInputs(ctx, flags)
	if err != nil {
		return err
	}
	defer in.Close()
	c := in.Config
	backend.Hello = guibackend.Hello{
		ClientVersion: buildinfo.Current().Version,
		Mode:          c.Mode,
		Platform:      platform,
		Profile:       map[string]any{"workspace": c.Workspace, "project": c.Project, "waiting_for_project": in.Waiting},
	}
	backend.Methods = map[string]guibackend.Method{
		"sessions.list": guibackend.SessionsList(in.Executor, guibackend.Scope{Workspace: c.Workspace, Project: c.Project, Session: c.Session, Instance: c.Instance}),
		"doctor.checks": guibackend.DoctorChecks(in.Doctor),
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Fprintf(stdout, "ClawBridge UI on %s (stops when you close the tab)\n", srv.URL())
	if noOpen {
		link, err := srv.NoOpenURL()
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Open this link in your browser (single use, valid for a minute): %s\n", link)
	} else {
		go openUI(ctx, stderr, webui.FileRoute(platform), func(ctx context.Context) (bool, error) { return srv.Open(ctx) })
	}
	err = srv.Serve(ctx)
	switch {
	case errors.Is(err, webui.ErrIdle):
		fmt.Fprintln(stdout, "ClawBridge UI stopped: no tab was connected for a minute.")
		return nil
	case errors.Is(err, webui.ErrNeverConnected):
		fmt.Fprintln(stdout, "ClawBridge UI stopped: no tab connected. Run clawbridge ui again, or clawbridge ui --no-open for a link.")
		return nil
	}
	return err
}

// openUI runs the opener and says how to get a link if it did not work. The
// message never carries a token.
func openUI(ctx context.Context, stderr io.Writer, fileRoute bool, open func(context.Context) (bool, error)) {
	opened, err := open(ctx)
	if fileRoute && err == nil {
		// Opening an .html file goes through the file association, which
		// can start an editor and still report success (U2).
		fmt.Fprintln(stderr, "If the browser didn't open ClawBridge, run `clawbridge ui --no-open` for a link.")
		return
	}
	if err != nil {
		fmt.Fprintf(stderr, "Could not open a browser (%v). Run clawbridge ui --no-open to get a link.\n", err)
		return
	}
	if !opened && ctx.Err() == nil {
		fmt.Fprintln(stderr, "If no tab opened, run clawbridge ui --no-open to get a link.")
	}
}

// handOffUI asks the UI already running for this profile for a fresh launch
// and opens it, or says where it runs.
func handOffUI(ctx context.Context, stateDir, profile, platform string, noOpen bool, stdout, stderr io.Writer) error {
	want := "open"
	if noOpen {
		want = "url"
	}
	h, err := webui.RequestHandoff(ctx, stateDir, profile, want)
	var noControl *webui.NoControlError
	if errors.As(err, &noControl) {
		if noControl.URL == "" {
			return err
		}
		fmt.Fprintf(stdout, "ClawBridge UI is already running on %s. Stop it with Ctrl-C in its terminal and run clawbridge ui again for a new tab.\n", noControl.URL)
		return nil
	}
	if err != nil {
		return err
	}
	if noOpen {
		fmt.Fprintf(stdout, "ClawBridge UI is already running on %s\nOpen this link in your browser (single use, valid for a minute): %s\n", h.URL, h.Target)
		return nil
	}
	fmt.Fprintf(stdout, "ClawBridge UI is already running on %s; opening a new tab.\n", h.URL)
	openUI(ctx, stderr, webui.FileRoute(platform), func(ctx context.Context) (bool, error) { return uiOpener(platform)(ctx, h.Target) })
	return nil
}
