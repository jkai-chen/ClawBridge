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
