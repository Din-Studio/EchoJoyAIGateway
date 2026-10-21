#!/usr/bin/env bash
# 扫描本次合并将落到 dev 上的改动（暂存区 vs HEAD），列出可能违反 dev 架构的位置。
# 命中只是线索，需逐条对照 ARCHITECTURE.md 判定；无命中也不代表适配完成。
set -euo pipefail

diff_range=(--cached HEAD)

section() { printf '\n== %s\n' "$1"; }

section "新增文件落在 dev 已删除的区域（应删除，或改写到 dev 对应位置）"
git diff "${diff_range[@]}" --name-only --diff-filter=A |
  grep -E 'frontends/classic|web/src/shared/frontend|modern_page_routes\.json|sqlite|mysql|securefile|authkey|_checkpoint\.go|migration_recovery|windows|installer|render' || echo "(无)"

section "仍处于冲突状态的文件"
git diff --name-only --diff-filter=U || true

section "新增迁移文件（核对编号连续、标识符已改名、台账已记录）"
git diff "${diff_range[@]}" --name-status -- internal/storage/migrations | grep -v '_test\.go' || echo "(无)"

section "新增行中的可疑模式"
# 格式：模式<TAB>范围（all = 含测试，prod = 仅生产代码）<TAB>对应 ARCHITECTURE.md 章节
patterns=$(cat <<'PATTERNS'
Dialector\.Name|sqlite|mysql|longtext|#building|ValidateRecoverable	all	§5 方言/迁移
sqlitetest|:memory:|t\.Skip\(	all	§11 测试
DATA_DIR|DataDir|os\.(WriteFile|Create|MkdirAll|OpenFile)|ioutil\.WriteFile	prod	§4 本地文件
sync\.Mutex|sync\.RWMutex|sync\.Map|atomic\.(Int64|Int32|Uint64)|map\[uint\]int	prod	§1 进程内共享状态（多为合法镜像，逐个确认）
optional:"true"|([Rr]edis|[Cc]luster|[Ss]tore|[Ll]imiter|[Ll]ease|[Gg]ate)[A-Za-z]* [!=]= nil	prod	§1 可选协作者/单节点分支
go func\(|time\.NewTicker|time\.Tick\(	prod	§3 后台任务是否需要 JobLease
db\.Transaction\(|\.Save\(|AssignmentColumns	prod	§2/§5 配置写入与共享计数
usage_aggregation_journal|UsageAggregationJournal	all	§6 已删除的 journal
_nano_usd|NanoUSD	prod	§7 只有 USD 金额？需本币孪生列
batch_text|BatchText	all	§8 凭据导入应为 entries
regexp\.MustCompile	prod	§8 正则应在包级编译
"gpt-load"	prod	§8 owned_by 等应取 gateway_name
PATTERNS
)
# 输出“文件:新行号: 内容”，只取新增行。
added=$(git diff "${diff_range[@]}" -U0 -- . ':(exclude)*.lock' ':(exclude)pnpm-lock.yaml' ':(exclude)go.sum' |
  awk '/^\+\+\+ /{file=substr($0,7); next} /^@@/{split($3,a,","); line=substr(a[1],2); next} /^\+/{print file ":" line ": " substr($0,2); line++}')
added_prod=$(grep -vE '^[^:]*(_test\.go|\.spec\.ts|/testutil/|/testdata/):' <<<"$added" || true)
while IFS=$'\t' read -r pattern scope label; do
  lines=$added
  [[ "$scope" == prod ]] && lines=$added_prod
  hits=$(grep -E -- "$pattern" <<<"$lines" || true)
  if [[ -n "$hits" ]]; then
    printf '\n-- %s\n%s\n' "$label" "$hits"
  fi
done <<<"$patterns"

section "go.mod 中不应回来的依赖"
grep -nE 'glebarez/sqlite|gorm.io/driver/mysql|go-sql-driver/mysql|modernc.org/sqlite' go.mod || echo "(无)"
