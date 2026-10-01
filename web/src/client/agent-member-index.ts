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
 * `Map<id, phase>` member index (design §6.2).
 *
 * Seeded from a sorted response's `stats.agents`, and kept live from
 * `agents-changed` under the page's add rule. The paged window and (in a
 * later phase) home share this shape. The project endpoint is bounded by
 * the 2,000-candidate ceiling (design §5.3 step 0), so `stats.agents` is
 * never omitted there — the global endpoint's count-only mode above 2,000
 * (design §4.6) does not apply to project pages, and is out of P1c's scope.
 */
export class AgentMemberIndex {
  private phases = new Map<string, string>();

  /** Replace the whole index, e.g. from a `stats.agents` response. */
  seed(entries: ReadonlyArray<readonly [string, string]>): void {
    this.phases = new Map(entries);
  }

  get size(): number {
    return this.phases.size;
  }

  has(id: string): boolean {
    return this.phases.has(id);
  }

  getPhase(id: string): string | undefined {
    return this.phases.get(id);
  }

  /** Add or update one member's phase (the SSE `created`/upsert add rule). */
  set(id: string, phase: string): void {
    this.phases.set(id, phase);
  }

  /** Idempotent: deleting an ID that is not present is a no-op (P1a FYI — `deleted` is a safe superset). */
  delete(id: string): void {
    this.phases.delete(id);
  }

  ids(): IterableIterator<string> {
    return this.phases.keys();
  }

  /** "Agents" and "Running" counts: `index.size` and the running count (design §6.2). */
  get stats(): { total: number; running: number } {
    let running = 0;
    for (const phase of this.phases.values()) {
      if (phase === 'running') running++;
    }
    return { total: this.phases.size, running };
  }
}
