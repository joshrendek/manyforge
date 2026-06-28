# deploy/sandbox/Dockerfile
# Builds the manyforge/opencode-sandbox image.
#
# Uses sst/opencode (github.com/sst/opencode, release mirror anomalyco/opencode),
# the maintained successor to the now-ARCHIVED opencode-ai/opencode. opencode-ai's
# final release (v0.0.55) carries a FROZEN model registry, so current OpenRouter
# model slugs 404 ("No endpoints found …") and it silently falls back to a dead
# default model. sst/opencode has a built-in OpenRouter provider with NO frozen
# registry, so any current model the provider serves works (manyforge-ht8).
#
# opencode ships as a self-contained (Bun-compiled) static binary — no node/bun
# runtime is required. We fetch the musl asset to match the Alpine runtime.
#
# The entrypoint runs `opencode run` non-interactively over /work and writes the
# structured JSON findings object to /out/review.json.

FROM alpine:3.20 AS downloader

ARG OPENCODE_VERSION=1.17.11
# TARGETARCH is injected by BuildKit (amd64 | arm64); map to opencode's asset arch
# so the image is native on both Apple-Silicon dev hosts and amd64 CI/prod.
ARG TARGETARCH
RUN apk add --no-cache curl tar ca-certificates && \
    case "${TARGETARCH:-amd64}" in \
      amd64) ocarch=x64 ;; \
      arm64) ocarch=arm64 ;; \
      *) echo "unsupported TARGETARCH=${TARGETARCH}" >&2; exit 1 ;; \
    esac && \
    curl -fsSL \
      "https://github.com/anomalyco/opencode/releases/download/v${OPENCODE_VERSION}/opencode-linux-${ocarch}-musl.tar.gz" \
      -o /tmp/opencode.tar.gz && \
    tar -xzf /tmp/opencode.tar.gz -C /usr/local/bin && \
    chmod +x /usr/local/bin/opencode

# ── runtime image ──────────────────────────────────────────────────────────────
FROM alpine:3.20

# ripgrep + fzf: opencode uses them for code search and degrades without them.
# git: VCS-aware tooling over the checkout. ca-certificates: TLS egress to the LLM.
# libstdc++ + libgcc: the Bun-compiled opencode binary links them on musl.
# sqlite: the entrypoint reads opencode's session DB after the run to capture token
# usage for cost accounting.
RUN apk add --no-cache ca-certificates git ripgrep fzf libstdc++ libgcc sqlite

# Copy the opencode binary from the downloader stage and fail the build loudly if
# it can't even report its version (catches a wrong arch / missing runtime lib).
COPY --from=downloader /usr/local/bin/opencode /usr/local/bin/opencode
RUN opencode --version

# Entrypoint maps ManyForge's injected LLM_* env onto sst/opencode's config +
# flags and runs the review, writing /out/review.json.
COPY deploy/sandbox/entrypoint.sh /usr/local/bin/review
RUN chmod +x /usr/local/bin/review

ENTRYPOINT ["/usr/local/bin/review"]
