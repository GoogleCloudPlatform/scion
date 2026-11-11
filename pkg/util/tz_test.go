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

package util

import (
	"testing"
	"time"
)

func TestPinProcessUTC(t *testing.T) {
	orig := time.Local
	defer func() { time.Local = orig }()

	// Simulate a non-UTC host zone, as if TZ=Asia/Tokyo were set.
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatalf("loading Asia/Tokyo: %v", err)
	}
	time.Local = tokyo

	PinProcessUTC()

	if time.Local != time.UTC {
		t.Fatalf("time.Local = %v, want time.UTC", time.Local)
	}

	// A bare time.Now() formatted with a local-zone layout must now render in
	// UTC, matching what ent's Default(time.Now) and slog would produce.
	now := time.Now()
	if now.Location() != time.UTC {
		t.Fatalf("time.Now().Location() = %v, want UTC", now.Location())
	}
}
