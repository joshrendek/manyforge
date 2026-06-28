# Planted issues (ground truth for scoring)

`service.go` contains four deliberately planted issues. `util.go` is clean (noise).

| # | Location | Issue | Keywords a good finding hits |
|---|----------|-------|------------------------------|
| 1 | `service.go` `Greet` (~line 35) | nil-pointer dereference — `findUser` may return nil, `u.Name` deref'd unconditionally | nil, panic, dereference, not found |
| 2 | `service.go` `statusHandler` (~line 44) | ignored error from `w.Write` | error, ignored, unchecked, Write |
| 3 | `service.go` `stripeKey` (~line 12) | hardcoded credential / secret in source | hardcoded, secret, credential, key |
| 4 | `service.go` `uploadHandler` (~line 52) | unbounded allocation from attacker-controlled Content-Length | DoS, unbounded, Content-Length, memory, size limit |

A model scores X/4 by surfacing a finding that clearly maps to each planted issue
(by file+approximate line and/or keywords). `util.go` findings are over-reports.
