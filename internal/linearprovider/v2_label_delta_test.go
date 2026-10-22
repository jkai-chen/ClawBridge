package linearprovider

import (
	"context"
	"reflect"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestV2LabelDeltasArePinnedAndSentAsIDs(t *testing.T) {
	f := &fake{tools: []*mcp.Tool{v2MutationTool("update_issue", true)}, result: issueResult()}
	p, _ := NewWithCaller(context.Background(), f)
	caps, _ := p.Capabilities(context.Background())
	if !caps.LabelDeltas {
		t.Fatal("documented addLabels/removeLabels not recognized", caps.Reasons)
	}
	if _, e := p.UpdateV2(context.Background(), UpdateInputV2{UpdateInput: UpdateInput{ID: iid}, AddLabels: []string{uid, tid}, RemoveLabels: []string{iid}}); e != nil {
		t.Fatal(e)
	}
	args := f.args[len(f.args)-1]
	if !reflect.DeepEqual(args["addLabels"], []string{tid, uid}) || !reflect.DeepEqual(args["removeLabels"], []string{iid}) {
		t.Fatal("deltas not sent as canonical label IDs", args)
	}
	if _, present := args["labelIds"]; present {
		t.Fatal("a delta replaced the label set", args)
	}
	calls := len(f.calls)
	for name, in := range map[string]UpdateInputV2{
		"with replacement": {UpdateInput: UpdateInput{ID: iid}, AddLabels: []string{tid}, Labels: Field[[]string]{Present: true, Value: []string{}}},
		"label name":       {UpdateInput: UpdateInput{ID: iid}, RemoveLabels: []string{"needs-agent"}},
		"empty":            {UpdateInput: UpdateInput{ID: iid}, AddLabels: []string{}},
	} {
		if _, e := p.UpdateV2(context.Background(), in); e == nil || len(f.calls) != calls {
			t.Fatal(name, "sent", e)
		}
	}
	drifted := v2MutationTool("update_issue", true)
	drifted.InputSchema.(map[string]any)["properties"].(map[string]any)["addLabels"].(map[string]any)["description"] = "Label names or IDs; replaces the set"
	f = &fake{tools: []*mcp.Tool{drifted}, result: issueResult()}
	p, _ = NewWithCaller(context.Background(), f)
	caps, _ = p.Capabilities(context.Background())
	in := UpdateInputV2{UpdateInput: UpdateInput{ID: iid}, AddLabels: []string{tid}}
	if caps.LabelDeltas || caps.Reasons["label_deltas"] == "" || p.ValidateUpdateV2(in) == nil {
		t.Fatal("drifted addLabels still trusted", caps)
	}
	if _, e := p.UpdateV2(context.Background(), in); e == nil || len(f.calls) != 0 {
		t.Fatal("drifted delta sent", e)
	}
}
