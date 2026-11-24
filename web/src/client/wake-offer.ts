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

/** The agent a send or review finalize offered to wake. */
export interface WakeOffer {
  agentId: string;
  agentSlug: string;
}

/**
 * Reads a wake offer from a parsed error response body. Returns null unless
 * the hub explicitly said the caller may wake the agent: a permission gate
 * fails closed.
 */
export function wakeOfferFromErrorBody(data: unknown): WakeOffer | null {
  if (!data || typeof data !== 'object') return null;
  const err = (data as { error?: unknown }).error;
  if (!err || typeof err !== 'object') return null;
  const { code, details } = err as { code?: unknown; details?: unknown };
  if (code !== 'agent_not_running' || !details || typeof details !== 'object') return null;
  const d = details as Record<string, unknown>;
  if (d.canWake !== true) return null;
  return {
    agentId: typeof d.agentId === 'string' ? d.agentId : '',
    agentSlug: typeof d.agentSlug === 'string' ? d.agentSlug : '',
  };
}
