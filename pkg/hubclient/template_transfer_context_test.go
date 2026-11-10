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

package hubclient

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTemplateUpload_CanceledContext(t *testing.T) {
	for _, cancelBefore := range []bool{false, true} {
		name := "active"
		if cancelBefore {
			name = "canceled"
		}
		t.Run(name, func(t *testing.T) {
			client, err := New("http://hub.invalid")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if cancelBefore {
				cancel()
			}
			dest := filepath.Join(t.TempDir(), "template.txt")
			if err := os.WriteFile(dest, []byte("keep"), 0600); err != nil {
				t.Fatal(err)
			}
			err = client.Templates().UploadFile(ctx, "file://"+dest, "", nil, strings.NewReader("replacement"))
			want := "replacement"
			if cancelBefore {
				want = "keep"
				if !errors.Is(err, context.Canceled) {
					t.Errorf("error = %v, want context.Canceled", err)
				}
			} else if err != nil {
				t.Errorf("active upload failed: %v", err)
			}
			if got, err := os.ReadFile(dest); err != nil || string(got) != want {
				t.Errorf("template = %q, %v; want %q", got, err, want)
			}
		})
	}
}
