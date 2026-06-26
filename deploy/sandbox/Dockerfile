# deploy/sandbox/Dockerfile
# Builds the manyforge/opencode-sandbox image.
# opencode is a Go binary distributed as a release tarball from GitHub.
# The entrypoint runs opencode non-interactively over /work and writes
# structured JSON findings to /out/review.json.

FROM alpine:3.20 AS downloader

ARG OPENCODE_VERSION=0.0.55
RUN apk add --no-cache curl tar ca-certificates && \
    curl -fsSL \
      "https://github.com/opencode-ai/opencode/releases/download/v${OPENCODE_VERSION}/opencode-linux-x86_64.tar.gz" \
      -o /tmp/opencode.tar.gz && \
    tar -xzf /tmp/opencode.tar.gz -C /tmp && \
    mv /tmp/opencode /usr/local/bin/opencode && \
    chmod +x /usr/local/bin/opencode

# ── runtime image ──────────────────────────────────────────────────────────────
FROM alpine:3.20

# ripgrep + fzf: opencode warns and degrades search without them. git for the
# checkout copy / any VCS-aware tooling. ca-certificates for the TLS egress.
RUN apk add --no-cache ca-certificates git ripgrep fzf

# Copy the opencode binary from the downloader stage.
COPY --from=downloader /usr/local/bin/opencode /usr/local/bin/opencode

# Entrypoint generates the opencode-ai config at runtime (provider/key/model from
# the injected LLM_* env — opencode-ai v0.0.55 does NOT do {env:} substitution) and
# invokes opencode, writing /out/review.json.
COPY deploy/sandbox/entrypoint.sh /usr/local/bin/review
RUN chmod +x /usr/local/bin/review

ENTRYPOINT ["/usr/local/bin/review"]
