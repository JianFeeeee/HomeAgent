#!/usr/bin/env python3
"""带 usage 的最小 OpenAI 兼容 mock —— 专用于验证「用量是否真的走到回包」。

与 scripts/kernel-stress/mockllm.py 的区别：那个只造 tool_call 压并发，
**不带 usage 字段**，所以验不了账目链路。这个只做一件事：每帧都带
prompt_tokens/completion_tokens/total_tokens + 缓存字段。

用法：
    python3 usage_mock_llm.py <port>

支持的标记（放在用户消息里）：
    普通文本   → 直接回文本
    !tool      → 先回一个 tool_call（触发工具回环，验「多轮求和」）
    任意请求都会带 usage；!nocache 时不带缓存字段（验「没报 ≠ 0」）
"""
import json
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


def _port_from_argv() -> int:
    """从 argv 取端口，缺失或非法时退回默认值。"""
    if len(sys.argv) > 1:
        try:
            return int(sys.argv[1])
        except ValueError:
            print(f"非法端口 {sys.argv[1]!r}，退回默认值", file=sys.stderr)
    return 18099


PORT = _port_from_argv()
# 固定的用量数字，便于断言。第一轮用小值，第二轮用大值，
# 这样「求和」与「只报最后一次」可以区分。
USAGE_TURN1 = {"prompt_tokens": 300, "completion_tokens": 30, "total_tokens": 330,
               "prompt_cache_hit_tokens": 200, "prompt_cache_miss_tokens": 100}
USAGE_TURN2 = {"prompt_tokens": 700, "completion_tokens": 70, "total_tokens": 770,
               "prompt_cache_hit_tokens": 500, "prompt_cache_miss_tokens": 200}
USAGE_PLAIN = {"prompt_tokens": 1000, "completion_tokens": 200, "total_tokens": 1200,
               "prompt_cache_hit_tokens": 768, "prompt_cache_miss_tokens": 232}


def _usage_for(body):
    """按对话轮数决定报哪一组用量（模拟多轮工具回环）。"""
    msgs = body.get("messages") or []
    has_tool = any(m.get("role") == "tool" for m in msgs)
    if body.get("_force_plain_after"):
        return USAGE_PLAIN
    return USAGE_TURN2 if has_tool else USAGE_TURN1


class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, format, *args):
        """吞掉访问日志：压测时它会淹掉真正的输出。"""
        return

    def _read(self):
        try:
            n = int(self.headers.get("Content-Length") or 0)
        except (TypeError, ValueError):
            n = 0
        raw = self.rfile.read(n) if n else b"{}"
        try:
            return json.loads(raw.decode("utf-8", "replace"))
        except Exception:
            return {}

    def do_POST(self):
        body = self._read()
        msgs = body.get("messages") or []
        text = ""
        for m in reversed(msgs):
            if m.get("role") == "user":
                c = m.get("content")
                text = c if isinstance(c, str) else json.dumps(c, ensure_ascii=False)
                break

        has_tool_msg = any(m.get("role") == "tool" for m in msgs)
        want_tool = ("!tool" in text) and not has_tool_msg
        usage = _usage_for(body)

        # 只有显式 !nocache 才剥掉缓存字段（验「上游没报」与「报了 0」不同）
        if "!nocache" in text:
            usage = {k: v for k, v in usage.items() if "cache" not in k}

        if body.get("stream"):
            self._stream(want_tool, usage)
        else:
            self._json(want_tool, usage)

    def _json(self, want_tool, usage):
        msg: dict = {"role": "assistant", "content": "" if want_tool else "收到（mock）"}
        finish = "stop"
        if want_tool:
            msg["tool_calls"] = [{
                "id": "call_mock_1", "type": "function",
                "function": {"name": "knowledge_list", "arguments": "{}"},
            }]
            finish = "tool_calls"
        payload = {
            "id": "chatcmpl-mock", "object": "chat.completion",
            "created": 1, "model": "mock",
            "choices": [{"index": 0, "message": msg, "finish_reason": finish}],
            "usage": usage,
        }
        b = json.dumps(payload).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)

    def _stream(self, want_tool, usage):
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Transfer-Encoding", "chunked")
        self.end_headers()

        # __setitem__ 的类型推断：msg 是 dict[str, str]，list 值需显式放宽。
        # 这是讨喜静态检查，不影响运行时行为。
        def send(obj):
            data = ("data: " + json.dumps(obj) + "\n\n").encode()
            self.wfile.write(f"{len(data):x}\r\n".encode() + data + b"\r\n")
            self.wfile.flush()

        base = {"id": "chatcmpl-mock", "object": "chat.completion.chunk",
                "created": 1, "model": "mock"}

        if want_tool:
            send({**base, "choices": [{"index": 0, "delta": {"tool_calls": [{
                "index": 0, "id": "call_mock_1", "type": "function",
                "function": {"name": "knowledge_list", "arguments": ""}}]},
                "finish_reason": None}]})
            send({**base, "choices": [{"index": 0, "delta": {"tool_calls": [{
                "index": 0, "function": {"arguments": "{}"}}]}, "finish_reason": None}]})
        else:
            send({**base, "choices": [{"index": 0, "delta": {"content": "收到（mock）"},
                                        "finish_reason": None}]})

        # 用量单独一帧（照 OpenAI 的 stream_options=include_usage 行为）
        send({**base, "choices": [], "usage": usage})
        self.wfile.write(b"0\r\n\r\n")
        self.wfile.flush()


if __name__ == "__main__":
    ThreadingHTTPServer(("127.0.0.1", PORT), H).serve_forever()
