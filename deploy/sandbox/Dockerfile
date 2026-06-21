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

RUN apk add --no-cache ca-certificates git

# Copy the opencode binary from the downloader stage.
COPY --from=downloader /usr/local/bin/opencode /usr/local/bin/opencode

# Provider config: LLM_API_KEY, LLM_BASE_URL, LLM_MODEL are substituted at
# runtime via opencode's {env:VAR} interpolation.
COPY deploy/sandbox/opencode.json /etc/opencode/opencode.json

# Entrypoint that invokes opencode and writes /out/review.json.
COPY deploy/sandbox/entrypoint.sh /usr/local/bin/review
RUN chmod +x /usr/local/bin/review

# Tell opencode where to find its config (overrides the default ~/.config/opencode/).
ENV OPENCODE_CONFIG=/etc/opencode/opencode.json

ENTRYPOINT ["/usr/local/bin/review"]
