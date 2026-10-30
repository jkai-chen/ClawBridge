package notify

import (
	"context"
	"encoding/json"
	"errors"

	"clawbridge/internal/bus"
)

// NotificationStatus adds recipient-controlled wake policy to unread metadata.
// Inbox status and direct resource reads retain their original six fields.
type NotificationStatus struct {
	Status
	Policy string `json:"policy"`
}

func ParseNotificationStatus(r bus.Result) (NotificationStatus, error) {
	var s NotificationStatus
	raw, err := json.Marshal(r.Data)
	var fields map[string]json.RawMessage
	if err != nil || len(raw) > 4096 || json.Unmarshal(raw, &fields) != nil || len(fields) != 7 {
		return s, Permanent(errors.New("invalid notification status"))
	}
	if json.Unmarshal(fields["policy"], &s.Policy) != nil || (s.Policy != "accept" && s.Policy != "hold" && s.Policy != "refuse") {
		return s, Permanent(errors.New("invalid notification policy"))
	}
	delete(fields, "policy")
	s.Status, err = ParseStatus(bus.Result{Source: r.Source, Data: fields})
	return s, err
}
func FetchNotification(ctx context.Context, e Executor, r bus.Request) (bus.Result, NotificationStatus, error) {
	r.Operation = "notification_status"
	result, err := e.Execute(ctx, r)
	if ctx.Err() != nil {
		return bus.Result{}, NotificationStatus{}, ctx.Err()
	}
	if err != nil {
		return bus.Result{}, NotificationStatus{}, err
	}
	s, err := ParseNotificationStatus(result)
	return result, s, err
}

// QueueStatus is what the channel server takes from notification_status
// with queue:true: the wake policy and the queue's two counts. Nothing else
// in the answer is read, so no text from the server can reach a push.
type QueueStatus struct {
	Policy    string
	Unacked   int64
	LatestSeq int64
}

// ParseQueueStatus reads a QueueStatus from an authoritative answer. An
// older server's answer, without the queue's counts, is refused.
func ParseQueueStatus(r bus.Result) (QueueStatus, error) {
	var s QueueStatus
	if r.Source != "server" && r.Source != "local-authority" {
		return s, Permanent(errors.New("notification requires authoritative status"))
	}
	raw, err := json.Marshal(r.Data)
	var fields struct {
		Policy    *string `json:"policy"`
		Unacked   *int64  `json:"unacked_count"`
		LatestSeq *int64  `json:"latest_seq"`
	}
	if err != nil || len(raw) > 4096 || json.Unmarshal(raw, &fields) != nil || fields.Policy == nil || fields.Unacked == nil || fields.LatestSeq == nil {
		return s, Permanent(errors.New("invalid queue status"))
	}
	if p := *fields.Policy; p != "accept" && p != "hold" && p != "refuse" {
		return s, Permanent(errors.New("invalid notification policy"))
	}
	if *fields.Unacked < 0 || *fields.Unacked > MaxSequence || *fields.LatestSeq < 0 || *fields.LatestSeq > MaxSequence {
		return s, Permanent(errors.New("invalid queue counts"))
	}
	return QueueStatus{Policy: *fields.Policy, Unacked: *fields.Unacked, LatestSeq: *fields.LatestSeq}, nil
}

// FetchQueueStatus asks for r's session's notification_status with the
// queue's counts.
func FetchQueueStatus(ctx context.Context, e Executor, r bus.Request) (QueueStatus, error) {
	r.Operation = "notification_status"
	args := map[string]any{"queue": true}
	for k, v := range r.Args {
		args[k] = v
	}
	r.Args = args
	result, err := e.Execute(ctx, r)
	if ctx.Err() != nil {
		return QueueStatus{}, ctx.Err()
	}
	if err != nil {
		return QueueStatus{}, err
	}
	return ParseQueueStatus(result)
}
