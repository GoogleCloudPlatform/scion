// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

import { Terminal } from '@xterm/xterm';
import { FitAddon } from '@xterm/addon-fit';
import '@xterm/xterm/css/xterm.css';

// A P0 proof of the proposed lifecycle, NOT the production terminal page.
// Keep the actual xterm parser/renderer; mock only the external PTY transport.
class RetainedTerminalFixture {
  readonly pane = document.querySelector<HTMLDivElement>('#pane')!;
  readonly terminal = new Terminal({ scrollback: 5000 });
  readonly fit = new FitAddon();
  readonly socket = new WebSocket(`ws://${location.host}/fixture-pty`);
  readonly observer = new ResizeObserver(() => this.scheduleFit());
  parsed = 0;
  disposals = 0;
  private closed = false;
  private timer: ReturnType<typeof setTimeout> | undefined;
  private lastSize = '';
  private remembered?: { terminal: Terminal; socket: WebSocket; element: HTMLElement | undefined };

  constructor() {
    this.terminal.loadAddon(this.fit);
    this.terminal.open(this.pane);
    const dispose = this.terminal.dispose.bind(this.terminal);
    this.terminal.dispose = (): void => {
      this.disposals++;
      dispose();
    };
    this.socket.onmessage = (event): void => {
      if (this.closed) return;
      const message = JSON.parse(event.data as string) as { type: string; data: string };
      if (message.type === 'data') {
        // Match terminal.ts: feed raw bytes, never decode each frame separately.
        const bytes = Uint8Array.from(atob(message.data), (char) => char.charCodeAt(0));
        this.terminal.write(bytes, () => this.parsed++);
      }
    };
    this.socket.onopen = (): void => this.fitVisible();
    this.observer.observe(this.pane);
  }

  private measurable(): boolean {
    return (
      !this.closed && !this.pane.hidden && this.pane.clientWidth > 0 && this.pane.clientHeight > 0
    );
  }

  private fitVisible(): void {
    if (!this.measurable() || this.socket.readyState !== WebSocket.OPEN) return;
    this.fit.fit();
    const { cols, rows } = this.terminal;
    const size = `${cols}x${rows}`;
    if (cols > 0 && rows > 0 && size !== this.lastSize) {
      this.socket.send(JSON.stringify({ type: 'resize', cols, rows }));
      this.lastSize = size;
    }
  }

  scheduleFit(): void {
    if (!this.measurable()) return;
    clearTimeout(this.timer);
    this.timer = setTimeout(() => this.fitVisible(), 100);
  }

  hide(): void {
    this.terminal.blur();
    this.pane.inert = true;
    this.pane.hidden = true;
    clearTimeout(this.timer);
  }

  async reveal(): Promise<void> {
    if (this.closed) return;
    this.pane.hidden = false;
    this.pane.inert = false;
    await new Promise<void>((resolve) => requestAnimationFrame(() => resolve()));
    this.fitVisible();
    if (!this.closed) this.terminal.refresh(0, this.terminal.rows - 1);
  }

  close(): void {
    if (this.closed) return;
    this.closed = true;
    if (this.socket.readyState === WebSocket.OPEN) {
      this.socket.send(JSON.stringify({ type: 'data', data: btoa('\x02d') }));
    }
    this.socket.close(1000, 'detach');
    this.observer.disconnect();
    clearTimeout(this.timer);
    this.terminal.dispose();
  }

  text(): string {
    const buffer = this.terminal.buffer.active;
    return Array.from({ length: buffer.length }, (_, index) =>
      buffer.getLine(index)!.translateToString(true)
    )
      .join('\n')
      .trimEnd();
  }

  remember(): void {
    this.remembered = {
      terminal: this.terminal,
      socket: this.socket,
      element: this.terminal.element,
    };
  }

  sameIdentity(): boolean {
    return (
      this.remembered?.terminal === this.terminal &&
      this.remembered?.socket === this.socket &&
      this.remembered?.element === this.terminal.element
    );
  }
}

declare global {
  interface Window {
    fixture: RetainedTerminalFixture;
  }
}
window.fixture = new RetainedTerminalFixture();
