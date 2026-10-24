"""计算 Web ACL `acl` 的目标规则列表（计划文档 5.3 节）。

输入：`aws wafv2 get-web-acl` 的 JSON（stdin）。输出：改动后的 Rules 列表（stdout）。
改动是声明式的，对已经改过的 ACL 再执行一次，输出与输入相同：

1. 删除 `block-llm-api-endpoints`。它是人工加的，按路径后缀拦 LLM 接口，dev-alb 上的
   其他服务也不需要它（2026-10-24 决定）。
2. 删除 `AWS-AWSManagedRulesPHPRuleSet`。
3. 给 `AWS-AWSManagedRulesKnownBadInputsRuleSet` 加 scope-down，跳过网关域名下转给新网关的
   文本接口（`/gateway/v1/chat/completions`、`/gateway/v1/messages*`、`/gateway/v1/responses*`）
   和 Gemini 接口（`/gateway/v1beta/models/*`）。

AWS CLI v2 的 JSON 输入输出里，ByteMatch 的 SearchString 是 base64。

用法：python3 waf_rules.py <gateway-host> [--only-php] < web-acl.json > rules.json

--only-php 只做第 2 项。PHP 规则组会把镜像层误判为 PHP 攻击、拦截向 docker.echojoy.cn
的推送，所以在 P1 先删（2026-10-15 决定）；第 1、3 项在 P3 切换时做。
"""

import base64
import json
import sys

BLOCK_RULE = "block-llm-api-endpoints"
PHP_RULE = "AWS-AWSManagedRulesPHPRuleSet"
SCOPED_RULE = "AWS-AWSManagedRulesKnownBadInputsRuleSet"
# 转给新网关的文本接口，KnownBadInputs 跳过它们。WAF 看到的是 ALB 改写之后的路径
# （2026-10-15 实测：采样里的 URI 是 /v1/chat/completions，而不是 /gateway/v1/chat/completions），
# 所以这里写改写后的前缀，
# 对应 gateway.yaml 监听规则里的 /gateway/v1/chat/completions、messages*、responses*、v1beta/models/*。
GATEWAY_TEXT_PREFIXES = ["/v1/chat/completions", "/v1/messages", "/v1/responses", "/v1beta/models/"]


def b64(text):
    return base64.b64encode(text.encode()).decode()


def byte_match(field, text, constraint, transform="NONE"):
    return {
        "ByteMatchStatement": {
            "SearchString": b64(text),
            "FieldToMatch": field,
            "TextTransformations": [{"Priority": 0, "Type": transform}],
            "PositionalConstraint": constraint,
        }
    }


def not_gateway_text(host):
    """匹配「不是 网关域名 + 文本接口路径」的请求。"""
    paths = [byte_match({"UriPath": {}}, p, "STARTS_WITH") for p in GATEWAY_TEXT_PREFIXES]
    return {
        "NotStatement": {
            "Statement": {
                "AndStatement": {
                    "Statements": [
                        byte_match({"SingleHeader": {"Name": "host"}}, host, "EXACTLY", "LOWERCASE"),
                        {"OrStatement": {"Statements": paths}},
                    ]
                }
            }
        }
    }


def target_rules(acl, host, only_php=False):
    removed = {PHP_RULE} if only_php else {PHP_RULE, BLOCK_RULE}
    rules = []
    for rule in acl["Rules"]:
        if rule["Name"] in removed:
            continue
        if not only_php and rule["Name"] == SCOPED_RULE:
            rule["Statement"]["ManagedRuleGroupStatement"]["ScopeDownStatement"] = not_gateway_text(host)
        rules.append(rule)
    if SCOPED_RULE not in {r["Name"] for r in rules}:
        raise SystemExit(f"Web ACL 里没有 {SCOPED_RULE}，规则结构和计划不符")
    return rules


def main():
    host = sys.argv[1]
    acl = json.load(sys.stdin)["WebACL"]
    only_php = "--only-php" in sys.argv[2:]
    json.dump(target_rules(acl, host, only_php), sys.stdout, ensure_ascii=False)


if __name__ == "__main__":
    main()
