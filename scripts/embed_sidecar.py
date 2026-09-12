#!/usr/bin/env python3
"""
Jina v5-omni-nano embedding sidecar for HomeAgent.

Minimal HTTP server exposing POST /embed matching the HTTPEmbedder contract:
  Request:  {"modality":"text|image", "side":"query|document", "text":"...", "data":"base64...", "mime":"..."}
  Response: {"embedding":[float...]}

Health: GET /health → {"status":"ok","model":"jina-v5-omni-nano","dim":768,"loaded":true}
"""

import base64
import io
import logging
import os
import signal
import sys
import threading
import time
from http.server import HTTPServer, BaseHTTPRequestHandler
from urllib.parse import urlparse

import numpy as np
import torch

# ─── Config ──────────────────────────────────────────────────────────────────
MODEL_DIR   = os.environ.get("JINA_MODEL_DIR", "/home/newqqagent/models/jina-v5-omni-nano")
PORT        = int(os.environ.get("JINA_PORT", "18999"))
DIMENSION   = int(os.environ.get("JINA_DIMENSION", "768"))
MAX_WORKERS = int(os.environ.get("JINA_MAX_WORKERS", "4"))
BATCH_SIZE  = int(os.environ.get("JINA_BATCH_SIZE", "8"))

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [embed_sidecar] %(message)s",
    datefmt="%Y-%m-%d %H:%M:%S",
)
log = logging.getLogger("embed_sidecar")

# ─── Model Loading ───────────────────────────────────────────────────────────
model = None
processor = None
model_lock = threading.Lock()
ready = False


def load_model():
    global model, processor, ready
    log.info("loading model from %s ...", MODEL_DIR)
    t0 = time.time()

    from transformers import AutoModel, AutoProcessor

    model = AutoModel.from_pretrained(
        MODEL_DIR,
        trust_remote_code=True,
        local_files_only=True,
        default_task="retrieval",
        modality="vision",
        dtype=torch.float32,
    ).eval()

    processor = AutoProcessor.from_pretrained(
        MODEL_DIR,
        trust_remote_code=True,
        local_files_only=True,
    )

    elapsed = time.time() - t0
    ready = True
    log.info("model loaded in %.1fs, dim=%d", elapsed, DIMENSION)


def embed_text(text: str, side: str = "query") -> list[float]:
    """Embed text with proper Query:/Document: prefix for retrieval."""
    prefix = "Query: " if side == "query" else "Document: "
    inputs = processor(
        text=[prefix + text],
        padding=True,
        truncation=True,
        max_length=1024,
        return_tensors="pt",
    )
    with torch.inference_mode():
        vec = model.embed(**inputs)
    return vec.float().cpu().numpy()[0].tolist()


def embed_image(data_b64: str, mime: str, side: str = "document") -> list[float]:
    """Embed image from base64 data."""
    from PIL import Image

    img_bytes = base64.b64decode(data_b64)
    img = Image.open(io.BytesIO(img_bytes)).convert("RGB")

    prefix = "Query: " if side == "query" else "Document: "
    inputs = processor(
        images=img,
        text=f"{prefix}<image>",
        return_tensors="pt",
    )
    with torch.inference_mode():
        vec = model.embed(**inputs)
    return vec.float().cpu().numpy()[0].tolist()


# ─── HTTP Server ─────────────────────────────────────────────────────────────
class EmbedHandler(BaseHTTPRequestHandler):
    """Handle /embed and /health endpoints."""

    def log_message(self, fmt, *args):
        # Suppress default access log for /health
        if "/health" not in str(args[0]):
            log.info(fmt, *args)

    def do_GET(self):
        parsed = urlparse(self.path)
        if parsed.path == "/health":
            self._respond(200, {
                "status": "ok" if ready else "loading",
                "model": "jina-v5-omni-nano",
                "dim": DIMENSION,
                "loaded": ready,
            })
        else:
            self._respond(404, {"error": "not found"})

    def do_POST(self):
        parsed = urlparse(self.path)
        if parsed.path != "/embed":
            self._respond(404, {"error": "not found"})
            return

        if not ready:
            self._respond(503, {"error": "model not loaded"})
            return

        # Read request body
        try:
            length = int(self.headers.get("Content-Length", 0))
            body = self.rfile.read(length)
            req = __import__("json").loads(body)
        except Exception as e:
            self._respond(400, {"error": f"invalid request: {e}"})
            return

        modality = req.get("modality", "text")
        side = req.get("side", "query")

        try:
            with model_lock:
                if modality == "text":
                    text = req.get("text", "")
                    if not text:
                        self._respond(400, {"error": "missing text field"})
                        return
                    vec = embed_text(text, side)
                elif modality == "image":
                    data = req.get("data", "")
                    mime = req.get("mime", "image/png")
                    if not data:
                        self._respond(400, {"error": "missing data field"})
                        return
                    vec = embed_image(data, mime, side)
                else:
                    self._respond(400, {"error": f"unsupported modality: {modality}"})
                    return

            self._respond(200, {"embedding": vec})
        except Exception as e:
            log.error("embed error: %s", e, exc_info=True)
            self._respond(500, {"error": str(e)})

    def _respond(self, status: int, data: dict):
        import json as json_mod
        body = json_mod.dumps(data).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


def main():
    # Load model in background thread so server can start accepting /health
    threading.Thread(target=load_model, daemon=True).start()

    server = HTTPServer(("0.0.0.0", PORT), EmbedHandler)

    def shutdown(signum, frame):
        log.info("shutting down...")
        server.shutdown()
        sys.exit(0)

    signal.signal(signal.SIGTERM, shutdown)
    signal.signal(signal.SIGINT, shutdown)

    log.info("listening on :%d", PORT)
    server.serve_forever()


if __name__ == "__main__":
    main()
