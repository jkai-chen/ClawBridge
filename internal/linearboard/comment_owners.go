package linearboard

import (
	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// CommentV2 is a comment read with what the bus comment ledger knows of it.
// PostedByClawBridge is absent when ownership is unavailable: the read then
// cannot tell ClawBridge's comments from anyone else's.
type CommentV2 struct {
	linearprovider.Comment
	PostedByClawBridge *bool              `json:"posted_by_clawbridge,omitempty"`
	ClawBridgeSession  *ClawBridgeSession `json:"clawbridge_session,omitempty"`
	// ClawBridgeMatch is "ledger" when the bus recorded the comment,
	// "pending_body" when an unsettled reservation's body matched it, or
	// "app" when the ClawBridge Linear app wrote it.
	ClawBridgeMatch string `json:"clawbridge_match,omitempty"`
}

// ClawBridgeSession is the session whose reservation posted a comment.
type ClawBridgeSession struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// pendingReservation is an unsettled reservation older than the bus's young
// window, and the comment its body matched in this read, if any.
type pendingReservation struct {
	digest, bodySHA256 string
	reserved           time.Time
	matched            *CommentV2
}

// ownership is what one linear_comments_posted call said about a read.
type ownership struct {
	available, youngPending bool
	pending                 []pendingReservation
}

// commentOwners flags which of an issue's comments ClawBridge posted: those
// the bus ledger recorded or a pending reservation's body matched, and any
// other the ClawBridge Linear app authored, since only the server holds the
// app's token. A bus that cannot answer leaves ownership unavailable; the
// read itself still succeeds.
func (s *Service) commentOwners(ctx context.Context, scope bus.Request, issue string, comments []linearprovider.Comment) ([]CommentV2, ownership) {
	out, own := s.ledgerOwners(ctx, scope, issue, comments)
	if app := s.appUserID(ctx); app != "" {
		yes := true
		for i := range out {
			if out[i].AuthorID == app && (out[i].PostedByClawBridge == nil || !*out[i].PostedByClawBridge) {
				out[i].PostedByClawBridge, out[i].ClawBridgeMatch = &yes, "app"
			}
		}
	}
	return out, own
}

func (s *Service) ledgerOwners(ctx context.Context, scope bus.Request, issue string, comments []linearprovider.Comment) ([]CommentV2, ownership) {
	out := make([]CommentV2, len(comments))
	ids := make([]any, len(comments))
	for i, c := range comments {
		out[i] = CommentV2{Comment: c}
		ids[i] = c.ID
	}
	if scope.Session == "" || len(ids) > 500 {
		return out, ownership{}
	}
	data, err := s.commentLedger(ctx, scope, "linear_comments_posted", map[string]any{"issue_uuid": issue, "comment_ids": ids})
	if err != nil {
		return out, ownership{}
	}
	posted, _ := data["posted"].(map[string]any)
	young, ok := data["young_pending"].(bool)
	if !ok {
		return out, ownership{}
	}
	own := ownership{available: true, youngPending: young}
	if list, ok := data["pending"].([]any); ok {
		for _, raw := range list {
			row, _ := raw.(map[string]any)
			digest, _ := row["operation_digest"].(string)
			body, _ := row["body_sha256"].(string)
			at, _ := row["reserved_at"].(string)
			reserved, err := time.Parse(time.RFC3339Nano, at)
			if err != nil || digest == "" || body == "" {
				return out, ownership{}
			}
			own.pending = append(own.pending, pendingReservation{digest: digest, bodySHA256: body, reserved: reserved})
		}
	}
	yes, no := true, false
	for i := range out {
		out[i].PostedByClawBridge = &no
		row, ok := posted[out[i].ID].(map[string]any)
		if !ok {
			continue
		}
		id, _ := row["session_id"].(string)
		name, _ := row["session_name"].(string)
		out[i].PostedByClawBridge, out[i].ClawBridgeMatch = &yes, "ledger"
		if id != "" {
			out[i].ClawBridgeSession = &ClawBridgeSession{ID: id, Name: name}
		}
	}
	own.matchPending(out)
	return out, own
}

// matchPending gives each old pending reservation the earliest unflagged
// comment whose exact body digest matches it, created from 2 minutes before
// the reservation to 10 minutes after; each comment matches one
// reservation.
func (o *ownership) matchPending(comments []CommentV2) {
	for i := range o.pending {
		p := &o.pending[i]
		var best *CommentV2
		var bestAt time.Time
		for j := range comments {
			c := &comments[j]
			if *c.PostedByClawBridge || c.BodyTruncated {
				continue
			}
			created, err := time.Parse(time.RFC3339Nano, c.CreatedAt)
			if err != nil || created.Before(p.reserved.Add(-2*time.Minute)) || created.After(p.reserved.Add(10*time.Minute)) {
				continue
			}
			sum := sha256.Sum256([]byte(c.UntrustedBody))
			if hex.EncodeToString(sum[:]) != p.bodySHA256 {
				continue
			}
			if best == nil || created.Before(bestAt) {
				best, bestAt = c, created
			}
		}
		if best != nil {
			yes := true
			best.PostedByClawBridge, best.ClawBridgeMatch = &yes, "pending_body"
			p.matched = best
		}
	}
}
