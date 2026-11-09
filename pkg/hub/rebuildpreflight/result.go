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

package rebuildpreflight

// ErrorKey is the run Result key that holds the run's error text. Only the
// hub sets it, from the error the executor returned (or its panic); an
// executor's own result never does.
const ErrorKey = "error"

// MergeFinal returns a finished run's Result: a copy of the executor's own
// result (nil if the executor reports none), with ErrorKey set to errText
// when the run failed. errText is applied last, so it always wins over an
// "error" key in the executor's result, and on success such a key is
// dropped. It returns nil when there is nothing to record, so a run of an
// executor that reports no result keeps an empty Result.
func MergeFinal(executorResult map[string]any, errText string) map[string]any {
	out := make(map[string]any, len(executorResult)+1)
	for k, v := range executorResult {
		out[k] = v
	}
	delete(out, ErrorKey)
	if errText != "" {
		out[ErrorKey] = errText
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
