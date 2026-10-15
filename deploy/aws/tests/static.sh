#!/usr/bin/env bash
# 静态检查：shellcheck、bash 语法、Python 语法、cfn-lint（宁夏区域规则）、CloudFormation 模板校验。
# validate-template 只读，不创建任何资源。
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

echo "== shellcheck"
shellcheck -x scripts/*.sh remote/*.sh tests/*.sh
echo "== 变量后紧跟中文（macOS bash 3.2 会把多字节字符当作变量名的一部分）"
python3 - scripts/*.sh remote/*.sh tests/*.sh <<'PY'
import re, sys
pat = re.compile(r"\$[A-Za-z_][A-Za-z0-9_]*(?=[^\x00-\x7f])")
bad = [f"{f}:{n}: {l.rstrip()}" for f in sys.argv[1:] for n, l in enumerate(open(f, encoding="utf-8"), 1) if pat.search(l)]
if bad:
    print("\n".join(bad))
    sys.exit("上面这些变量要写成 ${name}")
PY
echo "== bash -n"
for f in scripts/*.sh remote/*.sh tests/*.sh; do bash -n "$f"; done
echo "== python"
python3 -m py_compile scripts/waf_rules.py
echo "== cfn-lint"
uvx --quiet cfn-lint --regions cn-northwest-1 -- network.yaml data.yaml gateway.yaml
echo "== validate-template"
for t in network data gateway; do
  aws cloudformation validate-template --region cn-northwest-1 --template-body "file://$t.yaml" >/dev/null
  echo "  $t.yaml ok"
done
echo "静态检查通过"
