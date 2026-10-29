package linearboard

import (
	"context"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearconn"
	"clawbridge/internal/linearprovider"
)

// ProviderV2 is optional: legacy injected providers keep their original contract.
type ProviderV2 interface {
	IssueV2(context.Context, string) (linearprovider.IssueV2, error)
	IssuesV2(context.Context, linearprovider.IssueQueryV2) (linearprovider.Page[linearprovider.IssueV2], error)
	Labels(context.Context, string, linearprovider.PageOptions) (linearprovider.Page[linearprovider.Label], error)
	Members(context.Context, string, linearprovider.PageOptions) (linearprovider.Page[linearprovider.Member], error)
	Capabilities(context.Context) (linearprovider.Capabilities, error)
	CreateV2(context.Context, linearprovider.CreateInputV2) (linearprovider.IssueV2, error)
	UpdateV2(context.Context, linearprovider.UpdateInputV2) (linearprovider.IssueV2, error)
	Relation(context.Context, linearprovider.RelationInput) (linearprovider.RelationReceipt, error)
}
type ContextV2 struct {
	Binding              bus.TaskboardBindingV2 `json:"binding"`
	Profile              linearconn.Profile     `json:"profile"`
	WorkspaceIDAvailable bool                   `json:"linear_workspace_id_available"`
	// Capabilities is the provider's negotiated capability state, including
	// why a capability is off. It is omitted for a provider without V2
	// capability reporting.
	Capabilities *linearprovider.Capabilities `json:"capabilities,omitempty"`
	// Lane is the session's lane and the teams it writes, when the binding
	// has lanes and the call names a session (§3.1), so an agent knows
	// before it tries.
	Lane *LaneViewV2 `json:"lane,omitempty"`
}

// LaneViewV2 is a session's lane: its name, or read_only, and the write
// teams it writes, none when read-only.
type LaneViewV2 struct {
	Name         string   `json:"name"`
	WriteTeamIDs []string `json:"write_team_ids"`
}
type LinkInputV2 struct {
	TeamIDs            []string
	DefaultTeamID      string
	ProjectIDs         []string
	ExpectedRevision   int64
	TeamBoard, Confirm bool
	// Closes sets who closes some of the link's projects (A3): agent or
	// human. Via is how the person confirmed an agent answer (cli-tty or
	// tui); MinimalPolicy agrees to create the minimal status policy.
	Closes        map[string]string
	Via           string
	MinimalPolicy bool
	// OtherTeams are teams the binding lists besides TeamIDs (the write
	// teams), by access: read, comment or linked. ParentTeamID is the
	// parent team among them all (CROSS-TEAM-BINDING-DESIGN §3).
	OtherTeams   map[string]string
	ParentTeamID string
	// Lanes filter which write teams each session writes; Unpinned is the
	// lane of a session with no pin, or read_only, the default (§3.1).
	Lanes    []bus.Lane
	Unpinned string
	// TeamsWithoutProject are write teams no bound project carries yet;
	// each stays inert until one does (FR10 bootstrap).
	TeamsWithoutProject []string
}
type LinkPreviewV2 struct {
	Binding       bus.TaskboardBindingV2   `json:"current"`
	DefaultTeamID string                   `json:"linear_default_team_id"`
	Teams         []linearprovider.Team    `json:"teams"`
	Projects      []linearprovider.Project `json:"projects"`
	TeamBoard     bool                     `json:"team_board"`
	Confirmed     bool                     `json:"confirmed"`
	// Closes is who closes each project's issues, and what this link sets.
	Closes []ClosesLine `json:"closes,omitempty"`
	// MinimalPolicy states what creating the minimal status policy changes.
	MinimalPolicy string `json:"minimal_status_policy,omitempty"`
	// LinearWorkspaceID is the workspace the link records, read from Linear.
	LinearWorkspaceID string `json:"linear_workspace_id,omitempty"`
	// SeedDocs names the guide documents a confirmed link creates.
	SeedDocs any `json:"seed_docs,omitempty"`
	// OtherTeams and ParentTeamID are the link's teams beside the write
	// teams, by access, and its parent team (binding version 3).
	OtherTeams   map[string]string `json:"other_teams,omitempty"`
	ParentTeamID string            `json:"parent_team_id,omitempty"`
	// Lanes and Unpinned are the link's lanes and the lane of a session
	// with no pin.
	Lanes    []bus.Lane `json:"lanes,omitempty"`
	Unpinned string     `json:"unpinned,omitempty"`
	// TeamsWithoutProject are the link's teams no bound project carries
	// yet, and WithoutProject says what that means for each.
	TeamsWithoutProject []string `json:"teams_without_project,omitempty"`
	WithoutProject      []string `json:"without_project,omitempty"`
}

// CommentsInputV2 opts an issue read into its comments. Since is a
// comments_watermark from an earlier read of the same issue, or an absolute
// RFC 3339 time; empty returns every comment within the bound.
type CommentsInputV2 struct {
	Since         string
	ResolveLabels bool
}

// IssueCommentsV2 is an issue read with its comments. Comment bodies are
// untrusted data written by others. Items replaces the embedded comments
// with the same comments flagged from the bus comment ledger.
type IssueCommentsV2 struct {
	linearprovider.IssueV2
	linearprovider.Comments
	Items []CommentV2 `json:"comments"`
	// CommentsOwnership is "available" when every comment is flagged
	// posted_by_clawbridge true or false, or "unavailable" when the bus
	// could not say: nothing may then be taken as a human comment.
	CommentsOwnership string `json:"comments_ownership"`
	// CommentsYoungPending is true while a ClawBridge comment on the issue
	// may be posted but not yet recorded: an unflagged comment may be ours.
	CommentsYoungPending bool `json:"comments_young_pending,omitempty"`
	own                  ownership
}
type IssuesInputV2 struct {
	Limit                         int
	Cursor, LabelID, UpdatedAfter string
	// view filters the rows by a stored view; set only by ViewIssuesV2.
	view *issueView
}

// WriteInputV2 deliberately embeds, rather than extends, the historical digest
// input. Present distinguishes omitted fields from explicit null/empty values.
type WriteInputV2 struct {
	WriteInput
	TeamID  string
	Project linearprovider.Field[*string]
	Labels  linearprovider.Field[[]string]
	// ExpectedLabels is the complete label set the caller last read. It is
	// required for, and only valid with, an update that replaces labels.
	ExpectedLabels linearprovider.Field[[]string]
	Parent         linearprovider.Field[*string]
	Assignee       linearprovider.Field[*string]
	// Priority is Linear's 0-4 on a create or update; omitempty keeps the
	// ledger digest of writes without one unchanged.
	Priority *int `json:",omitempty"`
	// ExpectedTitle and ExpectedDescriptionSHA256 are optional guards on an
	// update that replaces the title or description: the title, or the
	// description_sha256, the caller last read. omitempty keeps the ledger
	// digest of unguarded writes identical to v0.2.0.
	ExpectedTitle             *string `json:",omitempty"`
	ExpectedDescriptionSHA256 *string `json:",omitempty"`
	// AddLabels and RemoveLabels change single labels without replacing the
	// set; omitempty keeps the ledger digest of other writes unchanged.
	AddLabels    []string `json:",omitempty"`
	RemoveLabels []string `json:",omitempty"`
	// InResponseTo names the notified comment a reply answers; omitempty
	// keeps the ledger digest of other writes unchanged.
	InResponseTo string `json:",omitempty"`
	// marker is set only by a claim marker's apply: the body is the bus's
	// exact text, already reserved by marker_go, and label adds are always
	// sent.
	marker bool
}
type RelationInputV2 struct {
	OperationID, Action, Type, FromID, ToID string
	// source is the caller's own end, its first argument, before
	// normalization swaps or sorts the ends; a lane checks it on add.
	source string
}
type ReceiptV2 struct {
	Kind       string  `json:"kind"`
	Outcome    string  `json:"outcome"`
	ID         string  `json:"id,omitempty"`
	Identifier string  `json:"identifier,omitempty"`
	TeamID     string  `json:"team_id,omitempty"`
	ProjectID  *string `json:"project_id,omitempty"`
	IssueID    string  `json:"issue_id,omitempty"`
	// ParentID is the comment a reply answers: the thread root when the
	// requested parent, RequestedParentID, was itself a reply.
	ParentID          string `json:"parent_id,omitempty"`
	RequestedParentID string `json:"requested_parent_id,omitempty"`
	// BodySHA256 is the SHA-256 of the comment body posted, marker included.
	BodySHA256 string `json:"body_sha256,omitempty"`
	// InResponseTo is the notified comment the bus recorded this reply as
	// answering.
	InResponseTo string `json:"in_response_to,omitempty"`
	Action       string `json:"action,omitempty"`
	Type         string `json:"type,omitempty"`
	FromID       string `json:"from_id,omitempty"`
	ToID         string `json:"to_id,omitempty"`
	// Unverified names the requested fields (labels, assignee, parent,
	// state) that Linear's response did not report, so the write could not
	// confirm them.
	Unverified []string `json:"unverified,omitempty"`
	// LabelsAdded and LabelsRemoved are the label deltas actually sent.
	LabelsAdded   []string `json:"labels_added,omitempty"`
	LabelsRemoved []string `json:"labels_removed,omitempty"`
}
