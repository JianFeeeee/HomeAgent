#!/usr/bin/env python3
"""最小 OpenAI 兼容 mock：可控延迟 + SSE 分块 + 可选工具调用。

用途：给压力测试一个**快且可控**的 LLM —— 没有它，无外网的 netns 里每条输入
都要走 provider 重试（≈2 分钟/条），既慢又压不出调度器行为。
"""
import json, os, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

DELAY_MS = int(os.environ.get("MOCK_DELAY_MS", "300"))
CHUNKS = int(os.environ.get("MOCK_CHUNKS", "8"))   # SSE 分块数：越多，流式段越长（可被中断的窗口越大）


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
        # 内核探活用 HEAD（见 internal/network/monitor.go CheckOnce）：
        # 不实现它 → BaseHTTPRequestHandler 回 501 → 判为不可达 → agent degraded/rollback。
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

        # 标记 !resident ⇒ 回一个工具调用，用于在**真实内核**里驱动驻留子工具链。
        if "!resident" in text and not any(m.get("role") == "tool" for m in msgs):
            tc = {"id": "call_mock_1", "type": "function",
                  "function": {"name": "resident_agents",
                               "arguments": json.dumps({"action": "create", "id": "r1",
                                                        "task_prompt": "驻留子任务：统计一下 !notify",
                                                        "input_chs": "cli"}, ensure_ascii=False)}}
            return self._respond(body, content=None, tool_calls=[tc])
        if "!notify" in text and not any(m.get("role") == "tool" for m in msgs):
            tc = {"id": "call_mock_2", "type": "function",
                  "function": {"name": "notify_parent",
                               "arguments": json.dumps({"text": "mock 汇报：子已完成统计"}, ensure_ascii=False)}}
            return self._respond(body, content=None, tool_calls=[tc])
        return self._respond(body, content="mock-ok:" + text[:40])

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
                emit({"role": "assistant", "tool_calls": tool_calls})
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
                    "choices": [{"index": 0, "message": msg, "finish_reason": "stop"}],
                    "usage": {"prompt_tokens": 10, "completion_tokens": 10, "total_tokens": 20}})

    def _chunk(self, b):
        self.wfile.write(("%x\r\n" % len(b)).encode() + b + b"\r\n")


if __name__ == "__main__":
    port = int(os.environ.get("MOCK_PORT", "9099"))
    ThreadingHTTPServer(("127.0.0.1", port), H).serve_forever()
