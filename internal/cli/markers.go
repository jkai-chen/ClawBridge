package cli

import (
	"errors"
	"strings"

	"clawbridge/internal/clirender"
	"clawbridge/internal/linearprovider"
)

// errMarkersOlderServer is `assignment markers` against a server without
// the marker_status read.
var errMarkersOlderServer = errors.New("the ClawBridge server is older than this client and has no marker read; upgrade the server")

func init() { clirender.Register("marker_status", markerStatusView) }

// markerArgs turns `assignment markers --issue X | --assignment ID` into
// marker_status's args: a UUID as issue_uuid, an identifier such as ENG-1
// as issue_identifier, which the server matches against the identifier its
// assignments recorded. Neither reads Linear.
func markerArgs(a map[string]any) error {
	issue, _ := a["issue"].(string)
	assignment, _ := a["assignment"].(string)
	delete(a, "issue")
	delete(a, "assignment")
	issue, assignment = strings.TrimSpace(issue), strings.TrimSpace(assignment)
	if (issue == "") == (assignment == "") {
		return errors.New("assignment markers takes --issue ISSUE_UUID|IDENTIFIER or --assignment ID, exactly one")
	}
	switch {
	case assignment != "":
		a["assignment_id"] = assignment
	case linearprovider.ValidID(strings.ToLower(issue)):
		a["issue_uuid"] = strings.ToLower(issue)
	default:
		// The server checks the identifier's shape.
		a["issue_identifier"] = issue
	}
	return nil
}

// errMarkersNoIdentifier is an identifier sent to a server whose read
// takes none.
var errMarkersNoIdentifier = errors.New("this server reads markers by issue UUID or assignment only; pass the issue UUID")

// markerStatusError says plainly when the server predates the read, or its
// read takes no identifier.
func markerStatusError(err error) error {
	if strings.Contains(err.Error(), `unsupported operation "marker_status"`) {
		return errMarkersOlderServer
	}
	if strings.Contains(err.Error(), `unsupported argument "issue_identifier"`) {
		return errMarkersNoIdentifier
	}
	return err
}

// markerPhase is the word a person uses for a marker's phase: the stored
// close is a release.
func markerPhase(phase string) string {
	if phase == "close" {
		return "release"
	}
	return phase
}

// markerStatusView is one line per marker row, newest first: its phase,
// when it last changed, and the server's summary. --json has every field.
func markerStatusView(v any) (clirender.Doc, bool) {
	r, ok := resultData(v)
	if !ok || r["markers"] == nil {
		return clirender.Doc{}, false
	}
	markers := list(r, "markers")
	rows := [][]string{}
	issue := ""
	for _, m := range markers {
		issue = firstNonEmpty(issue, field(m, "issue_identifier"), field(m, "issue_uuid"))
		rows = append(rows, []string{markerPhase(field(m, "phase")), exact(field(m, "updated_at")), field(m, "summary")})
	}
	var d clirender.Doc
	if issue != "" {
		d.Title("Claim markers of " + issue)
	} else {
		d.Title("Claim markers")
	}
	d.Records([]clirender.Col{{Name: "PHASE"}, {Name: "UPDATED"}, {Name: "SUMMARY", Keep: true}}, rows, "No claim or release markers for this issue or assignment.")
	return d, true
}
