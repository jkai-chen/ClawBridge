package bus

import "testing"

// GUI P2-6: the web UI's session History would read a session's inbox as
// that session. That is not a pure read: it stamps delivered_at on messages
// not yet delivered (messages.go inbox) and the named session's last_seen
// (store.go touchReadActor), which makes an idle session look alive. So the
// UI does not build History until FR46 gives a person's read.
func TestInboxReadStampsDeliveryAndPresence(t *testing.T) {
	h := newHarness(t)
	h.send("hello")
	before := h.stamps()
	h.call("inbox", "b", map[string]any{"unread_only": false})
	if after := h.stamps(); after == before {
		t.Fatal("an inbox read changed no delivery stamp or last_seen; History may be buildable")
	}
}
