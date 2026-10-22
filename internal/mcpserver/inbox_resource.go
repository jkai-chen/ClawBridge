package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/notify"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const InboxStatusURI = "clawbridge://inbox/status"

type Options struct {
	Taskboard    TaskboardTools
	Context      context.Context
	PollInterval time.Duration
	PollTimeout  time.Duration
	Wait         func(context.Context, time.Duration) error
}
type inboxResource struct {
	mu             sync.Mutex
	b              *binding
	server         *mcp.Server
	options        Options
	listeners      map[*mcp.ServerSession]*resourceListener
	observed       map[*mcp.ServerSession]bool
	cancel         context.CancelFunc
	epoch          uint64
	publication    notify.Status
	publicationGen uint64
	publicationCtx context.Context
}
type resourceListener struct {
	active, seen   bool
	unread, latest int64
	gen            uint64
}

func newInboxResource(b *binding, o Options) *inboxResource {
	if o.Context == nil {
		o.Context = context.Background()
	}
	if o.PollInterval <= 0 {
		o.PollInterval = 5 * time.Second
	}
	if o.PollTimeout <= 0 {
		o.PollTimeout = 5 * time.Second
	}
	if o.Wait == nil {
		o.Wait = notify.Wait
	}
	return &inboxResource{b: b, options: o, listeners: map[*mcp.ServerSession]*resourceListener{}, observed: map[*mcp.ServerSession]bool{}}
}
func (r *inboxResource) fetch(ctx context.Context, automatic bool) (bus.Result, notify.NotificationStatus, uint64, error) {
	bounded, cancel := context.WithTimeout(ctx, r.options.PollTimeout)
	defer cancel()
	for !r.b.mu.TryLock() {
		if err := notify.Wait(bounded, time.Millisecond); err != nil {
			return bus.Result{}, notify.NotificationStatus{}, r.b.generation.Load(), err
		}
	}
	defer r.b.mu.Unlock()
	if bounded.Err() != nil {
		return bus.Result{}, notify.NotificationStatus{}, r.b.generation.Load(), bounded.Err()
	}
	b := r.b
	gen := b.generation.Load()
	if b.instance == "" {
		return bus.Result{}, notify.NotificationStatus{}, gen, notify.Permanent(errors.New("register or configure an exact current instance before inbox subscription"))
	}
	w, p, s := b.workspace, b.project, b.session
	if b.statusScope.WorkspaceID != "" {
		w = b.statusScope.WorkspaceID
		p = b.statusScope.ProjectID
		s = b.statusScope.SessionID
	}
	request := bus.Request{Workspace: w, Project: p, Session: s, Args: map[string]any{"instance": b.instance, "after": int64(0)}}
	var result bus.Result
	var status notify.NotificationStatus
	var err error
	if automatic {
		result, status, err = notify.FetchNotification(bounded, b.store, request)
	} else {
		result, status.Status, err = notify.Fetch(bounded, b.store, request)
		status.Policy = "accept"
	}
	if err == nil {
		if b.statusScope.WorkspaceID != "" && b.statusScope != status.Scope() {
			return bus.Result{}, notify.NotificationStatus{}, gen, notify.Permanent(errors.New("inbox resource changed bound recipient"))
		}
		b.statusScope = status.Scope()
	}
	return result, status, gen, err
}
func (r *inboxResource) read(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	if req.Params.URI != InboxStatusURI {
		return nil, errors.New("unknown inbox status resource")
	}
	result, _, _, err := r.fetch(ctx, false)
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: InboxStatusURI, MIMEType: "application/json", Text: string(data)}}}, nil
}
func (r *inboxResource) subscribe(ctx context.Context, req *mcp.SubscribeRequest) error {
	if req.Params.URI != InboxStatusURI {
		return errors.New("unknown inbox status subscription")
	}
	_, _, gen, err := r.fetch(ctx, true)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if gen != r.b.generation.Load() {
		return errors.New("actor changed while subscribing; subscribe again explicitly")
	}
	if r.listeners[req.Session] == nil {
		r.listeners[req.Session] = &resourceListener{gen: gen}
	}
	// A session can repeatedly subscribe, unsubscribe or rotate its instance.
	// Keep one disconnect observer for its lifetime, rather than one per cycle.
	if !r.observed[req.Session] {
		r.observed[req.Session] = true
		go func() {
			_ = req.Session.Wait()
			r.remove(req.Session)
			r.mu.Lock()
			delete(r.observed, req.Session)
			r.mu.Unlock()
		}()
	}
	return nil
}

// SDK middleware starts legacy polling only after resources/subscribe has
// installed its subscription, and modern polling only after the SDK's listen
// acknowledgement. No timer or scheduling assumption establishes subscription.
func (r *inboxResource) receiving(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		result, err := next(ctx, method, req)
		if err == nil && method == "resources/subscribe" {
			if s, ok := req.GetSession().(*mcp.ServerSession); ok {
				r.activate(s)
			}
		}
		return result, err
	}
}
func (r *inboxResource) sending(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if method == "notifications/resources/updated" {
			s, ok := req.GetSession().(*mcp.ServerSession)
			if !ok {
				return nil, nil
			}
			r.mu.Lock()
			l := r.listeners[s]
			status := r.publication
			gen := r.publicationGen
			runCtx := r.publicationCtx
			if l == nil || !l.active || gen != r.b.generation.Load() || runCtx == nil || runCtx.Err() != nil || !needsNotice(l, status) {
				r.mu.Unlock()
				return nil, nil
			}
			l.seen = true
			l.unread = status.UnreadCount
			l.latest = status.LatestSequence
			r.mu.Unlock()
			bounded, cancel := context.WithCancel(ctx)
			stop := context.AfterFunc(runCtx, cancel)
			defer cancel()
			defer stop()
			return next(bounded, method, req)
		}
		result, err := next(ctx, method, req)
		if err == nil && method == "notifications/subscriptions/acknowledged" {
			if s, ok := req.GetSession().(*mcp.ServerSession); ok {
				r.activate(s)
			}
		}
		return result, err
	}
}
func needsNotice(l *resourceListener, s notify.Status) bool {
	return (!l.seen && s.UnreadCount > 0) || (l.seen && (l.unread != s.UnreadCount || l.latest != s.LatestSequence))
}
func (r *inboxResource) activate(s *mcp.ServerSession) {
	r.mu.Lock()
	defer r.mu.Unlock()
	l := r.listeners[s]
	if l == nil || l.gen != r.b.generation.Load() {
		return
	}
	l.active = true
	if r.cancel == nil {
		ctx, cancel := context.WithCancel(r.options.Context)
		r.cancel = cancel
		r.epoch++
		go r.poll(ctx, l.gen, r.epoch)
	}
}
func (r *inboxResource) unsubscribe(_ context.Context, req *mcp.UnsubscribeRequest) error {
	if req.Params.URI != InboxStatusURI {
		return errors.New("unknown inbox status subscription")
	}
	r.remove(req.Session)
	return nil
}
func (r *inboxResource) remove(s *mcp.ServerSession) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.listeners, s)
	if len(r.listeners) == 0 && r.cancel != nil {
		r.cancel()
		r.cancel = nil
		r.epoch++
	}
}
func (r *inboxResource) stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cancel != nil {
		r.cancel()
	}
	r.cancel = nil
	r.listeners = map[*mcp.ServerSession]*resourceListener{}
	r.epoch++
}
func (r *inboxResource) poll(ctx context.Context, gen, epoch uint64) {
	defer func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.epoch == epoch {
			r.cancel = nil
			r.listeners = map[*mcp.ServerSession]*resourceListener{}
			r.epoch++
		}
	}()
	delay := r.options.PollInterval
	first := true
	for {
		if (!first && r.options.Wait(ctx, delay) != nil) || ctx.Err() != nil {
			return
		}
		first = false
		_, status, current, err := r.fetch(ctx, true)
		if ctx.Err() != nil || current != gen {
			return
		}
		if err != nil {
			if notify.IsPermanent(err) {
				return
			}
			delay = min(60*time.Second, max(time.Second, delay*2))
			continue
		}
		delay = r.options.PollInterval
		r.mu.Lock()
		if r.epoch != epoch || gen != r.b.generation.Load() || len(r.listeners) == 0 || ctx.Err() != nil {
			r.mu.Unlock()
			return
		}
		if status.Policy != "accept" {
			r.mu.Unlock()
			continue
		}
		r.publication = status.Status
		r.publicationGen = gen
		r.publicationCtx = ctx
		changed := false
		for _, l := range r.listeners {
			if l.active {
				if needsNotice(l, status.Status) {
					changed = true
				} else if !l.seen {
					l.seen = true
					l.unread = status.UnreadCount
					l.latest = status.LatestSequence
				}
			}
		}
		r.mu.Unlock()
		if changed {
			_ = r.server.ResourceUpdated(ctx, &mcp.ResourceUpdatedNotificationParams{URI: InboxStatusURI})
		}
	}
}
