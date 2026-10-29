package linearboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/docpublish"
	"clawbridge/internal/linearprovider"
)

// The generated "Team roles" document (live/TEAM-ROLE-MAPPING-DESIGN.md
// §4, §13): ClawBridge renders the role mapping as one Linear document for
// people to read. The mapping is its only source, so a publish overwrites
// an edit made in Linear (§4's accepted exception, T4), but only of the
// document its own ledger row names. It never adopts a document by title,
// and no code reads the document's content except to compare digests.
// Until a reviewed probe shows the app can write documents, it goes
// through the owner's own Linear profile only.

// TeamRolesDocTitle is the document's title.
const TeamRolesDocTitle = "Team roles (ClawBridge)"

const teamRolesDocSource = "generated:team_roles"

// RolesPublishInput is one publish. Without Confirm it previews. The
// parent is needed the first time; after that the ledger row names it.
type RolesPublishInput struct {
	ParentKind, ParentID string
	Recreate, Confirm    bool
}

// RolesPublishResult is what a publish found and, with Confirm, did.
// Action is create, settle, update, none, gone, unreadable or
// pending_recent; Outcome is created, settled, updated, drifted, uncertain
// or failed. Refused, when set, says why nothing was written.
type RolesPublishResult struct {
	ParentKind string   `json:"parent_kind"`
	ParentID   string   `json:"parent_id"`
	Revision   int      `json:"revision"`
	Title      string   `json:"title"`
	Bytes      int      `json:"bytes"`
	Action     string   `json:"action"`
	Outcome    string   `json:"outcome,omitempty"`
	DocumentID string   `json:"document_id,omitempty"`
	Note       string   `json:"note,omitempty"`
	Archive    string   `json:"archive_by_hand,omitempty"`
	Hits       []string `json:"deny_hits,omitempty"`
	Refused    string   `json:"refused,omitempty"`
	Confirmed  bool     `json:"confirmed"`
}

// rolesDocRow is the generated document's ledger row.
type rolesDocRow struct {
	ParentKind string `json:"parent_kind"`
	ParentID   string `json:"parent_id"`
	State      string `json:"state"`
	Token      string `json:"publish_token"`
	Document   string `json:"document_id"`
	Digest     string `json:"published_sha256"`
	Source     string `json:"source_sha256"`
	Revision   int    `json:"revision"`
	Replaced   string `json:"replaced_document_id"`
	Age        int    `json:"age_seconds"`
}

// TeamRolesDocState is the mapping's revision and the row, if any.
type TeamRolesDocState struct {
	Revision  int          `json:"revision"`
	Published *rolesDocRow `json:"published"`
}

// TeamRolesDoc reads the generated document's row (owner only).
func (s *Service) TeamRolesDoc(ctx context.Context, scope bus.Request) (TeamRolesDocState, error) {
	scope.Operation = "team_roles_doc_get"
	scope.Args = instanceArgs(scope.Args)
	r, e := s.cfg.Executor.Execute(ctx, scope)
	if e != nil {
		return TeamRolesDocState{}, e
	}
	var out TeamRolesDocState
	raw, _ := json.Marshal(r.Data)
	if json.Unmarshal(raw, &out) != nil {
		return out, errors.New("the team roles document's record is unreadable")
	}
	return out, nil
}

// PublishTeamRoles previews, or with Confirm carries out, a publish of
// the role mapping.
func (s *Service) PublishTeamRoles(ctx context.Context, scope bus.Request, in RolesPublishInput) (RolesPublishResult, error) {
	out := RolesPublishResult{Title: TeamRolesDocTitle}
	b, e := s.BindingV2(ctx, scope)
	if e != nil {
		return out, e
	}
	if !b.Configured {
		return out, errors.New("task-board is not linked")
	}
	access, e := s.writeAccessV2(ctx, scope, b)
	if e != nil {
		return out, e
	}
	if access.ProjectRole != "owner" {
		return out, errors.New("publishing the team roles document is for the project owner")
	}
	mapping, e := s.teamRoles(ctx, scope)
	if e != nil {
		return out, e
	}
	if mapping.Revision == 0 {
		return out, errors.New("no team roles are set yet; set them with clawbridge linear roles first")
	}
	state, e := s.TeamRolesDoc(ctx, scope)
	if e != nil {
		return out, e
	}
	row := state.Published
	out.Revision = mapping.Revision
	out.ParentKind, out.ParentID = in.ParentKind, in.ParentID
	if out.ParentID == "" && row != nil {
		out.ParentKind, out.ParentID = row.ParentKind, row.ParentID
	}
	if out.ParentKind != "team" && out.ParentKind != "project" || !linearprovider.ValidID(out.ParentID) {
		return out, errors.New("name where the document goes the first time: --team or --linear-project, by UUID")
	}
	moving := row != nil && (row.ParentKind != out.ParentKind || row.ParentID != out.ParentID)
	if moving && !in.Recreate {
		return out, fmt.Errorf("the team roles document is under %s %s; to put it under another, pass --recreate (the old one is left for you to archive)", row.ParentKind, row.ParentID)
	}
	// Names are read now, through the owner's own connection, and never
	// stored: display names only (L4), never an email or an account.
	users, e := s.WorkspaceUsers(ctx, scope)
	if e != nil {
		return out, e
	}
	policy, e := s.publishPolicy(ctx, scope)
	if e != nil {
		return out, e
	}
	d, done, e := s.openDocScopeFor(ctx, scope, in.Confirm)
	if e != nil {
		return out, e
	}
	defer done()
	docs, ok := d.p.(docWriterV2)
	if !ok {
		return out, errors.New("native provider lacks the document tools")
	}
	if reason := docs.DocumentReason(); reason != "" {
		return out, errors.New("Linear document writes are unavailable: " + reason)
	}
	if out.ParentKind == "team" {
		e = d.proveTeam(out.ParentID)
	} else {
		e = d.proveProject(ctx, out.ParentID)
	}
	if e != nil {
		return out, e
	}
	teamNames := map[string]string{}
	for _, t := range d.v.Teams {
		teamNames[strings.ToLower(t.ID)] = t.Name
	}
	body := renderTeamRoles(mapping, b.LinearTeamIDs, teamNames, users)
	footer := teamRolesFooter(mapping.Revision, "000000000000")
	out.Bytes = len(body) + len(footer)
	if out.Bytes > docpublish.MaxContent {
		out.Refused = fmt.Sprintf("the document would be %d bytes, over the %d-byte limit; nothing is published", out.Bytes, docpublish.MaxContent)
		return out, nil
	}
	title := TeamRolesDocTitle
	if e = mentionGuard(&title, &body); e != nil {
		return out, e
	}
	// The project's deny patterns, with no override: the names come from
	// Linear, not from a file a person reviewed.
	patterns := make([]docpublish.Pattern, len(policy.DenyPatterns))
	for i, p := range policy.DenyPatterns {
		patterns[i] = docpublish.Pattern{ID: p.ID, Regex: p.Regex}
	}
	hits, e := docpublish.Scan(body, patterns)
	if e != nil {
		return out, e
	}
	if len(hits) > 0 {
		seen := map[string]bool{}
		for _, h := range hits {
			if !seen[h.Pattern] {
				seen[h.Pattern] = true
				out.Hits = append(out.Hits, h.Pattern)
			}
		}
		sort.Strings(out.Hits)
		out.Refused = fmt.Sprintf("the document matches deny patterns %v; nothing is published", out.Hits)
		return out, nil
	}
	source := contentDigest(body)
	found := s.planTeamRolesDoc(ctx, docs, row, in.Recreate || moving, source, &out)
	if out.Refused != "" || !in.Confirm {
		return out, nil
	}
	if e = s.unchangedV2(ctx, scope, d.b); e != nil {
		return out, e
	}
	s.publishTeamRolesDoc(ctx, scope, docs, row, found, title, body, source, &out)
	out.Confirmed = true
	return out, nil
}

// planTeamRolesDoc decides the action (§13). Unlike a git publish, an
// edit in Linear doesn't block: the mapping is the only source.
func (s *Service) planTeamRolesDoc(ctx context.Context, docs docWriterV2, row *rolesDocRow, recreate bool, source string, out *RolesPublishResult) linearprovider.Document {
	var found linearprovider.Document
	switch {
	case row == nil:
		out.Action = "create"
	case recreate:
		out.Action, out.Note = "create", "a new document; the earlier one is left as it is"
		if row.Document != "" {
			out.Archive = row.Document
			out.Note += ": archive document " + row.Document + " in Linear by hand"
		}
	case row.State == "pending":
		list, complete, e := docs.DocumentsUnder(ctx, row.ParentKind, row.ParentID)
		if e != nil || !complete {
			out.Action, out.Note = "unreadable", "could not list the parent's documents"
			break
		}
		// Found by its own token only, never by title (T4).
		for _, d := range list {
			if docpublish.HasToken(d.Content, row.Token) {
				found = d
			}
		}
		switch {
		case found.ID != "":
			out.Action, out.DocumentID = "settle", found.ID
		case row.Age < 0:
			out.Action, out.Note = "pending_recent", "could not tell when the earlier create was reserved; publish again later"
		case time.Duration(row.Age)*time.Second < publishPendingFor:
			out.Action, out.Note = "pending_recent", "an earlier create may still appear; publish again in a few minutes"
		default:
			out.Action, out.Note = "create", "an earlier create did not appear; it is created again"
		}
	default:
		out.DocumentID = row.Document
		doc, e := docs.GetDocument(ctx, row.Document)
		switch {
		case e != nil:
			out.Action, out.Note = "unreadable", "could not read the team roles document; only if you know it was deleted, publish with --recreate --confirm"
		case doc.Archived:
			out.Action, out.Note = "gone", "the team roles document is archived; publish with --recreate --confirm to create it again"
		case contentDigest(doc.Content) != row.Digest:
			out.Action, out.Note = "update", "edited in Linear since it was published; this publish overwrites the edit (change roles with clawbridge linear roles)"
		case row.Source == source:
			out.Action = "none"
		default:
			out.Action = "update"
		}
	}
	switch out.Action {
	case "gone", "unreadable", "pending_recent":
		out.Refused = "the team roles document cannot be published (" + out.Action + "); see the note. Nothing is written"
	}
	return found
}

// publishTeamRolesDoc carries out the action: reserve before a create,
// read back after every write, record the digest Linear returned.
func (s *Service) publishTeamRolesDoc(ctx context.Context, scope bus.Request, docs docWriterV2, row *rolesDocRow, found linearprovider.Document, title, body, source string, out *RolesPublishResult) {
	expect, replaced := "", ""
	if row != nil {
		expect = row.Token
		if out.Archive != "" {
			replaced = out.Archive
		}
	}
	put := func(state, token, expect, document, digest string) error {
		return s.teamRolesDocPut(ctx, scope, out, state, token, expect, document, digest, source, replaced)
	}
	switch out.Action {
	case "settle":
		if e := put("published", row.Token, expect, found.ID, contentDigest(found.Content)); e != nil {
			out.Outcome, out.Note = "failed", "found the document but could not record it: "+e.Error()
			return
		}
		out.Outcome = "settled"
	case "create":
		token, e := docpublish.NewToken()
		if e != nil {
			out.Outcome, out.Note = "failed", e.Error()
			return
		}
		if e = put("pending", token, expect, "", ""); e != nil {
			out.Outcome, out.Note = "failed", "nothing was sent: "+e.Error()
			return
		}
		created, e := docs.CreateDocument(ctx, out.ParentKind, out.ParentID, title, body+teamRolesFooter(out.Revision, token))
		if e != nil {
			out.Outcome, out.Note = "uncertain", "the create may or may not have happened; publish again to find or redo it"
			return
		}
		out.DocumentID = created.ID
		s.teamRolesReadBack(ctx, docs, token, created.ID, put, out, "created")
	case "update":
		// Read again right before the write: the document must still be
		// the one the row names, readable and not archived. An edit is
		// overwritten (§4); one landing after this read is too.
		current, e := docs.GetDocument(ctx, row.Document)
		if e != nil || current.Archived {
			out.Outcome, out.Note = "drifted", "the document could not be read or was archived just now; nothing was written to it"
			return
		}
		if _, e = docs.UpdateDocumentContent(ctx, row.Document, body+teamRolesFooter(out.Revision, row.Token)); e != nil {
			out.Outcome, out.Note = "uncertain", "the update may or may not have happened; publish again"
			return
		}
		s.teamRolesReadBack(ctx, docs, row.Token, row.Document, put, out, "updated")
	}
}

func (s *Service) teamRolesReadBack(ctx context.Context, docs docWriterV2, token, id string, put func(state, token, expect, document, digest string) error, out *RolesPublishResult, outcome string) {
	d, e := docs.GetDocument(ctx, id)
	if e != nil || !docpublish.HasToken(d.Content, token) {
		out.Outcome, out.Note = "uncertain", "written, but the read-back failed; publish again to record it"
		return
	}
	if e = put("published", token, token, id, contentDigest(d.Content)); e != nil {
		out.Outcome, out.Note = "uncertain", "written, but not recorded: "+e.Error()+"; publish again"
		return
	}
	out.Outcome = outcome
}

func (s *Service) teamRolesDocPut(ctx context.Context, scope bus.Request, out *RolesPublishResult, state, token, expect, document, digest, source, replaced string) error {
	scope.Operation = "team_roles_doc_put"
	scope.Args = instanceArgs(scope.Args)
	for k, v := range map[string]any{"parent_kind": out.ParentKind, "parent_id": out.ParentID, "source_path": teamRolesDocSource, "state": state, "publish_token": token, "expect_token": expect, "mapping_revision": out.Revision, "source_sha256": source} {
		scope.Args[k] = v
	}
	if document != "" {
		scope.Args["document_id"], scope.Args["published_sha256"] = document, digest
	}
	if replaced != "" {
		scope.Args["replaced_document_id"] = replaced
	}
	_, e := s.cfg.Executor.Execute(ctx, scope)
	return e
}

// teamRolesMapping is team_roles_get's answer, as the publish reads it.
type teamRolesMapping struct {
	Revision int `json:"revision"`
	Teams    []struct {
		TeamID    string   `json:"team_id"`
		Leads     []string `json:"leads"`
		TeamBound bool     `json:"team_bound"`
	} `json:"teams"`
	People []bus.TeamRolePerson `json:"people"`
}

func (s *Service) teamRoles(ctx context.Context, scope bus.Request) (teamRolesMapping, error) {
	scope.Operation = "team_roles_get"
	scope.Args = instanceArgs(scope.Args)
	r, e := s.cfg.Executor.Execute(ctx, scope)
	if e != nil {
		return teamRolesMapping{}, e
	}
	var m teamRolesMapping
	raw, _ := json.Marshal(r.Data)
	if json.Unmarshal(raw, &m) != nil {
		return m, errors.New("the server's answer had no team roles")
	}
	return m, nil
}

// renderTeamRoles is the document's body: each bound mapped team by name,
// its leads, engineers and reviewers by Linear display name. A person
// Linear no longer lists shows as a former user; nothing else names them.
func renderTeamRoles(m teamRolesMapping, bound []string, teamNames map[string]string, users []linearprovider.WorkspaceUser) string {
	names := map[string]string{}
	for _, u := range users {
		names[strings.ToLower(u.ID)] = oneLine(u.Name)
	}
	name := func(id string) string {
		if n := names[strings.ToLower(id)]; n != "" {
			return n
		}
		return "(a former user)"
	}
	var teams []string
	leads := map[string][]string{}
	for _, t := range m.Teams {
		leads[t.TeamID] = t.Leads
	}
	inMapping := map[string]bool{}
	for _, t := range m.Teams {
		inMapping[t.TeamID] = true
	}
	for _, p := range m.People {
		for _, t := range p.Teams {
			inMapping[t] = true
		}
	}
	for t := range inMapping {
		if contains(bound, t) {
			teams = append(teams, t)
		}
	}
	title := func(t string) string {
		if n := oneLine(teamNames[t]); n != "" {
			return n
		}
		return "Team " + t
	}
	// By title, then by ID: two teams may share a name, and the teams come
	// from a map, so without the ID the order (and the document's digest)
	// could change between two publishes of the same roles.
	sort.Slice(teams, func(i, j int) bool {
		if a, b := title(teams[i]), title(teams[j]); a != b {
			return a < b
		}
		return teams[i] < teams[j]
	})
	var b strings.Builder
	b.WriteString("Who leads each team and each person's role, as ClawBridge holds them.\n")
	if len(teams) == 0 {
		b.WriteString("\nNo bound team has roles yet.\n")
	}
	for _, t := range teams {
		b.WriteString("\n## " + title(t) + "\n")
		groups := []struct {
			heading string
			ids     []string
		}{{"Leads", leads[t]}}
		for _, role := range []string{"engineer", "reviewer"} {
			var ids []string
			for _, p := range m.People {
				if p.Role == role && contains(p.Teams, t) {
					ids = append(ids, p.LinearUserID)
				}
			}
			heading := "Engineers"
			if role == "reviewer" {
				heading = "Reviewers"
			}
			groups = append(groups, struct {
				heading string
				ids     []string
			}{heading, ids})
		}
		for _, g := range groups {
			b.WriteString("\n**" + g.heading + ":** ")
			if len(g.ids) == 0 {
				b.WriteString("none\n")
				continue
			}
			var shown []string
			for _, id := range g.ids {
				shown = append(shown, name(id))
			}
			sort.Strings(shown)
			b.WriteString(strings.Join(shown, ", ") + "\n")
		}
	}
	return b.String()
}

// teamRolesFooter ends the document: where it comes from, that edits are
// overwritten, and the token that finds it again.
func teamRolesFooter(revision int, token string) string {
	return fmt.Sprintf("\n\n---\n\nGenerated by ClawBridge from the project's role mapping, revision %d. Change it with `clawbridge linear roles`; edits here are overwritten at the next publish.\n\n%s\n", revision, docpublish.TokenLine(token))
}

// oneLine keeps a display name to one line of plain text (review L1): no
// markdown or HTML, and no bare URL that Linear would turn into a link.
func oneLine(s string) string {
	s = strings.NewReplacer("*", "", "_", "", "`", "", "#", "", "[", "", "]", "", "<", "", ">", "", "://", ":/ /").Replace(s)
	return strings.Join(strings.Fields(s), " ")
}

// RolesDocDrift is what doctor reports about the generated document: the
// mapping's revision against the one published, whether the document was
// edited in Linear, archived or unreadable, and a document a --recreate
// left behind for archiving by hand (review V2).
type RolesDocDrift struct {
	Published         bool   `json:"published"`
	Pending           bool   `json:"pending"`
	Revision          int    `json:"revision"`
	PublishedRevision int    `json:"published_revision"`
	Edited            bool   `json:"edited"`
	Archived          bool   `json:"archived"`
	Unreadable        bool   `json:"unreadable"`
	Replaced          string `json:"replaced_document_id,omitempty"`
}

// TeamRolesDocDrift reads the row and, when published, the document by its
// stored ID only, comparing digests. It never reads the content otherwise.
func (s *Service) TeamRolesDocDrift(ctx context.Context, scope bus.Request) (RolesDocDrift, error) {
	state, e := s.TeamRolesDoc(ctx, scope)
	if e != nil {
		return RolesDocDrift{}, e
	}
	out := RolesDocDrift{Revision: state.Revision}
	row := state.Published
	if row == nil {
		return out, nil
	}
	out.Published, out.Pending, out.PublishedRevision, out.Replaced = true, row.State == "pending", row.Revision, row.Replaced
	if out.Pending {
		return out, nil
	}
	_, p, done, e := s.open(ctx)
	if e != nil {
		out.Unreadable = true
		return out, nil
	}
	defer done()
	docs, ok := p.(interface {
		GetDocument(ctx context.Context, id string) (linearprovider.Document, error)
	})
	if !ok {
		out.Unreadable = true
		return out, nil
	}
	d, e := docs.GetDocument(ctx, row.Document)
	switch {
	case e != nil:
		out.Unreadable = true
	case d.Archived:
		out.Archived = true
	case contentDigest(d.Content) != row.Digest:
		out.Edited = true
	}
	return out, nil
}
