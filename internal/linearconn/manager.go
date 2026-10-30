package linearconn

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/zalando/go-keyring"
	"golang.org/x/oauth2"

	"clawbridge/internal/linearpace"
)

const maxFile = 128 << 10

var idPattern = regexp.MustCompile(`^lin_[0-9a-f]{32}$`)
var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var locks sync.Map

type Manager struct{ config Config }
type credential struct {
	Version      int          `json:"version"`
	Token        oauth2.Token `json:"token"`
	ClientID     string       `json:"client_id,omitempty"`
	ClientSecret string       `json:"client_secret,omitempty"`
	RedirectURL  string       `json:"redirect_url,omitempty"`
	Scopes       []string     `json:"scopes,omitempty"`
	Imported     bool         `json:"imported"`
}
type index struct {
	Version  int       `json:"version"`
	Profiles []Profile `json:"profiles"`
}

func New(c Config) *Manager {
	if c.Store == nil {
		c.Store = NativeStore{}
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return &Manager{config: c}
}
func mutex(key string) *sync.Mutex {
	m, _ := locks.LoadOrStore(key, &sync.Mutex{})
	return m.(*sync.Mutex)
}
func (m *Manager) dir() (string, error) {
	d := m.config.Directory
	if d == "" {
		base, e := os.UserConfigDir()
		if e != nil {
			return "", problem("profile storage unavailable")
		}
		d = filepath.Join(base, "clawbridge", "linear")
	}
	if !filepath.IsAbs(d) {
		return "", problem("profile directory must be absolute")
	}
	return filepath.Clean(d), nil
}
func safeText(s string, max int) bool {
	return len(s) <= max && !strings.ContainsFunc(s, func(r rune) bool { return unicode.IsControl(r) })
}
func validAccess(a Access) bool { return a == Read || a == Write }
func (m *Manager) load() (index, error) {
	d, e := m.dir()
	if e != nil {
		return index{}, e
	}
	if _, e = os.Lstat(d); errors.Is(e, os.ErrNotExist) {
		return index{Version: 1, Profiles: []Profile{}}, nil
	} else if e != nil {
		return index{}, problem("profile storage unavailable")
	}
	if e = privatePath(d, true); e != nil {
		return index{}, problem("unsafe profile directory")
	}
	p := filepath.Join(d, "profiles.json")
	f, e := os.Open(p)
	if errors.Is(e, os.ErrNotExist) {
		return index{Version: 1, Profiles: []Profile{}}, nil
	}
	if e != nil {
		return index{}, problem("profile storage unavailable")
	}
	defer f.Close()
	ls, e := os.Lstat(p)
	if e != nil || privatePath(p, false) != nil {
		return index{}, problem("unsafe profile storage")
	}
	fs, e := f.Stat()
	if e != nil || !os.SameFile(ls, fs) {
		return index{}, problem("unsafe profile storage")
	}
	b, e := io.ReadAll(io.LimitReader(f, maxFile+1))
	if e != nil || len(b) > maxFile {
		return index{}, problem("invalid profile storage")
	}
	var x index
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if dec.Decode(&x) != nil || dec.Decode(new(any)) != io.EOF || x.Version != 1 || len(x.Profiles) > 100 {
		return index{}, problem("invalid profile storage")
	}
	seen := map[string]bool{}
	for _, p := range x.Profiles {
		if !validProfile(p) || seen[p.ID] {
			return index{}, problem("invalid profile storage")
		}
		seen[p.ID] = true
	}
	return x, nil
}
func validProfile(p Profile) bool {
	return idPattern.MatchString(p.ID) && p.Name != "" && safeText(p.Name, 128) && validAccess(p.Access) && safeText(p.AccountLabel, 256) && safeText(p.WorkspaceLabel, 256) && (p.WorkspaceID == "" || uuidPattern.MatchString(p.WorkspaceID))
}
func (m *Manager) privateDir() (string, error) {
	d, e := m.dir()
	if e != nil {
		return "", e
	}
	_, e = os.Lstat(d)
	if errors.Is(e, os.ErrNotExist) {
		if e = os.MkdirAll(d, 0700); e != nil {
			return "", problem("profile storage unavailable")
		}
		if e = makePrivate(d); e != nil {
			return "", problem("cannot protect profile directory")
		}
	} else if e != nil {
		return "", problem("profile storage unavailable")
	}
	if e = privatePath(d, true); e != nil {
		return "", problem("unsafe profile directory")
	}
	return d, nil
}
func (m *Manager) save(x index) error {
	d, e := m.privateDir()
	if e != nil {
		return e
	}
	p := filepath.Join(d, "profiles.json")
	if _, e := os.Lstat(p); e == nil {
		if e = privatePath(p, false); e != nil {
			return problem("unsafe profile storage")
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return problem("profile storage unavailable")
	}
	b, e := json.Marshal(x)
	if e != nil || len(b) > maxFile {
		return problem("profile limit exceeded")
	}
	return writePrivate(d, "profiles.json", "profile storage", b)
}

// writePrivate replaces d/name with b through a private temporary file,
// synced and renamed into place. A failure names what, never b.
func writePrivate(d, name, what string, b []byte) error {
	p := filepath.Join(d, name)
	f, e := os.CreateTemp(d, "."+strings.TrimSuffix(name, ".json")+"-*")
	if e != nil {
		return problem(what + " unavailable")
	}
	defer os.Remove(f.Name())
	if e = makePrivate(f.Name()); e != nil {
		f.Close()
		return problem("cannot protect " + what)
	}
	if e = privatePath(f.Name(), false); e != nil {
		f.Close()
		return problem("unsafe staged " + what)
	}
	if _, e = f.Write(b); e != nil {
		f.Close()
		return problem(what + " unavailable")
	}
	if e = f.Sync(); e != nil {
		f.Close()
		return problem(what + " unavailable")
	}
	if e = f.Close(); e != nil {
		return problem(what + " unavailable")
	}
	if e = os.Rename(f.Name(), p); e != nil {
		return problem(what + " unavailable")
	}
	if e = syncDirectory(d); e != nil {
		return problem(what + " durability uncertain")
	}
	return nil
}
func (m *Manager) Profiles() ([]Profile, error) {
	d, e := m.dir()
	if e != nil {
		return nil, e
	}
	mu := mutex(d)
	mu.Lock()
	defer mu.Unlock()
	x, e := m.load()
	for i := range x.Profiles {
		// Best effort for a listing: Status reports an unreadable record.
		if store, err := m.recorded(x.Profiles[i].ID); err == nil && store == StoreFile {
			x.Profiles[i].KeyStore = StoreFile
		}
	}
	return x.Profiles, e
}
func (m *Manager) profile(id string) (Profile, error) {
	if !idPattern.MatchString(id) {
		return Profile{}, problem("invalid profile")
	}
	x, e := m.load()
	if e != nil {
		return Profile{}, e
	}
	for _, p := range x.Profiles {
		if p.ID == id {
			store, e := m.recorded(id)
			if e != nil {
				return Profile{}, e
			}
			if store == StoreFile {
				p.KeyStore = StoreFile
			}
			return p, nil
		}
	}
	return Profile{}, problem("profile not found")
}

// Status reads only local profile/secure-store state; it does not contact Linear
// or prove that a provider session remains authorized.
func (m *Manager) Status(ctx context.Context, id string) (Profile, error) {
	if e := ctx.Err(); e != nil {
		return Profile{}, e
	}
	d, e := m.dir()
	if e != nil {
		return Profile{}, e
	}
	mu := mutex(d)
	if e = lockContext(ctx, mu); e != nil {
		return Profile{}, e
	}
	defer mu.Unlock()
	p, e := m.profile(id)
	if e != nil {
		return Profile{}, e
	}
	_, e = m.readFrom(p.Store(), id)
	if err := ctx.Err(); err != nil {
		return Profile{}, err
	}
	return p, e
}

func (m *Manager) readFrom(store, id string) (credential, error) {
	var raw string
	if store == StoreFile {
		v, e := m.fileGet(id)
		if e != nil {
			return credential{}, e
		}
		raw = v
	} else {
		v, e := m.config.Store.Get(Service, id)
		if e != nil {
			return credential{}, &Error{Kind: "secure credential unavailable", Detail: keyStoreSaid(e)}
		}
		raw = v
	}
	if len(raw) > maxFile {
		return credential{}, problem("invalid secure credential")
	}
	var c credential
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&c) != nil || dec.Decode(new(any)) != io.EOF || c.Version != 1 || !validToken(c.Token.AccessToken) || (c.Token.TokenType != "" && !strings.EqualFold(c.Token.TokenType, "Bearer")) {
		return credential{}, problem("invalid secure credential")
	}
	return c, nil
}
func validToken(s string) bool {
	return s != "" && len(s) <= 16384 && !strings.ContainsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) })
}

func (m *Manager) writeTo(store, id string, c credential) error {
	b, e := json.Marshal(c)
	if e != nil || len(b) > maxFile {
		return problem("invalid secure credential")
	}
	if store == StoreFile {
		return m.fileSet(id, string(b))
	}
	if e = m.config.Store.Set(Service, id, string(b)); e != nil {
		return &Error{Kind: "secure credential storage failed", Detail: keyringDetail(e)}
	}
	return nil
}

// deleteFrom removes a credential; one already gone is not an error.
func (m *Manager) deleteFrom(store, id string) error {
	if store == StoreFile {
		return m.fileDelete(id)
	}
	if e := m.config.Store.Delete(Service, id); e != nil && !errors.Is(e, keyring.ErrNotFound) {
		return &Error{Kind: "secure credential deletion failed", Detail: keyStoreSaid(e)}
	}
	return nil
}

// newStore is where a new or reconnected credential goes: the store asked
// for, else a reconnected profile's own, else the keyring. It is checked
// before anything is asked of Linear or the person, and a missing OS key
// store is named with the way out: nothing falls back to a file.
func (m *Manager) newStore(ctx context.Context, asked string, existing *Profile) (string, error) {
	store := asked
	if store != "" && !ValidStore(store) {
		return "", &Error{Kind: "invalid connection parameters", Detail: "--key-store is keyring or file"}
	}
	if store == "" && existing != nil {
		store = existing.Store()
	}
	if store == "" {
		store = StoreKeyring
	}
	switch store {
	case StoreFile:
		if e := m.fileStoreUsable(); e != nil {
			return "", e
		}
	case StoreKeyring:
		if m.config.KeyringCheck == nil {
			break
		}
		if missing := m.config.KeyringCheck(ctx); missing != nil {
			if e := ctx.Err(); e != nil {
				return "", e
			}
			return "", &Error{Kind: "secure credential storage failed", Detail: keyringDetail(errNoSecretService)}
		}
	}
	return store, nil
}

func (m *Manager) commit(ctx context.Context, name, id string, access Access, store string, c credential) (Profile, error) {
	if !validAccess(access) || name == "" || !safeText(name, 128) {
		return Profile{}, problem("invalid profile parameters")
	}
	if m.config.Verify == nil {
		return Profile{}, problem("provider verification unavailable")
	}
	client := m.tokenClient(c.Token.AccessToken)
	identity, e := m.config.Verify(ctx, client, access)
	if e != nil {
		return Profile{}, problem("provider capability verification failed")
	}
	if e = ctx.Err(); e != nil {
		return Profile{}, e
	}
	if id == "" {
		var b [16]byte
		if _, e = rand.Read(b[:]); e != nil {
			return Profile{}, problem("randomness unavailable")
		}
		id = "lin_" + hex.EncodeToString(b[:])
	}
	p := Profile{ID: id, Name: name, Access: access, AccountLabel: identity.AccountLabel, WorkspaceID: identity.WorkspaceID, WorkspaceLabel: identity.WorkspaceLabel}
	if store == StoreFile {
		p.KeyStore = StoreFile
	}
	if !validProfile(p) {
		return Profile{}, problem("invalid verified profile metadata")
	}
	d, e := m.privateDir()
	if e != nil {
		return Profile{}, e
	}
	mu := mutex(d)
	if e = lockContext(ctx, mu); e != nil {
		return Profile{}, e
	}
	defer mu.Unlock()
	unlock, e := m.diskLock(ctx)
	if e != nil {
		return Profile{}, e
	}
	defer unlock()
	if e = ctx.Err(); e != nil {
		return Profile{}, e
	}
	x, e := m.load()
	if e != nil {
		return Profile{}, e
	}
	found := -1
	for i, v := range x.Profiles {
		if v.ID == id {
			found = i
		}
	}
	var old credential
	oldStore := ""
	if found >= 0 {
		if oldStore, e = m.recorded(id); e != nil {
			return Profile{}, e
		}
		old, e = m.readFrom(oldStore, id)
		if e != nil {
			return Profile{}, e
		}
	}
	if found < 0 && len(x.Profiles) >= 100 {
		return Profile{}, problem("profile limit exceeded")
	}
	if e = ctx.Err(); e != nil {
		return Profile{}, e
	}
	if store == StoreFile && oldStore != StoreFile {
		// The record goes first: a file credential is never unrecorded.
		if e = m.record(id); e != nil {
			return Profile{}, e
		}
	}
	if e = m.writeTo(store, id, c); e != nil {
		if store == StoreFile && oldStore != StoreFile {
			_ = m.unrecord(id)
		}
		return Profile{}, e
	}
	if found < 0 {
		x.Profiles = append(x.Profiles, p)
	} else {
		x.Profiles[found] = p
	}
	if e = m.save(x); e != nil {
		if oldStore == store {
			_ = m.writeTo(store, id, old)
		} else {
			_ = m.deleteFrom(store, id)
			if store == StoreFile {
				_ = m.unrecord(id)
			}
		}
		return Profile{}, e
	}
	if oldStore != "" && oldStore != store {
		// Moved to another store: the old copy goes, the file's record
		// first, so the profile reads its new store. A failure leaves the
		// old copy unused.
		if oldStore == StoreFile && m.unrecord(id) != nil {
			return Profile{}, problem("connection store record removal failed")
		}
		_ = m.deleteFrom(oldStore, id)
	}
	return p, nil
}

// Import reads a bounded token synchronously. A supplied io.ReadCloser is closed
// on cancellation; CLI callers transfer their owned stdin for this purpose. Plain
// Readers must be finite or caller-unblocked: Import never starts a detached read
// goroutine around an unowned blocking Reader.
func (m *Manager) Import(ctx context.Context, name string, access Access, r io.Reader) (Profile, error) {
	return m.ImportTo(ctx, name, access, "", r)
}

// ImportTo is Import into store: StoreKeyring (or empty) or StoreFile.
func (m *Manager) ImportTo(ctx context.Context, name string, access Access, store string, r io.Reader) (Profile, error) {
	if !validAccess(access) || name == "" || !safeText(name, 128) || r == nil {
		return Profile{}, problem("invalid profile parameters")
	}
	if e := ctx.Err(); e != nil {
		return Profile{}, e
	}
	store, e := m.newStore(ctx, store, nil)
	if e != nil {
		return Profile{}, e
	}
	if c, ok := r.(io.ReadCloser); ok {
		stop := context.AfterFunc(ctx, func() { _ = c.Close() })
		defer stop()
	}
	b, e := io.ReadAll(io.LimitReader(r, 16385))
	if err := ctx.Err(); err != nil {
		return Profile{}, err
	}
	if e != nil {
		return Profile{}, problem("token input failed")
	}
	if e = ctx.Err(); e != nil {
		return Profile{}, e
	}
	token := strings.TrimSpace(string(b))
	if len(b) > 16384 || !validToken(token) {
		return Profile{}, problem("invalid token input")
	}
	return m.commit(ctx, name, "", access, store, credential{Version: 1, Token: oauth2.Token{AccessToken: token, TokenType: "Bearer"}, Imported: true})
}
func (m *Manager) Logout(ctx context.Context, id string) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	d, e := m.dir()
	if e != nil {
		return e
	}
	mu := mutex(d)
	if e = lockContext(ctx, mu); e != nil {
		return e
	}
	defer mu.Unlock()
	unlock, e := m.diskLock(ctx)
	if e != nil {
		return e
	}
	defer unlock()
	if e = ctx.Err(); e != nil {
		return e
	}
	x, e := m.load()
	if e != nil {
		return e
	}
	at := -1
	for i, p := range x.Profiles {
		if p.ID == id {
			at = i
		}
	}
	if at < 0 {
		return problem("profile not found")
	}
	if e = ctx.Err(); e != nil {
		return e
	}
	// A credential already missing from the store is already logged out; the
	// profile must still be removable.
	store, e := m.recorded(id)
	if e != nil {
		return e
	}
	if e = m.deleteFrom(store, id); e != nil {
		return e
	}
	if e = m.unrecord(id); e != nil {
		return e
	}
	x.Profiles = append(x.Profiles[:at], x.Profiles[at+1:]...)
	return m.save(x)
}

// HTTPClient creates a fixed-origin authenticated transport without authorization
// prompts. Each request reloads native credentials; logout invalidates existing
// clients. Refresh is serialized and persisted before the new token is used.
func (m *Manager) HTTPClient(ctx context.Context, id string) (*http.Client, error) {
	p, e := m.Status(ctx, id)
	if e != nil {
		return nil, e
	}
	base := m.baseClient()
	// One pacer per profile in this process: its calls share the person's
	// Linear rate limit.
	base.Transport = linearpace.Transport{Base: &profileTransport{m: m, id: p.ID, access: p.Access}, Pacer: linearpace.For("profile:"+p.ID, linearpace.OAuthPerHour)}
	return base, nil
}
