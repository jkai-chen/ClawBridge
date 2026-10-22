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
	"sort"
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

// linearCapabilities opens the personal profile's provider and reports its
// negotiated capabilities. It reads only the advertised tool list.
func linearCapabilities(ctx context.Context, manager *linearconn.Manager, profileID string) (linearprovider.Capabilities, error) {
	profile, err := manager.Status(ctx, profileID)
	if err != nil {
		return linearprovider.Capabilities{}, err
	}
	client, err := manager.HTTPClient(ctx, profileID)
	if err != nil {
		return linearprovider.Capabilities{}, err
	}
	provider, err := openLinearProvider(ctx, client, profile.Access == linearconn.Read)
	if err != nil {
		return linearprovider.Capabilities{}, err
	}
	defer provider.Close()
	return provider.Capabilities(ctx)
}

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
	projects, labels, expectedLabels                                                                                     []string
	team, parent, assignee, relationType, relationAction, labelFilter                                                    string
	clearProject, clearLabels, clearParent, clearAssignee, expectNoLabels                                                bool
	limit                                                                                                                int
	revision                                                                                                             int64
	teamBoard, confirm, noBrowser, tokenStdin, resolveLabels                                                             bool
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
		f.StringVar(&v.project, "linear-project", "", "explicit allowed Linear destination UUID")
		f.Var((*repeatedIDs)(&v.labels), "label", "team label UUID; explicit replacement (repeat up to 100)")
		f.StringVar(&v.parent, "parent", "", "in-scope parent issue UUID")
		f.StringVar(&v.assignee, "assignee", "", "canonical active member UUID of the effective team")
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
		if command == "issues" {
			f.StringVar(&v.labelFilter, "label", "", "verified team-label UUID filter")
		}
	case "labels", "members":
		page()
		f.StringVar(&v.team, "team", "", "required allowed team UUID")
	case "issue", "states":
		profile()
		if command == "issue" {
			f.BoolVar(&v.resolveLabels, "resolve-labels", false, "prove label IDs when Linear returns label names only (extra reads)")
		}
		min, max = 1, 1
	case "link":
		profile()
		f.Var((*repeatedIDs)(&v.teams), "team", "allowed team UUID (repeat up to 20)")
		f.StringVar(&v.defaultTeam, "default-team", "", "default selected team UUID")
		f.Var((*repeatedIDs)(&v.projects), "linear-project", "allowed Linear project UUID (repeat up to 50)")
		f.BoolVar(&v.teamBoard, "team-board", false, "explicit team board without a Linear project")
		f.Int64Var(&v.revision, "expected-revision", -1, "required current mapping revision")
		f.BoolVar(&v.confirm, "confirm", false, "commit the displayed binding preview")
	case "unlink":
		f.Int64Var(&v.revision, "expected-revision", -1, "required current mapping revision")
		f.BoolVar(&v.confirm, "confirm", false, "commit project unlink; retains personal profiles")
	case "create":
		write()
		fields()
		f.StringVar(&v.team, "team", "", "explicit allowed issue team UUID")
	case "update":
		write()
		fields()
		f.BoolVar(&v.clearProject, "clear-project", false, "explicitly clear project (team-board mapping only)")
		f.BoolVar(&v.clearLabels, "clear-labels", false, "explicitly replace labels with an empty set")
		f.Var((*repeatedIDs)(&v.expectedLabels), "expect-label", "label UUID the issue has now; required with --label/--clear-labels (repeat for the complete set)")
		f.BoolVar(&v.expectNoLabels, "expect-no-labels", false, "the issue has no labels now; required instead of --expect-label when it has none")
		f.BoolVar(&v.clearParent, "clear-parent", false, "explicitly clear parent")
		f.BoolVar(&v.clearAssignee, "clear-assignee", false, "explicitly clear assignee")
		min, max = 1, 1
	case "relation":
		write()
		f.StringVar(&v.relationType, "type", "", "blocks, blocked-by or related; one edge only")
		min, max = 2, 2
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
		if len(v.teams) < 1 || len(v.teams) > 20 || !linearprovider.ValidID(v.defaultTeam) || v.teamBoard == (len(v.projects) > 0) {
			return v, nil, errors.New("link requires teams/default and --team-board or --linear-project")
		}
		seen := map[string]bool{}
		for _, id := range v.teams {
			if !linearprovider.ValidID(id) || seen[id] {
				return v, nil, errors.New("invalid or duplicate team UUID")
			}
			seen[id] = true
		}
		if !seen[v.defaultTeam] || len(v.projects) > 50 {
			return v, nil, errors.New("invalid selected project/default team")
		}
		seenProjects := map[string]bool{}
		for _, id := range v.projects {
			if !linearprovider.ValidID(id) || seenProjects[id] {
				return v, nil, errors.New("invalid or duplicate project UUID")
			}
			seenProjects[id] = true
		}
		sort.Strings(v.projects)
		if len(v.projects) == 1 {
			v.project = v.projects[0]
		}
	}
	if (command == "link" || command == "unlink") && (v.revision < 0 || v.revision >= 9007199254740991) {
		return v, nil, errors.New("expected-revision is required")
	}
	if command == "labels" || command == "members" {
		if !linearprovider.ValidID(v.team) {
			return v, nil, errors.New("discovery requires --team UUID")
		}
	}
	if v.supplied["label"] && command == "issues" && !linearprovider.ValidID(v.labelFilter) {
		return v, nil, errors.New("label filter requires UUID")
	}
	if command == "relation" {
		if !validLinearOperation(v.operation) || !linearprovider.ValidID(pos[0]) || !linearprovider.ValidID(pos[1]) || pos[0] == pos[1] || (v.relationType != "blocks" && v.relationType != "blocked-by" && v.relationType != "related") {
			return v, nil, errors.New("relation requires distinct issue UUIDs, supported --type and --operation-id")
		}
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
		for _, pair := range []struct {
			set, clear string
			active     bool
		}{{"linear-project", "clear-project", v.clearProject}, {"label", "clear-labels", v.clearLabels}, {"parent", "clear-parent", v.clearParent}, {"assignee", "clear-assignee", v.clearAssignee}} {
			if v.supplied[pair.set] && pair.active {
				return v, nil, errors.New("set and clear flags conflict")
			}
		}
		for _, value := range []struct{ key, id string }{{"linear-project", v.project}, {"parent", v.parent}, {"assignee", v.assignee}, {"team", v.team}, {"state", v.state}} {
			if v.supplied[value.key] && !linearprovider.ValidID(value.id) {
				return v, nil, errors.New("explicit destination/member/parent/team requires UUID")
			}
		}
		seenLabels := map[string]bool{}
		if len(v.labels) > 100 {
			return v, nil, errors.New("at most 100 labels")
		}
		for _, id := range v.labels {
			if !linearprovider.ValidID(id) || seenLabels[id] {
				return v, nil, errors.New("invalid or duplicate label UUID")
			}
			seenLabels[id] = true
		}
		sort.Strings(v.labels)
		replacing := v.supplied["label"] || v.clearLabels
		expecting := v.supplied["expect-label"] || v.expectNoLabels
		if v.supplied["expect-label"] && v.expectNoLabels {
			return v, nil, errors.New("choose --expect-label or --expect-no-labels")
		}
		if expecting && !replacing {
			return v, nil, errors.New("expected labels apply only with --label or --clear-labels")
		}
		if command == "update" && replacing && !expecting {
			return v, nil, errors.New("label replacement requires --expect-label (the complete set you last read) or --expect-no-labels")
		}
		seenExpected := map[string]bool{}
		if len(v.expectedLabels) > 100 {
			return v, nil, errors.New("at most 100 expected labels")
		}
		for _, id := range v.expectedLabels {
			if !linearprovider.ValidID(id) || seenExpected[id] {
				return v, nil, errors.New("invalid or duplicate expected label UUID")
			}
			seenExpected[id] = true
		}
		sort.Strings(v.expectedLabels)
		if command == "update" {
			changed := v.clearProject || v.clearLabels || v.clearParent || v.clearAssignee
			f.Visit(func(fl *flag.Flag) {
				if fl.Name == "title" || fl.Name == "description" || fl.Name == "description-file" || fl.Name == "state" || fl.Name == "linear-project" || fl.Name == "label" || fl.Name == "parent" || fl.Name == "assignee" {
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
func linearWriteInput(command string, v linearFlags, pos []string) (linearboard.WriteInputV2, error) {
	var writeInput linearboard.WriteInputV2
	var err error
	writeInput = linearboard.WriteInputV2{WriteInput: linearboard.WriteInput{Operation: command, OperationID: v.operation}, TeamID: v.team}
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
			return writeInput, e
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
				return writeInput, err
			}
		}
	}
	if v.supplied["linear-project"] {
		writeInput.Project = linearprovider.Field[*string]{Present: true, Value: &v.project}
	}
	if v.clearProject {
		writeInput.Project = linearprovider.Field[*string]{Present: true}
	}
	if v.supplied["label"] {
		writeInput.Labels = linearprovider.Field[[]string]{Present: true, Value: append([]string{}, v.labels...)}
	}
	if v.clearLabels {
		writeInput.Labels = linearprovider.Field[[]string]{Present: true, Value: []string{}}
	}
	if v.supplied["expect-label"] {
		writeInput.ExpectedLabels = linearprovider.Field[[]string]{Present: true, Value: append([]string{}, v.expectedLabels...)}
	}
	if v.expectNoLabels {
		writeInput.ExpectedLabels = linearprovider.Field[[]string]{Present: true, Value: []string{}}
	}
	if v.supplied["parent"] {
		writeInput.Parent = linearprovider.Field[*string]{Present: true, Value: &v.parent}
	}
	if v.clearParent {
		writeInput.Parent = linearprovider.Field[*string]{Present: true}
	}
	if v.supplied["assignee"] {
		writeInput.Assignee = linearprovider.Field[*string]{Present: true, Value: &v.assignee}
	}
	if v.clearAssignee {
		writeInput.Assignee = linearprovider.Field[*string]{Present: true}
	}
	if err = linearboard.ValidateWriteV2(writeInput); err != nil {
		return writeInput, err
	}
	return writeInput, nil
}

func runLinear(ctx context.Context, flags Config, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || len(args) == 1 && (args[0] == "--help" || args[0] == "-h" || args[0] == "help") {
		_, err := io.WriteString(stdout, "clawbridge linear connect|import|profiles|status|logout|teams|projects|binding|link|unlink|context|issues|issue|states|labels|members|create|update|comment|relation\nUse COMMAND --help for explicit flags. Token import accepts --token-stdin only.\n")
		return err
	}
	command := args[0]
	parseArgs := args[1:]
	relationAction := ""
	if command == "relation" && len(parseArgs) > 0 && parseArgs[0] != "--help" && parseArgs[0] != "-h" {
		relationAction = parseArgs[0]
		if relationAction != "add" && relationAction != "remove" {
			return errors.New("relation requires add or remove")
		}
		parseArgs = parseArgs[1:]
	}
	v, pos, err := parseLinear(command, parseArgs, stderr)
	v.relationAction = relationAction
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	var writeInput linearboard.WriteInputV2
	if command == "create" || command == "update" || command == "comment" {
		writeInput, err = linearWriteInput(command, v, pos)
		if err != nil {
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
		// Capability state is best effort: an unreachable provider is
		// reported, never turned into a failed status.
		status := struct {
			linearconn.Profile
			Capabilities      *linearprovider.Capabilities `json:"capabilities,omitempty"`
			CapabilitiesError string                       `json:"capabilities_error,omitempty"`
		}{Profile: profile}
		if caps, e := linearCapabilities(bounded, manager, v.profile); e != nil {
			status.CapabilitiesError = "capabilities unavailable: " + linearCapabilityCode(e)
		} else {
			status.Capabilities = &caps
		}
		return emit(status)
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
		data, err = service.BindingV2(bounded, scope)
	case "link":
		data, err = service.LinkV2(bounded, scope, linearboard.LinkInputV2{TeamIDs: v.teams, DefaultTeamID: v.defaultTeam, ProjectIDs: append([]string{}, v.projects...), ExpectedRevision: v.revision, TeamBoard: v.teamBoard, Confirm: v.confirm})
	case "unlink":
		data, err = service.UnlinkV2(bounded, scope, v.revision, v.confirm)
	case "context":
		data, err = service.ContextV2(bounded, scope)
	case "issues":
		data, err = service.IssuesV2(bounded, scope, linearboard.IssuesInputV2{Limit: v.limit, Cursor: v.cursor, LabelID: v.labelFilter})
	case "issue":
		if v.resolveLabels {
			data, err = service.IssueV2ResolvedLabels(bounded, scope, pos[0])
		} else {
			data, err = service.IssueV2(bounded, scope, pos[0])
		}
	case "states":
		data, err = service.StatesV2(bounded, scope, pos[0])
	case "labels":
		data, err = service.LabelsV2(bounded, scope, v.team, v.limit, v.cursor)
	case "members":
		data, err = service.MembersV2(bounded, scope, v.team, v.limit, v.cursor)
	case "relation":
		data, err = service.RelationV2(bounded, scope, linearboard.RelationInputV2{OperationID: v.operation, Action: v.relationAction, Type: v.relationType, FromID: pos[0], ToID: pos[1]})
	default:
		data, err = service.WriteV2(bounded, scope, writeInput)
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

// linearCapabilityCode renders a provider failure without its text, which may
// carry transport detail.
func linearCapabilityCode(err error) string {
	var coded interface{ Code() string }
	if errors.As(err, &coded) {
		return coded.Code()
	}
	return "unavailable"
}
