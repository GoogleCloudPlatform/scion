// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cmd

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseExecCaptureOutput(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", "", false},
		{"none", "", false},
		{"json", "json", false},
		{"tail", "tail", false},
		{"raw", "", true},
		{"JSON", "", true},
	} {
		got, err := parseExecCaptureOutput(tc.in)
		if tc.wantErr {
			assert.Error(t, err, "input %q", tc.in)
		} else {
			assert.NoError(t, err, "input %q", tc.in)
			assert.Equal(t, tc.want, got, "input %q", tc.in)
		}
	}
}

func TestWriteExecOutput(t *testing.T) {
	captureStdout := func(t *testing.T, fn func() error) string {
		t.Helper()
		orig := os.Stdout
		r, w, err := os.Pipe()
		require.NoError(t, err)
		os.Stdout = w
		defer func() { os.Stdout = orig }()
		err = fn()
		require.NoError(t, w.Close())
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		require.NoError(t, r.Close())
		require.NoError(t, err)
		return buf.String()
	}

	prevFormat := outputFormat
	t.Cleanup(func() { outputFormat = prevFormat })
	outputFormat = ""

	// Pretty-printed JSON mode.
	outJSON := captureStdout(t, func() error {
		return writeExecOutput(&hubclient.ExecOutput{
			CaptureMode:  "json",
			OutputStatus: "captured",
			Output:       json.RawMessage(`{"a":1}`),
		})
	})
	assert.Equal(t, "{\n  \"a\": 1\n}\n", outJSON)

	// Tail mode prints raw text.
	outTail := captureStdout(t, func() error {
		return writeExecOutput(&hubclient.ExecOutput{
			CaptureMode:  "tail",
			OutputStatus: "truncated",
			Output:       json.RawMessage(`"line1\nline2\n"`),
		})
	})
	assert.Equal(t, "line1\nline2\n", outTail)

	// No content prints status.
	outEmpty := captureStdout(t, func() error {
		return writeExecOutput(&hubclient.ExecOutput{
			CaptureMode:  "json",
			OutputStatus: "invalid_json",
		})
	})
	assert.Equal(t, "No captured output (status: invalid_json).\n", outEmpty)

	// --json emits raw envelope.
	outputFormat = "json"
	outRaw := captureStdout(t, func() error {
		return writeExecOutput(&hubclient.ExecOutput{
			CaptureMode:  "json",
			OutputStatus: "captured",
			Output:       json.RawMessage(`{"a":1}`),
		})
	})
	assert.Contains(t, outRaw, `"captureMode": "json"`)
	assert.Contains(t, outRaw, `"outputStatus": "captured"`)
}
