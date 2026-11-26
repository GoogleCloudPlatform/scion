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

package agydecode

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	"google.golang.org/protobuf/encoding/protowire"
)

type rawField struct {
	wireType protowire.Type
	varint   uint64
	bytes    []byte
}

type rawMessage map[protowire.Number][]rawField

// parseProtoMessage decodes a wire-format protobuf message into a field map.
// It rejects invalid tags, unsupported wire types, and truncated fields.
func parseProtoMessage(b []byte) (rawMessage, error) {
	msg := make(rawMessage)
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return nil, fmt.Errorf("invalid protobuf tag: %w", protowire.ParseError(n))
		}
		b = b[n:]

		var rf rawField
		rf.wireType = typ
		switch typ {
		case protowire.VarintType:
			v, m := protowire.ConsumeVarint(b)
			if m < 0 {
				return nil, fmt.Errorf("field %d invalid varint: %w", num, protowire.ParseError(m))
			}
			rf.varint = v
			b = b[m:]
		case protowire.Fixed32Type:
			_, m := protowire.ConsumeFixed32(b)
			if m < 0 {
				return nil, fmt.Errorf("field %d invalid fixed32: %w", num, protowire.ParseError(m))
			}
			b = b[m:]
			continue
		case protowire.Fixed64Type:
			_, m := protowire.ConsumeFixed64(b)
			if m < 0 {
				return nil, fmt.Errorf("field %d invalid fixed64: %w", num, protowire.ParseError(m))
			}
			b = b[m:]
			continue
		case protowire.BytesType:
			v, m := protowire.ConsumeBytes(b)
			if m < 0 {
				return nil, fmt.Errorf("field %d invalid bytes: %w", num, protowire.ParseError(m))
			}
			rf.bytes = v
			b = b[m:]
		case protowire.StartGroupType:
			_, m := protowire.ConsumeGroup(num, b)
			if m < 0 {
				return nil, fmt.Errorf("field %d invalid group: %w", num, protowire.ParseError(m))
			}
			b = b[m:]
			continue
		default:
			return nil, fmt.Errorf("field %d unsupported wire type %d", num, typ)
		}
		msg[num] = append(msg[num], rf)
	}
	return msg, nil
}

func (m rawMessage) firstVarint(num protowire.Number) (uint64, bool) {
	for _, f := range m[num] {
		if f.wireType == protowire.VarintType {
			return f.varint, true
		}
	}
	return 0, false
}

func (m rawMessage) firstBytes(num protowire.Number) ([]byte, bool) {
	for _, f := range m[num] {
		if f.wireType == protowire.BytesType {
			return f.bytes, true
		}
	}
	return nil, false
}

func (m rawMessage) firstString(num protowire.Number) (string, error) {
	b, ok := m.firstBytes(num)
	if !ok {
		return "", nil
	}
	if !utf8.Valid(b) {
		return "", fmt.Errorf("field %d contains invalid UTF-8", num)
	}
	return strings.TrimSpace(string(b)), nil
}

func (m rawMessage) nested(num protowire.Number) (rawMessage, bool, error) {
	b, ok := m.firstBytes(num)
	if !ok {
		return nil, false, nil
	}
	sub, err := parseProtoMessage(b)
	if err != nil {
		return nil, false, fmt.Errorf("nested field %d: %w", num, err)
	}
	return sub, true, nil
}

func parseProtoTimestamp(m rawMessage) *time.Time {
	if m == nil {
		return nil
	}
	secs, ok := m.firstVarint(1)
	if !ok || secs == 0 || secs > 1<<62 {
		return nil
	}
	nanos, _ := m.firstVarint(2)
	if nanos >= 1_000_000_000 {
		nanos = 0
	}
	t := time.Unix(int64(secs), int64(nanos)).UTC()
	return &t
}

type decodedUsageEntry struct {
	modelID int64
	tokens  TokenUsage
	hasAny  bool
	keys    []string
	isRetry bool
}

type decodedRowMetadata struct {
	model     string
	modelID   int64
	timestamp *time.Time
	usages    []decodedUsageEntry
}

// decodeRowMetadata parses a single gen_metadata.data (step=false) or
// steps.metadata (step=true) protobuf blob. Only copied usage sub-slices are
// retained so the caller's multi-MB row blob can be garbage collected promptly.
func decodeRowMetadata(blob []byte, step bool) (*decodedRowMetadata, error) {
	root, err := parseProtoMessage(blob)
	if err != nil {
		return nil, err
	}

	var data rawMessage
	if step {
		data = root
	} else {
		var ok bool
		data, ok, err = root.nested(1)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, errors.New("missing generation metadata wrapper (field 1)")
		}
	}

	modelMsg := data
	if step {
		if sub, ok, err := data.nested(24); err != nil {
			return nil, err
		} else if ok {
			modelMsg = sub
		} else {
			modelMsg = nil
		}
	}

	var usages []decodedUsageEntry
	usageField := protowire.Number(4)
	if step {
		usageField = 9
	}
	var primaryModelID int64
	if rawUsage, ok := data.firstBytes(usageField); ok {
		// Copy sub-slice so we do not pin the entire row blob in memory.
		copied := append([]byte(nil), rawUsage...)
		u, err := decodeUsageMessage(copied, false)
		if err != nil {
			return nil, fmt.Errorf("primary usage (field %d): %w", usageField, err)
		}
		primaryModelID = u.modelID
		usages = append(usages, u)
	}

	retryField := protowire.Number(17)
	if step {
		retryField = 28
	}
	for i, rf := range data[retryField] {
		if rf.wireType != protowire.BytesType {
			return nil, fmt.Errorf("retry entry %d (field %d): expected bytes wire type", i, retryField)
		}
		retryMsg, err := parseProtoMessage(rf.bytes)
		if err != nil {
			return nil, fmt.Errorf("retry entry %d (field %d): %w", i, retryField, err)
		}
		if rawRetryUsage, ok := retryMsg.firstBytes(2); ok {
			copied := append([]byte(nil), rawRetryUsage...)
			u, err := decodeUsageMessage(copied, true)
			if err != nil {
				return nil, fmt.Errorf("retry entry %d usage: %w", i, err)
			}
			usages = append(usages, u)
		}
	}

	// Resolve model name and numeric model ID.
	var primaryNameField, secondaryNameField, modelIDField protowire.Number
	if step {
		primaryNameField = 12
		secondaryNameField = 8
		modelIDField = 1
	} else {
		primaryNameField = 19
		secondaryNameField = 21
		modelIDField = 3
	}

	var model string
	var modelID int64
	if modelMsg != nil {
		n1, err := modelMsg.firstString(primaryNameField)
		if err != nil {
			return nil, err
		}
		n2, err := modelMsg.firstString(secondaryNameField)
		if err != nil {
			return nil, err
		}
		names := []string{NormalizeModelName(n1), NormalizeModelName(n2)}
		for _, n := range names {
			if IsConcreteModel(n) {
				model = n
				break
			}
		}
		if model == "" {
			for _, n := range names {
				if isUsableFallbackName(n) {
					model = n
					break
				}
			}
		}
		if id, ok := modelMsg.firstVarint(modelIDField); ok && id > 0 && id <= telemetrycontract.MaxSafeInteger {
			modelID = int64(id)
		}
	}
	if modelID == 0 {
		modelID = primaryModelID
	}

	// Resolve timestamp.
	var ts *time.Time
	if step {
		if sub, ok, err := data.nested(8); err != nil {
			return nil, err
		} else if ok {
			ts = parseProtoTimestamp(sub)
		}
		if ts == nil {
			if sub, ok, err := data.nested(1); err != nil {
				return nil, err
			} else if ok {
				ts = parseProtoTimestamp(sub)
			}
		}
	} else {
		if f9, ok, err := data.nested(9); err != nil {
			return nil, err
		} else if ok {
			if f94, ok, err := f9.nested(4); err != nil {
				return nil, err
			} else if ok {
				ts = parseProtoTimestamp(f94)
			}
		}
	}

	return &decodedRowMetadata{
		model:     model,
		modelID:   modelID,
		timestamp: ts,
		usages:    usages,
	}, nil
}

func safeTokenCount(v uint64, field int) (int64, error) {
	if v > telemetrycontract.MaxSafeInteger {
		return 0, fmt.Errorf("usage field %d value %d exceeds max safe integer", field, v)
	}
	return int64(v), nil
}

// decodeUsageMessage parses an Antigravity usage protobuf into canonical
// TokenUsage + identity keys [11, 12, 7].
func decodeUsageMessage(b []byte, isRetry bool) (decodedUsageEntry, error) {
	msg, err := parseProtoMessage(b)
	if err != nil {
		return decodedUsageEntry{}, err
	}

	var out decodedUsageEntry
	out.isRetry = isRetry

	if id, ok := msg.firstVarint(1); ok && id > 0 {
		mid, err := safeTokenCount(id, 1)
		if err != nil {
			return decodedUsageEntry{}, err
		}
		out.modelID = mid
	}

	// f2: uncached input (exclusive of cache_read / cache_write)
	if v, ok := msg.firstVarint(2); ok {
		n, err := safeTokenCount(v, 2)
		if err != nil {
			return decodedUsageEntry{}, err
		}
		out.tokens.Input = &n
		out.tokens.InputDerivation = telemetrycontract.InputDerivationReported
	}

	// f4: cache_write
	if v, ok := msg.firstVarint(4); ok {
		n, err := safeTokenCount(v, 4)
		if err != nil {
			return decodedUsageEntry{}, err
		}
		out.tokens.CacheWrite = &n
	}

	// f5: cache_read
	if v, ok := msg.firstVarint(5); ok {
		n, err := safeTokenCount(v, 5)
		if err != nil {
			return decodedUsageEntry{}, err
		}
		out.tokens.CacheRead = &n
	}

	// f3: output, f9: reasoning, f10: non-reasoning output.
	// Canonical output = max(f3, f9 + f10), inclusive of reasoning.
	f3, hasF3 := msg.firstVarint(3)
	f9, hasF9 := msg.firstVarint(9)
	f10, hasF10 := msg.firstVarint(10)

	var v3, v9, v10 int64
	if hasF3 {
		v3, err = safeTokenCount(f3, 3)
		if err != nil {
			return decodedUsageEntry{}, err
		}
	}
	if hasF9 {
		v9, err = safeTokenCount(f9, 9)
		if err != nil {
			return decodedUsageEntry{}, err
		}
	}
	if hasF10 {
		v10, err = safeTokenCount(f10, 10)
		if err != nil {
			return decodedUsageEntry{}, err
		}
	}

	if hasF3 || hasF9 || hasF10 {
		if v9 > telemetrycontract.MaxSafeInteger-v10 {
			return decodedUsageEntry{}, fmt.Errorf("usage f9+f10 overflow")
		}
		output := v3
		if sum := v9 + v10; sum > output {
			output = sum
		}
		out.tokens.Output = &output
		if hasF9 {
			reasoning := v9
			if reasoning > output {
				reasoning = output
			}
			out.tokens.Reasoning = &reasoning
			overlap := true
			out.tokens.ReasoningOverlapsOutput = &overlap
		}
	}

	// Extract identity keys from fields [11, 12, 7].
	for _, fn := range identityFieldOrder {
		id, err := msg.firstString(protowire.Number(fn))
		if err != nil {
			return decodedUsageEntry{}, err
		}
		if id != "" {
			out.keys = append(out.keys, formatProtoIdentityKey(fn, id))
		}
	}
	out.keys = dedupeNonEmpty(out.keys)

	sum, overflow := out.tokens.SummableTokens()
	if overflow {
		return decodedUsageEntry{}, errors.New("summable token total overflows int64")
	}
	out.hasAny = sum > 0

	return out, nil
}

func decodeTrajectoryTimestamp(blob []byte) (*time.Time, error) {
	root, err := parseProtoMessage(blob)
	if err != nil {
		return nil, err
	}
	f2, ok, err := root.nested(2)
	if err != nil || !ok {
		return nil, err
	}
	return parseProtoTimestamp(f2), nil
}
