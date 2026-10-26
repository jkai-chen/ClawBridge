package agentd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"
)

const usage = `usage:
  clawbridge-agentd proxy --listen SOCKET --upstream SOCKET
      the validating socket proxy, run as the agents' own user (cbagent),
      in front of its rootless dockerd
  clawbridge-agentd check
      validate agentd's configuration and its proxy socket
  clawbridge-agentd run
      the launcher (needs the server's launcher endpoints)`

// errNoLauncherEndpoints: the server side of §11.4 is not in this release.
var errNoLauncherEndpoints = errors.New("this server release has no launcher endpoints yet; clawbridge-agentd run waits for them (check and proxy work)")

// Main runs clawbridge-agentd with args, reading its configuration from
// getenv. Logs are JSON lines on stderr.
func Main(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	log := slog.New(slog.NewJSONHandler(stderr, nil))
	if len(args) == 0 {
		return errors.New(usage)
	}
	if err := notRoot(); err != nil {
		return err
	}
	switch args[0] {
	case "proxy":
		return runProxy(ctx, args[1:], getenv, log)
	case "check", "run":
		cfg, err := LoadConfig(getenv)
		if err != nil {
			return err
		}
		if err := checkSocket(cfg.Docker); err != nil {
			return err
		}
		if args[0] == "check" {
			_, err := fmt.Fprintln(stdout, "clawbridge-agentd: configuration ok")
			return err
		}
		return errNoLauncherEndpoints
	}
	return errors.New(usage)
}

func runProxy(ctx context.Context, args []string, getenv func(string) string, log *slog.Logger) error {
	fs := flag.NewFlagSet("proxy", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	listen := fs.String("listen", "", "")
	upstream := fs.String("upstream", "", "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return errors.New(usage)
	}
	// The proxy is on every agent's side of the boundary: it holds no
	// secrets.
	if getenv("CLAWBRIDGE_LAUNCHER_TOKEN") != "" {
		return errors.New("the proxy holds no secrets: remove CLAWBRIDGE_LAUNCHER_TOKEN from its environment")
	}
	for _, path := range []string{*listen, *upstream} {
		if err := validSocketPath(path); err != nil {
			return err
		}
	}
	if *listen == *upstream {
		return errors.New("the proxy can't listen on its upstream socket")
	}
	if err := checkSocket(*upstream); err != nil {
		return err
	}
	p, err := NewProxy(getenv("CLAWBRIDGE_AGENT_IMAGE"), getenv("CLAWBRIDGE_SERVER_URL"), *upstream, log)
	if err != nil {
		return err
	}
	// A socket left by an earlier run is replaced; anything else is not.
	if info, err := os.Lstat(*listen); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("%s exists and is not a socket", *listen)
		}
		if err := os.Remove(*listen); err != nil {
			return err
		}
	}
	// agentd connects through its group; nobody else can, not even while
	// the socket is created.
	l, err := listenSocket(*listen)
	if err != nil {
		return err
	}
	if err := os.Chmod(*listen, 0o660); err != nil {
		l.Close()
		return err
	}
	srv := &http.Server{Handler: p, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Info("proxy listening", "listen", *listen, "upstream", *upstream)
	if err := srv.Serve(l); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
