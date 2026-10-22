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
	Limit           int
	Cursor, LabelID string
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
}
