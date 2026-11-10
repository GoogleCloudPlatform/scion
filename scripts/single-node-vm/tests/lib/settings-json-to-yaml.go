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

// settings-json-to-yaml reads a JSON document on stdin and writes it as
// YAML with gopkg.in/yaml.v3's default encoder (4-space indent, unquoted
// scalars), the encoder the hub uses when it rewrites settings.yaml. The
// deploy-script tests use it to build Go-written settings fixtures. Run it
// from the repository root:
//
//	go run scripts/single-node-vm/tests/lib/settings-json-to-yaml.go < settings.json
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

func main() {
	var doc map[string]any
	if err := json.NewDecoder(os.Stdin).Decode(&doc); err != nil {
		fmt.Fprintln(os.Stderr, "decode JSON:", err)
		os.Exit(1)
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		fmt.Fprintln(os.Stderr, "encode YAML:", err)
		os.Exit(1)
	}
	os.Stdout.Write(out)
}
