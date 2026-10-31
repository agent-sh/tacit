# Hosted utility selection with Jev

The optional Jev selector reduced unrelated memory injections while preserving the baseline's reviewed useful items at a moderate threshold. It is opt-in because it sends redacted task text and candidate summaries to a paid external API.

## Method

- Freeze retrieval candidates from the existing scoped search. Do not use labels to retrieve, rewrite queries, or score candidates.
- Score the printed `Name + Description`, not the body. Share the task as state and put each memory in a structured question.
- Pin `jev-1.13.0`. Compare bare Noul, a Noul rubric, and three-way Choice. Choose the rubric using development data at a fixed 0.50 threshold before threshold selection.
- Keep episode-disjoint development and calibration splits. Three context arms give three scorer records per query: count queries, not records, as independent observations.
- Read usefulness labels only after scoring. Keep primary labels separate from a blinded assistant review of summary usefulness. The review is subjective, not human ground truth. Leave uncertain and unlisted items out of precision/recall and report them separately.
- Measure three repeats on every unique nonempty payload. Evaluate stored scores without new API calls. Reproduce the frozen baseline's counts before trusting the evaluator. Keep held-out cases sealed.

## Calibration result

Original-query candidate pools, calibration queries (measured on a real store; the counts stay with the owner):

| Selector | Reviewed useful retained | Reviewed incorrect retained | Uncertain retained |
|---|---|---|---|
| Existing local selection | all in the pools | many | none |
| Jev rubric, 0.70 | all in the pools | few | almost none |
| Jev rubric, 0.86 | about half | none | none |

The moderate point keeps every reviewed useful item the baseline keeps and cuts the known incorrect items sharply. Other reviewed useful items are absent from the original-query candidate pools. The precision-heavy threshold drops useful items as well; a zero-error count over a handful of items is not a deployment guarantee.

The 0.70 threshold is an exploratory operating point chosen after inspecting calibration curves, not a held-out result. Remaining errors include topical instructions that do not change the requested action. Do not describe the model's Noul values as domain-calibrated confidence. These results do not establish downstream task success.

## Latency and cost

The hosted scorer's HTTP round trip was 300 ms p50 and 375 ms p95, with all three prompt arms evaluated together and empty candidate sets excluded. At the published input-token rate the calls' token cost was negligible. This is not end-to-end hook latency or a billing receipt. The production Go path uses only the rubric and adds scrump redaction. An initial one-second deadline timed out in most live hook probes; the successful probe spent 725 ms in utility selection. The default was therefore raised to two seconds, retaining bounded fallback and leaving room in the integration's four-second prompt timeout. Review actual hook receipts, not only isolated HTTP timings.

## Production contract and review

Use the existing scope, applicability, lifecycle and printable-fact guards before Jev. Jev replaces only the score floor and order for that candidate pool. Keep the three-curated/one-derived limits and post-cap session deduplication without backfill. A timeout or error uses the original selector, not a partial response. Redaction failure prohibits external submission and falls back locally. Do not follow redirects with credentials.

Opt-in private audit receipts record redacted task/summary text, scores, selected flags, model, threshold, duration and fallback status. Review false positives, dropped useful facts and fallback rates after the store grows. Freeze each subsequent model/prompt/threshold revision before a fresh comparison. Never use the held-out set to tune these revisions.

No private prompts, memories, labels, keys or raw outputs belong in this repository.
