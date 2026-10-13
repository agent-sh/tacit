#!/usr/bin/env python3
"""Dense retrieval for the delivery check: EmbeddingGemma 2 vs idf vs fusion.

Embeds every fact in the store (title + description + body head) once and
every truth situation as a query, then reports hit@40 for three rankers over
the same eligible set delivery.py uses:

  idf     term overlap weighted by idf (delivery.rank)
  dense   cosine over EmbeddingGemma 2 at 256 dims
  rrf     reciprocal rank fusion of the two (k=60)

CPU by default (TACIT_EMBED_DEVICE=cuda to change); 270M text backbone.
Needs numpy and sentence-transformers in the interpreter that runs it.

  CUDA_VISIBLE_DEVICES= python3 eval/delivery/embed_rank.py
  TACIT_STORE=~/.cache/tacit-curated python3 eval/delivery/embed_rank.py   (curated store)

Same inputs as delivery.py, from the environment: TACIT_TRUTH, TACIT_STORE,
TACIT_HOST, TACIT_IMPORT_HOST; plus TACIT_EMBED_MODEL, TACIT_EMBED_DIM.
"""
import hashlib
import json
import math
import os
import sys
from collections import Counter, defaultdict
from pathlib import Path

import numpy as np

sys.path.insert(0, str(Path(__file__).resolve().parent))
import delivery  # noqa: E402

MODEL = os.environ.get("TACIT_EMBED_MODEL", "google/embeddinggemma-2")
DIM = int(os.environ.get("TACIT_EMBED_DIM", "256"))
CACHE = Path.home() / ".cache/tacit-embed"
CAP = 40


def doc_text(f):
    body = " ".join(f["body"].split())[:800]
    return f"title: {f['name']} | text: {f['description']} {body}"


def load_model():
    from sentence_transformers import SentenceTransformer

    kwargs = {}
    if "embeddinggemma-2" in MODEL:
        kwargs["config_kwargs"] = {"vision_config": None, "audio_config": None}
    return SentenceTransformer(MODEL, device=os.environ.get("TACIT_EMBED_DEVICE", "cpu"), **kwargs)


def embed_docs(model, facts):
    paths = sorted(facts)
    key = hashlib.sha256((MODEL + str(DIM) + "\n".join(p + "\n" + doc_text(facts[p]) for p in paths)).encode()).hexdigest()[:16]
    CACHE.mkdir(parents=True, exist_ok=True)
    cache = CACHE / f"{key}.npy"
    if cache.exists():
        return paths, np.load(cache)
    embs = model.encode([doc_text(facts[p]) for p in paths], batch_size=16, truncate_dim=DIM, normalize_embeddings=True, show_progress_bar=False)
    np.save(cache, embs)
    return paths, embs


def embed_queries(model, texts):
    return model.encode([f"task: search result | query: {t}" for t in texts], batch_size=16, truncate_dim=DIM, normalize_embeddings=True, show_progress_bar=False)


def rrf(rankings, k=60):
    score = defaultdict(float)
    for r in rankings:
        for i, p in enumerate(r):
            score[p] += 1.0 / (k + i + 1)
    return [p for p, _ in sorted(score.items(), key=lambda x: -x[1])]


def main():
    truth = delivery.load_truth()
    facts = delivery.load_store()
    by_origin = defaultdict(set)
    for f in facts.values():
        if f["origin"]:
            by_origin[f["origin"]].add(f["path"])
    curated = bool(by_origin)
    by_scope = defaultdict(list)
    for f in facts.values():
        by_scope[f["scope"]].append(f)
    df = Counter()
    for f in facts.values():
        for t in set(delivery.terms(f["name"] + " " + f["description"] + " " + f["body"])):
            df[t] += 1

    stored = []
    for it in truth:
        em = it.get("existing_memory")
        if not em:
            continue
        ip = delivery.imported_path(em)
        if curated:
            targets = by_origin.get(Path(ip).stem if ip else None, set())
        else:
            targets = {ip} if ip and ip in facts else set()
        if targets:
            stored.append((it, targets))

    print(f"store {delivery.STORE}: {len(facts)} facts, curated={curated}, situations {len(stored)}; model {MODEL} @ {DIM}d on cpu")
    model = load_model()
    paths, embs = embed_docs(model, facts)
    idx = {p: i for i, p in enumerate(paths)}
    qtexts = [delivery.situation_text(it, False) for it, _ in stored]
    qembs = embed_queries(model, qtexts)

    hits = Counter()
    per_mode = defaultdict(Counter)
    for (it, targets), q in zip(stored, qembs):
        repo = (it["situation"].get("repo") or "").lower()
        eligible = [f for s in delivery.eligible_scopes(repo) for f in by_scope.get(s, [])]
        if not eligible:
            continue
        m = it["mode"]
        per_mode[m]["n"] += 1
        idf_rank = delivery.rank(eligible, delivery.terms(delivery.situation_text(it, False)), df, len(facts))
        el_paths = [f["path"] for f in eligible]
        sims = embs[[idx[p] for p in el_paths]] @ q
        dense_rank = [el_paths[i] for i in np.argsort(-sims)]
        fused = rrf([idf_rank, dense_rank])
        for label, ranking in (("idf", idf_rank), ("dense", dense_rank), ("rrf", fused)):
            pos = next((i for i, p in enumerate(ranking) if p in targets), None)
            if pos is not None and pos < CAP:
                hits[label] += 1
                per_mode[m][label] += 1
    n = sum(per_mode[m]["n"] for m in per_mode)
    for label in ("idf", "dense", "rrf"):
        print(f"{label:6s} hit@{CAP} {hits[label]:3d}/{n} = {hits[label]/n:.2f}   " + "  ".join(f"{m}: {per_mode[m][label]}/{per_mode[m]['n']}" for m in sorted(per_mode)))


if __name__ == "__main__":
    main()
