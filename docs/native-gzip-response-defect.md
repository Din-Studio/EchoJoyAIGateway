# 缺陷：原生非流式 gzip 响应返回 502

- 日期：2026-11-09。
- 状态：阻塞，未修复。
- 影响：原生 OpenAI Chat 非流式请求，已在 CLIProxyAPI 渠道复现。
- 复现：请求发送 `Accept-Encoding: identity`，上游仍返回 `200 + Content-Encoding: gzip + 有效压缩 JSON`。
- 实际：客户端收到 `502 / upstream_protocol_error`；相同 JSON 的 identity 对照返回 200。
- 预期：有效单层 gzip 经有界解压后返回 200，保持模型、用量、计费和脱敏语义。
- 原因：Bifrost `PassthroughStream` 删除编码头，却原样交付压缩字节，触发执行器安全拒绝。
- 验证：真实 Handler + SDK 链复现；官方 core `v1.15.1` 独立测试仍丢失编码头。
- 阻塞：尚无经过验证的合格官方依赖；fasthttp `v1.78.0` 实测还会覆盖重复编码头，单独恢复 SDK 编码头不足以满足安全要求。
- 修复边界：保留可信编码声明，复用有界解压；继续拒绝损坏、超限和重复编码。不得按字节特征猜测后放行。fork 或依赖补丁必须另行批准。
- 上游跟踪：[Bifrost #9764](https://github.com/maximhq/bifrost/issues/9764)（编码头丢失）。
- 证据：`tmp/gzip-implementation-20261109/report.md`。
