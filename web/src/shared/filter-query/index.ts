/**
 * Copyright 2026 Google LLC
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

import rawSchemas from './schemas.generated.json' with { type: 'json' };
import type { ViewSchema } from './types.js';

export * from './types.js';
export { parse } from './parse.js';
export { validate } from './validate.js';
export {
  canonical,
  hasClientOnlyKeys,
  renderQueryPreservingMeaning,
  serverSubset,
} from './canonical.js';
export { match } from './match.js';
export {
  getSuggestions,
  type Suggestion,
  type ValueSuggestion,
  type ValueSuggestionProvider,
} from './suggest.js';
export {
  getActiveSingleKeyValue,
  hasIsFlagInQuery,
  setSingleKeyInQuery,
  setSingleKeyInQueryWithNotice,
  toggleIsFlagInQuery,
  toggleIsFlagInQueryWithNotice,
  UNREPRESENTABLE_CLEAR_NOTICE,
  type QueryEditResult,
} from './query-edit.js';
export {
  agentToFilterRow,
  buildAgentValueSuggestions,
  canLiveAddAgent,
  clientEvaluableAgentQuery,
  drainServerQuery,
  hasNonPhaseServerPredicate,
  hasScopeTermInQuery,
  hasServerOnlyAgentTerms,
  matchAgentQuery,
  nonPhasePredicateForQuery,
  nonPhaseServerPredicateForQuery,
  phasePredicateForQuery,
  refusalKeyForQuery,
  type AgentFilterContext,
} from './agent-accessors.js';

export const VIEW_SCHEMAS: readonly ViewSchema[] = rawSchemas as ViewSchema[];

const schemaByView = new Map<string, ViewSchema>(VIEW_SCHEMAS.map((s) => [s.view, s]));

export function getViewSchema(view: string): ViewSchema | undefined {
  return schemaByView.get(view);
}

export function knownKeysFor(schema: ViewSchema): (key: string) => boolean {
  const set = new Set(schema.keys.map((k) => k.name.toLowerCase()));
  return (k: string) => set.has(k.toLowerCase());
}
