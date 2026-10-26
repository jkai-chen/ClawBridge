package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// AWS (live/ONBOARDING-DESIGN.md §13): an EC2 instance reached over SSM with
// the user's own AWS CLI profile, never port 22. Every aws command names the
// profile and region explicitly and, in the checks, only reads.

var (
	instancePattern   = regexp.MustCompile(`^i-[0-9a-f]{8,17}$`)
	awsRegionPattern  = regexp.MustCompile(`^[a-z]{2}(-gov)?-[a-z]+-[0-9]$`)
	awsProfilePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
	accountPattern    = regexp.MustCompile(`^[0-9]{12}$`)
	s3BucketPattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
)

// defaultAWSProfile is the dedicated named profile the hand-over uses, the
// counterpart of the gcloud configuration. ClawBridge never creates it and
// never reads ~/.aws.
const defaultAWSProfile = "clawbridge-onboarding"

const (
	awsAdvice    = "the AWS CLI could not read it; check the profile's credentials and that --region is the server's region"
	ssmAdvice    = "the instance did not run the check over SSM; check that its role has AmazonSSMManagedInstanceCore and the SSM agent runs"
	accountMsg   = "the AWS profile is signed in to another account than --account; switch the profile's account"
	ssmPollEvery = time.Second
)

// ssmWait is how long a check waits for SSM to run a script; tests shorten it.
var ssmWait = 60 * time.Second

// awsCLI runs the user's own aws CLI with an optional stdin; tests replace it.
// Its stderr is dropped: the CLI's messages are not shown, only fixed reasons.
var awsCLI = func(ctx context.Context, argv []string, stdin []byte) ([]byte, error) {
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	cmd.Stdout = &limitedWriter{w: &out, n: 64 << 10}
	err := cmd.Run()
	return out.Bytes(), err
}

// awsGOOS is the client's OS; tests set it.
var awsGOOS = runtime.GOOS

var errCloudAuth = errors.New("the AWS profile is signed in to another account")

// aws is one aws command for the target: the profile, region and JSON output
// are always explicit.
func (t cloudTarget) aws(args ...string) []string {
	profile := t.profile
	if profile == "" {
		profile = defaultAWSProfile
	}
	return append(append([]string{"aws"}, args...), "--profile", profile, "--region", t.region, "--output", "json")
}

// awsInputs validates the AWS flags; nothing runs before they pass. The aws
// requests go on the CLI's stdin as file:///dev/stdin, which Windows lacks.
func (t cloudTarget) awsInputs() error {
	if awsGOOS == "windows" {
		return errors.New("the AWS route needs macOS, Linux or WSL: the aws CLI reads its request from /dev/stdin")
	}
	if !awsRegionPattern.MatchString(t.region) {
		return errors.New("--region must be an AWS region, such as us-east-1")
	}
	if t.profile != "" && !awsProfilePattern.MatchString(t.profile) {
		return errors.New("--profile must be a named AWS CLI profile")
	}
	if t.account != "" && !accountPattern.MatchString(t.account) {
		return errors.New("--account must be a 12-digit AWS account ID")
	}
	return nil
}

// awsAccount: with --account, the profile is signed in to that account.
func (t cloudTarget) awsAccount(ctx context.Context) error {
	if t.account == "" {
		return nil
	}
	out, err := awsCLI(ctx, t.aws("sts", "get-caller-identity"), nil)
	var id struct{ Account string }
	if err != nil || json.Unmarshal(out, &id) != nil {
		return errors.New("the AWS profile could not be read")
	}
	if id.Account != t.account {
		return errCloudAuth
	}
	return nil
}

// ssmTransport runs a fixed script on an EC2 instance with SSM's
// AWS-RunShellScript, as root, and reads its output back. The request goes on
// the aws CLI's stdin, so nothing is assembled into argv from the script. Its
// inputs were validated by transport(); the account is checked once.
type ssmTransport struct {
	target  cloudTarget
	account *bool // the account check passed
}

func (s ssmTransport) ready(ctx context.Context) error {
	if *s.account {
		return nil
	}
	if err := s.target.awsAccount(ctx); err != nil {
		return err
	}
	*s.account = true
	return nil
}

func (s ssmTransport) run(ctx context.Context, script string, _ io.Writer) ([]byte, error) {
	t := s.target
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	request, err := json.Marshal(map[string]any{
		"InstanceIds":    []string{t.instance},
		"DocumentName":   "AWS-RunShellScript",
		"Comment":        "clawbridge",
		"TimeoutSeconds": int(ssmWait / time.Second),
		// executionTimeout stops the script on the instance too, not only the wait here.
		"Parameters": map[string][]string{
			"commands":         {`sh -c '` + script + `'`},
			"executionTimeout": {strconv.Itoa(int(ssmWait / time.Second))},
		},
	})
	if err != nil {
		return nil, err
	}
	out, err := awsCLI(ctx, t.aws("ssm", "send-command", "--cli-input-json", "file:///dev/stdin"), request)
	var sent struct{ Command struct{ CommandId string } }
	if err != nil || json.Unmarshal(out, &sent) != nil || sent.Command.CommandId == "" {
		return nil, errors.New("SSM did not accept the command")
	}
	id := sent.Command.CommandId
	deadline := time.Now().Add(ssmWait)
	for {
		out, err = awsCLI(ctx, t.aws("ssm", "get-command-invocation", "--command-id", id, "--instance-id", t.instance), nil)
		var inv struct {
			Status, StandardOutputContent string
			ResponseCode                  int
		}
		// Right after send-command the invocation may not exist yet: poll on.
		if err == nil && json.Unmarshal(out, &inv) == nil {
			switch inv.Status {
			case "Success":
				return []byte(inv.StandardOutputContent), nil
			case "Failed", "Cancelled", "TimedOut", "Cancelling":
				return nil, &ssmRunError{commandID: id, code: inv.ResponseCode}
			}
		}
		if time.Now().After(deadline) {
			return nil, &ssmRunError{commandID: id, unfinished: true}
		}
		select {
		case <-ctx.Done():
			return nil, &ssmRunError{commandID: id, unfinished: true}
		case <-time.After(ssmPollEvery):
		}
	}
}

// ssmRunError is a script SSM ran without success: it failed with the
// script's exit code, or it had not finished when the wait ended.
type ssmRunError struct {
	commandID  string
	code       int
	unfinished bool
}

func (e *ssmRunError) Error() string {
	if e.unfinished {
		return "SSM did not finish the script in time"
	}
	return "the script did not succeed over SSM"
}

// secretAdvice is a secret write's failure that is safe to show as is: fixed
// text, never anything the value or the provider wrote.
type secretAdvice string

func (a secretAdvice) Error() string { return string(a) }

// setSecret writes one key through a short-lived Parameter Store
// SecureString (§13, ON-M3): SSM has no stdin path, and send-command records
// its parameters. The value goes to put-parameter on the CLI's stdin; a fixed
// script on the instance reads it with the instance's own role and pipes it
// into remoteSetScript; the parameter is deleted on every path.
func (s ssmTransport) setSecret(ctx context.Context, key, value string, stderr io.Writer) (err error) {
	t := s.target
	if err = s.ready(ctx); err != nil {
		return err
	}
	name := secretParameter(t.instance, key)
	// An expiration policy bounds its life even if this process dies before
	// the delete (policies need the Advanced tier: cents a month, prorated).
	expiry, err := json.Marshal([]map[string]any{{
		"Type": "Expiration", "Version": "1.0",
		"Attributes": map[string]string{"Timestamp": cloudNow().UTC().Add(secretParameterLife).Format(time.RFC3339)},
	}})
	if err != nil {
		return err
	}
	request, err := json.Marshal(map[string]any{
		"Name": name, "Type": "SecureString", "Overwrite": true, "Value": value, "Tier": "Advanced",
		"Policies":    string(expiry),
		"Description": "clawbridge cloud secret set: deleted once written, expires within 15 minutes",
	})
	if err != nil {
		return err
	}
	defer func() {
		// Even after a failed or cancelled write (a lost response may still
		// have created it), with its own deadline.
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if _, derr := awsCLI(cleanup, t.aws("ssm", "delete-parameter", "--name", name), nil); derr != nil && s.parameterExists(cleanup, name) {
			left := errors.New("parameter left behind: " + name + "; delete it with aws ssm delete-parameters --names " + name)
			err = errors.Join(err, left)
		}
	}()
	if _, err = awsCLI(ctx, t.aws("ssm", "put-parameter", "--cli-input-json", "file:///dev/stdin"), request); err != nil {
		return secretAdvice("Parameter Store did not take the value; check the profile may put Advanced-tier parameters, and this machine's clock (the parameter's expiry is set from it)")
	}
	_, err = s.run(ctx, ssmSetScript(name, t.region, key), stderr)
	var failed *ssmRunError
	switch {
	case !errors.As(err, &failed):
		return err
	case failed.unfinished:
		// Stop it before the delete, so it can't read the parameter after.
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_, _ = awsCLI(cleanup, t.aws("ssm", "cancel-command", "--command-id", failed.commandID, "--instance-ids", t.instance), nil)
		return secretAdvice("the write did not finish in time, so it may not have been written; run cloud secret set again")
	case failed.code == 3:
		return secretAdvice("the instance has no AWS CLI; install it there (Amazon Linux 2023 has it)")
	case failed.code == 5:
		return secretAdvice("the instance's role cannot read its parameter; attach the policy in deploy/aws-instance-role-policy.json")
	}
	return err
}

// parameterExists says the parameter is still there, or that this can't be
// confirmed (then it may be).
func (s ssmTransport) parameterExists(ctx context.Context, name string) bool {
	out, err := awsCLI(ctx, s.target.aws("ssm", "describe-parameters", "--parameter-filters", "Key=Name,Option=Equals,Values="+name), nil)
	var found struct{ Parameters []struct{ Name string } }
	if err != nil || json.Unmarshal(out, &found) != nil {
		return true
	}
	return len(found.Parameters) > 0
}

// secretParameterLife is the longest a secret's parameter can live: the
// write takes seconds, and the expiration policy removes it even if the
// delete never runs.
const secretParameterLife = 15 * time.Minute

// secretParameter is the short-lived parameter for one key: scoped to the
// instance, whose role may read only its own (the workspace may not exist yet).
func secretParameter(instance, key string) string { return "/clawbridge/" + instance + "/" + key }

// ssmSetScript reads the parameter with the instance's role and pipes the
// value straight into remoteSetScript, which refuses an empty one. Every input
// is pattern-checked (the instance and region) or from the fixed key list.
// It exits 3 without the AWS CLI, and 5 when the role cannot read the
// parameter (checked without decrypting it).
func ssmSetScript(name, region, key string) string {
	return "command -v aws >/dev/null || exit 3\n" +
		"aws ssm get-parameter --name " + name + " --region " + region + " --query Parameter.Name --output text >/dev/null || exit 5\n" +
		"aws ssm get-parameter --with-decryption --name " + name + " --region " + region +
		" --query Parameter.Value --output text | (\n" + remoteSetScript(key, serverEnvFile) + "\n)"
}

func (ssmTransport) describe() string {
	return "check the AWS profile, SSM access to the instance and its role"
}

// remoteFailure is a check's result when the server's shell could not run the
// facts script: the advice follows the route the user took.
func (p *cloudProbe) remoteFailure(check string, err error) cloudResult {
	switch {
	case errors.Is(err, errCloudAuth):
		return failed(check, "cloud_auth", accountMsg)
	case p.target.instance != "":
		return failed(check, "ssm_failed", ssmAdvice)
	}
	return failed(check, "ssh_failed", remoteAdvice)
}

type ec2Instance struct {
	State struct {
		Name string
	}
	PublicIpAddress     string
	RootDeviceName      string
	BlockDeviceMappings []struct {
		DeviceName string
		Ebs        struct{ VolumeId string }
	}
	SecurityGroups []struct{ GroupId string }
}

type ipPermission struct {
	IpProtocol       string
	FromPort, ToPort int
	IpRanges         []struct{ CidrIp string }
	Ipv6Ranges       []struct{ CidrIpv6 string }
}

// awsInfra: the instance runs, its public IP is an Elastic IP associated with
// it, the domain resolves to it, an encrypted EBS data volume is attached,
// its security groups allow tcp/443 and nothing else in, SSM reaches it, the
// data volume is mounted, and TCP 443 answers.
func awsInfra(ctx context.Context, p *cloudProbe) cloudResult {
	t := p.target
	if !instancePattern.MatchString(t.instance) {
		return failed("infra", "needs_instance", "pass --instance i-… --region R")
	}
	if err := t.awsInputs(); err != nil {
		return failed("infra", "needs_instance", err.Error())
	}
	if err := t.awsAccount(ctx); err != nil {
		return failed("infra", "cloud_auth", accountMsg)
	}
	out, err := awsCLI(ctx, t.aws("ec2", "describe-instances", "--instance-ids", t.instance), nil)
	var described struct {
		Reservations []struct{ Instances []ec2Instance }
	}
	if err != nil || json.Unmarshal(out, &described) != nil || len(described.Reservations) != 1 || len(described.Reservations[0].Instances) != 1 {
		return failed("infra", "vm_unreadable", awsAdvice)
	}
	vm := described.Reservations[0].Instances[0]
	if vm.State.Name != "running" {
		return failed("infra", "vm_not_running", "the instance is "+vm.State.Name+"; start it")
	}
	ip := vm.PublicIpAddress
	if net.ParseIP(ip) == nil {
		return failed("infra", "no_external_ip", "associate the Elastic IP with the instance")
	}
	out, err = awsCLI(ctx, t.aws("ec2", "describe-addresses", "--filters", "Name=public-ip,Values="+ip), nil)
	var addresses struct {
		Addresses []struct{ PublicIp, InstanceId string }
	}
	if err != nil || json.Unmarshal(out, &addresses) != nil {
		return failed("infra", "vm_unreadable", awsAdvice)
	}
	if len(addresses.Addresses) != 1 || addresses.Addresses[0].PublicIp != ip || addresses.Addresses[0].InstanceId != t.instance {
		return failed("infra", "ip_not_static", "the instance's IP "+ip+" is not an Elastic IP; allocate one and associate it")
	}
	u, ok := serverURL(p.server)
	if !ok {
		return failed("infra", "no_server", "pass --server https://YOUR-DOMAIN or run setup --cloud")
	}
	domain := u.Hostname()
	addrs, _ := lookupHost(ctx, domain)
	resolves := false
	for _, a := range addrs {
		resolves = resolves || net.ParseIP(a).Equal(net.ParseIP(ip))
	}
	if !resolves {
		return failed("infra", "dns_mismatch", domain+" must have an A record for "+ip)
	}
	var volumes []string
	for _, m := range vm.BlockDeviceMappings {
		if m.DeviceName != vm.RootDeviceName && m.Ebs.VolumeId != "" {
			volumes = append(volumes, m.Ebs.VolumeId)
		}
	}
	if len(volumes) == 0 {
		return failed("infra", "no_data_disk", "attach a separate encrypted EBS volume for the database")
	}
	out, err = awsCLI(ctx, t.aws(append([]string{"ec2", "describe-volumes", "--volume-ids"}, volumes...)...), nil)
	var vols struct {
		Volumes []struct {
			VolumeId  string
			Encrypted bool
		}
	}
	if err != nil || json.Unmarshal(out, &vols) != nil || len(vols.Volumes) != len(volumes) {
		return failed("infra", "vm_unreadable", awsAdvice)
	}
	for _, v := range vols.Volumes {
		if !v.Encrypted {
			return failed("infra", "data_disk_unencrypted", "the data volume "+v.VolumeId+" is not encrypted; replace it with an encrypted one")
		}
	}
	if r := awsIngress(ctx, t, vm); r != nil {
		return *r
	}
	out, err = awsCLI(ctx, t.aws("ssm", "describe-instance-information", "--filters", "Key=InstanceIds,Values="+t.instance), nil)
	var info struct{ InstanceInformationList []struct{ PingStatus string } }
	if err != nil || json.Unmarshal(out, &info) != nil {
		return failed("infra", "vm_unreadable", awsAdvice)
	}
	if len(info.InstanceInformationList) != 1 || info.InstanceInformationList[0].PingStatus != "Online" {
		return failed("infra", "ssm_offline", "SSM does not see the instance online; give it a role with AmazonSSMManagedInstanceCore and check the SSM agent")
	}
	// A secret's parameter outlives its write only if the write's process
	// died; its policy expires it, and this says so in the meantime.
	prefix := secretParameter(t.instance, "")
	out, err = awsCLI(ctx, t.aws("ssm", "describe-parameters", "--parameter-filters", "Key=Name,Option=BeginsWith,Values="+prefix), nil)
	var params struct{ Parameters []struct{ Name string } }
	if err != nil || json.Unmarshal(out, &params) != nil {
		return failed("infra", "parameters_unreadable", "the profile cannot list its secret parameters; it needs ssm:DescribeParameters (deploy/aws-profile-policy.json)")
	}
	if len(params.Parameters) > 0 {
		var names []string
		for _, prm := range params.Parameters {
			names = append(names, prm.Name)
		}
		return failed("infra", "secret_left_behind", "a secret's parameter outlived its write: delete it with aws ssm delete-parameters --names "+strings.Join(names, " "))
	}
	f, err := p.facts(ctx)
	if err != nil {
		return p.remoteFailure("infra", err)
	}
	if f.mount == "" || f.mount == "none" {
		return failed("infra", "data_not_mounted", "mount the data volume at "+dataMount+" (and in /etc/fstab)")
	}
	if dialTCP(ctx, net.JoinHostPort(domain, "443")) != nil {
		return failed("infra", "port_closed", "TCP 443 does not answer; allow tcp/443 in the security group and start the server")
	}
	return cloudResult{Check: "infra", Status: "pass"}
}

// awsIngress: the instance's security groups let in tcp/443 and nothing else.
// SSH from anywhere is called out on its own: the instance is reached over SSM.
func awsIngress(ctx context.Context, t cloudTarget, vm ec2Instance) *cloudResult {
	var groups []string
	for _, g := range vm.SecurityGroups {
		groups = append(groups, g.GroupId)
	}
	if len(groups) == 0 {
		return nil
	}
	out, err := awsCLI(ctx, t.aws(append([]string{"ec2", "describe-security-groups", "--group-ids"}, groups...)...), nil)
	var sg struct {
		SecurityGroups []struct{ IpPermissions []ipPermission }
	}
	if err != nil || json.Unmarshal(out, &sg) != nil {
		r := failed("infra", "vm_unreadable", awsAdvice)
		return &r
	}
	extra := false
	for _, g := range sg.SecurityGroups {
		for _, rule := range g.IpPermissions {
			if rule.IpProtocol == "tcp" && rule.FromPort == 443 && rule.ToPort == 443 {
				continue
			}
			world := false
			for _, r := range rule.IpRanges {
				world = world || r.CidrIp == "0.0.0.0/0"
			}
			for _, r := range rule.Ipv6Ranges {
				world = world || r.CidrIpv6 == "::/0"
			}
			ssh := rule.IpProtocol == "-1" || (rule.IpProtocol == "tcp" && rule.FromPort <= 22 && 22 <= rule.ToPort)
			if ssh && world {
				r := failed("infra", "ssh_port_open", "port 22 is open to the internet; remove that rule (the instance is reached over SSM)")
				return &r
			}
			extra = true
		}
	}
	if extra {
		r := failed("infra", "extra_ingress", "the security groups allow more than tcp/443 in (a default group's own rules count too); use a dedicated security group with only tcp/443")
		return &r
	}
	return nil
}

// awsBackups: the newest complete dump and checksum pair in the S3 bucket is
// under 26 hours old.
func awsBackups(ctx context.Context, p *cloudProbe) cloudResult {
	t := p.target
	if !s3BucketPattern.MatchString(p.bucket) {
		return failed("backups", "needs_bucket", "pass --bucket NAME (the bucket in CLAWBRIDGE_BACKUP_URL)")
	}
	if err := t.awsInputs(); err != nil {
		return failed("backups", "needs_region", err.Error())
	}
	if err := t.awsAccount(ctx); err != nil {
		return failed("backups", "cloud_auth", accountMsg)
	}
	// Keys only, so a bucket kept to 30 days (deploy/s3-lifecycle.json) stays
	// far under the output cap.
	out, err := awsCLI(ctx, t.aws("s3api", "list-objects-v2", "--bucket", p.bucket, "--prefix", "postgres/daily/", "--query", "Contents[].Key"), nil)
	var keys []string
	if err != nil || json.Unmarshal(out, &keys) != nil {
		return failed("backups", "bucket_unreadable", awsAdvice)
	}
	var names []string
	for _, k := range keys {
		names = append(names, "/"+strings.TrimPrefix(k, "/"))
	}
	return newestBackup(names)
}
