/**
 * Copyright 2026 Google LLC
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

import type { ReactiveController, ReactiveControllerHost } from 'lit';

export const LOCALE_CHANGE_EVENT = 'scion:locale-change';
export const LOCALE_STORAGE_KEY = 'scion-locale';

export const SUPPORTED_LOCALES = ['en', 'zh-CN'] as const;
export type Locale = (typeof SUPPORTED_LOCALES)[number];

type TranslationValues = Record<string, string | number>;
type TranslationTable = Record<string, string>;
type TranslatableRoot = Document | ShadowRoot;

interface TranslationPattern {
  regex: RegExp;
  names: string[];
  translated: string;
}

const ZH_CN: TranslationTable = {
  'Add a project workspace': '添加项目工作区',
  'Add local directory': '添加本地目录',
  Admin: '管理',
  Agent: 'Agent',
  'Agent Orchestration': 'Agent 编排',
  Agents: 'Agents',
  'Agents are AI-powered workers that can help you with coding tasks.':
    'Agent 是可以帮助你完成编码任务的 AI 工作单元。',
  'Agents are AI-powered workers that can help you with coding tasks. Create your first agent to get started.':
    'Agent 是可以帮助你完成编码任务的 AI 工作单元。创建第一个 Agent 开始使用。',
  'Allow List': '允许列表',
  'An error occurred during image operation.': '镜像操作时发生错误。',
  'Authorized users': '已授权用户',
  Back: '返回',
  Broker: 'Broker',
  Brokers: 'Brokers',
  'Browse project workspaces': '浏览项目工作区',
  'Build locally': '本地构建',
  'Build output ({count} lines)': '构建输出（{count} 行）',
  Collapse: '收起',
  'Collapse sidebar': '收起侧边栏',
  'Configure Agent': '配置 Agent',
  'Connect to an existing git repository for source-controlled workspaces.':
    '连接已有 git 仓库，用于受版本控制的工作区。',
  'Connect to running agent': '连接到正在运行的 Agent',
  'Container Images': '容器镜像',
  'Container Runtime': '容器运行时',
  'Create & Continue': '创建并继续',
  'Create Agent': '创建 Agent',
  'Create your first agent to get started.': '创建第一个 Agent 开始使用。',
  'Create Hub Workspace': '创建 Hub 工作区',
  'Create Project': '创建项目',
  'Create Skill': '创建技能',
  'Currently configured:': '当前配置：',
  Dashboard: '仪表盘',
  'Detected:': '检测到：',
  'Detecting runtime...': '正在检测运行时...',
  Directory: '目录',
  'Display Name': '显示名称',
  Email: '邮箱',
  'Expand sidebar': '展开侧边栏',
  'Failed to connect to the server.': '无法连接到服务器。',
  'Failed to create project': '创建项目失败',
  'Failed to initialize harnesses': '初始化 harness 失败',
  'Failed to load runtime info': '加载运行时信息失败',
  'Failed to save identity': '保存身份信息失败',
  'Failed to save runtime': '保存运行时失败',
  'Failed to start image build': '启动镜像构建失败',
  'Failed to start image pull': '启动镜像拉取失败',
  'First Workspace': '第一个工作区',
  'Git 2.47+ is required for agent worktrees. Detected: {version}. Run brew install git to upgrade.':
    'Agent worktree 需要 Git 2.47 或更高版本。检测到：{version}。运行 brew install git 升级。',
  'GitHub App Setup': 'GitHub App 设置',
  'Go to Dashboard': '进入仪表盘',
  Group: '用户组',
  Groups: '用户组',
  'Harness Config': 'Harness 配置',
  Help: '帮助',
  "Here's what's happening with your agents today.": '这里是今天 Agent 的运行概况。',
  'Hub Resources': 'Hub 资源',
  'Hub-managed project': 'Hub 托管项目',
  'A workspace managed by the Hub. No git repository required.':
    '由 Hub 管理的工作区，不需要 git 仓库。',
  'Image registry is required to pull container images.': '拉取容器镜像需要配置镜像仓库。',
  'Install Docker or Podman to pull or build images. You can skip this step and configure a runtime later.':
    '安装 Docker 或 Podman 后即可拉取或构建镜像。你也可以跳过这一步，稍后再配置运行时。',
  Invite: '邀请',
  Language: '语言',
  'Link a git repo': '关联 git 仓库',
  Maintenance: '维护',
  Management: '管理',
  Metrics: '指标',
  'Metrics Dashboard': '指标仪表盘',
  Next: '下一步',
  'No Agents Found': '没有 Agent',
  'No Brokers Found': '没有 Broker',
  'No Matching Agents': '没有匹配的 Agent',
  'No Shared Agents': '没有共享 Agent',
  'No agents have been shared with you yet.': '还没有与你共享的 Agent。',
  'No agents match the current filter. Try changing the status filter.':
    '没有 Agent 匹配当前筛选条件。请尝试更改状态筛选。',
  'No container runtime detected.': '未检测到容器运行时。',
  'No embedded broker available.': '没有可用的内嵌 Broker。',
  'No image registry configured.': '未配置镜像仓库。',
  'No project ID in response': '响应中没有项目 ID',
  'No Skills Found': '没有技能',
  'Open navigation menu': '打开导航菜单',
  'Open Terminal': '打开终端',
  Overview: '概览',
  'Page Not Found': '页面未找到',
  'Pending Invites': '待处理邀请',
  'Please enter at least a display name or email.': '请至少输入显示名称或邮箱。',
  Profile: '个人资料',
  Project: '项目',
  'Project Metrics': '项目指标',
  'Project Name': '项目名称',
  'Project Settings': '项目设置',
  'Project workspaces': '项目工作区',
  Projects: '项目',
  'Pull images': '拉取镜像',
  'Pull or build the container images for your selected harnesses.':
    '拉取或构建所选 harness 的容器镜像。',
  'Quick Actions': '快捷操作',
  'Re-check': '重新检查',
  Ready: '就绪',
  'Running checks...': '正在运行检查...',
  Runtime: '运行时',
  'Runtime Brokers': '运行时 Broker',
  Schedules: '计划任务',
  Scheduler: '调度器',
  'Select the container runtime for your workstation.': '选择当前工作站使用的容器运行时。',
  'Select which AI coding harnesses to configure.': '选择要配置的 AI 编码 harness。',
  'Server Config': '服务器配置',
  Settings: '设置',
  Setup: '设置',
  'Sign in': '登录',
  'Sign out': '退出登录',
  Skill: '技能',
  'Skill Registries': '技能注册表',
  'Skill Registry': '技能注册表',
  Skills: '技能',
  'Skip for now': '暂时跳过',
  'Spin up a new AI agent': '启动一个新的 AI Agent',
  'Switch language': '切换语言',
  'Switch to English': '切换到英文',
  'Switch to Chinese': '切换到中文',
  'System Check': '系统检查',
  'System check failed': '系统检查失败',
  Template: '模板',
  Terminal: '终端',
  'To pull pre-built images instead, configure an image registry.':
    '如果要拉取预构建镜像，请配置镜像仓库。',
  Users: '用户',
  'Verifying your environment is ready.': '正在确认环境是否可用。',
  'View Projects': '查看项目',
  'Welcome back, {name}!': '欢迎回来，{name}！',
  'Welcome to Scion': '欢迎使用 Scion',
  'You do not have permission to {action} this resource.': '你没有权限对该资源执行“{action}”。',
  "You're All Set": '全部设置完成',
  'Your name': '你的姓名',
  'Your workstation is configured and ready to use.': '你的工作站已经配置完成，可以开始使用。',
  "Let's get your workstation set up. First, tell us who you are.":
    '先完成工作站设置。第一步，请确认你的身份信息。',
  'Create your first project to get started.': '创建第一个项目后即可开始使用。',
  'Give your project a name.': '给项目起一个名称。',
  'Browse to or enter the path of a local directory.': '浏览或输入本地目录路径。',
  'Selected Path': '已选路径',
  'Validating...': '正在验证...',
  'Path does not exist.': '路径不存在。',
  'Not a directory.': '不是目录。',
  'Path is valid: {path}': '路径有效：{path}',
  'This is a git repository.': '这是一个 git 仓库。',
  'Already linked to another project.': '已关联到另一个项目。',
  'Project created but failed to link directory. You can retry.':
    '项目已创建，但目录关联失败。你可以重试。',
  'No harnesses selected. You can go back to select harnesses or skip this step.':
    '没有选择 harness。你可以返回选择 harness，或跳过这一步。',
  'Pre-built images are available via pull. Local builds require a source checkout.':
    '预构建镜像可以通过拉取获得。本地构建需要源码检出。',
  'Run the following to configure one, then restart the server:':
    '运行以下命令配置镜像仓库，然后重启服务器：',
  'If you installed via Homebrew, try reinstalling to auto-configure the registry:':
    '如果你通过 Homebrew 安装，可以尝试重新安装以自动配置镜像仓库：',
  '{count} total redemptions': '累计兑换 {count} 次',
  '{count}d ago': '{count} 天前',
  '{count}h ago': '{count} 小时前',
  '{count}m ago': '{count} 分钟前',
  '{used}/{max} uses': '已使用 {used}/{max} 次',
  'Access Tokens': '访问令牌',
  'Active Agents': '活跃 Agent',
  'Add Local Directory': '添加本地目录',
  'AI Harnesses': 'AI 编码工具',
  'An image registry is required to pull container images.': '拉取容器镜像需要配置镜像仓库。',
  Configuration: '配置',
  'Container (generic)': 'Container（通用）',
  'Environment Variables': '环境变量',
  'Git version': 'Git 版本',
  'GitHub Application': 'GitHub 应用',
  Integrations: '集成',
  'Invite {prefix} redeemed ({uses})': '邀请 {prefix} 已兑换（{uses}）',
  'just now': '刚刚',
  'Link a local directory. It stays where it is and is operated on in place.':
    '关联本地目录。目录位置保持不变，并在原地操作。',
  'Loading...': '正在加载...',
  Login: '登录',
  'No recent activity to display.': '暂无最近活动。',
  none: '无',
  Notifications: '通知',
  'Notifications & Settings': '通知和设置',
  'Recent Activity': '最近活动',
  redeemed: '已兑换',
  'Return to Hub': '返回 Hub',
  Secrets: '密钥',
  'Start by creating your first agent.': '先创建第一个 Agent。',
  'Step {current} of {total}': '第 {current} / {total} 步',
  Telegram: 'Telegram',
  there: '你好',
  'Toggle dark mode': '切换深色模式',
  User: '用户',
  pass: '通过',
  warn: '警告',
  fail: '失败',
  pending: '待处理',
  'Profile & Settings': '个人资料与设置',
  queued: '排队中',
  pulling: '拉取中',
  done: '完成',
  exists: '已存在',
  'Access Denied': '访问被拒绝',
  'Access Mode': '访问模式',
  'Account ID': '账号 ID',
  Actions: '操作',
  Active: '活跃',
  'Active Agents (24h)': '活跃 Agent（24 小时）',
  'Active Agents per Day': '每日活跃 Agent',
  'Active Profile': '当前配置',
  'Active Subscriptions': '活跃订阅',
  'Active Timers': '活跃计时器',
  Activity: '活动',
  Add: '添加',
  'Add Directory': '添加目录',
  'Add Member': '添加成员',
  'Add Secret': '添加密钥',
  'Add Shared Directory': '添加共享目录',
  'Add Variable': '添加变量',
  Added: '已添加',
  'Admin Emails': '管理员邮箱',
  'After granting the role, click the refresh icon to re-check verification.':
    '授予角色后，点击刷新图标重新检查验证状态。',
  'Agent configuration template.': 'Agent 配置模板。',
  'Agent Error': 'Agent 错误',
  'Agent ID': 'Agent ID',
  'Agent IDs:': 'Agent ID：',
  'Agent Instructions': 'Agent 指令',
  'Agent Lifecycle': 'Agent 生命周期',
  'Agent Name': 'Agent 名称',
  'Agent Notifications': 'Agent 通知',
  'Agent Orchestration Platform': 'Agent 编排平台',
  'Agent window': 'Agent 窗口',
  'Agents can read but not write to this directory': 'Agent 可以读取但不能写入此目录',
  'Agents I created': '我创建的 Agent',
  'Agents in shared projects': '共享项目中的 Agent',
  'Agents will use PAT fallback if available.': '如果可用，Agent 将回退使用 PAT。',
  All: '全部',
  'All agents': '全部 Agent',
  'All brokers': '全部 Broker',
  'All projects': '全部项目',
  'All scopes': '全部范围',
  'All Scopes': '全部范围',
  'All severities': '全部级别',
  'Allow agent progeny to access': '允许 Agent 的后代访问',
  always: '始终',
  Always: '始终',
  'API Base URL': 'API 基础 URL',
  'API Calls': 'API 调用',
  'API Calls (24h)': 'API 调用（24 小时）',
  'API Calls by Harness': '按 Harness 统计 API 调用',
  'API Calls by Model': '按模型统计 API 调用',
  'App ID': '应用 ID',
  'Applied to new agents unless overridden by template or agent config.':
    '应用到新 Agent，除非被模板或 Agent 配置覆盖。',
  Archive: '归档',
  'Are you sure you want to delete': '确定要删除',
  'Are you sure you want to delete agent': '确定要删除 Agent',
  'as needed': '按需',
  'As needed': '按需',
  '"As needed" injects only when the agent configuration requests this value.':
    '“按需”仅在 Agent 配置请求该值时注入。',
  'Ask your Hub admin to configure the GitHub App integration, then install it on your organization or account.':
    '请 Hub 管理员配置 GitHub App 集成，然后将其安装到你的组织或账号。',
  'Assign Service Account': '分配服务账号',
  'At time': '指定时间',
  Attach: '关联',
  'Auth Method': '认证方式',
  'Auth Token (leave blank to keep current)': '认证令牌（留空则保留当前值）',
  'Auth Token (optional)': '认证令牌（可选）',
  Authentication: '认证',
  'Authorized Domains': '授权域名',
  'Authorized domains are also enforced — users must match both the allow list':
    '授权域名也会生效，用户必须同时匹配允许列表',
  'Auto Detected': '自动检测',
  'Auto-provide to hub projects': '自动提供给 Hub 项目',
  'Auto-suspend stalled agents': '自动挂起停滞的 Agent',
  'Automated recurring tasks for this project.': '此项目的自动化周期任务。',
  'Automatic token management for GitHub operations via GitHub App installation tokens.':
    '通过 GitHub App 安装令牌自动管理 GitHub 操作令牌。',
  'Back to Agent': '返回 Agent',
  'Back to Agents': '返回 Agent 列表',
  'Back to Brokers': '返回 Broker 列表',
  'Back to Dashboard': '返回仪表盘',
  'Back to files': '返回文件',
  'Back to Groups': '返回用户组',
  'Back to Project': '返回项目',
  'Back to Projects': '返回项目列表',
  'Back to Registries': '返回注册表',
  'Back to Skills': '返回技能列表',
  Backend: '后端',
  'Base URL': '基础 URL',
  Block: '阻止',
  Branch: '分支',
  broadcast: '广播',
  'Broadcast to all running agents in this project': '广播给此项目中所有正在运行的 Agent',
  'Broker IDs:': 'Broker ID：',
  'Broker Name': 'Broker 名称',
  'Broker Nickname': 'Broker 昵称',
  'Browse Directory': '浏览目录',
  'Browse…': '浏览…',
  'Browser notifications are allowed.': '浏览器通知已允许。',
  Bucket: '存储桶',
  Build: '构建',
  'Build Image': '构建镜像',
  'Build Output': '构建输出',
  'Build Time': '构建时间',
  By: '由',
  'By signing in, you agree to the': '登录即表示你同意',
  Cancel: '取消',
  'Cancel event': '取消事件',
  Capabilities: '能力',
  'Capture Auth': '捕获认证',
  'Capture credentials from inside the container': '从容器内捕获凭据',
  'Check All': '全部检查',
  'Check for Updates': '检查更新',
  'Checking for GitHub App installation…': '正在检查 GitHub App 安装…',
  'Choose a provider to continue': '选择登录提供方继续',
  'Choose a template (optional)': '选择模板（可选）',
  Clear: '清除',
  'Clear session and try again': '清除会话并重试',
  'Click "Load Quota" to fetch current minting statistics.': '点击“加载配额”获取当前签发统计。',
  'Click to copy': '点击复制',
  Clone: '克隆',
  'Clone from Global': '从全局克隆',
  'Clone per agent': '每个 Agent 单独克隆',
  Close: '关闭',
  created: '已创建',
  Cloud: '云端',
  'Cloud Export (OTLP)': '云端导出（OTLP）',
  Code: '代码',
  'Collect telemetry data for this agent. The default reflects the global telemetry setting.':
    '为此 Agent 收集遥测数据。默认值跟随全局遥测设置。',
  'Comma-separated list of email addresses to auto-promote to admin':
    '用英文逗号分隔要自动提升为管理员的邮箱地址',
  'Comma-separated list of email domains allowed to authenticate (empty = all)':
    '用英文逗号分隔允许认证的邮箱域名（留空表示全部）',
  'Comma-separated list of tags.': '用英文逗号分隔标签。',
  'comma-separated tags': '用英文逗号分隔标签',
  Configure: '配置',
  'Configure and start a new AI agent.': '配置并启动新的 AI Agent。',
  'Configure GitHub App': '配置 GitHub App',
  'Configure Installation': '配置安装',
  Configured: '已配置',
  Connected: '已连接',
  'Connecting to agent...': '正在连接 Agent...',
  'Connection ID': '连接 ID',
  'Connection State': '连接状态',
  Connectivity: '连接性',
  'Console Output': '控制台输出',
  'Container Hub Endpoint': '容器 Hub 端点',
  'Container image override': '容器镜像覆盖',
  'Container image registry for agent images (e.g., ghcr.io/myorg)':
    'Agent 镜像使用的容器镜像仓库（例如 ghcr.io/myorg）',
  'Container User': '容器用户',
  'Content Hash': '内容哈希',
  'Controls GCP metadata server access for new agents. "Block" prevents access, "Passthrough" allows host identity, "Assign" binds a specific service account.':
    '控制新 Agent 对 GCP 元数据服务器的访问。“阻止”会禁止访问，“透传”允许使用主机身份，“分配”会绑定指定服务账号。',
  'Controls telemetry for agents in this project. "Use hub default" inherits the server-level setting.':
    '控制此项目中 Agent 的遥测设置。“使用 Hub 默认值”会继承服务器级设置。',
  'Controls who can log in to this hub. Takes effect immediately (hot-reloaded).':
    '控制谁可以登录此 Hub。会立即生效（热加载）。',
  'Copy hash to clipboard': '复制哈希到剪贴板',
  "Copy this token now. You won't be able to see it again.":
    '请立即复制此令牌，之后将无法再次查看。',
  Core: '核心',
  'CPU Limit': 'CPU 限制',
  'CPU Request': 'CPU 请求',
  Create: '创建',
  'Create & Edit': '创建并编辑',
  'Create & Start Agent': '创建并启动 Agent',
  'Create a new project linked to a GitHub repository to start running agents.':
    '创建一个关联 GitHub 仓库的新项目，以开始运行 Agent。',
  'Create a new project to get started.': '创建一个新项目开始使用。',
  'Create a new skill, optionally publishing its first version in one step.':
    '创建新技能，也可以一步发布第一个版本。',
  'Create a recurring schedule to automate tasks on a cron cadence.':
    '创建周期计划，按 cron 节奏自动执行任务。',
  'Create a scheduled event to send messages at a future time.':
    '创建计划事件，在未来时间发送消息。',
  'Create Access Token': '创建访问令牌',
  'Create Invite': '创建邀请',
  'Create invite codes to allow new users to join the hub.': '创建邀请码，允许新用户加入 Hub。',
  'Create New Project': '创建新项目',
  'Create Only': '仅创建',
  'Create Recurring Schedule': '创建周期计划',
  'Create Registry': '创建注册表',
  'Create Schedule': '创建计划',
  'Create Skill Registry': '创建技能注册表',
  'Create Token': '创建令牌',
  Created: '已创建',
  'Created By': '创建者',
  'Created:': '创建时间：',
  'Creating skill...': '正在创建技能...',
  Cron: 'Cron',
  'Cron Expression': 'Cron 表达式',
  'Cron:': 'Cron：',
  'Current Scope': '当前范围',
  'Current State': '当前状态',
  'Current Task': '当前任务',
  'Custom Harness Config Name': '自定义 Harness 配置名称',
  'Daily Sessions': '每日会话',
  'Danger Zone': '危险区域',
  'Data & Storage': '数据和存储',
  Database: '数据库',
  'Date & Time': '日期和时间',
  Debug: '调试',
  'Debug Panel': '调试面板',
  default: '默认',
  Default: '默认',
  'Default Agent Limits': '默认 Agent 限制',
  'Default Agent Resources': '默认 Agent 资源',
  'Default Branch': '默认分支',
  'Default broker': '默认 Broker',
  'Default Harness Config': '默认 Harness 配置',
  'Default Max Duration': '默认最长时长',
  'Default Max Model Calls': '默认最大模型调用数',
  'Default Max Turns': '默认最大轮数',
  'Default Service Account': '默认服务账号',
  'Default Template': '默认模板',
  'Delete this project': '删除此项目',
  Deleted: '已删除',
  'Deleted Project IDs:': '已删除项目 ID：',
  'Deleted Projects': '已删除项目',
  Description: '描述',
  Details: '详情',
  'Dev Auth': '开发认证',
  Disable: '禁用',
  Disabled: '已禁用',
  Discover: '发现',
  Disk: '磁盘',
  Display: '显示',
  Domain: '域名',
  Download: '下载',
  Edit: '编辑',
  Enabled: '已启用',
  Endpoint: '端点',
  'Entire Project': '整个项目',
  'Environment Variable': '环境变量',
  'Event Type': '事件类型',
  'Event Type:': '事件类型：',
  Events: '事件',
  Export: '导出',
  Failed: '失败',
  'Failed to Load': '加载失败',
  'Failed to Load Project': '项目加载失败',
  'Failed to Load Schedules': '计划加载失败',
  'Failed to Load Skills': '技能加载失败',
  'Failed to Load Subscriptions': '订阅加载失败',
  'Filter files…': '过滤文件…',
  Files: '文件',
  'Forgot something?': '忘了什么？',
  General: '通用',
  Global: '全局',
  'GCP Project ID': 'GCP 项目 ID',
  'GCP Service Accounts': 'GCP 服务账号',
  'Git Repository': 'Git 仓库',
  'Git Remote': 'Git 远程仓库',
  'Grid view': '网格视图',
  'Group Details': '用户组详情',
  'Harness Config Files': 'Harness 配置文件',
  'Harness configuration used by default for new agents.': '新 Agent 默认使用的 Harness 配置。',
  'Hub-managed': 'Hub 托管',
  'Hub-minted': 'Hub 签发',
  ID: 'ID',
  Image: '镜像',
  'Image Tag': '镜像标签',
  Import: '导入',
  'Import from URL': '从 URL 导入',
  'Import from workspace': '从工作区导入',
  'Import Resources': '导入资源',
  'Install the GitHub App on your organization or account, then click Discover.':
    '将 GitHub App 安装到你的组织或账号，然后点击“发现”。',
  'Interrupt agent': '打断 Agent',
  "Interrupt the agent's current task before delivering the message.":
    '发送消息前先打断 Agent 当前任务。',
  'Invalid invite link': '邀请链接无效',
  'Invite-only Mode': '仅邀请模式',
  'Irreversible actions that affect this project and its resources.':
    '影响此项目及其资源的不可逆操作。',
  'Last Error:': '最近错误：',
  'Last Run:': '上次运行：',
  'Last Token Mint': '最近令牌签发',
  'Link your Telegram account to receive notifications and interact with agents.':
    '关联你的 Telegram 账号，用于接收通知并与 Agent 交互。',
  'List view': '列表视图',
  Limits: '限制',
  'Load Quota': '加载配额',
  'Loading agent...': '正在加载 Agent...',
  'Loading brokers...': '正在加载 Broker...',
  'Loading messages...': '正在加载消息...',
  'Loading project...': '正在加载项目...',
  'Loading recurring schedules...': '正在加载周期计划...',
  'Loading schedules...': '正在加载计划...',
  'Loading service accounts...': '正在加载服务账号...',
  'Loading settings...': '正在加载设置...',
  'Loading skills...': '正在加载技能...',
  'Loading subscriptions...': '正在加载订阅...',
  'Manage GCP service accounts for agent identity assignment in this project.':
    '管理此项目中用于 Agent 身份分配的 GCP 服务账号。',
  'Manage scheduled and recurring events for this project.': '管理此项目的计划事件和周期事件。',
  'Manage your notification subscriptions and preferences.': '管理你的通知订阅和偏好设置。',
  'Manage subscriptions': '管理订阅',
  'Memory Limit': '内存限制',
  'Memory Request': '内存请求',
  Message: '消息',
  'Message to send': '要发送的消息',
  'Message:': '消息：',
  Mode: '模式',
  Model: '模型',
  Name: '名称',
  'New folder name': '新文件夹名称',
  'Next Run': '下次运行',
  'Next Run:': '下次运行：',
  'No active subscriptions': '没有活跃订阅',
  'No agents match the current filter.': '没有 Agent 匹配当前筛选条件。',
  'No auth data loaded': '没有加载认证数据',
  'No brokers match the current filters.': '没有 Broker 匹配当前筛选条件。',
  'No events captured': '未捕获事件',
  'No GCP Service Accounts': '没有 GCP 服务账号',
  'No image configured': '未配置镜像',
  'No Matching Skills': '没有匹配的技能',
  'No messages found': '没有找到消息',
  'No notifications': '没有通知',
  'No Recurring Schedules': '没有周期计划',
  'No runtime brokers are registered for this project.': '此项目没有注册运行时 Broker。',
  'No schedules yet': '还没有计划',
  'No scope set': '未设置范围',
  'No Shared Directories': '没有共享目录',
  'No Subscriptions': '没有订阅',
  'No verified service accounts available': '没有可用的已验证服务账号',
  'None (default to block)': '无（默认阻止）',
  'None (use server default)': '无（使用服务器默认值）',
  'Notification Subscriptions': '通知订阅',
  'Optional description': '可选描述',
  'Optional human-friendly label': '可选的易读标签',
  Owner: '所有者',
  Passthrough: '透传',
  Pause: '暂停',
  Permissions: '权限',
  'Pin hash': '固定哈希',
  'Please sign in to continue': '请登录后继续',
  'Project configuration and agent defaults.': '项目配置和 Agent 默认值。',
  'Project ID': '项目 ID',
  'Project IDs:': '项目 ID：',
  'Project-scoped agent templates imported into the Hub.': '导入到 Hub 的项目级 Agent 模板。',
  'Project-scoped resources available to agents.': 'Agent 可用的项目级资源。',
  'Projects I own': '我拥有的项目',
  'Push Notifications': '推送通知',
  'Read-only': '只读',
  Reconnect: '重新连接',
  'Reconnect Attempts': '重连次数',
  Recurring: '周期',
  'Recurring Schedules': '周期计划',
  Refresh: '刷新',
  Register: '注册',
  'Register GCP Service Account': '注册 GCP 服务账号',
  "Register an existing GCP service account, or mint a new one in the Hub's project.":
    '注册现有 GCP 服务账号，或在 Hub 项目中签发一个新的服务账号。',
  Remove: '移除',
  'Remove broker': '移除 Broker',
  'Remove GitHub App installation from this project? Agents will fall back to PAT authentication.':
    '要从此项目中移除 GitHub App 安装吗？Agent 将回退到 PAT 认证。',
  Retry: '重试',
  Resume: '恢复',
  Role: '角色',
  'Run Count:': '运行次数：',
  Save: '保存',
  'Schedule: {name}': '计划：{name}',
  Search: '搜索',
  'Search agents...': '搜索 Agent...',
  'Search projects...': '搜索项目...',
  'Search skills...': '搜索技能...',
  'Searching...': '搜索中...',
  'Secret value': '密钥值',
  'Secret values are encrypted and can never be retrieved after saving.':
    '密钥值会被加密保存，保存后无法再次读取。',
  'Select a broker...': '选择 Broker...',
  'Select a group...': '选择用户组...',
  'Select a harness...': '选择 Harness...',
  'Select a project': '选择项目',
  'Select a project...': '选择项目...',
  'Select a service account...': '选择服务账号...',
  'Select a template...': '选择模板...',
  'Select a verified service account': '选择已验证服务账号',
  'Select All': '全选',
  'Select auth method...': '选择认证方式...',
  'Select this folder': '选择此文件夹',
  'Select version': '选择版本',
  Send: '发送',
  Sensitive: '敏感',
  sensitive: '敏感',
  'Server Configuration': '服务器配置',
  'Server Mode': '服务器模式',
  'Server Time': '服务器时间',
  'Service Account': '服务账号',
  'Service Account Email': '服务账号邮箱',
  'Service Account Token Creator': '服务账号令牌创建者',
  'Session Cookie': '会话 Cookie',
  'Session Exists': '会话存在',
  'Session User': '会话用户',
  'Sessions (24h)': '会话（24 小时）',
  'Sessions & Users': '会话和用户',
  'Set as default': '设为默认',
  'Set as Viewer': '设为查看者',
  'Set at creation time and cannot be changed.': '创建时设置，之后不可更改。',
  'Set up a new project workspace for your agents.': '为 Agent 设置新的项目工作区。',
  'Shared Directories': '共享目录',
  'Shared directories provide filesystem-level state sharing between agents in this project.':
    '共享目录为此项目中的 Agent 提供文件系统级状态共享。',
  'Shared workspace': '共享工作区',
  'Shell window': 'Shell 窗口',
  'show .dot files': '显示 .dot 文件',
  'Sign in to accept your invitation and join the hub.': '登录以接受邀请并加入 Hub。',
  'Sign in with GitHub': '使用 GitHub 登录',
  'Sign in with Google': '使用 Google 登录',
  'Single use': '单次使用',
  Size: '大小',
  'Skill Details': '技能详情',
  'Skill URI': '技能 URI',
  'SKILL.md Content': 'SKILL.md 内容',
  'Skills will be created under your user account.': '技能将创建在你的用户账号下。',
  Slug: 'Slug',
  'Soft Delete Retention': '软删除保留期',
  "Sorry, we couldn't find the page you're looking for. The path":
    '抱歉，我们找不到你要访问的页面。路径',
  'Specific Agent': '指定 Agent',
  'Sort:': '排序：',
  'SSE Connection': 'SSE 连接',
  'Standard 5-field cron: minute hour day month weekday (UTC)':
    '标准 5 段 cron：分 时 日 月 星期（UTC）',
  Start: '启动',
  Started: '已启动',
  'State User': '状态用户',
  Status: '状态',
  status: '状态',
  'Status:': '状态：',
  'Status: Created (not started)': '状态：已创建（未启动）',
  'Sort: created': '排序：已创建',
  'Sort: name': '排序：名称',
  'Sort: status': '排序：状态',
  'Sort: updated': '排序：已更新',
  Stop: '停止',
  'Stop All': '全部停止',
  Stopped: '已停止',
  Storage: '存储',
  'Store as Secret': '保存为密钥',
  Stream: '流式',
  Streaming: '流式传输中',
  Subjects: '主题',
  Subscribe: '订阅',
  'Subscribe to get notified about agent activity.': '订阅后可接收 Agent 活动通知。',
  'Subscribe to Notifications': '订阅通知',
  Summary: '摘要',
  Suspend: '挂起',
  Suspended: '已挂起',
  'Switch between agent and shell tmux windows': '在 Agent 和 Shell 的 tmux 窗口之间切换',
  Sync: '同步',
  'Sync Permissions': '同步权限',
  'System Prompt': '系统提示词',
  Tags: '标签',
  Target: '目标',
  'Target Agent': '目标 Agent',
  'Target Agent:': '目标 Agent：',
  'Target Path': '目标路径',
  Task: '任务',
  'Task / Prompt': '任务 / 提示词',
  'Task & Prompts': '任务和提示词',
  'Task for the agent (optional)': 'Agent 任务（可选）',
  'Task:': '任务：',
  Team: '团队',
  Telemetry: '遥测',
  'Template Files': '模板文件',
  'Template Hash': '模板哈希',
  'Template ID': '模板 ID',
  'Template used when creating agents without specifying one.':
    '未指定模板创建 Agent 时使用的模板。',
  'Template:': '模板：',
  Templates: '模板',
  'Terminal Unavailable': '终端不可用',
  'Terms of Service': '服务条款',
  Text: '文本',
  'The compute node that will run this agent.': '将运行此 Agent 的计算节点。',
  'The default branch to use for this repository.': '此仓库使用的默认分支。',
  'The GCP service account to assign to new agents by default. Only verified accounts are shown.':
    '默认分配给新 Agent 的 GCP 服务账号。这里只显示已验证账号。',
  'The Hub could not impersonate the service account': 'Hub 无法模拟此服务账号',
  'The Hub will automatically attempt to verify impersonation access after registration.':
    '注册后 Hub 会自动尝试验证模拟访问权限。',
  "The Hub's service account": 'Hub 的服务账号',
  'The initial task or prompt for the agent.': 'Agent 的初始任务或提示词。',
  'The numeric ID of your registered GitHub App': '已注册 GitHub App 的数字 ID',
  'The project workspace for this agent.': '此 Agent 使用的项目工作区。',
  'The public link where users can install this GitHub App on their org or account':
    '用户可用来把此 GitHub App 安装到组织或账号的公开链接',
  'The runtime profile on the selected broker.': '所选 Broker 上的运行时配置。',
  'The scheduler has not been initialized on this server.': '此服务器尚未初始化调度器。',
  'The task or prompt to start the agent with.': '用于启动 Agent 的任务或提示词。',
  'There are no groups configured in the system.': '系统中还没有配置用户组。',
  'There are no users registered in the system.': '系统中还没有注册用户。',
  'There was a problem connecting to the API.': '连接 API 时出现问题。',
  'There was a problem loading GCP service accounts.': '加载 GCP 服务账号时出现问题。',
  'There was a problem loading this agent.': '加载此 Agent 时出现问题。',
  'There was a problem loading this broker.': '加载此 Broker 时出现问题。',
  'There was a problem loading this group.': '加载此用户组时出现问题。',
  'There was a problem loading this project.': '加载此项目时出现问题。',
  'There was a problem loading this registry.': '加载此注册表时出现问题。',
  'There was a problem loading this skill.': '加载此技能时出现问题。',
  'There was a problem loading your access tokens.': '加载你的访问令牌时出现问题。',
  'There was a problem loading your environment variables.': '加载你的环境变量时出现问题。',
  'There was a problem loading your secrets.': '加载你的密钥时出现问题。',
  'This action cannot be undone.': '此操作无法撤销。',
  'This broker is not currently providing for any projects.': '此 Broker 当前未服务任何项目。',
  'This directory is already linked to another project.': '此目录已关联到另一个项目。',
  "This project doesn't have any agents yet.": '此项目还没有 Agent。',
  "This group doesn't have any members yet.": '此用户组还没有成员。',
  'This is a git repository. Agents will operate on the working tree.':
    '这是一个 git 仓库。Agent 将直接在工作树上操作。',
  'This link will not be shown again. Make sure to copy it before closing.':
    '此链接不会再次显示。关闭前请务必复制。',
  'This operation will restart the server. You will temporarily lose connectivity.':
    '此操作会重启服务器，你会暂时失去连接。',
  'This page requires a valid invite link. Please ask your hub administrator for an invite.':
    '此页面需要有效邀请链接。请向 Hub 管理员索取邀请。',
  'Tick Count': 'Tick 计数',
  'Tick Interval': 'Tick 间隔',
  'Time Remaining': '剩余时间',
  Timing: '时间',
  'Toggle maintenance mode': '切换维护模式',
  'Token Created': '令牌已创建',
  'Token Permissions': '令牌权限',
  'Token Usage': '令牌用量',
  'Tokens (24h)': 'Token（24 小时）',
  Tool: '工具',
  'Total Sessions': '总会话数',
  'Total Tokens': '总 Token 数',
  'Trigger Activities': '触发活动',
  Triggers: '触发器',
  Trust: '信任',
  'Trust Level': '信任级别',
  Trusted: '受信任',
  'Try Different Account': '换一个账号重试',
  Type: '类型',
  Unchecked: '未检查',
  'Unique Agents': '唯一 Agent',
  'Unix user inside container': '容器内 Unix 用户',
  Unlimited: '无限制',
  Unpin: '取消固定',
  'Unsaved changes': '未保存的更改',
  'Update & Restart': '更新并重启',
  'Update Now': '立即更新',
  'Update Server': '更新服务器',
  'Update value': '更新值',
  Updated: '已更新',
  updated: '已更新',
  'Upload & Publish': '上传并发布',
  'Upload a CSV file with one email per line. An optional second column can contain notes.':
    '上传 CSV 文件，每行一个邮箱；可选第二列填写备注。',
  'Upload Files': '上传文件',
  'Upload SKILL.md': '上传 SKILL.md',
  'Uploading & publishing...': '正在上传并发布...',
  URI: 'URI',
  URL: 'URL',
  'URL-safe identifier. Auto-derived from name if left unchanged.':
    'URL 安全标识符。若保持不变，将根据名称自动生成。',
  'Use broker default': '使用 Broker 默认值',
  'Use the "Publish Version" button to upload the first version.':
    '使用“发布版本”按钮上传第一个版本。',
  'User Access Mode': '用户访问模式',
  'User-defined recurring schedules across all projects.': '所有项目中的用户定义周期计划。',
  'Users page': '用户页面',
  Uses: '使用次数',
  'Validating path…': '正在验证路径…',
  Value: '值',
  'Verification Failed': '验证失败',
  'Verify installation': '验证安装',
  Version: '版本',
  Versions: '版本',
  'View agent': '查看 Agent',
  'View Details': '查看详情',
  Visibility: '可见性',
  Warn: '警告',
  'Webhook Secret': 'Webhook 密钥',
  Webhooks: 'Webhook',
  Welcome: '欢迎',
  'Welcome!': '欢迎！',
  'What does this skill do?': '这个技能做什么？',
  'When enabled, agents spawned by your agents (and their descendants) will also receive this secret.':
    '启用后，由你的 Agent 创建的 Agent（及其后代）也会收到此密钥。',
  'When invite-only mode is enabled, only emails on this list (and admin emails) can log in. Add a member and generate their invite link.':
    '启用仅邀请模式后，只有此列表中的邮箱（以及管理员邮箱）可以登录。添加成员并生成邀请链接。',
  'Workspace Mode': '工作区模式',
  'Workspace Path': '工作区路径',
  'Workspace search unavailable — showing local results only': '工作区搜索不可用，仅显示本地结果',
  'Workspace Type': '工作区类型',
  Workstation: '工作站',
  'Worktree per agent': '每个 Agent 单独 worktree',
  'Write Timeout': '写入超时',
  'You do not have permission to create skills.': '你没有创建技能的权限。',
  "You don't have permission to modify this project.": '你没有修改此项目的权限。',
  "You don't own any projects yet.": '你还没有拥有任何项目。',
  "You haven't created any agents yet.": '你还没有创建任何 Agent。',
  'You now have access to this hub.': '你现在可以访问此 Hub。',
  'You will be notified when this agent reaches: Completed, Waiting for Input, or Limits Exceeded.':
    '当此 Agent 达到“已完成”“等待输入”或“超出限制”状态时，你会收到通知。',
  "You've Been Invited": '你已收到邀请',
  'Your browser does not support notifications.': '你的浏览器不支持通知。',
  'Your email domain is not authorized to access this Scion instance.':
    '你的邮箱域名未被授权访问此 Scion 实例。',
  'Your GitHub App installation has been recorded. Set up projects for your repositories below.':
    '你的 GitHub App 安装已记录。请在下方为仓库设置项目。',
  'Your invite link has been created. Copy it now — it will not be shown again.':
    '你的邀请链接已创建。请立即复制，它不会再次显示。',
  'Your Notification Subscriptions': '你的通知订阅',
  'Your Notifications': '你的通知',
  'No Access Tokens': '没有访问令牌',
  'Create and manage personal access tokens for CI/CD pipelines and automation. Tokens are scoped to a specific project with limited permissions.':
    '创建并管理用于 CI/CD 流水线和自动化的个人访问令牌。令牌会限定到指定项目并使用有限权限。',
  'Create and manage personal access tokens for CI/CD pipelines and automation.':
    '创建并管理用于 CI/CD 流水线和自动化的个人访问令牌。',
  'Tokens are scoped to a specific project with limited permissions.':
    '令牌会限定到指定项目并使用有限权限。',
  'Create personal access tokens to authenticate CI/CD pipelines and automation tools with your projects.':
    '创建个人访问令牌，用于让 CI/CD 流水线和自动化工具访问你的项目。',
  'Create personal access tokens to authenticate CI/CD pipelines':
    '创建个人访问令牌，用于认证 CI/CD 流水线',
  'and automation tools with your projects.': '以及访问你项目的自动化工具。',
  '7 days': '7 天',
  '30 days': '30 天',
  '90 days': '90 天',
  '180 days': '180 天',
  '365 days (maximum)': '365 天（最长）',
  Progress: '进度',
  'Copy to clipboard': '复制到剪贴板',
  'Copied!': '已复制！',
  'Show Debug': '显示调试',
  Show: '显示',
  'State Summary [+]': '状态摘要 [+]',
  'State Summary [-]': '状态摘要 [-]',
  'Idle (no scope)': '空闲（无范围）',
  '{count} seconds ago': '{count} 秒前',
  '{count} minutes ago': '{count} 分钟前',
  '{count} hours ago': '{count} 小时前',
  '{count} days ago': '{count} 天前',
  'Save & Reload': '保存并重新加载',
  'Save Configuration': '保存配置',
  'Edit the global server settings (settings.yaml). Some changes take effect immediately; others require a server restart.':
    '编辑全局服务器设置（settings.yaml）。部分更改会立即生效，其他更改需要重启服务器。',
  'No description': '无描述',
  'No versions published yet': '尚未发布版本',
  'Publish Version': '发布版本',
  'Replacement URI': '替代 URI',
  'Runtime Environment': '运行时环境',
  'Limits & Resources': '限制和资源',
  'Limits & Usage': '限制和用量',
  'Reset authentication state for this agent': '重置此 Agent 的认证状态',
  'Loading agent configuration...': '正在加载 Agent 配置...',
  'Configure Agent:': '配置 Agent：',
  'Initial task or prompt': '初始任务或提示词',
  'Provider API key environment variable': '提供方 API Key 环境变量',
  'Runtime Profile': '运行时配置档',
  'Local Directory (linked)': '本地目录（已关联）',
  'Path / Remote': '路径 / 远程仓库',
  'New Project': '新建项目',
  'No Projects Found': '没有项目',
  'No Users Found': '没有用户',
  'Promote to Admin': '提升为管理员',
  'Invite Created': '邀请已创建',
  'Maintenance Mode': '维护模式',
  'Run Migration': '运行迁移',
  'Run Operation': '运行操作',
  'Scheduler Not Available': '调度器不可用',
  'Pinned Hashes': '已固定哈希',
  'Register Existing': '注册现有账号',
  'Mint Service Account': '签发服务账号',
  'Mint GCP Service Account': '签发 GCP 服务账号',
  Delete: '删除',
  Error: '错误',
  'GCP Identity': 'GCP 身份',
  Scope: '范围',
  Key: '键',
  'Mark read': '标记为已读',
  Private: '私有',
  Public: '公开',
  'Runtime Broker': '运行时 Broker',
  'Also delete stored files': '同时删除已存储文件',
  and: '和',
  'Describe what this agent should work on...': '描述这个 Agent 要处理的任务...',
  Duration: '持续时间',
  Expires: '过期时间',
  'Fire At': '触发时间',
  'GCP Project': 'GCP 项目',
  'GitHub App': 'GitHub App',
  'Harness Configs': 'Harness 配置',
  'Harness credential file': 'Harness 凭据文件',
  Hash: '哈希',
  'Hash:': '哈希：',
  Hub: 'Hub',
  'Initial Task': '初始任务',
  Inject: '注入',
  Labels: '标签',
  'Last Heartbeat': '最近心跳',
  'Link Account': '关联账号',
  'Linked project': '已关联项目',
  'Loading files...': '正在加载文件...',
  'Loading groups...': '正在加载用户组...',
  Local: '本地',
  'Mark all read': '全部标记为已读',
  'Max Duration': '最长时长',
  'Max Model Calls': '最大模型调用数',
  'Max Turns': '最大轮数',
  Messages: '消息',
  Mine: '我的',
  name: '名称',
  'New Agent': '新建 Agent',
  'No Authentication': '无认证',
  'No limit': '无限制',
  'No Members': '没有成员',
  'OAuth Token (env var)': 'OAuth 令牌（环境变量）',
  'Once set, this field cannot be cleared.': '设置后，此字段无法清空。',
  Pinned: '已固定',
  Production: '生产环境',
  Profiles: '配置档',
  'Provider API Key': '提供方 API Key',
  'Reset Auth': '重置认证',
  Resources: '资源',
  Revoke: '撤销',
  Running: '运行中',
  Runs: '运行次数',
  'Scheduled Events': '计划事件',
  Shared: '共享',
  'Vertex Model Garden': 'Vertex Model Garden',
  'A project with this ID already exists.': '已存在使用此 ID 的项目。',
  'Absolute path to a local directory. The directory is operated on in place.':
    '本地目录的绝对路径。该目录会在原位置被操作。',
  'Default opt-in state for new agents': '新 Agent 的默认选择状态',
  'Default resource requests and limits for new agents.': '新 Agent 的默认资源请求和限制。',
  'Default runtime profile for agents': 'Agent 默认运行时配置档',
  'defaults to agent name': '默认使用 Agent 名称',
  Degraded: '降级',
  'Delete Agent': '删除 Agent',
  'Delete harness config': '删除 Harness 配置',
  'Delete Project': '删除项目',
  'Demote to Member': '降级为成员',
  Deprecate: '弃用',
  'Deprecate Version': '弃用版本',
  'Deprecation Message': '弃用说明',
  'Description (optional)': '描述（可选）',
  Detail: '详情',
  'Developer Token': '开发者令牌',
  'Development Auth': '开发认证',
  'Discover from GitHub': '从 GitHub 发现',
  'Discover Installation': '发现安装',
  'Dispatch Agent': '调度 Agent',
  "doesn't exist.": '不存在。',
  'Domain Not Authorized': '域名未授权',
  'Domain Restricted (authorized domains only)': '域名受限（仅允许授权域名）',
  Done: '完成',
  'Download Zip': '下载 Zip',
  Downloads: '下载',
  Driver: '驱动',
  'Drop files here or click to browse': '将文件拖到这里，或点击浏览',
  'Dry run (preview changes without applying)': '试运行（预览更改但不应用）',
  'Edit Configuration': '编辑配置',
  'Edit Skill': '编辑技能',
  'Email address': '邮箱地址',
  'Empty directory': '空目录',
  'Enable Cloud Export': '启用云端导出',
  'Enable Dev Auth': '启用开发认证',
  'Enable Hub Reporting': '启用 Hub 上报',
  'Enable Local Output': '启用本地输出',
  'Enable Message Broker': '启用消息 Broker',
  'Enable Push Notifications': '启用推送通知',
  'Enable Runtime Broker': '启用运行时 Broker',
  'Enable Telemetry': '启用遥测',
  'Enable Telemetry Collection': '启用遥测采集',
  'Enable the toggle to request notification permission.': '开启开关以请求通知权限。',
  'Enable to receive installation lifecycle events from GitHub':
    '启用后接收来自 GitHub 的安装生命周期事件',
  'Encrypt and store securely. Value will never be readable after saving.':
    '加密并安全存储。保存后将无法读取原始值。',
  'Endpoint URL for agents to call back to the Hub': 'Agent 回调 Hub 的端点 URL',
  "Ensure the Hub's service account has the": '请确保 Hub 的服务账号拥有',
  'Ensure this repository is accessible via the': '请确保可通过以下方式访问此仓库：',
  Environment: '环境',
  'Event Handlers': '事件处理器',
  'Excluded Events': '排除事件',
  Expiration: '过期时间',
  Expired: '已过期',
  'Expires in': '过期时间',
  'Export Destinations': '导出目标',
  'Failed to Load Agent': 'Agent 加载失败',
  'Failed to Load Agents': 'Agent 列表加载失败',
  'Failed to Load Broker': 'Broker 加载失败',
  'Failed to Load Brokers': 'Broker 列表加载失败',
  'Failed to Load Events': '事件加载失败',
  'Failed to Load Group': '用户组加载失败',
  'Failed to Load Groups': '用户组列表加载失败',
  'Failed to Load Projects': '项目列表加载失败',
  'Failed to Load Registries': '注册表列表加载失败',
  'Failed to Load Registry': '注册表加载失败',
  'Failed to Load Scheduler': '调度器加载失败',
  'Failed to Load Settings': '设置加载失败',
  'Failed to Load Skill': '技能加载失败',
  'Failed to Load Users': '用户列表加载失败',
  File: '文件',
  Filters: '筛选器',
  Flags: '标记',
  'from SKILL.md': '来自 SKILL.md',
  'GCP IAM permission changes may take several minutes to propagate.':
    'GCP IAM 权限变更可能需要几分钟传播。',
  'GCP Secret Manager': 'GCP Secret Manager',
  'GCP Service Account': 'GCP 服务账号',
  'GCP Service Account Minting': 'GCP 服务账号签发',
  GCPProjectID: 'GCP 项目 ID',
  'General Settings': '通用设置',
  'Generate Invite': '生成邀请',
  'Generating invite for': '正在生成邀请：',
  'Get notified when agents complete, need input, or encounter issues.':
    '当 Agent 完成、需要输入或遇到问题时收到通知。',
  'Get Started': '开始使用',
  'Git branch for the agent': 'Agent 使用的 Git 分支',
  "Git branch for this agent's workspace.": '此 Agent 工作区使用的 Git 分支。',
  'Git Commit': 'Git 提交',
  'Git Remote URL': 'Git 远程 URL',
  'GitHub App Configuration': 'GitHub App 配置',
  'GitHub App installed': 'GitHub App 已安装',
  'GitHub App Integration': 'GitHub App 集成',
  'GitHub Projects': 'GitHub 项目',
  'Global agent templates. Open one to browse and edit its files.':
    '全局 Agent 模板。打开一个模板即可浏览和编辑其文件。',
  'Global Cap': '全局上限',
  'Global harness configurations. Open one to browse and edit its files.':
    '全局 Harness 配置。打开一个配置即可浏览和编辑其文件。',
  'Global Minted': '全局已签发',
  'Go Back': '返回',
  'Go duration string. Empty means no limit.': 'Go 时长字符串。留空表示无限制。',
  'Go to Home': '前往首页',
  'Go to Skill': '前往技能',
  'Google Cloud Storage': 'Google Cloud Storage',
  'Harness & Model': 'Harness 和模型',
  'Harness Authentication': 'Harness 认证',
  'Hashed Attributes': '哈希属性',
  Host: '主机',
  'HTTPS or SSH URL of the git repository.': 'Git 仓库的 HTTPS 或 SSH URL。',
  'Hub API endpoint for status reporting (when Hub not co-located)':
    '用于状态上报的 Hub API 端点（Hub 非同机部署时）',
  'Hub API Server': 'Hub API 服务器',
  'Hub API URL': 'Hub API URL',
  'Hub Endpoint': 'Hub 端点',
  'Hub Reporting': 'Hub 上报',
  'Hub Server': 'Hub 服务器',
  'Hub-managed Workspace': 'Hub 托管工作区',
  'Hub-scoped resources available to all projects and agents.':
    '所有项目和 Agent 都可使用的 Hub 级资源。',
  'ID:': 'ID：',
  Identity: '身份',
  'Image Registry': '镜像仓库',
  'Import CSV': '导入 CSV',
  'Import Emails from CSV': '从 CSV 导入邮箱',
  'in a project directory.': '在项目目录中。',
  'In duration': '按时长',
  'In-Process': '进程内',
  Inbox: '收件箱',
  'Clone {kind}': '克隆 {kind}',
  'Delete {kind}': '删除 {kind}',
  'Delete {path}': '删除 {path}',
  'Download {path}': '下载 {path}',
  'Edit {path}': '编辑 {path}',
  'Preview {path}': '预览 {path}',
  'Verified {time}': '已验证 {time}',
  'Are you sure you want to archive this skill?': '确定要归档此技能吗？',
  'Are you sure you want to delete this agent?': '确定要删除此 Agent 吗？',
  'Are you sure you want to delete this registry?': '确定要删除此注册表吗？',
  'Are you sure you want to stop all running agents?':
    '确定要停止所有正在运行的 Agent 吗？',
  'Capture failed (exit {exitCode}).\n\n{output}': '捕获失败（退出码 {exitCode}）。\n\n{output}',
  'Credentials captured successfully.\n\n{output}': '凭据捕获成功。\n\n{output}',
  'Current scheduler state and counters.': '当前调度器状态和计数器。',
  'Delete environment variable "{key}"? This cannot be undone.':
    '确定要删除环境变量“{key}”吗？此操作无法撤销。',
  'Delete secret "{key}"? This cannot be undone.':
    '确定要删除密钥“{key}”吗？此操作无法撤销。',
  'Delete service account "{email}"? This cannot be undone.':
    '确定要删除服务账号“{email}”吗？此操作无法撤销。',
  'Discard unsaved changes?': '要放弃未保存的更改吗？',
  'Failed to archive skill': '归档技能失败',
  'Failed to capture auth': '捕获认证失败',
  'Failed to delete': '删除失败',
  'Failed to delete agent': '删除 Agent 失败',
  'Failed to delete project': '删除项目失败',
  'Failed to deprecate version': '弃用版本失败',
  'Failed to pin': '固定失败',
  'Failed to remove member': '移除成员失败',
  'Failed to reset auth': '重置认证失败',
  'Failed to reset auth for all agents': '重置所有 Agent 的认证失败',
  'Failed to revoke': '撤销失败',
  'Failed to run capture auth': '运行认证捕获失败',
  'Failed to save': '保存失败',
  'Failed to save skill': '保存技能失败',
  'Failed to set default broker': '设置默认 Broker 失败',
  'Failed to stop agents': '停止 Agent 失败',
  'Failed to stop all agents': '停止所有 Agent 失败',
  'Failed to toggle status': '切换状态失败',
  'Failed to unpin': '取消固定失败',
  'Failed to {action} agent': 'Agent 操作失败：{action}',
  'Inject a fresh auth token into every running agent without restarting them.':
    '向每个正在运行的 Agent 注入新的认证令牌，无需重启。',
  'Input Tokens by Model': '按模型统计输入 Token',
  'Loading maintenance operations...': '正在加载维护操作...',
  'Loading scheduler status...': '正在加载调度器状态...',
  'Metrics are collected from agent telemetry via Google Cloud Monitoring.':
    '指标通过 Google Cloud Monitoring 从 Agent 遥测数据中采集。',
  'No credentials found yet.\n\nAuthenticate first (e.g., run \'agy\' inside the container), then try again.\n\n{output}':
    '尚未找到凭据。\n\n请先完成认证（例如在容器内运行 \'agy\'），然后重试。\n\n{output}',
  'No installations found. Click "Discover from GitHub" to sync.':
    '未找到安装。点击“从 GitHub 发现”进行同步。',
  'No log output captured.': '未捕获日志输出。',
  'No metrics data available for this period.': '此时间段没有可用指标数据。',
  'No recurring handlers registered.': '未注册周期处理器。',
  'One-off administrative actions across all agents.':
    '面向所有 Agent 的一次性管理操作。',
  'Output Tokens by Model': '按模型统计输出 Token',
  'Periodic tasks driven by the root ticker. All handlers run on startup (tick 0), then at their configured interval.':
    '由根 ticker 驱动的周期任务。所有处理器会在启动时（tick 0）运行，之后按配置间隔运行。',
  'Public Installation URL': '公开安装 URL',
  'Rate Limit': '速率限制',
  'Recurring Handlers': '周期处理器',
  'Recurring Tasks': '周期任务',
  'Remove broker "{name}" from this project?':
    '确定要从此项目移除 Broker“{name}”吗？',
  'Remove {memberType} "{displayName}" from this group?':
    '确定要从此用户组移除 {memberType}“{displayName}”吗？',
  'Reset Auth — All Running Agents': '重置认证 — 所有正在运行的 Agent',
  'Revoke token "{name}"? It will no longer be usable for authentication.':
    '确定要撤销令牌“{name}”吗？它将无法继续用于认证。',
  'Run Detail': '运行详情',
  'Running...': '正在运行...',
  'Scion Server Version': 'Scion 服务器版本',
  'Server is up to date': '服务器已是最新版本',
  Sessions: '会话',
  'Stopped {stopped} agents, {failed} failed.':
    '已停止 {stopped} 个 Agent，{failed} 个失败。',
  'The token refresh loop in each agent will pick up the new credentials automatically.':
    '每个 Agent 中的令牌刷新循环会自动获取新的凭据。',
  'Unpin "{uri}"?': '确定取消固定“{uri}”吗？',
  'Update available': '有可用更新',
  'You have unsaved changes. Close anyway?': '有未保存的更改。仍要关闭吗？',
  'new commit': '个新提交',
  'new commits': '个新提交',
  error: '错误',
};

let currentLocale: Locale = detectInitialLocale();

export function getLocale(): Locale {
  return currentLocale;
}

export function getLocaleLabel(locale = currentLocale): string {
  return locale === 'zh-CN' ? '中文' : 'EN';
}

export function getIntlLocale(): string {
  return currentLocale;
}

export function setLocale(locale: Locale): void {
  if (!isSupportedLocale(locale) || locale === currentLocale) return;
  currentLocale = locale;
  try {
    window.localStorage.setItem(LOCALE_STORAGE_KEY, locale);
  } catch {
    // localStorage can be unavailable in restricted browser contexts.
  }
  applyDocumentLocale(locale);
  window.dispatchEvent(new CustomEvent(LOCALE_CHANGE_EVENT, { detail: { locale } }));
}

export function toggleLocale(): void {
  setLocale(currentLocale === 'zh-CN' ? 'en' : 'zh-CN');
}

export function t(source: string, values: TranslationValues = {}): string {
  const translated = currentLocale === 'zh-CN' ? ZH_CN[source] || source : source;
  return translated.replace(/\{(\w+)\}/g, (match, key: string) => {
    const value = values[key];
    return value === undefined ? match : String(value);
  });
}

export class LocaleController implements ReactiveController {
  constructor(private host: ReactiveControllerHost) {
    host.addController(this);
  }

  get locale(): Locale {
    return getLocale();
  }

  t(source: string, values?: TranslationValues): string {
    return t(source, values);
  }

  hostConnected(): void {
    window.addEventListener(LOCALE_CHANGE_EVENT, this.handleLocaleChange as EventListener);
  }

  hostDisconnected(): void {
    window.removeEventListener(LOCALE_CHANGE_EVENT, this.handleLocaleChange as EventListener);
  }

  private handleLocaleChange = (): void => {
    this.host.requestUpdate();
  };
}

function detectInitialLocale(): Locale {
  if (typeof window === 'undefined') return 'en';
  try {
    const stored = window.localStorage.getItem(LOCALE_STORAGE_KEY);
    if (isSupportedLocale(stored)) return stored;
  } catch {
    // Ignore storage access errors.
  }

  const browserLocale = window.navigator.language || '';
  return browserLocale.toLowerCase().startsWith('zh') ? 'zh-CN' : 'en';
}

function isSupportedLocale(value: unknown): value is Locale {
  return typeof value === 'string' && SUPPORTED_LOCALES.includes(value as Locale);
}

function applyDocumentLocale(locale: Locale): void {
  if (typeof document === 'undefined') return;
  document.documentElement.lang = locale;
}

const TRANSLATABLE_ATTRIBUTES = [
  'aria-label',
  'content',
  'help-text',
  'label',
  'placeholder',
  'summary',
  'title',
] as const;

const SKIP_TEXT_TAGS = new Set(['CODE', 'PRE', 'SCRIPT', 'STYLE', 'TEXTAREA']);

const observedRoots = new Set<TranslatableRoot>();
const rootObservers = new WeakMap<TranslatableRoot, MutationObserver>();
const originalTextValues = new WeakMap<Text, string>();
const originalAttributeValues = new WeakMap<Element, Map<string, string>>();

let translationPatterns: TranslationPattern[] | null = null;
let autoTranslatorInstalled = false;
let translateScheduled = false;

export function installAutoTranslator(): void {
  if (autoTranslatorInstalled || typeof window === 'undefined' || typeof document === 'undefined') {
    return;
  }
  autoTranslatorInstalled = true;

  patchShadowRootCreation();
  patchDialogFunctions();
  observeRoot(document);
  scanShadowRoots(document);
  window.addEventListener(LOCALE_CHANGE_EVENT, scheduleTranslateAll);
  scheduleTranslateAll();
}

function patchShadowRootCreation(): void {
  const originalAttachShadow = HTMLElement.prototype.attachShadow;
  HTMLElement.prototype.attachShadow = function attachShadow(
    this: HTMLElement,
    init: ShadowRootInit
  ): ShadowRoot {
    const root = originalAttachShadow.call(this, init);
    observeRoot(root);
    return root;
  };
}

function patchDialogFunctions(): void {
  const originalAlert = window.alert.bind(window);
  const originalConfirm = window.confirm.bind(window);
  const originalPrompt = window.prompt.bind(window);

  window.alert = (message?: unknown): void => {
    originalAlert(translateDialogMessage(message) as string | undefined);
  };
  window.confirm = (message?: string): boolean => {
    return originalConfirm(translateDialogMessage(message) as string | undefined);
  };
  window.prompt = (message?: string, defaultValue?: string): string | null => {
    return originalPrompt(translateDialogMessage(message) as string | undefined, defaultValue);
  };
}

function translateDialogMessage(message: unknown): unknown {
  if (currentLocale !== 'zh-CN' || typeof message !== 'string') return message;
  return translateStaticText(message);
}

function observeRoot(root: TranslatableRoot): void {
  if (rootObservers.has(root)) return;

  const observer = new MutationObserver((mutations) => {
    for (const mutation of mutations) {
      if (mutation.type === 'characterData' && mutation.target instanceof Text) {
        translateTextNode(mutation.target);
        continue;
      }

      if (mutation.type === 'attributes' && mutation.target instanceof Element) {
        const attr = mutation.attributeName;
        if (
          attr &&
          TRANSLATABLE_ATTRIBUTES.includes(attr as (typeof TRANSLATABLE_ATTRIBUTES)[number])
        ) {
          translateAttribute(mutation.target, attr);
        }
        continue;
      }

      for (const node of mutation.addedNodes) {
        translateNode(node);
        scanShadowRoots(node);
      }
    }
  });

  observer.observe(root, {
    attributes: true,
    attributeFilter: [...TRANSLATABLE_ATTRIBUTES],
    characterData: true,
    childList: true,
    subtree: true,
  });
  rootObservers.set(root, observer);
  observedRoots.add(root);
  translateNode(root);
}

function scheduleTranslateAll(): void {
  if (translateScheduled) return;
  translateScheduled = true;
  const run = (): void => {
    translateScheduled = false;
    for (const root of observedRoots) {
      translateNode(root);
      scanShadowRoots(root);
    }
  };
  if (typeof window.requestAnimationFrame === 'function') {
    window.requestAnimationFrame(run);
  } else {
    window.setTimeout(run, 0);
  }
}

function scanShadowRoots(root: Node): void {
  if (root instanceof Element && root.shadowRoot) {
    observeRoot(root.shadowRoot);
  }
  const queryRoot =
    root instanceof Document || root instanceof ShadowRoot || root instanceof Element;
  if (!queryRoot) return;

  root.querySelectorAll('*').forEach((element) => {
    if (element.shadowRoot) {
      observeRoot(element.shadowRoot);
    }
  });
}

function translateNode(node: Node): void {
  if (node instanceof Text) {
    translateTextNode(node);
    return;
  }

  if (node instanceof Element) {
    translateElement(node);
  }

  const queryRoot =
    node instanceof Document || node instanceof ShadowRoot || node instanceof Element;
  if (!queryRoot) return;

  const walker = document.createTreeWalker(node, NodeFilter.SHOW_TEXT | NodeFilter.SHOW_ELEMENT);
  let current: Node | null = walker.currentNode;
  while (current) {
    if (current instanceof Text) {
      translateTextNode(current);
    } else if (current instanceof Element) {
      translateElement(current);
    }
    current = walker.nextNode();
  }
}

function translateElement(element: Element): void {
  for (const attr of TRANSLATABLE_ATTRIBUTES) {
    translateAttribute(element, attr);
  }
}

function translateTextNode(node: Text): void {
  if (shouldSkipTextNode(node)) return;

  const current = node.data;
  const stored = originalTextValues.get(node);
  if (currentLocale === 'zh-CN') {
    const storedTranslation = stored ? translateStaticText(stored) : undefined;
    const source =
      stored && (current === stored || current === storedTranslation) ? stored : current;
    const translated = translateStaticText(source);
    if (translated !== source) {
      originalTextValues.set(node, source);
      if (current !== translated) {
        node.data = translated;
      }
    }
    return;
  }

  if (stored !== undefined && current !== stored) {
    node.data = stored;
  }
  if (stored !== undefined) {
    originalTextValues.delete(node);
  }
}

function translateAttribute(element: Element, attr: string): void {
  const current = element.getAttribute(attr);
  if (!current) return;

  let originals = originalAttributeValues.get(element);
  const stored = originals?.get(attr);
  if (currentLocale === 'zh-CN') {
    const storedTranslation = stored ? translateStaticText(stored) : undefined;
    const source =
      stored && (current === stored || current === storedTranslation) ? stored : current;
    const translated = translateStaticText(source);
    if (translated !== source) {
      if (!originals) {
        originals = new Map();
        originalAttributeValues.set(element, originals);
      }
      originals.set(attr, source);
      if (current !== translated) {
        element.setAttribute(attr, translated);
      }
    }
    return;
  }

  if (stored !== undefined) {
    if (current !== stored) {
      element.setAttribute(attr, stored);
    }
    originals?.delete(attr);
  }
}

function shouldSkipTextNode(node: Text): boolean {
  const parent = node.parentElement;
  if (!parent) return true;
  if (SKIP_TEXT_TAGS.has(parent.tagName)) return true;
  return parent.closest('code, pre, script, style, textarea') !== null;
}

function translateStaticText(source: string): string {
  const leading = source.match(/^\s*/)?.[0] || '';
  const trailing = source.match(/\s*$/)?.[0] || '';
  const text = source.trim();
  if (!text || !/[A-Za-z]/.test(text)) return source;

  const normalized = text.replace(/\s+/g, ' ');
  const direct = ZH_CN[text] || ZH_CN[normalized];
  const translated = direct || translatePattern(text) || translatePattern(normalized);
  if (!translated || translated === text) return source;
  const adjustedLeading =
    /^[ \t]+$/.test(leading) && startsWithCjkOrPunctuation(translated) ? '' : leading;
  const adjustedTrailing =
    /^[ \t]+$/.test(trailing) && endsWithCjkOrPunctuation(translated) ? '' : trailing;
  return `${adjustedLeading}${translated}${adjustedTrailing}`;
}

function startsWithCjkOrPunctuation(text: string): boolean {
  return /^[\u4e00-\u9fff，。！？、；：]/.test(text);
}

function endsWithCjkOrPunctuation(text: string): boolean {
  return /[\u4e00-\u9fff，。！？、；：]$/.test(text);
}

function translatePattern(text: string): string | undefined {
  for (const pattern of getTranslationPatterns()) {
    const match = pattern.regex.exec(text);
    if (!match) continue;

    const values: TranslationValues = {};
    pattern.names.forEach((name, index) => {
      values[name] = match[index + 1];
    });
    return pattern.translated.replace(/\{(\w+)\}/g, (placeholder, name: string) => {
      const value = values[name];
      return value === undefined ? placeholder : String(value);
    });
  }
  return undefined;
}

function getTranslationPatterns(): TranslationPattern[] {
  if (translationPatterns) return translationPatterns;
  translationPatterns = Object.entries(ZH_CN)
    .filter(([source]) => source.includes('{'))
    .map(([source, translated]) => {
      const names: string[] = [];
      const regex = new RegExp(
        `^${escapeRegex(source).replace(/\\\{(\w+)\\\}/g, (_match, name: string) => {
          names.push(name);
          return '([\\s\\S]+?)';
        })}$`
      );
      return { regex, names, translated };
    });
  return translationPatterns;
}

function escapeRegex(source: string): string {
  return source.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
}

applyDocumentLocale(currentLocale);
