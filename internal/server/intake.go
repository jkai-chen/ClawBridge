package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"

	"clawbridge/internal/bus"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// IntakePath is the send-only MCP endpoint a Linear Loop calls to ask for an
// agent on an issue (live/INTAKE-DESIGN.md §6).
const IntakePath = "/mcp/intake"

// intakeBodyBytes bounds one /mcp/intake request (§6.3).
const intakeBodyBytes = 8 << 10

// IntakeStore is the hosted store's intake API.
type IntakeStore interface {
	IntakeToken(ctx context.Context, token string) (bus.IntakeGrant, error)
	IntakeRequest(ctx context.Context, token, issue string, prove IssueProver) (map[string]any, error)
}

// IssueProver proves, as the Linear app, that an issue (UUID or display
// identifier) is inside a project's task board binding. It returns
// bus.ErrIntakeOutOfBinding for one that is not, or that it cannot see.
type IssueProver = func(ctx context.Context, workspaceID, projectID, issue string) (bus.IntakeIssue, error)

type intakeTokenKey struct{}

type intakeInput struct {
	Issue string `json:"issue" jsonschema:"the Linear issue UUID, or its identifier such as ABC-12"`
}

type intakeOutput struct {
	Result    string `json:"result" jsonschema:"queued, already_queued, assigned or refused"`
	Code      string `json:"code,omitempty" jsonschema:"why a request was refused: not_in_binding, intake_off, rate_limited, invalid_issue, app_unavailable or unavailable"`
	RequestID string `json:"request_id,omitempty"`
}

// newIntakeHandler serves /mcp/intake: Streamable HTTP, stateless, with one
// tool. Only an intake token is accepted, checked before any MCP handling;
// a Google or user token, or none, is refused. The tool returns no issue
// data, session names or queue position.
func newIntakeHandler(store IntakeStore, prove IssueProver, ips *limiter) http.Handler {
	closed, destructive := false, false
	tool := mcp.NewServer(&mcp.Implementation{Name: "clawbridge-intake", Version: "0.1.0"}, &mcp.ServerOptions{
		Instructions: "Send-only: ask ClawBridge for an agent on one Linear issue of this project. Nothing is read back; a human decides who takes it.",
	})
	mcp.AddTool(tool, &mcp.Tool{
		Name:        "clawbridge_request_agent",
		Description: "Ask ClawBridge for an agent on a Linear issue of this project. The request only queues the issue; it carries no instructions.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true, OpenWorldHint: &closed, DestructiveHint: &destructive},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in intakeInput) (*mcp.CallToolResult, intakeOutput, error) {
		token, _ := ctx.Value(intakeTokenKey{}).(string)
		out, err := store.IntakeRequest(ctx, token, strings.TrimSpace(in.Issue), prove)
		if errors.Is(err, bus.ErrIntakeUnauthorized) {
			return nil, intakeOutput{Result: "refused", Code: "unauthorized"}, nil
		}
		if err != nil {
			return nil, intakeOutput{Result: "refused", Code: "unavailable"}, nil
		}
		result, _ := out["result"].(string)
		code, _ := out["code"].(string)
		id, _ := out["request_id"].(string)
		return nil, intakeOutput{Result: result, Code: code, RequestID: id}, nil
	})
	// Send-only: every method but the tool's is refused.
	tool.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			switch method {
			case "initialize", "notifications/initialized", "ping", "tools/list", "tools/call":
				return next(ctx, method, req)
			}
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeMethodNotFound, Message: "method not found"}
		}
	})
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return tool }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, MaxRequestBodyBytes: intakeBodyBytes})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		if !ips.allow("intake-ip:" + host) {
			fail(w, 429, "rate_limited", "request limit reached")
			return
		}
		headers := r.Header.Values("Authorization")
		fields := []string{}
		if len(headers) == 1 {
			fields = strings.Fields(headers[0])
		}
		if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") || len(fields[1]) > 128 {
			fail(w, 401, "unauthenticated", "an intake token is required")
			return
		}
		if _, err = store.IntakeToken(r.Context(), fields[1]); err != nil {
			if errors.Is(err, bus.ErrIntakeUnauthorized) {
				fail(w, 401, "unauthenticated", "an intake token is required")
			} else {
				fail(w, 503, "unavailable", "intake is unavailable")
			}
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, intakeBodyBytes)
		mcpHandler.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), intakeTokenKey{}, fields[1])))
	})
}
