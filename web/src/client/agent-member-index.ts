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
 * `agents-changed` under the page's add rule. The paged window and home
 * share this shape.
 *
 * Count-only mode: the global endpoint omits `stats.agents` when its total
 * is above 2,000 and sends the counts alone. {@link AgentMemberIndex.seedCounts}
 * then holds those counts as a snapshot: with no IDs, the client cannot
 * tell whether a live change falls inside the counted population, so the
 * snapshot is never adjusted live (`set` and `delete` are ignored) until
 * the next seed. The project endpoint is bounded by its candidate ceiling,
 * so it never omits `stats.agents`.
 */
export class AgentMemberIndex {
  private phases = new Map<string, string>();
  private snapshot: { total: number; running: number } | null = null;

  /** Replace the whole index, e.g. from a `stats.agents` response. Leaves count-only mode. */
  seed(entries: ReadonlyArray<readonly [string, string]>): void {
    this.phases = new Map(entries);
    this.snapshot = null;
  }

  /**
   * Enter count-only mode with the counts of a response that omitted
   * `stats.agents`. The index holds no IDs until the next {@link seed}.
   */
  seedCounts(total: number, running: number): void {
    this.phases = new Map();
    this.snapshot = { total, running };
  }

  /** Whether the index holds a counts-only snapshot (see {@link seedCounts}). */
  get countOnly(): boolean {
    return this.snapshot !== null;
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
    if (this.snapshot) return; // count-only: not adjusted live.
    this.phases.set(id, phase);
  }

  /** Idempotent: deleting an ID that is not present is a no-op (`deleted` is a safe superset). */
  delete(id: string): void {
    if (this.snapshot) return; // count-only: not adjusted live.
    this.phases.delete(id);
  }

  ids(): IterableIterator<string> {
    return this.phases.keys();
  }

  /** "Agents" and "Running" counts: `index.size` and the running count, or the count-only snapshot. */
  get stats(): { total: number; running: number } {
    if (this.snapshot) return { ...this.snapshot };
    let running = 0;
    for (const phase of this.phases.values()) {
      if (phase === 'running') running++;
    }
    return { total: this.phases.size, running };
  }
}
