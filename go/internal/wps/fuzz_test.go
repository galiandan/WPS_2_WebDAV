package wps

import (
	"encoding/json"
	"testing"
)

// Fuzz target for 06-testing-risk-gates.md 10.1 entry 7/9: WPS JSON and
// checkpoint payloads. Invariants: no panic and no allocation proportional
// to an attacker-declared length.
func FuzzMultipartCheckpointJSON(f *testing.F) {
	seeds := []string{
		`{"upload_id": "u", "part_size": 1, "parts": []}`,
		`{"upload_id": 1}`,
		`{"parts": [{"index": 0, "etag": "e"}]}`,
		`{"part_size": -1, "upload_id": null}`,
		`[]`,
	}
	for _, seed := range seeds {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, payload []byte) {
		var decoded map[string]any
		if err := json.Unmarshal(payload, &decoded); err != nil {
			return
		}
		// The checkpoint reader consumes attacker-editable files (an
		// attacker-writable resume dir is out of threat model, but the
		// parser must still fail closed); exercise its full validation.
		_, _ = parseCheckpointPayload(decoded, "identity")
	})
}
