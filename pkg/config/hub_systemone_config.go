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

package config

import (
	"fmt"
	"time"
)

const (
	// SystemOneDefaultMaxEvalTime is the default wall-clock budget for
	// evaluating System One rules on a single message
	// (server.hub.system_one.max_eval_time / hub.systemOne.maxEvalTime).
	SystemOneDefaultMaxEvalTime = 3 * time.Second

	// SystemOneMaxEvalTimeLimit is the hard upper bound on
	// server.hub.system_one.max_eval_time.
	SystemOneMaxEvalTimeLimit = 30 * time.Second
)

// HubSystemOneConfig holds Hub-wide System One rule evaluation settings
// (server.hub.system_one). Values are kept as configured and checked by
// Validate.
type HubSystemOneConfig struct {
	// MaxEvalTime bounds the wall-clock time spent evaluating System One
	// rules for a single message ("" = SystemOneDefaultMaxEvalTime, maximum
	// SystemOneMaxEvalTimeLimit).
	MaxEvalTime string `json:"maxEvalTime,omitempty" yaml:"maxEvalTime,omitempty" koanf:"maxEvalTime"`
}

// IsZero reports whether no System One hub setting is configured.
func (c HubSystemOneConfig) IsZero() bool {
	return c.MaxEvalTime == ""
}

// MaxEvalTimeDuration parses MaxEvalTime ("" = SystemOneDefaultMaxEvalTime).
// Values <= 0 or > SystemOneMaxEvalTimeLimit are rejected.
func (c HubSystemOneConfig) MaxEvalTimeDuration() (time.Duration, error) {
	if c.MaxEvalTime == "" {
		return SystemOneDefaultMaxEvalTime, nil
	}
	d, err := time.ParseDuration(c.MaxEvalTime)
	if err != nil {
		return 0, fmt.Errorf("invalid server.hub.system_one.max_eval_time %q: %w", c.MaxEvalTime, err)
	}
	if d <= 0 || d > SystemOneMaxEvalTimeLimit {
		return 0, fmt.Errorf("invalid server.hub.system_one.max_eval_time %q: must be > 0 and <= %s", c.MaxEvalTime, SystemOneMaxEvalTimeLimit)
	}
	return d, nil
}

// Validate checks every System One hub setting.
func (c HubSystemOneConfig) Validate() error {
	_, err := c.MaxEvalTimeDuration()
	return err
}
