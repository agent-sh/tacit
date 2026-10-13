#!/usr/bin/env python3
"""Hindsight's recall vs tacit's idf ranker on the delivery check.

Loads the curated store into a Hindsight bank verbatim (no LLM: extraction mode
"chunks", one document per fact, the fact's scope as its tag), then for every
truth situation asks Hindsight's recall (semantic + BM25 + rerank) with the
same situation text tacit's ranker gets, restricted by tag to the same eligible
scopes. Reports hit@10 and hit@40 for both rankers.

  HINDSIGHT_API_LLM_PROVIDER=none HINDSIGHT_API_RETAIN_EXTRACTION_MODE=chunks \
    hindsight-api --host 127.0.0.1 --port 8899 &
  TACIT_STORE=~/.cache/tacit-curated python3 eval/delivery/hindsight_rank.py

Run it with an interpreter that has hindsight_client installed. Same inputs as
delivery.py, from the environment (TACIT_TRUTH, TACIT_STORE, TACIT_HOST,
TACIT_IMPORT_HOST), plus HINDSIGHT_URL, HINDSIGHT_BANK, HS_LIMIT, HS_OUT.
"""
import hashlib
import json
import os
import sys
import time
from collections import Counter, defaultdict
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import delivery  # noqa: E402
from hindsight_client import Hindsight  # noqa: E402

URL = os.environ.get("HINDSIGHT_URL", "http://127.0.0.1:8899")
BANK = os.environ.get("HINDSIGHT_BANK", "tacit-curated")
CAPS = (10, 40)
LIMIT = int(os.environ.get("HS_LIMIT", "0"))
OUT = Path(os.environ.get("HS_OUT", str(Path.home() / ".cache/hindsight-eval/per-situation.jsonl")))


def doc_text(f):
    return f"{f['name']}: {f['description']}\n{f['body'].strip()}"


def load_bank(client, facts):
    try:
        client.create_bank(BANK, name=BANK, retain_extraction_mode="chunks", enable_observations=False)
    except Exception as exc:  # exists already
        print("bank:", str(exc)[:120])
    # The bank is reused only for the exact store it was loaded from.
    print_ = hashlib.sha256("\n".join(p + "\n" + doc_text(facts[p]) for p in sorted(facts)).encode()).hexdigest()[:16]
    marker = Path.home() / ".cache/hindsight-eval" / f"{BANK}.loaded"
    if marker.exists():
        if marker.read_text().strip() == print_:
            return
        sys.exit(f"bank {BANK} was loaded from another store; delete it and {marker} first")
    paths = sorted(facts)
    for i in range(0, len(paths), 50):
        items = [{"content": doc_text(facts[p]), "document_id": p, "tags": [facts[p]["scope"]]} for p in paths[i:i + 50]]
        client.retain_batch(BANK, items)
        print(f"retained {min(i + 50, len(paths))}/{len(paths)}", flush=True)
    marker.write_text(print_ + "\n")


def recall_rank(client, query, scopes):
    res = client.recall(BANK, query, max_tokens=60000, budget="high", tags=scopes, tags_match="any_strict")
    seen, order = set(), []
    for r in res.results:
        doc = getattr(r, "document_id", None)
        if doc and doc not in seen:
            assert set(getattr(r, "tags", None) or []) & set(scopes), f"{doc} is outside the eligible scopes"
            seen.add(doc)
            order.append(doc)
    return order


def main():
    truth = delivery.load_truth()
    facts = delivery.load_store()
    by_origin = defaultdict(set)
    for f in facts.values():
        if f["origin"]:
            by_origin[f["origin"]].add(f["path"])
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
        ip = delivery.imported_path(em) if em else None
        targets = by_origin.get(Path(ip).stem if ip else None, set())
        if targets:
            stored.append((it, targets))
    if LIMIT:
        stored = stored[:LIMIT]
    client = Hindsight(base_url=URL)
    load_bank(client, facts)
    print(f"store {delivery.STORE}: {len(facts)} facts, situations {len(stored)}")

    hits = Counter()
    n = 0
    took = []
    out = OUT.open("w")
    for it, targets in stored:
        repo = (it["situation"].get("repo") or "").lower()
        scopes = delivery.eligible_scopes(repo)
        eligible = [f for s in scopes for f in by_scope.get(s, [])]
        if not eligible:
            continue
        n += 1
        q = delivery.situation_text(it, False)
        rankings = {
            "idf": delivery.rank(eligible, delivery.terms(q), df, len(facts)),
        }
        t0 = time.monotonic()
        rankings["hindsight"] = recall_rank(client, q, scopes)
        took.append(time.monotonic() - t0)
        rec = {"id": it.get("id"), "query": q, "seconds": round(took[-1], 1)}
        for label, ranking in rankings.items():
            pos = next((i for i, p in enumerate(ranking) if p in targets), None)
            rec[label] = pos
            for cap in CAPS:
                if pos is not None and pos < cap:
                    hits[(label, cap)] += 1
        out.write(json.dumps(rec) + "\n")
        out.flush()
    out.close()
    if n == 0:
        sys.exit("no situation has a target in this store (a raw store has no origin links)")
    took.sort()
    print(f"hindsight recall seconds: median {took[len(took) // 2]:.1f}, max {took[-1]:.1f}")
    for label in ("idf", "hindsight"):
        print(f"{label:10s} " + "  ".join(f"hit@{cap} {hits[(label, cap)]:3d}/{n} = {hits[(label, cap)] / n:.2f}" for cap in CAPS))


if __name__ == "__main__":
    main()
