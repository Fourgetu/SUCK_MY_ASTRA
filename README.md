# New Label, Old Model: Measured Evidence of Model Mislabeling and Silent Substitution in OpenAI Codex

**Date: 2026-09-29 · Status: sanitized (see the Sanitization note)**
**Method: direct probing of the private `chatgpt.com/backend-api/codex/*`
endpoints, corroborated by the official `openai/codex` source (commit
`6a39914e`) and public issue reports.**

## Executive summary

Three independently established findings together form a complete picture of
"new model, old substance":

1. **Mislabeling (measured).** Models that shipped under the names
   `gpt-6-sol` and `gpt-6-luna` — the "GPT-6 family" — carry a **GPT-5
   generation** fingerprint in their instruction templates. The upstream then
   hard-renamed them to `gpt-5.6-sol` / `gpt-5.6-luna`; the catalog
   descriptions now literally read "Older coding model (formerly
   gpt-6-sol)", which is an implicit admission that the "6" branding never
   matched the generation.
2. **Silent substitution (measured mechanism + community corroboration).**
   A request for the flagship `gpt-6-astra` comes back with response headers
   unilaterally declaring a safety-buffering fallback chain whose terminal
   node is `gpt-5.6-luna` — a 5-series "older fast model". The substitution
   event surfaces only as `response.model` being rewritten inside the
   stream; **no client-side knob can disable it**, and the official source
   confirms the consequence is a once-per-turn warning — not a refusal, not
   a retry, not user-facing disclosure.
3. **A hidden budget tier (measured).** The catalog contains
   `gpt-reserve` (`visibility: hide`, "Fast and affordable agentic
   coding") — also 5-series at the base. A model whose name carries no
   generation number is in fact the 5-series cheap tier, invisible in the
   client list by default.

**In one sentence:** of the seven slugs on sale, only `gpt-6-astra` is a
real GPT-6; every other "6-family" name is a 5-series rebrand — and even
the one genuine 6 can be silently swapped for a 5-series luna entirely at
the upstream's discretion.

## Evidence A — the naming layer: "6-series" names on a 5-series base

Source: `GET /backend-api/codex/models?client_version=0.154.0`
(measured 2026-09-29; requires `Authorization` + `Chatgpt-Account-Id`).

### A.1 The live catalog (every slug minted 200)

| slug | display name | official description | visibility | base fingerprint |
| --- | --- | --- | --- | --- |
| `gpt-6-astra` | GPT-6-Astra | Frontier intelligence, flagship | list | **GPT-6** |
| `gpt-reserve` | GPT-Reserve | Fast and affordable agentic coding | **hide** | GPT-5 |
| `gpt-5.6-sol` | GPT-5.6-Sol | Older coding model (**formerly gpt-6-sol**) | list | GPT-5 |
| `gpt-5.6-terra` | GPT-5.6-Terra | Older balanced model | list | GPT-5 |
| `gpt-5.6-luna` | GPT-5.6-Luna | Older fast model (**formerly gpt-6-luna**) | list | GPT-5 |
| `gpt-5.5` | GPT-5.5 | Legacy coding model | list | GPT-5 |
| `codex-auto-review` | Codex Auto Review | review model | hide | GPT-5 |

Generation fingerprints come from each slug's `instructions_template` and
internal fields:

- `comp_hash`: `3000` across the new family vs `2911` for `gpt-5.5` — an
  instruction/compatibility generation marker; the 5.6 series shares
  astra's hash but sits on a 5-series instruction base.
- `multi_agent_version`: astra/sol/terra = v2; reserve/luna/auto-review = v1.
- `use_responses_lite`: true across the new family, false for `gpt-5.5`;
  `tool_mode` likewise — `code_mode_only` for the new family, null for
  `gpt-5.5`.
- `supported_reasoning_levels`: astra and sol/terra reach `ultra`;
  the rest stop at `max`.

### A.2 The rename was a hard cutover with no parallel window

| dead name (now HTTP 400) | current name |
| --- | --- |
| `gpt-6-sol` | `gpt-5.6-sol` |
| `gpt-6-luna` | `gpt-5.6-luna` |
| `gpt-6` / `gpt-6-codex` / `gpt-6.1-sol` / `gpt-6-mini` | undefined, never existed |

Measured `minimal_client_version` gating: 0.153.0+ sees all seven;
0.144–0.152.x sees six (no astra); older versions narrow stepwise to none.
**No client version can ever see `gpt-6-sol` / `gpt-6-luna`** — old clients
are not shown the old names, they are locked out of the new catalog
entirely. Dead names return a fatal 400 with no alias rewriting.

### A.3 The retirement chain confirms it

`gpt-5.5` carries `retirement_at=2026-10-14`, and its official migration
target is `gpt-5.6-sol` — i.e., the model once sold under a "6" name is the
designated successor for the genuinely old 5.5. The naming system
internally admits the 5.6 series belongs to the 5-series generation.

## Evidence B — the routing layer: flagship request, budget model serves

Sources: direct measurement 2026-09-22/27 + official codex-rs source
(commit `6a39914e`).

### B.1 The fallback chain is declared on every healthy response

Responses carry `x-codex-safety-buffering-enabled: true` and
`x-codex-safety-buffering-faster-model: <model>`; the measured map:

| requested | buffering headers | fallback sink |
| --- | --- | --- |
| `gpt-6-astra` (flagship) | present | **`gpt-5.6-luna`** |
| `gpt-5.6-sol` | present | `gpt-5.6-luna` |
| `gpt-5.6-terra` | present | `gpt-5.6-luna` |
| `gpt-6-sol` (pre-rename) | present | `gpt-6-luna` |
| `gpt-5.5` / `gpt-5.6-luna` / `gpt-6-luna` | absent | — (luna is the floor) |

**Every chain ends at a luna**: whether the request asks for flagship or
balanced, degradation converges on the "older fast model". The headers only
announce eligibility; the event itself is `response.model` in the SSE
payload declaring the fallback (the official client separately reads the
`openai-model` / `x-openai-model` headers as the served-model signal).

### B.2 No client knob can disable it (measured negative)

Roughly fifty probes on a healthy account covered: 7 models × 6 gateway
nodes (unified-125/95/88/157/97/179, steered via replayed routing-cookie
pairs) × varied payloads × tools × high/xhigh effort × serial and parallel
bursts × security-review prompts. Every request-side lever was tested:

- `x-codex-safety-buffering-enabled: false` as a request header: no effect,
  the chain is still declared;
- `service_tier`: `default`/`priority` accepted but the headers persist;
  `auto`/`flex` rejected outright with 400;
- `store:true`: rejected (`Store must be set to false`);
- `x-codex-routing-hint: model=X`: a pure hint, does not alter routing;
- `previous_response_id`: rejected (`Unsupported parameter`) — continuations
  ride on the turn-state header alone, and there is no mid-turn model escape.

### B.3 Official source: a mismatch earns a warning, not a refusal

- `response_model()` reads only the `openai-model`/`x-openai-model`
  headers; on mismatch the consequence is a once-per-turn `ModelReroute`
  **warning** — output is delivered as usual.
- Model comparison is `to_ascii_lowercase` only, with no snapshot
  normalization: `gpt-6-astra-2026-09-15 ≠ gpt-6-astra`.
- `retry_model`/safety_buffering is a *candidate retry model* semantic, not
  evidence a swap already happened; but `turn/steer` input inherits
  whatever model the original request negotiated — **a turn that opened on
  a buffered node keeps feeding user input into the degraded service**,
  with no mid-turn escape.
- **No strict model-selection contract exists** anywhere in the official
  catalog or protocol.

### B.4 Community and third-party corroboration (degradation is live, not hypothetical)

- `openai/codex#47015` (2026-09-21): on the OAuth subscription route, a
  `gpt-6-astra` request returned `gpt-5.6-luna` metadata; the community
  formally demanded a strict model-selection contract — none exists.
- `openai/codex#47765` (2026-09-24): the "Codex severely dumbed down"
  thread is still active.
- `icoretech/codex-pooler#425`: cites a Reddit case where two of five Pro
  accounts were **persistently** degraded — degradation can stick per
  account.
- `wangyunjeff/sub2api-state-kit#16`: independent measurement — without a
  pinned routing-cookie pair the request comes back served by Luna, while
  the same request with the pair returns Astra. **Default routing lands a
  flagship request on luna**; which generation the user actually gets is
  decided by routing credentials, not by the model name.

## Evidence C — visibility and naming smoke screens (supporting)

- `available_access_programs` carries only `cyber: ["standard"]` per model —
  "cyber" is an access-program codename, not a model name; `gpt-cyber` /
  `gpt-6-cyber` requests all return 400. Generation numbers, tier labels,
  and program codenames are interleaved in one namespace, so the real
  generation cannot be inferred from a name.
- An extra `priority` (Fast) tier is advertised: 2x billing for astra,
  1.5x for the rest — **the premium tier rides on the same silently
  substitutable service**.

## Evidence boundaries (measured vs inferred)

| claim | status |
| --- | --- |
| 7-slug catalog, `served == requested` per slug, 5-series base for the 5.6 line, hard rename, no parallel window | **measured** (2026-09-29) |
| buffering fallback declarations, no client opt-out, steer inheritance, warn-only upstream behavior | **measured + official source** |
| substitution events actually occurring on user traffic (flagship → luna) | **community / third-party corroboration**; our ~50 probes on healthy accounts were all negative — no live swap captured |
| "5.6 = former gpt-6 renamed" is the upstream's own wording; deliberate *intent* to deceive | **fact holds; intent is inference** — but misleading naming + a silent substitution mechanism + no opt-out compound into a deceptive effect regardless of intent |
| comp_hash / base-template generation classification | our classification scheme, not an official statement |

## Reproduction (sanitized)

```bash
# Catalog enumeration (any valid ChatGPT-login credential)
curl -sS "https://chatgpt.com/backend-api/codex/models?client_version=0.154.0" \
  -H "Authorization: Bearer $ACCESS_TOKEN" \
  -H "Chatgpt-Account-Id: $ACCOUNT_ID" \
  -H "User-Agent: codex-tui/0.154.0" | jq -r '.models[].slug'

# Fallback declaration: send any legal streaming request to
# /backend-api/codex/responses and observe
# x-codex-safety-buffering-{enabled,faster-model} and whether
# response.created.response.model is rewritten mid-stream.
```

## Sanitization note

- Test accounts are referred to by plan tier only (`prolite` /
  `self_serve_business_prolite` / `pro` / `team`); emails, credential IDs,
  phone numbers, and Apple relay addresses are removed;
- no `access_token` / `id_token` / `refresh_token` / `account_id` / session
  UUID appears anywhere — `$ACCESS_TOKEN` / `$ACCOUNT_ID` are placeholders;
- gateway node IDs (unified-N) and edge colo codes are upstream
  infrastructure identifiers that do not identify any user; they are kept
  for reproducibility.
