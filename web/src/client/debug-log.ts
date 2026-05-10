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
 * Debug Event Logger
 *
 * Instruments SSEClient and StateManager to capture real-time event data
 * for the debug panel. Uses a circular buffer to keep memory bounded.
 */

import type { StateManager } from './state.js';
import type { SSEUpdateEvent } from './sse-client.js';

export type DebugCategory = 'sse' | 'state' | 'connection';
export type DebugDirection = 'in' | 'out';

export interface DebugEntry {
  id: number;
  timestamp: number;
  category: DebugCategory;
  label: string;
  subject?: string;
  data?: unknown;
  direction?: DebugDirection;
}

const MAX_ENTRIES = 500;

export class DebugEventLog extends EventTarget {
  private entries: DebugEntry[] = [];
  private nextId = 1;
  private _connectionId: string | null = null;
  private _attached = false;

  get log(): readonly DebugEntry[] {
    return this.entries;
  }

  get connectionId(): string | null {
    return this._connectionId;
  }

  get entryCount(): number {
    return this.entries.length;
  }

  add(category: DebugCategory, label: string, subject?: string, data?: unknown, direction?: DebugDirection): void {
    const entry: DebugEntry = {
      id: this.nextId++,
      timestamp: Date.now(),
      category,
      label,
    };
    if (subject !== undefined) entry.subject = subject;
    if (data !== undefined) entry.data = data;
    if (direction !== undefined) entry.direction = direction;

    this.entries.push(entry);
    if (this.entries.length > MAX_ENTRIES) {
      this.entries.shift();
    }

    this.dispatchEvent(new CustomEvent('log-updated'));
  }

  clear(): void {
    this.entries = [];
    this.dispatchEvent(new CustomEvent('log-updated'));
  }

  /**
   * Wire up listeners on the StateManager and its SSE client.
   * Safe to call multiple times — only attaches once.
   */
  attach(stateManager: StateManager): void {
    if (this._attached) return;
    this._attached = true;

    const sse = stateManager.sseClientInstance;

    // SSE update events
    sse.addEventListener('update', ((event: CustomEvent<SSEUpdateEvent>) => {
      const { subject, data } = event.detail;
      this.add('sse', 'update', subject, data, 'in');
    }) as EventListener);

    // SSE connection events
    sse.addEventListener('connected', ((event: CustomEvent<{ connectionId: string; subjects: string[] }>) => {
      this._connectionId = event.detail.connectionId;
      this.add('connection', 'connected', undefined, event.detail);
    }) as EventListener);

    sse.addEventListener('disconnected', () => {
      this._connectionId = null;
      this.add('connection', 'disconnected');
    });

    sse.addEventListener('reconnecting', ((event: CustomEvent<{ attempt: number }>) => {
      this.add('connection', 'reconnecting', undefined, event.detail);
    }) as EventListener);

    // State change events
    for (const eventType of ['agents-updated', 'projects-updated', 'brokers-updated'] as const) {
      stateManager.addEventListener(eventType, () => {
        const snapshot = stateManager.getStateSnapshot();
        this.add('state', eventType, undefined, snapshot);
      });
    }

    stateManager.addEventListener('scope-changed', () => {
      const scope = stateManager.currentScope;
      const subjects = stateManager.currentSubjects;
      this.add('connection', 'scope-changed', undefined, { scope, subjects });
    });

    stateManager.addEventListener('connected', () => {
      this.add('connection', 'state-connected');
    });

    stateManager.addEventListener('disconnected', () => {
      this.add('connection', 'state-disconnected');
    });
  }
}

/** Singleton instance */
export const debugLog = new DebugEventLog();
