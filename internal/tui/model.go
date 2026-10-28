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
	"clawbridge/internal/clirender"
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
	// Answer is the CLI doctor's plain answer for the status (O3), shown on
	// the line with Detail under it; empty when Detail reads as one.
	Answer string `json:"answer,omitempty"`
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

	Mode      Mode
	Workspace string
	Project   string
	Session   string
	Instance  string
	// AgentMarker names the environment variable that says an agent runs
	// this TUI, or ""; a person's own reads (a project's threads) refuse it.
	AgentMarker string
	Server      string
	Audience    string
	Principal   string
	Provenance  string
	// DeviceID is the device key this profile signs with, if any; the
	// Account screen marks it.
	DeviceID string
	// WaitingForProject opens on "Waiting for a project": the profile
	// has a workspace but no project yet. ProjectFound saves the project
	// once there is one; the TUI then quits so the caller reopens it.
	WaitingForProject bool
	ProjectFound      func(slug string) error

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
	// Assignment summarizes the live assignment: state, issue, task role, scope.
	Assignment string `json:"assignment,omitempty"`
	// Issue is the assignment's issue, by identifier, with its state when
	// it isn't confirmed yet; Coordinator marks the project's coordinator.
	Issue       string `json:"issue,omitempty"`
	Coordinator bool   `json:"coordinator,omitempty"`
	// IssueUUID and IssueState are the assignment's issue and state, for
	// opening it on Linear (i) and naming its holder there.
	IssueUUID  string `json:"issue_uuid,omitempty"`
	IssueState string `json:"issue_state,omitempty"`
	// Context is the session's last context report, in short ("612k warn
	// [1000k]"), and ContextDetail the same in full; both empty without
	// one.
	Context       string `json:"context,omitempty"`
	ContextDetail string `json:"context_detail,omitempty"`
	ContextAt     string `json:"context_at,omitempty"`
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
	// Error is the failed load, else the status line's text; Status is
	// that line's level.
	Error  string
	Status clirender.Level

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
	linear linearState
	intake intakeState
	// projectStatus is the Linear screen's project status view (U61).
	projectStatus projectStatusState
	// views is the Linear screen's shared views list (U63).
	views        viewsState
	linearCancel context.CancelFunc
	account      accountState
	modelSetup   modelSetupState
	cfg          Config

	screen Screen
	width  int
	height int

	tickStarted bool
	// inboxLive wakes the Inbox from the server's stream; nil without one.
	inboxLive *inboxLive
	loading   bool
	// loadErr is the last failed load, raw; the body shows it (§3.5).
	loadErr string
	source  string
	fresh   Freshness
	online  *bool

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

	// help is the ? overlay; quitAsk a q waiting for its y; inboxOpen
	// the selected message opened.
	help      bool
	quitAsk   bool
	inboxOpen bool
	// selectID is a row to select once the screen loads (a jump to a
	// session from its issue).
	selectID string
	// projectOpen shows the selected project's detail; overviews are the
	// listed projects' project_overview reads, by project ID.
	projectOpen bool
	overviews   map[string]projectOverview
	// threads is the opened project's threads, for its owner (U62).
	threads threadsState

	// attention is Overview's "Needs attention"; checksAt is when the
	// doctor's checks ran; doctorPasses shows the folded passes.
	attention       []attentionItem
	attentionLoaded bool
	// intakeLine is Overview's summary of the project's intake, a link
	// to the Intake view (after the attention lines); "" when unread.
	intakeLine string
	// waiting is the no-project mode; waitingOwner whom to ask.
	waiting      bool
	waitingOwner string
	// waitingChecked says a check has answered, so the screen knows
	// whether to say create or ask.
	waitingChecked bool
	checksAt       time.Time
	doctorPasses   bool

	inputMode   inputMode
	draft       string
	replyTarget string
	// sendTo is a new message's recipient, and sendBody whether the
	// cursor is in its message rather than its To field (U27).
	sendTo     string
	sendBody   bool
	confirming bool
	confirm    string
	status     status
	// done is what the write in flight says when it works.
	done string
}

type inputMode uint8

const (
	inputNone inputMode = iota
	inputReply
	inputSend
	inputLinearName
	inputAccountInvite
	inputAccountLabel
	inputAccountTransfer
	inputLinearClosesName
	// inputLinearUnlink takes the project's slug before an unlink
	// (TUI-UX-AUDIT U34).
	inputLinearUnlink
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
	if cfg.WaitingForProject && cfg.Executor != nil && cfg.Project == "" {
		m.waiting, m.screen = true, ScreenOverview
	}
	m.workspace = WorkspaceView{Name: cfg.Workspace}
	m.project = ProjectView{Slug: cfg.Project}
	m.session = SessionView{Name: cfg.Session}
	m.identity = IdentityView{Principal: cfg.Principal, Provenance: cfg.Provenance}
	m.inboxLive = newInboxLive(cfg)
	if cfg.Executor == nil {
		m.screen = ScreenCloud
	}
	return m, nil
}

// NewModel is an alias for New.
func NewModel(cfg Config) (Model, error) { return New(cfg) }

// Init loads the first view without changing authority.
func (m Model) Init() tea.Cmd {
	live := m.inboxLiveCmd()
	if m.screen != ScreenOverview {
		_, cmd := m.refreshView()
		return tea.Batch(cmd, live)
	}
	if m.waiting {
		_, cmd := m.loadWaiting()
		return cmd
	}
	screen, request := m.screen, m.request
	return tea.Batch(func() tea.Msg { return m.loadOverviewFor(screen, request) }, live)
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
	case overviewMsg:
		return m.applyOverviewMsg(x), nil
	case threadsMsg:
		return m.applyThreadsMsg(x), nil
	case threadMsg:
		return m.applyThreadMsg(x), nil
	case intakeLoadedMsg:
		return m.applyIntake(x), nil
	case intakeDoneMsg:
		return m.applyIntakeDone(x)
	case viewsLoadedMsg:
		return m.applyViews(x), nil
	case projectStatusLoadedMsg:
		return m.applyProjectStatus(x), nil
	case projectStatusDoneMsg:
		return m.applyProjectStatusDone(x)
	case accountLoadedMsg:
		return m.applyAccount(x), nil
	case accountActionMsg:
		return m.applyAccountAction(x)
	case modelLoadedMsg:
		return m.applyModelLoaded(x), nil
	case modelSentMsg:
		return m.applyModelSent(x)
	case modelTestedMsg:
		return m.applyModelTested(x)
	case ArchiveTickMsg:
		m = m.reclassify(x.Now)
		return m, archiveTick()
	case InboxChangedMsg:
		if m.inboxLive == nil {
			return m, nil
		}
		return m.inboxChanged()
	case InboxLiveEndedMsg:
		m.status = noteStatus(liveOffNote)
		return m, nil
	case tea.WindowSizeMsg:
		m.width, m.height = x.Width, x.Height
		return m, nil
	case tea.KeyMsg:
		return m.updateKey(x)
	case waitingTickMsg:
		if x.request != m.request || !m.waiting || m.screen != ScreenOverview {
			return m, nil
		}
		return m.loadWaiting()
	case waitingLoadedMsg:
		return m.applyWaiting(x)
	case attentionMsg:
		m = m.applyAttention(x)
		if !m.tickStarted {
			m.tickStarted = true
			return m, archiveTick()
		}
		return m, nil
	case loadedMsg:
		if x.request != m.request || x.screen != m.screen {
			return m, nil
		}
		m.loading = false
		m.loadErr = ""
		if x.err != nil {
			m = m.restorePage()
			m.loadErr = displayError(x.err)
			if x.kind == loadProjects {
				m.loadErr = projectError(x.err)
			}
			return m, nil
		}
		m.pageRollback = nil
		(&m).applyLoad(x)
		if x.kind == loadOverview && x.screen == ScreenOverview {
			// The attention lines follow the Overview, one read after
			// another; the archive tick starts once they're in.
			if cmd := m.loadAttention(x.request); cmd != nil {
				return m, cmd
			}
		}
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
		m.status = status{}
		if x.err != nil {
			m.status = errStatus(x.err)
			return m, nil
		}
		switch x.kind {
		case actionReply, actionSend, actionRead:
			m.source = x.result.Source
			m.status = okStatus(m.done)
			if x.kind == actionRead || x.kind == actionReply || x.kind == actionSend {
				m.inputMode = inputNone
				m.draft = ""
				_, cmd := m.reload()
				// That reload is the one the OK line outlives.
				m.status = m.status.kept()
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
	if !knownScreen(screen) {
		m.status = noteStatus("unknown screen")
		return m, nil
	}
	m.help, m.quitAsk, m.inboxOpen, m.projectOpen = false, false, false, false
	m.threads = threadsState{}
	// Leaving the Model screen ends a setup and wipes its key.
	m = m.closeModelSetup()
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
	m.status = status{}
	return m.refresh()
}

// Reload refreshes the selected screen through the configured executor or
// read-only adapter.
func (m Model) Reload() (Model, tea.Cmd) {
	if m.screen == ScreenProjects && m.threads.open {
		return m.reloadThreads()
	}
	// On Linear, r also refreshes the board's stored names (an owner or
	// lead; see canRefreshNames).
	m.linear.refreshNames = m.screen == ScreenLinear
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

func (m Model) messageByID(id string) (MessageRow, bool) {
	for _, msg := range m.messages {
		if msg.ID == id {
			return msg, true
		}
	}
	return MessageRow{}, false
}

// BeginReply opens an explicit reply draft for the selected inbox message.
func (m Model) BeginReply() Model {
	if m.cfg.Session == "" || m.cfg.Instance == "" {
		m.status = noteStatus("this action requires a bound session and matching instance; use explicit --session and --instance")
		return m
	}

	selected, ok := m.SelectedMessage()
	if !ok {
		m.status = noteStatus("select an inbox message before replying")
		return m
	}
	if m.writesBlocked() {
		m.status = noteStatus("the authority is offline; replies are not queued and remain unavailable until reconnect")
		return m
	}
	m.replyTarget = selected.ID
	m.inputMode = inputReply
	m.draft = ""
	m.status = status{}
	return m
}

// BeginSend opens a new message: To, then Message (U27).
func (m Model) BeginSend() Model { return m.BeginSendTo("") }

// BeginSendTo opens a new message to the named session, or with To empty.
func (m Model) BeginSendTo(to string) Model {
	if m.cfg.Session == "" || m.cfg.Instance == "" {
		m.status = noteStatus("Sending needs a session: messages go from one agent session to another. Open the TUI with --session and --instance to send as that session.")
		return m
	}
	if to != "" && to == firstNonempty(m.session.Name, m.cfg.Session) {
		m.status = noteStatus("That's this session; choose another one to message.")
		return m
	}
	if m.writesBlocked() {
		m.status = noteStatus("the authority is offline; sends are not queued and remain unavailable until reconnect")
		return m
	}
	m.inputMode = inputSend
	m.draft = ""
	m.sendTo, m.sendBody = to, to != ""
	m.status = status{}
	return m
}

// peerNames are the sessions a message can go to: the loaded ones that
// are neither retired, moved, nor this session.
func (m Model) peerNames() []string {
	rows := m.rawSessions
	if rows == nil {
		rows = m.sessions
	}
	self := firstNonempty(m.session.Name, m.cfg.Session)
	out := []string{}
	for _, r := range rows {
		if r.Name != "" && r.Name != self && r.Status != "retired" && r.Status != "moved" {
			out = append(out, r.Name)
		}
	}
	return out
}

// nextPeer is the name after to among the peers, the first when to isn't
// one, or the first that starts with what's typed.
func (m Model) nextPeer(to string) string {
	peers := m.peerNames()
	if len(peers) == 0 {
		return to
	}
	for i, p := range peers {
		if p == to {
			return peers[(i+1)%len(peers)]
		}
	}
	for _, p := range peers {
		if to != "" && strings.HasPrefix(p, to) {
			return p
		}
	}
	return peers[0]
}

// CancelInput abandons a draft without writing.
func (m Model) CancelInput() Model {
	m.replyTarget = ""
	m.inputMode = inputNone
	m.draft = ""
	m.sendTo, m.sendBody = "", false
	m.confirming = false
	m.confirm = ""
	m.account.pending = nil
	m.account.labelling = ""
	return m
}

// Reply is an explicit mutation. It is never invoked by navigation or refresh.
func (m Model) Reply(messageID, body, operationID string) (Model, tea.Cmd) {
	if m.cfg.Session == "" || m.cfg.Instance == "" {
		m.status = noteStatus("this action requires a bound session and matching instance; use explicit --session and --instance")
		return m, nil
	}

	if messageID == "" || strings.TrimSpace(body) == "" || operationID == "" {
		m.status = noteStatus("reply requires a selected message, nonempty body and operation ID")
		return m, nil
	}
	if m.writesBlocked() {
		m.status = noteStatus("authority is offline; reply was not attempted or queued")
		return m, nil
	}
	m.done = "Reply sent."
	if msg, ok := m.messageByID(messageID); ok && msg.Sender != "" {
		m.done = "Reply sent to " + msg.Sender + "."
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
		m.status = noteStatus("this action requires a bound session and matching instance; use explicit --session and --instance")
		return m, nil
	}

	if strings.TrimSpace(to) == "" || strings.TrimSpace(body) == "" || operationID == "" {
		m.status = noteStatus("send requires a recipient, nonempty body and operation ID")
		return m, nil
	}
	if m.writesBlocked() {
		m.status = noteStatus("authority is offline; send was not attempted or queued")
		return m, nil
	}
	m.done = "Sent to " + to + "."
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
		m.status = noteStatus("this action requires a bound session and matching instance; use explicit --session and --instance")
		return m, nil
	}

	if len(ids) == 0 {
		m.status = noteStatus("select at least one message to mark read")
		return m, nil
	}
	if m.writesBlocked() {
		m.status = noteStatus("authority is offline; mark-read was not attempted or queued")
		return m, nil
	}
	m.done = "Marked read."
	if len(ids) > 1 {
		m.done = fmt.Sprintf("Marked %d messages read.", len(ids))
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
		m.status = noteStatus("setup apply is available only in explicitly configured cloud mode")
		return m, nil
	}
	if m.cfg.Setup == nil {
		m.status = noteStatus("setup adapter is not configured")
		return m, nil
	}
	if m.plan == nil {
		m.status = noteStatus("load and review the exact setup plan before applying")
		return m, nil
	}
	if strings.TrimSpace(m.plan.Plan) == "" || strings.TrimSpace(m.plan.Confirmation) == "" || strings.TrimSpace(m.plan.MonthlyCost) == "" {
		m.status = noteStatus("setup plan must show resources and cost before apply")
		return m, nil
	}
	if confirmation == "" || confirmation != m.plan.Confirmation {
		m.status = noteStatus("confirmation phrase does not match the displayed plan; nothing was applied")
		return m, nil
	}
	m.request++
	m.loading = true
	m.status = status{}
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
	request   uint64
	screen    Screen
	kind      loadKind
	result    bus.Result
	overviews map[string]projectOverview
	plan      *SetupPlan
	checks    []Check
	err       error
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
	m.loadErr = ""
	m.status = m.status.kept()
	m.confirming = false
	m.confirm = ""
	screen, requestID := m.screen, m.request
	m.account.pending = nil
	switch screen {
	case ScreenLinear:
		return m.linearRefresh()
	case ScreenAccount:
		return m.accountRefresh()
	case ScreenModel:
		return m.modelRefresh()
	case ScreenOverview:
		if m.waiting {
			return m.loadWaiting()
		}
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
			msg := loadedMsg{request: requestID, screen: screen, kind: loadProjects, result: result, err: err}
			if err == nil {
				msg.overviews = m.readOverviews(projectRows(records(mapData(result.Data)["projects"])))
			}
			return msg
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
			// Nothing to load: the body says whose inboxes these are (U36).
			m.loading = false
			return m, nil
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
		if m.selectID != "" {
			oldID, m.selectID = m.selectID, ""
		}
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
		if x.kind == loadProjects {
			m.overviews = x.overviews
		}
	case loadCloud:
		m.plan = x.plan
		m.source = "setup-plan"
	case loadDoctor:
		m.checks = append([]Check(nil), x.checks...)
		m.checksAt = m.cfg.Now()
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
		Error: firstNonempty(m.loadErr, m.status.text), Status: m.status.level, Source: m.source,
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
	case ScreenAccount:
		return m.accountRowIDs()
	case ScreenProjects:
		if m.threads.open {
			return m.threadRowIDs()
		}
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
		rows := doctorRows(m)
		out := make([]string, len(rows))
		for i := range rows {
			out[i] = rows[i].ID
		}
		return out
	case ScreenOverview:
		if m.waiting {
			out := make([]string, len(m.projects))
			for i := range m.projects {
				out[i] = m.projects[i].ID
			}
			return out
		}
		return m.overviewRows()
	}
	return nil
}

func (m Model) updateKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	key := msg.String()
	if key == "ctrl+c" {
		if m.linearCancel != nil {
			m.linearCancel()
		}
		return m.closeModelSetup(), tea.Quit
	}
	if m.screen == ScreenModel && m.modelSetup.wizard.step >= modelStepProvider && m.modelSetup.wizard.step <= modelStepDisclosure {
		// A setup takes every key, so a digit or q typed into a field
		// neither switches screens nor quits.
		return m.modelKey(msg)
	}
	if m.screen == ScreenModel && (m.modelSetup.wizard.step == modelStepSending || m.modelSetup.wizard.step == modelStepTesting) {
		// While it sends and tests, the screen keeps every key, so its
		// answer isn't lost to a tab switch (U42).
		return m.modelBusyKey(key), nil
	}
	if m.screen == ScreenAccount && m.account.code != "" {
		// A code just issued holds the screen until it is dismissed.
		return m.accountKey(key)
	}
	if m.quitAsk {
		if key == "y" {
			return m.quit()
		}
		m.quitAsk, m.status = false, status{}
		return m, nil
	}
	if m.screen == ScreenLinear && m.linear.logoutAsk != "" {
		return m.linearLogoutAnswer(key)
	}
	if m.screen == ScreenLinear && m.linear.step == "intake" && (m.intake.ask != nil || m.intake.picking) {
		return m.intakeModalKey(key)
	}
	if m.screen == ScreenLinear && m.linear.step == "status" && (m.projectStatus.ask != nil || m.projectStatus.picking) {
		return m.projectStatusModalKey(key)
	}
	if m.screen == ScreenAccount && m.account.invite != nil {
		return m.inviteKey(key)
	}
	if m.help {
		// The overlay takes the keys it is closed with; anything else
		// is ignored, so a key read off it acts only once it's closed.
		if key == "esc" || key == "?" {
			m.help = false
		}
		return m, nil
	}
	if m.inputMode != inputNone || m.confirming {
		if key == "esc" {
			return m.CancelInput(), nil
		}
		return m.updateEditor(key, msg.Runes)
	}
	if b, ok := m.binding(key); ok {
		return b.Action(m, key)
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
	if m.inputMode == inputSend && !m.sendBody {
		return m.sendToKey(key, runes)
	}
	if m.inputMode == inputSend && (key == "tab" || key == "shift+tab" || key == "up") {
		m.sendBody = false
		return m, nil
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
		if m.inputMode == inputLinearClosesName {
			return m.linearClosesTyped()
		}
		if m.inputMode == inputLinearUnlink {
			return m.linearUnlinkTyped()
		}
		if m.screen == ScreenAccount && (m.confirming || m.inputMode == inputAccountInvite || m.inputMode == inputAccountLabel || m.inputMode == inputAccountTransfer) {
			return m.accountSubmit()
		}
		if m.inputMode == inputLinearName {
			name := strings.TrimSpace(m.draft)
			if name == "" || len(name) > 128 {
				m.status = noteStatus("Connection label requires 1–128 characters")
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
			m.status = noteStatus("selected inbox message is no longer available")
			return m, nil
		}
		if m.inputMode == inputSend {
			if strings.TrimSpace(m.draft) == "" {
				m.status = noteStatus("Write a message first; Esc cancels.")
				return m, nil
			}
			return m.Send(strings.TrimSpace(m.sendTo), strings.TrimSpace(m.draft), m.operationID("send"))
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

// sendToKey edits a new message's To field: Tab offers the next session,
// Enter goes on to the message.
func (m Model) sendToKey(key string, runes []rune) (Model, tea.Cmd) {
	switch key {
	case "tab":
		m.sendTo = m.nextPeer(m.sendTo)
	case "enter", "down":
		if strings.TrimSpace(m.sendTo) == "" {
			m.status = noteStatus("Choose whom to message: type a session's name, or Tab through them.")
			return m, nil
		}
		m.sendTo, m.sendBody, m.status = strings.TrimSpace(m.sendTo), true, status{}
	case "backspace", "ctrl+h":
		if r := []rune(m.sendTo); len(r) > 0 {
			m.sendTo = string(r[:len(r)-1])
		}
	default:
		if len(runes) > 0 && !containsControl(runes) && len(m.sendTo) < 256 {
			m.sendTo += string(runes)
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

// orderedScreens is every screen; the tabs a profile shows are tabs().
var orderedScreens = []Screen{ScreenOverview, ScreenProjects, ScreenSessions, ScreenInbox, ScreenLinear, ScreenModel, ScreenAccount, ScreenDoctor, ScreenCloud}

func knownScreen(s Screen) bool {
	for _, x := range orderedScreens {
		if x == s {
			return true
		}
	}
	return false
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
			Presence: text(r, "presence"), Stale: boolean(r, "stale"), Assignment: assignmentSummary(r["assignment"]),
			Issue: assignmentIssue(r["assignment"]), Coordinator: boolean(r, "coordinator"), ContextAt: text(r, "context_at"),
		})
		if a, ok := r["assignment"].(map[string]any); ok {
			out[len(out)-1].IssueUUID, out[len(out)-1].IssueState = text(a, "issue_uuid"), text(a, "state")
		}
		out[len(out)-1].Context, out[len(out)-1].ContextDetail = contextSummary(r)
	}
	return out
}

// contextSummary is a session's last context report from list_sessions:
// counts, a level and a time only.
func contextSummary(r map[string]any) (short, detail string) {
	at := text(r, "context_at")
	if at == "" {
		return "", ""
	}
	tokens, window, compactions := integer(r, "context_tokens"), integer(r, "context_window"), integer(r, "context_compactions")
	level := text(r, "context_level")
	short = kTokens(tokens)
	if level == "warn" || level == "handover" {
		short += " " + level
	}
	detail = fmt.Sprintf("%s tokens, level %s", kTokens(tokens), firstNonempty(level, "ok"))
	if window > 0 {
		short += " [" + kTokens(window) + "]"
		detail += ", window " + kTokens(window)
	}
	// When it was reported is shown relative to now, beside this.
	detail += fmt.Sprintf(", %d compaction(s)", compactions)
	if backstop := text(r, "context_backstop_at"); backstop != "" {
		detail += ", last compacted before its handover level at " + backstop
	}
	return short, detail
}

func kTokens(n int64) string {
	if n < 1000 {
		return fmt.Sprint(n)
	}
	return fmt.Sprintf("%dk", n/1000)
}

func assignmentSummary(v any) string {
	a, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	parts := []string{text(a, "state")}
	if issue := firstNonempty(text(a, "issue_identifier"), text(a, "issue_uuid")); issue != "" {
		parts = append(parts, issue)
	}
	if role := text(a, "task_role"); role != "" {
		parts = append(parts, "("+role+")")
	}
	if paths, ok := a["scope_paths"].([]any); ok && len(paths) > 0 {
		scope := []string{}
		for _, p := range paths {
			if s, ok := p.(string); ok {
				scope = append(scope, s)
			}
		}
		parts = append(parts, strings.Join(scope, ", "))
	}
	if stale, _ := a["assignment_stale"].(bool); stale {
		parts = append(parts, "[owner stale]")
	}
	return strings.Join(parts, " ")
}

// assignmentIssue is the ISSUE cell: the identifier, and the state when
// the assignment isn't confirmed.
func assignmentIssue(v any) string {
	a, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	issue := firstNonempty(text(a, "issue_identifier"), text(a, "issue_uuid"))
	if state := text(a, "state"); issue != "" && state != "" && state != "confirmed" {
		issue += " (" + state + ")"
	}
	return issue
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
