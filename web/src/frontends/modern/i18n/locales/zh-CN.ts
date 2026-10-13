import { protocolMessages as protocols } from '../protocols'
import { zhCN as inspector } from './inspector'
import { zhCN as home } from './home'
import { zhCN as accessKeys } from './access-keys'
import { zhCN as teams } from './teams'
import { zhCN as logs } from './logs'
import { zhCN as usage } from './usage'
import { zhCN as health } from './health'
import { zhCN as credentialCards } from './credential-cards'
import { zhCN as groupWorkflows } from './group-workflows'
import { zhCN as parameterRules } from './parameter-rules'
import { zhCN as groupDetail } from './group-detail'
import { zhCN as groupMessages } from './groups'
import { zhCN as ui } from './ui'
import { zhCN as groupCreate } from './group-create'
import { zhCN as modelSelection } from './model-selection'
import { zhCN as modelManager } from './model-manager'
import { zhCN as settingsForm } from './settings-form'
import { zhCN as autoModel } from './auto-model'
import { zhCN as subscriptions } from './subscriptions'

export default {
  home,
  inspector,
  protocols,
  logs,
  usage,
  health,
  accessKeys,
  teams,
  groupWorkflows,
  parameterRules,
  credentialCards,
  groupDetail,
  subscriptions,
  modelSelection,
  modelManager,
  settingsForm,
  autoModel,
  groupCreate,
  ui,
  ...groupMessages,
  sections: {
    workspace: '工作区',
    observe: '运行观测',
    system: '系统',
  },
  shell: {
    refresh: '刷新',
    refreshedAt: '更新于 {time}',
    importCredentials: '导入密钥',
    goHome: 'GPT-Load 总览',
    mascotHint: '长按吉祥物或聚焦后按空格与它互动，按回车返回总览。',
    collapseSidebar: '收起侧栏',
    expandSidebar: '展开侧栏',
    close: '关闭',
    documentation: '使用文档',
    sponsor: '赞助支持',
    mobileNavigationDescription: '直接进入管理工作区和运行观测页面。',
    navigationFailed: '无法加载页面，请重新加载。',
    reload: '重新加载',
  },
  auth: {
    title: '登录 GPT-Load',
    description: '使用 EchoJoy 账号登录，权限由所在团队和角色决定。',
    submit: '使用 EchoJoy 账号登录',
    submitting: '正在跳转…',
    noAccess: '该账号没有网关访问权限。请联系管理员把你加入团队或授予平台权限。',
    loginFailed: '登录失败，请重试。',
    network: '无法连接到管理 API，请检查服务后重试。',
    invalidResponse: '管理 API 返回了无法识别的响应，请稍后重试。',
    restoreTitle: '验证登录状态',
    checking: '正在验证当前会话…',
    retry: '重新验证',
    signIn: '重新登录',
    logout: '退出登录',
    logoutFailed: '退出登录失败，请重试。',
    platformAdmin: '平台管理员',
    teamAdmin: '团队管理员',
    member: '成员',
    teams: '所属团队',
    noTeams: '未加入团队',
  },
  pages: {
    home: { title: '总览' },
    groups: { title: '分组' },
    groupDetail: { title: '分组详情' },
    models: { title: '模型' },
    accessKeys: { title: '访问密钥' },
    teams: { title: '团队' },
    usage: { title: '用量统计' },
    logs: { title: '请求日志' },
    health: { title: '运行健康' },
    inspector: { title: '路由检查' },
    settings: { title: '全局设置' },
    import: { title: '导入凭据' },
    login: { title: '登录' },
  },
  notFound: {
    title: '页面不存在',
    description: '请检查页面地址，或返回总览。',
    backHome: '返回总览',
  },
  appearance: {
    theme: '显示模式',
    language: '界面语言',
    themes: {
      system: '跟随系统',
      light: '浅色',
      dark: '深色',
    },
    persistenceFailed: '浏览器未允许保存偏好，本次选择在当前访问中有效。',
  },
  system: {
    currentVersion: '当前版本：{version}',
    loadingVersion: '加载中…',
    versionUnavailable: '版本未知',
    checkUpdate: '检查更新',
    checking: '检查中…',
    latestVersion: '已是最新版本',
    updateAvailable: '新版本 {version}',
    checkFailed: '检查失败，请重试',
    authRequired: '需要管理员登录',
  },
  navigation: '主导航',
  skipToContent: '跳到主要内容',
}
