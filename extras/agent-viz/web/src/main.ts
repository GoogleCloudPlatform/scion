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

import { WSClient } from './ws';
import { FileGraph } from './graph';
import { AgentRing } from './agents';
import { MessageRenderer } from './messages';
import { FileEditRenderer } from './files';
import { DestroyBeamRenderer } from './destroy-beam';
import { CreateBeamRenderer } from './create-beam';
import { PlaybackControls } from './playback';
import type {
  PlaybackManifest,
  PlaybackEvent,
  StatusUpdate,
  AgentStateEvent,
  MessageEvent,
  FileEditEvent,
  AgentLifecycleEvent,
  SnapshotMessage,
} from './types';

// Main application state
let fileGraph: FileGraph;
let agentRing: AgentRing;
let messageRenderer: MessageRenderer;
let fileEditRenderer: FileEditRenderer;
let destroyBeamRenderer: DestroyBeamRenderer;
let createBeamRenderer: CreateBeamRenderer;
let playbackControls: PlaybackControls;
let overlayCanvas: HTMLCanvasElement;
let overlayCtx: CanvasRenderingContext2D;
let animFrameId: number;
let manifest: PlaybackManifest | null = null;

/**
 * Clamp a file path to the configured max depth.
 * If the path has more segments than maxDepth, truncate and return a directory path.
 * Returns the (possibly truncated) path and whether it's a directory due to truncation.
 */
function clampFilePath(filePath: string, maxDepth: number): { path: string; clamped: boolean } {
  if (maxDepth <= 0) return { path: filePath, clamped: false };
  const parts = filePath.split('/');
  if (parts.length <= maxDepth) return { path: filePath, clamped: false };
  return { path: parts.slice(0, maxDepth).join('/'), clamped: true };
}

function init(): void {
  const graphContainer = document.getElementById('graph-container')!;
  const controlsContainer = document.getElementById('controls-container')!;

  // Initialize force-graph FIRST — it wipes the container's innerHTML on init
  fileGraph = new FileGraph(graphContainer);

  // Create overlay canvas AFTER force-graph so it isn't destroyed
  overlayCanvas = document.createElement('canvas');
  overlayCanvas.id = 'overlay-canvas';
  overlayCanvas.style.cssText =
    'position:absolute;top:0;left:0;width:100%;height:100%;pointer-events:none;z-index:10;';
  graphContainer.appendChild(overlayCanvas);
  overlayCtx = overlayCanvas.getContext('2d')!;

  // Initialize components
  agentRing = new AgentRing();
  messageRenderer = new MessageRenderer();
  fileEditRenderer = new FileEditRenderer();
  destroyBeamRenderer = new DestroyBeamRenderer();
  createBeamRenderer = new CreateBeamRenderer();

  // Wire up cross-references
  fileEditRenderer.setFileGraph(fileGraph);
  fileEditRenderer.setAgentRing(agentRing);
  messageRenderer.setAgentRing(agentRing);
  destroyBeamRenderer.setAgentRing(agentRing);
  createBeamRenderer.setAgentRing(agentRing);

  // WebSocket
  const ws = new WSClient();
  playbackControls = new PlaybackControls(controlsContainer, ws);
  playbackControls.setOnShowFileLabelsChange((show) => fileGraph.setShowLabels(show));

  ws.onMessage((msg) => {
    if ('type' in msg) {
      switch (msg.type) {
        case 'manifest':
          handleManifest(msg as PlaybackManifest);
          break;
        case 'status':
          playbackControls.updateStatus(msg as StatusUpdate);
          break;
        case 'snapshot':
          handleSnapshot(msg as SnapshotMessage);
          break;
        case 'agent_state':
        case 'message':
        case 'file_edit':
        case 'file_read':
        case 'agent_create':
        case 'agent_destroy':
          handleEvent(msg as PlaybackEvent);
          break;
      }
    }
  });

  // Handle resize
  window.addEventListener('resize', handleResize);
  handleResize();

  // Connect
  ws.connect();

  // Start animation loop
  animate();
}

function handleManifest(m: PlaybackManifest): void {
  manifest = m;
  console.log('[agent-viz] Manifest received:', {
    agents: m.agents.length,
    files: m.files.length,
    timeRange: m.timeRange,
    projectId: m.projectId,
    projectName: m.projectName,
  });
  console.log('[agent-viz] Agents:', m.agents.map((a) => `${a.name} (${a.id.substring(0, 8)})`));

  // Update title
  const title = document.getElementById('project-title');
  if (title) {
    title.textContent = m.projectName || m.projectId || 'Agent Visualizer';
  }

  // Initialize file graph empty — files are added dynamically via events
  fileGraph.init([]);

  // Initialize agent ring empty — agents are added via agent_create events
  const w = overlayCanvas.width;
  const h = overlayCanvas.height;
  console.log('[agent-viz] Canvas dimensions:', w, 'x', h);
  agentRing.init([], w / 2, h / 2);

  // Set up playback controls
  playbackControls.setTimeRange(m.timeRange.start, m.timeRange.end);
  playbackControls.setAgents(m.agents);

  // Update info display
  updateInfoDisplay();
}

function updateInfoDisplay(): void {
  const info = document.getElementById('info-display');
  if (info) {
    info.textContent = `${manifest?.agents.length ?? 0} agents`;
  }
}

function handleSnapshot(snapshot: SnapshotMessage): void {
  console.log('[agent-viz] Snapshot received with', snapshot.events.length, 'events');

  // Reset all dynamic state
  resetState();

  // Replay all events instantly (no animations)
  for (const evt of snapshot.events) {
    handleEventInstant(evt);
  }
}

function resetState(): void {
  agentRing.reset();
  fileGraph.reset();
  messageRenderer.reset();
  fileEditRenderer.reset();
  destroyBeamRenderer.reset();
  createBeamRenderer.reset();

  // Re-init empty state
  const w = overlayCanvas.width;
  const h = overlayCanvas.height;
  agentRing.init([], w / 2, h / 2);
}

// Handle an event instantly without animations (used during snapshot replay)
function handleEventInstant(evt: PlaybackEvent): void {
  switch (evt.type) {
    case 'agent_state':
      agentRing.updateState(evt.data as AgentStateEvent);
      break;
    case 'message':
      // Skip message animations during replay
      break;
    case 'file_edit':
    case 'file_read': {
      const fileEvt = evt.data as FileEditEvent;
      if (!fileEvt.filePath) break;
      const { path: fp, clamped } = clampFilePath(fileEvt.filePath, manifest?.maxDepth ?? 0);
      if (!fileGraph.hasFile(fp)) {
        if (clamped) {
          fileGraph.addFile(fp + '/', true); // truncated → directory, visible immediately
        } else {
          fileGraph.addFile(fp, true); // visible immediately during replay
        }
      }
      break;
    }
    case 'agent_create': {
      const lifecycle = evt.data as AgentLifecycleEvent;
      const agentInfo = resolveAgentInfo(lifecycle);
      agentRing.addAgent(agentInfo);
      agentRing.updateState({
        agentId: lifecycle.agentId,
        phase: 'created',
        activity: 'working',
      });
      break;
    }
    case 'agent_destroy': {
      const lifecycle = evt.data as AgentLifecycleEvent;
      agentRing.updateState({
        agentId: lifecycle.agentId,
        phase: 'stopped',
        activity: 'completed',
      });
      agentRing.removeAgent(lifecycle.agentId);
      break;
    }
  }
}

function handleEvent(evt: PlaybackEvent): void {
  switch (evt.type) {
    case 'agent_state':
      agentRing.updateState(evt.data as AgentStateEvent);
      break;
    case 'message':
      messageRenderer.addMessage(evt.data as MessageEvent, agentRing);
      break;
    case 'file_edit':
    case 'file_read': {
      const fileEvt = evt.data as FileEditEvent;
      if (!fileEvt.filePath) break;
      const { path: fp, clamped } = clampFilePath(fileEvt.filePath, manifest?.maxDepth ?? 0);
      // Rewrite the event's filePath so particles target the clamped node
      const clampedEvt: FileEditEvent = { ...fileEvt, filePath: fp };
      // Dynamically add file to graph if not already present
      if (!fileGraph.hasFile(fp)) {
        if (clamped) {
          // Truncated path is a directory — always visible immediately
          fileGraph.addFile(fp + '/', true);
        } else {
          // For create actions, file starts invisible; for edits/reads it's visible
          const visible = fileEvt.action !== 'create';
          fileGraph.addFile(fp, visible);
        }
      }
      fileEditRenderer.addFileEdit(clampedEvt, agentRing, fileGraph);
      break;
    }
    case 'agent_create': {
      const lifecycle = evt.data as AgentLifecycleEvent;
      const agentInfo = resolveAgentInfo(lifecycle);

      if (lifecycle.requestedBy) {
        // Fire a create beam — the beam renderer will add the agent when it arrives.
        // If the requesting agent isn't on the ring (already removed, not yet created),
        // addBeam returns false and we fall back to adding the agent directly.
        const beamFired = createBeamRenderer.addBeam(lifecycle.requestedBy, agentInfo, agentRing);
        if (beamFired) {
          // Set initial state after a delay matching beam travel
          setTimeout(() => {
            agentRing.updateState({
              agentId: lifecycle.agentId,
              phase: 'created',
              activity: 'working',
            });
          }, 900); // BEAM_CHARGE + BEAM_TRAVEL duration
          break;
        }
        // Beam couldn't fire — fall through to direct add
      }
      agentRing.addAgent(agentInfo);
      agentRing.updateState({
        agentId: lifecycle.agentId,
        phase: 'created',
        activity: 'working',
      });
      break;
    }
    case 'agent_destroy': {
      const lifecycle = evt.data as AgentLifecycleEvent;

      if (lifecycle.requestedBy) {
        // Fire a destroy beam — freeze ring, beam handles timing
        destroyBeamRenderer.addBeam(
          lifecycle.requestedBy,
          lifecycle.name || lifecycle.agentId,
          agentRing
        );
        // Set stopping state immediately (visual badge only)
        agentRing.updateState({
          agentId: lifecycle.agentId,
          phase: 'stopping',
          activity: 'executing',
        });
        // Remove agent when beam arrives (charge 300 + travel 400 = 700ms)
        setTimeout(() => {
          agentRing.updateState({
            agentId: lifecycle.agentId,
            phase: 'stopped',
            activity: 'completed',
          });
          agentRing.removeAgent(lifecycle.agentId);
        }, 700);
      } else {
        agentRing.updateState({
          agentId: lifecycle.agentId,
          phase: 'stopped',
          activity: 'completed',
        });
        agentRing.removeAgent(lifecycle.agentId);
      }
      break;
    }
  }
}

/** Resolve agent info from lifecycle event, checking manifest for color/harness. */
function resolveAgentInfo(lifecycle: AgentLifecycleEvent) {
  const fromManifest = manifest?.agents.find(
    (a) => a.id === lifecycle.agentId || a.name === lifecycle.name
  );
  return fromManifest ?? {
    id: lifecycle.agentId,
    name: lifecycle.name || lifecycle.agentId.substring(0, 8),
    harness: 'unknown',
    color: '#888',
  };
}

function handleResize(): void {
  const w = window.innerWidth;
  const h = window.innerHeight - 60; // reserve space for controls

  overlayCanvas.width = w;
  overlayCanvas.height = h;

  fileGraph.resize(w, h);
  agentRing.updateLayout(w / 2, h / 2);
}

function animate(): void {
  // Clear overlay
  overlayCtx.clearRect(0, 0, overlayCanvas.width, overlayCanvas.height);

  // Draw agents on ring
  agentRing.draw(overlayCtx);

  // Draw message lines
  messageRenderer.draw(overlayCtx);

  // Draw file edit particles
  fileEditRenderer.draw(overlayCtx);

  // Draw beams
  createBeamRenderer.draw(overlayCtx);
  destroyBeamRenderer.draw(overlayCtx);

  animFrameId = requestAnimationFrame(animate);
}

// Start when DOM is ready
if (document.readyState === 'loading') {
  document.addEventListener('DOMContentLoaded', init);
} else {
  init();
}
