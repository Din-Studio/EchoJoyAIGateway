export const zhCN = {
  help: '团队与成员在 EchoJoy（Casdoor）中维护。这里为每个团队授权可用分组，团队管理员只能在授权范围内为成员创建密钥。',
  groups: '授权分组',
  selectGroups: '选择授权给该团队的分组',
  save: '保存授权',
  saved: '已更新「{name}」的授权分组',
  failed: '保存授权失败，请重试。',
  loadFailed: '无法读取团队列表',
  groupsFailed: '无法读取分组目录',
  empty: '还没有团队',
  emptyHelp: '在 Casdoor 的组织中创建分组后，它会作为团队出现在这里。',
  noGrants: '尚未授权分组：该团队的密钥在授权前不可用。',
}

export const enUS: typeof zhCN = {
  help: 'Teams and members live in EchoJoy (Casdoor). Grant each team the groups it may use; team admins can only create keys within those grants.',
  groups: 'Granted groups',
  selectGroups: 'Select groups granted to this team',
  save: 'Save grants',
  saved: 'Updated the granted groups of “{name}”',
  failed: 'Could not save the grants. Try again.',
  loadFailed: 'Unable to load teams',
  groupsFailed: 'Unable to load the group catalog',
  empty: 'No teams yet',
  emptyHelp: 'Groups created in the Casdoor organization appear here as teams.',
  noGrants: 'No groups granted: this team’s keys cannot route until you grant some.',
}

export const jaJP: typeof zhCN = {
  help: 'チームとメンバーは EchoJoy（Casdoor）で管理します。ここで各チームに利用できるグループを許可し、チーム管理者は許可範囲内でのみメンバーのキーを作成できます。',
  groups: '許可グループ',
  selectGroups: 'このチームに許可するグループを選択',
  save: '許可を保存',
  saved: '「{name}」の許可グループを更新しました',
  failed: '許可を保存できませんでした。再試行してください。',
  loadFailed: 'チームを読み込めません',
  groupsFailed: 'グループ一覧を読み込めません',
  empty: 'チームはまだありません',
  emptyHelp: 'Casdoor の組織で作成したグループがチームとしてここに表示されます。',
  noGrants: 'グループ未許可：許可されるまでこのチームのキーは利用できません。',
}
