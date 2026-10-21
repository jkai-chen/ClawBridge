package hostobserve

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"
)

const testThread = "11111111-2222-3333-4444-555555555555"

func testBinding() Binding {
	return Binding{"codex", "unix:///tmp/host-observer-fixture.sock", testThread}
}

type fakeTransport struct {
	requests  []map[string]any
	responses [][]byte
	closed    bool
}

func (t *fakeTransport) Send(_ context.Context, b []byte) error {
	var r map[string]any
	if err := json.Unmarshal(b, &r); err != nil {
		return err
	}
	t.requests = append(t.requests, r)
	return nil
}
func (t *fakeTransport) Receive(context.Context) ([]byte, error) {
	if len(t.responses) == 0 {
		return nil, errors.New("disconnected")
	}
	b := t.responses[0]
	t.responses = t.responses[1:]
	return b, nil
}
func (t *fakeTransport) Close() error { t.closed = true; return nil }
func jsonBytes(v any) []byte          { b, _ := json.Marshal(v); return b }
func response(id int, state string) []byte {
	return jsonBytes(map[string]any{"id": id, "result": map[string]any{"thread": map[string]any{"id": testThread, "status": map[string]any{"type": state}}}})
}
func TestCoordinationHostProtocolOnlyMetadataMethods(t *testing.T) {
	transport := &fakeTransport{responses: [][]byte{[]byte(`{"id":1,"result":{}}`), []byte(`{"method":"approval/request","id":50,"params":{"secret":"not returned"}}`), response(2, "idle")}}
	observer, err := NewObserver(context.Background(), testBinding(), transport)
	if err != nil {
		t.Fatal(err)
	}
	result, err := Run(context.Background(), Config{Binding: testBinding(), For: "idle", Timeout: time.Second, Interval: time.Second, Dial: func(context.Context, Binding) (Observer, error) { return observer, nil }, Now: func() time.Time { return time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC) }})
	if err != nil || result.Outcome != "matched" || !transport.closed || result.LastObservation.State != "idle" || result.LastObservation.QueueState != "unknown" {
		t.Fatal(result, err)
	}
	methods := []string{}
	for _, r := range transport.requests {
		methods = append(methods, r["method"].(string))
	}
	if !reflect.DeepEqual(methods, []string{"initialize", "initialized", "thread/read"}) {
		t.Fatal(methods)
	}
	params := transport.requests[2]["params"].(map[string]any)
	if params["includeTurns"] != false || params["threadId"] != testThread || result.LastObservation.ObservedAt != "2026-10-21T00:00:00Z" {
		t.Fatal(params, result)
	}
}
func TestCoordinationHostMalformedAndUnrelatedBounds(t *testing.T) {
	cases := map[string][]byte{
		"wrong-id": response(99, "idle"), "unknown-state": response(2, "stopped"), "null-status": []byte(`{"id":2,"result":{"thread":{"id":"` + testThread + `","status":null}}}`),
		"unknown-flag": []byte(`{"id":2,"result":{"thread":{"id":"` + testThread + `","status":{"type":"active","activeFlags":["secret"]}}}}`),
		"null-flag":    []byte(`{"id":2,"result":{"thread":{"id":"` + testThread + `","status":{"type":"active","activeFlags":null}}}}`),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			tr := &fakeTransport{responses: [][]byte{[]byte(`{"id":1,"result":{}}`), raw}}
			obs, err := NewObserver(context.Background(), testBinding(), tr)
			if err != nil {
				t.Fatal(err)
			}
			_, err = obs.Observe(context.Background())
			if err == nil {
				t.Fatal("malformed host accepted")
			}
			obs.Close()
		})
	}
	tr := &fakeTransport{responses: [][]byte{[]byte(`{"id":1,"result":{}}`)}}
	for i := 0; i < 129; i++ {
		tr.responses = append(tr.responses, []byte(`{"method":"unrelated"}`))
	}
	obs, err := NewObserver(context.Background(), testBinding(), tr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = obs.Observe(context.Background()); err == nil {
		t.Fatal("unbounded unrelated frames")
	}
	obs.Close()
}

type sequenceObserver struct {
	states []string
	calls  int
	closed bool
}

func (o *sequenceObserver) Observe(context.Context) (Observation, error) {
	if o.calls >= len(o.states) {
		return Observation{}, Unavailable()
	}
	state := o.states[o.calls]
	o.calls++
	return Observation{"codex", testThread, state, []string{}, "", "unknown"}, nil
}
func (o *sequenceObserver) Close() error { o.closed = true; return nil }
func TestCoordinationHostWaitIsLevelTriggeredAndNeverInfersExit(t *testing.T) {
	for _, target := range []string{"idle", "unloaded"} {
		state := "idle"
		if target == "unloaded" {
			state = "notLoaded"
		}
		o := &sequenceObserver{states: []string{"active", state}}
		waits := 0
		c := Config{Binding: testBinding(), For: target, Timeout: time.Second, Interval: time.Second, Dial: func(context.Context, Binding) (Observer, error) { return o, nil }, Wait: func(context.Context, time.Duration) error { waits++; return nil }}
		r, err := Run(context.Background(), c)
		if err != nil || r.Outcome != "matched" || o.calls != 2 || waits != 1 || !o.closed {
			t.Fatal(r, err)
		}
		o = &sequenceObserver{states: []string{state}}
		waits = 0
		r, err = Run(context.Background(), c)
		if err != nil || r.Outcome != "matched" || waits != 0 {
			t.Fatal("already matching waited", r, err)
		}
	}
	o := &sequenceObserver{states: []string{"active"}}
	c := Config{Binding: testBinding(), For: "idle", Timeout: time.Second, Interval: time.Second, Dial: func(context.Context, Binding) (Observer, error) { return o, nil }, Wait: func(context.Context, time.Duration) error { return nil }}
	r, err := Run(context.Background(), c)
	if err != nil || r.Outcome != "unavailable" || r.LastObservation.State != "active" {
		t.Fatal(r, err)
	}
	c.For = "exit"
	if _, err = Run(context.Background(), c); err == nil {
		t.Fatal("exit supported")
	}
}
func TestCoordinationHostOptionsRejectBeforeDial(t *testing.T) {
	for _, endpoint := range []string{"ws://localhost:9000", "unix://relative", "unix:///tmp/socket?x=y", "unix:///tmp/socket#", "unix:relative"} {
		c := Config{Binding: testBinding(), Timeout: time.Second, Interval: time.Second, Dial: func(context.Context, Binding) (Observer, error) { t.Fatal("invalid endpoint dialed"); return nil, nil }}
		c.Binding.Endpoint = endpoint
		if _, err := Run(context.Background(), c); err == nil {
			t.Fatal(endpoint)
		}
	}
}

func TestCoordinationHostWaitTimeoutAndCancellationPreserveLastObservation(t *testing.T) {
	for _, cancelEarly := range []bool{false, true} {
		o := &sequenceObserver{states: []string{"active"}}
		ctx, cancel := context.WithCancel(context.Background())
		c := Config{Binding: testBinding(), For: "idle", Timeout: time.Second, Interval: time.Second, Dial: func(context.Context, Binding) (Observer, error) { return o, nil }, Wait: func(ctx context.Context, _ time.Duration) error {
			if cancelEarly {
				cancel()
			}
			<-ctx.Done()
			return ctx.Err()
		}}
		r, err := Run(ctx, c)
		cancel()
		want := "timeout"
		if cancelEarly {
			want = "cancelled"
		}
		if err != nil || r.Outcome != want || r.LastObservation == nil || r.LastObservation.State != "active" || !o.closed {
			t.Fatal(r, err)
		}
	}
}
