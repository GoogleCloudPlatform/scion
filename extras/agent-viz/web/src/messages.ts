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

import type { MessageEvent } from './types';
import type { AgentRing } from './agents';

const PULSE_DURATION = 600; // ms for pulse travel
const FADE_DURATION = 500;  // ms for line fade after pulse
const LABEL_DURATION = 2000; // ms for content label to remain visible

const BROADCAST_DURATION = 1400; // ms for ripple expansion
const BROADCAST_FADE_START = 0.9; // start fading at 90% of expansion
const BROADCAST_DEDUP_WINDOW = 2000; // ms window for deduplicating broadcast messages

interface ActiveMessage {
  senderName: string;
  recipientName: string;
  color: string;
  msgType: string;
  content: string;
  startTime: number;
  broadcast: boolean;
}

/** Key for deduplicating broadcast messages (same sender + same content). */
function broadcastKey(sender: string, content: string): string {
  return `${sender}::${content}`;
}

export class MessageRenderer {
  private activeMessages: ActiveMessage[] = [];
  private agentRing: AgentRing | null = null;
  /** Tracks recently seen broadcasts for dedup: key -> timestamp of first occurrence. */
  private recentBroadcasts: Map<string, number> = new Map();
  // summarizeContent runs in the per-frame draw loop; cache by content so we don't re-run the
  // regex/split work at 60fps for every active message.
  private summaryCache: Map<string, string> = new Map();

  setAgentRing(ring: AgentRing): void {
    this.agentRing = ring;
  }

  addMessage(event: MessageEvent, agentRing: AgentRing): void {
    const color = agentRing.getAgentColor(event.sender);

    if (event.broadcasted) {
      const key = broadcastKey(event.sender, event.content || '');
      const now = Date.now();
      const prev = this.recentBroadcasts.get(key);
      if (prev && now - prev < BROADCAST_DEDUP_WINDOW) {
        // Duplicate broadcast in the same burst — skip animation
        return;
      }
      this.recentBroadcasts.set(key, now);

      this.activeMessages.push({
        senderName: event.sender,
        recipientName: '', // not used for broadcasts
        color,
        msgType: event.msgType,
        content: event.content || '',
        startTime: now,
        broadcast: true,
      });
      return;
    }

    this.activeMessages.push({
      senderName: event.sender,
      recipientName: event.recipient,
      color,
      msgType: event.msgType,
      content: event.content || '',
      startTime: Date.now(),
      broadcast: false,
    });
  }

  reset(): void {
    this.activeMessages = [];
    this.recentBroadcasts.clear();
  }

  draw(ctx: CanvasRenderingContext2D): void {
    const now = Date.now();

    // Expire old dedup entries
    for (const [key, ts] of this.recentBroadcasts) {
      if (now - ts > BROADCAST_DEDUP_WINDOW) {
        this.recentBroadcasts.delete(key);
      }
    }

    // Remove expired messages
    this.activeMessages = this.activeMessages.filter((m) => {
      if (m.broadcast) {
        return now - m.startTime < Math.max(BROADCAST_DURATION, LABEL_DURATION);
      }
      const totalDuration = Math.max(PULSE_DURATION + FADE_DURATION, LABEL_DURATION);
      return now - m.startTime < totalDuration;
    });

    for (const msg of this.activeMessages) {
      if (msg.broadcast) {
        this.drawBroadcast(ctx, msg, now);
      } else {
        this.drawDirectMessage(ctx, msg, now);
      }
    }
  }

  private drawBroadcast(ctx: CanvasRenderingContext2D, msg: ActiveMessage, now: number): void {
    const s = this.agentRing?.getAgentPosition(msg.senderName);
    const ring = this.agentRing?.getRingGeometry();
    if (!s || !ring) return;

    const elapsed = now - msg.startTime;
    if (elapsed >= BROADCAST_DURATION) return;

    const t = elapsed / BROADCAST_DURATION;

    // Ease-out (decelerate): 1 - (1-t)^3
    const eased = 1 - Math.pow(1 - t, 3);

    // Interpolate center: sender position → ring center
    const cx = s.x + (ring.centerX - s.x) * eased;
    const cy = s.y + (ring.centerY - s.y) * eased;

    // Interpolate radius: small start → ring radius (congruent at t=1)
    const minRadius = 28; // start just outside the agent circle
    const currentRadius = minRadius + (ring.radius - minRadius) * eased;

    // Fade out in last 10% of range
    let alpha = 0.7;
    if (t > BROADCAST_FADE_START) {
      const fadeT = (t - BROADCAST_FADE_START) / (1 - BROADCAST_FADE_START);
      alpha = 0.7 * (1 - fadeT);
    }

    // Draw the ripple ring
    ctx.save();
    ctx.beginPath();
    ctx.arc(cx, cy, currentRadius, 0, Math.PI * 2);
    ctx.strokeStyle = this.hexToRgba(msg.color, alpha);
    ctx.lineWidth = 2.5 * (1 - t * 0.5); // thin out slightly as it expands
    ctx.shadowBlur = 8 * (1 - t);
    ctx.shadowColor = msg.color;
    ctx.stroke();
    ctx.restore();

    // Content label at sender position (offset above)
    if (msg.content && elapsed < LABEL_DURATION) {
      const labelAlpha = t < 0.3
        ? Math.min(1, t / 0.15)
        : 1 - Math.max(0, (elapsed - PULSE_DURATION) / (LABEL_DURATION - PULSE_DURATION));

      const label = this.summarizeContent(msg.content);
      if (label) {
        ctx.save();
        ctx.globalAlpha = Math.max(0, labelAlpha) * 0.9;
        ctx.font = '10px sans-serif';
        ctx.textAlign = 'center';
        ctx.textBaseline = 'bottom';

        const metrics = ctx.measureText(label);
        const pad = 4;
        const pillW = metrics.width + pad * 2;
        const pillH = 14;
        const labelY = s.y - 40;
        ctx.fillStyle = 'rgba(0, 0, 0, 0.6)';
        roundRect(ctx, s.x - pillW / 2, labelY - pillH - 4, pillW, pillH, 4);
        ctx.fill();

        ctx.fillStyle = this.hexToRgba(msg.color, 1);
        ctx.fillText(label, s.x, labelY - 5);
        ctx.restore();
      }
    }
  }

  private drawDirectMessage(ctx: CanvasRenderingContext2D, msg: ActiveMessage, now: number): void {
    const elapsed = now - msg.startTime;

    // Resolve positions dynamically each frame
    const s = this.agentRing?.getAgentPosition(msg.senderName);
    const r = this.agentRing?.getAgentPosition(msg.recipientName);
    if (!s || !r) return;

    if (elapsed < PULSE_DURATION) {
      // Pulse traveling phase
      const t = elapsed / PULSE_DURATION;

      // Draw line (growing)
      ctx.beginPath();
      ctx.moveTo(s.x, s.y);
      const currentX = s.x + (r.x - s.x) * t;
      const currentY = s.y + (r.y - s.y) * t;
      ctx.lineTo(currentX, currentY);
      ctx.strokeStyle = this.getLineColor(msg, 0.6);
      ctx.lineWidth = this.getLineWidth(msg);
      ctx.stroke();

      // Pulse dot
      ctx.beginPath();
      ctx.arc(currentX, currentY, 4, 0, Math.PI * 2);
      ctx.fillStyle = this.getLineColor(msg, 1);
      ctx.shadowBlur = 10;
      ctx.shadowColor = msg.color;
      ctx.fill();
      ctx.shadowBlur = 0;
    } else if (elapsed < PULSE_DURATION + FADE_DURATION) {
      // Fading phase
      const fadeT = (elapsed - PULSE_DURATION) / FADE_DURATION;
      const alpha = 1 - fadeT;

      ctx.beginPath();
      ctx.moveTo(s.x, s.y);
      ctx.lineTo(r.x, r.y);
      ctx.strokeStyle = this.getLineColor(msg, alpha * 0.6);
      ctx.lineWidth = this.getLineWidth(msg);
      ctx.stroke();
    }

    // Content label near the SOURCE end of the line (not mid-edge) — it reads as "this is what
    // the sender just produced + is handing off", and stays clear of the recipient ring.
    if (msg.content && elapsed < LABEL_DURATION) {
      const labelAlpha = elapsed < PULSE_DURATION
        ? Math.min(1, (elapsed / PULSE_DURATION) * 2)
        : 1 - (elapsed - PULSE_DURATION) / (LABEL_DURATION - PULSE_DURATION);

      const frac = 0.28; // 28% of the way from sender → recipient
      const midX = s.x + (r.x - s.x) * frac;
      const midY = s.y + (r.y - s.y) * frac;

      // Extract a short summary from content
      const label = this.summarizeContent(msg.content);
      if (label) {
        ctx.save();
        ctx.globalAlpha = Math.max(0, labelAlpha) * 0.9;
        ctx.font = '10px sans-serif';
        ctx.textAlign = 'center';
        ctx.textBaseline = 'bottom';

        // Background pill
        const metrics = ctx.measureText(label);
        const pad = 4;
        const pillW = metrics.width + pad * 2;
        const pillH = 14;
        ctx.fillStyle = 'rgba(0, 0, 0, 0.6)';
        roundRect(ctx, midX - pillW / 2, midY - pillH - 4, pillW, pillH, 4);
        ctx.fill();

        // Text
        ctx.fillStyle = this.getLineColor(msg, 1);
        ctx.fillText(label, midX, midY - 5);
        ctx.restore();
      }
    }
  }

  private summarizeContent(content: string): string {
    if (!content) return '';
    const cached = this.summaryCache.get(content);
    if (cached !== undefined) return cached;

    let s = content.trim().replace(/^```[a-zA-Z]*\n?/, '').replace(/\n?```$/, '').trim();
    // Legacy scion state messages: "… has reached a state of COMPLETED: …"
    const stateMatch = s.match(/state of (\w+)/i);
    let result: string;
    if (stateMatch) {
      result = stateMatch[1];
    } else {
      // JSON/array payload → pull a "title" if there is one, else strip the punctuation.
      if (s[0] === '[' || s[0] === '{') {
        const m = s.match(/"title"\s*:\s*"((?:[^"\\]|\\.)*)"/);
        s = m ? m[1].replace(/\\"/g, '"') : s.replace(/[[\]{}"]/g, ' ').trim();
      }
      // First meaningful, de-marked line (drop leading #, >, *, - markdown).
      for (const line of s.split('\n')) {
        const ln = line.replace(/^[#>*\-\s]+/, '').trim();
        if (ln) { s = ln; break; }
      }
      result = s.length > 26 ? s.slice(0, 25) + '…' : s;
    }
    this.summaryCache.set(content, result);
    return result;
  }

  private hexToRgba(hex: string, alpha: number): string {
    const rr = parseInt(hex.slice(1, 3), 16);
    const g = parseInt(hex.slice(3, 5), 16);
    const b = parseInt(hex.slice(5, 7), 16);
    return `rgba(${rr}, ${g}, ${b}, ${alpha})`;
  }

  private getLineColor(msg: ActiveMessage, alpha: number): string {
    if (msg.msgType === 'input-needed') {
      return `rgba(255, 193, 7, ${alpha})`;
    }
    return this.hexToRgba(msg.color, alpha);
  }

  private getLineWidth(msg: ActiveMessage): number {
    switch (msg.msgType) {
      case 'instruction':
        return 2.5;
      case 'state-change':
        return 1.5;
      case 'input-needed':
        return 3;
      default:
        return 2;
    }
  }
}

function roundRect(
  ctx: CanvasRenderingContext2D,
  x: number,
  y: number,
  w: number,
  h: number,
  r: number
): void {
  ctx.beginPath();
  ctx.moveTo(x + r, y);
  ctx.lineTo(x + w - r, y);
  ctx.quadraticCurveTo(x + w, y, x + w, y + r);
  ctx.lineTo(x + w, y + h - r);
  ctx.quadraticCurveTo(x + w, y + h, x + w - r, y + h);
  ctx.lineTo(x + r, y + h);
  ctx.quadraticCurveTo(x, y + h, x, y + h - r);
  ctx.lineTo(x, y + r);
  ctx.quadraticCurveTo(x, y, x + r, y);
  ctx.closePath();
}
