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
}
type LinkPreviewV2 struct {
	Binding       bus.TaskboardBindingV2   `json:"current"`
	DefaultTeamID string                   `json:"linear_default_team_id"`
	Teams         []linearprovider.Team    `json:"teams"`
	Projects      []linearprovider.Project `json:"projects"`
	TeamBoard     bool                     `json:"team_board"`
	Confirmed     bool                     `json:"confirmed"`
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
	Action     string  `json:"action,omitempty"`
	Type       string  `json:"type,omitempty"`
	FromID     string  `json:"from_id,omitempty"`
	ToID       string  `json:"to_id,omitempty"`
	// Unverified names the requested fields (labels, assignee, parent,
	// state) that Linear's response did not report, so the write could not
	// confirm them.
	Unverified []string `json:"unverified,omitempty"`
	// LabelsAdded and LabelsRemoved are the label deltas actually sent.
	LabelsAdded   []string `json:"labels_added,omitempty"`
	LabelsRemoved []string `json:"labels_removed,omitempty"`
}
