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
	"container/list"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata"
	"unicode"
	"unicode/utf8"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/cost"
	"cel.dev/cel-go/common/functions"
	"cel.dev/cel-go/common/operators"
	"cel.dev/cel-go/common/overloads"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/common/types/traits"
	"cel.dev/cel-go/ext"
	"cel.dev/cel-go/interpreter"
)

const (
	maxPinnedFormatPrec  = 100
	rabinKarpPatMinBytes = 64
	rabinKarpWorkCutoff  = 4 << 20 // 4 MiB-comparisons before switching to linear KMP
	maxCachedTimezones   = 1024
	maxCachedTzNameBytes = 64
	tzLookupErrorCost    = 250
	walkInterruptStride  = 1024
	walkCostFlushStride  = 4096
)

func abortOutputLimit() {
	panic(interpreter.EvalCancelledError{
		Cause:   interpreter.CostLimitExceeded,
		Message: "operation cancelled: output size limit exceeded",
	})
}

func buildRawFormatProgram() (cel.Program, error) {
	rawEnv, err := cel.NewEnv(
		ext.Strings(ext.StringsVersion(pinnedStringsExtVersion)),
		cel.Variable("_fmt", cel.StringType),
		cel.Variable("_args", cel.ListType(cel.DynType)),
	)
	if err != nil {
		return nil, fmt.Errorf("celengine: build raw format env: %w", err)
	}
	ast, iss := rawEnv.Compile(`_fmt.format(_args)`)
	if iss != nil && iss.Err() != nil {
		return nil, fmt.Errorf("celengine: compile raw format helper: %w", iss.Err())
	}
	prog, err := rawEnv.Program(ast)
	if err != nil {
		return nil, fmt.Errorf("celengine: plan raw format helper: %w", err)
	}
	return prog, nil
}

// outputBoundsDecorator wraps CEL call nodes whose output size, collection
// cardinality, or inner loop work can amplify beyond their inputs within a
// single uninterruptible host call. Exceeding Limits.MaxOutputBytes,
// Limits.MaxCollectionElements, Limits.MaxRegexProgramSize, or
// Limits.MaxRegexWork aborts evaluation immediately with
// interpreter.CostLimitExceeded (-> EvalError{Reason: ReasonCostLimit}) before
// allocating or executing the unbounded work, and cannot be swallowed by
// non-strict ||.
func outputBoundsDecorator(limits Limits, rawFormat cel.Program, tzCache *tzLRUCache) interpreter.InterpretableDecoratorV2 {
	maxBytes := limits.MaxOutputBytes
	maxElems := limits.MaxCollectionElements
	maxRegexProgSize := limits.MaxRegexProgramSize
	maxRegexWork := limits.MaxRegexWork
	if maxBytes <= 0 {
		maxBytes = DefaultMaxOutputBytes
	}
	if maxElems <= 0 {
		maxElems = DefaultMaxCollectionElements
	}
	if maxRegexProgSize <= 0 {
		maxRegexProgSize = DefaultMaxRegexProgramSize
	}
	if maxRegexWork <= 0 {
		maxRegexWork = DefaultMaxRegexWork
	}
	if tzCache == nil {
		tzCache = newTZLRUCache(maxCachedTimezones)
	}

	return func(node interpreter.InterpretableV2) (interpreter.InterpretableV2, error) {
		call, ok := node.(interpreter.InterpretableCall)
		if !ok {
			return node, nil
		}
		arity := len(call.Args())
		var impl functions.FunctionOp

		switch call.Function() {
		case overloads.Matches:
			if arity == 2 {
				return newBoundedMatchesCall(call, maxBytes, maxRegexProgSize, maxRegexWork)
			}
		case "replace":
			if arity == 3 || arity == 4 {
				impl = boundedReplace(maxBytes)
			}
		case "join":
			if arity == 1 || arity == 2 {
				return &boundedJoinCall{
					InterpretableCall: call,
					maxBytes:          maxBytes,
					maxElems:          maxElems,
				}, nil
			}
		case "split":
			if arity == 2 || arity == 3 {
				impl = boundedSplit(maxBytes, maxElems)
			}
		case "format":
			if arity == 2 && rawFormat != nil {
				return &boundedFormatCall{
					InterpretableCall: call,
					maxBytes:          maxBytes,
					maxElems:          maxElems,
					rawFormat:         rawFormat,
				}, nil
			}
		case "strings.quote":
			if arity == 1 {
				impl = boundedQuote(maxBytes)
			}
		case operators.Add:
			if arity == 2 {
				return &boundedAddCall{
					InterpretableCall: call,
					maxBytes:          maxBytes,
					maxElems:          maxElems,
				}, nil
			}
		case "indexOf":
			if arity == 2 || arity == 3 {
				impl = boundedIndexOf(maxBytes)
			}
		case "lastIndexOf":
			if arity == 2 || arity == 3 {
				impl = boundedLastIndexOf(maxBytes)
			}
		case "charAt":
			if arity == 2 {
				impl = boundedCharAt(maxBytes)
			}
		case "lowerAscii":
			if arity == 1 {
				impl = boundedLowerASCII(maxBytes)
			}
		case "upperAscii":
			if arity == 1 {
				impl = boundedUpperASCII(maxBytes)
			}
		case "reverse":
			if arity == 1 {
				impl = boundedReverse(maxBytes)
			}
		case "substring":
			if arity == 2 || arity == 3 {
				impl = boundedSubstring(maxBytes)
			}
		case "trim":
			if arity == 1 {
				impl = boundedTrim(maxBytes)
			}
		case overloads.Contains:
			if arity == 2 {
				impl = boundedContains(maxBytes)
			}
		case overloads.StartsWith:
			if arity == 2 {
				impl = boundedStartsWith(maxBytes)
			}
		case overloads.EndsWith:
			if arity == 2 {
				impl = boundedEndsWith(maxBytes)
			}
		case overloads.Size:
			if arity == 1 {
				impl = boundedSize(maxBytes, maxElems)
			}
		case overloads.TypeConvertBytes:
			if arity == 1 {
				impl = boundedBytesConv(maxBytes)
			}
		case overloads.TypeConvertString:
			if arity == 1 {
				impl = boundedStringConv(maxBytes)
			}
		case overloads.TypeConvertBool:
			if arity == 1 {
				impl = boundedScalarStringInputConv(maxBytes, types.BoolType)
			}
		case overloads.TypeConvertDouble:
			if arity == 1 {
				impl = boundedScalarStringInputConv(maxBytes, types.DoubleType)
			}
		case overloads.TypeConvertDuration:
			if arity == 1 {
				impl = boundedScalarStringInputConv(maxBytes, types.DurationType)
			}
		case overloads.TypeConvertInt:
			if arity == 1 {
				impl = boundedScalarStringInputConv(maxBytes, types.IntType)
			}
		case overloads.TypeConvertTimestamp:
			if arity == 1 {
				impl = boundedScalarStringInputConv(maxBytes, types.TimestampType)
			}
		case overloads.TypeConvertUint:
			if arity == 1 {
				impl = boundedScalarStringInputConv(maxBytes, types.UintType)
			}
		case overloads.TimeGetFullYear,
			overloads.TimeGetMonth,
			overloads.TimeGetDayOfYear,
			overloads.TimeGetDayOfMonth,
			overloads.TimeGetDate,
			overloads.TimeGetDayOfWeek,
			overloads.TimeGetHours,
			overloads.TimeGetMinutes,
			overloads.TimeGetSeconds,
			overloads.TimeGetMilliseconds:
			if arity == 2 {
				return &boundedTimezoneCall{
					InterpretableCall: call,
					maxBytes:          maxBytes,
					tzCache:           tzCache,
				}, nil
			}
		case operators.Equals:
			if arity == 2 {
				return &boundedEqNeCall{
					InterpretableCall: call,
					wantEq:            true,
					maxBytes:          maxBytes,
					maxElems:          maxElems,
				}, nil
			}
		case operators.NotEquals:
			if arity == 2 {
				return &boundedEqNeCall{
					InterpretableCall: call,
					wantEq:            false,
					maxBytes:          maxBytes,
					maxElems:          maxElems,
				}, nil
			}
		case operators.In, operators.OldIn, overloads.DeprecatedIn:
			if arity == 2 {
				return &boundedInCall{
					InterpretableCall: call,
					maxBytes:          maxBytes,
					maxElems:          maxElems,
				}, nil
			}
		}

		if impl == nil {
			return node, nil
		}
		return &interruptibleCall{
			InterpretableCall: interpreter.NewCall(call.ID(), call.Function(), call.OverloadID(), call.Args(), impl),
		}, nil
	}
}

const prechargedWalkOverloadID = "_celengine_precharged_walk"

type prechargedCall struct{}

func (prechargedCall) Function() string   { return prechargedWalkOverloadID }
func (prechargedCall) OverloadID() string { return prechargedWalkOverloadID }

func checkFrameInterrupt(frame *interpreter.ExecutionFrame) {
	if frame != nil && frame.CheckInterrupt() {
		panic(interpreter.EvalCancelledError{
			Cause:   interpreter.ContextCancelled,
			Message: "operation cancelled: context interrupted",
		})
	}
}

func chargePrecomputedCost(frame *interpreter.ExecutionFrame, id int64, callCost uint64) {
	if frame == nil {
		return
	}
	if costs := frame.CostTracker(); costs != nil {
		costs.EvalZeroArity(frame, id, prechargedCall{}, types.Uint(callCost))
	}
}

type interruptibleCall struct {
	interpreter.InterpretableCall
}

func (c *interruptibleCall) Eval(ctx interpreter.Activation) ref.Val {
	return interpreter.EvalActivation(ctx, c.Exec)
}

func (c *interruptibleCall) Exec(frame *interpreter.ExecutionFrame) ref.Val {
	checkFrameInterrupt(frame)
	return c.InterpretableCall.Exec(frame)
}

func boundedReplace(maxBytes int) functions.FunctionOp {
	return func(args ...ref.Val) ref.Val {
		sVal, ok := args[0].(types.String)
		if !ok {
			return types.MaybeNoSuchOverloadErr(args[0])
		}
		oldVal, ok := args[1].(types.String)
		if !ok {
			return types.MaybeNoSuchOverloadErr(args[1])
		}
		newVal, ok := args[2].(types.String)
		if !ok {
			return types.MaybeNoSuchOverloadErr(args[2])
		}
		limit := int64(-1)
		if len(args) == 4 {
			nVal, ok := args[3].(types.Int)
			if !ok {
				return types.MaybeNoSuchOverloadErr(args[3])
			}
			limit = int64(nVal)
		}

		str := string(sVal)
		oldStr := string(oldVal)
		newStr := string(newVal)

		if len(str) > maxBytes {
			abortOutputLimit()
		}
		if limit == 0 {
			return sVal
		}

		var k int64
		baseLen := int64(len(str))
		if oldStr == "" {
			var runeCount int64
			if utf8.ValidString(str) {
				runeCount = int64(utf8.RuneCountInString(str))
			} else {
				baseLen = 0
				for _, r := range str {
					runeCount++
					baseLen += int64(utf8.RuneLen(r))
				}
			}
			k = runeCount + 1
			if limit > 0 && limit < k {
				k = limit
			}
			if baseLen > int64(maxBytes) {
				abortOutputLimit()
			}
			if len(newStr) > 0 && k > (int64(maxBytes)-baseLen)/int64(len(newStr)) {
				abortOutputLimit()
			}
		} else if shouldUseLinearKMPBytes(len(str), len(oldStr)) {
			maxMatches := -1
			if limit > 0 {
				if limit > int64(len(str))+1 {
					maxMatches = len(str) + 1
				} else {
					maxMatches = int(limit)
				}
			}
			matches := kmpFindBytes(str, oldStr, maxMatches, 0)
			if len(matches) == 0 {
				return sVal
			}
			k = int64(len(matches))
			if len(newStr) > len(oldStr) {
				if len(newStr) > maxBytes {
					abortOutputLimit()
				}
				delta := int64(len(newStr) - len(oldStr))
				if k > (int64(maxBytes)-baseLen)/delta {
					abortOutputLimit()
				}
			}
			outLen := int(baseLen + k*int64(len(newStr)-len(oldStr)))
			if outLen > maxBytes {
				abortOutputLimit()
			}
			var sb strings.Builder
			sb.Grow(outLen)
			prev := 0
			for _, pos := range matches {
				sb.WriteString(str[prev:pos])
				sb.WriteString(newStr)
				prev = pos + len(oldStr)
			}
			sb.WriteString(str[prev:])
			return types.String(sb.String())
		} else {
			if limit < 0 {
				k = int64(strings.Count(str, oldStr))
			} else {
				rem := str
				for k < limit {
					idx := strings.Index(rem, oldStr)
					if idx < 0 {
						break
					}
					k++
					rem = rem[idx+len(oldStr):]
				}
			}
			if k == 0 {
				return sVal
			}
			if len(newStr) > len(oldStr) {
				if len(newStr) > maxBytes {
					abortOutputLimit()
				}
				delta := int64(len(newStr) - len(oldStr))
				if k > (int64(maxBytes)-baseLen)/delta {
					abortOutputLimit()
				}
			}
		}

		maxCount := int64(len(str)) + 1
		if limit > maxCount {
			limit = maxCount
		}
		var out string
		if limit < 0 {
			out = strings.ReplaceAll(str, oldStr, newStr)
		} else {
			out = strings.Replace(str, oldStr, newStr, int(limit))
		}
		if len(out) > maxBytes {
			abortOutputLimit()
		}
		return types.String(out)
	}
}

type boundedJoinCall struct {
	interpreter.InterpretableCall
	maxBytes int
	maxElems int
}

func (c *boundedJoinCall) Eval(ctx interpreter.Activation) ref.Val {
	return interpreter.EvalActivation(ctx, c.Exec)
}

func (c *boundedJoinCall) Exec(frame *interpreter.ExecutionFrame) ref.Val {
	checkFrameInterrupt(frame)
	args := c.Args()
	lVal := args[0].Exec(frame)
	if len(args) == 1 {
		switch lVal.(type) {
		case *types.Err:
			return lVal
		case *types.Unknown:
			chargePrecomputedCost(frame, c.ID(), 1)
			return lVal
		}
	} else {
		switch l := lVal.(type) {
		case *types.Err:
			return lVal
		case *types.Unknown:
			rVal := args[1].Exec(frame)
			if rUnk, ok := rVal.(*types.Unknown); ok {
				merged := types.MergeUnknowns(l, rUnk)
				chargePrecomputedCost(frame, c.ID(), 1)
				return merged
			}
			chargePrecomputedCost(frame, c.ID(), 1)
			return lVal
		}
	}

	sep := ""
	if len(args) == 2 {
		rVal := args[1].Exec(frame)
		switch rVal.(type) {
		case *types.Err:
			return rVal
		case *types.Unknown:
			chargePrecomputedCost(frame, c.ID(), 1)
			return rVal
		}
		sepVal, ok := rVal.(types.String)
		if !ok {
			chargePrecomputedCost(frame, c.ID(), 1)
			return types.LabelErrNode(c.ID(), types.MaybeNoSuchOverloadErr(rVal))
		}
		sep = string(sepVal)
	}

	list, ok := lVal.(traits.Lister)
	if !ok {
		chargePrecomputedCost(frame, c.ID(), 1)
		return types.LabelErrNode(c.ID(), types.MaybeNoSuchOverloadErr(lVal))
	}
	szVal, ok := list.Size().(types.Int)
	if !ok {
		chargePrecomputedCost(frame, c.ID(), 1)
		return types.LabelErrNode(c.ID(), types.NewErr("join: invalid list size"))
	}
	sz := int64(szVal)
	if sz < 0 || sz > int64(c.maxElems) {
		abortOutputLimit()
	}
	if sz == 0 {
		chargePrecomputedCost(frame, c.ID(), 1)
		return types.String("")
	}

	b := newWalkBudget(frame, c.ID(), int64(c.maxBytes), int64(c.maxElems))
	b.chargeElems(sz)
	if sz > 1 && len(sep) > 0 {
		if len(sep) > c.maxBytes || (sz-1) > int64(c.maxBytes)/int64(len(sep)) {
			abortOutputLimit()
		}
		b.chargeBytes((sz - 1) * int64(len(sep)))
	}

	for i := types.Int(0); i < szVal; i++ {
		b.tickInterrupt()
		elem := list.Get(i)
		str, ok := elem.(types.String)
		if !ok {
			b.finishCharge(1)
			return types.LabelErrNode(c.ID(), types.NewErr("join: invalid input type %s", elem.Type().TypeName()))
		}
		b.chargeBytes(int64(len(str)))
	}

	totalBytes := int(b.initBytes - b.remBytes)
	b.finishCharge(1)
	checkFrameInterrupt(frame)

	var sb strings.Builder
	sb.Grow(totalBytes)
	steps := 0
	for i := types.Int(0); i < szVal; i++ {
		steps++
		if steps >= walkInterruptStride {
			steps = 0
			checkFrameInterrupt(frame)
		}
		if i != 0 {
			sb.WriteString(sep)
		}
		sb.WriteString(string(list.Get(i).(types.String)))
	}
	return types.String(sb.String())
}

func boundedSplit(maxBytes, maxElems int) functions.FunctionOp {
	return func(args ...ref.Val) ref.Val {
		sVal, ok := args[0].(types.String)
		if !ok {
			return types.MaybeNoSuchOverloadErr(args[0])
		}
		sepVal, ok := args[1].(types.String)
		if !ok {
			return types.MaybeNoSuchOverloadErr(args[1])
		}
		limit := int64(-1)
		if len(args) == 3 {
			nVal, ok := args[2].(types.Int)
			if !ok {
				return types.MaybeNoSuchOverloadErr(args[2])
			}
			limit = int64(nVal)
		}

		str := string(sVal)
		sep := string(sepVal)
		if len(str) > maxBytes {
			abortOutputLimit()
		}
		if limit == 0 {
			return types.DefaultTypeAdapter.NativeToValue([]string{})
		}
		if limit == 1 {
			return types.DefaultTypeAdapter.NativeToValue([]string{str})
		}

		if sep != "" && shouldUseLinearKMPBytes(len(str), len(sep)) {
			maxMatches := -1
			if limit > 1 {
				if limit-1 > int64(len(str)) {
					maxMatches = len(str)
				} else {
					maxMatches = int(limit - 1)
				}
			}
			matches := kmpFindBytes(str, sep, maxMatches, maxElems)
			if int64(len(matches))+1 > int64(maxElems) {
				abortOutputLimit()
			}
			out := make([]string, 0, len(matches)+1)
			prev := 0
			for _, pos := range matches {
				out = append(out, str[prev:pos])
				prev = pos + len(sep)
			}
			out = append(out, str[prev:])
			return types.DefaultTypeAdapter.NativeToValue(out)
		}

		var partsCount int64
		if sep == "" {
			if str != "" {
				if utf8.ValidString(str) {
					partsCount = int64(utf8.RuneCountInString(str))
				} else {
					var explodedBytes int64
					for _, r := range str {
						partsCount++
						explodedBytes += int64(utf8.RuneLen(r))
					}
					if explodedBytes > int64(maxBytes) {
						abortOutputLimit()
					}
				}
				if limit > 0 && limit < partsCount {
					partsCount = limit
				}
			}
		} else {
			if limit < 0 {
				// Fast upper bound before scanning; only scan if str could plausibly exceed maxElems.
				if int64(len(str))/int64(len(sep))+1 > int64(maxElems) {
					partsCount = int64(strings.Count(str, sep)) + 1
				}
			} else {
				partsCount = 1
				rem := str
				for partsCount < limit {
					idx := strings.Index(rem, sep)
					if idx < 0 {
						break
					}
					partsCount++
					if partsCount > int64(maxElems) {
						abortOutputLimit()
					}
					rem = rem[idx+len(sep):]
				}
			}
		}
		if partsCount > int64(maxElems) {
			abortOutputLimit()
		}

		maxSplit := int64(len(str)) + 1
		if limit > maxSplit {
			limit = maxSplit
		}
		var out []string
		if limit < 0 {
			out = strings.Split(str, sep)
		} else {
			out = strings.SplitN(str, sep, int(limit))
		}
		return types.DefaultTypeAdapter.NativeToValue(out)
	}
}

func boundedQuote(maxBytes int) functions.FunctionOp {
	return func(args ...ref.Val) ref.Val {
		sVal, ok := args[0].(types.String)
		if !ok {
			return types.MaybeNoSuchOverloadErr(args[0])
		}
		str := string(sVal)
		if len(str) > maxBytes {
			abortOutputLimit()
		}

		outBytes := int64(2) // surrounding double quotes
		for _, c := range str {
			if !utf8.ValidRune(c) {
				c = utf8.RuneError
			}
			switch c {
			case '\a', '\b', '\f', '\n', '\r', '\t', '\v', '\\', '"':
				outBytes += 2
			default:
				outBytes += int64(utf8.RuneLen(c))
			}
			if outBytes > int64(maxBytes) {
				abortOutputLimit()
			}
		}

		var sb strings.Builder
		sb.Grow(int(outBytes))
		sb.WriteByte('"')
		for _, c := range str {
			if !utf8.ValidRune(c) {
				c = utf8.RuneError
			}
			switch c {
			case '\a':
				sb.WriteString("\\a")
			case '\b':
				sb.WriteString("\\b")
			case '\f':
				sb.WriteString("\\f")
			case '\n':
				sb.WriteString("\\n")
			case '\r':
				sb.WriteString("\\r")
			case '\t':
				sb.WriteString("\\t")
			case '\v':
				sb.WriteString("\\v")
			case '\\':
				sb.WriteString("\\\\")
			case '"':
				sb.WriteString("\\\"")
			default:
				sb.WriteRune(c)
			}
		}
		sb.WriteByte('"')
		return types.String(sb.String())
	}
}

type boundedFormatCall struct {
	interpreter.InterpretableCall
	maxBytes  int
	maxElems  int
	rawFormat cel.Program
}

func (c *boundedFormatCall) Eval(ctx interpreter.Activation) ref.Val {
	return interpreter.EvalActivation(ctx, c.Exec)
}

func (c *boundedFormatCall) Exec(frame *interpreter.ExecutionFrame) ref.Val {
	checkFrameInterrupt(frame)
	args := c.Args()
	lVal := args[0].Exec(frame)
	switch l := lVal.(type) {
	case *types.Err:
		return lVal
	case *types.Unknown:
		rVal := args[1].Exec(frame)
		if rUnk, ok := rVal.(*types.Unknown); ok {
			merged := types.MergeUnknowns(l, rUnk)
			chargePrecomputedCost(frame, c.ID(), 1)
			return merged
		}
		chargePrecomputedCost(frame, c.ID(), 1)
		return lVal
	}
	rVal := args[1].Exec(frame)
	switch rVal.(type) {
	case *types.Err:
		return rVal
	case *types.Unknown:
		chargePrecomputedCost(frame, c.ID(), 1)
		return rVal
	}

	fmtVal, ok := lVal.(types.String)
	if !ok {
		chargePrecomputedCost(frame, c.ID(), 1)
		return types.LabelErrNode(c.ID(), types.MaybeNoSuchOverloadErr(lVal))
	}
	argList, ok := rVal.(traits.Lister)
	if !ok {
		chargePrecomputedCost(frame, c.ID(), 1)
		return types.LabelErrNode(c.ID(), types.MaybeNoSuchOverloadErr(rVal))
	}
	fmtStr := string(fmtVal)
	if len(fmtStr) > c.maxBytes {
		abortOutputLimit()
	}

	checkFormatOutputBound(frame, c.ID(), fmtStr, argList, c.maxBytes, c.maxElems)
	checkFrameInterrupt(frame)

	out, _, err := c.rawFormat.Eval(map[string]any{
		"_fmt":  fmtVal,
		"_args": argList,
	})
	if err != nil {
		if types.IsError(out) {
			return types.LabelErrNode(c.ID(), out)
		}
		return types.LabelErrNode(c.ID(), types.WrapErr(err))
	}
	if s, ok := out.(types.String); ok && len(s) > c.maxBytes {
		abortOutputLimit()
	}
	return types.LabelErrNode(c.ID(), out)
}

type formatBudget struct {
	frame       *interpreter.ExecutionFrame
	nodeID      int64
	initBytes   int64
	initElems   int64
	remBytes    int64
	remElems    int64
	flushedCost uint64
	steps       int
}

func newWalkBudget(frame *interpreter.ExecutionFrame, nodeID int64, maxBytes, maxElems int64) formatBudget {
	return formatBudget{
		frame:     frame,
		nodeID:    nodeID,
		initBytes: maxBytes,
		initElems: maxElems,
		remBytes:  maxBytes,
		remElems:  maxElems,
	}
}

func (b *formatBudget) tickInterrupt() {
	b.steps++
	if b.steps >= walkInterruptStride {
		b.steps = 0
		checkFrameInterrupt(b.frame)
	}
}

func (b *formatBudget) currentWalkCost() uint64 {
	usedElems := uint64(b.initElems - b.remElems)
	usedBytes := int(b.initBytes - b.remBytes)
	return cost.SafeAdd(usedElems, byteScanCost(usedBytes))
}

func (b *formatBudget) maybeFlush() {
	if b.frame == nil {
		return
	}
	cur := b.currentWalkCost()
	if cur > b.flushedCost && cur-b.flushedCost >= walkCostFlushStride {
		delta := cur - b.flushedCost
		b.flushedCost = cur
		chargePrecomputedCost(b.frame, b.nodeID, delta)
	}
}

func (b *formatBudget) finishCharge(extraCost uint64) {
	if b.frame == nil {
		return
	}
	total := cost.SafeAdd(extraCost, b.currentWalkCost())
	if total <= b.flushedCost {
		return
	}
	delta := total - b.flushedCost
	b.flushedCost = total
	chargePrecomputedCost(b.frame, b.nodeID, delta)
}

func (b *formatBudget) chargeBytes(n int64) {
	b.remBytes -= n
	if b.remBytes < 0 {
		abortOutputLimit()
	}
	b.maybeFlush()
}

func (b *formatBudget) chargeElems(n int64) {
	b.remElems -= n
	if b.remElems < 0 {
		abortOutputLimit()
	}
	b.maybeFlush()
}

func checkFormatOutputBound(frame *interpreter.ExecutionFrame, nodeID int64, fmtStr string, argList traits.Lister, maxBytes, maxElems int) {
	baseExtra := cost.SafeAdd(1, byteScanCost(len(fmtStr)))
	szVal, ok := argList.Size().(types.Int)
	if !ok {
		chargePrecomputedCost(frame, nodeID, baseExtra)
		return
	}
	argCount := int64(szVal)
	if argCount < 0 {
		abortOutputLimit()
	}

	b := newWalkBudget(frame, nodeID, int64(maxBytes), int64(maxElems))
	finish := func() {
		b.finishCharge(baseExtra)
	}

	var argIdx int64
	for i := 0; i < len(fmtStr); {
		b.tickInterrupt()
		if fmtStr[i] != '%' {
			b.chargeBytes(1)
			i++
			continue
		}
		if i+1 < len(fmtStr) && fmtStr[i+1] == '%' {
			b.chargeBytes(1)
			i += 2
			continue
		}
		if i+1 >= len(fmtStr) || argIdx >= argCount {
			// Malformed format string or missing arg; let rawFormat produce the exact CEL error.
			finish()
			return
		}
		j := i + 1
		prec := 6
		if fmtStr[j] == '.' {
			j++
			startDigits := j
			precVal := 0
			for j < len(fmtStr) && fmtStr[j] >= '0' && fmtStr[j] <= '9' {
				if precVal <= maxPinnedFormatPrec {
					precVal = precVal*10 + int(fmtStr[j]-'0')
				}
				j++
			}
			if j >= len(fmtStr) || j == startDigits || precVal > maxPinnedFormatPrec {
				finish()
				return
			}
			prec = precVal
		}
		verb := fmtStr[j]
		j++
		arg := argList.Get(types.Int(argIdx))
		argIdx++
		i = j

		switch verb {
		case 's':
			measureFormatStringArg(&b, arg, 0)
		case 'd':
			b.chargeBytes(upperBoundDecimalBytes(arg))
		case 'f':
			b.chargeBytes(upperBoundDecimalBytes(arg) + 1 + int64(prec))
		case 'e':
			b.chargeBytes(int64(prec) + 16)
		case 'b':
			b.chargeBytes(65)
		case 'x', 'X':
			switch v := arg.(type) {
			case types.String:
				b.chargeBytes(2 * int64(len(v)))
			case types.Bytes:
				b.chargeBytes(2 * int64(len(v)))
			default:
				b.chargeBytes(17)
			}
		case 'o':
			b.chargeBytes(23)
		default:
			finish()
			return
		}
	}
	finish()
}

func upperBoundDecimalBytes(arg ref.Val) int64 {
	if d, ok := arg.(types.Double); ok {
		fv := float64(d)
		if !math.IsNaN(fv) && !math.IsInf(fv, 0) && fv != 0 {
			abs := math.Abs(fv)
			if abs < 1e-9 || abs >= 1e20 {
				return 340
			}
		}
		return 32
	}
	return 20
}

func measureFormatStringArg(b *formatBudget, arg ref.Val, depth int) {
	if depth > MaxContainerDepth {
		abortOutputLimit()
	}
	b.tickInterrupt()
	switch v := arg.(type) {
	case types.String:
		b.chargeBytes(int64(len(v)))
	case types.Bytes:
		b.chargeBytes(int64(len(v)))
	case types.Bool:
		b.chargeBytes(5)
	case types.Int, types.Uint:
		b.chargeBytes(20)
	case types.Double:
		b.chargeBytes(upperBoundDecimalBytes(arg))
	case types.Duration, types.Timestamp:
		b.chargeBytes(35)
	case traits.Lister:
		szVal, ok := v.Size().(types.Int)
		if !ok || szVal < 0 {
			abortOutputLimit()
		}
		sz := int64(szVal)
		b.chargeElems(sz)
		b.chargeBytes(2) // "[" and "]"
		if sz > 1 {
			b.chargeBytes((sz - 1) * 2) // ", "
		}
		for idx := types.Int(0); idx < szVal; idx++ {
			measureFormatStringArg(b, v.Get(idx), depth+1)
		}
	case traits.Mapper:
		szVal, ok := v.Size().(types.Int)
		if !ok || szVal < 0 {
			abortOutputLimit()
		}
		sz := int64(szVal)
		b.chargeElems(sz)
		b.chargeBytes(2 + sz*2) // "{", "}", and ": " per entry
		if sz > 1 {
			b.chargeBytes((sz - 1) * 2) // ", "
		}
		it := v.Iterator()
		for it.HasNext() == types.True {
			k := it.Next()
			measureFormatStringArg(b, k, depth+1)
			if mv, found := v.Find(k); found {
				measureFormatStringArg(b, mv, depth+1)
			}
		}
	default:
		b.chargeBytes(32)
	}
}

type boundedAddCall struct {
	interpreter.InterpretableCall
	maxBytes int
	maxElems int
}

func (c *boundedAddCall) Eval(ctx interpreter.Activation) ref.Val {
	return interpreter.EvalActivation(ctx, c.Exec)
}

func (c *boundedAddCall) Exec(frame *interpreter.ExecutionFrame) ref.Val {
	checkFrameInterrupt(frame)
	args := c.Args()
	lVal := args[0].Exec(frame)
	switch l := lVal.(type) {
	case *types.Err:
		return lVal
	case *types.Unknown:
		rVal := args[1].Exec(frame)
		if rUnk, ok := rVal.(*types.Unknown); ok {
			merged := types.MergeUnknowns(l, rUnk)
			chargePrecomputedCost(frame, c.ID(), 1)
			return merged
		}
		chargePrecomputedCost(frame, c.ID(), 1)
		return lVal
	}
	rVal := args[1].Exec(frame)
	switch rVal.(type) {
	case *types.Err:
		return rVal
	case *types.Unknown:
		chargePrecomputedCost(frame, c.ID(), 1)
		return rVal
	}

	switch l := lVal.(type) {
	case types.String:
		if r, ok := rVal.(types.String); ok {
			if len(l) > c.maxBytes || len(r) > c.maxBytes-len(l) {
				abortOutputLimit()
			}
			chargePrecomputedCost(frame, c.ID(), atLeastOneCost(byteScanCost(len(l)+len(r))))
			return l + r
		}
	case types.Bytes:
		if r, ok := rVal.(types.Bytes); ok {
			if len(l) > c.maxBytes || len(r) > c.maxBytes-len(l) {
				abortOutputLimit()
			}
			chargePrecomputedCost(frame, c.ID(), atLeastOneCost(byteScanCost(len(l)+len(r))))
			return l.Add(r)
		}
	case traits.Lister:
		if r, ok := rVal.(traits.Lister); ok {
			lSz, ok1 := l.Size().(types.Int)
			rSz, ok2 := r.Size().(types.Int)
			if !ok1 || !ok2 || lSz < 0 || rSz < 0 || int64(lSz) > int64(c.maxElems) || int64(rSz) > int64(c.maxElems)-int64(lSz) {
				abortOutputLimit()
			}
			totalSz := int(lSz + rSz)
			// Charge result size BEFORE materializing so chained list `+` pays
			// O(depth * elements) and aborts at CostLimit before copying; flatten
			// into a slice-backed list so downstream Get(i) is O(1) without boxing.
			chargePrecomputedCost(frame, c.ID(), atLeastOneCost(uint64(totalSz)))
			if totalSz == 0 {
				return types.NewRefValList(types.DefaultTypeAdapter, nil)
			}
			elems := make([]ref.Val, totalSz)
			steps := 0
			outIdx := 0
			for i := types.Int(0); i < lSz; i++ {
				steps++
				if steps >= walkInterruptStride {
					steps = 0
					checkFrameInterrupt(frame)
				}
				elems[outIdx] = l.Get(i)
				outIdx++
			}
			for i := types.Int(0); i < rSz; i++ {
				steps++
				if steps >= walkInterruptStride {
					steps = 0
					checkFrameInterrupt(frame)
				}
				elems[outIdx] = r.Get(i)
				outIdx++
			}
			return types.NewRefValList(types.DefaultTypeAdapter, elems)
		}
	}

	var res ref.Val
	if adder, ok := lVal.(traits.Adder); ok {
		res = types.LabelErrNode(c.ID(), adder.Add(rVal))
	} else if lVal.Type().HasTrait(traits.ReceiverType) {
		res = types.LabelErrNode(c.ID(), lVal.(traits.Receiver).Receive(c.Function(), c.OverloadID(), []ref.Val{rVal}))
	} else {
		res = types.LabelErrNode(c.ID(), types.MaybeNoSuchOverloadErr(lVal))
	}
	chargePrecomputedCost(frame, c.ID(), 1)
	return res
}

func boundedIndexOf(maxBytes int) functions.FunctionOp {
	return func(args ...ref.Val) ref.Val {
		sVal, ok := args[0].(types.String)
		if !ok {
			return types.MaybeNoSuchOverloadErr(args[0])
		}
		subVal, ok := args[1].(types.String)
		if !ok {
			return types.MaybeNoSuchOverloadErr(args[1])
		}
		offset := int64(0)
		if len(args) == 3 {
			offVal, ok := args[2].(types.Int)
			if !ok {
				return types.MaybeNoSuchOverloadErr(args[2])
			}
			offset = int64(offVal)
		}

		str := string(sVal)
		sub := string(subVal)
		if len(str) > maxBytes || len(sub) > maxBytes {
			abortOutputLimit()
		}
		if offset < 0 {
			return types.NewErr("index out of range: %d", offset)
		}

		if sub == "" {
			runeLen := int64(utf8.RuneCountInString(str))
			if offset > runeLen {
				return types.Int(runeLen)
			}
			return types.Int(offset)
		}
		runes := []rune(str)
		if offset >= int64(len(runes)) {
			return types.Int(-1)
		}
		subRunes := []rune(sub)
		off := int(offset)
		if len(subRunes) > len(runes)-off {
			return types.Int(-1)
		}
		idx := kmpSearchRunes(runes[off:], subRunes, false)
		if idx < 0 {
			return types.Int(-1)
		}
		return types.Int(int64(off + idx))
	}
}

func boundedLastIndexOf(maxBytes int) functions.FunctionOp {
	return func(args ...ref.Val) ref.Val {
		sVal, ok := args[0].(types.String)
		if !ok {
			return types.MaybeNoSuchOverloadErr(args[0])
		}
		subVal, ok := args[1].(types.String)
		if !ok {
			return types.MaybeNoSuchOverloadErr(args[1])
		}
		str := string(sVal)
		sub := string(subVal)
		if len(str) > maxBytes || len(sub) > maxBytes {
			abortOutputLimit()
		}

		hasOffset := len(args) == 3
		var offset int64
		if hasOffset {
			offVal, ok := args[2].(types.Int)
			if !ok {
				return types.MaybeNoSuchOverloadErr(args[2])
			}
			offset = int64(offVal)
			if offset < 0 {
				return types.NewErr("index out of range: %d", offset)
			}
		}

		if sub == "" {
			runeLen := int64(utf8.RuneCountInString(str))
			if !hasOffset || offset > runeLen {
				return types.Int(runeLen)
			}
			return types.Int(offset)
		}
		runes := []rune(str)
		if len(runes) == 0 {
			return types.Int(-1)
		}
		if !hasOffset {
			if len(str) < len(sub) {
				return types.Int(-1)
			}
			offset = int64(len(runes) - 1)
		} else if offset >= int64(len(runes)) {
			return types.Int(-1)
		}

		subRunes := []rune(sub)
		maxStart := int(offset)
		if maxStart > len(runes)-len(subRunes) {
			maxStart = len(runes) - len(subRunes)
		}
		if maxStart < 0 {
			return types.Int(-1)
		}
		idx := kmpSearchRunes(runes[:maxStart+len(subRunes)], subRunes, true)
		return types.Int(int64(idx))
	}
}

// kmpSearchRunes finds either the first (findLast=false) or last (findLast=true)
// occurrence of pattern in text in O(len(text) + len(pattern)) time.
func kmpSearchRunes(text, pattern []rune, findLast bool) int {
	m := len(pattern)
	if m == 0 {
		if findLast {
			return len(text)
		}
		return 0
	}
	if len(text) < m {
		return -1
	}

	lps := make([]int32, m)
	for i, length := 1, int32(0); i < m; {
		if pattern[i] == pattern[length] {
			length++
			lps[i] = length
			i++
		} else if length != 0 {
			length = lps[length-1]
		} else {
			lps[i] = 0
			i++
		}
	}

	lastMatch := -1
	for i, j := 0, int32(0); i < len(text); {
		if text[i] == pattern[j] {
			i++
			j++
			if int(j) == m {
				matchIdx := i - m
				if !findLast {
					return matchIdx
				}
				lastMatch = matchIdx
				j = lps[j-1]
			}
		} else if j != 0 {
			j = lps[j-1]
		} else {
			i++
		}
	}
	return lastMatch
}

func boundedCharAt(maxBytes int) functions.FunctionOp {
	return func(args ...ref.Val) ref.Val {
		sVal, ok := args[0].(types.String)
		if !ok {
			return types.MaybeNoSuchOverloadErr(args[0])
		}
		indVal, ok := args[1].(types.Int)
		if !ok {
			return types.MaybeNoSuchOverloadErr(args[1])
		}
		str := string(sVal)
		if len(str) > maxBytes {
			abortOutputLimit()
		}
		ind := int64(indVal)
		if ind < 0 {
			return types.NewErr("index out of range: %d", ind)
		}
		var pos int64
		for _, r := range str {
			if pos == ind {
				return types.String(r)
			}
			pos++
		}
		if ind == pos {
			return types.String("")
		}
		return types.NewErr("index out of range: %d", ind)
	}
}

func boundedLowerASCII(maxBytes int) functions.FunctionOp {
	return func(args ...ref.Val) ref.Val {
		sVal, ok := args[0].(types.String)
		if !ok {
			return types.MaybeNoSuchOverloadErr(args[0])
		}
		str := string(sVal)
		if len(str) > maxBytes {
			abortOutputLimit()
		}
		runes := []rune(str)
		for i, r := range runes {
			if r <= unicode.MaxASCII {
				runes[i] = unicode.ToLower(r)
			}
		}
		out := string(runes)
		if len(out) > maxBytes {
			abortOutputLimit()
		}
		return types.String(out)
	}
}

func boundedUpperASCII(maxBytes int) functions.FunctionOp {
	return func(args ...ref.Val) ref.Val {
		sVal, ok := args[0].(types.String)
		if !ok {
			return types.MaybeNoSuchOverloadErr(args[0])
		}
		str := string(sVal)
		if len(str) > maxBytes {
			abortOutputLimit()
		}
		runes := []rune(str)
		for i, r := range runes {
			if r <= unicode.MaxASCII {
				runes[i] = unicode.ToUpper(r)
			}
		}
		out := string(runes)
		if len(out) > maxBytes {
			abortOutputLimit()
		}
		return types.String(out)
	}
}

func boundedReverse(maxBytes int) functions.FunctionOp {
	return func(args ...ref.Val) ref.Val {
		sVal, ok := args[0].(types.String)
		if !ok {
			return types.MaybeNoSuchOverloadErr(args[0])
		}
		str := string(sVal)
		if len(str) > maxBytes {
			abortOutputLimit()
		}
		chars := []rune(str)
		for i, j := 0, len(chars)-1; i < j; i, j = i+1, j-1 {
			chars[i], chars[j] = chars[j], chars[i]
		}
		out := string(chars)
		if len(out) > maxBytes {
			abortOutputLimit()
		}
		return types.String(out)
	}
}

func boundedSubstring(maxBytes int) functions.FunctionOp {
	return func(args ...ref.Val) ref.Val {
		sVal, ok := args[0].(types.String)
		if !ok {
			return types.MaybeNoSuchOverloadErr(args[0])
		}
		startVal, ok := args[1].(types.Int)
		if !ok {
			return types.MaybeNoSuchOverloadErr(args[1])
		}
		str := string(sVal)
		if len(str) > maxBytes {
			abortOutputLimit()
		}
		runes := []rune(str)
		l := int64(len(runes))
		start := int64(startVal)
		if len(args) == 2 {
			if start < 0 || start > l {
				return types.NewErr("index out of range: %d", start)
			}
			out := string(runes[start:])
			if len(out) > maxBytes {
				abortOutputLimit()
			}
			return types.String(out)
		}
		endVal, ok := args[2].(types.Int)
		if !ok {
			return types.MaybeNoSuchOverloadErr(args[2])
		}
		end := int64(endVal)
		if start > end {
			return types.NewErr("invalid substring range. start: %d, end: %d", start, end)
		}
		if start < 0 || start > l {
			return types.NewErr("index out of range: %d", start)
		}
		if end < 0 || end > l {
			return types.NewErr("index out of range: %d", end)
		}
		out := string(runes[start:end])
		if len(out) > maxBytes {
			abortOutputLimit()
		}
		return types.String(out)
	}
}

func boundedBytesConv(maxBytes int) functions.FunctionOp {
	return func(args ...ref.Val) ref.Val {
		switch v := args[0].(type) {
		case types.String:
			if len(v) > maxBytes {
				abortOutputLimit()
			}
		case types.Bytes:
			if len(v) > maxBytes {
				abortOutputLimit()
			}
		}
		return args[0].ConvertToType(types.BytesType)
	}
}

func boundedStringConv(maxBytes int) functions.FunctionOp {
	return func(args ...ref.Val) ref.Val {
		switch v := args[0].(type) {
		case types.String:
			if len(v) > maxBytes {
				abortOutputLimit()
			}
		case types.Bytes:
			if len(v) > maxBytes {
				abortOutputLimit()
			}
		}
		out := args[0].ConvertToType(types.StringType)
		if s, ok := out.(types.String); ok && len(s) > maxBytes {
			abortOutputLimit()
		}
		return out
	}
}

func isContainerVal(v ref.Val) bool {
	switch v.(type) {
	case traits.Lister, traits.Mapper:
		return true
	default:
		return false
	}
}

func checkContainerWalkBound(v ref.Val, b *formatBudget, depth int) {
	if depth > MaxContainerDepth {
		abortOutputLimit()
	}
	b.tickInterrupt()
	switch c := v.(type) {
	case types.String:
		b.chargeBytes(int64(len(c)))
	case types.Bytes:
		b.chargeBytes(int64(len(c)))
	case traits.Lister:
		szVal, ok := c.Size().(types.Int)
		if !ok || szVal < 0 {
			abortOutputLimit()
		}
		b.chargeElems(int64(szVal))
		for i := types.Int(0); i < szVal; i++ {
			checkContainerWalkBound(c.Get(i), b, depth+1)
		}
	case traits.Mapper:
		szVal, ok := c.Size().(types.Int)
		if !ok || szVal < 0 {
			abortOutputLimit()
		}
		b.chargeElems(int64(szVal))
		it := c.Iterator()
		for it.HasNext() == types.True {
			k := it.Next()
			checkContainerWalkBound(k, b, depth+1)
			if mv, found := c.Find(k); found {
				checkContainerWalkBound(mv, b, depth+1)
			}
		}
	}
}

type boundedEqNeCall struct {
	interpreter.InterpretableCall
	wantEq   bool
	maxBytes int
	maxElems int
}

func (c *boundedEqNeCall) Eval(ctx interpreter.Activation) ref.Val {
	return interpreter.EvalActivation(ctx, c.Exec)
}

func (c *boundedEqNeCall) Exec(frame *interpreter.ExecutionFrame) ref.Val {
	checkFrameInterrupt(frame)
	args := c.Args()
	lVal := args[0].Exec(frame)
	if types.IsError(lVal) {
		chargePrecomputedCost(frame, c.ID(), 1)
		return lVal
	}
	rVal := args[1].Exec(frame)
	if types.IsError(rVal) {
		chargePrecomputedCost(frame, c.ID(), 1)
		return rVal
	}
	if lUnk, okL := lVal.(*types.Unknown); okL {
		if rUnk, okR := rVal.(*types.Unknown); okR {
			merged := types.MergeUnknowns(lUnk, rUnk)
			chargePrecomputedCost(frame, c.ID(), 1)
			return merged
		}
		chargePrecomputedCost(frame, c.ID(), 1)
		return lVal
	}
	if types.IsUnknown(rVal) {
		chargePrecomputedCost(frame, c.ID(), 1)
		return rVal
	}

	if isContainerVal(lVal) || isContainerVal(rVal) {
		b := newWalkBudget(frame, c.ID(), int64(c.maxBytes)*2, int64(c.maxElems)*2)
		checkContainerWalkBound(lVal, &b, 0)
		checkContainerWalkBound(rVal, &b, 0)
		b.finishCharge(1)
	} else {
		callCost := uint64(1)
		if lStr, okL := lVal.(types.String); okL {
			if rStr, okR := rVal.(types.String); okR {
				callCost = atLeastOneCost(byteScanCost(min(len(lStr), len(rStr))))
			}
		} else if lBytes, okL := lVal.(types.Bytes); okL {
			if rBytes, okR := rVal.(types.Bytes); okR {
				callCost = atLeastOneCost(byteScanCost(min(len(lBytes), len(rBytes))))
			}
		}
		chargePrecomputedCost(frame, c.ID(), callCost)
	}

	checkFrameInterrupt(frame)
	eq := types.Equal(lVal, rVal)
	if c.wantEq {
		return eq
	}
	return types.Bool(eq != types.True)
}

type boundedInCall struct {
	interpreter.InterpretableCall
	maxBytes int
	maxElems int
}

func (c *boundedInCall) Eval(ctx interpreter.Activation) ref.Val {
	return interpreter.EvalActivation(ctx, c.Exec)
}

func (c *boundedInCall) Exec(frame *interpreter.ExecutionFrame) ref.Val {
	checkFrameInterrupt(frame)
	args := c.Args()
	lVal := args[0].Exec(frame)
	switch l := lVal.(type) {
	case *types.Err:
		return lVal
	case *types.Unknown:
		rVal := args[1].Exec(frame)
		if rUnk, ok := rVal.(*types.Unknown); ok {
			merged := types.MergeUnknowns(l, rUnk)
			chargePrecomputedCost(frame, c.ID(), 1)
			return merged
		}
		chargePrecomputedCost(frame, c.ID(), 1)
		return lVal
	}
	rVal := args[1].Exec(frame)
	switch rVal.(type) {
	case *types.Err:
		return rVal
	case *types.Unknown:
		chargePrecomputedCost(frame, c.ID(), 1)
		return rVal
	}

	if !rVal.Type().HasTrait(traits.ContainerType) {
		chargePrecomputedCost(frame, c.ID(), 1)
		return types.LabelErrNode(c.ID(), types.ValOrErr(rVal, "no such overload"))
	}

	if _, isList := rVal.(traits.Lister); isList || isContainerVal(lVal) {
		b := newWalkBudget(frame, c.ID(), int64(c.maxBytes)*2, int64(c.maxElems)*2)
		checkContainerWalkBound(lVal, &b, 0)
		checkContainerWalkBound(rVal, &b, 0)
		b.finishCharge(1)
	} else {
		callCost := uint64(1)
		if sKey, ok := lVal.(types.String); ok {
			callCost = atLeastOneCost(byteScanCost(len(sKey)))
		}
		chargePrecomputedCost(frame, c.ID(), callCost)
	}

	checkFrameInterrupt(frame)
	return types.LabelErrNode(c.ID(), rVal.(traits.Container).Contains(lVal))
}

func shouldUseLinearKMPBytes(textLen, patLen int) bool {
	return patLen > rabinKarpPatMinBytes && int64(textLen)*int64(patLen) > rabinKarpWorkCutoff
}

// kmpFindBytes returns non-overlapping start byte offsets of pattern in text
// using O(len(text) + len(pattern)) KMP so adversarial patterns longer than 64
// bytes cannot trigger Go stdlib IndexRabinKarp's fixed-PrimeRK hash-collision
// O(N*M) fallback. When limit >= 0, at most limit matches are returned; when
// abortAfter > 0, exceeding abortAfter matches aborts immediately.
func kmpFindBytes(text, pattern string, limit int, abortAfter int) []int {
	m := len(pattern)
	if m == 0 || len(text) < m || limit == 0 {
		return nil
	}
	lps := make([]int32, m)
	for i, length := 1, int32(0); i < m; {
		if pattern[i] == pattern[length] {
			length++
			lps[i] = length
			i++
		} else if length != 0 {
			length = lps[length-1]
		} else {
			lps[i] = 0
			i++
		}
	}

	var matches []int
	for i, j := 0, int32(0); i < len(text); {
		if text[i] == pattern[j] {
			i++
			j++
			if int(j) == m {
				matches = append(matches, i-m)
				if abortAfter > 0 && len(matches) > abortAfter {
					abortOutputLimit()
				}
				if limit > 0 && len(matches) >= limit {
					return matches
				}
				j = 0 // non-overlapping matches
			}
		} else if j != 0 {
			j = lps[j-1]
		} else {
			i++
		}
	}
	return matches
}

func indexStringLinear(s, sub string) int {
	if sub == "" {
		return 0
	}
	if len(s) < len(sub) {
		return -1
	}
	if !shouldUseLinearKMPBytes(len(s), len(sub)) {
		return strings.Index(s, sub)
	}
	matches := kmpFindBytes(s, sub, 1, 0)
	if len(matches) == 0 {
		return -1
	}
	return matches[0]
}

func boundedTrim(maxBytes int) functions.FunctionOp {
	return func(args ...ref.Val) ref.Val {
		sVal, ok := args[0].(types.String)
		if !ok {
			return types.MaybeNoSuchOverloadErr(args[0])
		}
		str := string(sVal)
		if len(str) > maxBytes {
			abortOutputLimit()
		}
		return types.String(strings.TrimSpace(str))
	}
}

func boundedContains(maxBytes int) functions.FunctionOp {
	return func(args ...ref.Val) ref.Val {
		sVal, ok := args[0].(types.String)
		if !ok {
			return types.MaybeNoSuchOverloadErr(args[0])
		}
		subVal, ok := args[1].(types.String)
		if !ok {
			return types.MaybeNoSuchOverloadErr(args[1])
		}
		str := string(sVal)
		sub := string(subVal)
		if len(str) > maxBytes || len(sub) > maxBytes {
			abortOutputLimit()
		}
		return types.Bool(indexStringLinear(str, sub) >= 0)
	}
}

func boundedStartsWith(maxBytes int) functions.FunctionOp {
	return func(args ...ref.Val) ref.Val {
		sVal, ok := args[0].(types.String)
		if !ok {
			return types.MaybeNoSuchOverloadErr(args[0])
		}
		preVal, ok := args[1].(types.String)
		if !ok {
			return types.MaybeNoSuchOverloadErr(args[1])
		}
		str := string(sVal)
		pre := string(preVal)
		if len(str) > maxBytes || len(pre) > maxBytes {
			abortOutputLimit()
		}
		return types.Bool(strings.HasPrefix(str, pre))
	}
}

func boundedEndsWith(maxBytes int) functions.FunctionOp {
	return func(args ...ref.Val) ref.Val {
		sVal, ok := args[0].(types.String)
		if !ok {
			return types.MaybeNoSuchOverloadErr(args[0])
		}
		sufVal, ok := args[1].(types.String)
		if !ok {
			return types.MaybeNoSuchOverloadErr(args[1])
		}
		str := string(sVal)
		suf := string(sufVal)
		if len(str) > maxBytes || len(suf) > maxBytes {
			abortOutputLimit()
		}
		return types.Bool(strings.HasSuffix(str, suf))
	}
}

func boundedSize(maxBytes, maxElems int) functions.FunctionOp {
	return func(args ...ref.Val) ref.Val {
		switch v := args[0].(type) {
		case types.String:
			if len(v) > maxBytes {
				abortOutputLimit()
			}
			return types.Int(utf8.RuneCountInString(string(v)))
		case types.Bytes:
			if len(v) > maxBytes {
				abortOutputLimit()
			}
			return types.Int(len(v))
		case traits.Lister:
			szVal, ok := v.Size().(types.Int)
			if !ok || szVal < 0 || int64(szVal) > int64(maxElems) {
				abortOutputLimit()
			}
			return szVal
		case traits.Mapper:
			szVal, ok := v.Size().(types.Int)
			if !ok || szVal < 0 || int64(szVal) > int64(maxElems) {
				abortOutputLimit()
			}
			return szVal
		case traits.Sizer:
			return v.Size()
		default:
			return types.MaybeNoSuchOverloadErr(args[0])
		}
	}
}

func boundedScalarStringInputConv(maxBytes int, targetType ref.Type) functions.FunctionOp {
	return func(args ...ref.Val) ref.Val {
		if s, ok := args[0].(types.String); ok && len(s) > maxBytes {
			abortOutputLimit()
		}
		return args[0].ConvertToType(targetType)
	}
}

type tzCacheEntry struct {
	key string
	loc *time.Location
	err error
}

type tzLRUCache struct {
	mu       sync.Mutex
	capacity int
	order    *list.List
	entries  map[string]*list.Element
}

func newTZLRUCache(capacity int) *tzLRUCache {
	if capacity <= 0 {
		capacity = maxCachedTimezones
	}
	return &tzLRUCache{
		capacity: capacity,
		order:    list.New(),
		entries:  make(map[string]*list.Element, 64),
	}
}

func (c *tzLRUCache) get(key string) (tzCacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	elem, ok := c.entries[key]
	if !ok {
		return tzCacheEntry{}, false
	}
	c.order.MoveToFront(elem)
	return elem.Value.(tzCacheEntry), true
}

func (c *tzLRUCache) put(key string, loc *time.Location, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if elem, ok := c.entries[key]; ok {
		elem.Value = tzCacheEntry{key: key, loc: loc, err: err}
		c.order.MoveToFront(elem)
		return
	}
	if c.order.Len() >= c.capacity {
		oldest := c.order.Back()
		if oldest != nil {
			oldEntry := oldest.Value.(tzCacheEntry)
			delete(c.entries, oldEntry.key)
			c.order.Remove(oldest)
		}
	}
	// Clone key so a substring of a large activation payload cannot pin the backing string.
	ownedKey := strings.Clone(key)
	elem := c.order.PushFront(tzCacheEntry{key: ownedKey, loc: loc, err: err})
	c.entries[ownedKey] = elem
}

func isSyntacticallyValidTZName(tz string) bool {
	if len(tz) == 0 || len(tz) > maxCachedTzNameBytes {
		return false
	}
	if tz[0] == '/' || tz[len(tz)-1] == '/' {
		return false
	}
	for i := 0; i < len(tz); i++ {
		ch := tz[i]
		if ch == '/' {
			if tz[i-1] == '/' {
				return false
			}
			continue
		}
		if (ch >= 'A' && ch <= 'Z') ||
			(ch >= 'a' && ch <= 'z') ||
			(ch >= '0' && ch <= '9') ||
			ch == '_' || ch == '+' || ch == '-' {
			continue
		}
		return false
	}
	return true
}

func truncateTZForErr(tz string) string {
	if len(tz) <= maxCachedTzNameBytes {
		return tz
	}
	return tz[:maxCachedTzNameBytes]
}

func parseCELFixedOffset(val string, colonIdx int) (*time.Location, error) {
	if len(val) > maxCachedTzNameBytes {
		return nil, fmt.Errorf("invalid timezone offset: %s", truncateTZForErr(val))
	}
	hr, err := strconv.Atoi(val[:colonIdx])
	if err != nil {
		return nil, err
	}
	minPart, err := strconv.Atoi(val[colonIdx+1:])
	if err != nil {
		return nil, err
	}
	if hr < -23 || hr > 23 {
		return nil, fmt.Errorf("timezone offset hours out of range [-23, 23]: %s", val)
	}
	if minPart < 0 || minPart > 59 {
		return nil, fmt.Errorf("timezone offset minutes out of range [0, 59]: %s", val)
	}
	var offsetMinutes int
	if val[0] == '-' {
		offsetMinutes = hr*60 - minPart
	} else {
		offsetMinutes = hr*60 + minPart
	}
	return time.FixedZone("", offsetMinutes*60), nil
}

func evalTimestampInLocation(fn string, t time.Time) (ref.Val, bool) {
	switch fn {
	case overloads.TimeGetFullYear:
		return types.Int(t.Year()), true
	case overloads.TimeGetMonth:
		return types.Int(t.Month() - 1), true
	case overloads.TimeGetDayOfYear:
		return types.Int(t.YearDay() - 1), true
	case overloads.TimeGetDayOfMonth:
		return types.Int(t.Day() - 1), true
	case overloads.TimeGetDate:
		return types.Int(t.Day()), true
	case overloads.TimeGetDayOfWeek:
		return types.Int(t.Weekday()), true
	case overloads.TimeGetHours:
		return types.Int(t.Hour()), true
	case overloads.TimeGetMinutes:
		return types.Int(t.Minute()), true
	case overloads.TimeGetSeconds:
		return types.Int(t.Second()), true
	case overloads.TimeGetMilliseconds:
		return types.Int(t.Nanosecond() / 1_000_000), true
	default:
		return nil, false
	}
}

type boundedTimezoneCall struct {
	interpreter.InterpretableCall
	maxBytes int
	tzCache  *tzLRUCache
}

func (c *boundedTimezoneCall) Eval(ctx interpreter.Activation) ref.Val {
	return interpreter.EvalActivation(ctx, c.Exec)
}

func (c *boundedTimezoneCall) Exec(frame *interpreter.ExecutionFrame) ref.Val {
	checkFrameInterrupt(frame)
	args := c.Args()
	lVal := args[0].Exec(frame)
	switch l := lVal.(type) {
	case *types.Err:
		return lVal
	case *types.Unknown:
		rVal := args[1].Exec(frame)
		if rUnk, ok := rVal.(*types.Unknown); ok {
			merged := types.MergeUnknowns(l, rUnk)
			chargePrecomputedCost(frame, c.ID(), 1)
			return merged
		}
		chargePrecomputedCost(frame, c.ID(), 1)
		return lVal
	}
	rVal := args[1].Exec(frame)
	switch rVal.(type) {
	case *types.Err:
		return rVal
	case *types.Unknown:
		chargePrecomputedCost(frame, c.ID(), 1)
		return rVal
	}

	ts, isTS := lVal.(types.Timestamp)
	if !isTS {
		var res ref.Val
		if recv, ok := lVal.(traits.Receiver); ok {
			res = types.LabelErrNode(c.ID(), recv.Receive(c.Function(), c.OverloadID(), []ref.Val{rVal}))
		} else {
			res = types.LabelErrNode(c.ID(), types.MaybeNoSuchOverloadErr(lVal))
		}
		chargePrecomputedCost(frame, c.ID(), 1)
		return res
	}

	tzVal, ok := rVal.(types.String)
	if !ok {
		chargePrecomputedCost(frame, c.ID(), 1)
		return types.LabelErrNode(c.ID(), types.MaybeNoSuchOverloadErr(rVal))
	}

	tz := string(tzVal)
	if len(tz) > c.maxBytes {
		abortOutputLimit()
	}

	fn := c.Function()
	if colonIdx := strings.IndexByte(tz, ':'); colonIdx >= 0 {
		loc, err := parseCELFixedOffset(tz, colonIdx)
		if err != nil {
			chargePrecomputedCost(frame, c.ID(), tzLookupErrorCost)
			return types.LabelErrNode(c.ID(), types.WrapErr(err))
		}
		chargePrecomputedCost(frame, c.ID(), 1)
		if out, handled := evalTimestampInLocation(fn, ts.In(loc)); handled {
			return out
		}
		return types.LabelErrNode(c.ID(), types.MaybeNoSuchOverloadErr(lVal))
	}

	if tz == "" || tz == "UTC" {
		chargePrecomputedCost(frame, c.ID(), 1)
		if out, handled := evalTimestampInLocation(fn, ts.In(time.UTC)); handled {
			return out
		}
		return types.LabelErrNode(c.ID(), types.MaybeNoSuchOverloadErr(lVal))
	}
	if tz == "Local" {
		chargePrecomputedCost(frame, c.ID(), 1)
		if out, handled := evalTimestampInLocation(fn, ts.In(time.Local)); handled {
			return out
		}
		return types.LabelErrNode(c.ID(), types.MaybeNoSuchOverloadErr(lVal))
	}

	if !isSyntacticallyValidTZName(tz) {
		chargePrecomputedCost(frame, c.ID(), tzLookupErrorCost)
		return types.LabelErrNode(c.ID(), types.NewErr("unknown time zone %s", truncateTZForErr(tz)))
	}

	if entry, hit := c.tzCache.get(tz); hit {
		if entry.err != nil {
			chargePrecomputedCost(frame, c.ID(), tzLookupErrorCost)
			return types.LabelErrNode(c.ID(), types.WrapErr(entry.err))
		}
		chargePrecomputedCost(frame, c.ID(), 1)
		if out, handled := evalTimestampInLocation(fn, ts.In(entry.loc)); handled {
			return out
		}
		return types.LabelErrNode(c.ID(), types.MaybeNoSuchOverloadErr(lVal))
	}

	// Uncached timezone lookup: pre-charge before consulting time.LoadLocation
	// (backed by embedded time/tzdata) and cache both positive and negative outcomes.
	chargePrecomputedCost(frame, c.ID(), tzLookupErrorCost)
	loc, err := time.LoadLocation(tz)
	c.tzCache.put(tz, loc, err)
	if err != nil {
		return types.LabelErrNode(c.ID(), types.WrapErr(err))
	}
	if out, handled := evalTimestampInLocation(fn, ts.In(loc)); handled {
		return out
	}
	return types.LabelErrNode(c.ID(), types.MaybeNoSuchOverloadErr(lVal))
}

// boundedMatchesCall wraps CEL's matches(string, string) in an InterpretableV2
// that deliberately does NOT implement interpreter.InterpretableCall. In
// cel-go, CustomDecoratorV2 runs before CompileRegexConstants (decRegexOptimizer,
// which type-asserts InterpretableCall and replaces constant-pattern matches
// nodes with a fresh NewCall). Returning a non-InterpretableCall node preserves
// both constant-pattern pre-compilation and pre-call input * program-size work
// bounds at evaluation time.
type boundedMatchesCall struct {
	id               int64
	origCall         interpreter.InterpretableCall
	lhs              interpreter.InterpretableV2
	rhs              interpreter.InterpretableV2
	compiled         *regexp.Regexp
	compiledProgSize int
	maxBytes         int
	maxRegexProgSize int
	maxRegexWork     int64
}

func newBoundedMatchesCall(
	call interpreter.InterpretableCall,
	maxBytes int,
	maxRegexProgSize int,
	maxRegexWork int64,
) (interpreter.InterpretableV2, error) {
	args := call.Args()
	node := &boundedMatchesCall{
		id:               call.ID(),
		origCall:         call,
		lhs:              args[0],
		rhs:              args[1],
		maxBytes:         maxBytes,
		maxRegexProgSize: maxRegexProgSize,
		maxRegexWork:     maxRegexWork,
	}
	if constPat, isConst := args[1].(interpreter.InterpretableConst); isConst {
		if patStr, ok := constPat.Value().(types.String); ok {
			pat := string(patStr)
			if len(pat) > maxBytes {
				abortOutputLimit()
			}
			progSize, err := types.RegexProgramSize(pat)
			if err != nil {
				return nil, err
			}
			if progSize > maxRegexProgSize {
				abortOutputLimit()
			}
			re, err := regexp.Compile(pat)
			if err != nil {
				return nil, err
			}
			node.compiled = re
			node.compiledProgSize = progSize
		}
	}
	return node, nil
}

func (m *boundedMatchesCall) ID() int64 {
	return m.id
}

func (m *boundedMatchesCall) Eval(ctx interpreter.Activation) ref.Val {
	return interpreter.EvalActivation(ctx, m.Exec)
}

func (m *boundedMatchesCall) Exec(frame *interpreter.ExecutionFrame) ref.Val {
	if frame != nil && frame.CheckInterrupt() {
		panic(interpreter.EvalCancelledError{
			Cause:   interpreter.ContextCancelled,
			Message: "operation cancelled: context interrupted",
		})
	}
	lVal := m.lhs.Exec(frame)
	switch l := lVal.(type) {
	case *types.Err:
		return lVal
	case *types.Unknown:
		rVal := m.rhs.Exec(frame)
		if rUnk, ok := rVal.(*types.Unknown); ok {
			merged := types.MergeUnknowns(l, rUnk)
			if costs := frame.CostTracker(); costs != nil {
				costs.EvalBinary(frame, m.id, m.origCall, lVal, rVal, merged)
			}
			return merged
		}
		if costs := frame.CostTracker(); costs != nil {
			costs.EvalBinary(frame, m.id, m.origCall, lVal, rVal, lVal)
		}
		return lVal
	}

	rVal := m.rhs.Exec(frame)
	switch rVal.(type) {
	case *types.Err:
		return rVal
	case *types.Unknown:
		if costs := frame.CostTracker(); costs != nil {
			costs.EvalBinary(frame, m.id, m.origCall, lVal, rVal, rVal)
		}
		return rVal
	}

	lStr, okL := lVal.(types.String)
	if !okL {
		res := types.MaybeNoSuchOverloadErr(lVal)
		if costs := frame.CostTracker(); costs != nil {
			costs.EvalBinary(frame, m.id, m.origCall, lVal, rVal, res)
		}
		return res
	}
	rStr, okR := rVal.(types.String)
	if !okR {
		res := types.MaybeNoSuchOverloadErr(rVal)
		if costs := frame.CostTracker(); costs != nil {
			costs.EvalBinary(frame, m.id, m.origCall, lVal, rVal, res)
		}
		return res
	}

	inStr := string(lStr)
	if len(inStr) > m.maxBytes {
		abortOutputLimit()
	}

	re := m.compiled
	progSize := m.compiledProgSize
	if re == nil {
		pat := string(rStr)
		if len(pat) > m.maxBytes {
			abortOutputLimit()
		}
		var err error
		progSize, err = types.RegexProgramSize(pat)
		if err != nil {
			res := types.WrapErr(err)
			if costs := frame.CostTracker(); costs != nil {
				costs.EvalBinary(frame, m.id, m.origCall, lVal, rVal, res)
			}
			return res
		}
		if progSize > m.maxRegexProgSize {
			abortOutputLimit()
		}
		if progSize > 0 && int64(len(inStr)) > m.maxRegexWork/int64(progSize) {
			abortOutputLimit()
		}
		re, err = regexp.Compile(pat)
		if err != nil {
			res := types.WrapErr(err)
			if costs := frame.CostTracker(); costs != nil {
				costs.EvalBinary(frame, m.id, m.origCall, lVal, rVal, res)
			}
			return res
		}
	} else if progSize > 0 && int64(len(inStr)) > m.maxRegexWork/int64(progSize) {
		abortOutputLimit()
	}

	// Charge the CostTracker before executing re.MatchString (the matches cost
	// depends only on input length and regex pattern/program size, never on the
	// boolean match result) so a call that exceeds CostLimit aborts pre-call.
	if costs := frame.CostTracker(); costs != nil {
		costs.EvalBinary(frame, m.id, m.origCall, lVal, rVal, types.False)
	}
	return types.Bool(re.MatchString(inStr))
}

func uint64Ptr(v uint64) *uint64 {
	return &v
}

func atLeastOneCost(v uint64) uint64 {
	if v == 0 {
		return 1
	}
	return v
}

func byteScanCost(n int) uint64 {
	if n <= 0 {
		return 0
	}
	return (uint64(n) + 9) / 10
}

func safeMulUint64(a, b uint64) uint64 {
	if a == 0 || b == 0 {
		return 0
	}
	if a > math.MaxUint64/b {
		return math.MaxUint64
	}
	return a * b
}

func measureContainerCost(v ref.Val, remElems *int64, depth int) uint64 {
	if depth > MaxContainerDepth || *remElems < 0 {
		return 1
	}
	switch c := v.(type) {
	case types.String:
		return byteScanCost(len(c))
	case types.Bytes:
		return byteScanCost(len(c))
	case traits.Lister:
		szVal, ok := c.Size().(types.Int)
		if !ok || szVal < 0 {
			return 1
		}
		sz := int64(szVal)
		*remElems -= sz
		total := cost.SafeAdd(1, uint64(sz))
		if *remElems < 0 {
			return total
		}
		for i := types.Int(0); i < szVal; i++ {
			elem := c.Get(i)
			switch s := elem.(type) {
			case types.String:
				total = cost.SafeAdd(total, byteScanCost(len(s)))
			case types.Bytes:
				total = cost.SafeAdd(total, byteScanCost(len(s)))
			case traits.Lister, traits.Mapper:
				total = cost.SafeAdd(total, measureContainerCost(elem, remElems, depth+1))
			}
			if *remElems < 0 {
				break
			}
		}
		return total
	case traits.Mapper:
		szVal, ok := c.Size().(types.Int)
		if !ok || szVal < 0 {
			return 1
		}
		sz := int64(szVal)
		*remElems -= sz
		total := cost.SafeAdd(1, uint64(sz))
		if *remElems < 0 {
			return total
		}
		it := c.Iterator()
		for it.HasNext() == types.True {
			k := it.Next()
			total = cost.SafeAdd(total, measureContainerCost(k, remElems, depth+1))
			if mv, found := c.Find(k); found {
				total = cost.SafeAdd(total, measureContainerCost(mv, remElems, depth+1))
			}
			if *remElems < 0 {
				break
			}
		}
		return total
	default:
		return 0
	}
}

func computeEqNeCost(maxElems int, args []ref.Val) *uint64 {
	if len(args) != 2 {
		return uint64Ptr(1)
	}
	lhs, rhs := args[0], args[1]
	if isContainerVal(lhs) || isContainerVal(rhs) {
		rem := int64(maxElems) * 2
		c := cost.SafeAdd(1, cost.SafeAdd(measureContainerCost(lhs, &rem, 0), measureContainerCost(rhs, &rem, 0)))
		return uint64Ptr(c)
	}
	if lStr, okL := lhs.(types.String); okL {
		if rStr, okR := rhs.(types.String); okR {
			return uint64Ptr(atLeastOneCost(byteScanCost(min(len(lStr), len(rStr)))))
		}
	}
	if lBytes, okL := lhs.(types.Bytes); okL {
		if rBytes, okR := rhs.(types.Bytes); okR {
			return uint64Ptr(atLeastOneCost(byteScanCost(min(len(lBytes), len(rBytes)))))
		}
	}
	return uint64Ptr(1)
}

func computeInCost(maxElems int, args []ref.Val) *uint64 {
	if len(args) != 2 {
		return uint64Ptr(1)
	}
	lhs, rhs := args[0], args[1]
	if _, isList := rhs.(traits.Lister); isList || isContainerVal(lhs) {
		rem := int64(maxElems) * 2
		c := cost.SafeAdd(1, cost.SafeAdd(measureContainerCost(lhs, &rem, 0), measureContainerCost(rhs, &rem, 0)))
		return uint64Ptr(c)
	}
	if sKey, ok := lhs.(types.String); ok {
		return uint64Ptr(atLeastOneCost(byteScanCost(len(sKey))))
	}
	return uint64Ptr(1)
}

func computeFormatCost(maxElems int, args []ref.Val, result ref.Val) *uint64 {
	if len(args) != 2 {
		return uint64Ptr(1)
	}
	total := uint64(1)
	if fmtStr, ok := args[0].(types.String); ok {
		total = cost.SafeAdd(total, byteScanCost(len(fmtStr)))
	}
	if outStr, ok := result.(types.String); ok {
		total = cost.SafeAdd(total, byteScanCost(len(outStr)))
	}
	if argList, ok := args[1].(traits.Lister); ok {
		rem := int64(maxElems)
		total = cost.SafeAdd(total, measureContainerCost(argList, &rem, 0))
	}
	return uint64Ptr(total)
}

func trackOrderCost(args []ref.Val, _ ref.Val) *uint64 {
	if len(args) != 2 {
		return uint64Ptr(1)
	}
	if lStr, okL := args[0].(types.String); okL {
		if rStr, okR := args[1].(types.String); okR {
			return uint64Ptr(atLeastOneCost(byteScanCost(min(len(lStr), len(rStr)))))
		}
	}
	if lBytes, okL := args[0].(types.Bytes); okL {
		if rBytes, okR := args[1].(types.Bytes); okR {
			return uint64Ptr(atLeastOneCost(byteScanCost(min(len(lBytes), len(rBytes)))))
		}
	}
	return uint64Ptr(1)
}

func trackMatchesCost(args []ref.Val, _ ref.Val) *uint64 {
	if len(args) != 2 {
		return uint64Ptr(1)
	}
	inVal, okL := args[0].(types.String)
	patVal, okR := args[1].(types.String)
	if !okL || !okR {
		return uint64Ptr(1)
	}
	pat := string(patVal)
	effPatLen := len(pat)
	if strings.IndexByte(pat, '{') >= 0 {
		if ps, err := types.RegexProgramSize(pat); err == nil && ps > effPatLen {
			effPatLen = ps
		}
	}
	inUnits := atLeastOneCost(byteScanCost(len(inVal) + 1))
	patUnits := atLeastOneCost((uint64(effPatLen) + 3) / 4)
	return uint64Ptr(safeMulUint64(inUnits, patUnits))
}

func trackStringSizeCost(args []ref.Val, _ ref.Val) *uint64 {
	if len(args) == 1 {
		if s, ok := args[0].(types.String); ok {
			return uint64Ptr(atLeastOneCost(byteScanCost(len(s))))
		}
	}
	return uint64Ptr(1)
}

func trackStringSearchCost(args []ref.Val, _ ref.Val) *uint64 {
	if len(args) >= 2 {
		s, ok0 := args[0].(types.String)
		sub, ok1 := args[1].(types.String)
		if ok0 && ok1 {
			sLen := max(1, len(s))
			subLen := max(1, len(sub))
			prod := safeMulUint64(uint64(sLen), uint64(subLen))
			scan := prod / 10
			if prod%10 != 0 {
				scan = cost.SafeAdd(scan, 1)
			}
			return uint64Ptr(cost.SafeAdd(1, scan))
		}
	}
	return uint64Ptr(1)
}

func trackStringInputConvCost(args []ref.Val, _ ref.Val) *uint64 {
	if len(args) == 1 {
		if s, ok := args[0].(types.String); ok {
			return uint64Ptr(atLeastOneCost(byteScanCost(len(s))))
		}
	}
	return uint64Ptr(1)
}

func trackAddListCost(args []ref.Val, _ ref.Val) *uint64 {
	if len(args) == 2 {
		l, ok1 := args[0].(traits.Lister)
		r, ok2 := args[1].(traits.Lister)
		if ok1 && ok2 {
			lSz, okL := l.Size().(types.Int)
			rSz, okR := r.Size().(types.Int)
			if okL && okR && lSz >= 0 && rSz >= 0 {
				return uint64Ptr(atLeastOneCost(cost.SafeAdd(uint64(lSz), uint64(rSz))))
			}
		}
	}
	return uint64Ptr(1)
}

func trackTimezoneCost(_ []ref.Val, result ref.Val) *uint64 {
	if types.IsError(result) {
		return uint64Ptr(tzLookupErrorCost)
	}
	return uint64Ptr(1)
}

type runtimeCostEstimator struct {
	maxElems int
}

func (e runtimeCostEstimator) CallCost(fn, overloadID string, args []ref.Val, result ref.Val) *uint64 {
	// Named overloads are handled by OverloadTracker / StandardOverloadTrackers.
	// Dynamic dispatch on dyn values (e.g. event.payload.*) leaves overloadID == "",
	// which cel-go's Tracker.CostCall would otherwise price at a flat 1.
	if overloadID != "" {
		return nil
	}
	switch fn {
	case operators.In, operators.OldIn, overloads.DeprecatedIn:
		return computeInCost(e.maxElems, args)
	case operators.Equals, operators.NotEquals:
		return computeEqNeCost(e.maxElems, args)
	case operators.Add:
		if len(args) == 2 {
			switch l := args[0].(type) {
			case types.String:
				if r, ok := args[1].(types.String); ok {
					return uint64Ptr(atLeastOneCost(byteScanCost(len(l) + len(r))))
				}
			case types.Bytes:
				if r, ok := args[1].(types.Bytes); ok {
					return uint64Ptr(atLeastOneCost(byteScanCost(len(l) + len(r))))
				}
			case traits.Lister:
				return trackAddListCost(args, result)
			}
		}
		return uint64Ptr(1)
	case operators.Less, operators.LessEquals, operators.Greater, operators.GreaterEquals:
		return trackOrderCost(args, result)
	case overloads.Size:
		return trackStringSizeCost(args, result)
	case overloads.TypeConvertString, overloads.TypeConvertBytes:
		if len(args) == 1 {
			switch v := args[0].(type) {
			case types.String:
				return uint64Ptr(atLeastOneCost(byteScanCost(len(v))))
			case types.Bytes:
				return uint64Ptr(atLeastOneCost(byteScanCost(len(v))))
			}
		}
		return uint64Ptr(1)
	case overloads.TypeConvertBool,
		overloads.TypeConvertInt,
		overloads.TypeConvertUint,
		overloads.TypeConvertDouble,
		overloads.TypeConvertTimestamp,
		overloads.TypeConvertDuration:
		return trackStringInputConvCost(args, result)
	case overloads.Matches:
		return trackMatchesCost(args, result)
	case overloads.Contains:
		if len(args) == 2 {
			if s, ok0 := args[0].(types.String); ok0 {
				if sub, ok1 := args[1].(types.String); ok1 {
					return uint64Ptr(atLeastOneCost(safeMulUint64(byteScanCost(len(s)), byteScanCost(len(sub)))))
				}
			}
		}
		return uint64Ptr(1)
	case overloads.StartsWith, overloads.EndsWith:
		if len(args) == 2 {
			if sub, ok := args[1].(types.String); ok {
				return uint64Ptr(atLeastOneCost(byteScanCost(len(sub))))
			}
		}
		return uint64Ptr(1)
	case overloads.TimeGetFullYear,
		overloads.TimeGetMonth,
		overloads.TimeGetDayOfYear,
		overloads.TimeGetDate,
		overloads.TimeGetDayOfMonth,
		overloads.TimeGetDayOfWeek,
		overloads.TimeGetHours,
		overloads.TimeGetMinutes,
		overloads.TimeGetSeconds,
		overloads.TimeGetMilliseconds:
		if len(args) == 2 {
			return trackTimezoneCost(args, result)
		}
		return uint64Ptr(1)
	default:
		return nil
	}
}

func buildCostTracking(limits Limits) (cost.ActualCostEstimator, []cost.TrackerOption) {
	maxElems := limits.MaxCollectionElements
	if maxElems <= 0 {
		maxElems = DefaultMaxCollectionElements
	}
	eqNeTracker := func(args []ref.Val, _ ref.Val) *uint64 {
		return computeEqNeCost(maxElems, args)
	}
	inTracker := func(args []ref.Val, _ ref.Val) *uint64 {
		return computeInCost(maxElems, args)
	}
	formatTracker := func(args []ref.Val, result ref.Val) *uint64 {
		return computeFormatCost(maxElems, args, result)
	}

	opts := []cost.TrackerOption{
		cost.OverloadTracker(prechargedWalkOverloadID, func(_ []ref.Val, result ref.Val) *uint64 {
			if u, ok := result.(types.Uint); ok {
				v := uint64(u)
				return &v
			}
			return uint64Ptr(1)
		}),
		cost.OverloadTracker(overloads.AddList, trackAddListCost),
		cost.OverloadTracker(overloads.Equals, eqNeTracker),
		cost.OverloadTracker(overloads.NotEquals, eqNeTracker),
		cost.OverloadTracker(overloads.InList, inTracker),
		cost.OverloadTracker(overloads.InMap, inTracker),
		cost.OverloadTracker(overloads.DeprecatedIn, inTracker),
		cost.OverloadTracker(overloads.ExtFormatString, formatTracker),
		cost.OverloadTracker(overloads.Matches, trackMatchesCost),
		cost.OverloadTracker(overloads.MatchesString, trackMatchesCost),

		cost.OverloadTracker(overloads.LessString, trackOrderCost),
		cost.OverloadTracker(overloads.LessEqualsString, trackOrderCost),
		cost.OverloadTracker(overloads.GreaterString, trackOrderCost),
		cost.OverloadTracker(overloads.GreaterEqualsString, trackOrderCost),
		cost.OverloadTracker(overloads.LessBytes, trackOrderCost),
		cost.OverloadTracker(overloads.LessEqualsBytes, trackOrderCost),
		cost.OverloadTracker(overloads.GreaterBytes, trackOrderCost),
		cost.OverloadTracker(overloads.GreaterEqualsBytes, trackOrderCost),

		cost.OverloadTracker(overloads.SizeString, trackStringSizeCost),
		cost.OverloadTracker(overloads.SizeStringInst, trackStringSizeCost),

		cost.OverloadTracker("string_index_of_string", trackStringSearchCost),
		cost.OverloadTracker("string_index_of_string_int", trackStringSearchCost),
		cost.OverloadTracker("string_last_index_of_string", trackStringSearchCost),
		cost.OverloadTracker("string_last_index_of_string_int", trackStringSearchCost),

		cost.OverloadTracker(overloads.StringToBool, trackStringInputConvCost),
		cost.OverloadTracker(overloads.StringToInt, trackStringInputConvCost),
		cost.OverloadTracker(overloads.StringToUint, trackStringInputConvCost),
		cost.OverloadTracker(overloads.StringToDouble, trackStringInputConvCost),
		cost.OverloadTracker(overloads.StringToTimestamp, trackStringInputConvCost),
		cost.OverloadTracker(overloads.StringToDuration, trackStringInputConvCost),

		cost.OverloadTracker(overloads.TimestampToYearWithTz, trackTimezoneCost),
		cost.OverloadTracker(overloads.TimestampToMonthWithTz, trackTimezoneCost),
		cost.OverloadTracker(overloads.TimestampToDayOfYearWithTz, trackTimezoneCost),
		cost.OverloadTracker(overloads.TimestampToDayOfMonthZeroBasedWithTz, trackTimezoneCost),
		cost.OverloadTracker(overloads.TimestampToDayOfMonthOneBasedWithTz, trackTimezoneCost),
		cost.OverloadTracker(overloads.TimestampToDayOfWeekWithTz, trackTimezoneCost),
		cost.OverloadTracker(overloads.TimestampToHoursWithTz, trackTimezoneCost),
		cost.OverloadTracker(overloads.TimestampToMinutesWithTz, trackTimezoneCost),
		cost.OverloadTracker(overloads.TimestampToSecondsWithTz, trackTimezoneCost),
		cost.OverloadTracker(overloads.TimestampToMillisecondsWithTz, trackTimezoneCost),
	}
	return runtimeCostEstimator{maxElems: maxElems}, opts
}
