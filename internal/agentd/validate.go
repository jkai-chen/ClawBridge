package agentd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// decodeExact decodes body into v, refusing unknown fields and trailing data.
// Unknown fields carry everything the spec leaves out (Privileged, Binds,
// Mounts, Volumes, CapAdd, Devices, PortBindings, Entrypoint, Cmd…), so they
// are refused by name.
func decodeExact(body []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if field, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
			return fmt.Errorf("field %s is not allowed", field)
		}
		return fmt.Errorf("the body is not the expected JSON: %w", err)
	}
	if dec.More() {
		return fmt.Errorf("the body has trailing data")
	}
	return nil
}

// checkCreate validates a container create body against the fixed spec for
// ref, deny by default, and returns the canonical body to forward. Forwarding
// agentd's own encoding of the decoded spec, not the bytes received, means the
// daemon never parses anything the check did not see (duplicate keys, case
// variants).
func checkCreate[T any](body []byte, want T) ([]byte, error) {
	var got T
	if err := decodeExact(body, &got); err != nil {
		return nil, err
	}
	if field := differs(reflect.ValueOf(got), reflect.ValueOf(want), ""); field != "" {
		return nil, fmt.Errorf("%s differs from the fixed spec", field)
	}
	return json.Marshal(got)
}

// checkNetwork is checkCreate for a network create body: the egress
// network exactly, or an owner's internal network on a /28 of the pool.
func checkNetwork(body []byte) ([]byte, error) {
	var got networkSpec
	if err := decodeExact(body, &got); err != nil {
		return nil, err
	}
	want := egressNetSpec()
	if got.Name != egressNetName {
		ref, ok := refFromName(got.Name, networkPrefix)
		if !ok {
			return nil, fmt.Errorf("network name is not cbnet-<owner ref>")
		}
		if len(got.IPAM.Config) != 1 {
			return nil, fmt.Errorf("IPAM.Config must name one subnet")
		}
		subnet, ok := validAgentSubnet(got.IPAM.Config[0].Subnet)
		if !ok {
			return nil, fmt.Errorf("the subnet is not a /28 of %s", subnetPool)
		}
		want = agentNetwork(ref, subnet)
	}
	if field := differs(reflect.ValueOf(got), reflect.ValueOf(want), ""); field != "" {
		return nil, fmt.Errorf("%s differs from the fixed spec", field)
	}
	return json.Marshal(got)
}

// differs names the first field where got and want differ ("" when equal),
// so a refusal says what was wrong without echoing the value.
func differs(got, want reflect.Value, path string) string {
	if got.Kind() == reflect.Struct {
		for i := range got.NumField() {
			name := got.Type().Field(i).Name
			if path != "" {
				name = path + "." + name
			}
			if f := differs(got.Field(i), want.Field(i), name); f != "" {
				return f
			}
		}
		return ""
	}
	if !reflect.DeepEqual(got.Interface(), want.Interface()) {
		return path
	}
	return ""
}
