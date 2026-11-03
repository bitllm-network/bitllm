# Spot checks: is a peer serving the model it names?

A node shares a model under a network name, and nothing stops it from running something cheaper
under that name: a small model, a coarser quant, another fine-tune. `--audit` lets your node check.
It is off by default, runs only on your side, and changes nothing on the wire (two optional fields
in the announce, which older nodes ignore). A node that turns on neither `--audit` nor
`--audit-serve` behaves exactly as before: it sends no canaries, scores nothing, and announces the
same as v0.1.

```bash
bitllm --audit                           # check the peers that answer you
bitllm --audit --auditor 12D3KooW...     # prefer an auditor you trust (repeatable)
bitllm --audit --auditor none            # no preferred auditor: any node that can score
curl -s 127.0.0.1:7778/audits            # what your node found
```

## How it works

1. **Canary.** When a peer first answers you for a model, and then after every 50 of your requests
   to it (`--audit-every`), your node sends it a canary: an ordinary chat request from a built-in
   pool of 56 prompts, at most 256 new tokens, streamed or not as your own requests are. Half the
   canaries are greedy (temperature 0); the others copy the temperature and top-p of your latest
   real request. At most 6 rounds per peer and model per hour. The canary is paid like any request.
2. **Auditor.** Another node that serves the same model and whose server returns prompt logprobs
   (vLLM) scores the canary's answer: it reads prompt + answer once (teacher forcing) and returns
   the log probability of every answer token and its top 8 alternatives. A node offers this with
   `--audit-serve` (off by default; checked once at start with one tiny request). The
   auditor gets only the text, never which peer wrote it. Your node picks the auditor: first one of
   `--auditor` (by default the maintainers' seed, below), else a random node that can score. Never
   the server itself, a node claimed by the same owner, or one on the same /24 (IPv6: /48).
3. **Score.** From the auditor's numbers:
   - greedy canary: **top1**, the share of answer tokens that are the auditor's top token.
     Bad if top1 < 0.93.
   - sampled canary: **z**, the sum over tokens of (log p(token) + entropy) divided by its standard
     deviation, with p the sampling distribution (temperature, top-p) over the auditor's top 8.
     For an honest sample z is about N(0, 1). Bad if z < -12.
4. **Verdict.** One bad score makes the peer a *suspect*: it goes last in peer selection and gets 4
   more canaries, each scored by another auditor when there is one. Three or more bad follow-ups
   make it *fail*, if the bad scores came from at least two auditors or from one you named with
   `--auditor`: your node then sends that peer no requests for that model for 24 hours, logs the
   numbers, and checks it again afterwards. A suspect with 4 good follow-ups passes; anything in
   between stays a suspect.
5. **Controls.** One check in four also hands the auditor the same answer with its words shuffled.
   An auditor that scores that as fine is not scoring: its verdict is dropped and it is not used for
   24 hours.

Answers with fewer than 40 tokens, and answers that carry reasoning (which the auditor cannot
rebuild), are not judged. `models.json` sets per model what every canary and score carries (e.g.
thinking off, which the scoring template must match) and the fixed sampling of servers that ignore
the client's.

## Weights commitment

`bitllm hash-model DIR` prints the sha256 over the weight files under DIR (configs and tokenizers
left out). A server can state it with `--weights NAME=HASH`; it is announced. When `models.json`
records a hash for a name, clients skip a server that states a different one. This is a claim, not
a proof: the node only proxies its upstream and cannot see what that loads. It catches honest
mistakes (wrong files) and makes a lie explicit. The spot checks are what test the claim.

## Calibration

Scoring by the ring (DeepSeek V4.1 Flash heretic on vLLM) of answers to 40 prompts, 256 tokens
max, per arm; min / median / max per answer:

| answers from | greedy top1 | sampled z (T=1, top-p 0.95) | caught (greedy / sampled) |
|---|---|---|---|
| the ring itself (honest) | 1.000 / 1.000 / 1.000 | -6.9 / 0.1 / 1.7 | 0 / 40, 0 / 40 |
| DeepSeek API, official 1017 weights (fine-tune swap) | 0.824 / 0.969 / 1.000 | -34.8 / -5.9 / 1.1 | 2 / 40, 7 / 40 |
| GLM 5.3 Flash (model swap) | 0.624 / 0.810 / 0.926 | -81.5 / -40.0 / -19.0 | 40 / 40, 40 / 40 |

The template matters: scored without `thinking: false` (so the scoring prompt differed from the
generation prompt by two tokens), honest greedy top1 fell to a median of 0.943 and min 0.727.

Quant swaps, on a small model (Qwen2.5 1.5B Instruct, llama.cpp, q8_0 as the reference, the ring's
honest answers as text):

| served instead of q8_0 | mean KLD | same top token |
|---|---|---|
| q4_k_m | 0.036 | 91.3 % |
| q3_k_m | 0.106 | 85.8 % |
| q2_k | 0.527 | 68.7 % |

The share of the same top token is what a greedy canary measures, so with top1 < 0.93 a Q4-level
swap is flagged on most greedy canaries (91.3 % mean, about ±2 % per 256-token answer) and Q3 or
coarser on nearly all. Under sampling the expected drift of z for q4_k_m is only about -0.6 per
answer, far inside the honest range: that is why half the canaries are greedy. Earlier on the ring,
DeepSeek V4.1 FP8 versus an RTN MXFP4 copy agreed on the top token 91.9 % of the time, the same
range as q4_k_m here.

End to end on a private network (one honest server and one auditor on the ring, a cheater serving
Qwen2.5 1.5B q4_k_m on a CPU under the DeepSeek name, one client with `--audit --audit-every 5`):
the client's first request to the cheater started a round, the first canary was bad (top1 0.808),
4 of 4 follow-ups were bad (top1 0.77 to 0.86, z -30.1), and the cheater failed about 3 minutes
after first use. The client then sent it nothing (15 of 15 requests went to honest servers), while
a client without `--audit` still used it. The honest server passed 6 of 6 rounds (greedy top1 1.000,
sampled z 0.1 and 0.7).

**Still to do:** honest answers from *another* engine or GPU with the same weights (the case the
limits must leave room for) have not been measured; every row above has the same engine on both
sides. That run needs the PC GPUs (Strata on the 3090 against Strata on the 3090 Ti, and against
llama.cpp) and is scheduled once they are free. The limits may move after it.

## What it catches, and what it does not

Catches:
- A server that runs another model under the name, on every request (GLM above: 40 of 40 single
  canaries bad).
- A coarser quant on greedy canaries, from about Q4 down (most single canaries bad at Q4, nearly
  all at Q3 and below).
- Honest mistakes: wrong files, wrong `--model` mapping, a server that switched models (the same
  numbers), and a stated weights hash that differs from the record.

Does not catch:
- **A server that recognizes canaries** and answers them with the real model. The pool is public
  (`canaries.txt`) and canaries ask for 256 tokens at most, so a determined cheater can spot many
  of them. Copying your sampling settings and streaming makes it harder, not impossible.
- **A quant swap on sampled requests only**: under sampling a Q4-level swap does not move z enough.
- **A fine-tune swap** such as official weights for heretic: only 2 of 40 greedy and 7 of 40 sampled
  canaries were bad, so a round rarely ends in a fail.
- **An auditor that lies.** It can pass a cheater (the shuffled controls catch only an auditor that
  passes everything) or make up bad scores for an honest server. Two auditors are needed for a fail
  unless you named one with `--auditor`, so with the default seed one maintainer node could fail a
  server on its own: trust in the seed is trust in its operator.
- **A server and an auditor run by the same person.** The auditor is not told which server wrote an
  answer, and nodes of the same claimed owner or /24 are skipped, but an operator with many
  unclaimed nodes on different networks can still pass their own answers. Phase 2 (signed audit
  reports, reputation per owner account) is the next step against that.
- **Truncated context** (a server that drops the start of a long prompt): canaries are short.
- **Models nobody can score**: a model served only by non-vLLM servers has no auditor, so its
  servers are never judged. Reasoning answers are not judged either.
- Nothing is shared: each client finds out on its own, and a failed server is not banned from the
  network.

## Cost

Measured in the end-to-end run (ring upstream, 20 requests per arm, twice, alternating):

| | `--audit` off | `--audit` on (every 5) |
|---|---|---|
| time to first token, p50 | 0.12 s, 0.12 s | 0.13 s, 0.12 s |
| full answer (200 tokens), p50 | 1.83 s, 1.86 s | 1.82 s, 1.85 s |
| full answer, p95 | 2.13 s, 2.07 s | 3.01 s, 2.66 s |

The check runs after your answer has arrived, so the median does not move. The p95 rose because at
`--audit-every 5` a canary often ran on the same server as your next request; at the default of 50
that is ten times rarer.

- Server: one canary (up to 256 tokens) per 50 requests from each client that audits it: about 2 %
  more requests. A canary took 1.8 to 3.9 s on the ring.
- Auditor: one prefill of prompt + answer (300 to 400 tokens) with logprobs: 0.20 to 0.41 s on
  the ring per score, plus the same again for one check in four (the shuffled control).
- Client: two small HTTP requests over libp2p per check, and up to 20 checks kept per peer.

## Seed auditor (run by us)

So that `--audit` works for a new user without setup, the default `--auditor` is a node that we,
the BitLLM maintainers, run ourselves: the omarchy node
(`12D3KooWSCjSmNqem7jVEwc3scunEjpTqSZPgPdhMx6VbZ5piETF`), which scores
`deepseek-v4.1-flash-1017-heretic` on the 4-node ring. It is used only for the models it announces.
It is not independent: using it means trusting us to score honestly. The node says so in its log
at start and in `GET /audits` (`preferred_auditors`, `seed: true`). Replace it with
`--auditor PEER_ID` (repeatable) or drop it with `--auditor none`; any other node that offers to
score is used after the ones you name.
