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

package main

import (
	"bufio"
	"bytes"
	"go/build/constraint"
	"strings"
)

// Known GOOS and GOARCH values, used to interpret file-name suffixes such as
// foo_linux.go or foo_linux_amd64.go (mirrors go/build's internal lists).
var knownOS = map[string]bool{
	"aix": true, "android": true, "darwin": true, "dragonfly": true, "freebsd": true,
	"hurd": true, "illumos": true, "ios": true, "js": true, "linux": true, "nacl": true,
	"netbsd": true, "openbsd": true, "plan9": true, "solaris": true, "wasip1": true,
	"windows": true, "zos": true,
}

var knownArch = map[string]bool{
	"386": true, "amd64": true, "amd64p32": true, "arm": true, "armbe": true, "arm64": true,
	"arm64be": true, "loong64": true, "mips": true, "mipsle": true, "mips64": true,
	"mips64le": true, "mips64p32": true, "mips64p32le": true, "ppc": true, "ppc64": true,
	"ppc64le": true, "riscv": true, "riscv64": true, "s390": true, "s390x": true,
	"sparc": true, "sparc64": true, "wasm": true,
}

// fileNameTags returns the GOOS/GOARCH tags implied by a file name.
func fileNameTags(name string) []string {
	stem := strings.TrimSuffix(name, ".go")
	stem = strings.TrimSuffix(stem, "_test")
	parts := strings.Split(stem, "_")
	n := len(parts)
	if n >= 3 && knownOS[parts[n-2]] && knownArch[parts[n-1]] {
		return []string{parts[n-2], parts[n-1]}
	}
	if n >= 2 {
		if knownOS[parts[n-1]] || knownArch[parts[n-1]] {
			return []string{parts[n-1]}
		}
	}
	return nil
}

// goBuildLine returns the //go:build expression of a file, or "".
func goBuildLine(src []byte) string {
	sc := bufio.NewScanner(bytes.NewReader(src))
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	inBlock := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if inBlock {
			if strings.Contains(line, "*/") {
				inBlock = false
			}
			continue
		}
		switch {
		case line == "":
			continue
		case strings.HasPrefix(line, "/*"):
			inBlock = !strings.Contains(line, "*/")
			continue
		case strings.HasPrefix(line, "//"):
			if constraint.IsGoBuild(line) {
				return line
			}
			continue
		}
		return "" // first non-comment line (the package clause)
	}
	return ""
}

// effectiveConstraint combines the //go:build line with the tags implied by
// the file name into one normalized expression ("" when unconstrained).
func effectiveConstraint(name string, src []byte) (string, error) {
	var parts []string
	if line := goBuildLine(src); line != "" {
		x, err := constraint.Parse(line)
		if err != nil {
			return "", err
		}
		s := x.String()
		if _, isAnd := x.(*constraint.AndExpr); !isAnd {
			if _, isTag := x.(*constraint.TagExpr); !isTag {
				if _, isNot := x.(*constraint.NotExpr); !isNot {
					s = "(" + s + ")"
				}
			}
		}
		parts = append(parts, s)
	}
	parts = append(parts, fileNameTags(name)...)
	if len(parts) == 0 {
		return "", nil
	}
	// Re-parse so the result is in canonical form.
	x, err := constraint.Parse("//go:build " + strings.Join(parts, " && "))
	if err != nil {
		return "", err
	}
	return x.String(), nil
}
