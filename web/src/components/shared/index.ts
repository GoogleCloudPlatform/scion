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
 * Shared Components Exports
 *
 * Re-exports all shared Lit components
 */

export { ScionNav } from './nav.js';
export { ScionHeader } from './header.js';
export { ScionBreadcrumb } from './breadcrumb.js';
export { ScionStatusBadge } from './status-badge.js';
export type { StatusType } from './status-badge.js';
export { ScionDebugPanel } from './debug-panel.js';
export { ScionEnvVarList } from './env-var-list.js';
export { ScionSecretList } from './secret-list.js';
export { ScionNotificationTray } from './notification-tray.js';
export { ScionInboxTray } from './inbox-tray.js';
export { ScionViewToggle } from './view-toggle.js';
export type { ViewMode } from './view-toggle.js';
export { resourceStyles, listPageStyles, brokerTypeBadgeStyles } from './resource-styles.js';
export { ScionJsonBrowser } from './json-browser.js';
export { ScionAgentLogViewer } from './agent-log-viewer.js';
export { ScionSharedDirList } from './shared-dir-list.js';
export { ScionFileBrowser } from './file-browser.js';
export type { FileEntry, FileListResult, FileBrowserDataSource } from './file-browser.js';
export {
  WorkspaceFileBrowserDataSource,
  SharedDirFileBrowserDataSource,
  TemplateFileBrowserDataSource,
} from './file-browser.js';
export { ScionCodeEditor, getLanguageFromPath } from './code-editor.js';
export {
  ScionFileEditor,
  WorkspaceFileEditorDataSource,
  SharedDirFileEditorDataSource,
  TemplateFileEditorDataSource,
} from './file-editor.js';
export type { FileEditorDataSource, FileContentResponse } from './file-editor.js';
export { ScionGCPServiceAccountList } from './gcp-service-account-list.js';
export { ScionSubscriptionManager } from './subscription-manager.js';
export { ScionGitRemoteDisplay } from './git-remote-display.js';
export { ScionHashDisplay } from './hash-display.js';
export { ScionPreStartHookList } from './pre-start-hook-list.js';
export { ScionQuickMessageDialog } from './quick-message-dialog.js';
export { ScionPrincipalPicker } from './principal-picker.js';
export type { PrincipalChangeDetail } from './principal-picker.js';
export { ScionEffectiveRoleProvenance } from './effective-role-provenance.js';
export { ScionAuthorizationLayerStack } from './authorization-layer-stack.js';
export type {
  BoundaryLayer,
  IntrinsicRestriction,
  DeniedPermission,
  PermissionDenialReason,
} from './authorization-layer-stack.js';
export { ScionEffectiveAccessBoundaryNotice } from './effective-access-boundary-notice.js';
export { ScionProjectMembersEditor } from './project-members-editor.js';
export {
  SYSTEM_DIRECT_USER_ONLY_ROLES,
  PROJECT_DIRECT_USER_ONLY_ROLES,
  PROJECT_OWNER_ROLE_NAMES,
  getLifecycleStatus,
  formatDateTime,
  getPrincipalIcon,
} from './role-binding-utils.js';
export type { LifecycleStatus } from './role-binding-utils.js';
