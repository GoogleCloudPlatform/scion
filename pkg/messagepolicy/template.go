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

package messagepolicy

import (
	"fmt"
	"strconv"
	"strings"
)

// TemplateData holds the content-free values permitted in reject_reason and
// reject_remediation templates (consumer design §5.3). Message body text is
// intentionally excluded so rendered rejections never echo untrusted body text.
type TemplateData struct {
	Policy         string
	Recipient      string
	JudgmentFlags  map[string]bool
	JudgmentLabels map[string]string
}

func isPlaceholderChar(b byte) bool {
	return (b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9') ||
		b == '_' || b == '.'
}

func isJudgmentIdent(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		b := s[i]
		ok := (b >= 'a' && b <= 'z') ||
			(b >= 'A' && b <= 'Z') ||
			(b >= '0' && b <= '9') ||
			b == '_'
		if !ok {
			return false
		}
	}
	return true
}

func isAllowedPlaceholder(token string) bool {
	if token == "policy" || token == "recipient" {
		return true
	}
	if name, ok := strings.CutPrefix(token, "judgment."); ok {
		return isJudgmentIdent(name)
	}
	return false
}

// ValidateTemplate checks that tmpl is within MaxTemplateBytes (1 KiB) and uses
// only supported content-free placeholders: {policy}, {recipient}, and
// {judgment.<name>}. Non-identifier braces (such as JSON examples `{"a": 1}`)
// are ignored.
func ValidateTemplate(field, tmpl string) error {
	if len(tmpl) > MaxTemplateBytes {
		return &ValidationError{
			Code:    ErrCodeValidation,
			Field:   field,
			Message: fmt.Sprintf("%s exceeds %d bytes", field, MaxTemplateBytes),
		}
	}
	for i := 0; i < len(tmpl); i++ {
		if tmpl[i] != '{' {
			continue
		}
		end := strings.IndexByte(tmpl[i+1:], '}')
		if end <= 0 {
			continue
		}
		token := tmpl[i+1 : i+1+end]
		allIdent := true
		for j := 0; j < len(token); j++ {
			if !isPlaceholderChar(token[j]) {
				allIdent = false
				break
			}
		}
		if !allIdent {
			continue
		}
		if !isAllowedPlaceholder(token) {
			return &ValidationError{
				Code:    ErrCodeValidation,
				Field:   field,
				Message: fmt.Sprintf("unsupported template placeholder {%s}; allowed placeholders are {policy}, {recipient}, and {judgment.<name>}", token),
			}
		}
		i += end + 1
	}
	return nil
}

// RenderTemplate substitutes {policy}, {recipient}, and {judgment.<name>}
// placeholders in tmpl using data. Non-placeholder braces (such as inline JSON
// snippets) are preserved verbatim.
func RenderTemplate(tmpl string, data TemplateData) string {
	if tmpl == "" || !strings.Contains(tmpl, "{") {
		return tmpl
	}
	var b strings.Builder
	b.Grow(len(tmpl))
	for i := 0; i < len(tmpl); i++ {
		if tmpl[i] != '{' {
			b.WriteByte(tmpl[i])
			continue
		}
		end := strings.IndexByte(tmpl[i+1:], '}')
		if end <= 0 {
			b.WriteByte(tmpl[i])
			continue
		}
		token := tmpl[i+1 : i+1+end]
		switch {
		case token == "policy":
			b.WriteString(data.Policy)
			i += end + 1
		case token == "recipient":
			b.WriteString(data.Recipient)
			i += end + 1
		case strings.HasPrefix(token, "judgment."):
			name := token[len("judgment."):]
			if isJudgmentIdent(name) {
				if lbl, ok := data.JudgmentLabels[name]; ok {
					b.WriteString(lbl)
				} else if flag, ok := data.JudgmentFlags[name]; ok {
					b.WriteString(strconv.FormatBool(flag))
				}
				i += end + 1
			} else {
				b.WriteByte(tmpl[i])
			}
		default:
			b.WriteByte(tmpl[i])
		}
	}
	return b.String()
}

// DefaultRejectReason returns a content-free fallback rejection message when a
// policy leaves reject_reason empty.
func DefaultRejectReason(policyName string) string {
	if policyName == "" {
		return "Message rejected by project message policy."
	}
	return fmt.Sprintf("Message rejected by policy %q.", policyName)
}
