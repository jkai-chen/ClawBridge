package cli

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/dispatchermodel"
	"clawbridge/internal/tui"
	tea "github.com/charmbracelet/bubbletea"
)

// §11.9: the CLI and the TUI are two front ends on one package. The same
// answers, given to each, send the same setup request in the same scope.
func TestModelSetupCLIAndTUIAgree(t *testing.T) {
	fixtures := []struct {
		name, provider, baseURL, model, runsPerHour, daily string
		choice                                             int
	}{
		{name: "listed", provider: "anthropic", model: "claude-sonnet-4-5", runsPerHour: "", daily: "500000", choice: 0},
		{name: "custom", provider: "openai-compatible", baseURL: "https://llm.example.test/v1", model: "local-model", runsPerHour: "4", daily: "20000", choice: 8},
		// A number picks from the agent's catalogue, the same model in both.
		{name: "picked from the list", provider: "anthropic", model: "2", daily: "500000", choice: 0},
	}
	for _, f := range fixtures {
		t.Run(f.name, func(t *testing.T) {
			config := Config{Workspace: "w", Project: "p", Session: "s"}

			// The CLI, answered at its prompts.
			answers := f.provider + "\n"
			if f.baseURL != "" {
				answers += f.baseURL + "\n"
			}
			answers += f.model + "\n" + f.runsPerHour + "\n\n\n\n" + f.daily + "\nyes\n"
			modelTerminal(t, answers, modelTestKey, true)
			cliServer := &fakeModelServer{outcomes: []string{"active"}}
			if err := runModelSetup(context.Background(), cliServer, config, map[string]any{}, &bytes.Buffer{}); err != nil {
				t.Fatal(err)
			}

			// The TUI, typed at its screen.
			tuiServer := &fakeModelServer{outcomes: []string{"active"}}
			doctor := modelDoctorServer{tuiServer}
			m, err := tui.New(tui.Config{Executor: doctor, Mode: tui.ModeLocal, Workspace: config.Workspace, Project: config.Project, Session: config.Session})
			if err != nil {
				t.Fatal(err)
			}
			m, cmd := m.Show(tui.DispatcherModel)
			m = settleTUI(m, cmd)
			keys := []tea.KeyMsg{runes("s")}
			for i := 0; i < f.choice; i++ {
				keys = append(keys, tea.KeyMsg{Type: tea.KeyDown})
			}
			keys = append(keys, enter())
			if f.baseURL != "" {
				keys = append(keys, runes(f.baseURL), enter())
			}
			keys = append(keys, runes(f.model), enter(), runes(modelTestKey), enter())
			for _, v := range []string{f.runsPerHour, "", "", "", f.daily} {
				if v != "" {
					keys = append(keys, runes(v))
				}
				keys = append(keys, enter())
			}
			keys = append(keys, runes("yes"), enter())
			for _, k := range keys {
				m, cmd = m.UpdateModel(k)
				m = settleTUI(m, cmd)
			}
			if s := m.ModelSetupState(); s.Step != "" || !strings.Contains(s.Outcome, "the model is set up") {
				t.Fatalf("%+v", s)
			}

			cliSetup, tuiSetup := setupRequest(t, cliServer.requests), setupRequest(t, doctor.requests)
			if !reflect.DeepEqual(cliSetup, tuiSetup) {
				t.Fatalf("the CLI sent\n%#v\nthe TUI sent\n%#v", cliSetup, tuiSetup)
			}
			if cliSetup.Args["key"] != modelTestKey || cliSetup.Args["provider_id"] != f.provider {
				t.Fatalf("%v", cliSetup.Args)
			}
			if shown, _ := dispatchermodel.ModelHints(f.provider); f.model == "2" && cliSetup.Args["model"] != shown[1].ID {
				t.Fatalf("picked %v, want %s", cliSetup.Args["model"], shown[1].ID)
			}
		})
	}
}

// modelDoctorServer adds the TUI's status read to the CLI's fake server.
type modelDoctorServer struct{ *fakeModelServer }

func (s modelDoctorServer) Execute(ctx context.Context, r bus.Request) (bus.Result, error) {
	if r.Operation == "dispatcher_model_doctor" {
		s.requests = append(s.requests, r)
		return bus.Result{Data: map[string]any{"seal_configured": true}}, nil
	}
	return s.fakeModelServer.Execute(ctx, r)
}

func setupRequest(t *testing.T, requests []bus.Request) bus.Request {
	t.Helper()
	for _, r := range requests {
		if r.Operation == "dispatcher_model_setup" {
			return r
		}
	}
	t.Fatal("no setup request")
	return bus.Request{}
}

func settleTUI(m tui.Model, cmd tea.Cmd) tui.Model {
	for i := 0; cmd != nil && i < 10; i++ {
		m, cmd = m.UpdateModel(cmd())
	}
	return m
}

func runes(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }
func enter() tea.KeyMsg         { return tea.KeyMsg{Type: tea.KeyEnter} }

// A rotation through the CLI and through the TUI's c sends the same request
// from the same stored setup.
func TestModelRotateCLIAndTUIAgree(t *testing.T) {
	config := Config{Workspace: "w", Project: "p", Session: "s"}
	modelTerminal(t, "", modelTestKey, true)
	cliServer := &rotateServer{fakeModelServer: fakeModelServer{outcomes: []string{"active"}}, active: storedSetup(dispatchermodel.DisclosureVersion)}
	if err := runModelRotate(context.Background(), cliServer, config, map[string]any{}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	tuiServer := &rotateServer{fakeModelServer: fakeModelServer{outcomes: []string{"active"}}, active: storedSetup(dispatchermodel.DisclosureVersion)}
	m, err := tui.New(tui.Config{Executor: rotateDoctorServer{tuiServer}, Mode: tui.ModeLocal, Workspace: config.Workspace, Project: config.Project, Session: config.Session})
	if err != nil {
		t.Fatal(err)
	}
	m, cmd := m.Show(tui.DispatcherModel)
	m = settleTUI(m, cmd)
	for _, k := range []tea.KeyMsg{runes("c"), runes(modelTestKey), enter()} {
		m, cmd = m.UpdateModel(k)
		m = settleTUI(m, cmd)
	}
	if cli, tuiReq := setupRequest(t, cliServer.requests), setupRequest(t, tuiServer.requests); !reflect.DeepEqual(cli, tuiReq) {
		t.Fatalf("the CLI sent\n%#v\nthe TUI sent\n%#v", cli, tuiReq)
	}
}

// rotateDoctorServer adds the TUI's status read to rotateServer.
type rotateDoctorServer struct{ *rotateServer }

func (s rotateDoctorServer) Execute(ctx context.Context, r bus.Request) (bus.Result, error) {
	if r.Operation == "dispatcher_model_doctor" {
		// The server lists the caller's own key; the TUI offers c only then.
		return bus.Result{Data: map[string]any{"seal_configured": true, "holders": []any{
			map[string]any{"principal": "owner@example.com", "you": true, "key": "active", "config_revision": int64(1)},
		}}}, nil
	}
	return s.rotateServer.Execute(ctx, r)
}
