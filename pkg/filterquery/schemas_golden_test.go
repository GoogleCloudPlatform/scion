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

package filterquery_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/filterquery/schemas"
)

var updateGolden = flag.Bool("update", false, "regenerate web/src/shared/filter-query/schemas.generated.json")

func TestSchemasGoldenExport(t *testing.T) {
	goldenPath := filepath.Join("..", "..", "web", "src", "shared", "filter-query", "schemas.generated.json")

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(schemas.All()); err != nil {
		t.Fatalf("encode schemas: %v", err)
	}
	want := buf.Bytes()

	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatalf("mkdir golden dir: %v", err)
		}
		if err := os.WriteFile(goldenPath, want, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}

	got, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read %s: %v (run: go test ./pkg/filterquery -update)", goldenPath, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s is out of date; run: go test ./pkg/filterquery -update", goldenPath)
	}
}
