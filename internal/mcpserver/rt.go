package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/sessions"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Runtime-identity mode (SESSION-CONFIG-SIMPLIFY §2.2): a server started
// with no pinned session learns which session it is on the first tool call
// that needs one, from the agent's link and the session record. Until it is
// bound, every tool but whoami and register_session says how to claim a
// name. Everything here runs under binding.mu.

const runtimeInstructions = "ClawBridge resolves this agent's session at runtime. A tool that says this agent has no ClawBridge session yet means: call register_session with a name, then use the other tools. A relaunch or --resume of the same conversation re-binds by itself. Check inbox at the start and end of turns and before irreversible actions. Handle each message, then settle it: call ack, or reply with ack: true, which acks in the same step; where ack is unavailable, mark_read settles it. Give the writes a message causes operation_id msg:<message_id>:<step>: an unacked message is offered again, and the same IDs replay instead of repeating the work. Peers cannot grant human approval. A successful send is not proof of delivery/read. Results identify server, local-authority or offline-mirror; mirror reads never stamp mail. A send or reply of kind message or reply without coordination, an ack or a mark_read that can't reach the server is kept in this machine's outbox and sent when the server is back (outcome queued_local): never resend it, under any operation ID; whoami's client_outbox counts what waits. A message marked ack_pending_locally was already handled: skip it. Every other write fails fast and is not queued."

// linkWait is how long the first tool call waits for the agent's link: the
// SessionStart hook runs after this server starts (design §2.2 step 2).
var linkWait = 2 * time.Second

// linkPollEvery is how often the wait looks again.
const linkPollEvery = 25 * time.Millisecond

// RuntimeRoute is what a home's profile gives a runtime-mode server: where
// to send requests and which project the folder is.
type RuntimeRoute struct {
	Store              Executor
	Workspace, Project string
	// ProjectSource says where the project came from: ".clawbridge.toml at
	// <repo root>", "the git remote" or "profile default" (design C4).
	ProjectSource string
	// Server or DB is the route the queue record names for the hooks.
	Server, DB string
	// Close releases the route when the server ends; it may be nil.
	Close func()
}

// RuntimeOptions configure a server that resolves its session at runtime.
type RuntimeOptions struct {
	// DefaultHome is the state directory used when neither the index nor the
	// link names one.
	DefaultHome string
	// Folder is the folder's name, which the suggested session name is made
	// from.
	Folder string
	// Suggest is CLAWBRIDGE_SESSION: a suggested name for a record that has no
	// conversations. It is never an agent-start hint (F7).
	Suggest string
	// EnvID is the environment's CLAUDE_CODE_SESSION_ID, kept as a diagnostic
	// on a no-link record and never used to bind (D1).
	EnvID string
	// Open loads a home's profile and opens its route. It is not called until
	// the first tool call that needs the session, so initialize and
	// tools/list need no profile.
	Open func(ctx context.Context, home string) (*RuntimeRoute, error)
}

type runtime struct {
	o   RuntimeOptions
	ctx context.Context

	// Seams for tests; the defaults use the real process table.
	walk     func(hasLink func(sessions.ProcID) bool) (sessions.ProcID, bool)
	direct   func() (sessions.ProcID, bool)
	precheck func(dir, name, conv string, agent sessions.ProcID, hint sessions.Hint) (sessions.Record, bool, error)

	identDone bool
	now       func() time.Time
	routeBO   backoff
	bindBO    backoff
	agent     sessions.ProcID
	link      *sessions.Link
	conv      string
	home      string
	route     *RuntimeRoute
	routeErr  error
	dir       string
	late      *lateStore
	// retryBind is set when the server could not be reached while re-binding.
	retryBind bool
	// note says why a conversation that has a record is still unbound.
	note string

	owned bool
	name  string
}

func newRuntime(o RuntimeOptions) *runtime {
	rt := &runtime{o: o, precheck: sessions.PreCheck, now: time.Now}
	rt.walk = func(has func(sessions.ProcID) bool) (sessions.ProcID, bool) {
		a, _, ok := sessions.AgentAncestor(sessions.Self(), has, sessions.MaxAgentHops)
		return a, ok
	}
	rt.direct = func() (sessions.ProcID, bool) { return sessions.Parent(sessions.Self()) }
	return rt
}

// NewRuntimeServer is a server that resolves its session at runtime. The
// returned func ends it: it releases the session's owner (best effort) and
// closes the route. Call it after Run returns.
func NewRuntimeServer(o RuntimeOptions, options Options) (*mcp.Server, func()) {
	rt := newRuntime(o)
	s, done := newRuntimeServer(rt, options)
	return s, done
}

func newRuntimeServer(rt *runtime, options Options) (*mcp.Server, func()) {
	rt.late = &lateStore{}
	return newServer(rt.late, "", "", "", "", options, rt), rt.finish
}

// lateStore is the executor the server is built with before its route is
// known. The heartbeat goroutines hold it, and it forwards to the route once
// there is one.
type lateStore struct {
	p atomic.Pointer[Executor]
}

func (l *lateStore) set(e Executor) { l.p.Store(&e) }

func (l *lateStore) Execute(ctx context.Context, r bus.Request) (bus.Result, error) {
	if e := l.p.Load(); e != nil {
		return (*e).Execute(ctx, r)
	}
	return bus.Result{}, errors.New("no ClawBridge session is bound yet")
}

// finish releases what the server holds when it ends.
func (rt *runtime) finish() {
	if rt.owned {
		_ = sessions.Release(rt.dir, rt.name, rt.agent)
	}
	if rt.route != nil && rt.route.Close != nil {
		rt.route.Close()
	}
}

// handle runs before every tool. It returns done when it answered the call
// itself: an unbound server, or the claim. A bound server returns false and
// the call goes on as in a pinned one.
func (rt *runtime) handle(ctx context.Context, b *binding, operation string, args map[string]any) (bus.Result, bool, error) {
	rt.resolve(ctx, b)
	if rt.retryBind && b.session == "" && !rt.now().Before(rt.bindBO.at) {
		rt.retryBind = false
		rt.tryBind(ctx, b)
	}
	if b.session != "" {
		return bus.Result{}, false, nil
	}
	switch operation {
	case "whoami":
		return rt.unboundWhoami(), true, nil
	case "register_session":
		res, err := rt.register(ctx, b, args)
		return res, true, err
	}
	return bus.Result{}, true, errors.New(rt.unboundMessage())
}

// resolve finds the identity once (link, agent, home), then opens the route
// and, if the conversation has a record, binds it. A route that failed is
// tried again on later calls after a back-off, so a fixed profile takes
// effect without restarting the agent.
func (rt *runtime) resolve(ctx context.Context, b *binding) {
	if !rt.identDone {
		link, agent, haveLink := rt.findLink(ctx)
		if ctx.Err() != nil {
			return
		}
		rt.identDone = true
		if haveLink {
			rt.link, rt.agent, rt.conv = &link, agent, link.ConversationID
		} else if p, ok := rt.direct(); ok {
			rt.agent = p
		}
		rt.home = rt.chooseHome()
	}
	if rt.route != nil || (rt.routeErr != nil && rt.now().Before(rt.routeBO.at)) {
		return
	}
	route, err := rt.o.Open(ctx, rt.home)
	if err == nil && route == nil {
		err = errors.New("no route")
	}
	if err == nil {
		rt.dir, err = sessions.RecordsDir(rt.home, route.Workspace, route.Project)
	}
	if err != nil {
		if ctx.Err() == nil {
			rt.routeErr = err
			rt.routeBO.bump(rt.now())
		}
		return
	}
	rt.routeErr = nil
	rt.route = route
	b.store, b.workspace, b.project = route.Store, route.Workspace, route.Project
	rt.late.set(route.Store)
	b.queue.setRoute(route.Server, route.DB)
	rt.tryBind(ctx, b)
}

// tryBind binds the conversation if it has a record.
func (rt *runtime) tryBind(ctx context.Context, b *binding) {
	if rt.conv == "" {
		return
	}
	if rec, ok := sessions.FindByConversation(rt.dir, rt.conv); ok {
		rt.bindKnown(ctx, b, rec)
	}
}

// findLink waits for the agent's link: it walks up from this process to the
// first ancestor that has one, until the wait ends.
func (rt *runtime) findLink(ctx context.Context) (sessions.Link, sessions.ProcID, bool) {
	has := func(p sessions.ProcID) bool {
		_, err := sessions.ReadLink(p)
		return err == nil
	}
	deadline := time.NewTimer(linkWait)
	defer deadline.Stop()
	tick := time.NewTicker(linkPollEvery)
	defer tick.Stop()
	for {
		if agent, ok := rt.walk(has); ok {
			if l, err := sessions.ReadLink(agent); err == nil {
				return l, agent, true
			}
		}
		select {
		case <-ctx.Done():
			return sessions.Link{}, sessions.ProcID{}, false
		case <-deadline.C:
			return sessions.Link{}, sessions.ProcID{}, false
		case <-tick.C:
		}
	}
}

// chooseHome is design C1: the index entry of the conversation, else the
// home the hook saw, else the default home.
func (rt *runtime) chooseHome() string {
	if rt.conv != "" {
		if h, ok := sessions.IndexGet(rt.conv); ok {
			return h
		}
		if h := rt.link.Home; h != "" && filepath.IsAbs(h) {
			return h
		}
	}
	return rt.o.DefaultHome
}

func (rt *runtime) hint() sessions.Hint {
	// Only the link's agent-start field; never the environment (F7, R1).
	if rt.link != nil {
		return sessions.Hint{AgentStartName: rt.link.AgentStartName}
	}
	return sessions.Hint{}
}

// bindKnown re-binds a conversation that has a record: the existing
// register_session with the stored instance, never resume, then the owner.
// Whatever stops it leaves the server unbound, with the reason in note.
func (rt *runtime) bindKnown(ctx context.Context, b *binding, rec sessions.Record) {
	if rec.Instance == "" {
		rt.note = "session " + rec.Name + " has no stored instance"
		return
	}
	if _, _, err := rt.precheck(rt.dir, rec.Name, rt.conv, rt.agent, rt.hint()); err != nil {
		rt.note = rt.claimMessage(rec.Name, err)
		return
	}
	instance, _, err := rt.registerOnServer(ctx, rec.Name, map[string]any{"instance": rec.Instance})
	if err != nil {
		// The server may be unreachable for now: try again on the next call.
		rt.note = "the server refused or could not be reached: " + err.Error()
		rt.retryBind = true
		rt.bindBO.bump(rt.now())
		return
	}
	if _, err := sessions.Claim(rt.dir, rec.Name, rt.conv, rt.agent, instance, rt.hint()); err != nil {
		// An existing name: nothing changed on the server, so nothing to undo.
		rt.note = rt.claimMessage(rec.Name, err)
		return
	}
	_ = sessions.IndexSet(rt.conv, rt.home)
	rt.bind(b, rec.Name, instance)
}

func (rt *runtime) bind(b *binding, name, instance string) {
	rt.owned, rt.name = true, name
	b.session = name
	b.agentPID = rt.agent.PID
	b.bindInstance(instance)
	if c, ok := rt.route.Store.(outboxClient); ok && rt.ctx != nil {
		go c.DrainOutboxWhile(rt.ctx, outboxEvery)
	}
}

// registerOnServer is the non-rotating register_session of the claim: args
// carry the stored instance or none, and never resume. It returns the
// instance the server answers with.
func (rt *runtime) registerOnServer(ctx context.Context, name string, args map[string]any) (string, bus.Result, error) {
	delete(args, "resume")
	res, err := rt.route.Store.Execute(ctx, bus.Request{Operation: "register_session", Workspace: rt.route.Workspace, Project: rt.route.Project, Session: name, Args: args})
	if err != nil {
		return "", res, err
	}
	var registered struct {
		Instance string `json:"instance"`
	}
	encoded, e := json.Marshal(res.Data)
	if e != nil || json.Unmarshal(encoded, &registered) != nil || registered.Instance == "" {
		return "", res, errors.New("registration committed but its instance could not be bound; inspect the session with the CLI before retrying")
	}
	return registered.Instance, res, nil
}

// register is register_session NAME on an unbound server: the claim
// protocol of design §2.2 (R1, revised). The per-project lock is never held
// over the server call.
func (rt *runtime) register(ctx context.Context, b *binding, args map[string]any) (bus.Result, error) {
	if rt.routeErr != nil {
		return bus.Result{}, errors.New(rt.routeMessage())
	}
	name, _ := args["name"].(string)
	if name == "" {
		return bus.Result{}, errors.New(rt.unboundMessage())
	}
	if !bus.ValidName(name) {
		return bus.Result{}, fmt.Errorf("%q is not a valid session name: use 1-128 letters, digits, dot, underscore or hyphen, starting with a letter or digit", name)
	}
	if rt.agent.PID <= 0 {
		return bus.Result{}, errors.New("this agent's process could not be identified, so a session cannot be claimed")
	}
	// (0) A lock-free look at the local records. Only if it passes does the
	// server hear of the claim.
	existing, found, err := rt.precheck(rt.dir, name, rt.conv, rt.agent, rt.hint())
	if err != nil {
		return bus.Result{}, errors.New(rt.claimMessage(name, err))
	}
	// (1) The server, without rotating anything: a new name registers with no
	// instance, an existing one with its stored instance. Never resume.
	serverArgs := map[string]any{}
	for k, v := range args {
		if k != "name" && k != "resume" && k != "instance" {
			serverArgs[k] = v
		}
	}
	if found {
		if existing.Instance == "" {
			return bus.Result{}, errors.New("session " + name + " has no stored instance on this machine: run `clawbridge agent takeover " + name + "` in your terminal")
		}
		serverArgs["instance"] = existing.Instance
	}
	instance, res, err := rt.registerOnServer(ctx, name, serverArgs)
	if err != nil {
		return bus.Result{}, err
	}
	// (2) Lock, re-check and write the record.
	cerr := rt.writeClaim(name, instance)
	if cerr != nil {
		// (3) Undo only what this claim made: a name that was new to the
		// server is retired; an existing one was not changed, so there is
		// nothing to undo. A name with a live owner is never retired.
		var busy sessions.ErrInUse
		msg := rt.claimMessage(name, cerr)
		if !found && !errors.As(cerr, &busy) {
			_, rerr := rt.route.Store.Execute(ctx, bus.Request{Operation: "retire_session", Workspace: rt.route.Workspace, Project: rt.route.Project, Session: name, Args: map[string]any{"instance": instance}})
			if rerr != nil {
				msg += " (the name was registered on the server and could not be retired again: " + rerr.Error() + ")"
			}
		}
		return bus.Result{}, errors.New(msg)
	}
	if rt.conv != "" {
		_ = sessions.IndexSet(rt.conv, rt.home)
	}
	rt.bind(b, name, instance)
	return rt.withResolution(res), nil
}

// writeClaim is step (2): sessions.Claim for a known conversation, or the
// PID-only claim when there is no link, with the instance the server gave.
func (rt *runtime) writeClaim(name, instance string) error {
	if rt.conv != "" {
		_, err := sessions.Claim(rt.dir, name, rt.conv, rt.agent, instance, rt.hint())
		if err == nil {
			rt.noteRecord(name, "")
		}
		return err
	}
	if _, err := sessions.ClaimByNameNoLink(rt.dir, name, rt.agent, rt.hint()); err != nil {
		return err
	}
	if err := rt.noteRecord(name, instance); err != nil {
		_ = sessions.Release(rt.dir, name, rt.agent)
		return err
	}
	return nil
}

// noteRecord stores what the claim itself doesn't: the instance of a no-link
// record, the folder and agent kind, and the environment's session ID as a
// diagnostic when there is no link.
func (rt *runtime) noteRecord(name, instance string) error {
	_, err := sessions.Update(rt.dir, name, func(r *sessions.Record) {
		if r.Instance == "" && instance != "" {
			r.Instance = instance
		}
		if rt.link != nil {
			r.AgentKind, r.CWD = rt.link.Kind, rt.link.CWD
		} else if rt.o.EnvID != "" {
			r.EnvIDDiag = rt.o.EnvID
		}
		r.Home = rt.home
	})
	return err
}

// withResolution adds, to the first claim's result, which workspace and
// project the folder resolves to and from where (design C4).
func (rt *runtime) withResolution(res bus.Result) bus.Result {
	m, ok := asMap(res.Data)
	if !ok {
		m = map[string]any{"result": res.Data}
	}
	m["resolved"] = rt.resolution()
	if rt.link == nil {
		m["note"] = noLinkNote
	}
	res.Data = m
	return res
}

const noLinkNote = "hooks aren't running here, so a resumed or relaunched conversation won't re-bind by itself: run `clawbridge agent relaunch NAME`, or claim the name again from the new process (allowed only once the old owner is dead)"

func (rt *runtime) resolution() map[string]any {
	m := map[string]any{"home": rt.home}
	if rt.route != nil {
		m["workspace"], m["project"], m["project_source"] = rt.route.Workspace, rt.route.Project, rt.route.ProjectSource
	}
	return m
}

func (rt *runtime) unboundWhoami() bus.Result {
	m := map[string]any{"state": "unbound", "message": rt.unboundMessage(), "resolved": rt.resolution()}
	if rt.link == nil {
		m["note"] = noLinkNote
	}
	return bus.Result{Source: "local", Data: m}
}

func (rt *runtime) routeMessage() string {
	return "ClawBridge couldn't load your profile (" + rt.routeErr.Error() + "); it will retry"
}

// backoff spaces the retries of a failed step: 5 s, then doubling, up to 60 s.
type backoff struct {
	wait time.Duration
	at   time.Time
}

func (b *backoff) bump(now time.Time) {
	switch {
	case b.wait == 0:
		b.wait = 5 * time.Second
	default:
		b.wait = min(2*b.wait, 60*time.Second)
	}
	b.at = now.Add(b.wait)
}

// unboundMessage is what every tool but whoami and register_session says
// while the agent has no session.
func (rt *runtime) unboundMessage() string {
	if rt.routeErr != nil {
		return rt.routeMessage()
	}
	msg := "this agent has no ClawBridge session yet: call register_session with a name (suggested: " + rt.suggest() + ")"
	if rt.note != "" {
		msg += ". " + rt.note
	}
	return msg
}

// suggest is the name to propose: the folder's, or with -2, -3 and so on when
// that is taken. CLAWBRIDGE_SESSION leads when its record has no
// conversations.
func (rt *runtime) suggest() string {
	var recs []sessions.Record
	if rt.dir != "" {
		recs, _ = sessions.List(rt.dir)
	}
	taken := func(n string) bool {
		for _, r := range recs {
			if strings.EqualFold(r.Name, n) {
				return true
			}
		}
		return false
	}
	if s := rt.o.Suggest; s != "" && bus.ValidName(s) {
		free := !taken(s)
		for _, r := range recs {
			if r.Name == s && len(r.Conversations) == 0 {
				free = true
			}
		}
		if free {
			return s
		}
	}
	base := folderName(rt.o.Folder)
	if !taken(base) {
		return base
	}
	for i := 2; i < 1000; i++ {
		if n := base + "-" + strconv.Itoa(i); !taken(n) {
			return n
		}
	}
	return base
}

// folderName turns a folder's name into a valid session name.
func folderName(folder string) string {
	var sb strings.Builder
	for _, r := range folder {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			sb.WriteRune(r)
		default:
			sb.WriteByte('-')
		}
	}
	out := strings.TrimLeft(sb.String(), "-._")
	if len(out) > 128 {
		out = out[:128]
	}
	if !bus.ValidName(out) {
		return "agent"
	}
	return out
}

// claimMessage words a refused claim for the agent.
func (rt *runtime) claimMessage(name string, err error) string {
	var busy sessions.ErrInUse
	switch {
	case errors.As(err, &busy):
		return fmt.Sprintf("session %s is in use by another running agent (pid %d); close it, or run `clawbridge agent takeover %s` in your terminal", name, busy.PID, name)
	case errors.Is(err, sessions.ErrBelongsToAnother):
		return "session " + name + " belongs to another conversation: resume that conversation, run `clawbridge agent takeover " + name + "` in your terminal, or claim another name"
	case errors.Is(err, sessions.ErrConversationTaken):
		return "this conversation already belongs to another session; resume it under that session's name"
	case errors.Is(err, sessions.ErrNameCaseCollision):
		return "a session whose name differs from " + name + " only in case already exists; use that exact name"
	}
	return "cannot claim session " + name + ": " + err.Error()
}
