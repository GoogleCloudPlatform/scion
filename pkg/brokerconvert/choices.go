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

package brokerconvert

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// choiceEntry is one entry of a choices file.
type choiceEntry struct {
	TargetGroup   string   `yaml:"target_group"`
	Field         string   `yaml:"field"`
	ChosenProfile string   `yaml:"chosen_profile"`
	Split         []string `yaml:"split"`
}

// ParseChoices parses a choices file: a YAML or JSON list of entries with
// target_group, field, chosen_profile and split. Only the file's shape is
// checked here; whether a choice applies is the converter's decision, which
// reports an invalid choice as a blocking finding.
func ParseChoices(data []byte) ([]config.ConversionChoice, error) {
	dec := yamlv3.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var entries []choiceEntry
	if err := dec.Decode(&entries); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		return nil, fmt.Errorf("choices: %w", err)
	}
	out := make([]config.ConversionChoice, 0, len(entries))
	for _, e := range entries {
		out = append(out, config.ConversionChoice{
			TargetGroup:   e.TargetGroup,
			Field:         e.Field,
			ChosenProfile: e.ChosenProfile,
			Split:         e.Split,
		})
	}
	return out, nil
}
