# Local-model code-review eval — results

Fixture: 4 planted issues (nil-deref, ignored-error, hardcoded-secret, unbounded-input) + 1 clean file.
Method: direct Ollama /v1 review with files inline (opencode's agent loop is unusable for small local models — see run.sh header and the design doc). Scored by the real `coding.ParseFindings`. Host: native Ollama (Metal, 64 GB Apple Silicon). Run: 2026-06-27.

| Model | Parses? | Issues caught | Latency | Findings | Notes |
|-------|---------|---------------|---------|----------|-------|
| `qwen2.5-coder:7b`  | yes | 4/4 | 15s | 5 | fastest; 1 over-report |
| `qwen2.5-coder:14b` | yes | 4/4 | 23s | 4 | precise (exactly the 4); **recommended** |
| `gemma3:12b`        | yes | 4/4 | 22s | 4 | strong general-model alternative |
| `qwen2.5-coder:32b` | yes | 4/4 | 40s | 4 | no quality gain over 14b; slower |

All four catch every planted issue with correct severities (after the
`ParseFindings` severity-normalization fix — 7b/32b initially emitted `" warning"`
/ `"high"`). Re-run with `./run.sh` (optionally `./run.sh "qwen2.5-coder:14b"`).
