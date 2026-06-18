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

import type { StatusUpdate } from './types';
import type { WSClient } from './ws';

export class PlaybackControls {
  private container: HTMLElement;
  private ws: WSClient;
  private playing = false;
  private speed = 1;
  private timeStart = '';
  private timeEnd = '';
  private currentTime = '';
  private position = 0;
  private total = 0;

  // DOM elements
  private playBtn!: HTMLButtonElement;
  private speedSelect!: HTMLSelectElement;
  private scrubber!: HTMLInputElement;
  private timeDisplay!: HTMLSpanElement;
  private filterPanel!: HTMLElement;
  private filterToggle!: HTMLButtonElement;

  // Callbacks
  private onFilterChange?: (agents: string[], eventTypes: string[]) => void;
  private onShowFileLabelsChange?: (show: boolean) => void;
  private onAgentColor?: (id: string, name: string, color: string) => void;

  constructor(container: HTMLElement, ws: WSClient) {
    this.container = container;
    this.ws = ws;
    this.buildUI();
  }

  setTimeRange(start: string, end: string): void {
    this.timeStart = start;
    this.timeEnd = end;
    this.updateTimeDisplay();
  }

  setAgents(agents: { id: string; name: string; color: string }[]): void {
    const list = this.filterPanel.querySelector('.agent-filters')!;
    list.innerHTML = '';
    for (const agent of agents) {
      const label = document.createElement('label');
      label.className = 'filter-item';
      // <input type="color"> requires #rrggbb; expand 3-digit hex (e.g. the default #888) so the
      // picker shows the real colour instead of silently falling back to black.
      const swatch = /^#[0-9a-f]{3}$/i.test(agent.color)
        ? '#' + agent.color.slice(1).replace(/./g, (c) => c + c)
        : agent.color;
      label.innerHTML = `
        <input type="checkbox" checked data-agent-id="${agent.id}">
        <input type="color" class="agent-color" value="${swatch}" title="Change ${agent.name} colour">
        ${agent.name}
      `;
      label.querySelector('input[type="checkbox"]')!.addEventListener('change', () => this.emitFilter());
      const colorInput = label.querySelector('input[type="color"]') as HTMLInputElement;
      colorInput.addEventListener('click', (e) => e.stopPropagation());  // don't toggle the checkbox
      colorInput.addEventListener('change', () => this.onAgentColor?.(agent.id, agent.name, colorInput.value));
      list.appendChild(label);
    }
  }

  setOnFilterChange(cb: (agents: string[], eventTypes: string[]) => void): void {
    this.onFilterChange = cb;
  }

  setOnShowFileLabelsChange(cb: (show: boolean) => void): void {
    this.onShowFileLabelsChange = cb;
  }

  setOnAgentColorChange(cb: (id: string, name: string, color: string) => void): void {
    this.onAgentColor = cb;
  }

  updateStatus(status: StatusUpdate): void {
    this.playing = status.playing;
    this.speed = status.speed;
    this.position = status.position;
    this.total = status.total;
    this.currentTime = status.timestamp;

    this.playBtn.textContent = this.playing ? '⏸' : '▶';
    this.scrubber.max = String(this.total);
    this.scrubber.value = String(this.position);
    this.updateTimeDisplay();
  }

  private buildUI(): void {
    this.container.innerHTML = `
      <div class="transport-bar">
        <div class="transport-controls">
          <button class="transport-btn" id="rewind-btn" title="Rewind to start">⏮</button>
          <button class="transport-btn" id="play-btn" title="Play/Pause">▶</button>
          <button class="transport-btn" id="forward-btn" title="Skip to end">⏭</button>
          <button class="transport-btn" id="replay-btn" title="Replay from start">↻</button>
        </div>
        <div class="scrubber-container">
          <input type="range" class="scrubber" id="scrubber" min="0" max="0" value="0">
        </div>
        <div class="speed-container">
          <select id="speed-select">
            <option value="1">1x</option>
            <option value="2">2x</option>
            <option value="5">5x</option>
            <option value="10">10x</option>
            <option value="20">20x</option>
            <option value="50">50x</option>
            <option value="100">100x</option>
          </select>
        </div>
        <div class="time-display">
          <span id="time-display">--:--:-- / --:--:--</span>
        </div>
        <button class="transport-btn" id="filter-toggle" title="Filters">⚙</button>
      </div>
      <div class="filter-panel" id="filter-panel" style="display:none">
        <div class="filter-section">
          <h4>Agents</h4>
          <div class="agent-filters"></div>
        </div>
        <div class="filter-section">
          <h4>Event Types</h4>
          <div class="event-filters">
            <label class="filter-item"><input type="checkbox" checked data-event-type="agent_state"> State Changes</label>
            <label class="filter-item"><input type="checkbox" checked data-event-type="message"> Messages</label>
            <label class="filter-item"><input type="checkbox" checked data-event-type="file_edit"> File Edits</label>
            <label class="filter-item"><input type="checkbox" checked data-event-type="file_read"> File Reads</label>
            <label class="filter-item"><input type="checkbox" checked data-event-type="agent_create"> Lifecycle</label>
          </div>
        </div>
        <div class="filter-section">
          <h4>Display</h4>
          <div class="display-options">
            <label class="filter-item"><input type="checkbox" checked id="show-file-labels"> File Labels</label>
          </div>
        </div>
      </div>
    `;

    this.playBtn = this.container.querySelector('#play-btn')!;
    this.speedSelect = this.container.querySelector('#speed-select')!;
    this.scrubber = this.container.querySelector('#scrubber')!;
    this.timeDisplay = this.container.querySelector('#time-display')!;
    this.filterPanel = this.container.querySelector('#filter-panel')!;
    this.filterToggle = this.container.querySelector('#filter-toggle')!;

    // Play/Pause
    this.playBtn.addEventListener('click', () => {
      this.ws.send({ type: this.playing ? 'pause' : 'play' });
    });

    // Rewind
    this.container.querySelector('#rewind-btn')!.addEventListener('click', () => {
      this.ws.send({ type: 'seek', timestamp: this.timeStart });
    });

    // Forward
    this.container.querySelector('#forward-btn')!.addEventListener('click', () => {
      this.ws.send({ type: 'seek', timestamp: this.timeEnd });
    });

    // Replay — jump back to the start AND play, in one click (vs. Rewind then Play).
    this.container.querySelector('#replay-btn')!.addEventListener('click', () => {
      this.ws.send({ type: 'seek', timestamp: this.timeStart });
      this.ws.send({ type: 'play' });
    });

    // Speed
    this.speedSelect.addEventListener('change', () => {
      const multiplier = parseFloat(this.speedSelect.value);
      this.ws.send({ type: 'speed', multiplier });
    });

    // Scrubber
    let scrubbing = false;
    this.scrubber.addEventListener('mousedown', () => { scrubbing = true; });
    this.scrubber.addEventListener('mouseup', () => {
      scrubbing = false;
      // Calculate timestamp from position
      if (this.timeStart && this.timeEnd) {
        const start = new Date(this.timeStart).getTime();
        const end = new Date(this.timeEnd).getTime();
        const ratio = parseInt(this.scrubber.value) / Math.max(1, parseInt(this.scrubber.max));
        const ts = new Date(start + (end - start) * ratio).toISOString();
        this.ws.send({ type: 'seek', timestamp: ts });
      }
    });
    this.scrubber.addEventListener('input', () => {
      if (!scrubbing) return;
      // Update display while dragging
      if (this.timeStart && this.timeEnd) {
        const start = new Date(this.timeStart).getTime();
        const end = new Date(this.timeEnd).getTime();
        const ratio = parseInt(this.scrubber.value) / Math.max(1, parseInt(this.scrubber.max));
        this.currentTime = new Date(start + (end - start) * ratio).toISOString();
        this.updateTimeDisplay();
      }
    });

    // Filter toggle
    this.filterToggle.addEventListener('click', () => {
      const showing = this.filterPanel.style.display !== 'none';
      this.filterPanel.style.display = showing ? 'none' : 'flex';
    });

    // Event type filter changes
    this.filterPanel.querySelectorAll('.event-filters input').forEach((input) => {
      input.addEventListener('change', () => this.emitFilter());
    });

    // Display toggle: file labels
    const fileLabelsToggle = this.container.querySelector<HTMLInputElement>('#show-file-labels')!;
    fileLabelsToggle.addEventListener('change', () => {
      this.onShowFileLabelsChange?.(fileLabelsToggle.checked);
    });
  }

  private emitFilter(): void {
    // Collect checked agents
    const agentInputs = this.filterPanel.querySelectorAll<HTMLInputElement>('.agent-filters input');
    const allAgentsChecked = Array.from(agentInputs).every((i) => i.checked);
    const agents = allAgentsChecked
      ? []
      : Array.from(agentInputs)
          .filter((i) => i.checked)
          .map((i) => i.dataset.agentId!);

    // Collect checked event types
    const eventInputs = this.filterPanel.querySelectorAll<HTMLInputElement>('.event-filters input');
    const allEventsChecked = Array.from(eventInputs).every((i) => i.checked);
    const eventTypes = allEventsChecked
      ? []
      : Array.from(eventInputs)
          .filter((i) => i.checked)
          .map((i) => i.dataset.eventType!);

    // Send to server
    this.ws.send({ type: 'filter', agents, eventTypes });

    // Notify local callback
    this.onFilterChange?.(agents, eventTypes);
  }

  private updateTimeDisplay(): void {
    const current = this.currentTime ? this.formatTime(this.currentTime) : '--:--:--';
    const end = this.timeEnd ? this.formatTime(this.timeEnd) : '--:--:--';
    this.timeDisplay.textContent = `${current} / ${end}`;
  }

  private formatTime(iso: string): string {
    try {
      const d = new Date(iso);
      return d.toLocaleTimeString();
    } catch {
      return '--:--:--';
    }
  }
}
