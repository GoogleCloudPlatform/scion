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

//go:build !no_sqlite

package artifacts

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"testing"
	"time"
)

// TestParseViewCapabilityRefusesPortViewLinks: a hub port page view link
// (ptone/scion#5569), in its own layout and in view capability layouts,
// signed with the artifact view key itself, is never accepted as a view
// capability.
func TestParseViewCapabilityRefusesPortViewLinks(t *testing.T) {
	const (
		agentID = "0b6f2c1e-4a8d-4f5e-9c3b-2d1e0f9a8b7c"
		userID  = "7d3e9f10-2b4c-4d6e-8f01-a2b3c4d5e6f7"
	)
	now := time.Now()
	e := strconv.FormatInt(now.Add(10*time.Minute).Unix(), 10)
	sign := func(msg string) string {
		m := hmac.New(sha256.New, testViewKey)
		m.Write([]byte(msg))
		return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
	}
	portViewMsg := "port-view\n" + agentID + "\n8080\n" + userID + "\n" + e
	for name, c := range map[string]string{
		"page link layout":         "v1." + agentID + ".8080." + userID + "." + e + "." + sign(portViewMsg),
		"view layout, page sig":    agentID + ".8080." + e + "." + sign(portViewMsg),
		"link layout, page sig":    agentID + ".8080." + e + "." + userID + "." + sign(portViewMsg),
		"view layout, page domain": agentID + ".1." + e + "." + sign("port-view\n"+agentID+"\n1\n"+e),
	} {
		if _, err := parseViewCapability(LegacyViewKeys(testViewKey), c, now); err == nil {
			t.Errorf("%s: %q accepted as a view capability", name, c)
		}
	}
}
