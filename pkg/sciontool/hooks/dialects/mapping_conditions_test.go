/*
Copyright 2026 The Scion Authors.
*/

package dialects

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeSpec(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dialect.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	return path
}

func TestMappingDialect_SkipWhen(t *testing.T) {
	md, err := LoadMappingDialect(writeSpec(t, `
dialect: t
event_name_field: name
mappings:
  Note:
    event: notification
    skip_when:
      - field: kind
        equals: error
      - field: .detail.level
        not_equals: user
`))
	require.NoError(t, err)

	tests := []struct {
		name string
		data map[string]interface{}
		want string
	}{
		{"equals holds", map[string]interface{}{"kind": "error", "detail": map[string]interface{}{"level": "user"}}, hooks.EventIgnored},
		{"not_equals holds on a missing field", map[string]interface{}{"kind": "question"}, hooks.EventIgnored},
		{"not_equals holds on a non-string", map[string]interface{}{"detail": map[string]interface{}{"level": 3}}, hooks.EventIgnored},
		{"no condition holds", map[string]interface{}{"kind": "question", "detail": map[string]interface{}{"level": "user"}}, hooks.EventNotification},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.data["name"] = "Note"
			event, err := md.Parse(tt.data)
			require.NoError(t, err)
			assert.Equal(t, tt.want, event.Name)
			assert.Equal(t, "Note", event.RawName)
		})
	}
}

func TestMappingDialect_Uncounted(t *testing.T) {
	md, err := LoadMappingDialect(writeSpec(t, `
dialect: t
event_name_field: name
mappings:
  Failed:
    event: model-end
    uncounted: true
  Done:
    event: model-end
`))
	require.NoError(t, err)

	failed, err := md.Parse(map[string]interface{}{"name": "Failed", "error": "boom"})
	require.NoError(t, err)
	assert.True(t, failed.Data.Uncounted)
	assert.Equal(t, "boom", failed.Data.Error)

	done, err := md.Parse(map[string]interface{}{"name": "Done"})
	require.NoError(t, err)
	assert.False(t, done.Data.Uncounted)
}

func TestLoadMappingDialect_RejectsInvalidSkipWhen(t *testing.T) {
	for name, cond := range map[string]string{
		"no field":        "      - equals: x",
		"no comparison":   "      - field: kind",
		"two comparisons": "      - field: kind\n        equals: a\n        not_equals: b",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := LoadMappingDialect(writeSpec(t, "dialect: t\nevent_name_field: name\nmappings:\n  Note:\n    event: notification\n    skip_when:\n"+cond+"\n"))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "skip_when")
		})
	}
}
