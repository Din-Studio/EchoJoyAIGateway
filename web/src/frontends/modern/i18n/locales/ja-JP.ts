import { protocolMessages as protocols } from '../protocols'
import { jaJP as inspector } from './inspector'
import { jaJP as home } from './home'
import { jaJP as accessKeys } from './access-keys'
import { jaJP as teams } from './teams'
import { jaJP as logs } from './logs'
import { jaJP as usage } from './usage'
import { jaJP as health } from './health'
import { jaJP as credentialCards } from './credential-cards'
import { jaJP as groupWorkflows } from './group-workflows'
import { jaJP as parameterRules } from './parameter-rules'
import { jaJP as groupDetail } from './group-detail'
import { jaJP as groupMessages } from './groups'
import { jaJP as ui } from './ui'
import { jaJP as groupCreate } from './group-create'
import { jaJP as modelSelection } from './model-selection'
import { jaJP as modelManager } from './model-manager'
import { jaJP as settingsForm } from './settings-form'
import { jaJP as autoModel } from './auto-model'
import { jaJP as subscriptions } from './subscriptions'

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
    workspace: 'ワークスペース',
    observe: '運用状況',
    system: 'システム',
  },
  shell: {
    refresh: '更新',
    refreshedAt: '{time} 更新',
    importCredentials: 'キーをインポート',
    goHome: 'GPT-Load の概要',
    mascotHint:
      'マスコットを長押し、またはフォーカス中にスペースで遊べます。Enter で概要へ戻ります。',
    collapseSidebar: 'サイドバーを折りたたむ',
    expandSidebar: 'サイドバーを展開',
    close: '閉じる',
    documentation: '利用ガイド',
    sponsor: 'スポンサー',
    mobileNavigationDescription: '管理ワークスペースや運用状況のページへ移動します。',
    navigationFailed: 'ページを読み込めません。再読み込みしてください。',
    reload: '再読み込み',
  },
  auth: {
    title: 'GPT-Load にログイン',
    description: 'EchoJoy アカウントでログインします。権限は所属チームとロールで決まります。',
    submit: 'EchoJoy アカウントでログイン',
    submitting: 'リダイレクト中…',
    noAccess:
      'このアカウントにはゲートウェイへのアクセス権がありません。管理者にチームへの追加またはプラットフォーム権限の付与を依頼してください。',
    loginFailed: 'ログインに失敗しました。再試行してください。',
    network: '管理 API に接続できません。サービスを確認して再試行してください。',
    invalidResponse: '管理 API から認識できない応答が返されました。後でもう一度お試しください。',
    restoreTitle: 'ログイン状態を確認',
    checking: '現在のセッションを確認中…',
    retry: '再確認',
    signIn: '再ログイン',
    logout: 'ログアウト',
    logoutFailed: 'ログアウトに失敗しました。再試行してください。',
    platformAdmin: 'プラットフォーム管理者',
    teamAdmin: 'チーム管理者',
    member: 'メンバー',
    teams: '所属チーム',
    noTeams: 'チーム未所属',
  },
  pages: {
    home: { title: '概要' },
    groups: { title: 'グループ' },
    groupDetail: { title: 'グループの詳細' },
    models: { title: 'モデル' },
    accessKeys: { title: 'アクセスキー' },
    teams: { title: 'チーム' },
    usage: { title: '使用量' },
    logs: { title: 'リクエストログ' },
    health: { title: '稼働状況' },
    inspector: { title: 'ルート検査' },
    settings: { title: 'グローバル設定' },
    import: { title: '認証情報のインポート' },
    login: { title: 'ログイン' },
  },
  notFound: {
    title: 'ページが見つかりません',
    description: 'ページのアドレスを確認するか、概要に戻ってください。',
    backHome: '概要に戻る',
  },
  appearance: {
    theme: '表示モード',
    language: '表示言語',
    themes: {
      system: 'システム',
      light: 'ライト',
      dark: 'ダーク',
    },
    persistenceFailed: 'ブラウザに保存できません。設定は今回のアクセス中のみ有効です。',
  },
  system: {
    currentVersion: '現在のバージョン：{version}',
    loadingVersion: '読み込み中…',
    versionUnavailable: 'バージョン不明',
    checkUpdate: '更新を確認',
    checking: '確認中…',
    latestVersion: '最新バージョンです',
    updateAvailable: '新バージョン {version}',
    checkFailed: '確認に失敗しました。再試行してください。',
    authRequired: '管理者ログインが必要です',
  },
  navigation: 'メインナビゲーション',
  skipToContent: 'メインコンテンツへ移動',
}
