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
	"testing"
)

func TestHubSchedulesConfig(t *testing.T) {
	d, err := HubSchedulesConfig{}.MinIntervalDuration()
	if err != nil || d != DefaultScheduleMinInterval {
		t.Fatalf("default min interval = %s, %v", d, err)
	}
	if DefaultScheduleMaxActivePerProject != 100 {
		t.Fatalf("DefaultScheduleMaxActivePerProject = %d, want 100", DefaultScheduleMaxActivePerProject)
	}
	n, err := HubSchedulesConfig{}.MaxActivePerProjectValue()
	if err != nil || n != DefaultScheduleMaxActivePerProject {
		t.Fatalf("default max active = %d, %v", n, err)
	}
	zero := 0
	if n, err := (HubSchedulesConfig{MaxActivePerProject: &zero}).MaxActivePerProjectValue(); err != nil || n != 0 {
		t.Fatalf("zero max active = %d, %v (want 0, nil)", n, err)
	}
	for _, ok := range []string{"0s", "1m", "15m", "24h"} {
		if err := (HubSchedulesConfig{MinInterval: ok}).Validate(); err != nil {
			t.Errorf("min_interval %q: %v", ok, err)
		}
	}
	for _, bad := range []string{"-1m", "25h", "5", "soon"} {
		if err := (HubSchedulesConfig{MinInterval: bad}).Validate(); err == nil {
			t.Errorf("min_interval %q accepted", bad)
		}
	}
	for _, v := range []int{0, 1, 10000} {
		v := v
		if err := (HubSchedulesConfig{MaxActivePerProject: &v}).Validate(); err != nil {
			t.Errorf("max_active_per_project %d: %v", v, err)
		}
	}
	for _, v := range []int{-1, 10001} {
		v := v
		if err := (HubSchedulesConfig{MaxActivePerProject: &v}).Validate(); err == nil {
			t.Errorf("max_active_per_project %d accepted", v)
		}
	}
	if d, _ := (HubSchedulesConfig{MinInterval: "0s"}).MinIntervalDuration(); d != 0 {
		t.Errorf("0s = %s, want 0", d)
	}
}

func TestHubSchedulesConfig_V1RoundTrip(t *testing.T) {
	zero := 0
	gc := ConvertV1ServerToGlobalConfig(&V1ServerConfig{Hub: &V1ServerHubConfig{
		Schedules: &V1ServerHubSchedulesConfig{MinInterval: "15m", MaxActivePerProject: &zero},
	}})
	if got := gc.Hub.Schedules; got.MinInterval != "15m" || got.MaxActivePerProject == nil || *got.MaxActivePerProject != 0 {
		t.Fatalf("Hub.Schedules = %+v", got)
	}
	v1 := ConvertGlobalToV1ServerConfig(gc)
	sc := v1.Hub.Schedules
	if sc == nil || sc.MinInterval != "15m" || sc.MaxActivePerProject == nil || *sc.MaxActivePerProject != 0 {
		t.Fatalf("round trip lost server.hub.schedules: %+v", sc)
	}
}
