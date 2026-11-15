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

package celengine

import (
	"context"
	"errors"
	"fmt"
	"math"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

type adversarialProbe struct {
	name            string
	expr            string
	wantCompileCat  string   // non-empty when Compile itself must reject the expression
	wantEvalReason  string   // optional exact EvalError.Reason when evaluation must fail
	wantEvalReasons []string // optional allowed EvalError.Reason set (e.g. cost_limit or timeout under -race)
}

type surfaceExemption struct {
	justification string
}

// primeRKCollisionPair returns two distinct 8-byte ASCII blocks whose uint32
// polynomial rolling hashes under Go stdlib's internal/bytealg PrimeRK = 16777619
// are identical (0xa03f3918), verifying the collision invariant at test start.
func primeRKCollisionPair(t *testing.T) (string, string) {
	t.Helper()
	const (
		primeRK uint32 = 16777619
		blockA         = "k1etelfn"
		blockB         = "1xfk34je"
	)
	hash := func(s string) uint32 {
		var h uint32
		for i := 0; i < len(s); i++ {
			h = h*primeRK + uint32(s[i])
		}
		return h
	}
	if blockA == blockB || len(blockA) != len(blockB) || hash(blockA) != hash(blockB) {
		t.Fatalf("invalid PrimeRK collision pair: %q (%#x) vs %q (%#x)", blockA, hash(blockA), blockB, hash(blockB))
	}
	return blockA, blockB
}

// TestAdversarialSurfaceCoversEveryFunctionAndOverload iterates the Engine's live
// Functions(), celEnv.Functions() overload declarations, and celEnv.Macros() keys,
// failing if any function, concrete overload ID, or macro key lacks either a
// max-admitted-input adversarial probe or an explicit, justified parser-internal
// exemption. Every runtime probe asserts wall-clock time (<= 2x EvalTimeout on
// error, <= EvalTimeout on any decision) and heap allocation (< 64 MiB).
func TestAdversarialSurfaceCoversEveryFunctionAndOverload(t *testing.T) {
	env := Environment{
		Name:    "adversarial_audit",
		Version: 1,
		Variables: []Variable{
			{Name: "event", Type: Map(String(), Dyn())},
			{Name: "strList", Type: List(String())},
			{Name: "intList", Type: List(Int())},
			{Name: "strMap", Type: Map(String(), String())},
			{Name: "intMap", Type: Map(Int(), String())},
			{Name: "boolMap", Type: Map(Bool(), String())},
			{Name: "s", Type: String()},
			{Name: "i", Type: Int()},
			{Name: "d", Type: Double()},
			{Name: "b", Type: Bool()},
			{Name: "ts", Type: Timestamp()},
		},
		Extensions: []Extension{ExtensionStrings},
		Macros:     MacrosStandard,
	}

	eng, err := New(env, DefaultLimits(), DefaultCacheConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rkA, rkB := primeRKCollisionPair(t)
	// 1) 64 KiB haystack + 72-byte PrimeRK-colliding needle (> 64B & 64KiB*72 > 4MiB => exercises KMP path within CostLimit).
	// 2) 576 KiB haystack + 288 KiB PrimeRK-colliding needle (within 1 MiB MaxOutputBytes => exercises KMP + CostLimit in <5ms).
	rkHaystack64k := strings.Repeat(rkA, 8*1024)     // 64 KiB (65,536 bytes)
	rkNeedle72 := strings.Repeat(rkA, 8) + rkB       // 72 bytes (> 64B, PrimeRK collision every 8 bytes)
	rkHaystack := strings.Repeat(rkA, 72*1024)       // 576 KiB (589,824 bytes)
	rkNeedle := strings.Repeat(rkA, 36*1024) + rkB   // 288 KiB + 8B
	kmpHaystack := strings.Repeat("a", 256*1024)     // 256 KiB
	kmpNeedle := strings.Repeat("a", 128*1024) + "b" // 128 KiB + 1B
	s64k := strings.Repeat("a", 64*1024)             // 64 KiB
	s64kQuotes := strings.Repeat(`"`, 64*1024)       // 64 KiB quotes
	s64kSpaces := strings.Repeat(" ", 32*1024) + "x" + strings.Repeat("\t", 32*1024)
	b64k := []byte(s64k)

	bigStrList := make([]string, 4096)
	for idx := range bigStrList {
		bigStrList[idx] = "item-value-0123"
	}
	bigIntList := make([]int64, 8192)
	for idx := range bigIntList {
		bigIntList[idx] = int64(idx)
	}
	bigStrMap := make(map[string]string, 2048)
	bigIntMap := make(map[int64]string, 2048)
	for idx := 0; idx < 2048; idx++ {
		k := fmt.Sprintf("key-%05d", idx)
		bigStrMap[k] = "val"
		bigIntMap[int64(idx)] = "val"
	}

	act := map[string]any{
		"event": map[string]any{
			"s64k":       s64k,
			"quotes64k":  s64kQuotes,
			"spaces64k":  s64kSpaces,
			"b64k":       b64k,
			"rkH64":      rkHaystack64k,
			"rkN72":      rkNeedle72,
			"rkH":        rkHaystack,
			"rkN":        rkNeedle,
			"kmpH":       kmpHaystack,
			"kmpN":       kmpNeedle,
			"oversiz":    strings.Repeat("z", 2<<20),
			"overBytes":  []byte(strings.Repeat("z", 2<<20)),
			"reBig":      `[a-z]{0,1000}x`,
			"reWork":     strings.Repeat("a?", 80) + strings.Repeat("a", 80) + "x",
			"reOk":       `^a+$`,
			"u":          uint64(1_000_000_007),
			"uZero":      uint64(0),
			"negInt":     int64(-1),
			"dur":        24 * time.Hour,
			"badList":    []any{[]any{s64k, s64k}, int64(1)},
			"bigDynList": []any{s64k, s64k, s64k, s64k},
		},
		"strList": bigStrList,
		"intList": bigIntList,
		"strMap":  bigStrMap,
		"intMap":  bigIntMap,
		"boolMap": map[bool]string{true: s64k, false: "no"},
		"s":       s64k,
		"i":       int64(math.MaxInt64 - 1),
		"d":       1.7976931348623157e+308,
		"b":       true,
		"ts":      time.Date(2026, 11, 15, 6, 0, 0, 987654321, time.UTC),
	}

	buildDAG := func(seed string) string {
		cur := "[[" + seed + "]]"
		for lvl := 0; lvl < 6; lvl++ {
			v := fmt.Sprintf("d%d", lvl)
			cur += fmt.Sprintf(".map(%s, [[%s, %s, %s, %s, %s, %s, %s, %s]])[0]", v, v, v, v, v, v, v, v, v)
		}
		return cur
	}
	dag1 := buildDAG("1")
	dag2 := buildDAG("2")

	// 1. Concrete overload ID -> >=1 max-admitted-input adversarial probe(s).
	overloadProbes := map[string][]adversarialProbe{
		// Logical & conditional operators
		"logical_not": {{name: "not_bool", expr: `!(s == "x")`}},
		"logical_and": {{name: "and_short_circuit_and_eval", expr: `b && (s == s)`}},
		"logical_or": {
			{name: "or_eval", expr: `!b || (s == s)`},
			{name: "or_cannot_swallow_cost_limit", expr: `event.s64k.matches(event.reWork) || true`, wantEvalReason: ReasonCostLimit},
		},
		"conditional": {{name: "ternary_large_branches", expr: `(b ? s : "fallback").size() == 65536`}},

		// Equality & inequality (including shared-structure DAG walk bound)
		"equals": {
			{name: "eq_64k_strings", expr: `s == event.s64k`},
			{name: "eq_nested_dag_bounded", expr: dag1 + " == " + dag1, wantEvalReason: ReasonCostLimit},
		},
		"not_equals": {
			{name: "ne_64k_strings", expr: `s != "other"`},
			{name: "ne_nested_dag_bounded", expr: dag1 + " != " + dag2, wantEvalReason: ReasonCostLimit},
		},

		// Ordering comparisons across all scalar types (cross-type numeric overloads dispatch via dyn)
		"less_bool":                    {{name: "lt_bool", expr: `false < b`}},
		"less_int64":                   {{name: "lt_int", expr: `-i < i`}},
		"less_int64_double":            {{name: "lt_int_double", expr: `dyn(i) < dyn(d)`}},
		"less_int64_uint64":            {{name: "lt_int_uint", expr: `dyn(1) < event.u`}},
		"less_uint64":                  {{name: "lt_uint", expr: `uint(1) < uint(event.u)`}},
		"less_uint64_double":           {{name: "lt_uint_double", expr: `event.u < dyn(d)`}},
		"less_uint64_int64":            {{name: "lt_uint_int", expr: `event.u < dyn(i)`}},
		"less_double":                  {{name: "lt_double", expr: `-d < d`}},
		"less_double_int64":            {{name: "lt_double_int", expr: `dyn(1.5) < dyn(i)`}},
		"less_double_uint64":           {{name: "lt_double_uint", expr: `dyn(1.5) < event.u`}},
		"less_string":                  {{name: "lt_64k_string", expr: `s < (s.substring(0, 65535) + "b")`}},
		"less_bytes":                   {{name: "lt_64k_bytes", expr: `bytes(event.b64k) < bytes(s.substring(0, 65535) + "b")`}},
		"less_timestamp":               {{name: "lt_timestamp", expr: `ts < ts + duration("1s")`}},
		"less_duration":                {{name: "lt_duration", expr: `duration("1s") < duration(event.dur)`}},
		"less_equals_bool":             {{name: "le_bool", expr: `b <= true`}},
		"less_equals_int64":            {{name: "le_int", expr: `i <= i`}},
		"less_equals_int64_double":     {{name: "le_int_double", expr: `dyn(i) <= dyn(d)`}},
		"less_equals_int64_uint64":     {{name: "le_int_uint", expr: `dyn(1) <= event.u`}},
		"less_equals_uint64":           {{name: "le_uint", expr: `uint(event.u) <= uint(event.u)`}},
		"less_equals_uint64_double":    {{name: "le_uint_double", expr: `event.u <= dyn(d)`}},
		"less_equals_uint64_int64":     {{name: "le_uint_int", expr: `event.u <= dyn(i)`}},
		"less_equals_double":           {{name: "le_double", expr: `d <= d`}},
		"less_equals_double_int64":     {{name: "le_double_int", expr: `dyn(1.5) <= dyn(i)`}},
		"less_equals_double_uint64":    {{name: "le_double_uint", expr: `dyn(1.5) <= event.u`}},
		"less_equals_string":           {{name: "le_64k_string", expr: `s <= event.s64k`}},
		"less_equals_bytes":            {{name: "le_64k_bytes", expr: `bytes(event.b64k) <= bytes(s)`}},
		"less_equals_timestamp":        {{name: "le_timestamp", expr: `ts <= ts`}},
		"less_equals_duration":         {{name: "le_duration", expr: `duration(event.dur) <= duration(event.dur)`}},
		"greater_bool":                 {{name: "gt_bool", expr: `b > false`}},
		"greater_int64":                {{name: "gt_int", expr: `i > 0`}},
		"greater_int64_double":         {{name: "gt_int_double", expr: `dyn(i) > dyn(1.5)`}},
		"greater_int64_uint64":         {{name: "gt_int_uint", expr: `dyn(i) > event.u`}},
		"greater_uint64":               {{name: "gt_uint", expr: `uint(event.u) > uint(1)`}},
		"greater_uint64_double":        {{name: "gt_uint_double", expr: `event.u > dyn(1.5)`}},
		"greater_uint64_int64":         {{name: "gt_uint_int", expr: `event.u > dyn(1)`}},
		"greater_double":               {{name: "gt_double", expr: `d > 0.0`}},
		"greater_double_int64":         {{name: "gt_double_int", expr: `dyn(d) > dyn(i)`}},
		"greater_double_uint64":        {{name: "gt_double_uint", expr: `dyn(d) > event.u`}},
		"greater_string":               {{name: "gt_64k_string", expr: `(s.substring(0, 65535) + "b") > s`}},
		"greater_bytes":                {{name: "gt_64k_bytes", expr: `bytes(s.substring(0, 65535) + "b") > bytes(event.b64k)`}},
		"greater_timestamp":            {{name: "gt_timestamp", expr: `ts + duration("1s") > ts`}},
		"greater_duration":             {{name: "gt_duration", expr: `duration(event.dur) > duration("1s")`}},
		"greater_equals_bool":          {{name: "ge_bool", expr: `b >= true`}},
		"greater_equals_int64":         {{name: "ge_int", expr: `i >= i`}},
		"greater_equals_int64_double":  {{name: "ge_int_double", expr: `dyn(i) >= dyn(1.5)`}},
		"greater_equals_int64_uint64":  {{name: "ge_int_uint", expr: `dyn(i) >= event.u`}},
		"greater_equals_uint64":        {{name: "ge_uint", expr: `uint(event.u) >= uint(event.u)`}},
		"greater_equals_uint64_double": {{name: "ge_uint_double", expr: `event.u >= dyn(1.5)`}},
		"greater_equals_uint64_int64":  {{name: "ge_uint_int", expr: `event.u >= dyn(1)`}},
		"greater_equals_double":        {{name: "ge_double", expr: `d >= d`}},
		"greater_equals_double_int64":  {{name: "ge_double_int", expr: `dyn(d) >= dyn(i)`}},
		"greater_equals_double_uint64": {{name: "ge_double_uint", expr: `dyn(d) >= event.u`}},
		"greater_equals_string":        {{name: "ge_64k_string", expr: `s >= event.s64k`}},
		"greater_equals_bytes":         {{name: "ge_64k_bytes", expr: `bytes(event.b64k) >= bytes(s)`}},
		"greater_equals_timestamp":     {{name: "ge_timestamp", expr: `ts >= ts`}},
		"greater_equals_duration":      {{name: "ge_duration", expr: `duration(event.dur) >= duration(event.dur)`}},

		// Arithmetic & concatenation (+, -, *, /, %, unary -)
		"negate_int64":  {{name: "neg_int", expr: `-i < 0`}},
		"negate_double": {{name: "neg_double", expr: `-d < 0.0`}},
		"add_int64": {
			{name: "add_int_ok", expr: `1 + 2 == 3`},
			{name: "add_int_overflow", expr: `i + 100 == 0`, wantEvalReason: ReasonRuntime},
		},
		"add_uint64": {{name: "add_uint", expr: `uint(event.u) + uint(1) > uint(event.u)`}},
		"add_double": {{name: "add_double_extreme", expr: `(d + d) > 0.0`}},
		"add_string": {
			{name: "add_64k_strings_ok", expr: `(s + s).size() == 131072`},
			{name: "add_string_doubling_over_1mib", expr: `[s].map(a, [a + a].map(b, [b + b].map(c, [c + c].map(d, [d + d].map(e, e + e)))))[0][0][0][0][0].size() > 0`, wantEvalReason: ReasonCostLimit},
		},
		"add_bytes": {
			{name: "add_64k_bytes_ok", expr: `(bytes(event.b64k) + bytes(event.b64k)).size() == 131072`},
			{name: "add_bytes_doubling_over_1mib", expr: `[bytes(event.b64k)].map(a, [a + a].map(b, [b + b].map(c, [c + c].map(d, [d + d].map(e, e + e)))))[0][0][0][0][0].size() > 0`, wantEvalReason: ReasonCostLimit},
		},
		"add_list": {
			{name: "add_8k_lists_ok", expr: `(intList + intList).size() == 16384`},
			{name: "add_list_over_max_elems", expr: `[s.split("")].map(l, (l + l).size() > 0)[0]`, wantEvalReason: ReasonCostLimit},
		},
		"add_timestamp_duration": {{name: "add_ts_dur", expr: `ts + duration(event.dur) > ts`}},
		"add_duration_timestamp": {{name: "add_dur_ts", expr: `duration(event.dur) + ts > ts`}},
		"add_duration_duration":  {{name: "add_dur_dur", expr: `duration(event.dur) + duration("1h") > duration(event.dur)`}},

		"subtract_int64":               {{name: "sub_int", expr: `i - 1 < i`}},
		"subtract_uint64":              {{name: "sub_uint_underflow", expr: `uint(1) - uint(2) == uint(0)`, wantEvalReason: ReasonRuntime}},
		"subtract_double":              {{name: "sub_double", expr: `d - 1.0 == d`}},
		"subtract_timestamp_timestamp": {{name: "sub_ts_ts", expr: `(ts - (ts - duration("5s"))) == duration("5s")`}},
		"subtract_timestamp_duration":  {{name: "sub_ts_dur", expr: `(ts - duration("5s")) < ts`}},
		"subtract_duration_duration":   {{name: "sub_dur_dur", expr: `(duration(event.dur) - duration("1s")) < duration(event.dur)`}},

		"multiply_int64":  {{name: "mul_int_overflow", expr: `i * 2 == 0`, wantEvalReason: ReasonRuntime}},
		"multiply_uint64": {{name: "mul_uint", expr: `uint(event.u) * uint(2) > uint(event.u)`}},
		"multiply_double": {{name: "mul_double", expr: `d * 0.5 < d`}},

		"divide_int64":  {{name: "div_int_by_zero", expr: `i / 0 == 0`, wantEvalReason: ReasonRuntime}},
		"divide_uint64": {{name: "div_uint_by_zero", expr: `uint(event.u) / uint(event.uZero) == uint(0)`, wantEvalReason: ReasonRuntime}},
		"divide_double": {{name: "div_double", expr: `(d / 0.0) > 0.0`}},

		"modulo_int64":  {{name: "mod_int_by_zero", expr: `i % 0 == 0`, wantEvalReason: ReasonRuntime}},
		"modulo_uint64": {{name: "mod_uint_by_zero", expr: `uint(event.u) % uint(event.uZero) == uint(0)`, wantEvalReason: ReasonRuntime}},

		// Indexing & membership
		"index_list": {{name: "index_8k_list", expr: `intList[8191] == 8191`}},
		"index_map":  {{name: "index_2k_map", expr: `strMap["key-02047"] == "val" && intMap[2047] == "val" && boolMap[true].size() == 65536`}},
		"in_list": {
			{name: "in_8k_list", expr: `!(999999 in intList)`},
			{name: "in_nested_dag_bounded", expr: dag2 + " in [" + dag1 + "]", wantEvalReason: ReasonCostLimit},
		},
		"in_map": {{name: "in_2k_map_64k_key", expr: `!(s in strMap)`}},

		// Size overloads (including oversized direct-activation rejection)
		"size_string": {
			{name: "size_64k_string", expr: `size(s) == 65536`},
			{name: "size_oversized_string_rejected", expr: `size(event.oversiz) > 0`, wantEvalReason: ReasonCostLimit},
		},
		"string_size": {
			{name: "member_size_64k_string", expr: `s.size() == 65536`},
			{name: "member_size_oversized_string_rejected", expr: `event.oversiz.size() > 0`, wantEvalReason: ReasonCostLimit},
		},
		"size_bytes": {
			{name: "size_64k_bytes", expr: `size(bytes(event.b64k)) == 65536`},
			{name: "size_oversized_bytes_rejected", expr: `size(event.overBytes) > 0`, wantEvalReason: ReasonCostLimit},
		},
		"bytes_size": {{name: "member_size_64k_bytes", expr: `bytes(event.b64k).size() == 65536`}},
		"size_list":  {{name: "size_8k_list", expr: `size(intList) == 8192`}},
		"list_size":  {{name: "member_size_8k_list", expr: `intList.size() == 8192`}},
		"size_map":   {{name: "size_2k_map", expr: `size(strMap) == 2048`}},
		"map_size":   {{name: "member_size_2k_map", expr: `strMap.size() == 2048`}},

		// Standard string predicates & RE2 matches
		"contains_string": {
			{name: "contains_64k_prime_rk_collision_linear_ok", expr: `!string(event.rkH64).contains(string(event.rkN72))`},
			{name: "contains_576k_prime_rk_collision_fast_cost_limit", expr: `!string(event.rkH).contains(string(event.rkN))`, wantEvalReason: ReasonCostLimit},
			{name: "contains_oversized_rejected", expr: `string(event.oversiz).contains("z")`, wantEvalReason: ReasonCostLimit},
		},
		"starts_with_string": {
			{name: "startsWith_64k", expr: `s.startsWith(s)`},
			{name: "startsWith_oversized_rejected", expr: `string(event.oversiz).startsWith("z")`, wantEvalReason: ReasonCostLimit},
		},
		"ends_with_string": {
			{name: "endsWith_64k", expr: `s.endsWith(s)`},
			{name: "endsWith_oversized_rejected", expr: `string(event.oversiz).endsWith("z")`, wantEvalReason: ReasonCostLimit},
		},
		"matches": {
			{name: "fn_matches_literal_oversized", expr: `matches(s, "[a-z]{0,1000}x")`, wantCompileCat: CategorySize},
			{name: "fn_matches_literal_overwork", expr: `matches(s, "` + strings.Repeat("a?", 80) + strings.Repeat("a", 80) + `x")`, wantEvalReason: ReasonCostLimit},
			{name: "fn_matches_dynamic_oversized", expr: `matches(s, string(event.reBig))`, wantEvalReason: ReasonCostLimit},
			{name: "fn_matches_dynamic_overwork", expr: `matches(s, string(event.reWork))`, wantEvalReason: ReasonCostLimit},
			{name: "fn_matches_normal_64k", expr: `matches(s, string(event.reOk))`},
		},
		"matches_string": {
			{name: "member_matches_literal_oversized", expr: `s.matches("[a-z]{0,1000}x")`, wantCompileCat: CategorySize},
			{name: "member_matches_literal_overwork", expr: `s.matches("` + strings.Repeat("a?", 80) + strings.Repeat("a", 80) + `x")`, wantEvalReason: ReasonCostLimit},
			{name: "member_matches_dynamic_oversized", expr: `s.matches(string(event.reBig))`, wantEvalReason: ReasonCostLimit},
			{name: "member_matches_dynamic_overwork", expr: `s.matches(string(event.reWork))`, wantEvalReason: ReasonCostLimit},
			{name: "member_matches_normal_64k", expr: `s.matches("^a+$")`},
		},

		// Type conversions
		"type":                   {{name: "type_of_64k", expr: `type(s) == string`}},
		"to_dyn":                 {{name: "dyn_64k", expr: `dyn(s) == s`}},
		"bool_to_bool":           {{name: "bool_id", expr: `bool(b) == true`}},
		"string_to_bool":         {{name: "string_to_bool_oversized", expr: `bool(string(event.oversiz))`, wantEvalReason: ReasonCostLimit}},
		"bytes_to_bytes":         {{name: "bytes_id_oversized", expr: `bytes(bytes(event.overBytes)).size() > 0`, wantEvalReason: ReasonCostLimit}},
		"string_to_bytes":        {{name: "string_to_bytes_64k", expr: `bytes(s).size() == 65536`}},
		"double_to_double":       {{name: "double_id", expr: `double(d) == d`}},
		"int64_to_double":        {{name: "int_to_double", expr: `double(i) > 0.0`}},
		"uint64_to_double":       {{name: "uint_to_double", expr: `double(uint(event.u)) > 0.0`}},
		"string_to_double":       {{name: "string_to_double_oversized", expr: `double(string(event.oversiz)) > 0.0`, wantEvalReason: ReasonCostLimit}},
		"duration_to_duration":   {{name: "duration_id", expr: `duration(duration("10s")) == duration("10s")`}},
		"string_to_duration":     {{name: "string_to_duration_oversized", expr: `duration(string(event.oversiz)) == duration("0s")`, wantEvalReason: ReasonCostLimit}},
		"int64_to_int64":         {{name: "int_id", expr: `int(i) == i`}},
		"uint64_to_int64":        {{name: "uint_to_int", expr: `int(uint(event.u)) == 1000000007`}},
		"double_to_int64":        {{name: "double_to_int_range_err", expr: `int(d) == 0`, wantEvalReason: ReasonRuntime}},
		"string_to_int64":        {{name: "string_to_int_oversized", expr: `int(string(event.oversiz)) == 0`, wantEvalReason: ReasonCostLimit}},
		"timestamp_to_int64":     {{name: "ts_to_int", expr: `int(ts) > 0`}},
		"duration_to_int64":      {{name: "dur_to_int", expr: `int(duration(event.dur)) > 0`}},
		"uint64_to_uint64":       {{name: "uint_id", expr: `uint(uint(event.u)) == uint(event.u)`}},
		"int64_to_uint64":        {{name: "int_to_uint_neg", expr: `uint(int(event.negInt)) == uint(0)`, wantEvalReason: ReasonRuntime}},
		"double_to_uint64":       {{name: "double_to_uint_range_err", expr: `uint(d) == uint(0)`, wantEvalReason: ReasonRuntime}},
		"string_to_uint64":       {{name: "string_to_uint_oversized", expr: `uint(string(event.oversiz)) == uint(0)`, wantEvalReason: ReasonCostLimit}},
		"string_to_string":       {{name: "string_id_oversized", expr: `string(string(event.oversiz)) == ""`, wantEvalReason: ReasonCostLimit}},
		"bool_to_string":         {{name: "bool_to_string", expr: `string(b) == "true"`}},
		"int64_to_string":        {{name: "int_to_string", expr: `string(i).size() > 0`}},
		"uint64_to_string":       {{name: "uint_to_string", expr: `string(uint(event.u)) == "1000000007"`}},
		"double_to_string":       {{name: "double_to_string_extreme", expr: `string(d).size() > 0`}},
		"bytes_to_string":        {{name: "bytes_to_string_64k", expr: `string(bytes(event.b64k)) == s`}},
		"timestamp_to_string":    {{name: "ts_to_string", expr: `string(ts).startsWith("2026-")`}},
		"duration_to_string":     {{name: "dur_to_string", expr: `string(duration(event.dur)) == "86400s"`}},
		"timestamp_to_timestamp": {{name: "ts_id", expr: `timestamp(ts) == ts`}},
		"string_to_timestamp":    {{name: "string_to_ts_oversized", expr: `timestamp(string(event.oversiz)) == ts`, wantEvalReason: ReasonCostLimit}},
		"int64_to_timestamp":     {{name: "int_to_ts", expr: `timestamp(1700000000) < ts`}},

		// Timestamp & duration accessors (including 64 KiB / oversized timezone arguments)
		"timestamp_to_year":                         {{name: "ts_year", expr: `ts.getFullYear() == 2026`}},
		"timestamp_to_year_with_tz":                 {{name: "ts_year_tz_bad_64k", expr: `ts.getFullYear(s) == 0`, wantEvalReason: ReasonRuntime}},
		"timestamp_to_month":                        {{name: "ts_month", expr: `ts.getMonth() == 10`}},
		"timestamp_to_month_with_tz":                {{name: "ts_month_tz_oversized", expr: `ts.getMonth(string(event.oversiz)) == 0`, wantEvalReason: ReasonCostLimit}},
		"timestamp_to_day_of_year":                  {{name: "ts_doy", expr: `ts.getDayOfYear() > 300`}},
		"timestamp_to_day_of_year_with_tz":          {{name: "ts_doy_tz", expr: `ts.getDayOfYear("UTC") > 300`}},
		"timestamp_to_day_of_month":                 {{name: "ts_dom", expr: `ts.getDayOfMonth() == 14`}},
		"timestamp_to_day_of_month_with_tz":         {{name: "ts_dom_tz", expr: `ts.getDayOfMonth("UTC") == 14`}},
		"timestamp_to_day_of_month_1_based":         {{name: "ts_date", expr: `ts.getDate() == 15`}},
		"timestamp_to_day_of_month_1_based_with_tz": {{name: "ts_date_tz", expr: `ts.getDate("+02:00") == 15`}},
		"timestamp_to_day_of_week":                  {{name: "ts_dow", expr: `ts.getDayOfWeek() >= 0`}},
		"timestamp_to_day_of_week_with_tz":          {{name: "ts_dow_tz", expr: `ts.getDayOfWeek("UTC") >= 0`}},
		"timestamp_to_hours":                        {{name: "ts_hours", expr: `ts.getHours() == 6`}},
		"timestamp_to_hours_with_tz":                {{name: "ts_hours_tz", expr: `ts.getHours("+01:00") == 7`}},
		"timestamp_to_minutes":                      {{name: "ts_mins", expr: `ts.getMinutes() == 0`}},
		"timestamp_to_minutes_with_tz":              {{name: "ts_mins_tz", expr: `ts.getMinutes("UTC") == 0`}},
		"timestamp_to_seconds":                      {{name: "ts_secs", expr: `ts.getSeconds() == 0`}},
		"timestamp_to_seconds_tz":                   {{name: "ts_secs_tz", expr: `ts.getSeconds("UTC") == 0`}},
		"timestamp_to_milliseconds":                 {{name: "ts_ms", expr: `ts.getMilliseconds() == 987`}},
		"timestamp_to_milliseconds_with_tz":         {{name: "ts_ms_tz", expr: `ts.getMilliseconds("UTC") == 987`}},
		"duration_to_hours":                         {{name: "dur_hours", expr: `duration(event.dur).getHours() == 24`}},
		"duration_to_minutes":                       {{name: "dur_mins", expr: `duration(event.dur).getMinutes() == 1440`}},
		"duration_to_seconds":                       {{name: "dur_secs", expr: `duration(event.dur).getSeconds() == 86400`}},
		"duration_to_milliseconds":                  {{name: "dur_ms", expr: `duration(event.dur).getMilliseconds() == 86400000`}},

		// ext.Strings (pinned v5) overloads
		"string_char_at_int": {{name: "charAt_64k", expr: `s.charAt(65535) == "a"`}},
		"string_index_of_string": {
			{name: "indexOf_64k_ok", expr: `s.indexOf("aaaaab") == -1`},
			{name: "indexOf_kmp_worst_case_fast_cost_limit", expr: `string(event.kmpH).indexOf(string(event.kmpN)) == -1`, wantEvalReason: ReasonCostLimit},
		},
		"string_index_of_string_int": {
			{name: "indexOf_offset_64k_ok", expr: `s.indexOf("aaaaab", 10) == -1`},
			{name: "indexOf_offset_kmp_worst_case_fast_cost_limit", expr: `string(event.kmpH).indexOf(string(event.kmpN), 10) == -1`, wantEvalReason: ReasonCostLimit},
		},
		"string_last_index_of_string": {
			{name: "lastIndexOf_64k_ok", expr: `s.lastIndexOf("aaaaab") == -1`},
			{name: "lastIndexOf_kmp_worst_case_fast_cost_limit", expr: `string(event.kmpH).lastIndexOf(string(event.kmpN)) == -1`, wantEvalReason: ReasonCostLimit},
		},
		"string_last_index_of_string_int": {
			{name: "lastIndexOf_offset_64k_ok", expr: `s.lastIndexOf("aaaaab", 60000) == -1`},
			{name: "lastIndexOf_offset_kmp_worst_case_fast_cost_limit", expr: `string(event.kmpH).lastIndexOf(string(event.kmpN), 200000) == -1`, wantEvalReason: ReasonCostLimit},
		},
		"string_lower_ascii":       {{name: "lowerAscii_64k", expr: `s.lowerAscii().size() == 65536`}},
		"string_upper_ascii":       {{name: "upperAscii_64k", expr: `s.upperAscii().size() == 65536`}},
		"string_reverse":           {{name: "reverse_64k", expr: `s.reverse().size() == 65536`}},
		"string_trim":              {{name: "trim_64k_spaces", expr: `string(event.spaces64k).trim() == "x"`}},
		"string_substring_int":     {{name: "substring_2arg_64k", expr: `s.substring(32768).size() == 32768`}},
		"string_substring_int_int": {{name: "substring_3arg_64k", expr: `s.substring(100, 65500).size() == 65400`}},
		"string_replace_string_string": {
			{name: "replace_3arg_64k_ok", expr: `s.replace("b", "c").size() == 65536`},
			{name: "replace_3arg_amplifying_64k", expr: `s.replace("a", s).size() > 0`, wantEvalReason: ReasonCostLimit},
			{name: "replace_3arg_576k_prime_rk_collision_fast_cost_limit", expr: `string(event.rkH).replace(string(event.rkN), "x").size() > 0`, wantEvalReason: ReasonCostLimit},
		},
		"string_replace_string_string_int": {
			{name: "replace_4arg_64k_ok", expr: `s.replace("b", "c", 5).size() == 65536`},
			{name: "replace_4arg_amplifying_64k", expr: `s.replace("a", s, 20).size() > 0`, wantEvalReason: ReasonCostLimit},
			{name: "replace_4arg_576k_prime_rk_collision_fast_cost_limit", expr: `string(event.rkH).replace(string(event.rkN), "x", 5).size() > 0`, wantEvalReason: ReasonCostLimit},
		},
		"string_split_string": {
			{name: "split_2arg_64k_chars", expr: `s.split("").size() == 65536`},
			{name: "split_2arg_over_max_elems", expr: `(s + "a").split("").size() > 0`, wantEvalReason: ReasonCostLimit},
			{name: "split_2arg_prime_rk_collision_linear", expr: `string(event.rkH64).split(string(event.rkN72)).size() == 1`},
		},
		"string_split_string_int": {
			{name: "split_3arg_over_max_elems", expr: `(s + "a").split("", 70000).size() > 0`, wantEvalReason: ReasonCostLimit},
			{name: "split_3arg_prime_rk_collision_linear", expr: `string(event.rkH64).split(string(event.rkN72), 10).size() == 1`},
		},
		"list_join": {
			{name: "join_1arg_amplifying", expr: `s.split("").join().size() == 65536`},
			{name: "join_1arg_non_string_dag_elem_safe", expr: `event.badList.join() == ""`, wantEvalReason: ReasonRuntime},
		},
		"list_join_string": {
			{name: "join_2arg_64k_squared_rejected", expr: `s.split("").join(s).size() > 0`, wantEvalReason: ReasonCostLimit},
		},
		"string_format": {
			{name: "format_amplifying_list_rejected", expr: `"%s".format([[s, s, s, s, s, s, s, s, s, s, s, s, s, s, s, s, s]]).size() > 0`, wantEvalReason: ReasonCostLimit},
			{name: "format_extreme_double_bounded", expr: `"%.100f".format([d]).size() > 300`},
		},
		"strings_quote": {
			{name: "quote_64k_quotes", expr: `strings.quote(string(event.quotes64k)).size() == 131074`},
			{name: "quote_over_1mib_rejected", expr: `[string(event.quotes64k)].map(a, [a + a].map(b, [b + b].map(c, [c + c].map(d, strings.quote(d + d)))))[0][0][0][0].size() > 0`, wantEvalReason: ReasonCostLimit},
		},
	}

	// Only parser-inaccessible internal/legacy CEL operator aliases are exempt,
	// each with an explicit non-empty justification.
	exemptFnOverloads := map[string]surfaceExemption{
		"@not_strictly_false/not_strictly_false": {
			justification: "Internal comprehension predicate operator emitted only by macro expansion (exercised via macroProbes for all/exists/exists_one); '@' is not callable in CEL surface syntax.",
		},
		"__not_strictly_false__/__not_strictly_false__": {
			justification: "Legacy internal comprehension predicate alias for @not_strictly_false; exercised via comprehension macros.",
		},
		"_in_/in_list": {
			justification: "Legacy CEL v0 operator alias superseded by @in (parser always emits @in for the 'in' keyword; runtime boundedIn decorates operators.In, operators.OldIn, and overloads.DeprecatedIn identically).",
		},
		"_in_/in_map": {
			justification: "Legacy CEL v0 operator alias superseded by @in (parser always emits @in for the 'in' keyword; runtime boundedIn decorates operators.In, operators.OldIn, and overloads.DeprecatedIn identically).",
		},
		"in/in_list": {
			justification: "Deprecated CEL function alias superseded by @in ('in' is a reserved keyword in the CEL grammar; runtime boundedIn decorates overloads.DeprecatedIn identically).",
		},
		"in/in_map": {
			justification: "Deprecated CEL function alias superseded by @in ('in' is a reserved keyword in the CEL grammar; runtime boundedIn decorates overloads.DeprecatedIn identically).",
		},
	}

	// 2. Macro keys -> >=1 max-admitted-input adversarial probe(s).
	macroProbes := map[string][]adversarialProbe{
		"has:1:false": {
			{name: "has_present_and_missing", expr: `has(event.s64k) && !has(event.nonexistent)`},
		},
		"all:2:true": {
			{name: "all_cartesian_bounded", expr: `strList.all(a, strList.all(b, (a + b).size() > 0))`, wantEvalReasons: []string{ReasonCostLimit, ReasonTimeout}},
		},
		"exists:2:true": {
			{name: "exists_cartesian_bounded", expr: `strList.exists(a, strList.exists(b, a == "never" && b == "never"))`, wantEvalReasons: []string{ReasonCostLimit, ReasonTimeout}},
		},
		"exists_one:2:true": {
			{name: "exists_one_8k_list", expr: `intList.exists_one(x, x == 4096)`},
		},
		"filter:2:true": {
			{name: "filter_8k_list", expr: `intList.filter(x, x < 10).size() == 10`},
		},
		"map:2:true": {
			{name: "map_2arg_amplifying_cost_bounded", expr: `strList.map(x, x + s).size() > 0`, wantEvalReason: ReasonCostLimit},
		},
		"map:3:true": {
			{name: "map_3arg_filtered_8k_list", expr: `intList.map(x, x < 5, x * 2).size() == 5`},
		},
	}

	// Verify 100% coverage of every (fnName, overloadID) in celEnv.Functions().
	coveredFnNames := make(map[string]bool)
	liveFnOverloads := make(map[string]bool)
	liveNonExemptOverloadIDs := make(map[string]bool)
	for fnName, decl := range eng.celEnv.Functions() {
		ovDecls := decl.OverloadDecls()
		if len(ovDecls) == 0 {
			t.Errorf("function %q in celEnv.Functions() has 0 overload declarations", fnName)
		}
		for _, ov := range ovDecls {
			qualKey := fnName + "/" + ov.ID()
			liveFnOverloads[qualKey] = true
			if ex, isExempt := exemptFnOverloads[qualKey]; isExempt {
				if strings.TrimSpace(ex.justification) == "" {
					t.Errorf("exempt overload %q must have a non-empty justification", qualKey)
				}
				coveredFnNames[fnName] = true
				continue
			}
			liveNonExemptOverloadIDs[ov.ID()] = true
			probes, hasProbes := overloadProbes[ov.ID()]
			if !hasProbes || len(probes) == 0 {
				t.Errorf("UNAUDITED OVERLOAD: function %q overload %q has no adversarial probe and no justified exemption", fnName, ov.ID())
				continue
			}
			coveredFnNames[fnName] = true
		}
	}
	for ovID := range overloadProbes {
		if !liveNonExemptOverloadIDs[ovID] {
			t.Errorf("stale overload probe %q does not match any non-exempt overload in celEnv.Functions()", ovID)
		}
	}
	for qualKey := range exemptFnOverloads {
		if !liveFnOverloads[qualKey] {
			t.Errorf("stale exemption %q does not match any live fn/overload in celEnv.Functions()", qualKey)
		}
	}

	// Verify 100% coverage of every macro key in celEnv.Macros().
	liveMacroKeys := make(map[string]bool)
	for _, m := range eng.celEnv.Macros() {
		mk := m.MacroKey()
		liveMacroKeys[mk] = true
		probes, ok := macroProbes[mk]
		if !ok || len(probes) == 0 {
			t.Errorf("UNAUDITED MACRO: macro %q (function %q) has no adversarial probe", mk, m.Function())
			continue
		}
		coveredFnNames[m.Function()] = true
	}
	for mk := range macroProbes {
		if !liveMacroKeys[mk] {
			t.Errorf("stale macro probe key %q does not match any live macro in celEnv.Macros()", mk)
		}
	}

	// Verify 100% coverage of the pinned Engine.Functions() allow-list itself.
	for _, fnName := range eng.Functions() {
		if !coveredFnNames[fnName] {
			t.Errorf("UNAUDITED ALLOW-LIST ENTRY: Engine.Functions() entry %q is neither probed nor exempted", fnName)
		}
	}

	// Execute every adversarial overload & macro probe under DefaultLimits()
	// (with EvalTimeout raised to the 1s ceiling for 64 KiB happy-path probes
	// when running under -race so 2-vCPU race runs do not flake).
	boolKind := Kind{Name: "filter", Result: Bool()}
	ctx := context.Background()
	evalTimeout := DefaultLimits().EvalTimeout
	happyEng := eng
	happyEvalTimeout := evalTimeout
	maxErrElapsed := 2 * evalTimeout
	if raceEnabled {
		happyLim := DefaultLimits()
		happyLim.EvalTimeout = MaxEvalTimeoutCeiling
		var err error
		happyEng, err = New(eng.Environment(), happyLim, DefaultCacheConfig())
		if err != nil {
			t.Fatalf("New happyEng: %v", err)
		}
		happyEvalTimeout = happyLim.EvalTimeout
		maxErrElapsed = 5 * time.Second
	}
	const maxAllocDelta = 64 << 20 // 64 MiB

	runProbe := func(t *testing.T, group string, p adversarialProbe) {
		t.Helper()
		t.Run(group+"/"+p.name, func(t *testing.T) {
			isErrProbe := p.wantEvalReason != "" || len(p.wantEvalReasons) > 0
			targetEng := happyEng
			if p.wantCompileCat != "" || isErrProbe {
				targetEng = eng
			}
			prog, compErr := targetEng.Compile(p.expr, boolKind)
			if p.wantCompileCat != "" {
				if compErr == nil {
					t.Fatalf("Compile(%s): expected CompileError(%s), got nil", p.expr, p.wantCompileCat)
				}
				var cerr *CompileError
				if !errors.As(compErr, &cerr) || cerr.Category != p.wantCompileCat {
					t.Fatalf("Compile(%s): got %T (%v), want Category=%q", p.expr, compErr, compErr, p.wantCompileCat)
				}
				return
			}
			if compErr != nil {
				t.Fatalf("Compile(%s) unexpected error: %v", p.expr, compErr)
			}

			var mBefore, mAfter runtime.MemStats
			runtime.ReadMemStats(&mBefore)
			start := time.Now()
			res, stats, evalErr := prog.Eval(ctx, act)
			elapsed := time.Since(start)
			runtime.ReadMemStats(&mAfter)

			allocDelta := mAfter.TotalAlloc - mBefore.TotalAlloc
			if allocDelta > maxAllocDelta {
				t.Errorf("TotalAlloc delta %d bytes (%.2f MiB) exceeds 64 MiB bound", allocDelta, float64(allocDelta)/(1<<20))
			}

			if isErrProbe {
				allowed := slices.Clone(p.wantEvalReasons)
				if p.wantEvalReason != "" {
					allowed = append([]string{p.wantEvalReason}, allowed...)
				}
				if raceEnabled && slices.Contains(allowed, ReasonCostLimit) && !slices.Contains(allowed, ReasonTimeout) {
					allowed = append(allowed, ReasonTimeout)
				}
				if res != nil {
					t.Fatalf("expected nil Result on %v error, got %v", allowed, res)
				}
				var eerr *EvalError
				if !errors.As(evalErr, &eerr) || !slices.Contains(allowed, eerr.Reason) {
					t.Fatalf("Eval(%s): got %T (%v), want EvalError.Reason in %v", p.expr, evalErr, evalErr, allowed)
				}
				if elapsed > maxErrElapsed {
					t.Errorf("error-path elapsed %s exceeds wall-clock bound (%s)", elapsed, maxErrElapsed)
				}
				return
			}

			if evalErr != nil {
				t.Fatalf("Eval(%s) unexpected error: %v", p.expr, evalErr)
			}
			if res == nil {
				t.Fatalf("Eval(%s) returned nil Result with nil error", p.expr)
			}
			got, bErr := res.Bool()
			if bErr != nil || !got {
				t.Fatalf("Eval(%s) = (%v, %v), want (true, nil)", p.expr, got, bErr)
			}
			if stats.Elapsed > happyEvalTimeout {
				t.Errorf("decision returned with stats.Elapsed=%s > EvalTimeout=%s", stats.Elapsed, happyEvalTimeout)
			}
			if elapsed > maxErrElapsed {
				t.Errorf("wall-clock elapsed %s exceeds wall-clock bound (%s)", elapsed, maxErrElapsed)
			}
		})
	}

	ovKeys := make([]string, 0, len(overloadProbes))
	for k := range overloadProbes {
		ovKeys = append(ovKeys, k)
	}
	slices.Sort(ovKeys)
	for _, k := range ovKeys {
		for _, p := range overloadProbes[k] {
			runProbe(t, "overload_"+k, p)
		}
	}

	mKeys := make([]string, 0, len(macroProbes))
	for k := range macroProbes {
		mKeys = append(mKeys, k)
	}
	slices.Sort(mKeys)
	for _, k := range mKeys {
		for _, p := range macroProbes[k] {
			runProbe(t, "macro_"+k, p)
		}
	}
}
