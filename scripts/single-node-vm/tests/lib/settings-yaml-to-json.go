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

//go:build ignore

// settings-yaml-to-json reads a settings.yaml document on stdin, parses
// it with the same koanf YAML parser the hub uses to load its settings
// file (pkg/config/koanf.go), and prints the result as JSON on stdout.
// The deploy-script tests use it to check that the settings deploy.sh
// writes are valid YAML with the expected structure, not just the
// expected substrings. Run it from the repository root:
//
//	go run scripts/single-node-vm/tests/lib/settings-yaml-to-json.go < settings.yaml
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/knadh/koanf/parsers/yaml"
)

func main() {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read stdin:", err)
		os.Exit(1)
	}
	parsed, err := yaml.Parser().Unmarshal(data)
	if err != nil {
		fmt.Fprintln(os.Stderr, "parse settings YAML:", err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(parsed); err != nil {
		fmt.Fprintln(os.Stderr, "encode JSON:", err)
		os.Exit(1)
	}
}
