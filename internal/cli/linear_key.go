package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"clawbridge/internal/linearconn"
	"clawbridge/internal/linearkey"
)

// keyStdin is where linear key set reads a piped key, and keyViewer reads
// the profile's own Linear user; tests replace them.
var (
	keyStdin  io.Reader = cloudStdin
	keyViewer           = linearViewer
)

// linearKeyEntry reports whether a command line is `linear key` or
// `linear view`, the only commands that may read the person's Linear API
// key. Every other process is marked key-forbidden at its start (K1).
func linearKeyEntry(args []string) bool {
	_, rest, err := extractGlobals(args)
	return err == nil && len(rest) >= 2 && rest[0] == "linear" && (rest[1] == "key" || rest[1] == "view")
}

func linearKeys(directory string) linearkey.Store {
	return linearkey.Store{Dir: filepath.Join(directory, "keys")}
}

// linearKey runs linear key set|status|remove for one profile.
func linearKey(ctx context.Context, flags Config, manager *linearconn.Manager, directory, action, profile string, stderr io.Writer) (any, error) {
	keys := linearKeys(directory)
	if action == "set" || action == "status" || action == "remove" {
		// Refused before anything is asked or read (K1, K2). An agent
		// doesn't remove the owner's key either; linear logout still does.
		if err := linearkey.Allowed(); err != nil {
			return nil, err
		}
	}
	switch action {
	case "remove":
		if err := keys.Remove(profile); err != nil {
			return nil, err
		}
		return map[string]any{"removed": true, "profile_id": profile}, nil
	case "status":
		out := map[string]any{"profile_id": profile, "saved": false}
		store, ok := keys.Recorded(profile)
		if !ok {
			return out, nil
		}
		out["saved"], out["store"] = true, store
		client, err := keys.Open(profile)
		if err != nil {
			return nil, err
		}
		if err = samePerson(ctx, manager, profile, client); err != nil {
			return nil, err
		}
		out["read"], out["same_person"] = "works", true
		out["write"] = "checked on the first view write"
		return out, nil
	case "set":
	default:
		return nil, errors.New("use linear key set|status|remove --profile ID")
	}
	store := flags.KeyStore
	if store == "" {
		store = linearkey.StoreKeyring
	}
	if store != linearkey.StoreKeyring && store != linearkey.StoreFile {
		return nil, errors.New("--key-store for linear key set is keyring or file")
	}
	terminal := stdinTerminal()
	if store == linearkey.StoreFile {
		// L2: a file is said, and confirmed at a terminal, before the key
		// is asked for.
		if !terminal {
			return nil, errors.New("--key-store file is confirmed at a terminal: " + linearkey.FileWarning)
		}
		fmt.Fprintf(stderr, "Warning: %s.\nSave it to a file? [y/N] ", linearkey.FileWarning)
		answer, _ := bufio.NewReader(keyStdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
			return nil, errors.New("nothing was saved")
		}
	}
	key, err := readLinearKey(terminal, stderr)
	if err != nil {
		return nil, err
	}
	client, err := linearkey.NewClient(key)
	if err != nil {
		return nil, err
	}
	if err = samePerson(ctx, manager, profile, client); err != nil {
		return nil, err
	}
	if err = keys.Set(profile, key, store); err != nil {
		return nil, err
	}
	return map[string]any{"saved": true, "profile_id": profile, "store": store, "read": "works", "same_person": true, "write": "checked on the first view write"}, nil
}

// readLinearKey reads the key at a hidden prompt, or one line of piped
// stdin; never from argv or the environment.
func readLinearKey(terminal bool, stderr io.Writer) (string, error) {
	if terminal {
		fmt.Fprint(stderr, "Linear personal API key (Read and Write, never Admin; hidden): ")
		raw, err := readHidden()
		fmt.Fprintln(stderr)
		if err != nil {
			return "", errors.New("no key was read; nothing was saved")
		}
		return strings.TrimSpace(string(raw)), nil
	}
	line, err := bufio.NewReader(io.LimitReader(keyStdin, 512)).ReadString('\n')
	if err != nil && line == "" {
		return "", errors.New("pipe the key on stdin, or run linear key set at a terminal; nothing was saved")
	}
	return strings.TrimSpace(line), nil
}

// samePerson checks the key acts as the Linear user behind the profile's
// own OAuth connection, so nobody saves someone else's key.
func samePerson(ctx context.Context, manager *linearconn.Manager, profile string, client *linearkey.Client) error {
	keyUser, err := client.Viewer(ctx)
	if err != nil {
		return err
	}
	viewer, err := keyViewer(ctx, manager, profile)
	if err != nil {
		return errors.New("could not read who this Linear profile is, to compare with the key; check clawbridge linear status --profile " + profile)
	}
	if !strings.EqualFold(viewer.ID, keyUser) {
		return errors.New("this API key belongs to another Linear user than the profile's connection; use your own key")
	}
	return nil
}
