package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"clawbridge/internal/auth"
	"clawbridge/internal/bus"
	"clawbridge/internal/clirender"
	"clawbridge/internal/devicekey"
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
		answer, err := askUntil(ask, stdout, "Choose 1 or 2 [1]: ", func(a string) (string, error) {
			if v, ok := pick(a, "1", joinChoices); ok {
				return v, nil
			}
			return "", errors.New("choose 1 or 2")
		})
		return answer == "invite", err
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
	email, err := askUntil(ask, stdout, "Email the invite was issued for (leave empty if you have an invite link): ", func(a string) (string, error) {
		if a != "" && !strings.Contains(a, "@") {
			return "", errors.New("that isn't an email address; leave it empty if you have an invite link")
		}
		return a, nil
	})
	if err != nil {
		return err
	}
	host, _ := os.Hostname()
	label, err := ask("Name for this device [" + clirender.Safe(host) + "]: ")
	if err != nil {
		return err
	}
	store := devicesign.StoreKeyring
	if e := wizardKeyStore(ctx); e != nil {
		// Headless Linux, or a container: no system key store.
		fmt.Fprintf(stdout, "This machine has no system key store (%s).\nClawBridge can keep the device key in a plain file under ~/.clawbridge/device, readable only by you. But %s.\n", clirender.Safe(e.Error()), devicesign.FileWarning)
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
	shownEmail := email
	if shownEmail == "" {
		shownEmail = "(from the invite link)"
	}
	fmt.Fprintf(stdout, "Service: %s\nEmail: %s\nThis device: %s\nKey: %s\nConfig: %s\n", clirender.Safe(server), clirender.Safe(shownEmail), clirender.Safe(shown), where, clirender.Safe(path))
	answer, err := ask("Continue, then paste the invite link or code? [y/N]: ")
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
	fmt.Fprint(stdout, "Invite link or code (not shown as you type): ")
	code, err := wizardInviteCode()
	fmt.Fprintln(stdout)
	if err != nil {
		return fmt.Errorf("the invite code could not be read; nothing was changed: %w", err)
	}
	if code == "" {
		return errors.New("no invite code was entered; nothing was changed")
	}
	// The same field takes the whole link (§17.1): it must be for this
	// server, and for the email typed, if one was.
	if link, e := devicekey.ParseLink(code); e == nil {
		if link.Origin != strings.TrimRight(strings.ToLower(server), "/") {
			return fmt.Errorf("that invite link is for %s, not %s; nothing was changed", link.Origin, clirender.Safe(server))
		}
		if email != "" && !strings.EqualFold(email, link.Email) {
			return fmt.Errorf("that invite link is for %s, not %s; nothing was changed", clirender.Safe(link.Email), clirender.Safe(email))
		}
		email, code = link.Email, link.Code
	} else if email == "" {
		return errors.New("without an email, paste the whole invite link; nothing was changed")
	}
	joined, err := wizardDeviceJoin(ctx, JoinParams{Server: server, Email: email, Code: code, Label: label, KeyStore: store, ConfigPath: path})
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Joined workspace %s as %s. This device: %s %q, key in %s.\n", clirender.Safe(joined.Workspace), clirender.Safe(joined.Principal), joined.DeviceID, clirender.Safe(joined.Label), where)
	if joined.Warning != "" {
		fmt.Fprintln(stdout, "Note: "+joined.Warning+".")
	}
	// The first signed request: it proves the key, and finds the projects.
	projects, err := wizardProjects(ctx, path)
	if err != nil {
		return fmt.Errorf("joined, but a request signed with the new key failed; run clawbridge doctor: %w", err)
	}
	project, err := wizardProject(ctx, projects, path, stdout)
	switch {
	case err != nil:
		return err
	case project == "":
		// No project yet: the profile is saved, and the TUI waits for one.
		return openNow(context.WithValue(ctx, guidedRouteKey{}, true), path, stdout, stderr)
	}
	return finishJoin(ctx, path, project, stdout, stderr)
}

// waitingForProject is what a member with no project yet is told (U47):
// the profile is saved, and the TUI waits for a project (PR D).
const waitingForProject = `
You have joined, but you have no project in this workspace yet. Ask the workspace owner to grant you
one. ClawBridge opens on a waiting screen that checks for it, and switches to the project once it is there.
`

// wizardProject picks the project the profile works in: the only one, or
// the person's choice from a numbered list. With none it says what it is
// waiting for, and returns "".
func wizardProject(ctx context.Context, projects []string, path string, stdout io.Writer) (_ string, retErr error) {
	switch len(projects) {
	case 0:
		_, err := io.WriteString(stdout, waitingForProject)
		return "", err
	case 1:
		fmt.Fprintf(stdout, "Project: %s\n", clirender.Safe(projects[0]))
		return projects[0], nil
	}
	prompt, err := newWizardPrompt(ctx, stdout, "guided terminal input cleanup failed; joined, restore the terminal and run clawbridge to choose the project")
	if err != nil {
		return "", err
	}
	defer func() {
		if e := prompt.close(); e != nil && !errors.Is(retErr, e) {
			retErr = errors.Join(retErr, e)
		}
	}()
	fmt.Fprintln(stdout, "Your projects:")
	for i, p := range projects {
		fmt.Fprintf(stdout, "  %d. %s\n", i+1, clirender.Safe(p))
	}
	return askUntil(prompt.ask, stdout, "Choose a project [1]: ", func(a string) (string, error) {
		if a == "" {
			a = "1"
		}
		if n, e := strconv.Atoi(a); e == nil && n >= 1 && n <= len(projects) {
			return projects[n-1], nil
		}
		if slices.Contains(projects, a) {
			return a, nil
		}
		return "", fmt.Errorf("choose a number from 1 to %d", len(projects))
	})
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
