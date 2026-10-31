package cli

import (
	"context"
	"sync"

	"clawbridge/internal/bus"
	"clawbridge/internal/doctor"
	"clawbridge/internal/remote"
	"clawbridge/internal/tui"
)

// uiInputs is what a person's interface needs from the client: the TUI
// today, and the GUI's backend (live/GUI-DESIGN.md §4.3). Both open it the
// same way, so neither can drift from the other or from the CLI.
type uiInputs struct {
	// Config is resolved, with Workspace and Project as ResolveProject
	// answered them; Path is the profile it came from.
	Config Config
	Path   string
	// Context is bounded by Close.
	Context  context.Context
	Executor doctor.Executor
	// OutboxWaiting counts queued writes; nil in local mode.
	OutboxWaiting func() int
	// Waiting opens on "Waiting for a project".
	Waiting bool
	Linear  tui.LinearSource
	// Info names the authority; the TUI shows it on its Cloud tab.
	Info string
	// Close cancels Context, stops the outbox drain and closes the local
	// store, once.
	Close func()
}

// openUIInputs resolves the profile and opens its executor, outbox drain,
// project and Linear source. On an error, everything it opened is closed
// again and no inputs are returned.
func openUIInputs(ctx context.Context, flags Config) (in *uiInputs, err error) {
	c, path, err := resolveConfig(ctx, flags)
	if err != nil {
		return nil, err
	}
	bounded, cancel := context.WithCancel(ctx)
	cleanups := []func(){cancel}
	var once sync.Once
	closeAll := func() {
		once.Do(func() {
			for i := len(cleanups) - 1; i >= 0; i-- {
				cleanups[i]()
			}
		})
	}
	defer func() {
		if err != nil {
			closeAll()
			in = nil
		}
	}()
	var executor doctor.Executor
	var outboxWaiting func() int
	if c.Mode == "cloud" {
		var client *remote.Client
		client, err = remote.New(remoteConfig(c))
		if err == nil {
			executor = client
			var outboxStopped func()
			outboxWaiting, outboxStopped = tuiOutbox(bounded, client)
			// The drain stops after the context is cancelled.
			cleanups = append(cleanups, func() {
				cancel()
				outboxStopped()
			})
		}
	} else {
		var store *bus.Store
		store, err = bus.Open(c.DB)
		if err == nil {
			cleanups = append(cleanups, func() { store.Close() })
			executor = store
		}
	}
	if err != nil {
		return nil, err
	}
	workspace := c.Workspace
	c.Workspace, c.Project, err = executor.ResolveProject(bounded, c.Workspace, c.Project, gitRemote(bounded))
	// A hosted profile that joined a workspace but has no project yet opens
	// on "Waiting for a project" (TUI-UX-AUDIT PR D), which saves the
	// project once one is shared and reopens the TUI on it.
	waiting := waitForProject(err, c.Mode, workspace, flags.Project, c.Project, func() error { return signedProjectList(bounded, executor, workspace) })
	if waiting {
		c.Workspace, err = workspace, nil
	}
	if err != nil {
		return nil, err
	}
	info := "Provisioning unavailable. This interface never creates infrastructure."
	if c.Mode == "cloud" {
		info = "Endpoint: " + c.Server + "\n" + info
	} else {
		info = "Local authority.\n" + info
	}
	linearSource, err := newLinearTUISource(c, executor)
	if err != nil {
		return nil, err
	}
	return &uiInputs{Config: c, Path: path, Context: bounded, Executor: executor, OutboxWaiting: outboxWaiting, Waiting: waiting, Linear: linearSource, Info: info, Close: closeAll}, nil
}

// Doctor runs doctor's checks on these inputs.
func (in *uiInputs) Doctor(ctx context.Context) (doctor.Report, error) {
	return collectDoctor(ctx, in.Config, in.Path, in.Executor)
}
