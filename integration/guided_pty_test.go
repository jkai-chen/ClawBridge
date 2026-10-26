//go:build darwin || linux

package integration

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Python supplies a real PTY without adding a production/module dependency.
// All paths stop before credential availability or acquisition.
func TestGuidedStartupPTYBeforeAcquisition(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 required for isolated PTY probe")
	}
	p := newProcessFixture(t)
	for _, kind := range []string{"decline", "invalid-url", "malformed", "incomplete", "symlink", "eof", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, "selected.toml")
			original := "invalid=[\n# private-config-sentinel\n"
			switch kind {
			case "malformed":
				if err := os.WriteFile(target, []byte(original), 0600); err != nil {
					t.Fatal(err)
				}
			case "incomplete":
				original = "mode='cloud'\n"
				if err := os.WriteFile(target, []byte(original), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.WriteFile(filepath.Join(dir, "other.toml"), []byte(original), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("other.toml", target); err != nil {
					t.Fatal(err)
				}
			}
			// A hostile repository must not cause bare startup to select its authority.
			if err := os.WriteFile(filepath.Join(dir, ".clawbridge.toml"), []byte("mode='cloud'\nserver='https://unrequested.invalid'\n"), 0600); err != nil {
				t.Fatal(err)
			}
			// Start's "2" leads to the guided join.
			input := "2\nhttps://selected.invalid\nws\nprj\ndevice\n\n"
			if kind == "invalid-url" {
				input = "2\nhttps://user:unsafe-url-sentinel@selected.invalid\nws\nprj\ndevice\ny\n"
			}
			if kind == "eof" {
				input = "2\n\x04"
			}
			if kind == "cancel" {
				input = "2\n"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, python, "-c", guidedPTYScript, p.binary, dir, target, input, kind)
			cmd.Env = append(os.Environ(), "CLAWBRIDGE_MODE=cloud", "CLAWBRIDGE_SERVER=https://unrequested.invalid", "CLAWBRIDGE_CREDENTIAL_SOURCE=gcloud")
			raw, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("PTY harness failed: %v: %s", err, raw)
			}
			var got struct {
				Code   int
				Output string
			}
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			if kind == "decline" {
				if got.Code != 0 || !strings.Contains(got.Output, "Canceled; no credentials") || !strings.Contains(got.Output, "Service: https://selected.invalid") {
					t.Fatalf("decline: %+v", got)
				}
			} else if got.Code == 0 {
				t.Fatalf("invalid target accepted: %+v", got)
			}
			if strings.Contains(got.Output, "unsafe-url-sentinel") || strings.Contains(got.Output, "private-config-sentinel") {
				t.Fatal("private input reflected")
			}
			if kind == "cancel" && !strings.Contains(got.Output, "context canceled") {
				t.Fatalf("signal cancellation not propagated: %+v", got)
			}
			if kind == "decline" || kind == "invalid-url" || kind == "eof" || kind == "cancel" {
				if _, err := os.Lstat(target); !os.IsNotExist(err) {
					t.Fatal("config created before acquisition")
				}
			} else if data, err := os.ReadFile(target); err != nil || string(data) != original {
				t.Fatal("existing target changed")
			}
			if _, err := os.Stat(filepath.Join(dir, "state")); !os.IsNotExist(err) {
				t.Fatal("state opened before acquisition")
			}
		})
	}
}

const guidedPTYScript = `
import os,pty,subprocess,sys,termios,select,time,json,signal
binary,directory,target,answer,action=sys.argv[1:]
master,slave=pty.openpty()
attrs=termios.tcgetattr(slave); attrs[3]&=~termios.ECHO; termios.tcsetattr(slave,termios.TCSANOW,attrs)
env=dict(os.environ,HOME=directory,PATH=directory,TERM='xterm',CLAWBRIDGE_HOME=directory+'/state',CLAWBRIDGE_CONFIG=directory+'/ignored.toml')
p=subprocess.Popen([binary,'--client-config',target],stdin=slave,stdout=slave,stderr=slave,cwd=directory,env=env)
os.close(slave); os.write(master,answer.encode()); output=b''; deadline=time.monotonic()+5
try:
 while True:
  if time.monotonic()>deadline: raise RuntimeError('terminal command blocked')
  ready,_,_=select.select([master],[],[],0.05)
  if ready:
   try: chunk=os.read(master,65536)
   except OSError: break
   if not chunk: break
   output+=chunk
   if action=='cancel' and b'Service HTTPS URL: ' in output:
    p.send_signal(signal.SIGINT); action='canceled'
  elif p.poll() is not None: break
 print(json.dumps(dict(Code=p.wait(timeout=1),Output=output.decode(errors='replace'))))
finally:
 if p.poll() is None: p.kill(); p.wait()
 os.close(master)
`
