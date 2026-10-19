#!/usr/bin/env python3
"""测试网关 prompt cache 命中率。

命中率 = sum(prompt_tokens_details.cached_tokens) / sum(prompt_tokens)，
第 1 个请求用于预热，单独列出，不计入汇总。

场景:
  same       每次发完全相同的请求
  prefix     固定长 system prompt + 每次不同的短问题（最常见的业务形态）
  multiturn  多轮对话，每轮把上一轮的回答追加进 messages

用法:
  export AIGW_KEY=sk-xxx
  python3 tools/cachehit/cache_hit.py --mode prefix -n 10
"""

import argparse
import json
import os
import sys
import time
import urllib.error
import urllib.request

DEFAULT_URL = "https://lingying-gateway.echojoy.cn/gateway/v1/chat/completions"

# 足够长的固定前缀（约 3k tokens），多数上游要求前缀 >= 1024 tokens 才会缓存。
SYSTEM_PROMPT = "你是一名资深小说编辑，负责审阅与创作中文短篇小说。以下是写作规范：\n" + "\n".join(
    f"第{i}条：人物动机必须清晰，情节推进要有因果，场景描写需服务于情绪，"
    f"对白要符合人物身份，避免空洞的形容词堆砌，每一章结尾留下悬念。"
    for i in range(1, 61)
)

QUESTIONS = [
    "用一句话概括一个科幻小说的开头。",
    "给一个悬疑故事起三个标题。",
    "描写一段雨夜的街景，50字以内。",
    "写一句反派的经典台词。",
    "给武侠小说主角起一个名字并说明寓意。",
    "用一句话写出离别的情绪。",
    "列出三个适合童话的场景。",
    "写一个反转结局的点子，30字以内。",
]


def build_messages(mode, i, history):
    if mode == "same":
        return [{"role": "system", "content": SYSTEM_PROMPT},
                {"role": "user", "content": QUESTIONS[0]}]
    if mode == "prefix":
        return [{"role": "system", "content": SYSTEM_PROMPT},
                {"role": "user", "content": QUESTIONS[i % len(QUESTIONS)]}]
    # multiturn
    history.append({"role": "user", "content": QUESTIONS[i % len(QUESTIONS)]})
    return [{"role": "system", "content": SYSTEM_PROMPT}] + history


def request(url, key, model, messages, max_tokens, timeout):
    body = json.dumps({
        "model": model,
        "messages": messages,
        "stream": True,
        "stream_options": {"include_usage": True},
        "max_tokens": max_tokens,
    }).encode()
    req = urllib.request.Request(url, data=body, headers={
        "Authorization": f"Bearer {key}",
        "Content-Type": "application/json",
    })
    start = time.monotonic()
    ttft = None
    content = []
    usage = None
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        for raw in resp:
            line = raw.decode().strip()
            if not line.startswith("data:"):
                continue
            data = line[5:].strip()
            if data == "[DONE]":
                break
            chunk = json.loads(data)
            if chunk.get("usage"):
                usage = chunk["usage"]
            for choice in chunk.get("choices") or []:
                delta = choice.get("delta") or {}
                if ttft is None and (delta.get("content") or delta.get("reasoning")):
                    ttft = time.monotonic() - start
                if delta.get("content"):
                    content.append(delta["content"])
    if usage is None:
        raise RuntimeError("响应中没有 usage，无法统计缓存")
    return usage, "".join(content), ttft, time.monotonic() - start


def main():
    p = argparse.ArgumentParser(description="测试网关 prompt cache 命中率")
    p.add_argument("--url", default=os.environ.get("AIGW_URL", DEFAULT_URL))
    p.add_argument("--key", default=os.environ.get("AIGW_KEY"))
    p.add_argument("--model", default="deepseek-flash")
    p.add_argument("--mode", choices=["same", "prefix", "multiturn"], default="prefix")
    p.add_argument("-n", "--requests", type=int, default=10, help="请求总数（含 1 个预热）")
    p.add_argument("--interval", type=float, default=1.0, help="请求间隔秒数")
    p.add_argument("--max-tokens", type=int, default=512)
    p.add_argument("--timeout", type=float, default=120)
    args = p.parse_args()
    if not args.key:
        sys.exit("缺少 key：设置 AIGW_KEY 或传 --key")

    history = []
    rows = []
    print(f"mode={args.mode} model={args.model} n={args.requests}\n")
    print(f"{'#':>3} {'prompt':>7} {'cached':>7} {'hit%':>6} {'write':>6} {'ttft':>6} {'total':>6}")
    for i in range(args.requests):
        messages = build_messages(args.mode, i, history)
        try:
            usage, content, ttft, total = request(
                args.url, args.key, args.model, messages, args.max_tokens, args.timeout)
        except urllib.error.HTTPError as e:
            sys.exit(f"请求 {i} 失败: HTTP {e.code} {e.read().decode(errors='replace')}")
        if args.mode == "multiturn":
            history.append({"role": "assistant", "content": content})

        prompt = usage.get("prompt_tokens", 0)
        cached = (usage.get("prompt_tokens_details") or {}).get("cached_tokens", 0)
        write = usage.get("cache_creation_input_tokens", 0)
        rows.append((prompt, cached, ttft or total))
        hit = cached / prompt * 100 if prompt else 0
        tag = " (预热)" if i == 0 else ""
        print(f"{i:>3} {prompt:>7} {cached:>7} {hit:>5.1f}% {write:>6} "
              f"{(ttft or 0):>5.2f}s {total:>5.2f}s{tag}")
        if i + 1 < args.requests:
            time.sleep(args.interval)

    measured = rows[1:]
    if not measured:
        return
    prompt_sum = sum(r[0] for r in measured)
    cached_sum = sum(r[1] for r in measured)
    hit_reqs = sum(1 for r in measured if r[1] > 0)
    avg_ttft = sum(r[2] for r in measured) / len(measured)
    print(f"\n汇总（不含预热，{len(measured)} 个请求）")
    print(f"  token 命中率:   {cached_sum}/{prompt_sum} = {cached_sum / prompt_sum * 100:.1f}%")
    print(f"  请求命中率:     {hit_reqs}/{len(measured)} = {hit_reqs / len(measured) * 100:.1f}%")
    print(f"  平均首字延迟:   {avg_ttft:.2f}s")


if __name__ == "__main__":
    main()
