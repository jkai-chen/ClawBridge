package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"slices"
	"sort"
	"strings"
)

// cloud env init (ANY-VM-PLAN-REVIEW M2): the server's generated secrets are
// made on the server by a fixed script, never by a shell line an agent
// composes. It creates the env file 0600 root if absent, adds each named
// key that has no line yet, keeps every existing value, and prints key
// names only.

const cloudEnvUsage = `clawbridge cloud env init [KEY...] (--ssh HOST | --vm NAME --project P --zone Z |
                                     --instance ID --region R [--profile P] [--account N])`

// generatedKeys are the keys env init makes, each with its generator:
// POSTGRES_PASSWORD always (hex, safe in the Postgres URL); the others only
// when named, since the Linear app's token key without its client ID and
// secret stops the server starting.
var generatedKeys = map[string]string{
	"POSTGRES_PASSWORD":               `od -An -vtx1 -N32 /dev/urandom | tr -d " \n"`,
	"CLAWBRIDGE_LINEAR_APP_TOKEN_KEY": `head -c 32 /dev/urandom | base64 | tr -d "\n"`,
	"CLAWBRIDGE_MODEL_KEY_SEAL_KEY":   `head -c 32 /dev/urandom | base64 | tr -d "\n"`,
}

// generatedLength is each generator's output length: 32 bytes as hex, or
// as padded base64.
var generatedLength = map[string]string{
	"POSTGRES_PASSWORD":               "64",
	"CLAWBRIDGE_LINEAR_APP_TOKEN_KEY": "44",
	"CLAWBRIDGE_MODEL_KEY_SEAL_KEY":   "44",
}

// envInitScript writes the file once, whole: a copy with the new lines,
// installed 0600 root over the old. A key with an empty line fails it
// (exit 5) before anything is written; a link, or a path that isn't a
// regular file, exit 4: root's chown and mv never follow a link. Values
// never reach the output. It has no single quote.
func envInitScript(keys []string, path string) string {
	lines := []string{
		`set -eu`,
		`umask 077`,
		`f=` + path,
		`if [ -L "$f" ] || { [ -e "$f" ] && [ ! -f "$f" ]; }; then exit 4; fi`,
		`[ -d "$(dirname "$f")" ] || install -d -o root -g root -m 0755 "$(dirname "$f")"`,
		`t=$(mktemp "$f.XXXXXX")`,
		`trap "rm -f \"\$t\"" EXIT`,
		`if [ -f "$f" ]; then cat "$f" > "$t"; fi`,
		// A last line with no newline would run into the first added key.
		`if [ -s "$t" ] && [ -n "$(tail -c 1 "$t")" ]; then echo >> "$t"; fi`,
	}
	for _, k := range keys {
		lines = append(lines, `if grep -q "^`+k+`=$" "$t"; then echo "empty `+k+`"; exit 5; fi`)
	}
	for _, k := range keys {
		lines = append(lines,
			`if grep -q "^`+k+`=" "$t"; then echo "kept `+k+`"; else`,
			`  v=$(`+generatedKeys[k]+`)`,
			`  [ ${#v} -eq `+generatedLength[k]+` ]`,
			`  printf "%s=%s\n" `+k+` "$v" >> "$t"`,
			`  echo "added `+k+`"`,
			`fi`)
	}
	return strings.Join(append(lines,
		`chown 0:0 "$t"`,
		`chmod 0600 "$t"`,
		`mv -f "$t" "$f"`,
	), "\n")
}

func runCloudEnv(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] != "init" {
		return errors.New("usage: " + cloudEnvUsage)
	}
	var target cloudTarget
	f := flag.NewFlagSet("cloud env init", flag.ContinueOnError)
	f.SetOutput(stderr)
	bindTarget(f, &target)
	ordered, err := interspersedFlags(f, args[1:])
	if err == nil {
		err = f.Parse(ordered)
	}
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	keys := []string{"POSTGRES_PASSWORD"}
	for _, k := range f.Args() {
		if generatedKeys[k] == "" {
			names := make([]string, 0, len(generatedKeys))
			for n := range generatedKeys {
				names = append(names, n)
			}
			sort.Strings(names)
			return errors.New("env init makes only these keys: " + strings.Join(names, ", "))
		}
		if !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	tr, err := target.transport()
	if err != nil {
		return err
	}
	out, err := tr.run(ctx, envInitScript(keys, serverEnvFile), stderr)
	if err != nil {
		advice := tr.describe()
		switch {
		case errors.Is(err, errHostKey):
			advice = hostKeyAdvice
		case errors.Is(err, errSudoPassword):
			advice = sudoAdvice
		case exitCode(err) == 4:
			advice = serverEnvFile + " is a link or not a regular file; env init writes only a regular file"
		case exitCode(err) == 5:
			advice = serverEnvFile + " has an empty line for " + emptyKey(out, keys) + "; remove that line, then run env init again"
		}
		return errors.New("env init did not finish; " + advice + " (nothing was printed, and the env file is as it was)")
	}
	added, kept, ok := parseEnvInit(out, keys)
	if !ok {
		return errors.New("env init: the server's answer did not name every key; check the env file with cloud check server")
	}
	return json.NewEncoder(stdout).Encode(map[string]any{"source": "cloud_env", "data": map[string]any{"added": added, "kept": kept}})
}

// parseEnvInit reads the script's answer strictly: each asked key exactly
// once, added or kept, and no other line.
func parseEnvInit(out []byte, keys []string) (added, kept []string, ok bool) {
	added, kept = []string{}, []string{}
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		verb, k, found := strings.Cut(line, " ")
		if !found || !slices.Contains(keys, k) || seen[k] {
			return nil, nil, false
		}
		seen[k] = true
		switch verb {
		case "added":
			added = append(added, k)
		case "kept":
			kept = append(kept, k)
		default:
			return nil, nil, false
		}
	}
	return added, kept, len(seen) == len(keys)
}

// emptyKey names the key the script found empty, from the asked keys only.
func emptyKey(out []byte, keys []string) string {
	for _, k := range keys {
		if strings.Contains(string(out), "empty "+k+"\n") {
			return k
		}
	}
	return "a generated key"
}
