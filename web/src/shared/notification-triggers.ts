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
export const DEFAULT_TRIGGERS: readonly string[] = [
  'COMPLETED',
  'WAITING_FOR_INPUT',
  'LIMITS_EXCEEDED',
  'HALTED',
];

/** Every trigger a subscription can select. */
export const ALL_TRIGGERS: readonly string[] = [
  'COMPLETED',
  'WAITING_FOR_INPUT',
  'LIMITS_EXCEEDED',
  'STALLED',
  'ERROR',
  'HALTED',
  'DELETED',
];

const TRIGGER_LABELS: Record<string, string> = {
  COMPLETED: 'Completed',
  // Display only: pairs with 'blocked' shown as 'waiting on others'
  // (ptone/scion#3301, ptone/scion#1571).
  WAITING_FOR_INPUT: 'Waiting on Parent',
  LIMITS_EXCEEDED: 'Limits Exceeded',
  STALLED: 'Stalled',
  ERROR: 'Error',
  // The platform ended a run that should still be running (ptone/scion#4853).
  HALTED: 'Halted',
  DELETED: 'Deleted',
};

/** Title-case label for a trigger; unknown triggers show the raw value. */
export function triggerLabel(trigger: string): string {
  return Object.hasOwn(TRIGGER_LABELS, trigger) ? TRIGGER_LABELS[trigger] : trigger;
}

/**
 * Triggers the Hub subscribes when an agent is created with notify on
 * (newNotifySubscription in pkg/hub/handlers_agent_create_helpers.go).
 */
export const AGENT_CREATE_TRIGGERS: readonly string[] = [
  'COMPLETED',
  'WAITING_FOR_INPUT',
  'LIMITS_EXCEEDED',
  'STALLED',
  'ERROR',
  'HALTED',
];

/**
 * Hint listing the default triggers.
 * Returns an empty string when there are no triggers.
 */
export function defaultTriggersHint(triggers: readonly string[] = DEFAULT_TRIGGERS): string {
  const labels = triggers.map(triggerLabel);
  if (labels.length === 0) return '';
  const last = labels.pop();
  const list =
    labels.length === 0
      ? last
      : labels.length === 1
        ? `${labels[0]} or ${last}`
        : `${labels.join(', ')}, or ${last}`;
  return `You will be notified when this agent reaches: ${list}.`;
}
