export const zhCN = {
  label: '并发上限',
  limitTag: '并发 {limit}',
  accessKeyLimitHelp: '该访问密钥在整个集群同时最多处理 {limit} 个请求，跨分组合计；满额立即拒绝。',
  inherit: '继承默认值',
  unlimited: '不限',
  overrideHelp: '留空继承默认值；0 表示不限。按整个集群计数，满额立即拒绝。',
}

export const enUS = {
  label: 'Concurrency limit',
  limitTag: 'Concurrency {limit}',
  accessKeyLimitHelp:
    'At most {limit} active requests for this access key across all groups and replicas; reject immediately when full.',
  inherit: 'Inherit default',
  unlimited: 'Unlimited',
  overrideHelp:
    'Leave blank to inherit; 0 means unlimited. Counted across the whole cluster; reject immediately when full.',
}

export const jaJP = {
  label: '同時実行数の上限',
  limitTag: '同時実行 {limit}',
  accessKeyLimitHelp:
    'このアクセスキーはクラスター全体で、グループをまたいで最大 {limit} 件を同時実行できます。上限到達時は即座に拒否します。',
  inherit: 'デフォルトを継承',
  unlimited: '無制限',
  overrideHelp:
    '空欄でデフォルトを継承、0 で無制限。クラスター全体で集計し、上限到達時は即座に拒否します。',
}
