package server

import "testing"

type mappingUnsupported struct{}

func (mappingUnsupported) Error() string { return "mapping unavailable to this API version" }
func (mappingUnsupported) Code() string  { return "unsupported_mapping" }

func TestLinearV2UnsupportedMappingHTTPStatus(t *testing.T) {
	code, status := serviceError(mappingUnsupported{})
	if code != "unsupported_mapping" || status != 409 {
		t.Fatal("version refusal lost its definitive code", code, status)
	}
}
