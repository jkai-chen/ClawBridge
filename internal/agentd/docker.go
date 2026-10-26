package agentd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// docker is agentd's Docker client: the few calls it needs, through the
// proxy's socket. Errors carry the call and the status code, never a body.
type docker struct {
	socket string
	http   *http.Client
}

func newDocker(socket string) *docker {
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}
	return &docker{socket: socket, http: &http.Client{Transport: &http.Transport{DialContext: dial}, Timeout: 30 * time.Second}}
}

// agentContainer is one clawbridge-agent-* container as Docker lists it.
type agentContainer struct {
	ref     string
	running bool
	// revision is the config revision it was started for; 0 if unreadable.
	revision int64
}

func (d *docker) call(ctx context.Context, method, path string, body any, ok ...int) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+path, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("docker %s %s: unreachable", method, strings.SplitN(path, "?", 2)[0])
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 == 2 {
		return out, nil
	}
	for _, code := range ok {
		if resp.StatusCode == code {
			return out, nil
		}
	}
	return nil, &dockerError{call: method + " " + strings.SplitN(path, "?", 2)[0], status: resp.StatusCode}
}

type dockerError struct {
	call   string
	status int
}

func (e *dockerError) Error() string { return fmt.Sprintf("docker %s: status %d", e.call, e.status) }

// list returns the clawbridge-agent-* containers, running or not. Others are
// never returned, so never touched.
func (d *docker) list(ctx context.Context) ([]agentContainer, error) {
	filters := url.QueryEscape(`{"name":["` + containerPrefix + `"]}`)
	out, err := d.call(ctx, http.MethodGet, "/containers/json?all=1&filters="+filters, nil)
	if err != nil {
		return nil, err
	}
	var listed []struct {
		Names  []string
		State  string
		Labels map[string]string
	}
	if err := json.Unmarshal(out, &listed); err != nil {
		return nil, fmt.Errorf("docker list: unreadable answer")
	}
	var agents []agentContainer
	for _, c := range listed {
		for _, name := range c.Names {
			// Docker's name filter matches substrings: keep exact names only.
			if ref, ok := refFromName(strings.TrimPrefix(name, "/"), containerPrefix); ok {
				revision, _ := parseRevision(c.Labels[revisionLabel])
				agents = append(agents, agentContainer{ref: ref, running: c.State == "running", revision: revision})
			}
		}
	}
	return agents, nil
}

// listNetworks returns each owner network's subnet, by owner ref.
func (d *docker) listNetworks(ctx context.Context) (map[string]netip.Prefix, error) {
	filters := url.QueryEscape(`{"name":["` + networkPrefix + `"]}`)
	out, err := d.call(ctx, http.MethodGet, "/networks?filters="+filters, nil)
	if err != nil {
		return nil, err
	}
	var listed []networkSpec
	if err := json.Unmarshal(out, &listed); err != nil {
		return nil, fmt.Errorf("docker network list: unreadable answer")
	}
	subnets := map[string]netip.Prefix{}
	for _, n := range listed {
		ref, ok := refFromName(n.Name, networkPrefix)
		if !ok || len(n.IPAM.Config) != 1 {
			continue
		}
		if subnet, ok := validAgentSubnet(n.IPAM.Config[0].Subnet); ok {
			subnets[ref] = subnet
		}
	}
	return subnets, nil
}

func (d *docker) createNetwork(ctx context.Context, spec networkSpec) error {
	_, err := d.call(ctx, http.MethodPost, "/networks/create", spec, http.StatusConflict)
	return err
}

func (d *docker) removeNetwork(ctx context.Context, ref string) error {
	_, err := d.call(ctx, http.MethodDelete, "/networks/"+networkName(ref), nil, http.StatusNotFound)
	return err
}

// egressConnected reports whether the egress proxy is on ref's network.
func (d *docker) egressConnected(ctx context.Context, ref string) (bool, error) {
	out, err := d.call(ctx, http.MethodGet, "/networks/"+networkName(ref), nil)
	if err != nil {
		return false, err
	}
	var n struct {
		Containers map[string]struct{ Name string }
	}
	if err := json.Unmarshal(out, &n); err != nil {
		return false, fmt.Errorf("docker network inspect: unreadable answer")
	}
	for _, c := range n.Containers {
		if c.Name == egressName {
			return true, nil
		}
	}
	return false, nil
}

func (d *docker) connectEgress(ctx context.Context, ref string) error {
	_, err := d.call(ctx, http.MethodPost, "/networks/"+networkName(ref)+"/connect", connectSpec{Container: egressName})
	return err
}

func (d *docker) disconnectEgress(ctx context.Context, ref string) error {
	_, err := d.call(ctx, http.MethodPost, "/networks/"+networkName(ref)+"/disconnect", connectSpec{Container: egressName}, http.StatusNotFound)
	return err
}

// create creates the container name from spec.
func (d *docker) create(ctx context.Context, name string, spec any) error {
	_, err := d.call(ctx, http.MethodPost, "/containers/create?name="+name, spec)
	return err
}

// running reports whether the container exists and whether it runs.
func (d *docker) running(ctx context.Context, name string) (exists, running bool, err error) {
	out, err := d.call(ctx, http.MethodGet, "/containers/"+name+"/json", nil, http.StatusNotFound)
	if err != nil {
		return false, false, err
	}
	var c struct{ State *struct{ Running bool } }
	if json.Unmarshal(out, &c) != nil || c.State == nil {
		return false, false, nil
	}
	return true, c.State.Running, nil
}

func (d *docker) start(ctx context.Context, name string) error {
	_, err := d.call(ctx, http.MethodPost, "/containers/"+name+"/start", nil, http.StatusNotModified)
	return err
}

func (d *docker) stop(ctx context.Context, name string) error {
	_, err := d.call(ctx, http.MethodPost, "/containers/"+name+"/stop?t=10", nil, http.StatusNotModified, http.StatusNotFound)
	return err
}

func (d *docker) remove(ctx context.Context, name string) error {
	_, err := d.call(ctx, http.MethodDelete, "/containers/"+name+"?force=1&v=1", nil, http.StatusNotFound)
	return err
}

// attach opens the container's stdin, before it starts. send writes the
// credential and closes stdin, so the agent reads it once and sees EOF.
func (d *docker) attach(ctx context.Context, ref string) (send func(string) error, err error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", d.socket)
	if err != nil {
		return nil, fmt.Errorf("docker attach: unreachable")
	}
	fail := func(err error) (func(string) error, error) {
		conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	req := "POST /containers/" + containerName(ref) + "/attach?stream=1&stdin=1 HTTP/1.1\r\n" +
		"Host: docker\r\nConnection: Upgrade\r\nUpgrade: tcp\r\nContent-Length: 0\r\n\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
		return fail(fmt.Errorf("docker attach: unreachable"))
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return fail(fmt.Errorf("docker attach: unreadable answer"))
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols && resp.StatusCode != http.StatusOK {
		return fail(&dockerError{call: "POST /containers/" + containerName(ref) + "/attach", status: resp.StatusCode})
	}
	return func(credential string) error {
		defer conn.Close()
		if _, err := io.WriteString(conn, credential+"\n"); err != nil {
			return fmt.Errorf("docker attach: the credential could not be written")
		}
		if c, ok := conn.(interface{ CloseWrite() error }); ok {
			_ = c.CloseWrite()
		}
		return nil
	}, nil
}
