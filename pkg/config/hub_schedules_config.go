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
	// DefaultScheduleMinInterval is the default of
	// server.hub.schedules.min_interval. Five minutes: a dispatch_agent
	// schedule starts a whole agent on each fire, and the default launch
	// budget (server.hub.launch_timeout) is five minutes, so a shorter
	// interval can fire again before the previous launch has finished; a
	// wake-message schedule every minute keeps an agent from ever idling.
	// "*/5" is also the shortest step common polling crons use.
	DefaultScheduleMinInterval = 5 * time.Minute

	// ScheduleMinIntervalLimit is the largest accepted
	// server.hub.schedules.min_interval.
	ScheduleMinIntervalLimit = 24 * time.Hour

	// DefaultScheduleMaxActivePerProject is the default of
	// server.hub.schedules.max_active_per_project.
	DefaultScheduleMaxActivePerProject = 50

	// ScheduleMaxActivePerProjectLimit is the largest accepted
	// server.hub.schedules.max_active_per_project.
	ScheduleMaxActivePerProjectLimit = 10000
)

// HubSchedulesConfig holds the limits on recurring schedules
// (server.hub.schedules). Read at startup. Both limits apply to every kind
// of recurring schedule and every author.
type HubSchedulesConfig struct {
	// MinInterval is the shortest interval a recurring schedule's cron
	// expression may fire at, as a Go duration ("" =
	// DefaultScheduleMinInterval; "0s" turns the check off; at most
	// ScheduleMinIntervalLimit).
	MinInterval string `json:"minInterval,omitempty" yaml:"minInterval,omitempty" koanf:"minInterval"`

	// MaxActivePerProject caps the active recurring schedules of one project
	// (nil = DefaultScheduleMaxActivePerProject; 0 = no cap; at most
	// ScheduleMaxActivePerProjectLimit).
	MaxActivePerProject *int `json:"maxActivePerProject,omitempty" yaml:"maxActivePerProject,omitempty" koanf:"maxActivePerProject"`
}

// IsZero reports whether no schedule limit is configured.
func (c HubSchedulesConfig) IsZero() bool {
	return c.MinInterval == "" && c.MaxActivePerProject == nil
}

// MinIntervalDuration parses MinInterval ("" = DefaultScheduleMinInterval).
func (c HubSchedulesConfig) MinIntervalDuration() (time.Duration, error) {
	if c.MinInterval == "" {
		return DefaultScheduleMinInterval, nil
	}
	d, err := time.ParseDuration(c.MinInterval)
	if err != nil {
		return 0, fmt.Errorf("invalid server.hub.schedules.min_interval %q: %w", c.MinInterval, err)
	}
	if d < 0 || d > ScheduleMinIntervalLimit {
		return 0, fmt.Errorf("invalid server.hub.schedules.min_interval %q: must be between 0s and %s", c.MinInterval, ScheduleMinIntervalLimit)
	}
	return d, nil
}

// MaxActivePerProjectValue returns MaxActivePerProject
// (nil = DefaultScheduleMaxActivePerProject).
func (c HubSchedulesConfig) MaxActivePerProjectValue() (int, error) {
	if c.MaxActivePerProject == nil {
		return DefaultScheduleMaxActivePerProject, nil
	}
	n := *c.MaxActivePerProject
	if n < 0 || n > ScheduleMaxActivePerProjectLimit {
		return 0, fmt.Errorf("invalid server.hub.schedules.max_active_per_project %d: must be between 0 and %d", n, ScheduleMaxActivePerProjectLimit)
	}
	return n, nil
}

// Validate checks every schedule limit.
func (c HubSchedulesConfig) Validate() error {
	if _, err := c.MinIntervalDuration(); err != nil {
		return err
	}
	_, err := c.MaxActivePerProjectValue()
	return err
}
