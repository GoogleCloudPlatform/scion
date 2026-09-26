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

package api

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"github.com/google/uuid"
	"golang.org/x/text/unicode/norm"
)

const (
	// MaxSlugLength is the maximum length for a slug
	MaxSlugLength = 63

	// ProjectIDSeparator is the separator between UUID and name in hosted project IDs
	ProjectIDSeparator = "__"
)

var (
	// nonAlphanumeric matches any character that isn't a lowercase letter or digit
	nonAlphanumeric = regexp.MustCompile(`[^a-z0-9]+`)
	// leadingTrailingDash matches leading or trailing dashes
	leadingTrailingDash = regexp.MustCompile(`^-+|-+$`)
)

// Slugify converts a string to a URL-safe slug.
// It normalizes unicode, converts to lowercase, replaces non-alphanumeric
// characters with dashes, and enforces length limits.
func Slugify(s string) string {
	// Normalize unicode (NFD decomposition then remove combining marks)
	s = norm.NFD.String(s)
	var builder strings.Builder
	for _, r := range s {
		if !unicode.Is(unicode.Mn, r) { // Mn = Mark, Nonspacing
			builder.WriteRune(r)
		}
	}
	s = builder.String()

	// Convert to lowercase
	s = strings.ToLower(s)

	// Replace non-alphanumeric with dashes
	s = nonAlphanumeric.ReplaceAllString(s, "-")

	// Remove leading/trailing dashes
	s = leadingTrailingDash.ReplaceAllString(s, "")

	// Enforce length limit
	if len(s) > MaxSlugLength {
		s = s[:MaxSlugLength]
		// Don't end with a dash after truncation
		s = strings.TrimRight(s, "-")
	}

	return s
}

// ValidateAgentName validates an agent name by slugifying it and checking
// that the result is non-empty. Returns the slug on success or an error
// if the name produces an empty slug (e.g. empty input, all special characters).
func ValidateAgentName(name string) (string, error) {
	slug := Slugify(name)
	if slug == "" {
		return "", fmt.Errorf("agent name %q produces an empty slug: must contain at least one alphanumeric character", name)
	}
	return slug, nil
}

// MaxDisplayNameLength is the maximum length, in runes, of an agent display
// name. It is independent of MaxSlugLength, which caps the slug itself in
// bytes.
const MaxDisplayNameLength = 63

// reservedDisplayNameKeys are identity keys (post-Slugify) an agent display
// name must not produce, so a display name can never be confused for a
// system principal or role rather than an agent identity.
var reservedDisplayNameKeys = map[string]bool{
	"user":   true,
	"system": true,
	"hub":    true,
	"scion":  true,
	"admin":  true,
}

// ValidateDisplayName enforces the agent display-name allowlist: 1-63 runes,
// each either one of the literal separators (space, '-', '_', '.') or a rune
// Slugify keeps or folds to a single ASCII letter or digit, slugifying
// overall to a non-empty key that is not a reserved word. Returns the
// identity key (Slugify(name)) on success.
//
// The per-rune check is deliberately narrower than
// unicode.Is(unicode.Latin, r) || unicode.IsDigit(r): that wider allowlist
// admits characters Slugify itself drops or folds to nothing -- small-capital
// and fullwidth letter forms, ligatures, Roman numerals, dotless i, letters
// with no canonical decomposition such as o-stroke or sharp s, and non-ASCII
// digits. Those would let a display name render as one thing (for example a
// reserved word, or a value equal to a different agent's key) while
// producing an unrelated key of its own that separately passes the reserved-
// word and uniqueness checks. Requiring Slugify(string(r)) to collapse to
// exactly one ASCII alphanumeric character keeps the persisted key visibly
// tied to what the name displays.
func ValidateDisplayName(name string) (key string, err error) {
	runes := []rune(name)
	if len(runes) == 0 {
		return "", fmt.Errorf("display name must not be empty")
	}
	if len(runes) > MaxDisplayNameLength {
		return "", fmt.Errorf("display name %q is too long: must be at most %d characters", name, MaxDisplayNameLength)
	}
	for _, r := range runes {
		if r == ' ' || r == '-' || r == '_' || r == '.' {
			continue
		}
		if len(Slugify(string(r))) == 1 {
			continue
		}
		return "", fmt.Errorf("display name %q contains an unsupported character %q: must use letters that reduce to a-z, digits, space, '-', '_' or '.'", name, r)
	}
	key = Slugify(name)
	if key == "" {
		return "", fmt.Errorf("display name %q produces an empty key: must contain at least one Latin letter or digit", name)
	}
	if reservedDisplayNameKeys[key] {
		return "", fmt.Errorf("display name %q is a reserved word", name)
	}
	return key, nil
}

// SlugifyWithSuffix creates a slug with a collision-avoidance suffix.
// The suffix is appended with a dash separator.
func SlugifyWithSuffix(s, suffix string) string {
	slug := Slugify(s)
	if suffix == "" {
		return slug
	}

	suffix = Slugify(suffix)
	maxBase := MaxSlugLength - len(suffix) - 1 // -1 for the dash
	if maxBase < 1 {
		return suffix
	}

	if len(slug) > maxBase {
		slug = slug[:maxBase]
		slug = strings.TrimRight(slug, "-")
	}

	return slug + "-" + suffix
}

// DisplayNameWithSerial returns a display name with a parenthesized serial
// qualifier when the slug has a serial suffix. For example, if baseName is
// "acme-widgets" and slug is "acme-widgets-2", returns "acme-widgets (2)".
// If the slug matches the base slug (no serial suffix), returns baseName as-is.
func DisplayNameWithSerial(baseName, slug, baseSlug string) string {
	if slug == baseSlug {
		return baseName
	}
	// Extract the serial number from the slug suffix
	prefix := baseSlug + "-"
	if strings.HasPrefix(slug, prefix) {
		serial := slug[len(prefix):]
		return fmt.Sprintf("%s (%s)", baseName, serial)
	}
	return baseName
}

// NewUUID generates a new UUID string.
func NewUUID() string {
	return uuid.New().String()
}

// NewShortID generates a short unique identifier (first 8 chars of UUID).
func NewShortID() string {
	id := uuid.New().String()
	return id[:8]
}

// MakeProjectID creates a hosted-format project ID from a UUID and name.
// Format: <uuid>__<slugified-name>
func MakeProjectID(id, name string) string {
	if id == "" {
		id = NewUUID()
	}
	slug := Slugify(name)
	return id + ProjectIDSeparator + slug
}

// ParseProjectID extracts the UUID and name slug from a hosted-format project ID.
// Returns the ID, slug, and whether the parse was successful.
func ParseProjectID(projectID string) (id, slug string, ok bool) {
	parts := strings.SplitN(projectID, ProjectIDSeparator, 2)
	if len(parts) != 2 {
		// Not a hosted format - treat entire string as the name/slug
		return "", projectID, false
	}
	return parts[0], parts[1], true
}

// IsHostedProjectID returns true if the project ID is in hosted format (uuid__name).
func IsHostedProjectID(projectID string) bool {
	_, _, ok := ParseProjectID(projectID)
	return ok
}
