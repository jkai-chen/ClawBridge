package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const ec2ID = "i-0123456789abcdef0"

// awsStub answers the aws CLI by service and operation ("ec2 describe-instances").
type awsStub struct {
	out   map[string]string
	err   map[string]error
	calls [][]string
	stdin map[string][]byte
	polls []string // successive get-command-invocation answers
}

func stubAWS(t *testing.T, a *awsStub, ips []string, dialErr error) {
	t.Helper()
	old, b, c, d, w := awsCLI, lookupHost, dialTCP, cloudNow, ssmWait
	t.Cleanup(func() { awsCLI, lookupHost, dialTCP, cloudNow, ssmWait = old, b, c, d, w })
	ssmWait = 3 * time.Second
	if a.stdin == nil {
		a.stdin = map[string][]byte{}
	}
	awsCLI = func(_ context.Context, argv []string, stdin []byte) ([]byte, error) {
		a.calls = append(a.calls, argv)
		key := argv[1] + " " + argv[2]
		if stdin != nil {
			a.stdin[key] = stdin
		}
		if key == "ssm get-command-invocation" && len(a.polls) > 0 {
			next := a.polls[0]
			a.polls = a.polls[1:]
			return []byte(next), nil
		}
		if out, ok := a.out[key]; ok || a.err[key] != nil {
			return []byte(out), a.err[key]
		}
		t.Fatalf("unexpected aws command %v", argv)
		return nil, nil
	}
	lookupHost = func(context.Context, string) ([]string, error) { return ips, nil }
	dialTCP = func(context.Context, string) error { return dialErr }
	cloudNow = func() time.Time { return time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC) }
}

const (
	ec2Running = `{"Reservations":[{"Instances":[{"State":{"Name":"running"},"PublicIpAddress":"203.0.113.7","RootDeviceName":"/dev/xvda",
		"BlockDeviceMappings":[{"DeviceName":"/dev/xvda","Ebs":{"VolumeId":"vol-root"}},{"DeviceName":"/dev/sdf","Ebs":{"VolumeId":"vol-data"}}],
		"SecurityGroups":[{"GroupId":"sg-0123"}]}]}]}`
	eipAttached  = `{"Addresses":[{"PublicIp":"203.0.113.7","InstanceId":"` + ec2ID + `"}]}`
	dataVolume   = `{"Volumes":[{"VolumeId":"vol-data","Encrypted":true}]}`
	httpsOnly    = `{"SecurityGroups":[{"IpPermissions":[{"IpProtocol":"tcp","FromPort":443,"ToPort":443,"IpRanges":[{"CidrIp":"0.0.0.0/0"}],"Ipv6Ranges":[{"CidrIpv6":"::/0"}]}]}]}`
	ssmOnline    = `{"InstanceInformationList":[{"PingStatus":"Online"}]}`
	commandSent  = `{"Command":{"CommandId":"cmd-1"}}`
	mountedFacts = `{"Status":"Success","StandardOutputContent":"mount /dev/nvme1n1\n"}`
)

func healthyAWS() *awsStub {
	return &awsStub{out: map[string]string{
		"ec2 describe-instances":            ec2Running,
		"ec2 describe-addresses":            eipAttached,
		"ec2 describe-volumes":              dataVolume,
		"ec2 describe-security-groups":      httpsOnly,
		"ssm describe-instance-information": ssmOnline,
		"ssm describe-parameters":           `{"Parameters":[]}`,
		"ssm send-command":                  commandSent,
		"ssm get-command-invocation":        mountedFacts,
		"sts get-caller-identity":           `{"Account":"123456789012"}`,
	}}
}

func awsProbe() *cloudProbe {
	return &cloudProbe{server: "https://bridge.example.test", target: cloudTarget{instance: ec2ID, region: "us-east-1"}}
}

func TestCloudRoutesAreExclusive(t *testing.T) {
	ssh := cloudTarget{host: "admin@bridge.example.test"}
	vm := cloudTarget{vm: "clawbridge-1", project: "example-project", zone: "europe-west1-b"}
	ec2 := cloudTarget{instance: ec2ID, region: "us-east-1"}
	for name, target := range map[string]cloudTarget{
		"ssh+vm":       {host: ssh.host, vm: vm.vm, project: vm.project, zone: vm.zone},
		"ssh+instance": {host: ssh.host, instance: ec2.instance, region: ec2.region},
		"vm+instance":  {vm: vm.vm, project: vm.project, zone: vm.zone, instance: ec2.instance, region: ec2.region},
	} {
		if _, err := target.transport(); err == nil || !strings.Contains(err.Error(), "pass one route") {
			t.Fatalf("%s: %v", name, err)
		}
	}
	for name, target := range map[string]cloudTarget{
		"bad instance":       {instance: "i-XYZ", region: "us-east-1"},
		"instance+project":   {instance: ec2ID, region: "us-east-1", project: "example-project"},
		"region without one": {host: ssh.host, region: "us-east-1"},
		"profile with vm":    {vm: vm.vm, project: vm.project, zone: vm.zone, profile: "p"},
	} {
		if _, err := target.transport(); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	if tr, err := ec2.transport(); err != nil {
		t.Fatal(err)
	} else if _, ok := tr.(ssmTransport); !ok {
		t.Fatalf("an instance is reached over SSM, got %T", tr)
	}
	// The CLI says the same.
	err := Run(context.Background(), []string{"cloud", "check", "server", "--ssh", ssh.host, "--instance", ec2ID, "--region", "us-east-1"}, &strings.Builder{}, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "pass one route") {
		t.Fatalf("cloud check with two routes: %v", err)
	}
}

func TestSSMRunsTheFixedScriptFromStdin(t *testing.T) {
	a := healthyAWS()
	a.polls = []string{`{"Status":"Pending"}`, `{"Status":"InProgress"}`, mountedFacts}
	stubAWS(t, a, nil, nil)
	tr, err := cloudTarget{instance: ec2ID, region: "us-east-1"}.transport()
	if err != nil {
		t.Fatal(err)
	}
	out, err := tr.run(context.Background(), factsScript(), nil)
	if err != nil || string(out) != "mount /dev/nvme1n1\n" {
		t.Fatalf("run = %q, %v", out, err)
	}
	var request struct {
		InstanceIds  []string
		DocumentName string
		Parameters   map[string][]string
	}
	if err := json.Unmarshal(a.stdin["ssm send-command"], &request); err != nil {
		t.Fatal(err)
	}
	if request.DocumentName != "AWS-RunShellScript" || len(request.InstanceIds) != 1 || request.InstanceIds[0] != ec2ID ||
		len(request.Parameters["commands"]) != 1 || request.Parameters["commands"][0] != `sh -c '`+factsScript()+`'` ||
		len(request.Parameters["executionTimeout"]) != 1 || request.Parameters["executionTimeout"][0] != "3" {
		t.Fatalf("send-command request: %+v", request)
	}
	for _, argv := range a.calls {
		joined := strings.Join(argv, " ")
		if strings.Contains(joined, "findmnt") {
			t.Fatalf("the script leaked into argv: %v", argv)
		}
		if !strings.HasSuffix(joined, "--profile "+defaultAWSProfile+" --region us-east-1 --output json") {
			t.Fatalf("profile and region must be explicit: %v", argv)
		}
	}
	for _, status := range []string{"Failed", "TimedOut", "Cancelled"} {
		a.polls = []string{`{"Status":"` + status + `"}`}
		if _, err := tr.run(context.Background(), factsScript(), nil); err == nil {
			t.Fatalf("%s accepted", status)
		}
	}
	ssmWait = 0
	a.polls = nil
	a.out["ssm get-command-invocation"] = `{"Status":"InProgress"}`
	if _, err := tr.run(context.Background(), factsScript(), nil); err == nil || !strings.Contains(err.Error(), "in time") {
		t.Fatalf("a script that never finishes: %v", err)
	}
}

func TestCloudCheckInfraOnAWS(t *testing.T) {
	cases := map[string]struct {
		edit  func(*awsStub)
		ips   []string
		dial  error
		probe func(*cloudProbe)
		want  string
	}{
		"pass":          {want: ""},
		"bad instance":  {probe: func(p *cloudProbe) { p.target.instance = "i-bad" }, want: "needs_instance"},
		"no region":     {probe: func(p *cloudProbe) { p.target.region = "" }, want: "needs_instance"},
		"other account": {probe: func(p *cloudProbe) { p.target.account = "999999999999" }, want: "cloud_auth"},
		"unreadable":    {edit: func(a *awsStub) { a.err = map[string]error{"ec2 describe-instances": errors.New("x")} }, want: "vm_unreadable"},
		"stopped": {edit: func(a *awsStub) {
			a.out["ec2 describe-instances"] = strings.Replace(ec2Running, "running", "stopped", 1)
		}, want: "vm_not_running"},
		"no public ip": {edit: func(a *awsStub) { a.out["ec2 describe-instances"] = strings.Replace(ec2Running, "203.0.113.7", "", 1) }, want: "no_external_ip"},
		"not elastic":  {edit: func(a *awsStub) { a.out["ec2 describe-addresses"] = `{"Addresses":[]}` }, want: "ip_not_static"},
		"elastic, other": {edit: func(a *awsStub) {
			a.out["ec2 describe-addresses"] = strings.Replace(eipAttached, ec2ID, "i-0000000000000000f", 1)
		}, want: "ip_not_static"},
		"dns": {ips: []string{"198.51.100.1"}, want: "dns_mismatch"},
		"no data volume": {edit: func(a *awsStub) {
			a.out["ec2 describe-instances"] = strings.Replace(ec2Running, `"/dev/sdf"`, `"/dev/xvda"`, 1)
		}, want: "no_data_disk"},
		"unencrypted": {edit: func(a *awsStub) { a.out["ec2 describe-volumes"] = strings.Replace(dataVolume, "true", "false", 1) }, want: "data_disk_unencrypted"},
		"ssh open": {edit: func(a *awsStub) {
			a.out["ec2 describe-security-groups"] = strings.Replace(httpsOnly, `"FromPort":443,"ToPort":443`, `"FromPort":22,"ToPort":22`, 1)
		}, want: "ssh_port_open"},
		"all traffic": {edit: func(a *awsStub) {
			a.out["ec2 describe-security-groups"] = strings.Replace(httpsOnly, `"tcp"`, `"-1"`, 1)
		}, want: "ssh_port_open"},
		"port 80 too": {edit: func(a *awsStub) {
			a.out["ec2 describe-security-groups"] = strings.Replace(httpsOnly, `"FromPort":443,"ToPort":443`, `"FromPort":80,"ToPort":80`, 1)
		}, want: "extra_ingress"},
		"ssh from a /32": {edit: func(a *awsStub) {
			a.out["ec2 describe-security-groups"] = strings.NewReplacer(`"FromPort":443,"ToPort":443`, `"FromPort":22,"ToPort":22`, "0.0.0.0/0", "192.0.2.10/32", "::/0", "2001:db8::1/128").Replace(httpsOnly)
		}, want: "extra_ingress"},
		"secret left": {edit: func(a *awsStub) {
			a.out["ssm describe-parameters"] = `{"Parameters":[{"Name":"/clawbridge/` + ec2ID + `/CLAWBRIDGE_OAUTH_CLIENT_SECRET"}]}`
		}, want: "secret_left_behind"},
		"ssm offline": {edit: func(a *awsStub) {
			a.out["ssm describe-instance-information"] = `{"InstanceInformationList":[{"PingStatus":"ConnectionLost"}]}`
		}, want: "ssm_offline"},
		"ssm run failed": {edit: func(a *awsStub) { a.out["ssm get-command-invocation"] = `{"Status":"Failed"}` }, want: "ssm_failed"},
		"not mounted": {edit: func(a *awsStub) {
			a.out["ssm get-command-invocation"] = `{"Status":"Success","StandardOutputContent":"mount none\n"}`
		}, want: "data_not_mounted"},
		"port closed": {dial: errors.New("refused"), want: "port_closed"},
	}
	for name, c := range cases {
		a := healthyAWS()
		if c.edit != nil {
			c.edit(a)
		}
		ips := c.ips
		if ips == nil {
			ips = []string{"203.0.113.7"}
		}
		stubAWS(t, a, ips, c.dial)
		p := awsProbe()
		if c.probe != nil {
			c.probe(p)
		}
		r := checkInfra(context.Background(), p)
		if (c.want == "" && r.Status != "pass") || (c.want != "" && r.Reason != c.want) {
			t.Fatalf("%s: %+v", name, r)
		}
	}
}

func TestCloudCheckBackupsOnS3(t *testing.T) {
	listing := func(keys ...string) string {
		b, _ := json.Marshal(keys)
		return string(b)
	}
	for name, c := range map[string]struct {
		bucket, objects string
		region          string
		err             error
		want            string
	}{
		"fresh":       {"clawbridge-backups-123456789012-us-east-1", listing("postgres/daily/20261026T023000Z.dump", "postgres/daily/20261026T023000Z.sha256"), "us-east-1", nil, ""},
		"half a pair": {"clawbridge-backups", listing("postgres/daily/20261026T023000Z.dump"), "us-east-1", nil, "no_backup"},
		"stale":       {"clawbridge-backups", listing("postgres/daily/20261024T023000Z.dump", "postgres/daily/20261024T023000Z.sha256"), "us-east-1", nil, "backup_stale"},
		"empty":       {"clawbridge-backups", `null`, "us-east-1", nil, "no_backup"},
		"unreadable":  {"clawbridge-backups", "", "us-east-1", errors.New("AccessDenied"), "bucket_unreadable"},
		"bad bucket":  {"Bad_Bucket", "", "us-east-1", nil, "needs_bucket"},
		"no region":   {"clawbridge-backups", "", "", nil, "needs_region"},
	} {
		a := &awsStub{out: map[string]string{"s3api list-objects-v2": c.objects}, err: map[string]error{"s3api list-objects-v2": c.err}}
		stubAWS(t, a, nil, nil)
		p := &cloudProbe{bucket: c.bucket, target: cloudTarget{region: c.region, profile: "my-profile"}}
		if c.region == "" {
			p.target.profile = "my-profile" // AWS by its profile, but no region
		}
		r := checkBackups(context.Background(), p)
		if (c.want == "" && r.Status != "pass") || (c.want != "" && r.Reason != c.want) {
			t.Fatalf("%s: %+v", name, r)
		}
		if c.want == "" {
			got := strings.Join(a.calls[0], " ")
			if !strings.Contains(got, "--bucket "+c.bucket+" --prefix postgres/daily/ --query Contents[].Key") || !strings.Contains(got, "--profile my-profile --region us-east-1") {
				t.Fatalf("argv %v", a.calls[0])
			}
		}
	}
}

// Over SSM, a shell failure reads as SSM's, never as SSH's.
func TestRemoteFailureFollowsTheRoute(t *testing.T) {
	a := healthyAWS()
	a.out["ssm send-command"] = `{}`
	stubAWS(t, a, []string{"203.0.113.7"}, nil)
	p := awsProbe()
	if _, err := p.facts(context.Background()); err == nil {
		t.Fatal("facts without a command id")
	}
	if r := p.remoteFailure("server", p.remoteErr); r.Reason != "ssm_failed" || strings.Contains(r.Advice, "SSH") {
		t.Fatalf("%+v", r)
	}
	ssh := &cloudProbe{target: cloudTarget{host: "admin@bridge.example.test"}}
	if r := ssh.remoteFailure("server", errors.New("x")); r.Reason != "ssh_failed" {
		t.Fatalf("%+v", r)
	}
	if r := p.remoteFailure("server", errCloudAuth); r.Reason != "cloud_auth" {
		t.Fatalf("%+v", r)
	}
}

// A missing or bad AWS flag is the flag's own error, before anything runs.
func TestCloudCheckAWSFlagsFailBeforeAnythingRuns(t *testing.T) {
	a := &awsStub{out: map[string]string{}}
	stubAWS(t, a, nil, nil)
	for flagErr, args := range map[string][]string{
		"--region must":  {"--instance", ec2ID},
		"--region must ": {"--instance", ec2ID, "--region", "useast1"},
		"--profile must": {"--instance", ec2ID, "--region", "us-east-1", "--profile", "a profile"},
		"--account must": {"--instance", ec2ID, "--region", "us-east-1", "--account", "12345"},
	} {
		err := Run(context.Background(), append([]string{"cloud", "check", "server", "--server", "https://bridge.example.test"}, args...), &strings.Builder{}, &strings.Builder{})
		if err == nil || !strings.Contains(err.Error(), strings.TrimSpace(flagErr)) || len(a.calls) != 0 {
			t.Fatalf("%v: %v, %d aws calls", args, err, len(a.calls))
		}
	}
}

// The account is checked once per transport, not per command.
func TestSSMChecksTheAccountOnce(t *testing.T) {
	a := healthyAWS()
	stubAWS(t, a, nil, nil)
	tr, err := cloudTarget{instance: ec2ID, region: "us-east-1", account: "123456789012"}.transport()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := tr.run(context.Background(), factsScript(), nil); err != nil {
			t.Fatal(err)
		}
	}
	sts := 0
	for _, argv := range a.calls {
		if argv[1] == "sts" {
			sts++
		}
	}
	if sts != 1 {
		t.Fatalf("%d account checks", sts)
	}
	a.out["sts get-caller-identity"] = `{"Account":"999999999999"}`
	other, err := cloudTarget{instance: ec2ID, region: "us-east-1", account: "123456789012"}.transport()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.run(context.Background(), factsScript(), nil); !errors.Is(err, errCloudAuth) {
		t.Fatalf("another account: %v", err)
	}
}

// The AWS tests run against a fake aws CLI, so they hold on every OS,
// Windows CI included; only TestAWSRouteIsRefusedOnWindows plays Windows.
func init() { awsGOOS = "linux" }

// Windows has no /dev/stdin for the aws CLI's request: the route says so up
// front, before any aws call, instead of failing later as ssm_failed.
func TestAWSRouteIsRefusedOnWindows(t *testing.T) {
	a := healthyAWS()
	stubAWS(t, a, nil, nil)
	old := awsGOOS
	awsGOOS = "windows"
	t.Cleanup(func() { awsGOOS = old })
	t.Cleanup(func() {
		if len(a.calls) != 0 {
			t.Errorf("aws ran on Windows: %v", a.calls)
		}
	})
	if _, err := (cloudTarget{instance: ec2ID, region: "us-east-1"}).transport(); err == nil || !strings.Contains(err.Error(), "macOS, Linux or WSL") {
		t.Fatalf("%v", err)
	}
	if r := awsBackups(context.Background(), &cloudProbe{bucket: "clawbridge-backups", target: cloudTarget{region: "us-east-1"}}); r.Reason != "needs_region" || !strings.Contains(r.Advice, "WSL") {
		t.Fatalf("%+v", r)
	}
}

// The S3 lifecycle template keeps 30 days of daily backups, as the GCS one does.
func TestS3LifecycleTemplate(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "s3-lifecycle.json"))
	if err != nil {
		t.Fatal(err)
	}
	var lc struct {
		Rules []struct {
			Status     string
			Filter     struct{ Prefix string }
			Expiration struct{ Days int }
		}
	}
	if json.Unmarshal(raw, &lc) != nil || len(lc.Rules) != 1 || lc.Rules[0].Status != "Enabled" ||
		lc.Rules[0].Filter.Prefix != "postgres/daily/" || lc.Rules[0].Expiration.Days != 30 {
		t.Fatalf("%s", raw)
	}
}
