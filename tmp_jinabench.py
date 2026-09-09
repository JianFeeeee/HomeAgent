#!/usr/bin/env python3
"""Real Jina v5-omni benchmark on HomeAgent production documents and media.

Inputs never leave the host. Reports Recall/Hit@K, MRR, margins, and latency for:
- Jina unified dense space: text query -> text document
- Jina unified dense space: text query -> production image
"""
from __future__ import annotations

import glob
import json
import os
import time
from pathlib import Path

import numpy as np
import torch
from PIL import Image
from transformers import AutoModel, AutoProcessor

MODEL = "/home/newqqagent/models/jina-v5-omni-nano"
DOC_DIR = "/home/newqqagent/memory/documents"
MEDIA_DB = "/home/newqqagent/memory/media/media.db"
BLOB_ROOT = "/home/newqqagent/memory/media/blobs"

CASES = [
    ("mail-semantic", "邮件代理是否已经成功接入", ["AgentMail 接入验证"]),
    ("fox-cross-language", "生成一张雪地红狐狸的图片", ["red fox in snowy forest"]),
    ("plugin-semantic", "升级安装 QQ 插件包", ["plugin_install"]),
    ("weather-paraphrase", "我所在城市的天气预报", ["河南新乡"]),
    ("textarea-paraphrase", "聊天输入区域文字多了会不会自动增高", ["输入框在内容超过一行"]),
    ("devices-paraphrase", "检查当前接入了哪些终端设备", ["你看看现在你都有哪些设备"]),
    ("memory-health", "长期文档记忆功能是否健康", ["文档记忆系统是否正常工作"]),
    ("reload-plugins", "重新加载全部扩展组件", ["热重载所有插件", "plgreload"]),
    ("exact-agentmail", "AgentMail 接入验证", ["AgentMail 接入验证"]),
    ("exact-plugin", "plugin_install", ["plugin_install"]),
]

MEDIA_QUERIES = [
    ("captcha-cn", "一张带有干扰线和字符的验证码图片", "7e689211"),
    ("captcha-en", "captcha with distorted letters and noise", "7e689211"),
    ("news-cn", "中文新闻报道页面截图", "f6f17229"),
    ("news-en", "a screenshot of a Chinese news article", "f6f17229"),
    ("notes-cn", "手机深色模式备忘录截图", "ee5bdb2a"),
    ("notes-en", "dark mode phone notes application screenshot", "ee5bdb2a"),
]


def load_docs():
    docs = []
    for p in sorted(glob.glob(DOC_DIR + "/doc_*.json")):
        try:
            d = json.load(open(p, encoding="utf-8"))
        except Exception:
            continue
        if not d.get("id"):
            continue
        docs.append({"id": d["id"], "text": d.get("summary", "") + "\n" + d.get("content", "")})
    return docs


def relevant(docs, seeds):
    seeds = [s.lower() for s in seeds]
    return {d["id"] for d in docs if any(s in d["text"].lower() for s in seeds)}


def embed_text(model, proc, texts, side, batch_size=4):
    result = []
    prefix = "Query: " if side == "query" else "Document: "
    for start in range(0, len(texts), batch_size):
        batch = [prefix + x for x in texts[start:start + batch_size]]
        inp = proc(text=batch, padding=True, truncation=True, max_length=1024, return_tensors="pt")
        with torch.inference_mode():
            vec = model.embed(**inp)
        result.append(vec.float().cpu().numpy())
    return np.concatenate(result, axis=0)


def measure_text(docs, doc_vecs, model, proc):
    out = []
    for name, query, seeds in CASES:
        rel = relevant(docs, seeds)
        t0 = time.perf_counter()
        qv = embed_text(model, proc, [query], "query", 1)[0]
        latency = (time.perf_counter() - t0) * 1000
        scores = doc_vecs @ qv
        order = np.argsort(-scores)
        rank = 0
        for pos, idx in enumerate(order, 1):
            if docs[int(idx)]["id"] in rel:
                rank = pos
                break
        best_rel = max((float(scores[i]) for i, d in enumerate(docs) if d["id"] in rel), default=float("nan"))
        best_neg = max((float(scores[i]) for i, d in enumerate(docs) if d["id"] not in rel), default=float("nan"))
        out.append({
            "name": name, "query": query, "relevant": len(rel), "rank": rank,
            "reciprocal_rank": 1.0 / rank if rank else 0.0,
            "hit_at_1": 0 < rank <= 1, "hit_at_5": 0 < rank <= 5,
            "query_latency_ms": latency,
            "best_relevant": best_rel, "best_negative": best_neg,
            "margin": best_rel - best_neg,
            "top": [{"id": docs[int(i)]["id"], "score": float(scores[i])} for i in order[:5]],
        })
    return out


def load_images():
    rows = [
        ("7e689211", "7e/689211bf78bc2f6366405b2a1f2fe0f7ec9ccdc59f368c13175c421c8e4018"),
        ("ee5bdb2a", "ee/5bdb2a5fa3927ede79384ee6ec7657428896e2690b622f2409daa4b3fa9fa3"),
        ("f6f17229", "f6/f17229b3513ea1f9c660353ee4ab021bec88e2b423780e8ed4445bc9aaf2fb"),
    ]
    return [(name, Image.open(os.path.join(BLOB_ROOT, rel)).convert("RGB")) for name, rel in rows]


def embed_image(model, proc, img):
    # Retrieval document side; nano's image geometry is nearly side-invariant, but use the documented prefix.
    inp = proc(images=img, text="Document: <image>", return_tensors="pt")
    with torch.inference_mode():
        return model.embed(**inp).float().cpu().numpy()[0]


def measure_media(images, image_vecs, model, proc):
    out = []
    names = [x[0] for x in images]
    for name, query, want in MEDIA_QUERIES:
        t0 = time.perf_counter()
        qv = embed_text(model, proc, [query], "query", 1)[0]
        latency = (time.perf_counter() - t0) * 1000
        scores = image_vecs @ qv
        order = np.argsort(-scores)
        want_idx = names.index(want)
        rank = int(np.where(order == want_idx)[0][0]) + 1
        best_neg = max(float(scores[i]) for i in range(len(names)) if i != want_idx)
        out.append({
            "name": name, "query": query, "want": want, "rank": rank,
            "query_latency_ms": latency, "positive": float(scores[want_idx]),
            "best_negative": best_neg, "margin": float(scores[want_idx]) - best_neg,
            "ranking": [{"id": names[int(i)], "score": float(scores[i])} for i in order],
        })
    return out


def summary(rows):
    return {
        "queries": len(rows),
        "hit_at_1": sum(r["hit_at_1"] for r in rows) / len(rows),
        "hit_at_5": sum(r["hit_at_5"] for r in rows) / len(rows),
        "mrr": sum(r["reciprocal_rank"] for r in rows) / len(rows),
        "mean_query_latency_ms": sum(r["query_latency_ms"] for r in rows) / len(rows),
        "mean_margin": sum(r["margin"] for r in rows) / len(rows),
    }


def main():
    torch.set_num_threads(min(8, os.cpu_count() or 1))
    print("loading Jina v5-omni-nano (vision + text)...", flush=True)
    t0 = time.perf_counter()
    model = AutoModel.from_pretrained(
        MODEL, trust_remote_code=True, local_files_only=True,
        default_task="retrieval", modality="vision", dtype=torch.float32,
    ).eval()
    proc = AutoProcessor.from_pretrained(MODEL, trust_remote_code=True, local_files_only=True)
    load_s = time.perf_counter() - t0
    docs = load_docs()
    print(f"loaded model in {load_s:.2f}s, embedding {len(docs)} docs", flush=True)
    t0 = time.perf_counter()
    doc_vecs = embed_text(model, proc, [d["text"] for d in docs], "document", batch_size=4)
    doc_embed_s = time.perf_counter() - t0
    text_rows = measure_text(docs, doc_vecs, model, proc)

    images = load_images()
    t0 = time.perf_counter()
    image_vecs = np.stack([embed_image(model, proc, img) for _, img in images])
    image_embed_s = time.perf_counter() - t0
    media_rows = measure_media(images, image_vecs, model, proc)

    report = {
        "model": "jina-v5-omni-nano", "dimension": int(doc_vecs.shape[1]),
        "model_load_seconds": load_s, "documents": len(docs),
        "document_embedding_seconds": doc_embed_s,
        "document_embedding_ms_per_doc": doc_embed_s * 1000 / len(docs),
        "text_retrieval": text_rows, "text_summary": summary(text_rows),
        "images": len(images), "image_embedding_seconds": image_embed_s,
        "image_embedding_ms_per_image": image_embed_s * 1000 / len(images),
        "media_retrieval": media_rows,
        "media_summary": {
            "queries": len(media_rows),
            "hit_at_1": sum(r["rank"] == 1 for r in media_rows) / len(media_rows),
            "mrr": sum(1.0 / r["rank"] for r in media_rows) / len(media_rows),
            "mean_margin": sum(r["margin"] for r in media_rows) / len(media_rows),
            "mean_query_latency_ms": sum(r["query_latency_ms"] for r in media_rows) / len(media_rows),
        },
    }
    Path("/tmp/homeagent-jina-benchmark.json").write_text(json.dumps(report, ensure_ascii=False, indent=2), encoding="utf-8")
    print(json.dumps({k: report[k] for k in ["model", "dimension", "model_load_seconds", "documents", "document_embedding_seconds", "document_embedding_ms_per_doc", "text_summary", "images", "image_embedding_seconds", "image_embedding_ms_per_image", "media_summary"]}, ensure_ascii=False, indent=2))
    for r in text_rows:
        print(f"TEXT {r['name']:20s} rank={r['rank']:3d} margin={r['margin']:+.4f} latency={r['query_latency_ms']:.1f}ms")
    for r in media_rows:
        print(f"MEDIA {r['name']:20s} rank={r['rank']} pos={r['positive']:+.4f} neg={r['best_negative']:+.4f} margin={r['margin']:+.4f}")


if __name__ == "__main__":
    main()
