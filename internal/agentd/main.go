package agentd

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"time"
)

const usage = `usage:
  clawbridge-agentd proxy --listen SOCKET --upstream SOCKET
      the validating socket proxy, run as the agents' own user (cbagent),
      in front of its rootless dockerd
  clawbridge-agentd egress --listen HOST:PORT --policy FILE
      the per-owner egress proxy on the agents' networks; it reloads the
      policy agentd writes, and refuses everything while it is invalid
  clawbridge-agentd check
      validate agentd's configuration and its proxy socket
  clawbridge-agentd run
      the launcher: every 30 s, matches the agents to the server's wanted set`

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
	case "egress":
		return runEgress(ctx, args[1:], getenv, log)
	case "check", "run":
		cfg, err := LoadConfig(getenv)
		if err != nil {
			return err
		}
		if err := checkSocket(cfg.Docker); err != nil {
			return err
		}
		if info, err := os.Stat(cfg.PolicyDir); err != nil || !info.IsDir() {
			return fmt.Errorf("CLAWBRIDGE_EGRESS_POLICY_DIR %s is not a directory", cfg.PolicyDir)
		}
		if args[0] == "check" {
			_, err := fmt.Fprintln(stdout, "clawbridge-agentd: configuration ok")
			return err
		}
		log.Info("agentd running", "server", cfg.ServerURL, "docker", cfg.Docker)
		return NewReconciler(cfg, NewLauncherClient(cfg.ServerURL, cfg.LauncherToken), log).Run(ctx)
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
	spec, err := loadSpec(getenv)
	if err != nil {
		return err
	}
	p, err := NewProxy(spec, *upstream, log)
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
	srv := proxyServer(p)
	// Requests in flight finish, for up to 5 s, before the proxy exits.
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Info("proxy listening", "listen", *listen, "upstream", *upstream)
	if err := srv.Serve(l); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-stopped
	return nil
}

// egressPoll is how often the egress proxy rereads its policy file.
var egressPoll = 2 * time.Second

func runEgress(ctx context.Context, args []string, getenv func(string) string, log *slog.Logger) error {
	fs := flag.NewFlagSet("egress", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	listen := fs.String("listen", "", "")
	policyPath := fs.String("policy", "", "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *listen == "" || *policyPath == "" {
		return errors.New(usage)
	}
	// Like the socket proxy, it is on every agent's side: no secrets.
	if getenv("CLAWBRIDGE_LAUNCHER_TOKEN") != "" {
		return errors.New("the egress proxy holds no secrets: remove CLAWBRIDGE_LAUNCHER_TOKEN from its environment")
	}
	if _, err := netip.ParseAddrPort(*listen); err != nil {
		return fmt.Errorf("--listen must be IP:PORT, not %q", *listen)
	}
	e := NewEgress(log)
	last, err := os.ReadFile(*policyPath)
	if err != nil {
		return fmt.Errorf("the egress policy could not be read: %w", err)
	}
	p, err := ParseEgressPolicy(last)
	if err == nil {
		err = e.SetPolicy(p)
	}
	if err != nil {
		return err
	}
	l, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	log.Info("egress proxy listening", "listen", *listen, "networks", len(p.Networks))
	go func() {
		ticker := time.NewTicker(egressPoll)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				l.Close()
				_ = e.SetPolicy(EgressPolicy{})
				return
			case <-ticker.C:
			}
			data, err := os.ReadFile(*policyPath)
			if err == nil && bytes.Equal(data, last) {
				continue
			}
			last = data
			var p EgressPolicy
			if err == nil {
				if p, err = ParseEgressPolicy(data); err == nil {
					err = e.SetPolicy(p)
				}
			}
			if err != nil {
				// Fail closed: an unreadable or invalid policy allows nothing.
				_ = e.SetPolicy(EgressPolicy{})
				log.Warn("egress policy refused; refusing all egress until it is valid", "error", err.Error())
				continue
			}
			log.Info("egress policy reloaded", "networks", len(p.Networks))
		}
	}()
	return e.Serve(l)
}
