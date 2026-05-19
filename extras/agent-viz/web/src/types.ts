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

export interface PlaybackManifest {
  type: 'manifest';
  timeRange: { start: string; end: string };
  agents: AgentInfo[];
  files: FileNode[];
  projectId: string;
  projectName: string;
  maxDepth: number;
}

export interface AgentInfo {
  id: string;
  name: string;
  harness: string;
  color: string;
}

export interface FileNode {
  id: string;
  name: string;
  parent: string;
  isDir: boolean;
}

export interface PlaybackEvent {
  type: 'agent_state' | 'message' | 'file_edit' | 'file_read' | 'agent_create' | 'agent_destroy';
  timestamp: string;
  data: AgentStateEvent | MessageEvent | FileEditEvent | AgentLifecycleEvent;
}

export interface AgentStateEvent {
  agentId: string;
  phase?: string;
  activity?: string;
  toolName?: string;
}

export interface MessageEvent {
  sender: string;
  recipient: string;
  msgType: string;
  content?: string;
  broadcasted: boolean;
}

export interface FileEditEvent {
  agentId: string;
  filePath: string;
  action: 'create' | 'edit' | 'read';
}

export interface AgentLifecycleEvent {
  agentId: string;
  name: string;
  action: 'create' | 'destroy';
  requestedBy?: string;
}

export interface SnapshotMessage {
  type: 'snapshot';
  events: PlaybackEvent[];
}

export interface PlaybackCommand {
  type: 'play' | 'pause' | 'seek' | 'speed' | 'filter';
  timestamp?: string;
  multiplier?: number;
  agents?: string[];
  eventTypes?: string[];
  timeRange?: { start: string; end: string };
}

export interface StatusUpdate {
  type: 'status';
  playing: boolean;
  speed: number;
  position: number;
  total: number;
  timestamp: string;
}

// Internal graph node used by force-graph
export interface GraphNode {
  id: string;
  name: string;
  isDir: boolean;
  parent: string;
  x?: number;
  y?: number;
  fx?: number;
  fy?: number;
  // Visual state
  highlighted?: boolean;
  highlightTime?: number;
  visible: boolean;
  revealTime?: number;
}

export interface GraphLink {
  source: string | GraphNode;
  target: string | GraphNode;
}

// Agent rendering state
export interface AgentRenderState {
  info: AgentInfo;
  angle: number;
  x: number;
  y: number;
  phase: string;
  activity: string;
  toolName: string;
}
