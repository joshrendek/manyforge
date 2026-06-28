# Local-model code-review eval — results

Fixture: 4 planted issues (nil-deref, ignored-error, hardcoded-secret, unbounded-input) + 1 clean file.
Method: direct Ollama /v1 review with files inline (opencode's agent loop is unusable for small local models — see run.sh header).
Scored by the real coding.ParseFindings. Host: native Ollama (Metal).

| Model | Parses? | Issues caught | Latency | Findings | Notes |
|-------|---------|---------------|---------|----------|-------|
| `qwen2.5-coder:7b` | yes | 4/4 | 13s | 4 |  |
| `qwen2.5-coder:14b` | yes | 4/4 | 23s | 4 |  |
| `gemma3:12b` | yes | 4/4 | 27s | 4 |  |
| `qwen2.5-coder:32b` | yes | 4/4 | 59s | 4 |  |
