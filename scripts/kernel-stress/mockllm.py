#!/usr/bin/env python3
"""最小 OpenAI 兼容 mock：可控延迟 + SSE 分块 + 可选工具调用。

用途：给压力测试一个**快且可控**的 LLM —— 没有它，无外网的 netns 里每条输入
都要走 provider 重试（≈2 分钟/条），既慢又压不出调度器行为。

## 批内并发（本文件最主要的能力）

内核的并发判据是「同一批**全部**工具都声明 ParallelSafe 才并发，一个不声明
就整批退回串行」，且 `len(PendingTools) <= 1` 时恒不并发。

所以要压批内并发，**必须让一次响应带多个 tool_call**，否则压的全是串行路径。
本 mock 用 `!batchN` 标记发 N 个全部只读的工具调用：

    !batch8   → 一轮里发 8 个 knowledge_search/knowledge_list/…

为什么这 8 个工具是"安全"的：它们都标了 ParallelSafe（已核实执行体无共享
写），而 knowledge_search 走的是 TF-IDF 关键词检索，**不触发 ONNX 推理**。
用 !mixed 混入一个未声明并发安全的工具（knowledge_create），验证**整批降级
为串行**这条规则在真实内核里也成立。

## ONNX / 多模态路径

生产有 3.4G 的 chinese-clip 与 qwen3-vl ONNX 模型，但推理跑在**独立
provider 进程**里（providers/chineseclip），内核只走 IPC。所以压测不需要
加载模型，用 `!img` / `!ocr` 触发内核侧的工具路径即可 —— 验的是内核的
IPC 接线、错误处理与超时，不是推理精度。

## 故障注入

`!slowN`  让响应慢 N 毫秒（压超时/中断窗口）
`!err`    返回 500（压 provider 重试与降级）
`!hang`   只回一半就不结束（压客户端超时与断连清理）
"""
import json
import os
import re
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

DELAY_MS = int(os.environ.get("MOCK_DELAY_MS", "300"))
CHUNKS = int(os.environ.get("MOCK_CHUNKS", "8"))   # SSE 分块数：越多，流式段越长（可被中断的窗口越大）

# 正则预编译：每条输入都要匹配，模块级编一次。
_RE_BATCH = re.compile(r"!batch(\d+)")
_RE_MIXED = re.compile(r"!mixed(\d+)")
# ★ 定量对比用的批次：工具**慢且可控**。
#
# 为什么必须让工具慢：并发的收益 = 单工具耗时 × (N-1)。工具若只跑几微秒，
# 串行与并发的总耗时差淹没在 LLM 延迟（DELAY_MS，默认 200ms）里，
# 测出来的差异全是噪声。
#
# cmd_run 已声明 ParallelSafe（执行体只依赖入参，共享的 p.history 由
# recordCmd 加锁保护），且 sleep 是纯计算、不碰磁盘 —— 延迟可精确预期。
_RE_SLOW = re.compile(r"!slowbatch(\d+)")

# 批内并发的工具集。全部是**只读且已核实无共享写**的内置工具，
# 且都不触发 ONNX（TF-IDF 关键词路）。
PARALLEL_TOOLS = ["knowledge_search", "knowledge_list", "doc_query",
                  "person_query", "person_network", "input_channels"]

# 混在批里的"不安全"工具：一旦出现，整批必须退回串行。
SERIAL_TOOL = "knowledge_create"


def mk_tool_call(idx, name, args):
    """造一个 tool_call。

    ★ `index` 字段**必须给**，且要与它在数组里的位置一致。

    内核靠分片自带的 StreamIndex（即上游 JSON 里的 "index"）分槽累积
    arguments（process.go:347 `idx := tc.StreamIndex`）。缺 index 时所有
    分片都落到槽 0，几个 tool_call 的 arguments 被**混拼**在一起 ——
    症状是每个工具都报「参数不是合法 JSON」，而工具一次都没真跑过。

    ★ 这个坑很隐蔽：单 tool_call 时不设 index 也正常（只有一个槽），
    所以老 mock 一直没暴露问题；一旦发多个就全崩。
    """
    return {"index": idx, "id": "call_batch_%d" % idx, "type": "function",
            "function": {"name": name, "arguments": json.dumps(args, ensure_ascii=False)}}


def slow_tool_calls(n, sleep_ms):
    """造 n 个 sleep 型 cmd_run —— 用于量化串行 vs 并发的差异。

    为什么用 sleep 而不是真跑命令：
      · 延迟可精确预期（不用去猜命令要多久）；
      · 不产生外部副作用（不写文件、不动网络）；
      · 不会因机器负载而失真。
    每个工具的 sleep 时长**故意错开**（递增），这样能验证落消息顺序
    按声明序而非完成序 —— 完成的顺序是反的（大的先完成）。
    """
    tcs = []
    for i in range(n):
        # 时长随索引递增 ⇒ 完成顺序与声明顺序**相反**
        ms = sleep_ms + i * 5
        tcs.append(mk_tool_call(i, "cmd_run", {
            "command": f"sleep {ms / 1000:.3f}; echo done-{i}",
            "timeout": "30s",
        }))
    return tcs


def batch_tool_calls(n, mixed=False):
    """造 n 个 tool_call；mixed=True 时夹一个未声明并发安全的工具。"""
    tcs = []
    for i in range(n):
        name = PARALLEL_TOOLS[i % len(PARALLEL_TOOLS)]
        if name == "knowledge_search":
            args = {"query": "并发压测 q%d" % i, "top_k": 3}
        elif name == "doc_query":
            args = {"query": "并发压测 q%d" % i, "mode": "auto"}
        elif name in ("person_query", "person_network"):
            args = {"name": "压测人物%d" % i}
        else:
            args = {}
        tcs.append(mk_tool_call(i, name, args))
    if mixed:
        # 放在**中间**：确保降级判据不能靠"最后一个工具"侥幸通过
        tcs.insert(len(tcs) // 2, mk_tool_call(999, SERIAL_TOOL,
                                              {"name": "压测/批内", "content": "mixed"}))
    return tcs


class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        pass

    def _json(self, obj, code=200):
        b = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)

    def do_HEAD(self):
        # 内核会拿 HEAD 探活。必须实现它 —— 不实现则 BaseHTTPRequestHandler
        # 回 501 → 判为不可达 → agent degraded/rollback。
        self.send_response(200)
        self.send_header("Content-Length", "0")
        self.end_headers()

    def do_GET(self):
        self._json({"object": "list", "data": [{"id": "mock", "object": "model"}]})

    def do_POST(self):
        n = int(self.headers.get("Content-Length", "0"))
        body = json.loads(self.rfile.read(n) or b"{}")
        msgs = body.get("messages") or []
        text = ""
        for m in reversed(msgs):
            if m.get("role") == "user":
                c = m.get("content")
                text = c if isinstance(c, str) else json.dumps(c, ensure_ascii=False)
                break

        has_tool_msg = any(m.get("role") == "tool" for m in msgs)

        # ---- 故障注入 ----
        if "!err" in text and not has_tool_msg:
            self._json({"error": {"message": "mock injected failure", "type": "server_error"}}, 500)
            return
        if "!hang" in text and not has_tool_msg:
            # 只回一半就断开：压客户端超时与连接清理
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", "999999")
            self.end_headers()
            self.wfile.write(b'{"id":"mock"')
            self.wfile.flush()
            time.sleep(30)
            return

        # ---- 多模态 / ONNX 路径（不加载模型，只验内核接线与错误处理）----
        if ("!img" in text or "!ocr" in text) and not has_tool_msg:
            name = "describe_image" if "!img" in text else "ocr_image"
            return self._respond(body, content=None,
                                 tool_calls=[mk_tool_call(1, name, {"path": "/nonexistent.png"})])

        # ---- 批内并发：!batchN / !mixedN ----
        if not has_tool_msg:
            ms = _RE_SLOW.search(text)
            if ms:
                cnt = max(2, min(int(ms.group(1)), 32))
                sm = int(os.environ.get("MOCK_TOOL_SLEEP_MS", "200"))
                return self._respond(body, content=None,
                                     tool_calls=slow_tool_calls(cnt, sm))

            mb = _RE_BATCH.search(text)
            mm = _RE_MIXED.search(text)
            if mb or mm:
                cnt = int((mb or mm).group(1))
                cnt = max(2, min(cnt, 32))   # 下限 2（1 个不会并发），上限 32 防止把 mock 压成瓶颈
                return self._respond(body, content=None,
                                     tool_calls=batch_tool_calls(cnt, mixed=mm is not None))

        # 标记 !resident ⇒ 回一个工具调用，用于在**真实内核**里驱动驻留子工具链。
        if "!resident" in text and not has_tool_msg:
            tc = {"index": 0, "id": "call_mock_1", "type": "function",
                  "function": {"name": "resident_agents",
                               "arguments": json.dumps({"action": "create", "id": "r1",
                                                        "task_prompt": "驻留子任务：统计一下 !notify",
                                                        "input_chs": "cli"}, ensure_ascii=False)}}
            return self._respond(body, content=None, tool_calls=[tc])
        if "!notify" in text and not has_tool_msg:
            tc = {"index": 0, "id": "call_mock_2", "type": "function",
                  "function": {"name": "notify_parent",
                               "arguments": json.dumps({"text": "mock 汇报：子已完成统计"}, ensure_ascii=False)}}
            return self._respond(body, content=None, tool_calls=[tc])
        return self._respond(body, content="mock-ok:" + text[:40])

    def _chunk(self, b):
        """写一个 HTTP/1.1 chunked 块。

        ★ 我重写本文件时把原有的 _chunk 漏掉了 —— 而流式路径每一帧都要调它，
        漏掉的表现是 AttributeError，**只在 stream=true 时才炸**。
        非流式路径照跑，所以粗看"能用"，一开流式就崩。
        """
        self.wfile.write(("%x\r\n" % len(b)).encode() + b + b"\r\n")

    def _respond(self, body, content=None, tool_calls=None):
        if body.get("stream"):
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Cache-Control", "no-cache")
            self.send_header("Transfer-Encoding", "chunked")
            self.end_headers()
            def emit(delta):
                data = json.dumps({"id": "mock", "object": "chat.completion.chunk",
                                   "model": "mock", "choices": [{"index": 0, "delta": delta}]})
                self._chunk(("data: " + data + "\n\n").encode())
            if tool_calls:
                # ★ 必须按 OpenAI 流式协议**分片**发：一个 chunk 一个 tool_call，
                #   各自带 index；后续 chunk 只续 arguments。
                #
                #   我第一版把整个数组塞进**一个** chunk，内核按"续传"语义累积
                #   arguments（process.go:364 acc.argsRaw.WriteString）——
                #   结果 4 个 tool_call 的参数被**混拼**到槽 0，
                #   每个工具都报"参数不是合法 JSON"，而工具一次都没真跑过。
                #
                #   症状离原因很远：看起来像"内核不支持多工具调用"，
                #   实际是我没按协议发。
                # OpenAI 真实语义：**每个** tool_call 都先发一片带
                # name 的首片，再发续传片。
                #
                # 我第一版只给第 0 个发首片、其余直接发续传片，看起来省事，
                # 但内核 flush 时按「无 name 即丢弃」处理（process.go:253
                # `flushed with EMPTY name`），于是 idx=1/2/3 三个分片
                # 全部被丢 ⇒ 只跑 1 个工具。
                #
                # 症状：内核日志里 idx 分对了，却只有一个 tool_call 活下来 ——
                # 看起来像"index 透传修好了但还有别的问题"。
                for tc in tool_calls:
                    emit({"tool_calls": [{
                        "index": tc["index"],
                        "id": tc["id"],
                        "type": "function",
                        "function": {"name": tc["function"]["name"],
                                      "arguments": ""},
                    }]})
                    emit({"tool_calls": [{
                        "index": tc["index"],
                        "function": {"arguments": tc["function"]["arguments"]},
                    }]})
                time.sleep(DELAY_MS / 1000.0)
            if content:
                emit({"role": "assistant", "content": content[: max(1, len(content) // CHUNKS)]})
                per = max(1, len(content) // CHUNKS)
                for i in range(per, len(content), per):
                    time.sleep(DELAY_MS / 1000.0 / CHUNKS)
                    emit({"content": content[i:i + per]})
            self._chunk(b"data: [DONE]\n\n")
            self._chunk(b"")
            return
        msg = {"role": "assistant", "content": content}
        if tool_calls:
            msg["tool_calls"] = tool_calls
            msg["content"] = None
        time.sleep(DELAY_MS / 1000.0)
        self._json({"id": "mock", "object": "chat.completion", "model": "mock",
                    "choices": [{"index": 0, "message": msg, "finish_reason": "tool_calls" if tool_calls else "stop"}]})


if __name__ == "__main__":
    import sys
    # 端口：先位置参数（原 mockllm.py 单独跑时用法），再 MOCK_PORT（launch.sh 用它），
    # 最后默认 9099。★ 三个来源都要留 —— 只认一个会破坏另外两个调用方。
    if len(sys.argv) > 1:
        port = int(sys.argv[1])
    else:
        port = int(os.environ.get("MOCK_PORT", "9099"))
    ThreadingHTTPServer(("127.0.0.1", port), H).serve_forever()
