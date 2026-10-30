package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Google Cloud's route to the server's shell is gcloud compute ssh over
// IAP, as the user's own Google account. Without OS Login, gcloud adds the
// user's SSH key to the project's metadata, or the VM's when it blocks
// project-wide keys; --ssh-key-expire-after makes that key expire within
// the hour (gcpKeyExpiry). The first connection trusts the VM's host key as
// gcloud records it in ~/.ssh/google_compute_known_hosts: acceptable here,
// since the tunnel itself is IAP's, authenticated to Google, not an open
// port anyone could answer on.
//
// gcloud runs with --quiet, so no prompt reads stdin, which carries a
// secret or the one-time code. --quiet would also make a key without a
// passphrase; gcpKeyReady, in cloudTarget.transport (the one place every
// --vm command passes), refuses first unless the user already has gcloud's
// own key, from connecting once in their own terminal.

const gcpKeyExpiry = "--ssh-key-expire-after=1h"

// errIAPFirewall is IAP's "failed to connect to backend": no firewall rule
// lets IAP reach the VM's port 22.
var errIAPFirewall = errors.New("IAP could not reach the VM")

const iapFirewallAdvice = "IAP could not reach the VM's SSH port: allow tcp:22 from IAP's range 35.235.240.0/20 to the VM in a firewall rule (docs/operations/deployment.md), then run this again"

// gcpFailure names what a failed gcloud compute ssh run's stderr says,
// from fixed phrases only, as sshFailure does for ssh's own.
func gcpFailure(err error, stderr []byte) error {
	s := string(stderr)
	if strings.Contains(s, "failed to connect to backend") || strings.Contains(s, "[4003:") {
		return errIAPFirewall
	}
	return sshFailure(err, stderr)
}

// osLoginNote is the one-line warning when OS Login is off for the VM,
// saying where gcloud may put the user's key; "" when it is on. The VM's
// own enable-oslogin wins over the project's. A VM it can't describe gets
// no note: the run itself then says what is wrong.
func osLoginNote(ctx context.Context, t cloudTarget) string {
	vm, err := cloudCLI(ctx, []string{"gcloud", "compute", "instances", "describe", t.vm, "--project", t.project, "--zone", t.zone, "--format=json(metadata.items)"})
	if err != nil {
		return ""
	}
	project, err := cloudCLI(ctx, []string{"gcloud", "compute", "project-info", "describe", "--project", t.project, "--format=json(commonInstanceMetadata.items)"})
	if err != nil {
		return ""
	}
	own, projectWide := metadataItems(vm, "metadata"), metadataItems(project, "commonInstanceMetadata")
	on, set := own["enable-oslogin"]
	if !set {
		on = projectWide["enable-oslogin"]
	}
	if strings.EqualFold(on, "true") {
		return ""
	}
	scope := "the project's metadata (every VM in " + t.project + " trusts it)"
	if strings.EqualFold(own["block-project-ssh-keys"], "true") {
		scope = "the VM's own metadata"
	}
	return "warning: OS Login is off for " + t.vm + ", so gcloud may add your SSH key to " + scope + "; it expires within the hour (" + gcpKeyExpiry + "). Prefer OS Login: enable-oslogin=TRUE"
}

// gcpKey is gcloud's own SSH key; tests replace it.
var gcpKey = func() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".ssh", "google_compute_engine"), nil
}

// gcpKeyReady refuses before anything reaches gcloud's stdin unless the
// user has gcloud's SSH key already.
func gcpKeyReady(t cloudTarget) error {
	path, err := gcpKey()
	if err == nil {
		_, err = os.Stat(path)
	}
	if err != nil {
		return secretAdvice("gcloud has no SSH key of yours yet (~/.ssh/google_compute_engine): connect once in your own terminal with gcloud compute ssh " +
			t.vm + " --project " + t.project + " --zone " + t.zone + " --tunnel-through-iap, then run this again")
	}
	return nil
}

// metadataItems reads gcloud's {KEY: {items: [{key, value}]}} answer.
func metadataItems(raw []byte, key string) map[string]string {
	var doc map[string]struct {
		Items []struct{ Key, Value string } `json:"items"`
	}
	out := map[string]string{}
	if json.Unmarshal(raw, &doc) != nil {
		return out
	}
	for _, it := range doc[key].Items {
		out[it.Key] = it.Value
	}
	return out
}
