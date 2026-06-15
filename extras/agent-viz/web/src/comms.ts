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

import { marked } from 'marked';

import type { MessageEvent } from './types';
import type { AgentRing } from './agents';

// Render message bodies as inline-friendly HTML: no leading <h1> ids, GitHub-style
// line breaks so single newlines in agent output become visible breaks.
marked.use({ breaks: true, gfm: true });

/**
 * Window (in event-time ms) for collapsing duplicate broadcast deliveries into a
 * single transcript line. A broadcast is logged once per recipient, so the same
 * sender+content arrives several times within a short span.
 */
const BROADCAST_DEDUP_WINDOW_MS = 2000;

interface AddOptions {
  /**
   * When false the card is inserted without the fade-in animation. Used while
   * replaying a snapshot on seek, where every prior message arrives at once.
   */
  animate?: boolean;
}

/**
 * CommsPanel renders a scrolling, human-readable transcript of inter-agent
 * messages next to the force-graph. It consumes the same `message` playback
 * events that drive the on-graph pulse lines, so it stays in sync with playback
 * and is rebuilt from the snapshot on seek. It needs no external data source —
 * the messages already flow through the playback stream.
 */
export class CommsPanel {
  private readonly bodyEl: HTMLElement;
  private readonly countEl: HTMLElement;
  private readonly toggleEl: HTMLButtonElement;
  private readonly panelEl: HTMLElement;
  private agentRing: AgentRing | null = null;
  private startMs: number | null = null;
  private count = 0;
  private collapsed = false;
  /** Pending requestAnimationFrame handle for the deferred scroll-to-bottom. */
  private scrollRafId: number | null = null;
  /** Dedup state for broadcasts: `sender::content` -> last event time (ms). */
  private readonly recentBroadcasts = new Map<string, number>();

  constructor(parent: HTMLElement = document.body) {
    const panel = document.createElement('div');
    panel.className = 'comms-panel';
    panel.innerHTML = `
      <div class="comms-header">
        <div class="comms-titles">
          <div class="comms-title">Agent Communications</div>
          <div class="comms-subtitle"><span class="comms-count">0</span> messages</div>
        </div>
        <button class="comms-toggle" title="Collapse / expand">&minus;</button>
      </div>
      <div class="comms-body"></div>
    `;
    parent.appendChild(panel);

    this.panelEl = panel;
    this.bodyEl = panel.querySelector('.comms-body') as HTMLElement;
    this.countEl = panel.querySelector('.comms-count') as HTMLElement;
    this.toggleEl = panel.querySelector('.comms-toggle') as HTMLButtonElement;

    this.toggleEl.addEventListener('click', () => this.setCollapsed(!this.collapsed));
  }

  setAgentRing(ring: AgentRing): void {
    this.agentRing = ring;
  }

  /** Anchor for relative `T+m:ss` timestamps (the playback start time). */
  setStartTime(iso: string): void {
    const ms = Date.parse(iso);
    this.startMs = Number.isNaN(ms) ? null : ms;
  }

  /** Clear the transcript (called on a new manifest and before snapshot replay). */
  reset(): void {
    if (this.scrollRafId !== null) {
      cancelAnimationFrame(this.scrollRafId);
      this.scrollRafId = null;
    }
    this.bodyEl.innerHTML = '';
    this.count = 0;
    this.recentBroadcasts.clear();
    this.updateCount();
  }

  addMessage(event: MessageEvent, timestamp: string, opts: AddOptions = {}): void {
    // Collapse duplicate broadcast deliveries (same sender+content) into one line.
    if (event.broadcasted) {
      const key = `${event.sender}::${event.content ?? ''}`;
      const t = Date.parse(timestamp);
      const prev = this.recentBroadcasts.get(key);
      if (prev !== undefined && Math.abs(t - prev) < BROADCAST_DEDUP_WINDOW_MS) return;
      this.recentBroadcasts.set(key, t);
    }

    const animate = opts.animate ?? true;

    // Only auto-scroll when the user is already near the bottom, so manual
    // scroll-back to read history isn't yanked away by new arrivals. Skip the
    // layout-reading measurement during non-animated batch loads (snapshot
    // replay on seek): interleaving these reads with appendChild in that loop
    // would cause layout thrashing.
    const nearBottom =
      animate &&
      this.bodyEl.scrollTop + this.bodyEl.clientHeight >= this.bodyEl.scrollHeight - 60;

    this.bodyEl.appendChild(this.makeCard(event, timestamp, animate));
    this.count++;
    this.updateCount();

    if (animate) {
      if (nearBottom) this.bodyEl.scrollTop = this.bodyEl.scrollHeight;
    } else {
      // Defer a single scroll-to-bottom until after the synchronous replay loop.
      if (this.scrollRafId !== null) cancelAnimationFrame(this.scrollRafId);
      this.scrollRafId = requestAnimationFrame(() => {
        this.bodyEl.scrollTop = this.bodyEl.scrollHeight;
        this.scrollRafId = null;
      });
    }
  }

  private setCollapsed(collapsed: boolean): void {
    this.collapsed = collapsed;
    this.panelEl.classList.toggle('collapsed', collapsed);
    this.toggleEl.innerHTML = collapsed ? '&plus;' : '&minus;';
  }

  private updateCount(): void {
    this.countEl.textContent = String(this.count);
  }

  private makeCard(event: MessageEvent, timestamp: string, animate: boolean): HTMLElement {
    const senderColor = this.agentRing?.getAgentColor(event.sender) ?? '#888';
    const broadcast = event.broadcasted;
    const recipientColor = broadcast
      ? '#22c55e'
      : (this.agentRing?.getAgentColor(event.recipient) ?? '#888');
    const accent = broadcast ? '#22c55e' : senderColor;

    const card = document.createElement('div');
    card.className = broadcast ? 'comms-msg comms-msg-bcast' : 'comms-msg';
    card.style.borderLeftColor = accent;
    if (!animate) card.style.animation = 'none';

    const recipientLabel = broadcast ? 'ALL' : event.recipient || '?';
    const arrow = broadcast ? '↯' : '→';
    const typeTag = event.msgType
      ? `<span class="comms-type">${escapeHtml(event.msgType)}</span>`
      : '';
    const bcastBadge = broadcast ? '<span class="comms-badge">BROADCAST</span>' : '';

    // The raw message body. Rendered as Markdown lazily on first expand (below), so
    // collapsed cards — the default, and every card during snapshot replay — pay no
    // parse cost.
    const rawContent = event.content ?? '';

    // A one-line plain-text summary shown while the message is collapsed (the default).
    // Slice first so a huge payload (e.g. a large JSON blob) can't block the main thread
    // in the regex; strip markdown punctuation + collapse whitespace to keep it one line.
    const summary = rawContent
      .slice(0, 1000)
      .replace(/[#*`>_~\[\]]/g, '')
      .replace(/\s+/g, ' ')
      .trim()
      .slice(0, 120);

    card.innerHTML = `
      <div class="comms-msg-header">
        <div class="comms-msg-meta">
          <span class="comms-time">${this.formatTime(timestamp)}</span>
          ${bcastBadge}
          <span class="comms-index">#${this.count + 1}</span>
        </div>
        <div class="comms-route">
          <span class="comms-msg-toggle">▸</span>
          <span style="color:${senderColor}">${escapeHtml(event.sender || '?')}</span>
          <span class="comms-arrow">${arrow}</span>
          <span style="color:${recipientColor}">${escapeHtml(recipientLabel)}</span>
          ${typeTag}
        </div>
        <div class="comms-summary">${escapeHtml(summary)}</div>
      </div>
      <div class="comms-content markdown"></div>
    `;
    // Collapsed by default. Parse + render the Markdown only on first expand, escaping the
    // raw body first: marked does NOT strip raw HTML, so escaping neutralizes any
    // <script>/<img onerror> in agent output before it reaches innerHTML. (Trade-off: `>`
    // is escaped too, so Markdown blockquotes render as literal text — acceptable here.)
    let rendered = false;
    card.querySelector('.comms-msg-header')?.addEventListener('click', () => {
      if (!rendered) {
        const contentEl = card.querySelector('.comms-content');
        if (contentEl) contentEl.innerHTML = marked.parse(escapeHtml(rawContent)) as string;
        rendered = true;
      }
      card.classList.toggle('expanded');
    });
    return card;
  }

  private formatTime(timestamp: string): string {
    const t = Date.parse(timestamp);
    if (Number.isNaN(t)) return '';
    if (this.startMs !== null) {
      const sec = Math.max(0, Math.floor((t - this.startMs) / 1000));
      const m = Math.floor(sec / 60);
      const s = sec % 60;
      return `T+${m}:${String(s).padStart(2, '0')}`;
    }
    return new Date(t).toLocaleTimeString();
  }
}

function escapeHtml(s: string): string {
  return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
}
