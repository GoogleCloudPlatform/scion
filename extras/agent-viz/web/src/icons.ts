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

// Bootstrap Icon SVG paths for agent state rendering on canvas.
// These are simplified paths from Bootstrap Icons, designed for 16x16 viewBox.

const ICON_PATHS: Record<string, { path: string; color: string; pulse?: boolean }> = {
  // Activity icons
  working: {
    path: 'M8 16A8 8 0 1 0 8 0a8 8 0 0 0 0 16z',
    color: '#198754',
  },
  thinking: {
    path: 'M11.251.068a.5.5 0 0 1 .227.58L9.677 6.5H13a.5.5 0 0 1 .364.843l-8 8.5a.5.5 0 0 1-.842-.49L6.323 9.5H3a.5.5 0 0 1-.364-.843l8-8.5a.5.5 0 0 1 .615-.09z',
    color: '#0d6efd',
    pulse: true,
  },
  executing: {
    path: 'M8 4.754a3.246 3.246 0 1 0 0 6.492 3.246 3.246 0 0 0 0-6.492zM5.754 8a2.246 2.246 0 1 1 4.492 0 2.246 2.246 0 0 1-4.492 0zM8 0a.5.5 0 0 1 .5.5v2a.5.5 0 0 1-1 0v-2A.5.5 0 0 1 8 0zM3.464 3.464a.5.5 0 0 1 .707 0l1.414 1.414a.5.5 0 1 1-.707.707L3.464 4.17a.5.5 0 0 1 0-.707zM0 8a.5.5 0 0 1 .5-.5h2a.5.5 0 0 1 0 1h-2A.5.5 0 0 1 0 8zm3.464 4.536a.5.5 0 0 1 0-.707l1.414-1.414a.5.5 0 0 1 .707.707L4.17 12.536a.5.5 0 0 1-.707 0zM8 13a.5.5 0 0 1 .5.5v2a.5.5 0 0 1-1 0v-2A.5.5 0 0 1 8 13zm4.536-1.464a.5.5 0 0 1 0 .707l-1.414 1.414a.5.5 0 0 1-.707-.707l1.414-1.414a.5.5 0 0 1 .707 0zM13 8a.5.5 0 0 1 .5-.5h2a.5.5 0 0 1 0 1h-2A.5.5 0 0 1 13 8zm-1.464-4.536a.5.5 0 0 1 .707 0l1.414 1.414a.5.5 0 0 1-.707.707L11.536 4.17a.5.5 0 0 1 0-.707z',
    color: '#0d6efd',
    pulse: true,
  },
  waiting_for_input: {
    path: 'M5 8a1 1 0 1 1-2 0 1 1 0 0 1 2 0zm4 0a1 1 0 1 1-2 0 1 1 0 0 1 2 0zm3 1a1 1 0 1 0 0-2 1 1 0 0 0 0 2z M2.678 11.894a1 1 0 0 1 .287.801 10.97 10.97 0 0 1-.398 2c1.395-.323 2.247-.697 2.634-.893a1 1 0 0 1 .71-.074A8.06 8.06 0 0 0 8 14c3.996 0 7-2.807 7-6 0-3.192-3.004-6-7-6S1 4.808 1 8c0 1.468.617 2.83 1.678 3.894z',
    color: '#ffc107',
  },
  completed: {
    path: 'M16 8A8 8 0 1 1 0 8a8 8 0 0 1 16 0zm-3.97-3.03a.75.75 0 0 0-1.08.022L7.477 9.417 5.384 7.323a.75.75 0 0 0-1.06 1.06L6.97 11.03a.75.75 0 0 0 1.079-.02l3.992-4.99a.75.75 0 0 0-.01-1.05z',
    color: '#198754',
  },
  limits_exceeded: {
    path: 'M11.46.146A.5.5 0 0 0 11.107 0H4.893a.5.5 0 0 0-.353.146L.146 4.54A.5.5 0 0 0 0 4.893v6.214a.5.5 0 0 0 .146.353l4.394 4.394a.5.5 0 0 0 .353.146h6.214a.5.5 0 0 0 .353-.146l4.394-4.394a.5.5 0 0 0 .146-.353V4.893a.5.5 0 0 0-.146-.353L11.46.146zM8 4c.535 0 .954.462.9.995l-.35 3.507a.552.552 0 0 1-1.1 0L7.1 4.995A.905.905 0 0 1 8 4zm.002 6a1 1 0 1 1 0 2 1 1 0 0 1 0-2z',
    color: '#dc3545',
  },
  stalled: {
    path: 'M16 8A8 8 0 1 1 0 8a8 8 0 0 1 16 0zM8 3.5a.5.5 0 0 0-1 0V8a.5.5 0 0 0 .252.434l3.5 2a.5.5 0 0 0 .496-.868L8 7.71V3.5z',
    color: '#ffc107',
  },
  blocked: {
    path: 'M16 8A8 8 0 1 1 0 8a8 8 0 0 1 16 0zM8 3.5a.5.5 0 0 0-1 0V8a.5.5 0 0 0 .252.434l3.5 2a.5.5 0 0 0 .496-.868L8 7.71V3.5z',
    color: '#6c757d',
  },
  offline: {
    path: 'M10.706 3.294A12.545 12.545 0 0 0 8 3C5.259 3 2.723 3.882.663 5.379a.485.485 0 0 0-.048.736.518.518 0 0 0 .668.05A11.448 11.448 0 0 1 8 4c.63 0 1.249.05 1.852.148l.854-.854zM8 6c-1.905 0-3.68.56-5.166 1.526a.48.48 0 0 0-.063.745.525.525 0 0 0 .652.065 8.448 8.448 0 0 1 3.51-1.27L8 6zm2.596 1.404.785-.785c.63.24 1.227.545 1.785.907a.482.482 0 0 1 .063.745.525.525 0 0 1-.652.065 8.462 8.462 0 0 0-1.98-.932zM8 10l.933-.933a6.455 6.455 0 0 1 2.013.637c.285.145.326.524.1.75l-.015.015a.532.532 0 0 1-.611.09A5.478 5.478 0 0 0 8 10zm4.905-4.905.747-.747c.59.3 1.153.645 1.685 1.03a.485.485 0 0 1 .047.737.518.518 0 0 1-.668.05 11.493 11.493 0 0 0-1.811-1.07zM9.02 11.78c.238.14.236.464.04.66l-.707.706a.5.5 0 0 1-.707 0l-.707-.707c-.195-.195-.197-.518.04-.66A1.99 1.99 0 0 1 8 11.5c.374 0 .723.102 1.021.28zm4.355-9.905a.53.53 0 0 1 .75.75l-10.75 10.75a.53.53 0 0 1-.75-.75l10.75-10.75z',
    color: '#6c757d',
  },

  // Phase icons
  created: {
    path: 'M8 15A7 7 0 1 1 8 1a7 7 0 0 1 0 14zm0 1A8 8 0 1 0 8 0a8 8 0 0 0 0 16z',
    color: '#6c757d',
  },
  starting: {
    path: 'M5.615 0a.5.5 0 0 1 .447.276l1.492 2.986a.5.5 0 0 1-.894.448L5.185 1h-.37C4.18 3.07 3.2 5.47 3.2 8c0 2.53.98 4.93 2.615 6.999h.37l1.475-2.71a.5.5 0 1 1 .894.448l-1.492 2.986A.5.5 0 0 1 6.615 16h-1.23a.5.5 0 0 1-.386-.184C3.21 13.764 2.2 10.972 2.2 8c0-2.972 1.01-5.764 2.8-7.816A.5.5 0 0 1 5.385 0h.23zm4.77 0a.5.5 0 0 1 .386.184C12.79 2.236 13.8 5.028 13.8 8c0 2.972-1.01 5.764-2.8 7.816a.5.5 0 0 1-.386.184h-.23a.5.5 0 0 1-.447-.276l-1.492-2.986a.5.5 0 0 1 .894-.448l1.475 2.71h.37C12.82 12.93 13.8 10.53 13.8 8c0-2.53-.98-4.93-2.615-7H10.815l-1.475 2.71a.5.5 0 1 1-.894-.448L9.938.276A.5.5 0 0 1 10.385 0h.23z',
    color: '#ffc107',
    pulse: true,
  },
  running: {
    path: 'M16 8A8 8 0 1 1 0 8a8 8 0 0 1 16 0zM6.79 5.093A.5.5 0 0 0 6 5.5v5a.5.5 0 0 0 .79.407l3.5-2.5a.5.5 0 0 0 0-.814l-3.5-2.5z',
    color: '#198754',
  },
  stopping: {
    path: 'M11.534 7h3.932a.25.25 0 0 1 .192.41l-1.966 2.36a.25.25 0 0 1-.384 0l-1.966-2.36a.25.25 0 0 1 .192-.41zm-11 2h3.932a.25.25 0 0 0 .192-.41L2.692 6.23a.25.25 0 0 0-.384 0L.342 8.59A.25.25 0 0 0 .534 9z M8 3c-1.552 0-2.94.707-3.857 1.818a.5.5 0 1 1-.771-.636A6.002 6.002 0 0 1 13.917 7H12.9A5.002 5.002 0 0 0 8 3zM3.1 9a5.002 5.002 0 0 0 8.757 2.182.5.5 0 1 1 .771.636A6.002 6.002 0 0 1 2.083 9H3.1z',
    color: '#ffc107',
    pulse: true,
  },
  stopped: {
    path: 'M16 8A8 8 0 1 1 0 8a8 8 0 0 1 16 0zM6.5 5A1.5 1.5 0 0 0 5 6.5v3A1.5 1.5 0 0 0 6.5 11h3A1.5 1.5 0 0 0 11 9.5v-3A1.5 1.5 0 0 0 9.5 5h-3z',
    color: '#6c757d',
  },
  error: {
    path: 'M8.982 1.566a1.13 1.13 0 0 0-1.96 0L.165 13.233c-.457.778.091 1.767.98 1.767h13.713c.889 0 1.438-.99.98-1.767L8.982 1.566zM8 5c.535 0 .954.462.9.995l-.35 3.507a.552.552 0 0 1-1.1 0L7.1 5.995A.905.905 0 0 1 8 5zm.002 6a1 1 0 1 1 0 2 1 1 0 0 1 0-2z',
    color: '#dc3545',
  },
};

export function getIconForState(
  phase: string,
  activity: string
): { path: string; color: string; pulse: boolean } {
  // Activity takes precedence if present
  if (activity && ICON_PATHS[activity]) {
    const icon = ICON_PATHS[activity];
    return { path: icon.path, color: icon.color, pulse: icon.pulse ?? false };
  }
  // Fall back to phase
  if (phase && ICON_PATHS[phase]) {
    const icon = ICON_PATHS[phase];
    return { path: icon.path, color: icon.color, pulse: icon.pulse ?? false };
  }
  // Default
  return { path: ICON_PATHS.working.path, color: ICON_PATHS.working.color, pulse: false };
}
