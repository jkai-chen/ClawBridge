package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"clawbridge/internal/bus"
	"clawbridge/internal/mcpserver"
	"clawbridge/internal/remote"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// runtimeIdentity says whether `clawbridge mcp` runs in runtime-identity mode
// (SESSION-CONFIG-SIMPLIFY §2.2): the --session flag was not given. Only that
// flag on the mcp argv pins a session, as every installed entry carries it.
// CLAWBRIDGE_SESSION, CLAWBRIDGE_INSTANCE and a session in a config file never
// pin: a child agent inherits the environment, and a pin would skip the
// liveness and conversation checks (F7). The session they name is only a
// suggestion.
func runtimeIdentity(flags Config) bool {
	return flags.Session == ""
}

// runMCPRuntime serves MCP with no pinned session. The route (the profile of
// the agent's home, the project of the folder) is opened on the first tool
// call that needs a session, not here.
func runMCPRuntime(ctx context.Context, flags, resolved Config) error {
	if flags.LinearProfile != "" {
		return errors.New("--linear-profile needs an explicit --session: the native Linear connection is not available while the session is resolved at runtime")
	}
	home, err := stateHome()
	if err != nil {
		return err
	}
	folder := ""
	if wd, e := os.Getwd(); e == nil {
		folder = filepath.Base(wd)
	}
	o := mcpserver.RuntimeOptions{
		DefaultHome: home,
		Folder:      folder,
		Suggest:     suggestedSession(resolved),
		EnvID:       os.Getenv("CLAUDE_CODE_SESSION_ID"),
		Open: func(ctx context.Context, home string) (*mcpserver.RuntimeRoute, error) {
			return openRuntimeRoute(ctx, flags, home)
		},
	}
	server, done := mcpserver.NewRuntimeServer(o, mcpserver.Options{Context: ctx, ContextDir: contextDirOrEmpty()})
	defer done()
	return server.Run(ctx, &mcp.StdioTransport{})
}

// openRuntimeRoute loads the profile of home and resolves the folder's
// workspace and project, as a pinned server does at start.
func openRuntimeRoute(ctx context.Context, flags Config, home string) (*mcpserver.RuntimeRoute, error) {
	config, _, err := resolveConfigAt(ctx, home, flags, nil)
	if err != nil {
		return nil, err
	}
	var executor interface {
		mcpserver.Executor
		ResolveProject(context.Context, string, string, string) (string, string, error)
	}
	var closeFn func()
	if config.Mode == "cloud" {
		client, e := remote.New(remoteConfig(config))
		if e != nil {
			return nil, e
		}
		executor = client
	} else {
		store, e := bus.Open(config.DB)
		if e != nil {
			return nil, e
		}
		executor, closeFn = store, func() { _ = store.Close() }
	}
	remoteURL := gitRemote(ctx)
	workspace, project, err := executor.ResolveProject(ctx, config.Workspace, config.Project, remoteURL)
	if err != nil {
		if closeFn != nil {
			closeFn()
		}
		return nil, err
	}
	return &mcpserver.RuntimeRoute{
		Store: executor, Workspace: workspace, Project: project,
		ProjectSource: projectSource(config, remoteURL),
		Server:        queueServer(config), DB: queueDB(config), Close: closeFn,
	}, nil
}

// projectSource says where the folder's project came from, for the first
// claim and for whoami (design C4): the repo's binding file, the git remote,
// or the profile.
func projectSource(c Config, remoteURL string) string {
	if c.Project == "" && remoteURL != "" {
		return "the git remote"
	}
	if rest, ok := strings.CutPrefix(c.Sources["project"], ".clawbridge.toml at "); ok {
		return ".clawbridge.toml at " + filepath.Dir(rest)
	}
	return "profile default"
}

// suggestedSession is the session name the environment or a config file
// offers, which the unbound message shows; it never binds.
func suggestedSession(resolved Config) string {
	if s := os.Getenv("CLAWBRIDGE_SESSION"); s != "" {
		return s
	}
	return resolved.Session
}
