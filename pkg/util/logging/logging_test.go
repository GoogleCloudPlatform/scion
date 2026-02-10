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

package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGCPHandler(t *testing.T) {
	var buf bytes.Buffer
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	handler := NewGCPHandler(&buf, opts, "test-component")
	logger := slog.New(handler)

	logger.Info("test message", "key", "value")

	var data map[string]interface{}
	err := json.Unmarshal(buf.Bytes(), &data)
	assert.NoError(t, err)

	assert.Equal(t, "INFO", data[GCPKeySeverity])
	assert.Equal(t, "test message", data[GCPKeyMessage])
	assert.NotNil(t, data[GCPKeyTimestamp])
	assert.Equal(t, "value", data["key"])

	labels := data[GCPKeyLabels].(map[string]interface{})
	assert.Equal(t, "test-component", labels["component"])
	
	hostname, _ := os.Hostname()
	if hostname != "" {
		assert.Equal(t, hostname, labels["hostname"])
	}
}

func TestGCPHandlerSourceLocation(t *testing.T) {
	var buf bytes.Buffer
	opts := &slog.HandlerOptions{Level: slog.LevelInfo, AddSource: true}
	handler := NewGCPHandler(&buf, opts, "test-component")
	logger := slog.New(handler)

	logger.Info("test message")

	var data map[string]interface{}
	err := json.Unmarshal(buf.Bytes(), &data)
	assert.NoError(t, err)

	source := data[GCPKeySourceLocation].(map[string]interface{})
	assert.Contains(t, source["file"], "logging_test.go")
	assert.NotEmpty(t, source["line"])
	assert.Contains(t, source["function"], "TestGCPHandlerSourceLocation")
}
