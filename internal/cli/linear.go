package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearboard"
	"clawbridge/internal/linearconn"
	"clawbridge/internal/linearprovider"
	"clawbridge/internal/remote"
)

var newLinearManager = func(directory string) *linearconn.Manager {
	return linearconn.New(linearconn.Config{Directory: directory, Verify: func(ctx context.Context, h *http.Client, a linearconn.Access) (linearconn.Identity, error) {
		p, err := linearprovider.New(ctx, h, a == linearconn.Read)
		if err != nil {
			return linearconn.Identity{}, err
		}
		defer p.Close()
		if err = p.VerifyAccess(ctx, a == linearconn.Write); err != nil {
			return linearconn.Identity{}, err
		}
		return linearconn.Identity{}, nil
	}})
}
var openLinearProvider = linearprovider.New
var openLinearBrowser = func(ctx context.Context, url string) error {
	var command string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		command = "open"
		args = []string{url}
	case "windows":
		command = "rundll32"
		args = []string{"url.dll,FileProtocolHandler", url}
	default:
		command = "xdg-open"
		args = []string{url}
	}
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if exec.CommandContext(bounded, command, args...).Run() != nil {
		return errors.New("cannot open browser; reconnect with --no-browser and arrange the loopback route")
	}
	return nil
}

type linearFlags struct {
	supplied                                                                                                             map[string]bool
	profile, name, access, cursor, project, defaultTeam, title, description, descriptionFile, state, operation, bodyFile string
	teams                                                                                                                []string
	limit                                                                                                                int
	revision                                                                                                             int64
	teamBoard, confirm, noBrowser, tokenStdin                                                                            bool
}
type repeatedIDs []string

func (v *repeatedIDs) String() string     { return strings.Join(*v, ",") }
func (v *repeatedIDs) Set(s string) error { *v = append(*v, s); return nil }
func linearDirectory() (string, error) {
	home, err := stateHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "linear"), nil
}
func parseLinear(command string, args []string, stderr io.Writer) (linearFlags, []string, error) {
	v := linearFlags{limit: 50, access: "read", revision: -1}
	f := flag.NewFlagSet("linear "+command, flag.ContinueOnError)
	f.SetOutput(stderr)
	profile := func() { f.StringVar(&v.profile, "profile", "", "explicit personal profile ID") }
	page := func() {
		profile()
		f.IntVar(&v.limit, "limit", 50, "page size (1–100)")
		f.StringVar(&v.cursor, "cursor", "", "opaque next cursor")
	}
	write := func() { profile(); f.StringVar(&v.operation, "operation-id", "", "required durable write identifier") }
	fields := func() {
		f.StringVar(&v.title, "title", "", "explicit title")
		f.StringVar(&v.description, "description", "", "explicit description")
		f.StringVar(&v.descriptionFile, "description-file", "", "bounded UTF-8 description file")
		f.StringVar(&v.state, "state", "", "workflow state UUID")
	}
	min, max := 0, 0
	switch command {
	case "connect", "login":
		profile()
		f.StringVar(&v.name, "name", "", "new personal connection label")
		f.StringVar(&v.access, "access", "read", "read or write (explicit elevation)")
		f.BoolVar(&v.noBrowser, "no-browser", false, "print OAuth URL without opening a browser")
	case "import":
		f.StringVar(&v.name, "name", "", "new personal connection label")
		f.StringVar(&v.access, "access", "read", "read or write")
		f.BoolVar(&v.tokenStdin, "token-stdin", false, "read imported token only from stdin")
	case "profiles", "binding":
	case "status", "logout", "context":
		profile()
	case "teams", "projects", "issues":
		page()
	case "issue", "states":
		profile()
		min, max = 1, 1
	case "link":
		profile()
		f.Var((*repeatedIDs)(&v.teams), "team", "allowed team UUID (repeat up to20)")
		f.StringVar(&v.defaultTeam, "default-team", "", "default selected team UUID")
		f.StringVar(&v.project, "linear-project", "", "optional Linear project UUID")
		f.BoolVar(&v.teamBoard, "team-board", false, "explicit team board without a Linear project")
		f.Int64Var(&v.revision, "expected-revision", -1, "required current mapping revision")
		f.BoolVar(&v.confirm, "confirm", false, "commit the displayed binding preview")
	case "unlink":
		f.Int64Var(&v.revision, "expected-revision", -1, "required current mapping revision")
		f.BoolVar(&v.confirm, "confirm", false, "commit project unlink; retains personal profiles")
	case "create":
		write()
		fields()
	case "update":
		write()
		fields()
		min, max = 1, 1
	case "comment":
		write()
		f.StringVar(&v.bodyFile, "body-file", "", "bounded UTF-8 comment file")
		min, max = 1, 2
	default:
		return v, nil, errors.New("unknown Linear command; use linear --help")
	}
	ordered, err := interspersedFlags(f, args)
	if err != nil {
		return v, nil, err
	}
	if err = f.Parse(ordered); err != nil {
		return v, nil, err
	}
	v.supplied = map[string]bool{}
	f.Visit(func(fl *flag.Flag) { v.supplied[fl.Name] = true })
	pos := f.Args()
	if len(pos) < min || len(pos) > max {
		return v, nil, errors.New("invalid Linear positional arguments")
	}
	needsProfile := command != "profiles" && command != "connect" && command != "login" && command != "import" && command != "binding" && command != "unlink"
	if needsProfile && !validLinearProfile(v.profile) {
		return v, nil, errors.New("select --profile with the opaque ID from linear profiles")
	}
	if (command == "connect" || command == "login" || command == "import") && (v.access != "read" && v.access != "write" || strings.TrimSpace(v.name) == "" || len(v.name) > 128) {
		return v, nil, errors.New("connection requires --name and read or write access")
	}
	if command == "import" && !v.tokenStdin {
		return v, nil, errors.New("import requires --token-stdin; tokens are never accepted in argv")
	}
	if v.profile != "" && !validLinearProfile(v.profile) {
		return v, nil, errors.New("invalid personal profile ID")
	}
	if v.limit < 1 || v.limit > 100 || len(v.cursor) > 8192 {
		return v, nil, errors.New("invalid page limit or cursor")
	}
	if command == "link" {
		if len(v.teams) < 1 || len(v.teams) > 20 || !linearprovider.ValidID(v.defaultTeam) || v.teamBoard == (v.project != "") {
			return v, nil, errors.New("link requires teams/default and --team-board or --linear-project")
		}
		seen := map[string]bool{}
		for _, id := range v.teams {
			if !linearprovider.ValidID(id) || seen[id] {
				return v, nil, errors.New("invalid or duplicate team UUID")
			}
			seen[id] = true
		}
		if !seen[v.defaultTeam] || v.project != "" && !linearprovider.ValidID(v.project) {
			return v, nil, errors.New("invalid selected project/default team")
		}
	}
	if (command == "link" || command == "unlink") && (v.revision < 0 || v.revision >= 9007199254740991) {
		return v, nil, errors.New("expected-revision is required")
	}
	if command == "create" || command == "update" || command == "comment" {
		if !validLinearOperation(v.operation) {
			return v, nil, errors.New("write requires a bounded --operation-id")
		}
		if command == "create" && strings.TrimSpace(v.title) == "" {
			return v, nil, errors.New("create requires --title")
		}
		if v.supplied["description"] && v.supplied["description-file"] {
			return v, nil, errors.New("choose description text or file")
		}
		if command == "comment" && ((len(pos) == 2) == (v.bodyFile != "")) {
			return v, nil, errors.New("comment requires issue UUID and one body or --body-file")
		}
		if len(v.title) > 512 || len(v.description) > 65536 || v.state != "" && !linearprovider.ValidID(v.state) {
			return v, nil, errors.New("invalid task field bounds")
		}
		if command == "update" {
			changed := false
			f.Visit(func(fl *flag.Flag) {
				if fl.Name == "title" || fl.Name == "description" || fl.Name == "description-file" || fl.Name == "state" {
					changed = true
				}
			})
			if !changed {
				return v, nil, errors.New("update requires explicit changed fields")
			}
		}
	}
	if min > 0 && !linearprovider.ValidID(pos[0]) {
		return v, nil, errors.New("issue requires its immutable UUID")
	}
	return v, pos, nil
}
func validLinearProfile(s string) bool {
	if len(s) < 1 || len(s) > 128 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._-", c)) {
			return false
		}
	}
	return true
}
func validLinearOperation(s string) bool { return validLinearProfile(s) }
func readLinearFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 65536 {
		return "", errors.New("task body requires a bounded regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", errors.New("cannot read bounded task body file")
	}
	defer f.Close()
	opened, e := f.Stat()
	if e != nil || !os.SameFile(info, opened) {
		return "", errors.New("task body file changed")
	}
	data, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil || len(data) > 65536 {
		return "", errors.New("task body file exceeds bound")
	}
	return string(data), nil
}
func linearExecutor(ctx context.Context, flags Config) (linearboard.Executor, func(), bus.Request, error) {
	c, _, err := resolveConfig(ctx, flags)
	if err != nil {
		return nil, nil, bus.Request{}, err
	}
	if c.Workspace == "" || c.Project == "" {
		return nil, nil, bus.Request{}, errors.New("task-board actions require explicit existing ClawBridge workspace/project")
	}
	var executor interface {
		Execute(context.Context, bus.Request) (bus.Result, error)
		ResolveProject(context.Context, string, string, string) (string, string, error)
	}
	cleanup := func() {}
	if c.Mode == "cloud" {
		executor, err = remote.New(remoteConfig(c))
	} else {
		var local *bus.Store
		local, err = bus.Open(c.DB)
		executor = local
		if err == nil {
			cleanup = func() { local.Close() }
		}
	}
	if err != nil {
		return nil, nil, bus.Request{}, err
	}
	workspace, project, err := executor.ResolveProject(ctx, c.Workspace, c.Project, "")
	if err != nil {
		cleanup()
		return nil, nil, bus.Request{}, err
	}
	scope := bus.Request{Workspace: workspace, Project: project, Session: c.Session, Args: map[string]any{"linear_authority": linearAuthority(c)}}
	if c.Instance != "" {
		scope.Args["instance"] = c.Instance
	}
	return executor, cleanup, scope, nil
}
func newLinearService(directory, profile string, manager *linearconn.Manager, executor linearboard.Executor, authority ...string) *linearboard.Service {
	route := ""
	if len(authority) > 0 {
		route = authority[0]
	}
	return linearboard.New(linearboard.Config{Authority: route, Executor: executor, Connections: manager, ProfileID: profile, LedgerDirectory: filepath.Join(directory, "ledger"), Open: func(ctx context.Context, h *http.Client, ro bool) (linearboard.Provider, error) {
		return openLinearProvider(ctx, h, ro)
	}})
}
func runLinear(ctx context.Context, flags Config, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || len(args) == 1 && (args[0] == "--help" || args[0] == "-h" || args[0] == "help") {
		_, err := io.WriteString(stdout, "clawbridge linear connect|import|profiles|status|logout|teams|projects|binding|link|unlink|context|issues|issue|states|create|update|comment\nUse COMMAND --help for explicit flags. Token import accepts --token-stdin only.\n")
		return err
	}
	command := args[0]
	v, pos, err := parseLinear(command, args[1:], stderr)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	var writeInput linearboard.WriteInput
	if command == "create" || command == "update" || command == "comment" {
		writeInput = linearboard.WriteInput{Operation: command, OperationID: v.operation}
		if len(pos) > 0 {
			writeInput.IssueID = pos[0]
		}
		if v.supplied["title"] {
			writeInput.Title = &v.title
		}
		if v.supplied["description"] {
			writeInput.Description = &v.description
		}
		if v.descriptionFile != "" {
			text, e := readLinearFile(v.descriptionFile)
			if e != nil {
				return e
			}
			writeInput.Description = &text
		}
		if v.state != "" {
			writeInput.StateID = &v.state
		}
		if command == "comment" {
			if len(pos) == 2 {
				writeInput.Body = pos[1]
			} else {
				writeInput.Body, err = readLinearFile(v.bodyFile)
				if err != nil {
					return err
				}
			}
		}
		if err = linearboard.ValidateWrite(writeInput); err != nil {
			return err
		}
	}
	bounded, cancel := context.WithTimeout(ctx, 45*time.Second)
	if command == "connect" || command == "login" {
		cancel()
		bounded, cancel = context.WithTimeout(ctx, 5*time.Minute)
	}
	defer cancel()
	directory, err := linearDirectory()
	if err != nil {
		return err
	}
	manager := newLinearManager(directory)
	emit := func(data any) error { return json.NewEncoder(stdout).Encode(bus.Result{Source: "linear", Data: data}) }
	switch command {
	case "connect", "login":
		profile, e := manager.Connect(bounded, linearconn.ConnectOptions{Name: v.name, ProfileID: v.profile, Access: linearconn.Access(v.access), NoBrowser: v.noBrowser, PresentURL: func(url string) error {
			if v.noBrowser {
				_, e := fmt.Fprintln(stderr, url)
				return e
			}
			return openLinearBrowser(bounded, url)
		}})
		if e != nil {
			return e
		}
		return emit(profile)
	case "import":
		reader, cleanupInput, e := newLinearImportReader(bounded, os.Stdin)
		if e != nil {
			return e
		}
		defer cleanupInput()
		profile, e := manager.Import(bounded, v.name, linearconn.Access(v.access), reader)
		if e != nil {
			return e
		}
		return emit(profile)
	case "profiles":
		profiles, e := manager.Profiles()
		if e != nil {
			return e
		}
		return emit(profiles)
	case "status":
		profile, e := manager.Status(bounded, v.profile)
		if e != nil {
			return e
		}
		return emit(profile)
	case "logout":
		if e := manager.Logout(bounded, v.profile); e != nil {
			return e
		}
		return emit(map[string]any{"logged_out": true, "profile_id": v.profile})
	case "teams", "projects":
		profile, e := manager.Status(bounded, v.profile)
		if e != nil {
			return e
		}
		httpClient, e := manager.HTTPClient(bounded, v.profile)
		if e != nil {
			return e
		}
		provider, e := openLinearProvider(bounded, httpClient, profile.Access == linearconn.Read)
		if e != nil {
			return e
		}
		defer provider.Close()
		if command == "teams" {
			page, e := provider.Teams(bounded, linearprovider.PageOptions{Limit: v.limit, Cursor: v.cursor})
			if e != nil {
				return e
			}
			return emit(page)
		}
		page, e := provider.Projects(bounded, linearprovider.PageOptions{Limit: v.limit, Cursor: v.cursor})
		if e != nil {
			return e
		}
		return emit(page)
	}
	executor, closeExecutor, scope, err := linearExecutor(bounded, flags)
	if err != nil {
		return err
	}
	defer closeExecutor()
	service := newLinearService(directory, v.profile, manager, executor, scope.Args["linear_authority"].(string))
	var data any
	switch command {
	case "binding":
		data, err = service.Binding(bounded, scope)
	case "link":
		data, err = service.Link(bounded, scope, linearboard.LinkInput{TeamIDs: v.teams, DefaultTeamID: v.defaultTeam, ProjectID: v.project, ExpectedRevision: v.revision, TeamBoard: v.teamBoard, Confirm: v.confirm})
	case "unlink":
		data, err = service.Unlink(bounded, scope, v.revision, v.confirm)
	case "context":
		data, err = service.Context(bounded, scope)
	case "issues":
		data, err = service.Issues(bounded, scope, v.limit, v.cursor)
	case "issue":
		data, err = service.Issue(bounded, scope, pos[0])
	case "states":
		data, err = service.States(bounded, scope, pos[0])
	default:
		data, err = service.Write(bounded, scope, writeInput)
	}
	if err != nil {
		var receipt *linearboard.ReceiptError
		if data != nil && errors.As(err, &receipt) {
			if e := emit(map[string]any{"outcome": receipt.Outcome, "receipt": data}); e != nil {
				return e
			}
		}
		return err
	}
	return emit(data)
}

func linearAuthority(c Config) string {
	var route []string
	if c.Mode == "cloud" {
		server := strings.TrimRight(c.Server, "/")
		if u, e := url.Parse(server); e == nil {
			u.Host = strings.ToLower(u.Host)
			server = u.String()
		}
		route = []string{"cloud", server}
	} else {
		path, _ := filepath.Abs(c.DB)
		if resolved, e := filepath.EvalSymlinks(path); e == nil {
			path = resolved
		}
		route = []string{"local", path}
	}
	raw, _ := json.Marshal(route)
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}
