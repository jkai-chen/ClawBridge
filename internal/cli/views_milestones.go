package cli

import (
	"encoding/json"
	"strconv"

	"clawbridge/internal/bus"
	"clawbridge/internal/clirender"
	"clawbridge/internal/linearboard"
)

func init() { clirender.Register("linear_milestone_list", milestoneListView) }

// milestoneListView is `linear milestone list`: one line per milestone, in
// the order the board gives (sort order, then name): NAME, TARGET DATE or -
// (? when the provider's date was unreadable), PROGRESS as a whole percent
// or ?, then the ID that --milestone takes. The name is text written by
// others; clirender makes it terminal-safe.
func milestoneListView(v any) (clirender.Doc, bool) {
	if r, ok := v.(bus.Result); ok {
		v = r.Data
	}
	list, typed := v.(linearboard.MilestoneListV2)
	if !typed {
		raw, err := json.Marshal(v)
		if err != nil || json.Unmarshal(raw, &list) != nil || list.Project == "" {
			return clirender.Doc{}, false
		}
	}
	var d clirender.Doc
	if len(list.Milestones) == 0 {
		d.Text("No milestones.")
	}
	for _, m := range list.Milestones {
		date := "?"
		switch {
		case m.TargetDate != nil:
			date = *m.TargetDate
		case m.TargetDateKnown:
			date = "-"
		}
		progress := "?"
		if m.Progress != nil && m.ProgressKnown {
			progress = strconv.Itoa(*m.Progress) + "%"
		}
		d.Whole(m.Name + "  " + date + "  " + progress + "  " + m.ID)
	}
	return d, true
}
