// Package faketool lets a test place a real executable where a command-line
// tool is expected, on every operating system: it copies the running test
// binary under the tool's name, and that copy, started with the behaviour in
// its environment, acts as the tool instead of running tests. Shell-script
// fakes do not run on Windows, which needs a real .exe on PATH.
package faketool

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

const env = "CLAWBRIDGE_FAKE_TOOL"

// LogEnv names the file the record behaviours write the argument list to.
const LogEnv = "CLAWBRIDGE_FAKE_TOOL_LOG"

// Main runs the tool behaviour when this binary was started as a fake tool,
// and the package's tests otherwise. Call it from TestMain.
func Main(m *testing.M) {
	if behaviour, ok := os.LookupEnv(env); ok {
		os.Exit(run(behaviour, os.Args[1:], os.Stdout))
	}
	os.Exit(m.Run())
}

// Install copies the test binary into dir as name (name.exe on Windows) and
// sets the behaviour every fake started by this test follows:
//
//	args        print each argument on its own line
//	print:TEXT  print TEXT without a newline
//	exit:N      exit with status N
//	sleep:D     stay running for duration D, then exit 0
//	record      write the arguments, each followed by a NUL, to the file named
//	            by LogEnv; print a line of synthetic output
//	codex-queue as record, but answer "queue --help" as Codex's CLI does
//	append      add one JSON line per run to the file named by LogEnv: the
//	            arguments, standard input and CLAWBRIDGE_NOTICE_TEXT
func Install(t testing.TB, dir, name, behaviour string) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(dir, name)
	if err = copyFile(self, path); err != nil {
		t.Fatal(err)
	}
	t.Setenv(env, behaviour)
	return path
}

// Run is one run of the append behaviour, as a line of its log.
type Run struct {
	Args       []string `json:"args"`
	Stdin      string   `json:"stdin"`
	NoticeText string   `json:"notice_text"`
}

// ReadRuns reads the append behaviour's log: one Run per start, oldest
// first. A missing log is no runs.
func ReadRuns(path string) ([]Run, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var runs []Run
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		var r Run
		if err = json.Unmarshal([]byte(line), &r); err != nil {
			return nil, fmt.Errorf("faketool log line %q: %w", line, err)
		}
		runs = append(runs, r)
	}
	return runs, nil
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0700)
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func run(behaviour string, args []string, out io.Writer) int {
	switch {
	case behaviour == "args":
		for _, a := range args {
			fmt.Fprintln(out, a)
		}
		return 0
	case behaviour == "record" || behaviour == "codex-queue":
		if behaviour == "codex-queue" && len(args) == 2 && args[0] == "queue" && args[1] == "--help" {
			fmt.Fprintln(out, "queue --thread --message")
			return 0
		}
		var log strings.Builder
		for _, a := range args {
			log.WriteString(a + "\x00")
		}
		if os.WriteFile(os.Getenv(LogEnv), []byte(log.String()), 0600) != nil {
			return 124
		}
		fmt.Fprintln(out, "synthetic-host-output-not-for-user")
		return 0
	case behaviour == "append":
		stdin, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
		if err != nil {
			return 124
		}
		line, _ := json.Marshal(Run{Args: append([]string{}, args...), Stdin: string(stdin), NoticeText: os.Getenv("CLAWBRIDGE_NOTICE_TEXT")})
		f, err := os.OpenFile(os.Getenv(LogEnv), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
		if err != nil {
			return 124
		}
		_, err = f.Write(append(line, '\n'))
		if e := f.Close(); err != nil || e != nil {
			return 124
		}
		return 0
	case strings.HasPrefix(behaviour, "print:"):
		fmt.Fprint(out, strings.TrimPrefix(behaviour, "print:"))
		return 0
	case strings.HasPrefix(behaviour, "sleep:"):
		if d, err := time.ParseDuration(strings.TrimPrefix(behaviour, "sleep:")); err == nil {
			time.Sleep(d)
			return 0
		}
	case strings.HasPrefix(behaviour, "exit:"):
		code, err := strconv.Atoi(strings.TrimPrefix(behaviour, "exit:"))
		if err == nil {
			return code
		}
	}
	fmt.Fprintln(os.Stderr, "faketool: unknown behaviour", behaviour)
	return 125
}
