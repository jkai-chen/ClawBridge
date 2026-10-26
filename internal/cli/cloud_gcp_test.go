package cli

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
)

type gcloudStub struct {
	instance, addresses, objects string
	err                          map[string]error
	calls                        [][]string
}

func stubCloud(t *testing.T, g *gcloudStub, ips []string, dialErr error) {
	t.Helper()
	a, b, c, d := cloudCLI, lookupHost, dialTCP, cloudNow
	t.Cleanup(func() { cloudCLI, lookupHost, dialTCP, cloudNow = a, b, c, d })
	cloudCLI = func(_ context.Context, argv []string) ([]byte, error) {
		g.calls = append(g.calls, argv)
		key := argv[1] + " " + argv[2]
		switch key {
		case "compute instances":
			return []byte(g.instance), g.err[key]
		case "compute addresses":
			return []byte(g.addresses), g.err[key]
		case "storage ls":
			return []byte(g.objects), g.err[key]
		}
		t.Fatalf("unexpected command %v", argv)
		return nil, nil
	}
	lookupHost = func(context.Context, string) ([]string, error) { return ips, nil }
	dialTCP = func(context.Context, string) error { return dialErr }
	cloudNow = func() time.Time { return time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC) }
}

const runningVM = `{"status":"RUNNING","networkInterfaces":[{"accessConfigs":[{"natIP":"203.0.113.7"}]}],"disks":[{"boot":true},{"boot":false}]}`
const staticIP = `[{"address":"203.0.113.7","status":"IN_USE"}]`

func infraProbe() *cloudProbe {
	return &cloudProbe{server: "https://bridge.example.test", target: cloudTarget{vm: "clawbridge-1", project: "example-project", zone: "europe-west1-b"}}
}

func TestCloudCheckInfra(t *testing.T) {
	mounted := allSet()
	mounted.mount = "/dev/sdb"
	for name, c := range map[string]struct {
		probe     *cloudProbe
		instance  string
		addresses string
		err       map[string]error
		ips       []string
		dialErr   error
		mount     string
		sshErr    error
		reason    string
	}{
		"ready":             {infraProbe(), runningVM, staticIP, nil, []string{"203.0.113.7"}, nil, "/dev/sdb", nil, ""},
		"also another IP":   {infraProbe(), runningVM, staticIP, nil, []string{"198.51.100.1", "203.0.113.7"}, nil, "/dev/sdb", nil, ""},
		"no vm":             {&cloudProbe{server: "https://bridge.example.test", target: cloudTarget{host: "host"}}, runningVM, staticIP, nil, nil, nil, "", nil, "needs_vm"},
		"bad zone":          {&cloudProbe{target: cloudTarget{vm: "clawbridge-1", project: "example-project", zone: "x"}}, runningVM, staticIP, nil, nil, nil, "", nil, "needs_vm"},
		"gcloud fails":      {infraProbe(), runningVM, staticIP, map[string]error{"compute instances": errors.New("x")}, nil, nil, "", nil, "vm_unreadable"},
		"not json":          {infraProbe(), "ERROR", staticIP, nil, nil, nil, "", nil, "vm_unreadable"},
		"stopped":           {infraProbe(), strings.Replace(runningVM, "RUNNING", "TERMINATED", 1), staticIP, nil, nil, nil, "", nil, "vm_not_running"},
		"no external IP":    {infraProbe(), `{"status":"RUNNING","networkInterfaces":[{}],"disks":[{"boot":false}]}`, staticIP, nil, nil, nil, "", nil, "no_external_ip"},
		"addresses fail":    {infraProbe(), runningVM, staticIP, map[string]error{"compute addresses": errors.New("x")}, nil, nil, "", nil, "vm_unreadable"},
		"ephemeral IP":      {infraProbe(), runningVM, `[]`, nil, nil, nil, "", nil, "ip_not_static"},
		"another address":   {infraProbe(), runningVM, `[{"address":"203.0.113.8","status":"IN_USE"}]`, nil, nil, nil, "", nil, "ip_not_static"},
		"reserved, unused":  {infraProbe(), runningVM, `[{"address":"203.0.113.7","status":"RESERVED"}]`, nil, nil, nil, "", nil, "ip_not_static"},
		"no server":         {&cloudProbe{target: infraProbe().target}, runningVM, staticIP, nil, nil, nil, "", nil, "no_server"},
		"DNS elsewhere":     {infraProbe(), runningVM, staticIP, nil, []string{"198.51.100.1"}, nil, "", nil, "dns_mismatch"},
		"443 closed":        {infraProbe(), runningVM, staticIP, nil, []string{"203.0.113.7"}, errors.New("refused"), "", nil, "port_closed"},
		"boot disk only":    {infraProbe(), strings.Replace(runningVM, `,{"boot":false}`, "", 1), staticIP, nil, []string{"203.0.113.7"}, nil, "", nil, "no_data_disk"},
		"ssh fails":         {infraProbe(), runningVM, staticIP, nil, []string{"203.0.113.7"}, nil, "/dev/sdb", errors.New("x"), "ssh_failed"},
		"not mounted":       {infraProbe(), runningVM, staticIP, nil, []string{"203.0.113.7"}, nil, "none", nil, "data_not_mounted"},
		"mount not printed": {infraProbe(), runningVM, staticIP, nil, []string{"203.0.113.7"}, nil, "", nil, "data_not_mounted"},
	} {
		g := &gcloudStub{instance: c.instance, addresses: c.addresses, err: c.err}
		stubCloud(t, g, c.ips, c.dialErr)
		f := allSet()
		f.mount = c.mount
		out := factsOutput(f)
		if c.mount != "" {
			out += "mount " + c.mount + "\n"
		}
		stubRemote(t, out, c.sshErr)
		got := checkInfra(context.Background(), c.probe)
		if got.Check != "infra" || got.Reason != c.reason || (c.reason == "") != (got.Status == "pass") || (c.reason != "" && got.Advice == "") {
			t.Fatalf("%s: %+v", name, got)
		}
		if name == "ready" {
			want := [][]string{
				{"gcloud", "compute", "instances", "describe", "clawbridge-1", "--project", "example-project", "--zone", "europe-west1-b", "--format=json"},
				{"gcloud", "compute", "addresses", "list", "--project", "example-project", "--filter=address=203.0.113.7", "--format=json"},
			}
			if !reflect.DeepEqual(g.calls, want) {
				t.Fatalf("calls %v", g.calls)
			}
		}
	}
}

func TestCloudCheckBackups(t *testing.T) {
	const b = "gs://example-backups/postgres/daily/"
	probe := func(bucket, project string) *cloudProbe {
		return &cloudProbe{bucket: bucket, target: cloudTarget{project: project}}
	}
	for name, c := range map[string]struct {
		probe   *cloudProbe
		objects string
		err     error
		reason  string
	}{
		"fresh":          {probe("example-backups", "example-project"), b + "20261025T020000Z.dump\n" + b + "20261025T020000Z.sha256\n" + b + "20261026T020000Z.dump\n" + b + "20261026T020000Z.sha256\n", nil, ""},
		"25 hours":       {probe("example-backups", "example-project"), b + "20261025T110000Z.dump\n" + b + "20261025T110000Z.sha256\n", nil, ""},
		"27 hours":       {probe("example-backups", "example-project"), b + "20261025T090000Z.dump\n" + b + "20261025T090000Z.sha256\n", nil, "backup_stale"},
		"newest partial": {probe("example-backups", "example-project"), b + "20261024T020000Z.dump\n" + b + "20261024T020000Z.sha256\n" + b + "20261026T020000Z.dump\n", nil, "backup_stale"},
		"only a dump":    {probe("example-backups", "example-project"), b + "20261026T020000Z.dump\n", nil, "no_backup"},
		"other bucket":   {probe("example-backups", "example-project"), "gs://other/postgres/daily/20261026T020000Z.dump\ngs://other/postgres/daily/20261026T020000Z.sha256\n", nil, "no_backup"},
		"empty":          {probe("example-backups", "example-project"), "", nil, "no_backup"},
		"unreadable":     {probe("example-backups", "example-project"), "", errors.New("x"), "bucket_unreadable"},
		"no bucket":      {probe("", "example-project"), "", nil, "needs_bucket"},
		"bad bucket":     {probe("gs://x", "example-project"), "", nil, "needs_bucket"},
		"no project":     {probe("example-backups", ""), "", nil, "needs_project"},
	} {
		g := &gcloudStub{objects: c.objects, err: map[string]error{"storage ls": c.err}}
		stubCloud(t, g, nil, nil)
		got := checkBackups(context.Background(), c.probe)
		if got.Check != "backups" || got.Reason != c.reason || (c.reason == "") != (got.Status == "pass") {
			t.Fatalf("%s: %+v", name, got)
		}
		if name == "fresh" && !reflect.DeepEqual(g.calls, [][]string{{"gcloud", "storage", "ls", b, "--project", "example-project"}}) {
			t.Fatalf("calls %v", g.calls)
		}
	}
}

// --project alone is accepted for backups; other partial targets are not.
func TestCloudCheckBackupsFlags(t *testing.T) {
	isolated(t)
	g := &gcloudStub{objects: "", err: map[string]error{}}
	stubCloud(t, g, nil, nil)
	got, _ := cloudCheck(t, "backups", "--bucket", "example-backups", "--project", "example-project")
	if len(got) != 1 || got[0].Reason != "no_backup" {
		t.Fatalf("%+v", got)
	}
	// With --project alone, server and linear-app run nothing over SSH.
	runs := stubRemote(t, "", errors.New("no ssh"))
	withVersion(t, "1.2.3")
	got, _ = cloudCheck(t, "server", "backups", "--server", healthServer(t, 200, `{"status":"ok","version":"1.2.3"}`), "--bucket", "example-backups", "--project", "example-project")
	if len(*runs) != 0 || got[0].Status != "pass" || got[0].Advice != serverUnchecked {
		t.Fatalf("%+v %+v", got, *runs)
	}
	if r := checkLinearApp(context.Background(), &cloudProbe{configErr: errors.New("x"), target: cloudTarget{project: "example-project"}}); r.Reason != "no_config" || len(*runs) != 0 {
		t.Fatalf("%+v", r)
	}
	if err := Run(context.Background(), []string{"cloud", "check", "backups", "--zone", "europe-west1-b"}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "reached with") {
		t.Fatal("a zone alone was accepted")
	}
}
