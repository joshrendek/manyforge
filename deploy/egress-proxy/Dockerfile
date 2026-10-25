# syntax=docker/dockerfile:1@sha256:4edf897a3ffa55b89f906fc8cc78afdb3f1834cc9c7083565e611a8a7d5fe99e
FROM golang:1.26.10-alpine@sha256:27f2d3b83cfc5803c8078f8e730af0f7c431bf067afaec8f60a36fb763187cba AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /mf-egress-proxy ./cmd/mf-egress-proxy

FROM alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc
COPY --from=build /mf-egress-proxy /mf-egress-proxy
# Non-root: the proxy only listens on :8080 (cmd/mf-egress-proxy/main.go default
# LISTEN), a high port, so it needs no root privilege to bind it.
USER 65532:65532
ENTRYPOINT ["/mf-egress-proxy"]
