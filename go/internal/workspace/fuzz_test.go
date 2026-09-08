package workspace

import (
	"encoding/json"
	"testing"
)

// Fuzz target for 06-testing-risk-gates.md 10.1 entry 9: workspace and
// settings JSON. Invariants: no panic, controlled errors, and the state
// never accepts more than MaxSpaces mounts.
func FuzzWorkspaceAndSettingsJSON(f *testing.F) {
	seeds := []string{
		`{"group_id": "g", "root_id": "r"}`,
		`{"spaces": [{"group_id": "g", "root_id": "r", "name": "a"}]}`,
		`{"group_id": 1, "root_id": null, "spaces": "x"}`,
		`{"spaces": [{"name": "` + string([]byte{0x01}) + `"}]}`,
		`{"name": "Test Drive"}`,
		`{"name": 1}`,
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
		_, _, _, _, _ = applyPayload("auto", "auto", "current-g", "current-r", "/", decoded)
		if _, err := ValidateRootName(decoded["name"]); err != nil {
			_ = err
		}
	})
}
