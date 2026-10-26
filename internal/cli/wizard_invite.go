package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"clawbridge/internal/auth"
	"clawbridge/internal/bus"
	"clawbridge/internal/devicesign"
	"clawbridge/internal/remote"
	"github.com/pelletier/go-toml/v2"
)

// The wizard's "I have an invite code" path (live/AUTH-INVITE-DESIGN.md
// §11): the server's URL, the invited email and this device's name, then
// the code, unechoed, and the same join as `clawbridge join`.

// Adapter seams keep the server, the key store and the terminal out of tests.
var (
	wizardMethods = func(ctx context.Context, server string) ([]string, error) {
		discovered, err := remote.DiscoverOAuthClient(ctx, server, nil)
		return discovered.AuthMethods, err
	}
	wizardKeyStore   = auth.CheckNativeAvailability
	wizardInviteCode = func() (string, error) {
		raw, err := readHidden()
		return strings.TrimSpace(string(raw)), err
	}
	wizardDeviceJoin = JoinDevice
	wizardProjects   = func(ctx context.Context, path string) ([]string, error) {
		c, _, err := resolveConfig(ctx, Config{ClientConfig: path})
		if err != nil {
			return nil, err
		}
		executor, err := newAccountExecutor(c)
		if err != nil {
			return nil, err
		}
		result, err := executor.Execute(ctx, bus.Request{Operation: "list_projects", Workspace: c.Workspace, Args: map[string]any{}})
		if err != nil {
			return nil, err
		}
		data, _ := result.Data.(map[string]any)
		rows, _ := data["projects"].([]any)
		out := []string{}
		for _, row := range rows {
			if p, ok := row.(map[string]any); ok {
				if slug, _ := p["slug"].(string); slug != "" {
					out = append(out, slug)
				}
			}
		}
		return out, nil
	}
)

// wizardSignIn asks how to sign in when the server takes device keys. It
// reports whether the person has an invite code.
func wizardSignIn(ask func(string) (string, error), methods []string, stdout io.Writer) (bool, error) {
	device := slices.Contains(methods, "device")
	// A server older than device sign-in advertises nothing: Google.
	google := methods == nil || slices.Contains(methods, "google")
	switch {
	case device && google:
		fmt.Fprintln(stdout, "How do you sign in?\n  1. I have an invite code\n  2. Sign in with Google")
		answer, err := ask("Choose 1 or 2 [1]: ")
		if err != nil {
			return false, err
		}
		switch answer {
		case "", "1":
			return true, nil
		case "2":
			return false, nil
		}
		return false, errors.New("choose 1 or 2; run clawbridge onboard to start again")
	case device:
		fmt.Fprintln(stdout, "This server signs in with an invite code.")
		return true, nil
	case google:
		return false, nil
	}
	return false, errors.New("this server offers no sign-in this client supports; update clawbridge")
}

// wizardInvite joins with an invite. Nothing is written before the person
// says yes, and the code is read only after the prompts are closed, with
// echo off.
func wizardInvite(ctx context.Context, prompt *wizardPrompt, path, server string, stdout, stderr io.Writer) error {
	ask := prompt.ask
	fmt.Fprintln(stdout, "Join with an invite. ClawBridge makes a key for this device and keeps it on this machine.")
	email, err := ask("Email the invite was issued for: ")
	if err != nil {
		return err
	}
	if !strings.Contains(email, "@") {
		return errors.New("the invite's email is required; run clawbridge onboard to start again")
	}
	host, _ := os.Hostname()
	label, err := ask("Name for this device [" + wizardSafe(host) + "]: ")
	if err != nil {
		return err
	}
	store := devicesign.StoreKeyring
	if e := wizardKeyStore(ctx); e != nil {
		// Headless Linux, or a container: no system key store.
		fmt.Fprintf(stdout, "This machine has no system key store (%s).\nClawBridge can keep the device key in a plain file under ~/.clawbridge/device, readable only by you. But %s.\n", wizardSafe(e.Error()), devicesign.FileWarning)
		answer, err := ask("Keep the device key in a file? [y/N]: ")
		if err != nil {
			return err
		}
		if answer != "y" && answer != "yes" {
			fmt.Fprintln(stdout, "Canceled; nothing was changed. Unlock or start a key store, then run clawbridge onboard again.")
			return nil
		}
		store = devicesign.StoreFile
	}
	shown := label
	if shown == "" {
		shown = host
	}
	where := map[string]string{devicesign.StoreKeyring: "the system key store", devicesign.StoreFile: "a file (0600)"}[store]
	fmt.Fprintf(stdout, "Service: %s\nEmail: %s\nThis device: %s\nKey: %s\nConfig: %s\n", wizardSafe(server), wizardSafe(email), wizardSafe(shown), where, wizardSafe(path))
	answer, err := ask("Continue, then enter the invite code? [y/N]: ")
	if err != nil {
		return err
	}
	if answer != "y" && answer != "yes" {
		fmt.Fprintln(stdout, "Canceled; no key or configuration changed.")
		return nil
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = prompt.close(); err != nil {
		return err
	} // The code is read with echo off, from the terminal itself.
	fmt.Fprint(stdout, "Invite code (not shown as you type): ")
	code, err := wizardInviteCode()
	fmt.Fprintln(stdout)
	if err != nil || code == "" {
		return errors.New("no invite code was entered; nothing was changed")
	}
	joined, err := wizardDeviceJoin(ctx, JoinParams{Server: server, Email: email, Code: code, Label: label, KeyStore: store, ConfigPath: path})
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Joined workspace %s as %s. This device: %s %q, key in %s.\n", wizardSafe(joined.Workspace), wizardSafe(joined.Principal), joined.DeviceID, wizardSafe(joined.Label), where)
	if joined.Warning != "" {
		fmt.Fprintln(stdout, "Note: "+joined.Warning+".")
	}
	// The first signed request: it proves the key, and finds the projects.
	projects, err := wizardProjects(ctx, path)
	if err != nil {
		return fmt.Errorf("joined, but a request signed with the new key failed; run clawbridge doctor: %w", err)
	}
	project, err := wizardProject(ctx, projects, path, stdout)
	if err != nil {
		return err
	}
	if err = setProfileValue(path, "project", project); err != nil {
		return fmt.Errorf("joined, but the project could not be saved; set project = %q in %s: %w", project, path, err)
	}
	isolated := context.WithValue(ctx, guidedRouteKey{}, true)
	if err = runOnboarding(isolated, path, stdout, stderr); err != nil {
		return fmt.Errorf("joined; saved route preserved, but guided setup stopped: %w", err)
	}
	fmt.Fprintln(stdout, "Opening TUI…")
	if err = wizardTUI(isolated, Config{ClientConfig: path}, stdout, stderr); err != nil {
		return fmt.Errorf("joined; saved route preserved, but TUI startup failed: %w", err)
	}
	return nil
}

// wizardProject picks the project the profile works in: the only one, or
// the person's choice among them.
func wizardProject(ctx context.Context, projects []string, path string, stdout io.Writer) (string, error) {
	switch len(projects) {
	case 0:
		return "", fmt.Errorf("joined, but you have no project in this workspace yet; once an owner grants you one, set project = \"SLUG\" in %s", path)
	case 1:
		fmt.Fprintf(stdout, "Project: %s\n", wizardSafe(projects[0]))
		return projects[0], nil
	}
	prompt, err := newWizardPrompt(ctx, stdout, "guided terminal input cleanup failed; joined, restore the terminal and set project in "+path)
	if err != nil {
		return "", err
	}
	defer prompt.close()
	shown := make([]string, len(projects))
	for i, p := range projects {
		shown[i] = wizardSafe(p)
	}
	fmt.Fprintln(stdout, "Your projects: "+strings.Join(shown, ", "))
	answer, err := prompt.ask("Project to work in [" + shown[0] + "]: ")
	if err != nil {
		return "", err
	}
	if answer == "" {
		return projects[0], nil
	}
	if !slices.Contains(projects, answer) {
		return "", fmt.Errorf("%s is not one of your projects; set project = \"SLUG\" in %s", wizardSafe(answer), path)
	}
	if err = prompt.close(); err != nil {
		return "", err
	}
	return answer, nil
}

// setProfileValue sets one key of a profile, keeping every other.
func setProfileValue(path, key string, value any) error {
	values := map[string]any{}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err = toml.Unmarshal(b, &values); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}
	values[key] = value
	return writeConfigValues(path, values)
}
