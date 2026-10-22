// Package tui provides the interactive ClawBridge operator interface.
//
// The model is deliberately independent of the CLI configuration and storage
// implementation. It only sees the narrow Executor boundary and explicit setup
// and doctor adapters. Scope is copied from Config into every authority request
// and cannot be changed through screen navigation.
package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"clawbridge/internal/bus"
	tea "github.com/charmbracelet/bubbletea"
)

// Mode is the explicitly trusted storage mode used by the TUI.
type Mode string

const (
	ModeLocal Mode = "local"
	ModeCloud Mode = "cloud"
)

// Screen identifies an operator view.
type Screen string

const (
	ScreenOverview Screen = "overview"
	ScreenProjects Screen = "projects"
	ScreenSessions Screen = "sessions"
	ScreenInbox    Screen = "inbox"
	ScreenCloud    Screen = "cloud"
	ScreenDoctor   Screen = "doctor"
)

// Short aliases make screen selection read naturally for callers.
const (
	Overview = ScreenOverview
	Projects = ScreenProjects
	Sessions = ScreenSessions
	Inbox    = ScreenInbox
	Cloud    = ScreenCloud
	Doctor   = ScreenDoctor
)

// Executor is the same application boundary used by the CLI and MCP adapter.
// The TUI never opens a store or invokes an operation through another path.
type Executor interface {
	Execute(context.Context, bus.Request) (bus.Result, error)
}

// Status is the stable diagnostic status vocabulary.
type Status string

const (
	Pass Status = "pass"
	Warn Status = "warn"
	Fail Status = "fail"
)

// Check is one diagnostic result. Detail and Fix must be safe to display: the
// doctor implementation is responsible for redacting credentials and tokens.
type Check struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status Status `json:"status"`
	Detail string `json:"detail,omitempty"`
	Fix    string `json:"fix,omitempty"`
}

// DoctorSource supplies diagnostics without coupling the TUI to doctor internals.
type DoctorSource interface {
	Checks(context.Context) ([]Check, error)
}

// DoctorFunc adapts a function to DoctorSource.
type DoctorFunc func(context.Context) ([]Check, error)

func (f DoctorFunc) Checks(ctx context.Context) ([]Check, error) { return f(ctx) }

// Resource is one resource in a setup plan. Change is "create", "update" or
// "retain"; the TUI only displays it and never infers a billing change.
type Resource struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Region  string `json:"region,omitempty"`
	Action  string `json:"action"`
	Monthly string `json:"monthly,omitempty"`
}

// SetupPlan is the reviewable cloud setup proposal. Plan is rendered verbatim.
// Confirmation is the exact phrase the operator must enter before Apply.
type SetupPlan struct {
	Title          string     `json:"title"`
	Plan           string     `json:"plan"`
	Resources      []Resource `json:"resources,omitempty"`
	MonthlyCost    string     `json:"monthly_cost"`
	Confirmation   string     `json:"confirmation"`
	Region         string     `json:"region,omitempty"`
	RetentionDays  int        `json:"retention_days,omitempty"`
	ResidencyNotes string     `json:"residency_notes,omitempty"`
}

// ApplyResult is returned only after an explicitly confirmed setup apply.
type ApplyResult struct {
	Summary string   `json:"summary"`
	Applied []string `json:"applied,omitempty"`
}

// SetupSource is a narrow adapter over the setup package. Plan must be read-only.
// Apply must independently reject a missing or mismatched confirmation phrase.
type SetupSource interface {
	Plan(context.Context) (SetupPlan, error)
	Apply(context.Context, string) (ApplyResult, error)
}

// SetupFuncs adapts functions to SetupSource.
type SetupFuncs struct {
	PlanFunc  func(context.Context) (SetupPlan, error)
	ApplyFunc func(context.Context, string) (ApplyResult, error)
}

func (f SetupFuncs) Plan(ctx context.Context) (SetupPlan, error) {
	if f.PlanFunc == nil {
		return SetupPlan{}, errors.New("setup planning is not configured")
	}
	return f.PlanFunc(ctx)
}

func (f SetupFuncs) Apply(ctx context.Context, confirmation string) (ApplyResult, error) {
	if f.ApplyFunc == nil {
		return ApplyResult{}, errors.New("setup apply is not configured")
	}
	return f.ApplyFunc(ctx, confirmation)
}

// Config contains trusted routing and the adapters assembled by the CLI.
//
// Mode is required. Workspace, Project, Session and Instance are immutable for
// the lifetime of the Model. Local mode expects the caller to have selected an
// already-opened local executor; TUI never creates one. Cloud mode expects an
// authenticated remote executor or compatible adapter.
type Config struct {
	Linear        LinearSource
	InitialScreen Screen
	CloudInfo     string
	Executor      Executor
	Doctor        DoctorSource
	Setup         SetupSource
	// AllowSetupOnly permits a fresh cloud install to browse the Cloud screen
	// before an authority executor or workspace scope exists. Any authority-backed
	// view still fails with an actionable configuration error.
	AllowSetupOnly bool

	Mode       Mode
	Workspace  string
	Project    string
	Session    string
	Instance   string
	Server     string
	Audience   string
	Principal  string
	Provenance string

	Context context.Context
	Now     func() time.Time
}

// WorkspaceView is the trusted workspace scope shown in the header.
type WorkspaceView struct {
	ID             string `json:"id,omitempty"`
	Name           string `json:"name,omitempty"`
	Mode           string `json:"mode,omitempty"`
	RetentionDays  int    `json:"retention_days,omitempty"`
	OwnerPrincipal string `json:"owner_principal,omitempty"`
}

// ProjectView is the trusted project scope shown in the header.
type ProjectView struct {
	ID        string `json:"id,omitempty"`
	Workspace string `json:"workspace_id,omitempty"`
	Slug      string `json:"slug,omitempty"`
	Name      string `json:"name,omitempty"`
	GitRemote string `json:"git_remote,omitempty"`
}

// SessionView identifies the bound actor without exposing peer instances.
type SessionView struct {
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Role      string `json:"role,omitempty"`
	AgentKind string `json:"agent_kind,omitempty"`
	Status    string `json:"status,omitempty"`
}

// IdentityView records application identity provenance returned by whoami.
type IdentityView struct {
	Principal  string `json:"principal,omitempty"`
	Provenance string `json:"identity_provenance,omitempty"`
	Storage    string `json:"storage_mode,omitempty"`
	Verified   bool   `json:"verified"`
	Online     bool   `json:"online"`
}

// ProjectRow is the display-safe project listing shape.
type ProjectRow struct {
	ID      string `json:"id"`
	Slug    string `json:"slug"`
	Name    string `json:"name"`
	Role    string `json:"role,omitempty"`
	GitRepo string `json:"git_remote,omitempty"`
}

// SessionRow is the display-safe session listing shape.
type SessionRow struct {
	UpdatedAt string `json:"updated_at,omitempty"`
	ID        string `json:"id"`
	Name      string `json:"name"`
	Owner     string `json:"owner_principal,omitempty"`
	Host      string `json:"host,omitempty"`
	Account   string `json:"account,omitempty"`
	Role      string `json:"role,omitempty"`
	AgentKind string `json:"agent_kind,omitempty"`
	Status    string `json:"status,omitempty"`
	Presence  string `json:"presence,omitempty"`
	Stale     bool   `json:"stale"`
}

// MessageRow is one inbox item. Delivery and read stamps come from the server.
type MessageRow struct {
	ID          string `json:"id"`
	Sequence    int64  `json:"sequence,omitempty"`
	Sender      string `json:"sender"`
	ThreadID    string `json:"thread_id,omitempty"`
	ReplyTo     string `json:"reply_to,omitempty"`
	Kind        string `json:"kind,omitempty"`
	Body        string `json:"body"`
	CreatedAt   string `json:"created_at,omitempty"`
	DeliveredAt string `json:"delivered_at,omitempty"`
	ReadAt      string `json:"read_at,omitempty"`
}

// Freshness describes an authoritative response or a read-only mirror view.
type Freshness struct {
	UpdatedAt  string `json:"updated_at,omitempty"`
	AgeSeconds int64  `json:"age_seconds,omitempty"`
	Stale      bool   `json:"stale"`
}

// Snapshot is a stable, renderable copy of the current model state.
type Snapshot struct {
	Filter       ArchiveFilter
	Page         int
	HasMore      bool
	Cursor       int64
	ScrollOffset int
	Screen       Screen
	Mode         Mode
	Loading      bool
	Error        string

	Source     string
	Workspace  WorkspaceView
	Project    ProjectView
	Session    SessionView
	Identity   IdentityView
	Freshness  Freshness
	Projects   []ProjectRow
	Sessions   []SessionRow
	Messages   []MessageRow
	Plan       *SetupPlan
	Checks     []Check
	Apply      *ApplyResult
	Confirming bool
	Selected   int
}

// Model implements tea.Model and can be tested directly with UpdateModel.
type Model struct {
	linear       linearState
	linearCancel context.CancelFunc
	cfg          Config

	screen Screen
	width  int
	height int

	tickStarted bool
	loading     bool
	errText     string
	source      string
	fresh       Freshness
	online      *bool

	workspace WorkspaceView
	project   ProjectView
	session   SessionView
	identity  IdentityView

	projects []ProjectRow
	sessions []SessionRow
	messages []MessageRow
	plan     *SetupPlan
	checks   []Check
	apply    *ApplyResult

	selected     int
	rawSessions  []SessionRow
	rawMessages  []MessageRow
	filter       ArchiveFilter
	classifiedAt time.Time
	pages        []int64
	pageIndex    int
	pageRollback *pagingState
	nextCursor   int64
	hasMore      bool
	scroll       int
	scrollManual bool
	request      uint64

	inputMode   inputMode
	draft       string
	replyTarget string
	confirming  bool
	confirm     string
	actionErr   string
}

type inputMode uint8

const (
	inputNone inputMode = iota
	inputReply
	inputSend
	inputLinearName
)

// New validates the trusted mode and immutable boundary.
func New(cfg Config) (Model, error) {
	if cfg.Executor == nil && !(cfg.AllowSetupOnly && cfg.Setup != nil && cfg.Mode == ModeCloud) {
		return Model{}, errors.New("TUI requires an application executor")
	}
	switch cfg.Mode {
	case ModeLocal, ModeCloud:
	default:
		return Model{}, errors.New("TUI requires an explicit local or cloud mode")
	}
	if cfg.Mode == ModeCloud {
		if _, local := cfg.Executor.(*bus.Store); local {
			return Model{}, errors.New("cloud TUI requires the authenticated remote executor; refusing a local store")
		}
	}
	if cfg.Context == nil {
		cfg.Context = context.Background()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	m := Model{cfg: cfg, screen: ScreenOverview, width: 100, height: 30, pages: []int64{0}, classifiedAt: cfg.Now()}
	if cfg.InitialScreen != "" {
		valid := false
		for _, screen := range orderedScreens {
			if screen == cfg.InitialScreen {
				valid = true
			}
		}
		if !valid {
			return Model{}, errors.New("invalid initial TUI screen")
		}
		m.screen = cfg.InitialScreen
	}
	m.workspace = WorkspaceView{Name: cfg.Workspace}
	m.project = ProjectView{Slug: cfg.Project}
	m.session = SessionView{Name: cfg.Session}
	m.identity = IdentityView{Principal: cfg.Principal, Provenance: cfg.Provenance}
	if cfg.Executor == nil {
		m.screen = ScreenCloud
	}
	return m, nil
}

// NewModel is an alias for New.
func NewModel(cfg Config) (Model, error) { return New(cfg) }

// Init loads the first view without changing authority.
func (m Model) Init() tea.Cmd {
	if m.screen != ScreenOverview {
		_, cmd := m.refreshView()
		return cmd
	}
	screen, request := m.screen, m.request
	return func() tea.Msg { return m.loadOverviewFor(screen, request) }
}

// Update implements tea.Model.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	next, cmd := m.UpdateModel(msg)
	return next, cmd
}

// UpdateModel is the concrete counterpart to Update for tests and embedders.
func (m Model) UpdateModel(msg tea.Msg) (Model, tea.Cmd) {
	switch x := msg.(type) {
	case linearLoadedMsg:
		return m.applyLinear(x), nil
	case ArchiveTickMsg:
		m = m.reclassify(x.Now)
		return m, archiveTick()
	case tea.WindowSizeMsg:
		m.width, m.height = x.Width, x.Height
		return m, nil
	case tea.KeyMsg:
		return m.updateKey(x)
	case loadedMsg:
		if x.request != m.request || x.screen != m.screen {
			return m, nil
		}
		m.loading = false
		m.errText = ""
		m.actionErr = ""
		if x.err != nil {
			m = m.restorePage()
			m.errText = displayError(x.err)
			if x.kind == loadProjects {
				m.errText = projectError(x.err)
			}
			return m, nil
		}
		m.pageRollback = nil
		(&m).applyLoad(x)
		if !m.tickStarted {
			m.tickStarted = true
			return m, archiveTick()
		}
		return m, nil
	case actionMsg:
		if x.request != m.request {
			return m, nil
		}
		m.loading = false
		m.actionErr = ""
		if x.err != nil {
			m.actionErr = displayError(x.err)
			return m, nil
		}
		switch x.kind {
		case actionReply, actionSend, actionRead:
			m.source = x.result.Source
			if x.kind == actionRead || x.kind == actionReply || x.kind == actionSend {
				m.inputMode = inputNone
				m.draft = ""
				_, cmd := m.reload()
				return m, cmd
			}
		case actionApply:
			m.apply = x.applied
			m.source = "setup"
			m.confirming = false
			m.confirm = ""
		}
		return m, nil
	}
	return m, nil
}

// View implements tea.Model.
func (m Model) View() string { return Render(m) }

// CurrentScreen returns the selected view.
func (m Model) CurrentScreen() Screen { return m.screen }

// State returns an immutable display snapshot.
func (m Model) State() Snapshot { return m.snapshot() }

// SetSize sets the available drawable area.
func (m Model) SetSize(width, height int) Model {
	if width > 0 {
		m.width = width
	}
	if height > 0 {
		m.height = height
	}
	return m
}

// Show selects a screen and starts its read-only refresh. Screen selection never
// changes trusted workspace, project, session or instance.
func (m Model) Show(screen Screen) (Model, tea.Cmd) {
	if m.linearCancel != nil {
		m.linearCancel()
		m.linearCancel = nil
	}
	m.loading = false
	if _, ok := screenOrder[screen]; !ok {
		m.actionErr = "unknown screen"
		return m, nil
	}
	m.screen = screen
	m.pageRollback = nil
	m.pages = []int64{0}
	m.pageIndex = 0
	m.scroll = 0
	m.scrollManual = false
	m = m.reclassify(m.cfg.Now())
	m.selected = 0
	m.inputMode = inputNone
	m.confirming = false
	m.confirm = ""
	m.actionErr = ""
	return m.refresh()
}

// Reload refreshes the selected screen through the configured executor or
// read-only adapter.
func (m Model) Reload() (Model, tea.Cmd) {
	m = m.stagePage()
	m.pages = []int64{0}
	m.pageIndex = 0
	m.scroll = 0
	m.scrollManual = false
	return m.refresh()
}

func (m Model) reload() (Model, tea.Cmd) { return m.refresh() }

// Select moves the current list selection while keeping it in range.
func (m Model) Select(index int) Model {
	m.scroll = 0
	m.scrollManual = false
	n := len(m.currentRows())
	if n == 0 {
		m.selected = 0
	} else if index < 0 {
		m.selected = 0
	} else if index >= n {
		m.selected = n - 1
	} else {
		m.selected = index
	}
	return m
}

// Move changes the selection by delta, wrapping at the ends.
func (m Model) Move(delta int) Model {
	m.scroll = 0
	m.scrollManual = false
	n := len(m.currentRows())
	if n == 0 {
		m.selected = 0
		return m
	}
	m.selected = (m.selected + delta%n + n) % n
	return m
}

// SelectedMessage returns the selected inbox message.
func (m Model) SelectedMessage() (MessageRow, bool) {
	if m.selected < 0 || m.selected >= len(m.messages) {
		return MessageRow{}, false
	}
	return m.messages[m.selected], true
}

// BeginReply opens an explicit reply draft for the selected inbox message.
func (m Model) BeginReply() Model {
	if m.cfg.Session == "" || m.cfg.Instance == "" {
		m.actionErr = "this action requires a bound session and matching instance; use explicit --session and --instance"
		return m
	}

	selected, ok := m.SelectedMessage()
	if !ok {
		m.actionErr = "select an inbox message before replying"
		return m
	}
	if m.writesBlocked() {
		m.actionErr = "the authority is offline; replies are not queued and remain unavailable until reconnect"
		return m
	}
	m.replyTarget = selected.ID
	m.inputMode = inputReply
	m.draft = ""
	m.actionErr = ""
	return m
}

// BeginSend opens an explicit broadcast-to-peer send draft. The recipient is
// entered before the body as "session|body" when submitted by the TUI.
func (m Model) BeginSend() Model {
	if m.cfg.Session == "" || m.cfg.Instance == "" {
		m.actionErr = "this action requires a bound session and matching instance; use explicit --session and --instance"
		return m
	}

	if m.writesBlocked() {
		m.actionErr = "the authority is offline; sends are not queued and remain unavailable until reconnect"
		return m
	}
	m.inputMode = inputSend
	m.draft = ""
	m.actionErr = ""
	return m
}

// CancelInput abandons a draft without writing.
func (m Model) CancelInput() Model {
	m.replyTarget = ""
	m.inputMode = inputNone
	m.draft = ""
	m.confirming = false
	m.confirm = ""
	return m
}

// Reply is an explicit mutation. It is never invoked by navigation or refresh.
func (m Model) Reply(messageID, body, operationID string) (Model, tea.Cmd) {
	if m.cfg.Session == "" || m.cfg.Instance == "" {
		m.actionErr = "this action requires a bound session and matching instance; use explicit --session and --instance"
		return m, nil
	}

	if messageID == "" || strings.TrimSpace(body) == "" || operationID == "" {
		m.actionErr = "reply requires a selected message, nonempty body and operation ID"
		return m, nil
	}
	if m.writesBlocked() {
		m.actionErr = "authority is offline; reply was not attempted or queued"
		return m, nil
	}
	m.request++
	m.loading = true
	req := m.requestWithInstance("reply", map[string]any{
		"message_id":   messageID,
		"body":         body,
		"operation_id": operationID,
	})
	return m, m.command(m.request, actionReply, req)
}

// Send is an explicit mutation. operationID is required for safe idempotent retry.
func (m Model) Send(to, body, operationID string) (Model, tea.Cmd) {
	if m.cfg.Session == "" || m.cfg.Instance == "" {
		m.actionErr = "this action requires a bound session and matching instance; use explicit --session and --instance"
		return m, nil
	}

	if strings.TrimSpace(to) == "" || strings.TrimSpace(body) == "" || operationID == "" {
		m.actionErr = "send requires a recipient, nonempty body and operation ID"
		return m, nil
	}
	if m.writesBlocked() {
		m.actionErr = "authority is offline; send was not attempted or queued"
		return m, nil
	}
	m.request++
	m.loading = true
	req := m.requestWithInstance("send", map[string]any{
		"to":           to,
		"body":         body,
		"operation_id": operationID,
	})
	return m, m.command(m.request, actionSend, req)
}

// MarkRead is an explicit mutation. Offline mirror reads never become writes.
func (m Model) MarkRead(ids []string) (Model, tea.Cmd) {
	if m.cfg.Session == "" || m.cfg.Instance == "" {
		m.actionErr = "this action requires a bound session and matching instance; use explicit --session and --instance"
		return m, nil
	}

	if len(ids) == 0 {
		m.actionErr = "select at least one message to mark read"
		return m, nil
	}
	if m.writesBlocked() {
		m.actionErr = "authority is offline; mark-read was not attempted or queued"
		return m, nil
	}
	m.request++
	m.loading = true
	req := m.requestWithInstance("mark_read", map[string]any{"ids": append([]string(nil), ids...)})
	return m, m.command(m.request, actionRead, req)
}

// ApplySetup runs setup only after the exact plan phrase has been entered by a
// human. The adapter also receives the phrase so a caller cannot bypass the gate.
func (m Model) ApplySetup(confirmation string) (Model, tea.Cmd) {
	if m.cfg.Mode != ModeCloud {
		m.actionErr = "setup apply is available only in explicitly configured cloud mode"
		return m, nil
	}
	if m.cfg.Setup == nil {
		m.actionErr = "setup adapter is not configured"
		return m, nil
	}
	if m.plan == nil {
		m.actionErr = "load and review the exact setup plan before applying"
		return m, nil
	}
	if strings.TrimSpace(m.plan.Plan) == "" || strings.TrimSpace(m.plan.Confirmation) == "" || strings.TrimSpace(m.plan.MonthlyCost) == "" {
		m.actionErr = "setup plan must show resources and cost before apply"
		return m, nil
	}
	if confirmation == "" || confirmation != m.plan.Confirmation {
		m.actionErr = "confirmation phrase does not match the displayed plan; nothing was applied"
		return m, nil
	}
	m.request++
	m.loading = true
	m.actionErr = ""
	setup, phrase, req := m.cfg.Setup, confirmation, m.request
	ctx := m.cfg.Context
	return m, func() tea.Msg {
		result, err := setup.Apply(ctx, phrase)
		if err != nil {
			return actionMsg{request: req, kind: actionApply, err: err}
		}
		return actionMsg{request: req, kind: actionApply, applied: &result}
	}
}

type loadKind uint8

const (
	loadOverview loadKind = iota
	loadProjects
	loadSessions
	loadInbox
	loadCloud
	loadDoctor
)

type actionKind uint8

const (
	actionReply actionKind = iota
	actionSend
	actionRead
	actionApply
)

type loadedMsg struct {
	request uint64
	screen  Screen
	kind    loadKind
	result  bus.Result
	plan    *SetupPlan
	checks  []Check
	err     error
}

type actionMsg struct {
	request uint64
	kind    actionKind
	result  bus.Result
	applied *ApplyResult
	err     error
}

func (m Model) refresh() (Model, tea.Cmd) {
	m.request++
	return m.refreshView()
}
func (m Model) refreshView() (Model, tea.Cmd) {
	m.loading = true
	m.errText = ""
	m.actionErr = ""
	m.confirming = false
	m.confirm = ""
	screen, requestID := m.screen, m.request
	switch screen {
	case ScreenLinear:
		return m.linearRefresh()
	case ScreenOverview:
		if m.cfg.Executor == nil {
			target := ScreenCloud
			if m.cfg.Setup == nil {
				return m, func() tea.Msg {
					return loadedMsg{request: requestID, screen: ScreenCloud, kind: loadCloud,
						err: errors.New("no authority or setup adapter is configured; run `clawbridge setup --help`")}
				}
			}
			m.screen = target
			screen, requestID = target, m.request
		} else {
			return m, func() tea.Msg { return m.loadOverview(requestID) }
		}
	}
	switch screen {
	case ScreenProjects:
		if m.cfg.Executor == nil {
			return m, func() tea.Msg {
				return loadedMsg{request: requestID, screen: screen, kind: loadProjects,
					err: errors.New("no project authority is configured; complete create or join setup first")}
			}
		}
		return m, func() tea.Msg {
			result, err := m.cfg.Executor.Execute(m.cfg.Context, bus.Request{
				Operation: "list_projects", Workspace: m.cfg.Workspace,
				Args: map[string]any{},
			})
			return loadedMsg{request: requestID, screen: screen, kind: loadProjects, result: result, err: err}
		}
	case ScreenSessions:
		if m.cfg.Executor == nil {
			return m, func() tea.Msg {
				return loadedMsg{request: requestID, screen: screen, kind: loadSessions,
					err: errors.New("no session authority is configured; complete create or join setup first")}
			}
		}
		return m, func() tea.Msg {
			result, err := m.cfg.Executor.Execute(m.cfg.Context, m.requestWithInstance("list_sessions", map[string]any{"limit": 100, "after": m.pageCursor()}))
			return loadedMsg{request: requestID, screen: screen, kind: loadSessions, result: result, err: err}
		}
	case ScreenInbox:
		if m.cfg.Executor == nil {
			return m, func() tea.Msg {
				return loadedMsg{request: requestID, screen: screen, kind: loadInbox,
					err: errors.New("no session authority is configured; complete create or join setup first")}
			}
		}
		if m.cfg.Session == "" || m.cfg.Instance == "" {
			return m, func() tea.Msg {
				return loadedMsg{request: requestID, screen: screen, kind: loadInbox,
					err: errors.New("inbox requires an explicitly bound session and instance; run register or resume first")}
			}
		}
		return m, func() tea.Msg {
			result, err := m.cfg.Executor.Execute(m.cfg.Context, m.requestWithInstance("inbox", map[string]any{
				"unread_only": false, "limit": 100, "after": m.pageCursor(),
			}))
			return loadedMsg{request: requestID, screen: screen, kind: loadInbox, result: result, err: err}
		}
	case ScreenCloud:
		return m, func() tea.Msg {
			if m.cfg.Setup == nil {
				if m.cfg.CloudInfo != "" {
					return loadedMsg{request: requestID, screen: screen, kind: loadCloud, plan: &SetupPlan{Title: "Selected authority", Plan: m.cfg.CloudInfo}}
				}
				return loadedMsg{request: requestID, screen: screen, kind: loadCloud,
					err: errors.New("cloud setup planning is not configured; use the deployment CLI or configure a setup adapter")}
			}
			plan, err := m.cfg.Setup.Plan(m.cfg.Context)
			if err == nil && strings.TrimSpace(plan.Plan) == "" {
				err = errors.New("setup planner returned no reviewable plan")
			}
			return loadedMsg{request: requestID, screen: screen, kind: loadCloud, plan: &plan, err: err}
		}
	case ScreenDoctor:
		return m, func() tea.Msg {
			if m.cfg.Doctor == nil {
				return loadedMsg{request: requestID, screen: screen, kind: loadDoctor,
					err: errors.New("doctor adapter is not configured; run `clawbridge doctor`")}
			}
			checks, err := m.cfg.Doctor.Checks(m.cfg.Context)
			return loadedMsg{request: requestID, screen: screen, kind: loadDoctor, checks: checks, err: err}
		}
	}
	return m, func() tea.Msg {
		return loadedMsg{request: requestID, screen: screen, err: errors.New("unknown screen")}
	}
}

func (m Model) loadOverview(requestID uint64) loadedMsg {
	return m.loadOverviewFor(ScreenOverview, requestID)
}

func (m Model) loadOverviewFor(screen Screen, requestID uint64) loadedMsg {
	if m.cfg.Executor == nil {
		if m.cfg.Setup == nil {
			return loadedMsg{request: requestID, screen: screen, kind: loadOverview,
				err: errors.New("no authority or setup adapter is configured; run `clawbridge setup --help`")}
		}
		plan, err := m.cfg.Setup.Plan(m.cfg.Context)
		if err == nil && strings.TrimSpace(plan.Plan) == "" {
			err = errors.New("setup planner returned no reviewable plan")
		}
		return loadedMsg{request: requestID, screen: ScreenCloud, kind: loadCloud, plan: &plan, err: err}
	}
	result, err := m.cfg.Executor.Execute(m.cfg.Context, bus.Request{
		Operation: "whoami", Workspace: m.cfg.Workspace, Project: m.cfg.Project,
		Session: m.cfg.Session, Args: m.actorArgs(),
	})
	if err != nil {
		return loadedMsg{request: requestID, screen: screen, kind: loadOverview, err: err}
	}
	msg := loadedMsg{request: requestID, screen: screen, kind: loadOverview, result: result}
	if m.cfg.Workspace != "" && m.cfg.Project != "" {
		if sessions, e := m.cfg.Executor.Execute(m.cfg.Context, m.requestWithInstance("list_sessions", map[string]any{"limit": 100, "after": m.pageCursor()})); e == nil {
			data := mapData(sessions.Data)
			if rows := records(data["sessions"]); rows != nil {
				msg.result.Data = mergeResultData(msg.result.Data, sessions.Data, "sessions", sessionRows(rows))
			}
		}
	}
	if m.cfg.Session != "" && m.cfg.Instance != "" && m.cfg.Workspace != "" && m.cfg.Project != "" {
		if inbox, e := m.cfg.Executor.Execute(m.cfg.Context, m.requestWithInstance("inbox", map[string]any{"unread_only": true, "limit": 100})); e == nil {
			data := mapData(inbox.Data)
			if rows := records(data["messages"]); rows != nil {
				msg.result.Data = mergeResultData(msg.result.Data, inbox.Data, "messages", messageRows(rows))
			}
		}
	}
	return msg
}

func mergeResultData(base, extra any, key string, value any) any {
	out := mapData(base)
	if out == nil {
		out = map[string]any{}
	}
	out[key] = value
	if mirror, ok := mapData(extra)["mirror"]; ok {
		out["mirror"] = mirror
	}
	return out
}

func (m Model) actorArgs() map[string]any {
	a := map[string]any{}
	if m.cfg.Session != "" && m.cfg.Instance != "" {
		a["instance"] = m.cfg.Instance
	}
	return a
}

func (m Model) requestWithInstance(op string, args map[string]any) bus.Request {
	copyArgs := map[string]any{}
	for k, v := range args {
		copyArgs[k] = v
	}
	if m.cfg.Session != "" && m.cfg.Instance != "" {
		switch op {
		case "list_sessions", "inbox", "history", "send", "reply", "mark_read", "message_status":
			copyArgs["instance"] = m.cfg.Instance
		}
	}
	return bus.Request{Operation: op, Workspace: m.cfg.Workspace, Project: m.cfg.Project, Session: m.cfg.Session, Args: copyArgs}
}

func (m Model) command(requestID uint64, kind actionKind, req bus.Request) tea.Cmd {
	executor := m.cfg.Executor
	ctx := m.cfg.Context
	return func() tea.Msg {
		result, err := executor.Execute(ctx, req)
		if err == nil && result.Source == "offline-mirror" {
			err = errors.New("authority returned an offline-mirror result for a write; nothing was applied or queued")
		}
		return actionMsg{request: requestID, kind: kind, result: result, err: err}
	}
}

func (m *Model) applyLoad(x loadedMsg) {
	oldID := ""
	oldRows := m.currentRows()
	if m.selected >= 0 && m.selected < len(oldRows) {
		oldID = oldRows[m.selected]
	}
	defer func() {
		rows := m.currentRows()
		m.selected = min(m.selected, max(0, len(rows)-1))
		for i, id := range rows {
			if id == oldID {
				m.selected = i
				break
			}
		}
	}()
	m.source = x.result.Source
	switch x.kind {
	case loadOverview, loadProjects, loadSessions, loadInbox:
		m.applyLoaded(x.kind, x.result)
	case loadCloud:
		m.plan = x.plan
		m.source = "setup-plan"
	case loadDoctor:
		m.checks = append([]Check(nil), x.checks...)
		m.source = "doctor"
	}
}

func (m *Model) applyLoaded(kind loadKind, result bus.Result) {
	m.source = result.Source
	data := mapData(result.Data)
	if data == nil {
		return
	}
	if mirror := mapData(data["mirror"]); mirror != nil {
		m.fresh = Freshness{
			UpdatedAt:  text(mirror, "updated_at"),
			AgeSeconds: integer(mirror, "age_seconds"),
			Stale:      boolean(mirror, "stale"),
		}
	}
	if result.Source == "offline-mirror" {
		m.fresh.Stale = true
	}
	switch kind {
	case loadOverview:
		m.applyWhoami(data)
		if sessions, ok := data["sessions"].([]SessionRow); ok {
			m.sessions = append([]SessionRow(nil), sessions...)
		} else if rows := records(data["sessions"]); rows != nil {
			m.sessions = sessionRows(rows)
		}
		if messages, ok := data["messages"].([]MessageRow); ok {
			m.messages = append([]MessageRow(nil), messages...)
		} else if rows := records(data["messages"]); rows != nil {
			m.messages = messageRows(rows)
		}
	case loadProjects:
		m.projects = projectRows(records(data["projects"]))
	case loadSessions:
		m.rawSessions = sessionRows(records(data["sessions"]))
		m.hasMore = boolean(data, "has_more")
		m.nextCursor = integer(data, "next_cursor")
		*m = m.reclassify(m.cfg.Now())
	case loadInbox:
		m.rawMessages = messageRows(records(data["messages"]))
		m.hasMore = boolean(data, "has_more")
		m.nextCursor = integer(data, "next_cursor")
		*m = m.reclassify(m.cfg.Now())
		m.selected = min(m.selected, max(0, len(m.messages)-1))
	}
}

func (m *Model) applyWhoami(data map[string]any) {
	if w := mapData(data["workspace"]); w != nil {
		m.workspace = WorkspaceView{
			ID: text(w, "id"), Name: text(w, "name"), Mode: text(w, "mode"),
			RetentionDays: int(integer(w, "retention_days")), OwnerPrincipal: text(w, "owner_principal"),
		}
	}
	if p := mapData(data["project"]); p != nil {
		m.project = ProjectView{
			ID: text(p, "id"), Workspace: text(p, "workspace_id"), Slug: text(p, "slug"),
			Name: text(p, "name"), GitRemote: text(p, "git_remote"),
		}
	}
	if s := mapData(data["session"]); s != nil {
		m.session = SessionView{
			ID: text(s, "id"), Name: text(s, "name"), Role: text(s, "role"),
			AgentKind: text(s, "agent_kind"), Status: text(s, "status"),
		}
	}
	online := boolean(data, "online")
	m.online = &online
	m.identity = IdentityView{
		Principal: text(data, "principal"), Provenance: text(data, "identity_provenance"),
		Storage: text(data, "storage_mode"), Verified: boolean(data, "verified"), Online: online,
	}
}

func (m Model) snapshot() Snapshot {
	_, lines, budget := layoutParts(m)
	s := Snapshot{
		Filter: m.filter, Page: m.pageIndex + 1, HasMore: m.hasMore, Cursor: m.pageCursor(), ScrollOffset: bodyOffset(m, lines, budget),
		Screen: m.screen, Mode: m.cfg.Mode, Loading: m.loading,
		Error: firstNonempty(m.errText, m.actionErr), Source: m.source,
		Workspace: m.workspace, Project: m.project, Session: m.session, Identity: m.identity,
		Freshness: m.fresh, Projects: append([]ProjectRow(nil), m.projects...),
		Sessions: append([]SessionRow(nil), m.sessions...), Messages: append([]MessageRow(nil), m.messages...),
		Checks: append([]Check(nil), m.checks...), Apply: m.apply, Confirming: m.confirming, Selected: m.selected,
	}
	if m.plan != nil {
		p := *m.plan
		p.Resources = append([]Resource(nil), m.plan.Resources...)
		s.Plan = &p
	}
	return s
}

func (m Model) writesBlocked() bool {
	if m.cfg.Mode != ModeCloud {
		return false
	}
	if m.online != nil && !*m.online {
		return true
	}
	return m.source == "offline-mirror"
}

func (m Model) currentRows() []string {
	switch m.screen {
	case ScreenLinear:
		return m.linearRows()
	case ScreenProjects:
		out := make([]string, len(m.projects))
		for i := range m.projects {
			out[i] = m.projects[i].ID
		}
		return out
	case ScreenSessions:
		out := make([]string, len(m.sessions))
		for i := range m.sessions {
			out[i] = m.sessions[i].ID
		}
		return out
	case ScreenInbox:
		out := make([]string, len(m.messages))
		for i := range m.messages {
			out[i] = m.messages[i].ID
		}
		return out
	case ScreenDoctor:
		out := make([]string, len(m.checks))
		for i := range m.checks {
			out[i] = m.checks[i].ID
		}
		return out
	}
	return nil
}

func (m Model) updateKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	key := msg.String()
	if key == "ctrl+c" {
		if m.linearCancel != nil {
			m.linearCancel()
		}
		return m, tea.Quit
	}
	if key == "esc" {
		if m.screen == ScreenLinear {
			return m.linearKey(key)
		}
		return m.CancelInput(), nil
	}
	if m.inputMode != inputNone || m.confirming {
		return m.updateEditor(key, msg.Runes)
	}
	if m.screen == ScreenLinear && (key == "enter" || key == " " || key == "c" || key == "w" || key == "l" || key == "u" || key == "x") {
		return m.linearKey(key)
	}
	switch key {
	case "q":
		if m.linearCancel != nil {
			m.linearCancel()
		}
		return m, tea.Quit
	case "1":
		return m.Show(ScreenOverview)
	case "2":
		return m.Show(ScreenProjects)
	case "3":
		return m.Show(ScreenSessions)
	case "4":
		return m.Show(ScreenInbox)
	case "5":
		return m.Show(ScreenCloud)
	case "6":
		return m.Show(ScreenDoctor)
	case "7":
		return m.Show(ScreenLinear)
	case "tab", "right":
		return m.Show(screenAt(indexOf(m.screen) + 1))
	case "shift+tab", "left":
		return m.Show(screenAt(indexOf(m.screen) - 1))
	case "v":
		return m.CycleFilter(), nil
	case "]":
		if m.screen == ScreenLinear {
			return m.linearPage(true)
		}
		return m.NextPage()
	case "[":
		if m.screen == ScreenLinear {
			return m.linearPage(false)
		}
		return m.PreviousPage()
	case "pgdown":
		return m.Scroll(max(1, m.height/2)), nil
	case "pgup":
		return m.Scroll(-max(1, m.height/2)), nil
	case "r":
		return m.Reload()
	case "up", "k":
		return m.Move(-1), nil
	case "down", "j":
		return m.Move(1), nil
	case "?":
		m.actionErr = helpText
		return m, nil
	}
	if m.screen == ScreenInbox {
		if key == "enter" {
			selected, ok := m.SelectedMessage()
			if ok {
				return m.MarkRead([]string{selected.ID})
			}
		}
		if key == "r" {
			key = ""
		}
		if key == "reply" || key == "p" {
			return m.BeginReply(), nil
		}
		if key == "n" {
			return m.BeginSend(), nil
		}
		if key == "m" {
			if selected, ok := m.SelectedMessage(); ok {
				return m.MarkRead([]string{selected.ID})
			}
		}
	}
	if m.screen == ScreenCloud && key == "a" && m.plan != nil {
		m.confirming = true
		m.confirm = ""
		m.actionErr = ""
	}
	return m, nil
}

func (m Model) updateEditor(key string, runes []rune) (Model, tea.Cmd) {
	if key == "ctrl+c" {
		return m, tea.Quit
	}
	if key == "esc" {
		return m.CancelInput(), nil
	}
	if key == "backspace" || key == "ctrl+h" {
		if m.inputMode != inputNone {
			r := []rune(m.draft)
			if len(r) > 0 {
				m.draft = string(r[:len(r)-1])
			}
		} else if len(m.confirm) > 0 {
			r := []rune(m.confirm)
			m.confirm = string(r[:len(r)-1])
		}
		return m, nil
	}
	if key == "enter" {
		if m.inputMode == inputLinearName {
			name := strings.TrimSpace(m.draft)
			if name == "" || len(name) > 128 {
				m.actionErr = "Connection label requires 1–128 characters"
				return m, nil
			}
			m.inputMode = inputNone
			m.draft = ""
			return m.linearCommand(LinearRequest{Operation: "connect", Name: name, Access: m.linear.connectAccess})
		}
		if m.confirming {
			return m.ApplySetup(m.confirm)
		}
		if m.inputMode == inputReply {
			if m.replyTarget != "" {
				return m.Reply(m.replyTarget, m.draft, m.operationID("reply"))
			}
			m.actionErr = "selected inbox message is no longer available"
			return m, nil
		}
		if m.inputMode == inputSend {
			to, body, ok := strings.Cut(m.draft, "|")
			if !ok || strings.TrimSpace(to) == "" || strings.TrimSpace(body) == "" {
				m.actionErr = "send draft must be `recipient|message`"
				return m, nil
			}
			return m.Send(strings.TrimSpace(to), strings.TrimSpace(body), m.operationID("send"))
		}
	}
	if len(runes) > 0 && !containsControl(runes) {
		if m.inputMode != inputNone && len(m.draft) < 65536 {
			m.draft += string(runes)
		} else if m.confirming && len(m.confirm) < 512 {
			m.confirm += string(runes)
		}
	}
	return m, nil
}

func (m Model) operationID(kind string) string {
	now := m.cfg.Now().UTC().Format("20060102T150405.000000000")
	return fmt.Sprintf("tui-%s-%s-%d", kind, now, m.request+1)
}

func containsControl(runes []rune) bool {
	for _, r := range runes {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

var screenOrder = map[Screen]int{
	ScreenOverview: 0, ScreenProjects: 1, ScreenSessions: 2,
	ScreenInbox: 3, ScreenCloud: 4, ScreenDoctor: 5, ScreenLinear: 6,
}
var orderedScreens = []Screen{ScreenOverview, ScreenProjects, ScreenSessions, ScreenInbox, ScreenCloud, ScreenDoctor, ScreenLinear}

func indexOf(s Screen) int {
	if i, ok := screenOrder[s]; ok {
		return i
	}
	return 0
}
func screenAt(i int) Screen {
	n := len(orderedScreens)
	return orderedScreens[(i%n+n)%n]
}

func mapData(v any) map[string]any {
	switch x := v.(type) {
	case map[string]any:
		return x
	case map[string]string:
		out := map[string]any{}
		for k, v := range x {
			out[k] = v
		}
		return out
	default:
		return nil
	}
}

func records(v any) []map[string]any {
	switch rows := v.(type) {
	case []map[string]any:
		return rows
	case []any:
		out := make([]map[string]any, 0, len(rows))
		for _, row := range rows {
			if m := mapData(row); m != nil {
				out = append(out, m)
			}
		}
		return out
	default:
		return nil
	}
}

func projectRows(rows []map[string]any) []ProjectRow {
	out := make([]ProjectRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, ProjectRow{
			ID: text(r, "id"), Slug: text(r, "slug"), Name: text(r, "name"),
			Role: text(r, "role"), GitRepo: text(r, "git_remote"),
		})
	}
	return out
}

func sessionRows(rows []map[string]any) []SessionRow {
	out := make([]SessionRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, SessionRow{
			UpdatedAt: text(r, "updated_at"), ID: text(r, "id"), Name: text(r, "name"), Owner: text(r, "owner_principal"), Host: text(r, "host"), Account: text(r, "account"),
			Role: text(r, "role"), AgentKind: text(r, "agent_kind"), Status: text(r, "status"),
			Presence: text(r, "presence"), Stale: boolean(r, "stale"),
		})
	}
	return out
}

func messageRows(rows []map[string]any) []MessageRow {
	out := make([]MessageRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, MessageRow{
			ID: text(r, "id"), Sequence: integer(r, "sequence"), Sender: text(r, "sender"),
			ThreadID: text(r, "thread_id"), ReplyTo: text(r, "reply_to"), Kind: text(r, "kind"),
			Body: text(r, "body"), CreatedAt: text(r, "created_at"),
			DeliveredAt: text(r, "delivered_at"), ReadAt: text(r, "read_at"),
		})
	}
	return out
}

func text(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, _ := m[key].(string)
	return v
}

func boolean(m map[string]any, key string) bool {
	if m == nil {
		return false
	}
	v, _ := m[key].(bool)
	return v
}

func integer(m map[string]any, key string) int64 {
	if m == nil {
		return 0
	}
	switch v := m[key].(type) {
	case int:
		return int64(v)
	case int64:
		return v
	case float64:
		return int64(v)
	case interface{ Int64() (int64, error) }:
		n, _ := v.Int64()
		return n
	}
	return 0
}

func firstNonempty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func displayError(err error) string {
	if err == nil {
		return ""
	}
	return strings.TrimSpace(err.Error())
}

func projectError(err error) string {
	message := displayError(err)
	if strings.Contains(strings.ToLower(message), "unsupported operation") {
		return "This authority does not provide project listing. Confirm the ClawBridge client and server versions before retrying."
	}
	return message
}

const helpText = "1-6/tab screens · ↑↓ select · r refresh · enter mark read · p reply · n send · a confirm setup · esc cancel · q quit"
