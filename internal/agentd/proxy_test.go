package agentd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"testing"
)

func bufioReader(r io.Reader) *bufio.Reader { return bufio.NewReader(r) }

// specMap is agentSpec for ref as a generic JSON object, to edit.
func specMap(t *testing.T, ref string) map[string]any {
	t.Helper()
	b, err := json.Marshal(agentSpec(testImage, testServer, ref))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func hostCfg(m map[string]any) map[string]any { return m["HostConfig"].(map[string]any) }

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The create body is refused unless it is exactly the fixed spec [AG-M1]:
// each way out of the sandbox is refused by the proxy, and the daemon never
// sees the request.
func TestProxyRefusesCreatesOutsideTheSpec(t *testing.T) {
	s := newStack(t)
	create := "/containers/create?name=clawbridge-agent-" + refA
	for name, edit := range map[string]func(m map[string]any){
		"privileged":       func(m map[string]any) { hostCfg(m)["Privileged"] = true },
		"privileged, case": func(m map[string]any) { hostCfg(m)["privileged"] = true },
		"host bind":        func(m map[string]any) { hostCfg(m)["Binds"] = []string{"/:/host"} },
		"host mount": func(m map[string]any) {
			hostCfg(m)["Mounts"] = []any{map[string]any{"Type": "bind", "Source": "/", "Target": "/host"}}
		},
		"volume":           func(m map[string]any) { m["Volumes"] = map[string]any{"/data": map[string]any{}} },
		"host network":     func(m map[string]any) { hostCfg(m)["NetworkMode"] = "host" },
		"other network":    func(m map[string]any) { hostCfg(m)["NetworkMode"] = "cbnet-" + refB },
		"added capability": func(m map[string]any) { hostCfg(m)["CapAdd"] = []string{"SYS_ADMIN"} },
		"no cap drop":      func(m map[string]any) { hostCfg(m)["CapDrop"] = []string{} },
		"device":           func(m map[string]any) { hostCfg(m)["Devices"] = []any{map[string]any{"PathOnHost": "/dev/sda"}} },
		"host pid":         func(m map[string]any) { hostCfg(m)["PidMode"] = "host" },
		"seccomp off":      func(m map[string]any) { hostCfg(m)["SecurityOpt"] = []string{"seccomp=unconfined"} },
		"extra env":        func(m map[string]any) { m["Env"] = append(m["Env"].([]any), "NODE_OPTIONS=--require /tmp/x.js") },
		"other server":     func(m map[string]any) { m["Env"] = []string{"CLAWBRIDGE_SERVER_URL=https://elsewhere.example.test"} },
		"other digest":     func(m map[string]any) { m["Image"] = strings.Replace(testImage, "0123", "3210", 1) },
		"tag only":         func(m map[string]any) { m["Image"] = "ghcr.io/example/clawbridge-dispatcher-agent:latest" },
		"root user":        func(m map[string]any) { m["User"] = "root" },
		"command":          func(m map[string]any) { m["Cmd"] = []string{"sh"} },
		"entrypoint":       func(m map[string]any) { m["Entrypoint"] = []string{"sh"} },
		"published port":   func(m map[string]any) { hostCfg(m)["PortBindings"] = map[string]any{"80/tcp": []any{}} },
		"writable root":    func(m map[string]any) { hostCfg(m)["ReadonlyRootfs"] = false },
		"no memory limit":  func(m map[string]any) { delete(hostCfg(m), "Memory") },
		"swap":             func(m map[string]any) { hostCfg(m)["MemorySwap"] = -1 },
		"no pids limit":    func(m map[string]any) { delete(hostCfg(m), "PidsLimit") },
		"exec tmpfs":       func(m map[string]any) { hostCfg(m)["Tmpfs"] = map[string]any{"/tmp": "rw,exec"} },
		"restart always":   func(m map[string]any) { hostCfg(m)["RestartPolicy"] = map[string]any{"Name": "always"} },
		"no stdin":         func(m map[string]any) { m["OpenStdin"] = false },
		"no host config":   func(m map[string]any) { delete(m, "HostConfig") },
		"networking config": func(m map[string]any) {
			m["NetworkingConfig"] = map[string]any{"EndpointsConfig": map[string]any{"host": map[string]any{}}}
		},
	} {
		m := specMap(t, refA)
		edit(m)
		if got := s.send(t, http.MethodPost, create, mustJSON(t, m)); got != http.StatusForbidden {
			t.Errorf("%s: status %d", name, got)
		}
	}
	// The same through an API version prefix, and with a bad name or query.
	privileged := specMap(t, refA)
	hostCfg(privileged)["Privileged"] = true
	good := mustJSON(t, specMap(t, refA))
	for name, c := range map[string]struct {
		path string
		body []byte
	}{
		"versioned": {"/v1.47" + create, mustJSON(t, privileged)},
		// Prefixes other than /vN.N are not stripped, so they fail closed.
		"short version":   {"/v1" + create, good},
		"upper version":   {"/V1.47" + create, good},
		"two versions":    {"/v1.47/v1.47" + create, good},
		"version only":    {"/v1.47", good},
		"other name":      {"/containers/create?name=postgres", good},
		"ref mismatch":    {"/containers/create?name=clawbridge-agent-" + refB, good},
		"bad ref":         {"/containers/create?name=clawbridge-agent-P_AAAAAAAA", good},
		"no name":         {"/containers/create", good},
		"repeated name":   {create + "&name=postgres", good},
		"extra query":     {create + "&platform=linux/amd64", good},
		"trailing data":   {create, append(append([]byte{}, good...), []byte(`{"Privileged":true}`)...)},
		"not json":        {create, []byte("Image=x")},
		"too large":       {create, append(append([]byte{}, good[:len(good)-1]...), []byte(`,"x":"`+strings.Repeat("a", maxBody)+`"}`)...)},
		"duplicate image": {create, []byte(strings.TrimSuffix(string(good), "}") + `,"Image":"busybox"}`)},
	} {
		if got := s.send(t, http.MethodPost, c.path, c.body); got != http.StatusForbidden {
			t.Errorf("%s: status %d", name, got)
		}
	}
	// A chunked body is read and checked the same way, and a form-encoded
	// body can't add parameters dockerd would merge into the query.
	chunk := func(body string) string { return fmt.Sprintf("%x\r\n%s\r\n0\r\n\r\n", len(body), body) }
	agent := "/containers/clawbridge-agent-" + refA
	for name, raw := range map[string]string{
		"chunked privileged create": "POST " + create + " HTTP/1.1\r\nHost: docker\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n" + chunk(string(mustJSON(t, privileged))),
		"chunked start body":        "POST " + agent + "/start HTTP/1.1\r\nHost: docker\r\nTransfer-Encoding: chunked\r\n\r\n" + chunk(`{"Binds":["/:/host"]}`),
		"form-encoded stop":         "POST " + agent + "/stop HTTP/1.1\r\nHost: docker\r\nContent-Type: application/x-www-form-urlencoded\r\nContent-Length: 15\r\n\r\nt=0&signal=KILL",
	} {
		if got := s.sendRaw(t, raw); got != http.StatusForbidden {
			t.Errorf("%s: status %d", name, got)
		}
	}
	if seen := s.daemon.seen(); len(seen) != 0 {
		t.Fatalf("the daemon saw refused requests: %v", seen)
	}
	if strings.Contains(s.log.String(), "/:/host") || strings.Contains(s.log.String(), "elsewhere") {
		t.Fatalf("a refusal echoed the body: %s", s.log.String())
	}
}

// Only the endpoints agentd uses pass, on agent containers and networks.
func TestProxyRefusesOtherEndpoints(t *testing.T) {
	s := newStack(t)
	agent := "/containers/clawbridge-agent-" + refA
	internal := mustJSON(t, agentNetwork(refA, netip.MustParsePrefix("172.30.0.0/28")))
	open := agentNetwork(refA, netip.MustParsePrefix("172.30.0.0/28"))
	open.Internal = false
	for name, c := range map[string]struct {
		method, path string
		body         []byte
	}{
		"start another":    {"POST", "/containers/postgres/start", nil},
		"stop by id":       {"POST", "/containers/0123456789ab/stop", nil},
		"exec":             {"POST", agent + "/exec", []byte(`{"Cmd":["sh"]}`)},
		"logs":             {"GET", agent + "/logs?stdout=1", nil},
		"archive":          {"GET", agent + "/archive?path=/", nil},
		"update":           {"POST", agent + "/update", []byte(`{"Memory":0}`)},
		"attach stdout":    {"POST", agent + "/attach?stream=1&stdin=1&stdout=1", nil},
		"attach no stdin":  {"POST", agent + "/attach?stream=1", nil},
		"start with body":  {"POST", agent + "/start", []byte(`{"Binds":["/:/host"]}`)},
		"start query":      {"POST", agent + "/start?detachKeys=x", nil},
		"dot segment":      {"POST", agent + "/../postgres/start", nil},
		"escaped slash":    {"POST", "/containers/clawbridge-agent-" + refA + "%2F..%2Fpostgres/start", nil},
		"encoded name":     {"POST", "/containers/clawbridge-agent-p_aaaaaaa%61/start", nil},
		"double slash":     {"POST", "//containers/clawbridge-agent-" + refA + "/start", nil},
		"images":           {"POST", "/images/create?fromImage=busybox", nil},
		"build":            {"POST", "/build", nil},
		"info":             {"GET", "/info", nil},
		"volumes":          {"POST", "/volumes/create", []byte(`{}`)},
		"open network":     {"POST", "/networks/create", mustJSON(t, open)},
		"other network":    {"POST", "/networks/create", []byte(`{"Name":"bridge2","Driver":"bridge","Internal":true}`)},
		"network options":  {"POST", "/networks/create", []byte(strings.TrimSuffix(string(internal), "}") + `,"Options":{"x":"y"}}`)},
		"network connect":  {"POST", "/networks/cbnet-" + refA + "/connect", []byte(`{"Container":"postgres"}`)},
		"remove bridge":    {"DELETE", "/networks/bridge", nil},
		"list extra query": {"GET", "/containers/json?size=1", nil},
	} {
		if got := s.send(t, c.method, c.path, c.body); got != http.StatusForbidden {
			t.Errorf("%s: status %d", name, got)
		}
	}
	if seen := s.daemon.seen(); len(seen) != 0 {
		t.Fatalf("the daemon saw refused requests: %v", seen)
	}
}

// A spec that passes is forwarded in agentd's own encoding, not the bytes
// received.
func TestProxyForwardsTheCanonicalBody(t *testing.T) {
	s := newStack(t)
	good := mustJSON(t, agentSpec(testImage, testServer, refA))
	// A case variant, and a duplicate key the check resolves as the daemon
	// would: the daemon still gets only the canonical body.
	lower := strings.Replace(string(good), `{"Image":`, `{"Image":"busybox","image":`, 1)
	if got := s.send(t, http.MethodPost, "/v1.47/containers/create?name=clawbridge-agent-"+refA, []byte(lower)); got != http.StatusCreated {
		t.Fatalf("status %d: %s", got, s.log.String())
	}
	if got := string(s.daemon.bodies["clawbridge-agent-"+refA]); got != string(good) {
		t.Fatalf("forwarded %s", got)
	}
	if seen := s.daemon.seen(); !slices.Equal(seen, []string{"POST /v1.47/containers/create?name=clawbridge-agent-" + refA}) {
		t.Fatalf("%v", seen)
	}
}

// The egress container, the networks and the connects have fixed specs too:
// only exact bodies pass, and the egress proxy is never attached to.
func TestProxyEgressAndNetworks(t *testing.T) {
	s := newStack(t)
	egress := func(edit func(m map[string]any)) []byte {
		var m map[string]any
		if err := json.Unmarshal(mustJSON(t, egressSpec(testEgressImage, s.policyDir)), &m); err != nil {
			t.Fatal(err)
		}
		edit(m)
		return mustJSON(t, m)
	}
	network := func(subnet string, edit func(n *networkSpec)) []byte {
		n := agentNetwork(refA, netip.MustParsePrefix("172.30.0.0/28"))
		n.IPAM.Config[0].Subnet = subnet
		if edit != nil {
			edit(&n)
		}
		return mustJSON(t, n)
	}
	createEgress := "/containers/create?name=" + egressName
	for name, c := range map[string]struct {
		method, path string
		body         []byte
	}{
		"egress bind rw":     {"POST", createEgress, egress(func(m map[string]any) { hostCfg(m)["Binds"] = []string{s.policyDir + ":/policy"} })},
		"egress other bind":  {"POST", createEgress, egress(func(m map[string]any) { hostCfg(m)["Binds"] = []string{"/:/policy:ro"} })},
		"egress second bind": {"POST", createEgress, egress(func(m map[string]any) { hostCfg(m)["Binds"] = []string{s.policyDir + ":/policy:ro", "/etc:/etc:ro"} })},
		"egress shell":       {"POST", createEgress, egress(func(m map[string]any) { m["Cmd"] = []string{"sh"} })},
		"egress open policy": {"POST", createEgress, egress(func(m map[string]any) {
			m["Cmd"] = []string{"egress", "--listen", "0.0.0.0:3128", "--policy", "/tmp/x"}
		})},
		"egress root":              {"POST", createEgress, egress(func(m map[string]any) { m["User"] = "0" })},
		"egress env":               {"POST", createEgress, egress(func(m map[string]any) { m["Env"] = []string{"CLAWBRIDGE_LAUNCHER_TOKEN=x"} })},
		"egress privileged":        {"POST", createEgress, egress(func(m map[string]any) { hostCfg(m)["Privileged"] = true })},
		"egress host network":      {"POST", createEgress, egress(func(m map[string]any) { hostCfg(m)["NetworkMode"] = "host" })},
		"egress on owner net":      {"POST", createEgress, egress(func(m map[string]any) { hostCfg(m)["NetworkMode"] = networkName(refA) })},
		"egress agent image":       {"POST", createEgress, egress(func(m map[string]any) { m["Image"] = testImage })},
		"egress cap add":           {"POST", createEgress, egress(func(m map[string]any) { hostCfg(m)["CapAdd"] = []string{"NET_ADMIN"} })},
		"agent spec as egress":     {"POST", createEgress, mustJSON(t, agentSpec(testImage, testServer, refA))},
		"egress spec as agent":     {"POST", "/containers/create?name=" + containerName(refA), egress(func(map[string]any) {})},
		"attach egress":            {"POST", "/containers/" + egressName + "/attach?stream=1&stdin=1", nil},
		"misaligned subnet":        {"POST", "/networks/create", network("172.30.0.8/28", nil)},
		"wide subnet":              {"POST", "/networks/create", network("172.30.0.0/24", nil)},
		"subnet outside pool":      {"POST", "/networks/create", network("10.0.0.0/28", nil)},
		"egress subnet":            {"POST", "/networks/create", network("172.31.255.0/28", nil)},
		"unnormal subnet":          {"POST", "/networks/create", network("172.30.000.0/28", nil)},
		"two subnets":              {"POST", "/networks/create", network("172.30.0.0/28", func(n *networkSpec) { n.IPAM.Config = append(n.IPAM.Config, ipamConfig{Subnet: "172.30.0.16/28"}) })},
		"other IPAM driver":        {"POST", "/networks/create", network("172.30.0.0/28", func(n *networkSpec) { n.IPAM.Driver = "macvlan" })},
		"open owner network":       {"POST", "/networks/create", network("172.30.0.0/28", func(n *networkSpec) { n.Internal = false })},
		"internal egress net":      {"POST", "/networks/create", mustJSON(t, networkSpec{Name: egressNetName, Driver: "bridge", Internal: true, IPAM: egressNetSpec().IPAM})},
		"egress net, other subnet": {"POST", "/networks/create", mustJSON(t, networkSpec{Name: egressNetName, Driver: "bridge", IPAM: ipamSpec{Driver: "default", Config: []ipamConfig{{Subnet: "172.30.0.0/28"}}}})},
		"connect postgres":         {"POST", "/networks/" + networkName(refA) + "/connect", []byte(`{"Container":"postgres"}`)},
		"connect another agent":    {"POST", "/networks/" + networkName(refA) + "/connect", []byte(`{"Container":"` + containerName(refB) + `"}`)},
		"connect with config":      {"POST", "/networks/" + networkName(refA) + "/connect", []byte(`{"Container":"clawbridge-egress","EndpointConfig":{"IPAddress":"172.30.0.20"}}`)},
		"connect egress net":       {"POST", "/networks/" + egressNetName + "/connect", []byte(`{"Container":"clawbridge-agent-p_aaaaaaaa"}`)},
		"remove egress net":        {"DELETE", "/networks/" + egressNetName, nil},
		"inspect egress net":       {"GET", "/networks/" + egressNetName, nil},
		"network list verbose":     {"GET", "/networks?verbose=1", nil},
		"network prune":            {"POST", "/networks/prune", nil},
	} {
		if got := s.send(t, c.method, c.path, c.body); got != http.StatusForbidden {
			t.Errorf("%s: status %d", name, got)
		}
	}
	if seen := s.daemon.seen(); len(seen) != 0 {
		t.Fatalf("the daemon saw refused requests: %v", seen)
	}
	// The exact bodies pass, in the order agentd sends them.
	for _, c := range []struct {
		name, path string
		body       []byte
		want       int
	}{
		{"egress net", "/networks/create", mustJSON(t, egressNetSpec()), http.StatusCreated},
		{"egress", createEgress, egress(func(map[string]any) {}), http.StatusCreated},
		{"owner net", "/networks/create", network("172.30.255.240/28", nil), http.StatusCreated},
		{"connect", "/networks/" + networkName(refA) + "/connect", []byte(`{"Container":"clawbridge-egress"}`), http.StatusOK},
	} {
		if got := s.send(t, http.MethodPost, c.path, c.body); got != c.want {
			t.Errorf("%s: status %d", c.name, got)
		}
	}
}
