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
func checkCreate(body []byte, want createSpec) ([]byte, error) {
	var got createSpec
	if err := decodeExact(body, &got); err != nil {
		return nil, err
	}
	if field := differs(reflect.ValueOf(got), reflect.ValueOf(want), ""); field != "" {
		return nil, fmt.Errorf("%s differs from the fixed spec", field)
	}
	return json.Marshal(got)
}

// checkNetwork is checkCreate for a network create body.
func checkNetwork(body []byte) ([]byte, string, error) {
	var got networkSpec
	if err := decodeExact(body, &got); err != nil {
		return nil, "", err
	}
	ref, ok := refFromName(got.Name, networkPrefix)
	if !ok {
		return nil, "", fmt.Errorf("network name is not cbnet-<owner ref>")
	}
	if field := differs(reflect.ValueOf(got), reflect.ValueOf(agentNetwork(ref)), ""); field != "" {
		return nil, "", fmt.Errorf("%s differs from the fixed spec", field)
	}
	out, err := json.Marshal(got)
	return out, ref, err
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
