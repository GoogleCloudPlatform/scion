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
	"bytes"
	"fmt"
	"strings"
)

// gitConfigVar is one variable read from a git config file.
type gitConfigVar struct {
	// Section and Name are lower-cased. Subsection keeps its case, except
	// in the legacy [section.subsection] form, which git lower-cases.
	Section    string
	Subsection string
	Name       string
	Value      string
	// HasSubsection distinguishes [section "sub"] (true, even when sub is
	// empty — [section ""]) from [section] (false). git treats these as
	// different sections, so an empty quoted subsection is not the same as
	// no subsection.
	HasSubsection bool
	Line          int
}

// Key returns the variable's canonical git key: section.name with no
// subsection, section.subsection.name with one. An empty subsection keeps
// both dots (section..name), matching how `git config --list` renders it, so
// the cross-check against git compares like for like.
func (v gitConfigVar) Key() string {
	if !v.HasSubsection {
		return v.Section + "." + v.Name
	}
	return v.Section + "." + v.Subsection + "." + v.Name
}

// parseGitConfigFile parses git config file syntax without running git.
// It is strict: input it does not fully understand is an error, so callers
// can refuse instead of guessing.
func parseGitConfigFile(data []byte) ([]gitConfigVar, error) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	p := &gitConfigParser{src: string(data), line: 1}
	return p.parse()
}

type gitConfigParser struct {
	src        string
	pos        int
	line       int
	section    string
	subsection string
	hasSub     bool
	inSection  bool
	vars       []gitConfigVar
}

func (p *gitConfigParser) errorf(format string, args ...any) error {
	return fmt.Errorf("git config line %d: %s", p.line, fmt.Sprintf(format, args...))
}

func (p *gitConfigParser) eof() bool { return p.pos >= len(p.src) }

func (p *gitConfigParser) peek() byte { return p.src[p.pos] }

func isConfigSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\r' }

func isConfigAlpha(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

func isConfigNameChar(c byte) bool { return isConfigAlpha(c) || (c >= '0' && c <= '9') || c == '-' }

func (p *gitConfigParser) skipSpace() {
	for !p.eof() && isConfigSpace(p.peek()) {
		p.pos++
	}
}

// skipComment consumes a comment up to (not including) the newline.
func (p *gitConfigParser) skipComment() {
	for !p.eof() && p.peek() != '\n' {
		p.pos++
	}
}

// endOfLine expects only spaces and an optional comment before the newline.
func (p *gitConfigParser) endOfLine() error {
	p.skipSpace()
	if p.eof() {
		return nil
	}
	switch p.peek() {
	case '\n':
		p.pos++
		p.line++
		return nil
	case '#', ';':
		p.skipComment()
		return nil
	}
	return p.errorf("unexpected %q", p.peek())
}

func (p *gitConfigParser) parse() ([]gitConfigVar, error) {
	for !p.eof() {
		p.skipSpace()
		if p.eof() {
			break
		}
		switch c := p.peek(); {
		case c == '\n':
			p.pos++
			p.line++
		case c == '#' || c == ';':
			p.skipComment()
		case c == '[':
			if err := p.parseSection(); err != nil {
				return nil, err
			}
			// A variable may follow the header on the same line.
			p.skipSpace()
			if !p.eof() && isConfigAlpha(p.peek()) {
				if err := p.parseVariable(); err != nil {
					return nil, err
				}
			} else if err := p.endOfLine(); err != nil {
				return nil, err
			}
		case isConfigAlpha(c):
			if err := p.parseVariable(); err != nil {
				return nil, err
			}
		default:
			return nil, p.errorf("unexpected %q", c)
		}
	}
	return p.vars, nil
}

func (p *gitConfigParser) parseSection() error {
	p.pos++ // '['
	start := p.pos
	for !p.eof() && (isConfigNameChar(p.peek()) || p.peek() == '.') {
		p.pos++
	}
	name := p.src[start:p.pos]
	if name == "" {
		return p.errorf("empty section name")
	}
	if p.eof() {
		return p.errorf("unterminated section header")
	}
	switch p.peek() {
	case ']':
		p.pos++
		if i := strings.Index(name, "."); i >= 0 {
			// Legacy [section.subsection]: git lower-cases both parts.
			if i == 0 || i == len(name)-1 {
				return p.errorf("invalid section name %q", name)
			}
			p.setSection(name[:i], strings.ToLower(name[i+1:]), true)
		} else {
			p.setSection(name, "", false)
		}
		return nil
	case ' ', '\t':
		if strings.Contains(name, ".") {
			return p.errorf("invalid section name %q", name)
		}
		p.skipSpace()
		if p.eof() || p.peek() != '"' {
			return p.errorf("expected quoted subsection")
		}
		p.pos++
		var sub strings.Builder
		for {
			if p.eof() || p.peek() == '\n' {
				return p.errorf("unterminated subsection")
			}
			c := p.peek()
			p.pos++
			if c == '"' {
				break
			}
			if c == '\\' {
				if p.eof() || p.peek() == '\n' {
					return p.errorf("unterminated subsection")
				}
				c = p.peek()
				p.pos++
			}
			if c == 0 {
				return p.errorf("NUL in subsection")
			}
			sub.WriteByte(c)
		}
		if p.eof() || p.peek() != ']' {
			return p.errorf("expected ] after subsection")
		}
		p.pos++
		p.setSection(name, sub.String(), true)
		return nil
	}
	return p.errorf("unexpected %q in section header", p.peek())
}

func (p *gitConfigParser) setSection(section, subsection string, hasSub bool) {
	p.section = strings.ToLower(section)
	p.subsection = subsection
	p.hasSub = hasSub
	p.inSection = true
}

func (p *gitConfigParser) parseVariable() error {
	if !p.inSection {
		return p.errorf("variable outside a section")
	}
	start := p.pos
	for !p.eof() && isConfigNameChar(p.peek()) {
		p.pos++
	}
	name := strings.ToLower(p.src[start:p.pos])
	v := gitConfigVar{Section: p.section, Subsection: p.subsection, HasSubsection: p.hasSub, Name: name, Line: p.line}
	p.skipSpace()
	if p.eof() || p.peek() == '\n' || p.peek() == '#' || p.peek() == ';' {
		// "name" alone: boolean true.
		v.Value = "true"
		p.vars = append(p.vars, v)
		return p.endOfLine()
	}
	if p.peek() != '=' {
		return p.errorf("expected = after %q", name)
	}
	p.pos++
	value, err := p.parseValue()
	if err != nil {
		return err
	}
	v.Value = value
	p.vars = append(p.vars, v)
	return nil
}

// parseValue reads a value up to the end of its (possibly continued) line,
// handling quotes, escapes, comments and backslash-newline continuation,
// and consumes the newline.
func (p *gitConfigParser) parseValue() (string, error) {
	p.skipSpace()
	var out strings.Builder
	quoted := false
	// pendingSpace holds unquoted whitespace, kept only if more value
	// follows (git trims trailing unquoted whitespace).
	pendingSpace := ""
	for {
		if p.eof() {
			if quoted {
				return "", p.errorf("unterminated quote")
			}
			return out.String(), nil
		}
		c := p.peek()
		p.pos++
		switch {
		case c == '\n':
			if quoted {
				return "", p.errorf("unterminated quote")
			}
			p.line++
			return out.String(), nil
		case !quoted && (c == '#' || c == ';'):
			p.skipComment()
		case !quoted && isConfigSpace(c):
			if out.Len() > 0 {
				pendingSpace += string(c)
			}
		case c == '"':
			out.WriteString(pendingSpace)
			pendingSpace = ""
			quoted = !quoted
		case c == '\\':
			if p.eof() {
				return "", p.errorf("trailing backslash")
			}
			e := p.peek()
			p.pos++
			out.WriteString(pendingSpace)
			pendingSpace = ""
			switch e {
			case '\n':
				p.line++ // continuation
			case 'n':
				out.WriteByte('\n')
			case 't':
				out.WriteByte('\t')
			case 'b':
				out.WriteByte('\b')
			case '\\', '"':
				out.WriteByte(e)
			default:
				return "", p.errorf("invalid escape \\%c", e)
			}
		case c == 0:
			return "", p.errorf("NUL in value")
		default:
			out.WriteString(pendingSpace)
			pendingSpace = ""
			out.WriteByte(c)
		}
	}
}
