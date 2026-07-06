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

package services

import (
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

const readyPollInterval = 250 * time.Millisecond

// waitForReady blocks until the ready check passes or the timeout expires.
func waitForReady(check *api.ReadyCheck) error {
	timeout, err := time.ParseDuration(check.Timeout)
	if err != nil {
		return fmt.Errorf("invalid timeout %q: %w", check.Timeout, err)
	}

	switch check.Type {
	case "tcp":
		return waitForTCP(check.Target, timeout)
	case "http":
		return waitForHTTP(check.Target, timeout)
	case "delay":
		return waitForDelay(check.Target)
	default:
		return fmt.Errorf("unknown ready check type: %q", check.Type)
	}
}

func waitForTCP(target string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", target, readyPollInterval)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		time.Sleep(readyPollInterval)
	}
	return fmt.Errorf("tcp ready check timed out after %s: %s", timeout, target)
}

func waitForHTTP(target string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: readyPollInterval}
	for time.Now().Before(deadline) {
		resp, err := client.Get(target)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return nil
			}
		}
		time.Sleep(readyPollInterval)
	}
	return fmt.Errorf("http ready check timed out after %s: %s", timeout, target)
}

func waitForDelay(target string) error {
	d, err := time.ParseDuration(target)
	if err != nil {
		return fmt.Errorf("invalid delay duration %q: %w", target, err)
	}
	time.Sleep(d)
	return nil
}
