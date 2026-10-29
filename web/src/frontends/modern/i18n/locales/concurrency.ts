export const zhCN = {
  label: '并发上限',
  value: '并发 {current} / {limit}',
  valueUnlimited: '并发不限',
  inherit: '继承默认值',
  unlimited: '不限',
  overrideHelp: '留空继承默认值；0 表示不限。按整个集群计数，满额立即拒绝。',
}

export const enUS = {
  label: 'Concurrency limit',
  value: 'Concurrency {current} / {limit}',
  valueUnlimited: 'Concurrency unlimited',
  inherit: 'Inherit default',
  unlimited: 'Unlimited',
  overrideHelp:
    'Leave blank to inherit; 0 means unlimited. Counted across the whole cluster; reject immediately when full.',
}

export const jaJP = {
  label: '同時実行数の上限',
  value: '同時実行 {current} / {limit}',
  valueUnlimited: '同時実行は無制限',
  inherit: 'デフォルトを継承',
  unlimited: '無制限',
  overrideHelp:
    '空欄でデフォルトを継承、0 で無制限。クラスター全体で集計し、上限到達時は即座に拒否します。',
}
