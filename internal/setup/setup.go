// Package setup plans and applies explicit ClawBridge deployment choices.
//
// Planning is pure. Applying requires the exact plan returned by BuildPlan and
// its typed confirmation phrase. Every cloud command names both the GCP project
// and initiating account so ambient gcloud configuration never selects either.
package setup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"clawbridge/internal/bus"
	"clawbridge/internal/remote"
	"github.com/pelletier/go-toml/v2"
)

// Executor is the shared application boundary used by local and hosted setup.
type Executor interface {
	Execute(context.Context, bus.Request) (bus.Result, error)
	ResolveProject(context.Context, string, string, string) (string, string, error)
}

// Runner executes one already-tokenised gcloud command. Implementations must not
// pass the arguments through a shell.
type Runner interface {
	Run(context.Context, []string) ([]byte, error)
}

// MonthlyCost is an explicitly supplied estimate in minor currency units. Setup
// does not silently invent prices for a region or billing account.
type MonthlyCost struct {
	Currency       string `json:"currency"`
	ComputeMinor   int64  `json:"compute_minor"`
	DiskMinor      int64  `json:"disk_minor"`
	SnapshotMinor  int64  `json:"snapshot_minor"`
	IPv4Minor      int64  `json:"ipv4_minor"`
	EgressMinor    int64  `json:"egress_minor"`
	TaxMinor       int64  `json:"tax_minor"`
	PricingBasis   string `json:"pricing_basis"`
	EstimatedMonth string `json:"estimated_month"`
}

// CostLine makes the total auditable without relying on floating-point math.
type CostLine struct {
	Item         string `json:"item"`
	MonthlyMinor int64  `json:"monthly_minor"`
}

// Cost is the normalised estimate shown in a create plan.
type Cost struct {
	Currency          string     `json:"currency"`
	Lines             []CostLine `json:"lines"`
	TotalMonthlyMinor int64      `json:"total_monthly_minor"`
	PricingBasis      string     `json:"pricing_basis"`
	EstimatedMonth    string     `json:"estimated_month"`
}

// CreateInput contains every choice that changes a billable create plan. Empty
// values are rejected; there is intentionally no ambient project or region.
type CreateInput struct {
	Project               string      `json:"project"`
	Zone                  string      `json:"zone"`
	VM                    string      `json:"vm"`
	MachineType           string      `json:"machine_type"`
	DiskType              string      `json:"disk_type"`
	DiskSizeGB            int         `json:"disk_size_gb"`
	ImageFamily           string      `json:"image_family"`
	ImageProject          string      `json:"image_project"`
	Network               string      `json:"network"`
	Subnetwork            string      `json:"subnetwork"`
	FirewallRule          string      `json:"firewall_rule"`
	ServiceAccountID      string      `json:"service_account_id"`
	HumanAccount          string      `json:"human_account"`
	Audience              string      `json:"audience"`
	Workspace             string      `json:"workspace"`
	SnapshotPolicy        string      `json:"snapshot_policy"`
	SnapshotRetentionDays int         `json:"snapshot_retention_days"`
	SnapshotStartTimeUTC  string      `json:"snapshot_start_time_utc"`
	Cost                  MonthlyCost `json:"cost"`
}

// Operation is one displayed command. ReadOnly commands must not change cloud
// resources; all other operations are billable or IAM-mutating.
type Operation struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Command     []string `json:"command"`
	ReadOnly    bool     `json:"read_only"`
}

// Plan is deterministic for a given CreateInput. Confirmation includes the plan
// fingerprint, project, VM, zone and estimated monthly total.
type Plan struct {
	Project               string      `json:"project"`
	Region                string      `json:"region"`
	Zone                  string      `json:"zone"`
	VM                    string      `json:"vm"`
	MachineType           string      `json:"machine_type"`
	DiskType              string      `json:"disk_type"`
	DiskSizeGB            int         `json:"disk_size_gb"`
	Network               string      `json:"network"`
	Subnetwork            string      `json:"subnetwork"`
	ServiceAccount        string      `json:"service_account"`
	BootstrapPrincipal    string      `json:"bootstrap_principal"`
	Audience              string      `json:"audience"`
	Workspace             string      `json:"workspace"`
	SnapshotPolicy        string      `json:"snapshot_policy"`
	SnapshotRetentionDays int         `json:"snapshot_retention_days"`
	SnapshotStartTimeUTC  string      `json:"snapshot_start_time_utc"`
	Cost                  Cost        `json:"cost"`
	Operations            []Operation `json:"operations"`
	Fingerprint           string      `json:"fingerprint"`
	Confirmation          string      `json:"confirmation"`
}

// ApplyResult reports only operations whose commands completed. Cloud creation
// has no automatic rollback.
type ApplyResult struct {
	Fingerprint string      `json:"fingerprint"`
	Executed    []Operation `json:"executed"`
}

// LocalResult proves the local workspace initialization response.
type LocalResult struct {
	WorkspaceID string `json:"workspace_id"`
	Workspace   string `json:"workspace"`
	Principal   string `json:"principal"`
}

// JoinInput routes an online authorization check to an existing hosted service.
// ConfigPath is updated only after the server proves the expected identity.
type JoinInput struct {
	OAuthClientSecret         string `json:"-"`
	OAuthDeviceClientSecret   string `json:"-"`
	ConfigPath                string `json:"config_path"`
	Workspace                 string `json:"workspace"`
	Project                   string `json:"project"`
	GitRemote                 string `json:"git_remote"`
	Session                   string `json:"session"`
	Instance                  string `json:"instance"`
	Server                    string `json:"server"`
	Audience                  string `json:"audience"`
	Account                   string `json:"account"`
	ImpersonateServiceAccount string `json:"impersonate_service_account"`
	Principal                 string `json:"principal"`
	Cache                     string `json:"cache"`
	CredentialSource          string `json:"credential_source"`
	CredentialsFile           string `json:"credentials_file"`
	OAuthClientID             string `json:"oauth_client_id"`
	OAuthDeviceClientID       string `json:"oauth_device_client_id"`
	OAuthKeyringService       string `json:"oauth_keyring_service"`
}

// Format keeps private installed-app credentials out of all fmt diagnostics,
// including Go-syntax formatting (%#v). JSON uses the same public projection.
func (in JoinInput) Format(state fmt.State, verb rune) {
	type public JoinInput
	raw, err := json.Marshal(public(in))
	if err != nil {
		_, _ = io.WriteString(state, "JoinInput{redacted}")
		return
	}
	_, _ = state.Write(raw)
}

// JoinResult describes the trusted route written after a successful online check.
type JoinResult struct {
	ConfigPath string `json:"config_path"`
	Workspace  string `json:"workspace"`
	Project    string `json:"project"`
	Principal  string `json:"principal"`
	Changed    bool   `json:"changed"`
}

// GCloudRunner accepts a full tokenized gcloud command and invokes the selected
// executable with its argument tail, without a shell. Stderr is discarded and
// stdout is bounded; command output is never interpolated into errors.
type GCloudRunner struct {
	Path string
}

func (r GCloudRunner) Run(ctx context.Context, args []string) ([]byte, error) {
	if ctx == nil {
		return nil, errors.New("command context is required")
	}
	if len(args) < 2 || args[0] != "gcloud" {
		return nil, errors.New("a full tokenized gcloud command is required")
	}
	name := r.Path
	if name == "" {
		name = "gcloud"
	}
	command := exec.CommandContext(ctx, name, args[1:]...)
	command.Stdin = nil
	var stdout cappedBuffer
	stdout.limit = 64 << 10
	command.Stdout = &stdout
	// gcloud may include account/project context in stderr; suppress it entirely.
	command.Stderr = io.Discard
	err := command.Run()
	if ctx.Err() != nil {
		return nil, errors.New("gcloud command did not complete")
	}
	if err != nil {
		return nil, errors.New("gcloud command failed; inspect the displayed operation and Google Cloud audit log")
	}
	if stdout.overflow {
		return nil, errors.New("gcloud command output exceeded the safety limit")
	}
	return stdout.Bytes(), nil
}

type cappedBuffer struct {
	bytes.Buffer
	limit    int
	overflow bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) <= b.limit {
		return b.Buffer.Write(p)
	}
	remaining := b.limit - b.Len()
	if remaining > 0 {
		_, _ = b.Buffer.Write(p[:remaining])
	}
	b.overflow = true
	return len(p), nil
}

var (
	projectPattern   = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)
	zonePattern      = regexp.MustCompile(`^[a-z][a-z0-9-]*-[a-z]$`)
	resourcePattern  = regexp.MustCompile(`^[a-z](?:[-a-z0-9]{0,61}[a-z0-9])?$`)
	machinePattern   = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	serviceIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)
	emailPattern     = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
	startTimePattern = regexp.MustCompile(`^(?:[01][0-9]|2[0-3]):[0-5][0-9]$`)
	currencyPattern  = regexp.MustCompile(`^[A-Z]{3}$`)
)

func BuildPlan(in CreateInput) (Plan, error) {
	if err := validateCreateInput(in); err != nil {
		return Plan{}, err
	}
	region := regionForZone(in.Zone)
	serviceAccount := in.ServiceAccountID + "@" + in.Project + ".iam.gserviceaccount.com"
	cost, err := normaliseCost(in.Cost)
	if err != nil {
		return Plan{}, err
	}
	base := func(name, description string, readOnly bool, args ...string) Operation {
		command := append([]string{"gcloud"}, args...)
		command = append(command, "--project="+in.Project, "--account="+in.HumanAccount, "--quiet")
		return Operation{Name: name, Description: description, Command: command, ReadOnly: readOnly}
	}
	ops := []Operation{
		base("preflight-project", "Verify the explicitly selected GCP project is visible to the initiating account.", true,
			"projects", "describe", in.Project, "--format=value(projectId)"),
		base("enable-apis", "Enable Compute Engine, IAP, IAM and Resource Manager APIs.", false,
			"services", "enable", "compute.googleapis.com", "iap.googleapis.com", "iam.googleapis.com", "cloudresourcemanager.googleapis.com"),
		base("create-client-identity", "Create the service identity used by the personal pilot client.", false,
			"iam", "service-accounts", "create", in.ServiceAccountID, "--display-name=ClawBridge client identity"),
		base("grant-token-creation", "Allow the selected human to mint audience-bound tokens as the client identity.", false,
			"iam", "service-accounts", "add-iam-policy-binding", serviceAccount, "--member=user:"+in.HumanAccount, "--role=roles/iam.serviceAccountTokenCreator"),
		base("grant-service-account-use", "Allow the selected human to attach the client identity during operator work.", false,
			"iam", "service-accounts", "add-iam-policy-binding", serviceAccount, "--member=user:"+in.HumanAccount, "--role=roles/iam.serviceAccountUser"),
		base("grant-iap-access", "Grant the human IAP TCP-forwarding permission for the private service.", false,
			"projects", "add-iam-policy-binding", in.Project, "--member=user:"+in.HumanAccount, "--role=roles/iap.tunnelResourceAccessor", "--condition=None"),
		base("grant-os-login", "Grant the human OS Login access to the pilot VM.", false,
			"projects", "add-iam-policy-binding", in.Project, "--member=user:"+in.HumanAccount, "--role=roles/compute.osLogin", "--condition=None"),
		base("create-iap-firewall", "Allow SSH only from Google IAP to tagged ClawBridge VMs.", false,
			"compute", "firewall-rules", "create", in.FirewallRule, "--network="+in.Network, "--direction=INGRESS", "--priority=1000", "--action=ALLOW", "--rules=tcp:22", "--source-ranges=35.235.240.0/20", "--target-tags=clawbridge-server"),
		base("create-snapshot-schedule", "Create the selected daily disk snapshot schedule.", false,
			"compute", "resource-policies", "create", "snapshot-schedule", in.SnapshotPolicy, "--region="+region, "--max-retention-days="+fmt.Sprint(in.SnapshotRetentionDays), "--on-source-disk-delete=keep-auto-snapshots", "--daily-schedule", "--start-time="+in.SnapshotStartTimeUTC),
		base("create-vm", "Create the single-VM application and database host with loopback-only application exposure.", false,
			"compute", "instances", "create", in.VM, "--zone="+in.Zone, "--machine-type="+in.MachineType, "--network-interface=network="+in.Network+",subnet="+in.Subnetwork+",stack-type=IPV4_ONLY", "--image-family="+in.ImageFamily, "--image-project="+in.ImageProject, "--boot-disk-size="+fmt.Sprint(in.DiskSizeGB)+"GB", "--boot-disk-type="+in.DiskType, "--service-account="+serviceAccount, "--scopes=https://www.googleapis.com/auth/cloud-platform", "--tags=clawbridge-server", "--metadata=enable-oslogin=TRUE", "--labels=app=clawbridge"),
		base("attach-snapshot-schedule", "Attach the daily snapshot policy to the VM boot disk.", false,
			"compute", "disks", "add-resource-policies", in.VM, "--resource-policies="+in.SnapshotPolicy, "--zone="+in.Zone),
	}
	for _, op := range ops {
		if err = validateOperation(op, in.Project, in.HumanAccount); err != nil {
			return Plan{}, err
		}
	}
	p := Plan{
		Project: in.Project, Region: region, Zone: in.Zone, VM: in.VM,
		MachineType: in.MachineType, DiskType: in.DiskType, DiskSizeGB: in.DiskSizeGB,
		Network: in.Network, Subnetwork: in.Subnetwork,
		ServiceAccount: serviceAccount, BootstrapPrincipal: serviceAccount,
		Audience: in.Audience, Workspace: in.Workspace,
		SnapshotPolicy: in.SnapshotPolicy, SnapshotRetentionDays: in.SnapshotRetentionDays,
		SnapshotStartTimeUTC: in.SnapshotStartTimeUTC, Cost: cost, Operations: ops,
	}
	fingerprint, err := fingerprintPlan(p)
	if err != nil {
		return Plan{}, err
	}
	p.Fingerprint = fingerprint
	p.Confirmation = fmt.Sprintf("CREATE %s/%s/%s %s %s plan=%s",
		in.Project, in.VM, in.Zone, cost.Currency,
		formatMinor(cost.TotalMonthlyMinor), fingerprint[:12])
	return p, nil
}

// Apply refuses to execute unless a fresh plan built from the same input exactly
// equals the reviewed plan and confirmation. It never accepts raw command input.
func Apply(ctx context.Context, in CreateInput, approved Plan, confirmation string, runner Runner) (ApplyResult, error) {
	if ctx == nil {
		return ApplyResult{}, errors.New("apply context is required")
	}
	if runner == nil {
		return ApplyResult{}, errors.New("configured command runner is required")
	}
	fresh, err := BuildPlan(in)
	if err != nil {
		return ApplyResult{}, err
	}
	if !reflect.DeepEqual(fresh, approved) {
		return ApplyResult{}, errors.New("setup plan changed; display and confirm the fresh plan before applying")
	}
	if confirmation != fresh.Confirmation {
		return ApplyResult{}, errors.New("confirmation does not exactly match the displayed setup plan; no command was run")
	}
	result := ApplyResult{Fingerprint: fresh.Fingerprint}
	for _, op := range fresh.Operations {
		if err = validateOperation(op, fresh.Project, in.HumanAccount); err != nil {
			return result, err
		}
		if _, err = runner.Run(ctx, append([]string{}, op.Command...)); err != nil {
			return result, fmt.Errorf("setup operation %s failed: %w", op.Name, err)
		}
		result.Executed = append(result.Executed, op)
	}
	return result, nil
}

func validateCreateInput(in CreateInput) error {
	switch {
	case !projectPattern.MatchString(in.Project):
		return errors.New("explicit valid GCP project is required")
	case !zonePattern.MatchString(in.Zone):
		return errors.New("explicit valid GCP zone is required")
	case !resourcePattern.MatchString(in.VM):
		return errors.New("explicit valid VM name is required")
	case !machinePattern.MatchString(in.MachineType):
		return errors.New("explicit machine type is required")
	case in.DiskType != "pd-balanced" && in.DiskType != "pd-standard" && in.DiskType != "pd-ssd":
		return errors.New("disk type must be pd-balanced, pd-standard or pd-ssd")
	case in.DiskSizeGB < 10 || in.DiskSizeGB > 65536:
		return errors.New("disk size must be between 10 and 65536 GiB")
	case strings.TrimSpace(in.ImageFamily) == "" || !resourcePattern.MatchString(in.ImageFamily):
		return errors.New("explicit valid image family is required")
	case strings.TrimSpace(in.ImageProject) == "" || !resourcePattern.MatchString(in.ImageProject):
		return errors.New("explicit valid image project is required")
	case !resourcePattern.MatchString(in.Network) || !resourcePattern.MatchString(in.Subnetwork):
		return errors.New("explicit valid network and subnetwork are required")
	case !resourcePattern.MatchString(in.FirewallRule):
		return errors.New("explicit valid firewall rule name is required")
	case !serviceIDPattern.MatchString(in.ServiceAccountID):
		return errors.New("explicit valid service-account ID is required")
	case !emailPattern.MatchString(in.HumanAccount):
		return errors.New("explicit initiating Google account email is required")
	case strings.TrimSpace(in.Audience) == "":
		return errors.New("explicit audience is required")
	case strings.TrimSpace(in.Workspace) == "":
		return errors.New("explicit workspace is required")
	case !resourcePattern.MatchString(in.SnapshotPolicy):
		return errors.New("explicit valid snapshot policy name is required")
	case in.SnapshotRetentionDays < 1 || in.SnapshotRetentionDays > 365:
		return errors.New("snapshot retention must be between 1 and 365 days")
	case !startTimePattern.MatchString(in.SnapshotStartTimeUTC):
		return errors.New("snapshot start time must be UTC HH:MM")
	}
	return nil
}

func validateOperation(op Operation, project, account string) error {
	if len(op.Command) < 2 || op.Command[0] != "gcloud" {
		return errors.New("invalid setup operation executable")
	}
	hasProject, hasAccount := false, false
	for _, arg := range op.Command[1:] {
		if arg == "--project="+project {
			hasProject = true
		}
		if arg == "--account="+account {
			hasAccount = true
		}
		if strings.ContainsAny(arg, "\x00\r\n") {
			return errors.New("invalid control character in setup operation")
		}
	}
	if !hasProject || !hasAccount {
		return errors.New("every setup operation must name the explicit project and account")
	}
	return nil
}

func normaliseCost(in MonthlyCost) (Cost, error) {
	if !currencyPattern.MatchString(in.Currency) {
		return Cost{}, errors.New("cost currency must be a three-letter uppercase code")
	}
	if strings.TrimSpace(in.PricingBasis) == "" || strings.TrimSpace(in.EstimatedMonth) == "" {
		return Cost{}, errors.New("cost pricing basis and estimated billing month are required")
	}
	values := []struct {
		name string
		v    int64
	}{
		{"compute", in.ComputeMinor}, {"persistent disk", in.DiskMinor},
		{"snapshots", in.SnapshotMinor}, {"external IPv4", in.IPv4Minor},
		{"egress", in.EgressMinor}, {"tax", in.TaxMinor},
	}
	var total int64
	lines := make([]CostLine, 0, len(values))
	for _, value := range values {
		if value.v < 0 {
			return Cost{}, errors.New("cost components cannot be negative")
		}
		total += value.v
		lines = append(lines, CostLine{Item: value.name, MonthlyMinor: value.v})
	}
	if total <= 0 {
		return Cost{}, errors.New("a positive displayed monthly estimate is required")
	}
	return Cost{Currency: in.Currency, Lines: lines, TotalMonthlyMinor: total, PricingBasis: in.PricingBasis, EstimatedMonth: in.EstimatedMonth}, nil
}

func regionForZone(zone string) string {
	i := strings.LastIndex(zone, "-")
	if i < 0 {
		return ""
	}
	return zone[:i]
}

func formatMinor(v int64) string {
	return fmt.Sprintf("%d.%02d", v/100, v%100)
}

func fingerprintPlan(p Plan) (string, error) {
	copy := p
	copy.Fingerprint = ""
	copy.Confirmation = ""
	raw, err := json.Marshal(copy)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// Local initializes an explicitly selected local workspace. It does not write
// trusted routing; the caller can persist that only after this operation succeeds.
func Local(ctx context.Context, executor Executor, workspace string, retentionDays int) (LocalResult, error) {
	if ctx == nil {
		return LocalResult{}, errors.New("local setup context is required")
	}
	if executor == nil {
		return LocalResult{}, errors.New("local executor is required")
	}
	if strings.TrimSpace(workspace) == "" {
		return LocalResult{}, errors.New("local workspace name is required")
	}
	if retentionDays == 0 {
		retentionDays = 90
	}
	if retentionDays < 1 || retentionDays > 3650 {
		return LocalResult{}, errors.New("retention days must be between 1 and 3650")
	}
	result, err := executor.Execute(ctx, bus.Request{
		Operation: "workspace_init",
		Workspace: workspace,
		Args:      map[string]any{"mode": "local", "name": workspace, "retention_days": retentionDays},
	})
	if err != nil {
		return LocalResult{}, err
	}
	if result.Source != "local-authority" {
		return LocalResult{}, errors.New("local setup did not return the local authority")
	}
	data, ok := result.Data.(map[string]any)
	if !ok {
		return LocalResult{}, errors.New("invalid local workspace response")
	}
	id, _ := data["id"].(string)
	name, _ := data["name"].(string)
	owner, _ := data["owner_principal"].(string)
	if id == "" || name == "" || owner == "" {
		return LocalResult{}, errors.New("invalid local workspace identity response")
	}
	return LocalResult{WorkspaceID: id, Workspace: name, Principal: owner}, nil
}

// Join proves an online hosted identity and only then atomically writes trusted
// cloud routing. It never initializes or adopts a workspace.
func Join(ctx context.Context, in JoinInput, executor Executor) (JoinResult, error) {
	if err := ValidateJoinTarget(in.ConfigPath, in.Server, in.Principal); err != nil {
		return JoinResult{}, err
	}
	if err := validateJoinInput(in); err != nil {
		return JoinResult{}, err
	}
	if executor == nil {
		return JoinResult{}, errors.New("cloud executor is required")
	}
	wid, pid, err := executor.ResolveProject(ctx, in.Workspace, in.Project, in.GitRemote)
	if err != nil {
		return JoinResult{}, fmt.Errorf("online join resolution failed: %w", err)
	}
	if strings.TrimSpace(wid) == "" || strings.TrimSpace(pid) == "" {
		return JoinResult{}, errors.New("join requires nonempty immutable resolved workspace and project IDs; no routing written")
	}
	args := map[string]any{}
	if in.Instance != "" {
		args["instance"] = in.Instance
	}
	result, err := executor.Execute(ctx, bus.Request{
		Operation: "whoami", Workspace: wid, Project: pid, Session: in.Session, Args: args,
	})
	if err != nil {
		return JoinResult{}, fmt.Errorf("online join authorization failed: %w", err)
	}
	data, ok := result.Data.(map[string]any)
	if !ok || result.Source != "server" {
		return JoinResult{}, errors.New("join requires an authoritative online response; no routing written")
	}
	if boolean(data, "online") != true || boolean(data, "verified") != true ||
		text(data, "storage_mode") != "cloud" ||
		text(data, "identity_provenance") != "google-id-token" ||
		text(data, "principal") != strings.ToLower(in.Principal) {
		return JoinResult{}, errors.New("join identity was not verified by the hosted service; no routing written")
	}
	workspace, _ := data["workspace"].(map[string]any)
	project, _ := data["project"].(map[string]any)
	if text(workspace, "id") != wid || text(project, "id") != pid {
		return JoinResult{}, errors.New("join response did not preserve the resolved scope; no routing written")
	}
	if err := ctx.Err(); err != nil {
		return JoinResult{}, err
	}
	if err := ValidateJoinTarget(in.ConfigPath, in.Server, in.Principal); err != nil {
		return JoinResult{}, err
	}
	changed, err := writeCloudConfig(in.ConfigPath, wid, pid, in)
	if err != nil {
		return JoinResult{}, err
	}
	return JoinResult{ConfigPath: in.ConfigPath, Workspace: wid, Project: pid, Principal: strings.ToLower(in.Principal), Changed: changed}, nil
}

func validateJoinInput(in JoinInput) error {
	if !filepath.IsAbs(in.ConfigPath) {
		return errors.New("join requires an absolute trusted config path")
	}
	if strings.TrimSpace(in.Workspace) == "" {
		return errors.New("join requires a workspace")
	}
	if strings.TrimSpace(in.Project) == "" && strings.TrimSpace(in.GitRemote) == "" {
		return errors.New("join requires a project or registered git remote")
	}
	if (in.Session == "") != (in.Instance == "") {
		return errors.New("join session and instance must be supplied together")
	}
	u, err := url.Parse(in.Server)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("join requires a valid trusted server URL")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && (net.ParseIP(u.Hostname()) != nil && net.ParseIP(u.Hostname()).IsLoopback())) {
		return errors.New("join server requires HTTPS except on loopback")
	}
	if strings.TrimSpace(in.CredentialSource) == "" {
		return errors.New("join requires an explicit credential source")
	}
	if strings.TrimSpace(in.Cache) == "" {
		return errors.New("join requires an explicit mirror cache path")
	}
	if in.CredentialSource == "oauth" && strings.TrimSpace(in.OAuthClientSecret) == "" {
		return errors.New("join requires a complete active OAuth client pair; acquire the matching installed-app credential before joining")
	}
	if in.CredentialSource == "oauth" && (strings.TrimSpace(in.OAuthDeviceClientID) == "") != (strings.TrimSpace(in.OAuthDeviceClientSecret) == "") {
		return errors.New("join requires both device OAuth client ID and matching installed-app credential when either is configured")
	}
	if _, err = remote.ValidateRoute(remote.Config{
		URL: in.Server, Audience: in.Audience, Account: in.Account,
		ImpersonateServiceAccount: in.ImpersonateServiceAccount,
		Principal:                 in.Principal, CachePath: in.Cache,
		CredentialSource: in.CredentialSource, CredentialsFile: in.CredentialsFile,
		OAuthClientID: in.OAuthClientID, OAuthDeviceClientID: in.OAuthDeviceClientID,
		OAuthClientSecret: in.OAuthClientSecret, OAuthDeviceClientSecret: in.OAuthDeviceClientSecret,
		OAuthKeyringService: in.OAuthKeyringService,
	}); err != nil {
		return fmt.Errorf("join credential route: %w", err)
	}
	return nil
}

// privateConfigMode reports whether an existing config's mode is already the
// one a rewrite would set, so identical content needs no rewrite. Windows
// reports only the read-only bit (0666 or 0444), never 0600, so there the
// content comparison alone decides.
func privateConfigMode(goos string, perm os.FileMode) bool {
	return goos == "windows" || perm == 0600
}

func writeCloudConfig(path, workspace, project string, in JoinInput) (bool, error) {
	values := map[string]any{}
	info, err := os.Lstat(path)
	exists := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("cannot inspect trusted config: %w", err)
	}
	mode := os.FileMode(0600)
	var previous []byte
	if exists {
		if !info.Mode().IsRegular() {
			return false, errors.New("trusted config path must be a regular file")
		}
		if info.Size() > 4<<20 {
			return false, errors.New("trusted config exceeds 4 MiB")
		}
		raw, e := os.ReadFile(path)
		if e != nil {
			return false, fmt.Errorf("cannot read trusted config: %w", e)
		}
		if e = toml.Unmarshal(raw, &values); e != nil {
			return false, errors.New("trusted config is malformed; no routing written")
		}
		previous = raw
	}
	if old, _ := values["mode"].(string); old != "" && old != "cloud" {
		return false, errors.New("refusing to replace non-cloud trusted configuration")
	}
	if in.CredentialSource != "oauth" {
		in.OAuthClientID = ""
		in.OAuthClientSecret = ""
		in.OAuthDeviceClientID = ""
		in.OAuthDeviceClientSecret = ""
		in.OAuthKeyringService = ""
	}
	if in.CredentialSource == "oauth" {
		in.Account = ""
		in.ImpersonateServiceAccount = ""
		in.CredentialsFile = ""
	}
	if in.CredentialSource != "adc" {
		in.CredentialsFile = ""
	}
	updates := map[string]any{
		"mode": "cloud", "workspace": workspace, "project": project,
		"server": in.Server, "audience": in.Audience,
		"principal": strings.ToLower(in.Principal), "cache": in.Cache,
		"credential_source": in.CredentialSource,
	}
	for key, value := range map[string]string{
		"account": in.Account, "impersonate_service_account": in.ImpersonateServiceAccount,
		"credentials_file": in.CredentialsFile, "oauth_client_id": in.OAuthClientID,
		"oauth_device_client_id": in.OAuthDeviceClientID,
		"oauth_client_secret":    in.OAuthClientSecret, "oauth_device_client_secret": in.OAuthDeviceClientSecret,
		"oauth_keyring_service": in.OAuthKeyringService,
	} {
		if value != "" {
			updates[key] = value
		} else {
			delete(values, key)
		}
	}
	if in.Session != "" {
		updates["session"] = in.Session
		updates["instance"] = in.Instance
	} else if values["workspace"] != workspace || values["project"] != project {
		delete(values, "session")
		delete(values, "instance")
	}
	for key, value := range updates {
		values[key] = value
	}
	raw, err := toml.Marshal(values)
	if err != nil {
		return false, errors.New("cannot encode trusted config")
	}
	if exists && privateConfigMode(runtime.GOOS, info.Mode().Perm()) {
		var old map[string]any
		if toml.Unmarshal(previous, &old) == nil && reflect.DeepEqual(old, values) {
			return false, nil
		}
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return false, err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".clawbridge-join-*")
	if err != nil {
		return false, err
	}
	tmp := file.Name()
	defer os.Remove(tmp)
	if err = file.Chmod(mode); err != nil {
		file.Close()
		return false, err
	}
	if _, err = file.Write(raw); err != nil {
		file.Close()
		return false, err
	}
	if err = file.Sync(); err != nil {
		file.Close()
		return false, err
	}
	if err = file.Close(); err != nil {
		return false, err
	}
	if err = os.Rename(tmp, path); err != nil {
		return false, err
	}
	return true, nil
}

func text(m map[string]any, key string) string {
	value, _ := m[key].(string)
	return value
}

func boolean(m map[string]any, key string) bool {
	value, _ := m[key].(bool)
	return value
}

// SortedOperations is useful for stable human rendering without changing the
// execution order. Operations execute in dependency order as returned by BuildPlan.
func SortedOperations(operations []Operation) []Operation {
	out := append([]Operation{}, operations...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ValidateJoinTarget performs offline authority/config preflight before consent.
func ValidateJoinTarget(path, server, principal string) error {
	if !filepath.IsAbs(path) {
		return errors.New("join requires an absolute trusted config path")
	}
	u, err := url.Parse(server)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("join requires a valid trusted server URL")
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && !(u.Scheme == "http" && ip != nil && ip.IsLoopback()) {
		return errors.New("join requires HTTPS except numeric loopback")
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("cannot inspect trusted config")
	}
	if !info.Mode().IsRegular() || info.Size() > 4<<20 {
		return errors.New("trusted config must be a regular nonsymlink file within 4 MiB")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return errors.New("cannot read trusted config")
	}
	var values map[string]any
	if toml.Unmarshal(raw, &values) != nil {
		return errors.New("trusted config is malformed")
	}
	mode, _ := values["mode"].(string)
	if mode != "" && mode != "cloud" {
		return errors.New("join refuses non-cloud trusted configuration; select another client config")
	}
	old, _ := values["server"].(string)
	if old != "" && strings.TrimRight(old, "/") != strings.TrimRight(server, "/") {
		return errors.New("join refuses a different existing server; select another client config")
	}
	oldPrincipal, _ := values["principal"].(string)
	if principal != "" && oldPrincipal != "" && !strings.EqualFold(principal, oldPrincipal) {
		return errors.New("join refuses a conflicting pinned principal; select another client config")
	}
	return nil
}
