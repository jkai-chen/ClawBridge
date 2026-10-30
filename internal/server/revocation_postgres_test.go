package server

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/devicekey"
	"github.com/jackc/pgx/v5"
)

// rvFixture is two servers on one Postgres, each with its own pool, device
// cache and stream hub, and a member with a device and a session of
// project p.
type rvFixture struct {
	t         *testing.T
	dsn       string
	owner     string
	member    string
	workspace string
	hosts     [2]*httptest.Server
	hubs      [2]*StreamHub
	stores    [2]*bus.Store
	db        *sql.DB
	device    string
	priv      ed25519.PrivateKey
}

const rvOrigin = "https://cb.example.com"

// newRVFixture starts the two servers; listen false starts their hubs
// without a LISTEN connection, as a server whose notifications never come.
func newRVFixture(t *testing.T, listen bool) *rvFixture {
	t.Helper()
	dsn := os.Getenv("CLAWBRIDGE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("reviewer supplies disposable Postgres DSN")
	}
	f := &rvFixture{t: t, dsn: dsn, owner: awUnique("owner") + "@revoke-test.example", member: awUnique("member") + "@revoke-test.example", workspace: awUnique("rvws")}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	verifier := VerifierFunc(func(_ context.Context, token string) (Identity, error) {
		return Identity{Principal: strings.TrimPrefix(token, "tok-"), Subject: "sub-" + strings.TrimPrefix(token, "tok-")}, nil
	})
	for i := range f.hosts {
		store, err := bus.OpenPostgres(ctx, dsn, f.owner)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { store.Close() })
		store.RequireVerifiedAccount()
		f.stores[i] = store
		f.hubs[i] = NewStreamHub()
		if listen {
			go f.hubs[i].Listen(ctx, dsn)
		}
		f.hosts[i] = httptest.NewServer(NewHandlerWithOptions(store, verifier, HandlerOptions{Streams: f.hubs[i], Auth: AuthOptions{Methods: []string{AuthGoogle, AuthDevice}, PublicOrigin: rvOrigin, Devices: store}}))
		t.Cleanup(f.hosts[i].Close)
		t.Cleanup(f.hubs[i].Close)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	f.db = db
	f.must(0, f.owner, bus.Request{Operation: "workspace_init", Workspace: f.workspace, Args: map[string]any{"mode": "server"}})
	f.must(0, f.owner, bus.Request{Operation: "project_create", Workspace: f.workspace, Args: map[string]any{"slug": "p"}})
	invite := f.must(0, f.owner, bus.Request{Operation: "invite_create", Workspace: f.workspace, Args: map[string]any{"email": f.member, "grants": map[string]any{"p": "member"}}})
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.stores[0].Join(ctx, bus.JoinRequest{Email: f.member, Code: invite["code"].(string), PublicKey: devicekey.EncodePublicKey(pub), Label: "laptop", KeyStore: "keyring"}); err != nil {
		t.Fatal("join:", err)
	}
	f.device, f.priv = devicekey.ID(pub), priv
	f.must(0, f.member, bus.Request{Operation: "register_session", Workspace: f.workspace, Project: "p", Session: "m", Args: map[string]any{"instance": "m-inst", "role": "worker"}})
	return f
}

// post runs r through server i's /v1/execute, signed by the member's
// device when principal is "", else with principal's Google token.
func (f *rvFixture) post(i int, principal string, r bus.Request) (int, map[string]any) {
	f.t.Helper()
	body, _ := json.Marshal(r)
	req, _ := http.NewRequest(http.MethodPost, f.hosts[i].URL+"/v1/execute", strings.NewReader(string(body)))
	if principal == "" {
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		nonce := nonce(16)
		sig := ed25519.Sign(f.priv, devicekey.RequestBase(http.MethodPost, rvOrigin, "/v1/execute", ts, nonce, body))
		req.Header.Set("Authorization", fmt.Sprintf("%s v1 device=%s, ts=%s, nonce=%s, sig=%s", DeviceScheme, f.device, ts, nonce, base64.RawURLEncoding.EncodeToString(sig)))
	} else {
		req.Header.Set("Authorization", "Bearer tok-"+principal)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (f *rvFixture) must(i int, principal string, r bus.Request) map[string]any {
	f.t.Helper()
	code, out := f.post(i, principal, r)
	if code != 200 {
		f.t.Fatalf("%s: %d %v", r.Operation, code, out)
	}
	data, _ := out["data"].(map[string]any)
	return data
}

// open opens the member's observe stream of session m on server i, with
// its Google token; it starts with status.
func (f *rvFixture) open(i int) *sseReader {
	f.t.Helper()
	ticket := f.must(i, f.member, bus.Request{Operation: "stream_ticket", Workspace: f.workspace, Project: "p", Session: "m", Args: map[string]any{"instance": "m-inst", "mode": bus.StreamObserve}})["ticket"].(string)
	req, _ := http.NewRequest(http.MethodGet, f.hosts[i].URL+StreamPath+"?"+url.Values{"workspace": {f.workspace}, "project": {"p"}, "session": {"m"}, "mode": {bus.StreamObserve}}.Encode(), nil)
	req.Header.Set("Authorization", "Bearer tok-"+f.member)
	req.Header.Set(StreamTicketHeader, ticket)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		f.t.Fatal(resp.Status)
	}
	return readSSE(f.t, resp)
}

func (f *rvFixture) deviceRow() (used string, nonces int) {
	f.t.Helper()
	if err := f.db.QueryRow(`SELECT COALESCE(last_used_at,''),(SELECT COUNT(*) FROM device_nonces WHERE device_id=$1) FROM devices WHERE device_id=$1`, f.device).Scan(&used, &nonces); err != nil {
		f.t.Fatal(err)
	}
	return used, nonces
}

// A device revoked on server 0 is refused as revoked by server 1 at once,
// though server 1 cached it as live a moment before, and its refused
// request writes nothing.
func TestPostgresRevokedDeviceIsRefusedOnEveryServer(t *testing.T) {
	f := newRVFixture(t, true)
	list := bus.Request{Operation: "list_projects", Workspace: f.workspace}
	if code, out := f.post(1, "", list); code != 200 {
		t.Fatal("live device on server 1:", code, out)
	}
	f.must(0, f.owner, bus.Request{Operation: "device_revoke", Workspace: f.workspace, Args: map[string]any{"device_id": f.device}})
	used, nonces := f.deviceRow()
	code, out := f.post(1, "", list)
	if e, _ := out["error"].(map[string]any); code != 401 || e["code"] != "unauthenticated" || e["message"] != "this device was revoked or has expired; ask an owner for an invite" {
		t.Fatal("server 1 after the revoke on server 0:", code, out)
	}
	if u, n := f.deviceRow(); u != used || n != nonces {
		t.Fatalf("the refused request wrote: last used %s→%s, nonces %d→%d", used, u, nonces, n)
	}
}

// Taking access away on server 0 closes the member's stream on server 1
// through the notify, well inside its 30 s re-read: a project role revoke
// and a member's removal, neither of them a device revoke.
func TestPostgresAccessChangeClosesStreamsOnEveryServer(t *testing.T) {
	for _, op := range []bus.Request{
		{Operation: "project_role_revoke", Project: "p", Args: map[string]any{"principal": ""}},
		{Operation: "user_revoke", Args: map[string]any{"principal": ""}},
	} {
		t.Run(op.Operation, func(t *testing.T) {
			f := newRVFixture(t, true)
			s := f.open(1)
			s.must(5*time.Second, "status")
			op.Workspace, op.Args["principal"] = f.workspace, f.member
			f.must(0, f.owner, op)
			s.must(5*time.Second, "revoked")
		})
	}
}

// Without notifications, the revoking server's own wake still closes its
// streams at once; another server's waits for its re-read.
func TestAccessChangeWakesTheRevokingServerWithoutListen(t *testing.T) {
	f := newRVFixture(t, false)
	here, there := f.open(0), f.open(1)
	here.must(5*time.Second, "status")
	there.must(5*time.Second, "status")
	f.must(0, f.owner, bus.Request{Operation: "project_role_revoke", Workspace: f.workspace, Project: "p", Args: map[string]any{"principal": f.member}})
	here.must(5*time.Second, "revoked")
	there.quiet(2 * time.Second)
}

// The notify carries nothing, a rolled-back change sends none, and one
// change sends one.
func TestPostgresAccessNotifyIsEmptyAndTransactional(t *testing.T) {
	f := newRVFixture(t, true)
	conn, err := pgx.Connect(context.Background(), f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	if _, err = conn.Exec(context.Background(), "LISTEN "+pgx.Identifier{bus.AccessChannel}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	wait := func(d time.Duration) (string, bool) {
		ctx, cancel := context.WithTimeout(context.Background(), d)
		defer cancel()
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return "", false
		}
		return n.Payload, true
	}
	// A refused change rolls back: an owner can't revoke themselves.
	if code, _ := f.post(0, f.owner, bus.Request{Operation: "user_revoke", Workspace: f.workspace, Args: map[string]any{"principal": f.owner}}); code == 200 {
		t.Fatal("the owner revoked themselves")
	}
	if p, ok := wait(time.Second); ok {
		t.Fatalf("a refused change notified %q", p)
	}
	f.must(0, f.owner, bus.Request{Operation: "device_revoke", Workspace: f.workspace, Args: map[string]any{"device_id": f.device}})
	if p, ok := wait(5 * time.Second); !ok || p != "" {
		t.Fatalf("the revoke's notify: %q %v", p, ok)
	}
	if p, ok := wait(500 * time.Millisecond); ok {
		t.Fatalf("one revoke notified twice: %q", p)
	}
}

// When the hub's LISTEN connection comes back, it forgets the cached
// devices and wakes every stream, catching up on notifies it missed.
func TestPostgresListenerReconnectForgetsDevices(t *testing.T) {
	f := newRVFixture(t, true)
	var forgot atomic.Int32
	f.hubs[1].onAccess(func() { forgot.Add(1) })
	before := forgot.Load()
	if _, err := f.db.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE query LIKE 'LISTEN%' AND pid<>pg_backend_pid()`); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for forgot.Load() == before {
		if time.Now().After(deadline) {
			t.Fatal("the reconnect didn't forget the cached devices")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A burst of access notifies, as a bulk revoke sends, is applied once.
func TestAccessNotifiesCoalesce(t *testing.T) {
	h := NewStreamHub()
	var forgot atomic.Int32
	h.onAccess(func() { forgot.Add(1) })
	for range 5 {
		h.notified(bus.AccessChannel, "")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.applyAccess(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for forgot.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	if n := forgot.Load(); n != 1 {
		t.Fatal("5 notifies applied", n, "times")
	}
}
