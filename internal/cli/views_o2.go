package cli

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"clawbridge/internal/clirender"
	"clawbridge/internal/doctor"
)

// The owner's first group of views (live/CLI-OUTPUT-PLAN.md §5, O2): who
// you are, doctor, a message's status, and the account lists. IDs are shown
// only where the next command needs them; times are relative, with UTC
// where a detail is the point (owner, Q6).

// viewNow is the views' clock, a seam for the tests.
var viewNow = time.Now

func init() {
	clirender.Register("whoami", whoamiView)
	clirender.Register("message_status", messageStatusView)
	clirender.Register("doctor", doctorView)
	clirender.Register("invite_list", inviteListView)
	clirender.Register("device_list", deviceListView)
	clirender.Register("user_list", userListView)
	clirender.Register("owner_transfer_get", ownerView)
}

// asMap reads any result as JSON objects, the shape --json shows.
func asMap(v any) (map[string]any, bool) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, false
	}
	var out map[string]any
	if json.Unmarshal(raw, &out) != nil || out == nil {
		return nil, false
	}
	return out, true
}

func field(m map[string]any, k string) string {
	switch x := m[k].(type) {
	case string:
		return x
	case nil:
		return ""
	case bool:
		if x {
			return "yes"
		}
		return "no"
	case float64:
		return fmt.Sprint(x)
	}
	return ""
}

func object(m map[string]any, k string) map[string]any {
	o, _ := m[k].(map[string]any)
	return o
}

func list(m map[string]any, k string) []map[string]any {
	items, _ := m[k].([]any)
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if o, ok := item.(map[string]any); ok {
			out = append(out, o)
		}
	}
	return out
}

func orDash(s string) string { return firstNonEmpty(s, "-") }

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func ago(stored string) string {
	if stored == "" {
		return "-"
	}
	return clirender.When(viewNow(), stored)
}

func exact(stored string) string {
	if stored == "" {
		return "-"
	}
	return clirender.WhenExact(viewNow(), stored)
}

func whoamiView(v any) (clirender.Doc, bool) {
	r, ok := resultData(v)
	if !ok || r["principal"] == nil {
		return clirender.Doc{}, false
	}
	var d clirender.Doc
	d.Title("Who you are")
	who := field(r, "principal")
	proof := field(r, "identity_provenance")
	if words, ok := provenanceWords[proof]; ok {
		proof = words
	}
	switch {
	case r["verified"] == true:
		who += " (verified by " + proof + ")"
	case proof != "":
		who += " (" + proof + ", not verified)"
	}
	d.KV("Signed in as", who)
	ws := object(r, "workspace")
	d.KV("Workspace", orDash(strings.TrimSpace(field(ws, "name")+" "+parens(field(ws, "mode")))))
	if p := object(r, "project"); p != nil {
		name := field(p, "slug")
		if n := field(p, "name"); n != "" && n != name {
			name += " (" + n + ")"
		}
		d.KV("Project", name)
	} else {
		d.KV("Project", "none")
	}
	if s := object(r, "session"); s != nil {
		line := field(s, "name") + " · " + firstNonEmpty(field(s, "presence"), field(s, "status"))
		if role := field(s, "role"); role != "" {
			line += " · " + role
		}
		if seen := field(s, "last_seen"); seen != "" {
			line += " · last seen " + ago(seen)
		}
		d.KV("Session", line)
	} else {
		d.KV("Session", "none on this profile")
	}
	online := "offline"
	if r["online"] == true {
		online = "online"
	}
	d.KV("Connection", orDash(field(r, "storage_mode"))+" · "+online)
	if pb := object(r, "coordinator_playbook"); pb != nil {
		d.KV("Coordinator", playbookLine(pb))
	}
	return d, true
}

// playbookLine says where the coordinator's playbook is, for the
// coordinator's whoami.
func playbookLine(pb map[string]any) string {
	switch {
	case pb["merged"] == true:
		return "you are; your project's playbook is " + field(pb, "path") + " on " + field(pb, "ref")
	case pb["local"] == true && field(pb, "ref") != "":
		return "you are; " + field(pb, "path") + " is here but not on " + field(pb, "ref") + ", so it doesn't count until merged"
	case field(pb, "note") != "":
		return "you are; " + field(pb, "note")
	default:
		return "you are; your project has no " + field(pb, "path") + " yet"
	}
}

// provenanceWords names how the caller was proven; any other value is
// shown as the server sent it.
var provenanceWords = map[string]string{
	"google-id-token":          "Google sign-in (before v1.0)",
	"device-key":               "this device's key",
	"cached-sign-in":           "the last sign-in, offline",
	"local-os-user-unverified": "the local OS user",
}

func parens(s string) string {
	if s == "" {
		return ""
	}
	return "(" + s + ")"
}

func messageStatusView(v any) (clirender.Doc, bool) {
	r, ok := resultData(v)
	if !ok || field(r, "id") == "" {
		return clirender.Doc{}, false
	}
	var d clirender.Doc
	d.Title("Message")
	d.KV("ID", field(r, "id"))
	d.KV("Sent", exact(field(r, "created_at")))
	if to := field(r, "reply_to"); to != "" {
		d.KV("Answers", to)
	}
	if th := field(r, "thread_id"); th != "" && th != field(r, "id") {
		d.KV("Thread", th)
	}
	d.KV("Expires", ago(field(r, "expires_at")))
	rows := [][]string{}
	for _, del := range list(r, "deliveries") {
		read := "not yet"
		if at := field(del, "read_at"); at != "" {
			read = ago(at)
		}
		delivered := "not yet"
		if at := field(del, "delivered_at"); at != "" {
			delivered = ago(at)
		}
		rows = append(rows, []string{field(del, "recipient"), delivered, read})
	}
	d.Text("")
	d.Table([]clirender.Col{{Name: "TO"}, {Name: "DELIVERED"}, {Name: "READ"}}, rows, "No deliveries yet.")
	return d, true
}

// doctorView is shape D: a line per check with its word, the message, the
// action indented under it, and the summary first.
func doctorView(v any) (clirender.Doc, bool) {
	report, ok := v.(doctor.Report)
	if !ok {
		return clirender.Doc{}, false
	}
	counts := map[string]int{}
	for _, c := range report.Checks {
		counts[c.Status]++
	}
	var d clirender.Doc
	title := fmt.Sprintf("Doctor: %d pass · %d warn · %d fail", counts[doctor.Pass], counts[doctor.Warn], counts[doctor.Fail])
	if counts[doctor.Info] > 0 {
		title += fmt.Sprintf(" · %d info", counts[doctor.Info])
	}
	d.Title(title)
	// Failures first, then warnings, notes and passes; each group in report order.
	order := map[string]int{doctor.Fail: 0, doctor.Warn: 1, doctor.Info: 2, doctor.Pass: 3}
	checks := append([]doctor.Check(nil), report.Checks...)
	sort.SliceStable(checks, func(i, j int) bool { return order[checks[i].Status] < order[checks[j].Status] })
	for _, c := range checks {
		level := clirender.Info
		switch c.Status {
		case doctor.Pass:
			level = clirender.Pass
		case doctor.Warn:
			level = clirender.Warn
		case doctor.Fail:
			level = clirender.Fail
		}
		line, detail := doctorLine(c)
		d.Status(level, line)
		if detail != "" {
			d.Detail(detail)
		}
		if c.Path != "" && c.Status != doctor.Pass {
			d.Detail(c.Path)
		}
		if c.Action != "" && c.Status != doctor.Pass {
			d.Detail("→ " + c.Action)
		}
	}
	if report.Status == doctor.Fail {
		d.Text("")
		d.Note("Doctor found failures that need fixing; the actions above say how.")
	}
	return d, true
}

func inviteListView(v any) (clirender.Doc, bool) {
	r, ok := resultData(v)
	if !ok || r["invites"] == nil {
		return clirender.Doc{}, false
	}
	rows := [][]string{}
	for _, i := range list(r, "invites") {
		grants := []string{}
		for project, role := range object(i, "grants") {
			grants = append(grants, fmt.Sprintf("%s=%v", project, role))
		}
		sort.Strings(grants)
		expires := ago(field(i, "expires_at"))
		if field(i, "status") != "pending" {
			expires = "-"
		}
		rows = append(rows, []string{field(i, "invite_id"), field(i, "email"), field(i, "purpose"), field(i, "status"), expires, orDash(strings.Join(grants, ", "))})
	}
	var d clirender.Doc
	d.Title("Invites")
	d.Records([]clirender.Col{{Name: "ID", Keep: true}, {Name: "EMAIL"}, {Name: "FOR"}, {Name: "STATUS"}, {Name: "EXPIRES"}, {Name: "GRANTS"}}, rows,
		"No invites. An owner invites someone with clawbridge invite create --email ADDRESS.")
	return d, true
}

// deviceStatus is a device's status, with dormancy in words.
func deviceStatus(dev map[string]any) string {
	s := field(dev, "status")
	if s == "active" {
		if at := field(dev, "dormant_at"); at != "" && dev["dormant_soon"] == true {
			return "dormant " + ago(at)
		}
	}
	return orDash(s)
}

func deviceListView(v any) (clirender.Doc, bool) {
	r, ok := resultData(v)
	if !ok || r["devices"] == nil {
		return clirender.Doc{}, false
	}
	rows := [][]string{}
	for _, dev := range list(r, "devices") {
		used := "never"
		if at := field(dev, "last_used_at"); at != "" {
			used = ago(at)
		}
		rows = append(rows, []string{field(dev, "device_id"), orDash(field(dev, "label")), orDash(field(dev, "kind")), deviceStatus(dev), used, ago(field(dev, "created_at"))})
	}
	var d clirender.Doc
	d.Title("Devices")
	d.Records([]clirender.Col{{Name: "ID", Keep: true}, {Name: "LABEL"}, {Name: "KIND"}, {Name: "STATUS"}, {Name: "LAST USED"}, {Name: "ADDED"}}, rows, "No devices.")
	return d, true
}

func userListView(v any) (clirender.Doc, bool) {
	r, ok := resultData(v)
	if !ok || r["users"] == nil {
		return clirender.Doc{}, false
	}
	rows := [][]string{}
	for _, u := range list(r, "users") {
		who := field(u, "principal")
		if u["owner"] == true {
			who += " (owner)"
		}
		roles := []string{}
		for _, role := range list(u, "roles") {
			roles = append(roles, field(role, "slug")+"="+field(role, "role"))
		}
		devices := list(u, "devices")
		active := 0
		for _, dev := range devices {
			if field(dev, "status") == "active" {
				active++
			}
		}
		rows = append(rows, []string{who, orDash(strings.Join(roles, ", ")), fmt.Sprintf("%d active of %d", active, len(devices)), orDash(field(u, "invited_by")), ago(field(u, "added_at"))})
	}
	var d clirender.Doc
	d.Title("Members")
	d.Records([]clirender.Col{{Name: "MEMBER", Keep: true}, {Name: "ROLES"}, {Name: "DEVICES"}, {Name: "INVITED BY"}, {Name: "ADDED"}}, rows, "No members.")
	return d, true
}

func ownerView(v any) (clirender.Doc, bool) {
	r, ok := resultData(v)
	if !ok {
		return clirender.Doc{}, false
	}
	if _, has := r["pending"]; !has {
		return clirender.Doc{}, false
	}
	var d clirender.Doc
	d.Title("Workspace ownership")
	p := object(r, "pending")
	if p == nil {
		d.Text("No ownership offer is pending.")
		return d, true
	}
	d.KV("Offered by", field(p, "from"))
	d.KV("Offered to", field(p, "to"))
	d.KV("Offered", exact(field(p, "created_at")))
	d.KV("Expires", exact(field(p, "expires_at")))
	d.Text("")
	d.Note("The member accepts or declines with clawbridge owner accept or owner decline, on their own device. The owner withdraws it with clawbridge owner cancel.")
	return d, true
}
