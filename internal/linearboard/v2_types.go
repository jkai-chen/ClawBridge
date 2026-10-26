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
type RelationInputV2 struct{ OperationID, Action, Type, FromID, ToID string }
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
