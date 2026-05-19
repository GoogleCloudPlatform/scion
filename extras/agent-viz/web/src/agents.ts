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

import type { AgentInfo, AgentRenderState, AgentStateEvent } from './types';
import { getIconForState } from './icons';

const AGENT_RADIUS = 24;
const RING_PADDING = 100;
const ENTER_DURATION = 600; // ms for agent to appear (scale-in)
const EXIT_DURATION = 800; // ms for agent to disappear (fade + shrink)
const REBALANCE_DURATION = 500; // ms for ring positions to animate

interface AnimatingAgent extends AgentRenderState {
  enterTime?: number; // timestamp when agent was added
  exitTime?: number; // timestamp when removal started
  removing?: boolean;
  // For smooth position animation during rebalance
  targetAngle: number;
  prevAngle: number;
  rebalanceStart: number;
}

export class AgentRing {
  private agents: Map<string, AnimatingAgent> = new Map();
  private ringRadius = 300;
  private centerX = 0;
  private centerY = 0;
  private animationPhase = 0;
  private freezeCount = 0; // reference-counted freeze (multiple beams can overlap)
  private pendingSlots = 0; // slots claimed by in-flight create beams
  private pendingAngles: number[] = []; // angles of claimed pending slots

  init(agentInfos: AgentInfo[], centerX: number, centerY: number): void {
    this.centerX = centerX;
    this.centerY = centerY;
    this.ringRadius = Math.min(centerX, centerY) - RING_PADDING;
    if (this.ringRadius < 150) this.ringRadius = 150;

    const n = agentInfos.length;
    agentInfos.forEach((info, i) => {
      const angle = (2 * Math.PI * i) / n - Math.PI / 2;
      this.agents.set(info.id, {
        info,
        angle,
        targetAngle: angle,
        prevAngle: angle,
        rebalanceStart: 0,
        x: this.centerX + Math.cos(angle) * this.ringRadius,
        y: this.centerY + Math.sin(angle) * this.ringRadius,
        phase: 'created',
        activity: 'working',
        toolName: '',
        enterTime: Date.now(),
      });
    });
  }

  updateLayout(centerX: number, centerY: number): void {
    this.centerX = centerX;
    this.centerY = centerY;
    this.ringRadius = Math.min(centerX, centerY) - RING_PADDING;
    if (this.ringRadius < 150) this.ringRadius = 150;

    for (const agent of this.agents.values()) {
      if (agent.removing) continue;
      agent.x = this.centerX + Math.cos(agent.angle) * this.ringRadius;
      agent.y = this.centerY + Math.sin(agent.angle) * this.ringRadius;
    }
  }

  addAgent(info: AgentInfo): void {
    if (this.agents.has(info.id)) return;
    for (const a of this.agents.values()) {
      if (a.info.name === info.name && !a.removing) return;
    }

    const now = Date.now();
    const angle = this.findBestInsertAngle();

    this.agents.set(info.id, {
      info,
      angle,
      targetAngle: angle,
      prevAngle: angle,
      rebalanceStart: 0,
      x: this.centerX + Math.cos(angle) * this.ringRadius,
      y: this.centerY + Math.sin(angle) * this.ringRadius,
      phase: 'created',
      activity: 'working',
      toolName: '',
      enterTime: now,
    });

    // Release one pending slot if any (beam has delivered)
    if (this.pendingSlots > 0) {
      this.pendingSlots--;
      this.pendingAngles.shift();
    }

    this.redistributeAgents();
  }

  /** Find the angle in the largest gap between existing agents. First agent goes to top. */
  private findBestInsertAngle(): number {
    const liveAgents = this.getLiveAgents();
    if (liveAgents.length === 0) return -Math.PI / 2; // first agent at top

    const angles = liveAgents.map((a) => normalizeAngle(a.angle));
    angles.sort((a, b) => a - b);

    // Find largest angular gap
    let bestGap = 0;
    let bestMid = 0;
    for (let i = 0; i < angles.length; i++) {
      const next = i + 1 < angles.length ? angles[i + 1] : angles[0] + 2 * Math.PI;
      const gap = next - angles[i];
      if (gap > bestGap) {
        bestGap = gap;
        bestMid = angles[i] + gap / 2;
      }
    }

    // Normalize back to [-π, π)
    if (bestMid >= Math.PI) bestMid -= 2 * Math.PI;
    return bestMid;
  }

  removeAgent(agentId: string): void {
    let agent = this.agents.get(agentId);
    if (!agent) {
      for (const a of this.agents.values()) {
        if (a.info.name === agentId || a.info.id === agentId) {
          agent = a;
          break;
        }
      }
    }
    if (!agent || agent.removing) return;

    agent.removing = true;
    agent.exitTime = Date.now();

    // Rebalance remaining agents after a short delay
    setTimeout(() => this.redistributeAgents(), 100);
  }

  private getLiveAgents(): AnimatingAgent[] {
    return Array.from(this.agents.values()).filter((a) => !a.removing);
  }

  private redistributeAgents(): void {
    if (this.freezeCount > 0) return;
    const liveAgents = this.getLiveAgents();
    const n = liveAgents.length;
    if (n === 0) return;

    // Sort by current angle to preserve ring order
    liveAgents.sort((a, b) => normalizeAngle(a.angle) - normalizeAngle(b.angle));

    // Target angles: evenly spaced starting from -π/2
    const targets: number[] = [];
    for (let i = 0; i < n; i++) {
      targets.push((2 * Math.PI * i) / n - Math.PI / 2);
    }

    // Find the rotation offset that minimizes total angular displacement.
    // Try all n rotations of the sorted→target assignment and pick the cheapest.
    let bestOffset = 0;
    let bestCost = Infinity;
    for (let offset = 0; offset < n; offset++) {
      let cost = 0;
      for (let i = 0; i < n; i++) {
        cost += angularDistance(liveAgents[i].angle, targets[(i + offset) % n]);
      }
      if (cost < bestCost) {
        bestCost = cost;
        bestOffset = offset;
      }
    }

    const now = Date.now();
    for (let i = 0; i < n; i++) {
      const agent = liveAgents[i];
      const target = targets[(i + bestOffset) % n];
      // Use shortest-arc direction to avoid going the long way around
      agent.prevAngle = agent.angle;
      agent.targetAngle = agent.angle + shortestArc(agent.angle, target);
      agent.rebalanceStart = now;
    }
  }

  /** Freeze agent positions — prevents rebalance animations during beam travel. Reference-counted. */
  freezeRebalance(): void {
    this.freezeCount++;
    // Stop any in-progress rebalance by snapping to current positions
    for (const agent of this.agents.values()) {
      if (agent.rebalanceStart > 0 && !agent.removing) {
        agent.prevAngle = agent.angle;
        agent.targetAngle = agent.angle;
        agent.rebalanceStart = 0;
        // Snap x,y to match current angle
        agent.x = this.centerX + Math.cos(agent.angle) * this.ringRadius;
        agent.y = this.centerY + Math.sin(agent.angle) * this.ringRadius;
      }
    }
  }

  /** Unfreeze (decrement ref count). Triggers rebalance when all freezes released. */
  unfreezeRebalance(): void {
    this.freezeCount = Math.max(0, this.freezeCount - 1);
    if (this.freezeCount === 0) {
      this.redistributeAgents();
    }
  }

  /**
   * Claim an estimated ring position for a new agent that doesn't exist yet.
   * Uses the largest-gap algorithm, including previously claimed pending positions.
   */
  claimNextSlotPosition(): { x: number; y: number } {
    const live = this.getLiveAgents();
    // Collect all occupied angles: live agents + previously claimed pending slots
    const angles = live.map((a) => normalizeAngle(a.angle));
    // Add synthetic angles for previously claimed pending slots (evenly distributed in gaps)
    for (let p = 0; p < this.pendingSlots; p++) {
      angles.push(normalizeAngle(this.pendingAngles[p] ?? 0));
    }
    angles.sort((a, b) => a - b);

    let angle: number;
    if (angles.length === 0) {
      angle = -Math.PI / 2; // first agent at top
    } else {
      // Find largest gap
      let bestGap = 0;
      let bestMid = 0;
      for (let i = 0; i < angles.length; i++) {
        const next = i + 1 < angles.length ? angles[i + 1] : angles[0] + 2 * Math.PI;
        const gap = next - angles[i];
        if (gap > bestGap) {
          bestGap = gap;
          bestMid = angles[i] + gap / 2;
        }
      }
      if (bestMid >= Math.PI) bestMid -= 2 * Math.PI;
      angle = bestMid;
    }

    // Track this pending angle so the next claim avoids it
    this.pendingAngles.push(normalizeAngle(angle));
    this.pendingSlots++;

    return {
      x: this.centerX + Math.cos(angle) * this.ringRadius,
      y: this.centerY + Math.sin(angle) * this.ringRadius,
    };
  }

  updateState(event: AgentStateEvent): void {
    let agent = this.agents.get(event.agentId);
    if (!agent) {
      for (const a of this.agents.values()) {
        if (a.info.name === event.agentId || a.info.id === event.agentId) {
          agent = a;
          break;
        }
      }
    }
    if (!agent) return;

    if (event.phase) agent.phase = event.phase;
    if (event.activity) agent.activity = event.activity;
    if (event.toolName !== undefined) agent.toolName = event.toolName;
  }

  getAgentPosition(agentIdOrName: string): { x: number; y: number } | null {
    const byId = this.agents.get(agentIdOrName);
    if (byId && !byId.removing) return { x: byId.x, y: byId.y };
    for (const a of this.agents.values()) {
      if (a.info.name === agentIdOrName && !a.removing) return { x: a.x, y: a.y };
    }
    return null;
  }

  getRingGeometry(): { centerX: number; centerY: number; radius: number } {
    return { centerX: this.centerX, centerY: this.centerY, radius: this.ringRadius };
  }

  getAgentColor(agentIdOrName: string): string {
    const byId = this.agents.get(agentIdOrName);
    if (byId) return byId.info.color;
    for (const a of this.agents.values()) {
      if (a.info.name === agentIdOrName) return a.info.color;
    }
    return '#888';
  }

  reset(): void {
    this.agents.clear();
    this.freezeCount = 0;
    this.pendingSlots = 0;
    this.pendingAngles = [];
  }

  draw(ctx: CanvasRenderingContext2D): void {
    this.animationPhase = (Date.now() / 1000) % (2 * Math.PI);
    const now = Date.now();

    // Draw ring circle (faint)
    ctx.beginPath();
    ctx.arc(this.centerX, this.centerY, this.ringRadius, 0, Math.PI * 2);
    ctx.strokeStyle = 'rgba(255,255,255,0.05)';
    ctx.lineWidth = 1;
    ctx.stroke();

    // Clean up fully removed agents
    for (const [id, agent] of this.agents.entries()) {
      if (agent.removing && agent.exitTime && now - agent.exitTime > EXIT_DURATION) {
        this.agents.delete(id);
      }
    }

    // Update positions with rebalance animation and draw
    for (const agent of this.agents.values()) {
      // Animate rebalance
      if (agent.rebalanceStart > 0 && !agent.removing) {
        const elapsed = now - agent.rebalanceStart;
        if (elapsed < REBALANCE_DURATION) {
          const t = easeInOutCubic(elapsed / REBALANCE_DURATION);
          agent.angle = agent.prevAngle + (agent.targetAngle - agent.prevAngle) * t;
        } else {
          agent.angle = agent.targetAngle;
          agent.rebalanceStart = 0;
        }
        agent.x = this.centerX + Math.cos(agent.angle) * this.ringRadius;
        agent.y = this.centerY + Math.sin(agent.angle) * this.ringRadius;
      }

      this.drawAgent(ctx, agent, now);
    }
  }

  private drawAgent(ctx: CanvasRenderingContext2D, agent: AnimatingAgent, now: number): void {
    const { x, y, info, phase, activity } = agent;
    const icon = getIconForState(phase, activity);

    // Calculate enter/exit animation scales
    let scale = 1;
    let alpha = 1;

    if (agent.enterTime) {
      const elapsed = now - agent.enterTime;
      if (elapsed < ENTER_DURATION) {
        const t = elapsed / ENTER_DURATION;
        // Elastic ease-out for a bouncy entrance
        scale = elasticOut(t);
        alpha = Math.min(1, t * 2);
      }
    }

    if (agent.removing && agent.exitTime) {
      const elapsed = now - agent.exitTime;
      if (elapsed < EXIT_DURATION) {
        const t = elapsed / EXIT_DURATION;
        scale = 1 - easeInCubic(t);
        alpha = 1 - t;
      } else {
        return; // fully gone
      }
    }

    // Pulse effect for pulsing states
    let pulseScale = 1;
    if (icon.pulse) {
      pulseScale = 1 + 0.08 * Math.sin(this.animationPhase * 3);
    }

    const r = AGENT_RADIUS * pulseScale * scale;
    if (r < 0.5) return;

    ctx.save();
    ctx.globalAlpha = alpha;

    // Outer glow for active agents
    if ((activity === 'thinking' || activity === 'executing') && scale > 0.5) {
      ctx.save();
      ctx.globalAlpha = alpha * (0.3 + 0.1 * Math.sin(this.animationPhase * 3));
      ctx.shadowBlur = 20;
      ctx.shadowColor = icon.color;
      ctx.beginPath();
      ctx.arc(x, y, r + 6, 0, Math.PI * 2);
      ctx.fillStyle = icon.color;
      ctx.fill();
      ctx.restore();
    }

    // Sparkle effect on creation
    if (agent.enterTime) {
      const elapsed = now - agent.enterTime;
      if (elapsed < ENTER_DURATION * 1.5) {
        const sparkleT = elapsed / (ENTER_DURATION * 1.5);
        const sparkleAlpha = alpha * (1 - sparkleT) * 0.6;
        const sparkleR = r + 20 * sparkleT;
        ctx.save();
        ctx.globalAlpha = sparkleAlpha;
        ctx.beginPath();
        ctx.arc(x, y, sparkleR, 0, Math.PI * 2);
        ctx.strokeStyle = info.color;
        ctx.lineWidth = 2 * (1 - sparkleT);
        ctx.stroke();
        ctx.restore();
      }
    }

    // Shatter effect on removal
    if (agent.removing && agent.exitTime) {
      const elapsed = now - agent.exitTime;
      const t = elapsed / EXIT_DURATION;
      if (t < 1) {
        // Expanding ring fragments
        const fragCount = 6;
        for (let i = 0; i < fragCount; i++) {
          const fragAngle = (2 * Math.PI * i) / fragCount + this.animationPhase;
          const dist = r * 0.5 + r * 1.5 * t;
          const fx = x + Math.cos(fragAngle) * dist;
          const fy = y + Math.sin(fragAngle) * dist;
          ctx.save();
          ctx.globalAlpha = alpha * (1 - t) * 0.5;
          ctx.beginPath();
          ctx.arc(fx, fy, 2 * (1 - t), 0, Math.PI * 2);
          ctx.fillStyle = info.color;
          ctx.fill();
          ctx.restore();
        }
      }
    }

    // Agent circle background
    ctx.beginPath();
    ctx.arc(x, y, r, 0, Math.PI * 2);
    ctx.fillStyle = info.color;
    ctx.fill();

    // Agent circle border
    ctx.strokeStyle = icon.color;
    ctx.lineWidth = 2.5;
    ctx.stroke();

    // Icon indicator inside circle
    ctx.beginPath();
    ctx.arc(x, y, r * 0.45, 0, Math.PI * 2);
    ctx.fillStyle = icon.color;
    ctx.fill();

    // Phase indicator badge (top-right)
    if (phase && phase !== 'running' && phase !== 'created' && scale > 0.7) {
      const badgeR = 5;
      const bx = x + r * 0.7;
      const by = y - r * 0.7;
      ctx.beginPath();
      ctx.arc(bx, by, badgeR, 0, Math.PI * 2);
      ctx.fillStyle = this.getPhaseColor(phase);
      ctx.fill();
      ctx.strokeStyle = 'rgba(0,0,0,0.4)';
      ctx.lineWidth = 1;
      ctx.stroke();
    }

    // Agent name label below
    if (scale > 0.3) {
      ctx.font = 'bold 12px sans-serif';
      ctx.textAlign = 'center';
      ctx.textBaseline = 'top';
      ctx.fillStyle = `rgba(255,255,255,${0.9 * alpha})`;
      ctx.fillText(info.name, x, y + r + 6);
    }

    // Tool name (when executing)
    if (activity === 'executing' && agent.toolName && scale > 0.5) {
      ctx.font = '10px sans-serif';
      ctx.fillStyle = `rgba(255,255,255,${0.5 * alpha})`;
      ctx.fillText(agent.toolName, x, y + r + 20);
    }

    ctx.restore();
  }

  private getPhaseColor(phase: string): string {
    switch (phase) {
      case 'starting':
        return '#ffc107';
      case 'stopping':
        return '#ff5722';
      case 'stopped':
        return '#6c757d';
      case 'error':
        return '#dc3545';
      default:
        return '#198754';
    }
  }
}

/** Normalize angle to [0, 2π) for consistent sorting. */
function normalizeAngle(a: number): number {
  return ((a % (2 * Math.PI)) + 2 * Math.PI) % (2 * Math.PI);
}

/** Shortest signed arc from angle a to angle b. */
function shortestArc(a: number, b: number): number {
  let d = b - a;
  // Wrap to [-π, π]
  d = ((d + Math.PI) % (2 * Math.PI)) - Math.PI;
  if (d < -Math.PI) d += 2 * Math.PI;
  return d;
}

/** Absolute shortest angular distance between two angles. */
function angularDistance(a: number, b: number): number {
  return Math.abs(shortestArc(a, b));
}

// Easing functions
function easeInOutCubic(t: number): number {
  return t < 0.5 ? 4 * t * t * t : 1 - Math.pow(-2 * t + 2, 3) / 2;
}

function easeInCubic(t: number): number {
  return t * t * t;
}

function elasticOut(t: number): number {
  if (t === 0 || t === 1) return t;
  const p = 0.4;
  return Math.pow(2, -10 * t) * Math.sin(((t - p / 4) * (2 * Math.PI)) / p) + 1;
}
