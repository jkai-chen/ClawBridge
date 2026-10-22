package linearprovider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// commentServer is a real go-sdk MCP server, reached through the production
// client (New, boundedTransport, the streamable transport). Its list_comments
// answers page 1 normally; page 2 is larger than MaxResultBytes whenever the
// requested limit exceeds fitsAt, so only a smaller page fits.
type commentServer struct {
	mu     sync.Mutex
	limits []int
}

func newCommentServer(t *testing.T, jsonResponse bool, page1Fits, fitsAt int) (*Provider, *commentServer) {
	t.Helper()
	cs := &commentServer{}
	srv := mcp.NewServer(&mcp.Implementation{Name: "synthetic", Version: "1"}, nil)
	native := nativeCommentList()
	srv.AddTool(&mcp.Tool{Name: native.Name, Description: native.Description, InputSchema: native.InputSchema}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args struct {
			Limit  int    `json:"limit"`
			Cursor string `json:"cursor"`
		}
		_ = json.Unmarshal(req.Params.Arguments, &args)
		cs.mu.Lock()
		cs.limits = append(cs.limits, args.Limit)
		cs.mu.Unlock()
		var page map[string]any
		switch {
		case args.Cursor == "" && args.Limit <= page1Fits:
			page = commentPage("page-2", commentRow(commentA, "2026-10-22T08:00:00.000Z", "2026-10-22T08:00:00.000Z"))
		case args.Cursor == "page-2" && args.Limit <= fitsAt:
			page = commentPage("", commentRow(commentB, "2026-10-22T09:00:00.000Z", "2026-10-22T09:00:00.000Z"))
		default:
			huge := commentRow(commentC, "2026-10-22T09:00:00.000Z", "2026-10-22T09:00:00.000Z")
			huge["body"] = strings.Repeat("x", MaxResultBytes+1024)
			page = commentPage("", huge)
		}
		raw, _ := json.Marshal(page)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(raw)}}}, nil
	})
	h := httptest.NewTLSServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{JSONResponse: jsonResponse}))
	t.Cleanup(h.Close)
	u, _ := url.Parse(h.URL)
	p, e := New(context.Background(), &http.Client{Transport: toLocal{u, h.Client().Transport}}, true)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { p.Close() })
	return p, cs
}

func (cs *commentServer) seen() []int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return append([]int{}, cs.limits...)
}

// Through the real transport, an oversized page is recognised as the
// response cap (go-sdk reports the body error only as text), the connection
// it broke is reopened, and the same page is retried at half size.
func TestV2CommentsRealTransportShrinksAnOversizedPage(t *testing.T) {
	withCommentClock(t, "2026-10-22T10:10:00Z")
	for _, jsonResponse := range []bool{true, false} {
		t.Run(fmt.Sprint("json=", jsonResponse), func(t *testing.T) {
			p, cs := newCommentServer(t, jsonResponse, 50, 12)
			out, e := readComments(t, p, "")
			if e != nil || !out.Complete || len(out.Items) != 2 {
				t.Fatal("oversized page not retried smaller", out, e, cs.seen())
			}
			if got := fmt.Sprint(cs.seen()); got != "[50 50 25 12]" {
				t.Fatal("page sizes", got)
			}
			if p.Alive(context.Background()) {
				t.Fatal("a connection that hit the cap offered for reuse")
			}
		})
	}
}

// A page that never fits ends the read incomplete with earlier rows kept;
// a first page that never fits returns the issue's read with no comments.
func TestV2CommentsRealTransportGivesUpAtTheSmallestPage(t *testing.T) {
	withCommentClock(t, "2026-10-22T10:10:00Z")
	p, cs := newCommentServer(t, true, 50, 0)
	out, e := readComments(t, p, "")
	if e != nil || out.Complete || out.WatermarkReason != "result_limit" || !out.Truncated || len(out.Items) != 1 || out.Items[0].ID != commentA {
		t.Fatal("page 2 never fitting", out, e, cs.seen())
	}
	if got := fmt.Sprint(cs.seen()); got != "[50 50 25 12 6 5]" {
		t.Fatal("page sizes", got)
	}
	p, _ = newCommentServer(t, true, 0, 0)
	out, e = readComments(t, p, "")
	if e != nil || out.WatermarkReason != "result_limit" || len(out.Items) != 0 {
		t.Fatal("page 1 never fitting", out, e)
	}
}
