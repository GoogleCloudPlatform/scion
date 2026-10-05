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

/**
 * Display labels for notification trigger activities. The trigger values
 * (e.g. WAITING_FOR_INPUT) are the API and CLI values and stay unchanged;
 * only the text shown in the web UI comes from here.
 */

/** Triggers a new subscription starts with. */
export const DEFAULT_TRIGGERS = ['COMPLETED', 'WAITING_FOR_INPUT', 'LIMITS_EXCEEDED'];

/** Every trigger a subscription can select. */
export const ALL_TRIGGERS = [
  'COMPLETED',
  'WAITING_FOR_INPUT',
  'LIMITS_EXCEEDED',
  'STALLED',
  'ERROR',
  'DELETED',
];

const TRIGGER_LABELS: Record<string, string> = {
  COMPLETED: 'Completed',
  // Display only: pairs with 'blocked' shown as 'waiting on others'
  // (ptone/scion#3301, ptone/scion#1571).
  WAITING_FOR_INPUT: 'Waiting for User',
  LIMITS_EXCEEDED: 'Limits Exceeded',
  STALLED: 'Stalled',
  ERROR: 'Error',
  DELETED: 'Deleted',
};

/** Title-case label for a trigger; unknown triggers show the raw value. */
export function triggerLabel(trigger: string): string {
  return TRIGGER_LABELS[trigger] ?? trigger;
}

/** Hint listing the default triggers, e.g. on the agent create form. */
export function defaultTriggersHint(): string {
  const labels = DEFAULT_TRIGGERS.map(triggerLabel);
  const last = labels.pop();
  return `You will be notified when this agent reaches: ${labels.join(', ')}, or ${last}.`;
}
