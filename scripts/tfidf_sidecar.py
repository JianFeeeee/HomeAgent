#!/usr/bin/env python3
"""
TF-IDF embedding sidecar for HomeAgent.

HTTP interface:
  POST /embed   → {"text":"...", "side":"query|document"} → {"embedding":[...]}
  POST /train   → {"documents": {"id":"text", ...}} → {"status":"ok", "count":N}
  POST /search  → {"query":"...", "topK":5} → {"results":[{"id":"...", "score":0.3, "text":"..."}]}
  GET  /health  → {"status":"ok","count":N,"type":"tfidf"}

Provides TF-IDF sparse vectors via the same HTTP contract as Jina/ONNX dense vectors,
allowing the core to treat all embedding providers uniformly.
"""

import json
import math
import os
import re
import sys
import threading
import time
from collections import Counter
from http.server import HTTPServer, BaseHTTPRequestHandler
from urllib.parse import urlparse

# ─── Chinese Tokenizer (jieba) ──────────────────────────────────────────────
try:
    import jieba
    def tokenize(text):
        return [w for w in jieba.cut(text) if w.strip()]
except ImportError:
    # Fallback: simple character/word split
    def tokenize(text):
        return re.findall(r'[\w\u4e00-\u9fff]+', text)

# ─── TF-IDF Engine ──────────────────────────────────────────────────────────
class TFIDFEngine:
    def __init__(self):
        self.lock = threading.RLock()
        self.doc_freq = Counter()
        self.total_docs = 0
        self.docs = {}  # id -> {"text": ..., "vec": ...}

    def train(self, docs: dict):
        """Train IDF statistics from a batch of documents."""
        with self.lock:
            self.total_docs = len(docs)
            self.doc_freq = Counter()
            seen_per_doc = []
            for text in docs.values():
                features = tokenize(text)
                seen = set()
                for f in features:
                    if f not in seen:
                        self.doc_freq[f] += 1
                        seen.add(f)
            # Store docs with their vectors
            for did, text in docs.items():
                self.docs[did] = {"text": text, "vec": self._vectorize(text)}
            print(f"[tfidf] trained on {len(docs)} docs, {len(self.doc_freq)} features", flush=True)

    def _vectorize(self, text):
        features = tokenize(text)
        tf = Counter(features)
        max_tf = max(tf.values()) if tf else 1
        vec = {}
        for f, count in tf.items():
            tf_norm = count / max_tf
            if self.total_docs < 3:
                vec[f] = tf_norm
                continue
            df = self.doc_freq.get(f, 0)
            if df <= 0:
                continue
            idf = math.log((self.total_docs + 1) / (df + 1))
            if idf < 0.1:
                continue
            vec[f] = tf_norm * idf
        return vec

    def vectorize(self, text):
        with self.lock:
            return self._vectorize(text)

    def search(self, query, top_k=5):
        with self.lock:
            q_vec = self._vectorize(query)
            results = []
            for did, entry in self.docs.items():
                score = self._cosine(q_vec, entry["vec"])
                if score > 0.01:
                    results.append({"id": did, "score": score, "text": entry["text"][:200]})
            results.sort(key=lambda x: -x["score"])
            return results[:top_k]

    def add(self, did, text):
        with self.lock:
            self.docs[did] = {"text": text, "vec": self._vectorize(text)}

    def remove(self, did):
        with self.lock:
            self.docs.pop(did, None)

    @staticmethod
    def _cosine(a, b):
        dot = sum(a.get(k, 0) * b.get(k, 0) for k in set(a) | set(b))
        na = math.sqrt(sum(v * v for v in a.values()))
        nb = math.sqrt(sum(v * v for v in b.values()))
        if na == 0 or nb == 0:
            return 0
        return dot / (na * nb)

# ─── HTTP Server ─────────────────────────────────────────────────────────────
PORT = int(os.environ.get("TFIDF_PORT", "18998"))
engine = TFIDFEngine()

class TFIDFHandler(BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        if "/health" not in str(args[0]):
            print(f"[tfidf] {fmt % args}", flush=True)

    def do_GET(self):
        if urlparse(self.path).path == "/health":
            self._respond(200, {"status": "ok", "count": len(engine.docs), "type": "tfidf"})
        else:
            self._respond(404, {"error": "not found"})

    def do_POST(self:
        path = urlparse(self.path).path
        try:
            length = int(self.headers.get("Content-Length", 0))
            body = json.loads(self.rfile.read(length))
        except Exception as e:
            self._respond(400, {"error": str(e)})
            return

        if path == "/train":
            docs = body.get("documents", {})
            engine.train(docs)
            self._respond(200, {"status": "ok", "count": len(docs)})

        elif path == "/embed":
            text = body.get("text", "")
            vec = engine.vectorize(text)
            # Convert to list format matching HTTPEmbedder contract
            # Sparse vector → dense-ish list (feature indices as keys)
            embedding = [vec.get(k, 0) for k in sorted(vec.keys())] if vec else []
            self._respond(200, {"embedding": embedding, "sparse": vec})

        elif path == "/search":
            query = body.get("query", "")
            top_k = body.get("topK", 5)
            results = engine.search(query, top_k)
            self._respond(200, {"results": results})

        elif path == "/add":
            did = body.get("id", "")
            text = body.get("text", "")
            engine.add(did, text)
            self._respond(200, {"status": "ok"})

        elif path == "/remove":
            did = body.get("id", "")
            engine.remove(did)
            self._respond(200, {"status": "ok"})

        else:
            self._respond(404, {"error": "not found"})

    def _respond(self, status, data):
        body = json.dumps(data).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

if __name__ == "__main__":
    # Load from disk if available
    data_file = os.environ.get("TFIDF_DATA", "/data/homeagent/memory/tfidf_index.json")
    if os.path.exists(data_file):
        try:
            with open(data_file) as f:
                docs = json.load(f)
            engine.train(docs)
            print(f"[tfidf] loaded {len(docs)} docs from {data_file}", flush=True)
        except Exception as e:
            print(f"[tfidf] failed to load: {e}", flush=True)

    server = HTTPServer(("0.0.0.0", PORT), TFIDFHandler)
    print(f"[tfidf] listening on :{PORT}", flush=True)
    server.serve_forever()
