# Choosing what each prompt sees

On every prompt, the hook finds up to six curated facts and one computed fact that match the request. By default a lexical score and a floor decide which of them reach the agent. The optional utility selector asks a model instead: for each candidate, does this memory add something useful for this request, beyond what the request already says? Topic overlap alone is not enough.

Code decides first. Scope, repository, lifecycle and the per-session limits stay in code, and a prompt that needs no memory (a text operation on a literal it supplies) gets no facts and no model call. The model only judges the rest. If it is slow, misconfigured or answers badly, the local selection is used.

## Recommendation

- **Jev** (TypeSafe, hosted). The most accurate and fastest option we measured. It is the default provider.
- **Amazon Nova Micro on Bedrock**, when the data should stay in your AWS account. It never selected a wrong fact in our runs, but it kept fewer useful ones than Jev (2 of 9 on the development prompts, 4 of 6 on the sealed test; Jev 8 and 5), and it was about 0.3 s slower. If 1.5-2 s per prompt is acceptable, Claude Sonnet 5.5 or Haiku 5.5 on Bedrock matched Jev on the development prompts (they were not run on the sealed test).

Any other model can be plugged in: a System One-compatible server, any Bedrock model, or any OpenAI-compatible chat endpoint. Pick its threshold on your own labelled prompts (see [Measure your own](#measure-your-own)).

## Configure

Put this in `~/.config/tacit/config.yaml`. Keys belong in the environment or in a private dotenv `key_file`, never in YAML. Only the provider's own key variable is read from that file, and the key is sent only to that provider's endpoint.

Jev:

```yaml
utility:
  provider: jev            # model jev-1.13.0, endpoint api.typesafe.ai (pinned)
  threshold: 0.85          # default 0.70; 0.85 was best on our calibration set
  key_file: ~/.config/tacit/typesafe.env   # TYPESAFE_API_KEY=...
  audit_dir: ~/.local/state/tacit/utility  # optional private receipts
```

Amazon Bedrock (any model ID; uses a Bedrock API key):

```yaml
utility:
  provider: bedrock
  model: us.amazon.nova-micro-v1:0
  region: us-east-1        # default
  threshold: 0.95          # required for every non-Jev provider
  key_file: ~/.config/tacit/bedrock.env    # AWS_BEARER_TOKEN_BEDROCK=...
```

A self-hosted System One server, for example `strands-decider serve` or a d3 model server:

```yaml
utility:
  provider: systemone
  endpoint: http://127.0.0.1:8000/v1/systemone
  model: ""                # optional; sent when set
  threshold: 0.80
  key_file: ./endpoint.env # optional: TACIT_UTILITY_API_KEY=...
```

An OpenAI-compatible chat endpoint (OpenAI, OpenRouter, Ollama, vLLM and similar):

```yaml
utility:
  provider: openai
  endpoint: https://openrouter.ai/api/v1   # base URL; /chat/completions is appended
  model: your-model-id
  threshold: 0.90
  key_file: ./endpoint.env # TACIT_UTILITY_API_KEY=...
```

Endpoints must use HTTPS, except a server on `localhost`, `127.0.0.1` or `::1`. System One providers return P(true) for each candidate. Bedrock and OpenAI-compatible models score each candidate 0-100 against the same rubric in one reply; tacit divides by 100. Scores from different models are not on one scale, so every provider except Jev needs its own `threshold`. The full reference is in [Local and portable setup](portable.md#optional-utility-selector).

## What we measured (2026-10)

One owner's private store and real prompts. The data is private; the method is in [eval/JEV.md](../eval/JEV.md) and [eval/DELIVERY.md](../eval/DELIVERY.md), and the tools are in `eval/cpu` and `eval/selection`. Each prompt has a frozen pool of up to 6 curated and 1 computed candidate. Labels mark which candidates are useful and which are wrong. A threshold was picked on the calibration prompts (12), then applied unchanged to the development prompts (19). The sealed test prompts (20) were opened once, after every choice was frozen. Latency is the median round trip for one prompt, from one laptop on a corporate network, so network time is included.

Development prompts, useful / wrong facts selected:

| Selector | Useful | Wrong | Median latency |
|---|---|---|---|
| Today's lexical selection | 9 | 8 | (local) |
| **Jev** | 8 | 0 | 0.41 s |
| **Amazon Nova Micro** (Bedrock) | 2 | 0 | 0.69 s |
| Claude Sonnet 5.5 / Haiku 5.5 (Bedrock) | 9 | 0 | 1.5 / 2.0 s |
| GPT-6 Luna (Bedrock) | 9 | 1 | 1.3 s |
| Amazon Nova 2 Lite (Bedrock) | 9 | 1 | 1.4 s |
| Cohere Rerank 3.5 / Amazon Rerank 1.0 (Bedrock) | 0-2 | 0-2 | 0.44 / 0.58 s |
| Local CPU cross-encoders (ettin 68M, gte-modernbert) | 2-3 | 0-1 | 0.07-0.12 s |
| Strands Decider 2B, d1-3B (local, laptop GPU) | 3-7 | 0-8 | 1.6-2.9 s (21 s on CPU) |

Sealed test prompts, with the thresholds frozen beforehand (6 useful facts were findable):

| Selector | Useful | Wrong |
|---|---|---|
| Today's lexical selection | 5 | 2 |
| **Jev** | **5** | **0** |
| **Amazon Nova Micro** | 4 | 0 |

What this means:

- Jev keeps the useful facts and drops the wrong ones. Nova Micro drops the wrong ones too but misses more useful ones. The large Bedrock models are as accurate as Jev, but 1.3-2 s per prompt is close to the hook's 2 s budget.
- Rerankers score topical relevance, which is exactly the "on topic but not helpful" case, so they keep little.
- No model small enough to run in milliseconds on an ordinary CPU was accurate enough. Decision models such as Strands Decider need a GPU to be fast; d3-mini (4.5B) was too slow to finish on a laptop CPU.
- Code rules: four were tried in front of the models. Only the "no memory needed" rule held on the sealed test and ships. Rules that restricted by repository or file mentions removed useful facts on new prompts ([eval/DELIVERY.md](../eval/DELIVERY.md#code-before-the-scorer-2026-10-17)).
- The sets are small: one or two facts is within noise. Treat the tables as a ranking, not a guarantee for your store.

Cost per million prompts, from the tokens these runs used and the published prices in October 2026: Jev about $40 (941 input tokens per prompt, $0.042 per million input tokens, output free); Nova Micro about $23 (474 input and 43 output tokens; $0.035 and $0.14 per million). Both are a few dollars a month for a busy user.

## Measure your own

Label a few dozen of your own prompts, freeze the candidate pools with `eval/selection/harness.py retrieve`, score them with your model, and pick a threshold on a calibration split with `eval/cpu/qwen/evaluate_jev.py`. Keep a sealed test split and open it once. Never put prompts, facts or labels in this repository.
