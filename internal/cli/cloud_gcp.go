package cli

import (
	"context"
	"encoding/json"
	"net"
	"regexp"
	"sort"
	"strings"
	"time"
)

// The checks that read the user's cloud with their own gcloud credentials
// (live/ONBOARDING-DESIGN.md §4): infra and backups. Every command names the
// project explicitly and only reads.

// cloudCLI runs the user's own cloud CLI; tests replace it.
var cloudCLI = func(ctx context.Context, argv []string) ([]byte, error) {
	return remoteOutput(ctx, argv, nil)
}

var (
	bucketPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,61}[a-z0-9]$`)
	backupObject  = regexp.MustCompile(`/postgres/daily/([0-9]{8}T[0-9]{6}Z)\.(dump|sha256)$`)
	dialTCP       = func(ctx context.Context, address string) error {
		conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", address)
		if err == nil {
			conn.Close()
		}
		return err
	}
	lookupHost = net.DefaultResolver.LookupHost
	cloudNow   = time.Now
)

// maxBackupAge is how old the newest complete backup may be: the daily
// timer, plus slack for one late run.
const maxBackupAge = 26 * time.Hour

const gcloudAdvice = "gcloud could not read it; check gcloud auth and that --project is the server's project"

type gcpInstance struct {
	Status            string `json:"status"`
	NetworkInterfaces []struct {
		AccessConfigs []struct {
			NatIP string `json:"natIP"`
		} `json:"accessConfigs"`
	} `json:"networkInterfaces"`
	Disks []struct {
		Boot bool `json:"boot"`
	} `json:"disks"`
}

// checkInfra: the VM runs, its external IP is a reserved static address,
// the server's domain resolves to it, a data disk is attached and mounted at
// the data path, and TCP 443 answers.
func checkInfra(ctx context.Context, p *cloudProbe) cloudResult {
	t := p.target
	if t.onAWS() {
		return awsInfra(ctx, p)
	}
	if t.vm == "" {
		return failed("infra", "needs_vm", "pass --vm NAME --project P --zone Z")
	}
	if _, err := t.transport(); err != nil {
		return failed("infra", "needs_vm", "pass --vm NAME --project P --zone Z")
	}
	out, err := cloudCLI(ctx, []string{"gcloud", "compute", "instances", "describe", t.vm, "--project", t.project, "--zone", t.zone, "--format=json"})
	var vm gcpInstance
	if err != nil || json.Unmarshal(out, &vm) != nil {
		return failed("infra", "vm_unreadable", gcloudAdvice)
	}
	if vm.Status != "RUNNING" {
		return failed("infra", "vm_not_running", "the VM is "+strings.ToLower(vm.Status)+"; start it")
	}
	ip := ""
	if len(vm.NetworkInterfaces) > 0 && len(vm.NetworkInterfaces[0].AccessConfigs) > 0 {
		ip = vm.NetworkInterfaces[0].AccessConfigs[0].NatIP
	}
	if net.ParseIP(ip) == nil {
		return failed("infra", "no_external_ip", "attach the reserved static IP to the VM")
	}
	out, err = cloudCLI(ctx, []string{"gcloud", "compute", "addresses", "list", "--project", t.project, "--filter=address=" + ip, "--format=json"})
	var addresses []struct {
		Address string `json:"address"`
		Status  string `json:"status"`
	}
	if err != nil || json.Unmarshal(out, &addresses) != nil {
		return failed("infra", "vm_unreadable", gcloudAdvice)
	}
	if len(addresses) != 1 || addresses[0].Address != ip || addresses[0].Status != "IN_USE" {
		return failed("infra", "ip_not_static", "the VM's IP "+ip+" is ephemeral; reserve it as a static address")
	}
	u, ok := serverURL(p.server)
	if !ok {
		return failed("infra", "no_server", "pass --server https://YOUR-DOMAIN or run setup --cloud")
	}
	domain := u.Hostname()
	// A failed lookup resolves to nothing.
	addrs, _ := lookupHost(ctx, domain)
	resolves := false
	for _, a := range addrs {
		resolves = resolves || net.ParseIP(a).Equal(net.ParseIP(ip))
	}
	if !resolves {
		return failed("infra", "dns_mismatch", domain+" must have an A record for "+ip)
	}
	data := 0
	for _, d := range vm.Disks {
		if !d.Boot {
			data++
		}
	}
	if data == 0 {
		return failed("infra", "no_data_disk", "attach a separate persistent disk for the database")
	}
	f, err := p.facts(ctx)
	if err != nil {
		return p.remoteFailure("infra", err)
	}
	if f.mount == "" || f.mount == "none" {
		return failed("infra", "data_not_mounted", "mount the data disk at "+dataMount+" (and in /etc/fstab)")
	}
	// Last: nothing answers on 443 until the server is installed, so a
	// fresh VM fails here and nowhere earlier.
	if dialTCP(ctx, net.JoinHostPort(domain, "443")) != nil {
		return failed("infra", "port_closed", "TCP 443 does not answer; open it in the firewall (443 only) and start the server")
	}
	return cloudResult{Check: "infra", Status: "pass"}
}

// checkBackups: the newest complete dump and checksum pair in the bucket is
// under 26 hours old.
func checkBackups(ctx context.Context, p *cloudProbe) cloudResult {
	if p.target.onAWS() {
		return awsBackups(ctx, p)
	}
	if p.bucket == "" || !bucketPattern.MatchString(p.bucket) {
		return failed("backups", "needs_bucket", "pass --bucket NAME (the CLAWBRIDGE_BACKUP_BUCKET)")
	}
	if !projectPattern.MatchString(p.target.project) {
		return failed("backups", "needs_project", "pass --project P, the bucket's Google Cloud project")
	}
	out, err := cloudCLI(ctx, []string{"gcloud", "storage", "ls", "gs://" + p.bucket + "/postgres/daily/", "--project", p.target.project})
	if err != nil {
		return failed("backups", "bucket_unreadable", gcloudAdvice)
	}
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); strings.HasPrefix(line, "gs://"+p.bucket+"/") {
			names = append(names, line)
		}
	}
	return newestBackup(names)
}

// newestBackup: of the objects named, the newest complete dump and checksum
// pair is under 26 hours old. Each name ends in /postgres/daily/STAMP.KIND.
func newestBackup(names []string) cloudResult {
	kinds := map[string]map[string]bool{}
	for _, name := range names {
		if m := backupObject.FindStringSubmatch(name); m != nil {
			if kinds[m[1]] == nil {
				kinds[m[1]] = map[string]bool{}
			}
			kinds[m[1]][m[2]] = true
		}
	}
	var complete []string
	for stamp, k := range kinds {
		if k["dump"] && k["sha256"] {
			complete = append(complete, stamp)
		}
	}
	if len(complete) == 0 {
		return failed("backups", "no_backup", "no complete backup yet; run sudo systemctl start clawbridge-postgres-backup.service on the VM")
	}
	sort.Strings(complete)
	newest, err := time.Parse("20060102T150405Z", complete[len(complete)-1])
	if err != nil {
		return failed("backups", "no_backup", "no complete backup yet; run sudo systemctl start clawbridge-postgres-backup.service on the VM")
	}
	if age := cloudNow().Sub(newest); age > maxBackupAge {
		return failed("backups", "backup_stale", "the newest backup is "+age.Truncate(time.Hour).String()+" old; check the backup timer on the VM")
	}
	return cloudResult{Check: "backups", Status: "pass"}
}
