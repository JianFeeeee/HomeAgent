#!/usr/bin/env python3
"""最小远程设备客户端（WebSocket，无第三方依赖）。

用途：在**真内核**上验证"agent → 设备"这条出站链路 ——
`output_send__device/<id>` 应该以 `op=push` 的帧落到设备。

流程：握手 → hello（自报 id/kind/caps）→ bind → 打印收到的帧；
看到 push 就往 --out 文件里写一行（便于 shell 断言）。

注意：实例跑在私有 netns 里，设备网关的 127.0.0.1:9890 在 netns 内，
所以本脚本要用 nsenter 进同一个 netns 跑，例如：
  nsenter -t <homed-pid> -n python3 devclient.py --port 9890 --token X --id pydev-1 --out /tmp/push.txt
"""
import argparse
import base64
import hashlib
import json
import os
import socket
import struct
import threading
import time

GUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"


class WS:
    def __init__(self, host, port, path, timeout=30):
        self.sock = socket.create_connection((host, port), timeout=timeout)
        self.sock.settimeout(timeout)
        self._handshake(host, port, path)

    def _handshake(self, host, port, path):
        key = base64.b64encode(os.urandom(16)).decode()
        req = (
            f"GET {path} HTTP/1.1\r\n"
            f"Host: {host}:{port}\r\n"
            "Upgrade: websocket\r\n"
            "Connection: Upgrade\r\n"
            f"Sec-WebSocket-Key: {key}\r\n"
            "Sec-WebSocket-Version: 13\r\n\r\n"
        )
        self.sock.sendall(req.encode())
        buf = b""
        while b"\r\n\r\n" not in buf:
            chunk = self.sock.recv(4096)
            if not chunk:
                raise RuntimeError("握手未完成: 连接关闭")
            buf += chunk
        head = buf.decode("utf-8", "replace")
        if "101" not in head.split("\r\n")[0]:
            raise RuntimeError("握手被拒: " + head.split("\r\n")[0])
        expect = base64.b64encode(hashlib.sha1((key + GUID).encode()).digest()).decode()
        if expect.lower() not in head.lower():
            raise RuntimeError("Sec-WebSocket-Accept 校验失败")
        self.buf = buf.split(b"\r\n\r\n", 1)[1]

    # ---- 发送 ----
    def send(self, opcode, payload=b""):
        header = bytes([0x80 | opcode])
        mask = os.urandom(4)
        n = len(payload)
        if n < 126:
            header += bytes([0x80 | n])
        elif n < 65536:
            header += bytes([0x80 | 126]) + struct.pack(">H", n)
        else:
            header += bytes([0x80 | 127]) + struct.pack(">Q", n)
        masked = bytes(b ^ mask[i % 4] for i, b in enumerate(payload))
        self.sock.sendall(header + mask + masked)

    def send_json(self, obj):
        self.send(0x1, json.dumps(obj).encode())

    # ---- 接收 ----
    def _read(self, n):
        while len(self.buf) < n:
            chunk = self.sock.recv(65536)
            if not chunk:
                raise RuntimeError("连接关闭")
            self.buf += chunk
        out, self.buf = self.buf[:n], self.buf[n:]
        return out

    def recv_frame(self):
        b0, b1 = self._read(2)
        opcode = b0 & 0x0F
        ln = b1 & 0x7F
        if ln == 126:
            ln = struct.unpack(">H", self._read(2))[0]
        elif ln == 127:
            ln = struct.unpack(">Q", self._read(8))[0]
        masked = b1 & 0x80
        mask = self._read(4) if masked else None
        payload = self._read(ln)
        if mask:
            payload = bytes(b ^ mask[i % 4] for i, b in enumerate(payload))
        return opcode, payload


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--host", default="127.0.0.1")
    ap.add_argument("--port", type=int, default=9890)
    ap.add_argument("--token", required=True, help="设备接入 token（config_remotedevice.ws_token）")
    ap.add_argument("--id", default="pydev-1")
    ap.add_argument("--kind", default="computer")
    ap.add_argument("--caps", default="cmd")
    ap.add_argument("--seconds", type=float, default=25)
    ap.add_argument("--out", default="", help="收到 push 时写一行到此文件")
    args = ap.parse_args()

    ws = WS(args.host, args.port, "/api/v1/device/ws?token=" + args.token)
    ws.send_json({"op": "hello", "device": {
        "device_id": args.id, "name": "python 设备", "kind": args.kind,
        "caps": [c for c in args.caps.split(",") if c],
    }})
    op, payload = ws.recv_frame()
    ack = json.loads(payload)
    print("[devclient] hello_ack:", ack, flush=True)

    ws.send_json({"op": "bind", "device_id": args.id, "token": args.token})
    op, payload = ws.recv_frame()
    bind = json.loads(payload)
    print("[devclient] bind_ack:", bind, flush=True)
    if not bind.get("ok"):
        raise SystemExit("bind 被拒: %s" % bind)

    deadline = time.time() + args.seconds
    ws.sock.settimeout(1.0)
    while time.time() < deadline:
        try:
            op, payload = ws.recv_frame()
        except socket.timeout:
            continue
        except Exception as e:  # 服务端关闭
            print("[devclient] 连接结束:", e, flush=True)
            break
        if op == 0x9:  # ping → pong
            ws.send(0xA, payload)
            continue
        if op == 0x2:
            print("[devclient] 收到二进制帧 %d 字节" % len(payload), flush=True)
            continue
        if op != 0x1:
            continue
        try:
            m = json.loads(payload)
        except Exception:
            print("[devclient] 非 JSON 帧:", payload[:120], flush=True)
            continue
        print("[devclient] 收到帧:", json.dumps(m, ensure_ascii=False)[:300], flush=True)
        if m.get("op") == "push" and args.out:
            with open(args.out, "a") as f:
                f.write(json.dumps(m, ensure_ascii=False) + "\n")
            print("[devclient] ✅ 收到 push（agent → 设备链路通）", flush=True)


if __name__ == "__main__":
    main()
