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

package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestStandaloneMode_EnvDetection(t *testing.T) {
	// TELEGRAM_STANDALONE=true should trigger standalone mode.
	t.Setenv("TELEGRAM_STANDALONE", "true")
	assert.True(t, isStandaloneRequested())
}

func TestStandaloneMode_LegacyEnvDetection(t *testing.T) {
	t.Setenv("SCION_TELEGRAM_STANDALONE", "1")
	assert.True(t, isStandaloneRequested())
}

func TestStandaloneMode_NotSet(t *testing.T) {
	t.Setenv("TELEGRAM_STANDALONE", "")
	t.Setenv("SCION_TELEGRAM_STANDALONE", "")
	assert.False(t, isStandaloneRequested())
}

func TestPollModeRejection(t *testing.T) {
	cfg := map[string]string{
		"inbound_mode": "poll",
	}
	err := validateStandaloneConfig(cfg)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "webhook mode")
}

func TestPollModeRejection_CaseInsensitive(t *testing.T) {
	cfg := map[string]string{
		"inbound_mode": "Poll",
	}
	err := validateStandaloneConfig(cfg)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "webhook mode")
}

func TestWebhookModeAccepted(t *testing.T) {
	cfg := map[string]string{
		"inbound_mode": "webhook",
	}
	err := validateStandaloneConfig(cfg)
	assert.NoError(t, err)
}

func TestEmptyModeAccepted(t *testing.T) {
	cfg := map[string]string{}
	err := validateStandaloneConfig(cfg)
	assert.NoError(t, err)
}

// isStandaloneRequested checks env vars for standalone mode (extracted for testing).
func isStandaloneRequested() bool {
	return strings.EqualFold(envOrEmpty("TELEGRAM_STANDALONE"), "true") ||
		envOrEmpty("SCION_TELEGRAM_STANDALONE") == "1"
}

// validateStandaloneConfig checks that a merged config map does not contain
// inbound_mode=poll (design decision D8: HA Telegram is webhook-only).
func validateStandaloneConfig(cfg map[string]string) error {
	if v, ok := cfg["inbound_mode"]; ok && strings.EqualFold(v, "poll") {
		return errPollRejected
	}
	return nil
}
