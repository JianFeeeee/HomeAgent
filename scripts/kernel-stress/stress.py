#!/usr/bin/env python3
"""二进制级调度器压力：多并发连接轰炸排队输入 + L4 中断，并采样峰值。

用法: stress.py <sock> <key> <conns> <inputs_per_conn> <interrupt_threads> <interrupts_each> [resident]
"""
import json, os, socket, sys, threading, time

SOCK, KEY = sys.argv[1], sys.argv[2]
NC, NI, IT, IE = (int(x) for x in sys.argv[3:7])
MODE = sys.argv[7] if len(sys.argv) > 7 else ""
GAP = float(sys.argv[8]) if len(sys.argv) > 8 else 0.05

lock = threading.Lock()
sent = 0
done = 0
errs = []
lat = []
peak = {"queue": 0, "pending": 0, "stack": 0}


def connect():
    s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    s.settimeout(60)
    s.connect(SOCK)
    s.sendall(("/auth %s\n" % KEY).encode())
    buf = b""
    while b"\n" not in buf:
        buf += s.recv(65536)
    return s


def chat(s, text):
    """发一条输入并等到终止帧（response/error）。"""
    s.sendall((text + "\n").encode())
    buf = b""
    while True:
        c = s.recv(65536)
        if not c:
            raise RuntimeError("closed")
        buf += c
        while b"\n" in buf:
            ln, buf = buf.split(b"\n", 1)
            if not ln.strip():
                continue
            try:
                obj = json.loads(ln.decode("utf-8", "replace"))
            except Exception:
                continue
            if obj.get("type") in ("response", "error"):
                return obj


def worker(idx, n):
    global sent, done
    try:
        s = connect()
    except Exception as e:
        with lock:
            errs.append("connect: %r" % e)
        return
    for i in range(n):
        t = time.time()
        try:
            obj = chat(s, "w%d-%d%s" % (idx, i, " !resident" if MODE == "resident" else ""))
        except Exception as e:
            with lock:
                errs.append("chat: %r" % e)
            return
        dt = time.time() - t
        with lock:
            sent += 1
            if obj.get("type") == "response":
                done += 1
                lat.append(dt)
    s.close()


def interrupter(idx, n, gap):
    for i in range(n):
        try:
            s = connect()
            s.sendall(("/interrupt stress-%d-%d\n" % (idx, i)).encode())
            time.sleep(gap)
            s.close()
        except Exception as e:
            with lock:
                errs.append("intr: %r" % e)
            return


def sampler(stop, dur):
    while not stop.is_set():
        try:
            s = connect()
            s.sendall(b"/kernel\n")
            buf = b""
            while b"\n" not in buf:
                buf += s.recv(65536)
            obj = json.loads(buf.split(b"\n")[0].decode())
            sc = json.loads(obj["content"]).get("scheduler", {})
            with lock:
                peak["queue"] = max(peak["queue"], sc.get("ready_queue_depth", 0))
                peak["pending"] = max(peak["pending"], sc.get("pending_interrupts", 0))
                peak["stack"] = max(peak["stack"], sc.get("suspend_stack", 0))
            s.close()
        except Exception:
            time.sleep(0.05)
        time.sleep(0.1)


def kstat():
    s = connect()
    s.sendall(b"/kernel\n")
    buf = b""
    while b"\n" not in buf:
        buf += s.recv(65536)
    return json.loads(json.loads(buf.split(b"\n")[0].decode())["content"])


t0 = time.time()
stop = threading.Event()
threads = [threading.Thread(target=worker, args=(i, NI)) for i in range(NC)]
threads += [threading.Thread(target=interrupter, args=(i, IE, GAP)) for i in range(IT)]
smp = threading.Thread(target=sampler, args=(stop, 0), daemon=True)
smp.start()
for t in threads:
    t.start()
for t in threads:
    t.join()
stop.set()
time.sleep(0.4)
st = kstat()
with lock:
    lat.sort()
    p = lambda q: (lat[int(len(lat) * q)] if lat else 0)
    print(json.dumps({
        "耗时s": round(time.time() - t0, 2),
        "并发连接": NC, "每连接输入": NI, "中断线程": IT, "每线程中断": IE,
        "完成": done, "错误": len(errs),
        "延迟_p50": round(p(0.5), 2), "延迟_p95": round(p(0.95), 2), "延迟_max": round(lat[-1] if lat else 0, 2),
        "峰值_队列": peak["queue"], "峰值_待处理中断": peak["pending"], "峰值_中断栈": peak["stack"],
        "scheduler": st.get("scheduler"), "goroutines": st.get("runtime", {}).get("goroutines"),
        "错误样本": errs[:3],
    }, ensure_ascii=False))
