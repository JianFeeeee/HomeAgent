import json, os, socket, sys, threading, time

SOCK = sys.argv[1]

KEY = os.environ.get("KCLI_KEY", "")

def connect(auth=True):
    s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    s.settimeout(20)
    s.connect(SOCK)
    if auth and KEY:
        s.sendall(("/auth %s\n" % KEY).encode())
        buf = b""
        while b"\n" not in buf:
            c = s.recv(65536)
            if not c:
                break
            buf += c
        if b"authenticated" not in buf:
            raise RuntimeError("auth failed: %r" % buf[:120])
    return s

def kernel_stats(sock=None, timeout=15):
    """连一条新连接查 /kernel，返回解析后的 dict。"""
    s = sock or connect()
    try:
        s.sendall(b"/kernel\n")
        buf = b""
        deadline = time.time() + timeout
        while time.time() < deadline:
            chunk = s.recv(65536)
            if not chunk:
                break
            buf += chunk
            for line in buf.split(b"\n"):
                if not line.strip():
                    continue
                try:
                    obj = json.loads(line.decode("utf-8", "replace"))
                except Exception:
                    continue
                if obj.get("type") == "response" and obj.get("content", "").lstrip().startswith("{"):
                    return json.loads(obj["content"])
        raise RuntimeError("no /kernel response")
    finally:
        if sock is None:
            s.close()

def blast(n_queued, n_interrupt, tag):
    """一条连接狂发：排队输入与中断交错。响应在后台线程里丢弃，避免缓冲阻塞。"""
    s = connect()
    threading.Thread(target=lambda: drain(s), daemon=True).start()
    sent = 0
    for i in range(max(n_queued, n_interrupt)):
        if i < n_queued:
            s.sendall(("queued-%s-%d\n" % (tag, i)).encode())
            sent += 1
        if i < n_interrupt:
            s.sendall(("/interrupt intr-%s-%d\n" % (tag, i)).encode())
            sent += 1
    return sent

def drain(s):
    try:
        while s.recv(65536):
            pass
    except Exception:
        pass

if __name__ == "__main__":
    cmd = sys.argv[2] if len(sys.argv) > 2 else "stats"
    if cmd == "stats":
        st = kernel_stats()
        if len(sys.argv) > 3 and sys.argv[3] == "full":
            print(json.dumps(st, ensure_ascii=False))
        else:
            sched = st.get("scheduler") or st
            print(json.dumps(sched, ensure_ascii=False))
    elif cmd == "blast":
        n = blast(int(sys.argv[3]), int(sys.argv[4]), sys.argv[5] if len(sys.argv) > 5 else "b")
        print("sent=%d" % n)
