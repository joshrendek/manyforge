# syntax=docker/dockerfile:1@sha256:4edf897a3ffa55b89f906fc8cc78afdb3f1834cc9c7083565e611a8a7d5fe99e
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

FROM alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc AS downloader

ARG OPENCODE_VERSION=1.17.11
# TARGETARCH is injected by BuildKit (amd64 | arm64); map to opencode's asset arch
# so the image is native on both Apple-Silicon dev hosts and amd64 CI/prod.
ARG TARGETARCH
RUN apk add --no-cache curl=8.14.1-r2 tar=1.35-r2 ca-certificates=20261020-r0 && \
    case "${TARGETARCH:-amd64}" in \
      amd64) ocarch=x64; ocsha=c4bb67f7be959de6ff54a7821d9345550322a4b07cd50ca6be395a55e8643846 ;; \
      arm64) ocarch=arm64; ocsha=8531805bdbcdd0b912652a989992a8857651fd09d715ee70c0eb9dd2f134953e ;; \
      *) echo "unsupported TARGETARCH=${TARGETARCH}" >&2; exit 1 ;; \
    esac && \
    curl -fsSL \
      "https://github.com/anomalyco/opencode/releases/download/v${OPENCODE_VERSION}/opencode-linux-${ocarch}-musl.tar.gz" \
      -o /tmp/opencode.tar.gz && \
    printf '%s  /tmp/opencode.tar.gz\n' "$ocsha" | sha256sum -c - && \
    tar -xzf /tmp/opencode.tar.gz -C /usr/local/bin && \
    chmod +x /usr/local/bin/opencode

# ── runtime image ──────────────────────────────────────────────────────────────
FROM alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc

# ripgrep + fzf: opencode uses them for code search and degrades without them.
# git: VCS-aware tooling over the checkout. ca-certificates: TLS egress to the LLM.
# libstdc++ + libgcc: the Bun-compiled opencode binary links them on musl.
# sqlite: the entrypoint reads opencode's session DB after the run to capture token
# usage for cost accounting.
RUN apk add --no-cache ca-certificates=20261020-r0 git=2.45.4-r0 ripgrep=14.1.0-r0 fzf=0.52.0-r3 libstdc++=13.2.1_git20240309-r1 libgcc=13.2.1_git20240309-r1 sqlite=3.45.3-r3

# Copy the opencode binary from the downloader stage and fail the build loudly if
# it can't even report its version (catches a wrong arch / missing runtime lib).
COPY --from=downloader /usr/local/bin/opencode /usr/local/bin/opencode
RUN opencode --version

# Entrypoint maps ManyForge's injected LLM_* env onto sst/opencode's config +
# flags and runs the review, writing /out/review.json.
COPY deploy/sandbox/entrypoint.sh /usr/local/bin/review
# rules.sh (manyforge-8qs.2): sourced by the entrypoint to extract the reviewed repo's own
# rule docs from /work when CITE_RULES=1. Kept separate so it is unit-testable via bash.
COPY deploy/sandbox/rules.sh /usr/local/bin/rules.sh
RUN chmod +x /usr/local/bin/review

# Non-root: entrypoint.sh only ever writes under /tmp (HOME/XDG_*, and its
# working copy at /tmp/src) and /out (mkdir -p /out; review.json, usage.json) —
# both are tmpfs/world-writable mounts at runtime (read-only root fs otherwise),
# so uid 65532 needs no extra permissions.
USER 65532:65532

ENTRYPOINT ["/usr/local/bin/review"]
