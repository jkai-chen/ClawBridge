package cli

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"clawbridge/internal/bus"
	"clawbridge/internal/devicekey"
	"clawbridge/internal/devicesign"
	"clawbridge/internal/remote"
	"github.com/pelletier/go-toml/v2"
)

// ClawBridge's own sign-in on the client (live/AUTH-INVITE-DESIGN.md §3, §4,
// §6, §9): join with an invite, and the invite, device, user and owner
// commands.

const accountUsage = `usage:
  clawbridge --server URL join --email EMAIL [--label LABEL] [--key-store keyring|file]
      Joins a hosted workspace with an invite. It asks for the code without echoing it, or reads it
      from stdin when that isn't a terminal. (--code CODE exists for scripts, but a command line is kept
      in shell history and shown to other users' process lists.)
  clawbridge invite --email EMAIL --grant PROJECT=ROLE [--grant PROJECT=ROLE]... [--expires 72h]
  clawbridge invite --email EMAIL --recovery [--revoke-others]     (workspace owner)
  clawbridge invite list | invite revoke INVITE_ID
  clawbridge device list [--member EMAIL] | device revoke DEVICE_ID [--reason TEXT] | device label DEVICE_ID LABEL
  clawbridge device add [--expires 24h]       (an invite for another device of yours; join with it there)
  clawbridge device add --ci NAME --grant PROJECT=reader|member [--grant ...]... --expires 30d [--label LABEL]
  clawbridge device enroll [--label LABEL] [--key-store keyring|file]   (signed in with Google: no invite)
  clawbridge device events [--all] | device ack EVENT_ID... | device ack --all
      Devices added to or revoked from your account (every member's, for the workspace owner), shown on
      this device until it acknowledges them. Any command run here prints them on stderr too.
  clawbridge user list | user revoke EMAIL --confirm
  clawbridge owner show | owner transfer --to EMAIL | owner accept | owner decline | owner cancel
  invite and device add print their code, or a CI device's key, only to a terminal; --out FILE writes
  it to a new file instead (0600, never over an existing one).`

// stringList is a repeatable flag.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

// grantsFrom reads repeated --grant PROJECT=ROLE flags. (--project is a
// global flag, so a grant names its project itself.)
func grantsFrom(grants []string) (map[string]any, error) {
	if len(grants) == 0 {
		return nil, nil
	}
	out := map[string]any{}
	for _, g := range grants {
		project, role, ok := strings.Cut(g, "=")
		if !ok || project == "" || role == "" {
			return nil, errors.New("--grant is PROJECT=ROLE, such as api=member")
		}
		if _, dup := out[project]; dup {
			return nil, errors.New("--grant names " + project + " twice")
		}
		out[project] = role
	}
	return out, nil
}

// wholeHours reads a duration given in hours or days ("72h", "3d").
func wholeHours(v string) (int64, error) {
	if n, ok := strings.CutSuffix(v, "d"); ok {
		var days int64
		if _, err := fmt.Sscan(n, &days); err != nil || fmt.Sprint(days) != n {
			return 0, errors.New("--expires takes hours or days, such as 72h or 30d")
		}
		return days * 24, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 || d%time.Hour != 0 {
		return 0, errors.New("--expires takes whole hours or days, such as 72h or 30d")
	}
	return int64(d / time.Hour), nil
}

func parseAccountFlags(f *flag.FlagSet, args []string, min, max int) ([]string, error) {
	ordered, err := interspersedFlags(f, args)
	if err != nil {
		return nil, err
	}
	if err = f.Parse(ordered); err != nil {
		return nil, err
	}
	if f.NArg() < min || f.NArg() > max {
		return nil, fmt.Errorf("%s expects %d–%d positional arguments", f.Name(), min, max)
	}
	return f.Args(), nil
}

// newDeviceKey makes a device key; tests see which.
var newDeviceKey = devicesign.NewKey

// accountExecutor runs the account commands' operations.
type accountExecutor interface {
	Execute(context.Context, bus.Request) (bus.Result, error)
}

// newAccountExecutor is the hosted client the account commands use; tests
// replace it.
var newAccountExecutor = func(c Config) (accountExecutor, error) { return remote.New(remoteConfig(c)) }

// runAccount runs the invite, device, user and owner commands, which act on
// the configured workspace as a whole.
func runAccount(ctx context.Context, flags Config, command string, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		_, err := fmt.Fprintln(stdout, accountUsage)
		return err
	}
	config, _, err := resolveConfig(ctx, flags)
	if err != nil {
		return err
	}
	if config.Mode != "cloud" {
		return errors.New(command + " works on a hosted workspace; local mode has no invites or devices")
	}
	if config.Workspace == "" {
		return errors.New(command + " needs --workspace or a configured workspace")
	}
	f := flag.NewFlagSet(command, flag.ContinueOnError)
	f.SetOutput(stderr)
	sub := args[0]
	if command == "invite" && strings.HasPrefix(sub, "-") {
		sub = "create"
	} else {
		args = args[1:]
	}
	var grantFlags stringList
	op, a, out := "", map[string]any{}, ""
	var ciKey ed25519.PrivateKey
	switch command + " " + sub {
	case "invite create":
		email := f.String("email", "", "the invited email")
		f.Var(&grantFlags, "grant", "PROJECT=ROLE to grant: owner, lead, member or reader (repeatable)")
		expires := f.String("expires", "", "how long the invite lasts (default 72h, at most 168h)")
		recovery := f.Bool("recovery", false, "a recovery invite for an existing member (workspace owner)")
		revokeOthers := f.Bool("revoke-others", false, "with --recovery: revoke the member's other devices when it is used")
		f.StringVar(&out, "out", "", "write the code to this new file (0600, never over an existing one) instead of this terminal")
		if _, err = parseAccountFlags(f, args, 0, 0); err != nil {
			return err
		}
		grants, e := grantsFrom(grantFlags)
		if e != nil {
			return e
		}
		op, a["email"], a["purpose"] = "invite_create", *email, "user"
		if *recovery {
			a["purpose"] = "recovery"
			a["revoke_others"] = *revokeOthers
		}
		if grants != nil {
			a["grants"] = grants
		}
		if *expires != "" {
			if a["expires_hours"], err = wholeHours(*expires); err != nil {
				return err
			}
		}
	case "invite list":
		op = "invite_list"
		if _, err = parseAccountFlags(f, args, 0, 0); err != nil {
			return err
		}
	case "invite revoke":
		pos, e := parseAccountFlags(f, args, 1, 1)
		if e != nil {
			return e
		}
		op, a["invite_id"] = "invite_revoke", pos[0]
	case "device list":
		who := f.String("member", "", "another member's devices (workspace owner)")
		if _, err = parseAccountFlags(f, args, 0, 0); err != nil {
			return err
		}
		op, a["principal"] = "device_list", *who
	case "device revoke":
		reason := f.String("reason", "", "why, kept in the audit")
		pos, e := parseAccountFlags(f, args, 1, 1)
		if e != nil {
			return e
		}
		op, a["device_id"], a["reason"] = "device_revoke", pos[0], *reason
	case "device label":
		pos, e := parseAccountFlags(f, args, 2, 2)
		if e != nil {
			return e
		}
		op, a["device_id"], a["label"] = "device_label", pos[0], pos[1]
	case "device add":
		ci := f.String("ci", "", "make a CI device with this name")
		f.Var(&grantFlags, "grant", "with --ci: PROJECT=reader|member (repeatable)")
		expires := f.String("expires", "", "how long the invite (default 24h), or the CI device (required, 1d-90d), lasts")
		label := f.String("label", "", "with --ci: the device's label")
		f.StringVar(&out, "out", "", "write the code, or the CI device's key, to this new file (0600, never over an existing one) instead of this terminal")
		if _, err = parseAccountFlags(f, args, 0, 0); err != nil {
			return err
		}
		grants, e := grantsFrom(grantFlags)
		if e != nil {
			return e
		}
		if *ci == "" {
			if grants != nil || *label != "" {
				return errors.New("--grant and --label go with --ci; another device of yours gets your roles")
			}
			op, a["email"], a["purpose"] = "invite_create", config.Principal, "device"
			if *expires != "" {
				if a["expires_hours"], err = wholeHours(*expires); err != nil {
					return err
				}
			}
			break
		}
		if *expires == "" {
			return errors.New("a CI device needs --expires, 1d to 90d")
		}
		hours, e := wholeHours(*expires)
		if e != nil {
			return e
		}
		if hours%24 != 0 {
			return errors.New("a CI device's --expires is in days, such as 30d")
		}
		key, _, e := newDeviceKey()
		if e != nil {
			return e
		}
		ciKey = key
		op = "device_ci_create"
		a["name"], a["grants"], a["expires_days"], a["label"] = *ci, grants, hours/24, *label
		a["public_key"] = devicekey.EncodePublicKey(key.Public().(ed25519.PublicKey))
	case "device events":
		all := f.Bool("all", false, "include the events this device has acknowledged")
		if _, err = parseAccountFlags(f, args, 0, 0); err != nil {
			return err
		}
		op, a["all"] = "device_events", *all
	case "device ack":
		all := f.Bool("all", false, "acknowledge every event shown on this device")
		pos, e := parseAccountFlags(f, args, 0, 100)
		if e != nil {
			return e
		}
		if *all == (len(pos) > 0) {
			return errors.New("name the event IDs to acknowledge, or --all")
		}
		op = "device_ack"
		if *all {
			a["all"] = true
		} else {
			ids := []any{}
			for _, id := range pos {
				ids = append(ids, id)
			}
			a["event_ids"] = ids
		}
	case "device enroll":
		return runEnroll(ctx, config, f, args, stdout)
	case "user list":
		op = "user_list"
		if _, err = parseAccountFlags(f, args, 0, 0); err != nil {
			return err
		}
	case "user revoke":
		confirm := f.Bool("confirm", false, "confirm revoking every device and role of this member")
		pos, e := parseAccountFlags(f, args, 1, 1)
		if e != nil {
			return e
		}
		if !*confirm {
			return errors.New("user revoke removes the member, their roles and their devices here; repeat with --confirm")
		}
		op, a["principal"] = "user_revoke", pos[0]
	case "owner show":
		op = "owner_transfer_get"
		if _, err = parseAccountFlags(f, args, 0, 0); err != nil {
			return err
		}
	case "owner transfer":
		to := f.String("to", "", "the member to offer ownership to")
		if _, err = parseAccountFlags(f, args, 0, 0); err != nil {
			return err
		}
		op, a["to"] = "owner_transfer_offer", *to
	case "owner accept", "owner decline", "owner cancel":
		if _, err = parseAccountFlags(f, args, 0, 0); err != nil {
			return err
		}
		op, a["action"] = "owner_transfer_respond", sub
	default:
		return fmt.Errorf("unknown %s command %q; see clawbridge %s --help", command, sub, command)
	}
	// G9 F2: an invite code or a CI key goes to this terminal or a new
	// file, decided before it exists.
	var secret *secretOut
	if op == "invite_create" || op == "device_ci_create" {
		if secret, err = openSecretOut("clawbridge "+command+" "+sub, out, stdout); err != nil {
			return err
		}
	}
	executor, err := newAccountExecutor(config)
	if err != nil {
		secret.discard()
		return err
	}
	result, err := executor.Execute(ctx, bus.Request{Operation: op, Workspace: config.Workspace, Args: a})
	if err != nil {
		secret.discard()
		return err
	}
	data, _ := result.Data.(map[string]any)
	switch {
	case ciKey != nil:
		// The private key is shown once, here, for the CI secret store; the
		// server holds only the public key.
		if data == nil {
			secret.discard()
			return errors.New("the server's answer for the CI device was unreadable; revoke it and create another")
		}
		note := "Store " + devicesign.EnvKey + " in the CI secret store now; it is not shown again. CI runs with CLAWBRIDGE_CREDENTIAL_SOURCE=device, CLAWBRIDGE_KEY_STORE=env, CLAWBRIDGE_DEVICE_ID and CLAWBRIDGE_PRINCIPAL set to this device's."
		if secret.toFile() {
			if err = secret.write(devicesign.EnvKey + "=" + devicesign.EncodeKey(ciKey) + "\n"); err != nil {
				// The key reached no file: revoke the device rather than
				// leave it live.
				_, _ = executor.Execute(ctx, bus.Request{Operation: "device_revoke", Workspace: config.Workspace, Args: map[string]any{"device_id": data["device_id"], "reason": "its key could not be written"}})
				return fmt.Errorf("%w; the new CI device was revoked", err)
			}
			data["key_file"] = secret.path
			note = "Wrote " + devicesign.EnvKey + " to " + secret.path + " (0600): move it into the CI secret store and delete the file. " + note[strings.Index(note, "CI runs"):]
		} else {
			data[devicesign.EnvKey] = devicesign.EncodeKey(ciKey)
		}
		data["note"] = note
	case op == "invite_create" && secret.toFile():
		code, _ := data["code"].(string)
		if code == "" {
			secret.discard()
			return errors.New("the server's answer had no invite code; revoke the invite and create another")
		}
		if err = secret.write(code + "\n"); err != nil {
			_, _ = executor.Execute(ctx, bus.Request{Operation: "invite_revoke", Workspace: config.Workspace, Args: map[string]any{"invite_id": data["invite_id"]}})
			return fmt.Errorf("%w; the new invite was revoked", err)
		}
		delete(data, "code")
		data["code_file"] = secret.path
		data["note"] = "Wrote the code to " + secret.path + " (0600). Give it to the invitee privately, then delete the file; `clawbridge join` reads it from stdin."
	}
	safeLabels(result.Data)
	return json.NewEncoder(stdout).Encode(result)
}

// safeLabels escapes terminal controls in every device label of an answer
// before it is printed: whoever adds a device chooses its label (§17.3, R1),
// and a label written before the server checked them could hold any.
func safeLabels(v any) {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			if s, ok := x.(string); ok && k == "label" {
				t[k] = wizardSafe(s)
			} else {
				safeLabels(x)
			}
		}
	case []any:
		for _, x := range t {
			safeLabels(x)
		}
	}
}

// deviceEventLine is one device event as the line printed on stderr.
func deviceEventLine(e map[string]any) string {
	s := func(k string) string { v, _ := e[k].(string); return wizardSafe(v) }
	at := s("created_at")
	if t, err := time.Parse(time.RFC3339Nano, at); err == nil {
		at = t.Local().Format("2006-01-02 15:04")
	}
	route := strings.ReplaceAll(s("route"), "_", " ")
	switch s("kind") {
	case "added":
	case "revoked":
		return fmt.Sprintf("Device \"%s\" (%s) of %s was revoked at %s by %s. Not expected? Check `clawbridge device list` and tell the workspace owner. [%s]", s("label"), s("device_id"), s("principal"), at, s("actor"), s("event_id"))
	default:
		// A kind this client doesn't know is shown as it is, never as a
		// new device.
		return fmt.Sprintf("Device \"%s\" (%s) of %s: %s at %s [%s]", s("label"), s("device_id"), s("principal"), strings.ReplaceAll(s("kind"), "_", " "), at, s("event_id"))
	}
	return fmt.Sprintf("New device \"%s\" (%s) joined %s at %s via %s. Not you? clawbridge device revoke %s [%s]", s("label"), s("device_id"), s("principal"), at, route, s("device_id"), s("event_id"))
}

// shownDeviceEvents is how many device events a command prints: the
// newest, so a flood of events can't scroll the one that matters away.
const shownDeviceEvents = 5

// printDeviceEvents prints this device's newest unacknowledged device
// events, one line each, and how to acknowledge them.
func printDeviceEvents(stderr io.Writer) func([]map[string]any) {
	return func(events []map[string]any) {
		for i, e := range events {
			if i == shownDeviceEvents {
				fmt.Fprintf(stderr, "…and %d more: clawbridge device events\n", len(events)-i)
				break
			}
			fmt.Fprintln(stderr, deviceEventLine(e))
		}
		fmt.Fprintln(stderr, "Seen them? clawbridge device ack --all (or device ack EVENT_ID); clawbridge device events lists them.")
	}
}

// runEnroll adds a device key to this machine while signed in with Google:
// no invite, the verified Google account is the proof (§10). The profile
// then signs with the key; the Google credential stays until logout.
func runEnroll(ctx context.Context, config Config, f *flag.FlagSet, args []string, stdout io.Writer) error {
	label := f.String("label", "", "the device's label (default: this machine's name)")
	store := f.String("key-store", "", "where the key is kept: keyring (the default), or file")
	_, err := parseAccountFlags(f, args, 0, 0)
	if err != nil {
		return err
	}
	if config.CredentialSource == remote.CredentialSourceDevice {
		return errors.New("this profile already signs with device " + config.DeviceID)
	}
	// R85-H1: Run reads --key-store as the global flag.
	if *store, err = newKeyStore(*store, config.KeyStore); err != nil {
		return err
	}
	name, err := deviceLabel(*label)
	if err != nil {
		return err
	}
	home, err := stateHome()
	if err != nil {
		return err
	}
	// R85-M1: the server must take device keys at this very origin, or the
	// profile would switch to a route every request then fails on.
	origin, err := enrollServer(ctx, config.Server, nil)
	if err != nil {
		return err
	}
	u, _ := url.Parse(origin)
	executor, err := newAccountExecutor(config)
	if err != nil {
		return err
	}
	key, id, err := newDeviceKey()
	if err != nil {
		return err
	}
	where := devicesign.Location{Store: *store, Host: u.Host, Home: home, DeviceID: id}
	if err = devicesign.Save(where, key); err != nil {
		return err
	}
	result, err := executor.Execute(ctx, bus.Request{Operation: "device_enroll", Workspace: config.Workspace, Args: map[string]any{
		"public_key": devicekey.EncodePublicKey(key.Public().(ed25519.PublicKey)), "label": name, "key_store": *store,
	}})
	if err != nil {
		var refused *remote.HTTPError
		// Refused, or never sent because the Google credential failed
		// first (R85-L2): the key is no device's, so it goes.
		if errors.As(err, &refused) || remote.IsCredentialFailure(err) {
			_ = devicesign.Delete(where)
			return err
		}
		return fmt.Errorf("the enrolment's outcome is unknown; the key for %s was kept in its store: %w", id, err)
	}
	data, _ := result.Data.(map[string]any)
	if data == nil || data["device_id"] != id {
		return fmt.Errorf("the server's answer names another device; the key for %s was kept in its store and the profile was not changed", id)
	}
	route := map[string]any{"credential_source": remote.CredentialSourceDevice, "device_id": id, "key_store": *store}
	// R85-M1: one signed read with the new key before the profile uses it.
	signed := config
	signed.CredentialSource, signed.DeviceID, signed.KeyStore = remote.CredentialSourceDevice, id, *store
	check, err := newAccountExecutor(signed)
	if err == nil {
		_, err = check.Execute(ctx, bus.Request{Operation: "device_list", Workspace: config.Workspace, Args: map[string]any{}})
	}
	if err != nil {
		return fmt.Errorf("device %s was enrolled, but a request signed with it failed, so the profile still signs with Google (revoke the device with clawbridge device revoke %s): %w", id, id, err)
	}
	if err = writeDeviceRoute(config.ClientConfig, route); err != nil {
		return fmt.Errorf("device %s was enrolled but the profile could not be updated; set %s in it by hand: %w", id, routeValues(route), err)
	}
	out := map[string]any{"enrolled": true, "device_id": id, "label": name, "key_store": *store, "principal": config.Principal,
		"note": "This profile now signs with the device key. The Google credential stays until clawbridge logout."}
	if *store == devicesign.StoreFile {
		out["warning"] = devicesign.FileWarning
	}
	return json.NewEncoder(stdout).Encode(map[string]any{"source": "client", "data": out})
}

// enrollServer checks, sending nothing signed, that the server takes device
// keys at its public origin; a test replaces it.
var enrollServer = remote.JoinServer

// newKeyStore is where a new key is kept: the command's --key-store, or
// the global one Run takes it as, or keyring.
func newKeyStore(local, global string) (string, error) {
	store := local
	if store == "" {
		store = global
	}
	if store == "" {
		store = devicesign.StoreKeyring
	}
	if store != devicesign.StoreKeyring && store != devicesign.StoreFile {
		return "", errors.New("--key-store is keyring or file")
	}
	return store, nil
}

// routeValues lists a device route's profile values, for a person to set
// by hand when the profile can't be written. None is a secret.
func routeValues(values map[string]any) string {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s = %q", k, fmt.Sprint(values[k])))
	}
	return strings.Join(parts, ", ")
}

// deviceLabel is a label as given, or this machine's name.
func deviceLabel(label string) (string, error) {
	label = strings.TrimSpace(label)
	if label == "" {
		host, err := os.Hostname()
		if err != nil || strings.TrimSpace(host) == "" {
			return "", errors.New("give the device a --label")
		}
		label = strings.TrimSpace(host)
	}
	if !utf8.ValidString(label) || utf8.RuneCountInString(label) > 64 || strings.IndexFunc(label, func(r rune) bool { return !unicode.IsPrint(r) }) >= 0 {
		return "", errors.New("a device label is 1-64 printable characters on one line")
	}
	return label, nil
}

// JoinParams is an in-process join: what `clawbridge join` takes.
type JoinParams struct {
	// Server is the server's URL, its public origin.
	Server string
	// Email and Code are the invite's.
	Email, Code string
	// Label names the device; empty is this machine's name.
	Label string
	// KeyStore is devicesign.StoreKeyring (the default) or StoreFile.
	KeyStore string
	// ConfigPath is the profile to write; empty is the selected one
	// (CLAWBRIDGE_CONFIG, or config.toml in the state directory).
	ConfigPath string
	// HTTPClient carries the requests; nil is a default client.
	HTTPClient *http.Client
}

// JoinResult is a completed join.
type JoinResult struct {
	DeviceID   string            `json:"device_id"`
	Principal  string            `json:"principal"`
	Workspace  string            `json:"workspace"`
	Grants     map[string]string `json:"grants"`
	Label      string            `json:"label"`
	KeyStore   string            `json:"key_store"`
	ConfigPath string            `json:"config"`
	Warning    string            `json:"warning,omitempty"`
}

// JoinUncertainError is a join whose answer was lost: the device may or
// may not have been added, and its key was kept so that a caller can find
// out by signing with it (whoami). It unwraps to remote.ErrJoinUncertain.
type JoinUncertainError struct {
	DeviceID, KeyStore, KeyHome, Server, Principal string
}

func (e *JoinUncertainError) Error() string {
	return fmt.Sprintf("%v; the key for %s was kept in its store. If the invite now shows used, ask an owner to revoke %s and issue another", remote.ErrJoinUncertain, e.DeviceID, e.DeviceID)
}

func (e *JoinUncertainError) Unwrap() error { return remote.ErrJoinUncertain }

// JoinDevice joins a hosted workspace with an invite (§4): it makes this
// device's key, stores it and reads it back, binds it with the invite, and
// writes the profile to sign with it. A refused join removes the key; a
// lost answer keeps it and returns a *JoinUncertainError.
func JoinDevice(ctx context.Context, p JoinParams) (JoinResult, error) {
	email := strings.ToLower(strings.TrimSpace(p.Email))
	if !strings.Contains(email, "@") {
		return JoinResult{}, errors.New("join needs the --email the invite was issued for")
	}
	if _, ok := devicekey.NormalCode(p.Code); !ok {
		return JoinResult{}, errors.New("an invite code is CBJ- and 16 letters and digits, as the inviter gave it")
	}
	if p.KeyStore == "" {
		p.KeyStore = devicesign.StoreKeyring
	}
	if p.KeyStore != devicesign.StoreKeyring && p.KeyStore != devicesign.StoreFile {
		return JoinResult{}, errors.New("--key-store is keyring or file")
	}
	label, err := deviceLabel(p.Label)
	if err != nil {
		return JoinResult{}, err
	}
	home, err := stateHome()
	if err != nil {
		return JoinResult{}, err
	}
	path, err := joinConfigPath(p.ConfigPath)
	if err != nil {
		return JoinResult{}, err
	}
	origin, err := remote.JoinServer(ctx, p.Server, p.HTTPClient)
	if err != nil {
		return JoinResult{}, err
	}
	existing, err := joinableProfile(path, origin)
	if err != nil {
		return JoinResult{}, err
	}
	key, id, err := newDeviceKey()
	if err != nil {
		return JoinResult{}, err
	}
	u, _ := url.Parse(origin)
	where := devicesign.Location{Store: p.KeyStore, Host: u.Host, Home: home, DeviceID: id}
	if err = devicesign.Save(where, key); err != nil {
		return JoinResult{}, err
	}
	joined, err := remote.Join(ctx, origin, key, remote.JoinRequest{Email: email, Code: p.Code, Label: label, KeyStore: p.KeyStore}, p.HTTPClient)
	if err != nil {
		if errors.Is(err, remote.ErrJoinUncertain) {
			return JoinResult{}, &JoinUncertainError{DeviceID: id, KeyStore: p.KeyStore, KeyHome: home, Server: origin, Principal: email}
		}
		_ = devicesign.Delete(where)
		return JoinResult{}, err
	}
	values := map[string]any{
		"mode": "cloud", "server": origin, "workspace": joined.Workspace.Name, "principal": joined.Principal,
		"credential_source": remote.CredentialSourceDevice, "device_id": id, "key_store": p.KeyStore,
	}
	if _, ok := existing["cache"]; !ok {
		values["cache"] = filepath.Join(filepath.Dir(path), strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))+".cache.db")
	}
	if !joined.ClockOffsetAt.IsZero() {
		values["clock_offset_seconds"] = int64(joined.ClockOffset / time.Second)
		values["clock_offset_at"] = joined.ClockOffsetAt.UTC().Format(time.RFC3339)
	}
	if err = writeDeviceRoute(path, values); err != nil {
		return JoinResult{}, fmt.Errorf("joined as device %s, but the profile %s could not be written; set %s in it by hand: %w", id, path, routeValues(values), err)
	}
	out := JoinResult{DeviceID: id, Principal: joined.Principal, Workspace: joined.Workspace.Name, Grants: joined.Grants, Label: label, KeyStore: p.KeyStore, ConfigPath: path}
	if p.KeyStore == devicesign.StoreFile {
		out.Warning = devicesign.FileWarning
	}
	return out, nil
}

// joinConfigPath is the profile a join writes: path, else the selected one
// (CLAWBRIDGE_CONFIG, or config.toml in the state directory).
func joinConfigPath(path string) (string, error) {
	if path == "" {
		path = firstConfigPath("", os.Getenv("CLAWBRIDGE_CONFIG"))
	}
	if path == "" {
		home, err := stateHome()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, "config.toml")
	}
	return filepath.Abs(path)
}

// joinableProfile reads the profile a join will write and refuses one that
// routes elsewhere: a local workspace, another server, or a device already
// joined (a device belongs to one workspace, so another workspace gets its
// own profile). A Google profile of the same server may switch to the key.
func joinableProfile(path, origin string) (map[string]any, error) {
	values := map[string]any{}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return values, nil
	}
	if err != nil {
		return nil, err
	}
	if err = toml.Unmarshal(b, &values); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	other := "; join into another profile with --client-config /absolute/path.toml"
	mode, _ := values["mode"].(string)
	server, _ := values["server"].(string)
	switch {
	case mode != "cloud" && len(values) > 0:
		return nil, errors.New("this profile is a local workspace" + other)
	case mode == "cloud" && strings.TrimRight(strings.ToLower(server), "/") != origin:
		return nil, errors.New("this profile is for another server (" + server + ")" + other)
	case values["credential_source"] == remote.CredentialSourceDevice:
		return nil, fmt.Errorf("this profile already signs as device %v%s", values["device_id"], other)
	}
	return values, nil
}

// writeDeviceRoute sets a profile's device route, keeping every other key,
// in a 0600 file in a 0700 directory. A stale clock correction goes with
// the old route.
func writeDeviceRoute(path string, set map[string]any) error {
	if path == "" {
		return errors.New("no profile is selected")
	}
	values := map[string]any{}
	b, err := os.ReadFile(path)
	if err == nil {
		if err = toml.Unmarshal(b, &values); err != nil {
			return fmt.Errorf("invalid config: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	delete(values, "clock_offset_seconds")
	delete(values, "clock_offset_at")
	for k, v := range set {
		values[k] = v
	}
	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return writeConfigValues(path, values)
}

// runDeviceJoin is `clawbridge join`.
func runDeviceJoin(ctx context.Context, flags Config, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	f := flag.NewFlagSet("join", flag.ContinueOnError)
	f.SetOutput(stderr)
	email := f.String("email", "", "the email the invite was issued for")
	code := f.String("code", "", "the invite code; leave it out to be asked for it unechoed (or pipe it on stdin): a command line is kept in shell history and visible to other users")
	label := f.String("label", "", "this device's label (default: this machine's name)")
	store := f.String("key-store", "", "where the key is kept: keyring (the default), or file where there is no OS key store")
	if _, err := parseAccountFlags(f, args, 0, 0); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.Server == "" {
		return errors.New("join needs --server URL, the server the invite is for")
	}
	// R85-H1: Run reads --key-store as the global flag.
	var err error
	if *store, err = newKeyStore(*store, flags.KeyStore); err != nil {
		return err
	}
	if *code == "" {
		fmt.Fprint(stderr, "Invite code: ")
		if stdin == os.Stdin && stdinTerminal() {
			// R85-L4: the code is a secret; a terminal doesn't echo it.
			raw, e := readHidden()
			fmt.Fprintln(stderr)
			if e != nil {
				return errors.New("join needs the invite code: type it at the prompt, or pipe it on stdin")
			}
			*code = strings.TrimSpace(string(raw))
		} else {
			line, e := bufio.NewReader(stdin).ReadString('\n')
			if e != nil && line == "" {
				return errors.New("join needs the invite code: type it at the prompt, or pipe it on stdin")
			}
			*code = strings.TrimSpace(line)
		}
	}
	if *store == devicesign.StoreFile {
		fmt.Fprintln(stderr, "warning: "+devicesign.FileWarning+".")
	}
	joined, err := JoinDevice(ctx, JoinParams{Server: flags.Server, Email: *email, Code: *code, Label: *label, KeyStore: *store, ConfigPath: flags.ClientConfig})
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(map[string]any{"source": "client", "data": joined})
}
