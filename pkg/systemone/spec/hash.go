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

package spec

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/text/unicode/norm"
)

var paramIdentRE = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// HashContext supplies the rule-envelope and content-environment context folded
// into QuestionHash (provider-design §5.4, runtime-design §6.1).
type HashContext struct {
	Params     map[string]string
	Inputs     []string
	EnvName    string
	EnvVersion int
}

type canonicalEnvironment struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
}

type canonicalCriteria struct {
	False string `json:"false"`
	True  string `json:"true"`
}

type canonicalOption struct {
	Abstain     bool   `json:"abstain,omitempty"`
	Description string `json:"description"`
	ID          string `json:"id"`
}

type canonicalLevel struct {
	Description string `json:"description"`
	ID          string `json:"id"`
}

type canonicalQuestion struct {
	Criteria     *canonicalCriteria   `json:"criteria,omitempty"`
	Environment  canonicalEnvironment `json:"environment"`
	ExtraOptions []canonicalOption    `json:"extra_options,omitempty"`
	Inputs       []string             `json:"inputs"`
	Instructions string               `json:"instructions"`
	Levels       []canonicalLevel     `json:"levels,omitempty"`
	Options      []canonicalOption    `json:"options,omitempty"`
	OptionsFrom  string               `json:"options_from,omitempty"`
	Type         string               `json:"type"`
}

// QuestionHash computes the canonical "sha256:<hex>" digest for q in hctx
// (provider-design §5.4).
//
// Properties enforced:
//   - All strings (instructions, criteria, option/level IDs and descriptions,
//     params, inputs, and environment name) are normalized to Unicode NFC.
//   - Trusted {param} placeholders are substituted in a single pass prior to hashing.
//   - Object keys and the referenced input field set are sorted deterministically.
//   - Declared option, extra-option, and level arrays preserve exact declared order.
func QuestionHash(q Question, hctx HashContext) (string, error) {
	nfcParams, err := normalizeParams(hctx.Params)
	if err != nil {
		return "", err
	}

	instr, err := substituteParams(norm.NFC.String(q.Instructions), nfcParams, fmt.Sprintf("spec.questions[%s].instructions", q.ID))
	if err != nil {
		return "", err
	}

	var crit *canonicalCriteria
	if q.Criteria != nil && (q.Criteria.True != "" || q.Criteria.False != "") {
		tStr, err := substituteParams(norm.NFC.String(q.Criteria.True), nfcParams, fmt.Sprintf("spec.questions[%s].criteria.true", q.ID))
		if err != nil {
			return "", err
		}
		fStr, err := substituteParams(norm.NFC.String(q.Criteria.False), nfcParams, fmt.Sprintf("spec.questions[%s].criteria.false", q.ID))
		if err != nil {
			return "", err
		}
		crit = &canonicalCriteria{
			False: fStr,
			True:  tStr,
		}
	}

	inputs := canonicalizeStringSet(hctx.Inputs)

	var opts []canonicalOption
	if len(q.Options) > 0 {
		opts = make([]canonicalOption, len(q.Options))
		for i, o := range q.Options {
			opts[i] = canonicalOption{
				Abstain:     o.Abstain,
				Description: norm.NFC.String(o.Description),
				ID:          norm.NFC.String(o.ID),
			}
		}
	}

	var extraOpts []canonicalOption
	if len(q.ExtraOptions) > 0 {
		extraOpts = make([]canonicalOption, len(q.ExtraOptions))
		for i, o := range q.ExtraOptions {
			extraOpts[i] = canonicalOption{
				Abstain:     o.Abstain,
				Description: norm.NFC.String(o.Description),
				ID:          norm.NFC.String(o.ID),
			}
		}
	}

	var levels []canonicalLevel
	if len(q.Levels) > 0 {
		levels = make([]canonicalLevel, len(q.Levels))
		for i, l := range q.Levels {
			levels[i] = canonicalLevel{
				Description: norm.NFC.String(l.Description),
				ID:          norm.NFC.String(l.ID),
			}
		}
	}

	cq := canonicalQuestion{
		Criteria: crit,
		Environment: canonicalEnvironment{
			Name:    norm.NFC.String(hctx.EnvName),
			Version: hctx.EnvVersion,
		},
		ExtraOptions: extraOpts,
		Inputs:       inputs,
		Instructions: instr,
		Levels:       levels,
		Options:      opts,
		OptionsFrom:  norm.NFC.String(q.OptionsFrom),
		Type:         norm.NFC.String(q.Type),
	}

	encoded, err := marshalCanonicalJSON(cq)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func normalizeParams(params map[string]string) (map[string]string, error) {
	if len(params) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(params))
	for k, v := range params {
		nk := norm.NFC.String(k)
		if !paramIdentRE.MatchString(nk) {
			return nil, &CompileError{
				Path:     "params." + k,
				Category: CategoryInput,
				Message:  fmt.Sprintf("invalid parameter name %q", k),
			}
		}
		if _, dup := out[nk]; dup {
			return nil, &CompileError{
				Path:     "params." + nk,
				Category: CategoryInput,
				Message:  fmt.Sprintf("duplicate normalized parameter name %q", nk),
			}
		}
		out[nk] = norm.NFC.String(v)
	}
	return out, nil
}

// substituteParams performs single-pass substitution of {name} placeholders from
// trusted params. Substituted values are never recursively re-scanned.
func substituteParams(text string, nfcParams map[string]string, path string) (string, error) {
	if !strings.Contains(text, "{") {
		return text, nil
	}
	var b strings.Builder
	b.Grow(len(text))
	for i := 0; i < len(text); {
		open := strings.IndexByte(text[i:], '{')
		if open < 0 {
			b.WriteString(text[i:])
			break
		}
		b.WriteString(text[i : i+open])
		start := i + open + 1
		closeRel := strings.IndexByte(text[start:], '}')
		if closeRel < 0 {
			return "", &CompileError{
				Path:     path,
				Category: CategorySyntax,
				Message:  "unclosed '{' in parameter placeholder",
			}
		}
		token := text[start : start+closeRel]
		if !paramIdentRE.MatchString(token) {
			return "", &CompileError{
				Path:     path,
				Category: CategorySyntax,
				Message:  fmt.Sprintf("invalid parameter placeholder {%s}", token),
			}
		}
		val, ok := nfcParams[token]
		if !ok {
			return "", &CompileError{
				Path:     path,
				Category: CategoryInput,
				Message:  fmt.Sprintf("unresolved parameter {%s}", token),
			}
		}
		b.WriteString(val)
		i = start + closeRel + 1
	}
	return norm.NFC.String(b.String()), nil
}

func canonicalizeStringSet(items []string) []string {
	if len(items) == 0 {
		return []string{}
	}
	seen := make(map[string]struct{}, len(items))
	out := make([]string, 0, len(items))
	for _, s := range items {
		ns := norm.NFC.String(s)
		if _, ok := seen[ns]; ok {
			continue
		}
		seen[ns] = struct{}{}
		out = append(out, ns)
	}
	sort.Strings(out)
	return out
}

// marshalCanonicalJSON encodes v into compact UTF-8 JSON without HTML escaping
// and with struct/map object keys sorted lexicographically.
func marshalCanonicalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}
