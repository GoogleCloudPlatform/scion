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

import type { MessageCatalog } from './en.js';

export const zhCN = {
  'nav.overview': '概览',
  'nav.management': '管理',
  'nav.admin': '系统管理',
  'nav.tagline': '智能体编排',
  'nav.expand': '展开侧边栏',
  'nav.collapse': '收起侧边栏',
  'nav.collapseShort': '收起',
  'page.dashboard': '仪表盘',
  'page.projects': '项目',
  'page.agents': '智能体',
  'page.terminals': '终端',
  'page.brokers': '运行代理',
  'page.settings': '设置',
  'page.hubResources': 'Hub 资源',
  'page.scheduler': '调度器',
  'page.users': '用户',
  'page.groups': '用户组',
  'page.serverConfig': '服务器配置',
  'page.federation': '联合身份',
  'page.integrations': '集成',
  'page.roles': '角色',
  'page.roleBindings': '角色绑定',
  'page.accessConstraints': '访问约束',
  'page.quotas': '配额',
  'page.health': '健康状态',
  'page.metrics': '指标',
  'page.skillRegistries': '技能注册中心',
  'page.skills': '技能',
  'page.githubSetup': 'GitHub App 设置',
  'page.setup': '初始设置',
  'page.createProject': '创建项目',
  'page.projectSettings': '项目设置',
  'page.schedules': '计划任务',
  'page.projectMetrics': '项目指标',
  'page.artifact': '产物',
  'page.artifacts': '产物',
  'page.template': '模板',
  'page.project': '项目',
  'page.createAgent': '创建智能体',
  'page.terminal': '终端',
  'page.editAgent': '编辑智能体',
  'page.agent': '智能体',
  'page.broker': '运行代理',
  'page.harnessConfig': 'Harness 配置',
  'page.group': '用户组',
  'page.maintenance': '维护',
  'page.integration': '集成',
  'page.createSkill': '创建技能',
  'page.skill': '技能',
  'page.skillRegistry': '技能注册中心',
  'page.notFound': '页面不存在',
  'page.diagnostics': '诊断',
  'breadcrumb.home': '首页',
  'header.navigation': '打开导航菜单',
  'header.notifications': '通知',
  'header.help': '帮助',
  'header.toggleTheme': '切换深色模式',
  'header.profile': '个人资料',
  'header.signOut': '退出登录',
  'header.signOutMenu': '退出登录',
  'header.signIn': '登录',
  'header.switchView': '切换视图',
  'header.switchMode': '切换视图模式',
  'header.switchModeUnread': '切换视图模式，{{unread}}',
  'header.terminalsCount': '终端（{{count}}）',
  'header.chat': '聊天',
  'header.chatUnread': '聊天，{{unread}}',
  'header.unread_other': '{{count}} 个未读会话',
  'header.account': '账户菜单',
  'header.lightMode': '浅色模式',
  'header.darkMode': '深色模式',
  'header.jumpToAgent': '跳转到智能体（{{shortcut}}）',
  'header.openJumpToAgent': '打开智能体跳转',
  'language.label': '语言',
  'language.auto': '跟随系统',
} satisfies MessageCatalog;
