FROM golang:1.25-alpine AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /mf-egress-proxy ./cmd/mf-egress-proxy

FROM alpine:3.20
COPY --from=build /mf-egress-proxy /mf-egress-proxy
# Non-root: the proxy only listens on :8080 (cmd/mf-egress-proxy/main.go default
# LISTEN), a high port, so it needs no root privilege to bind it.
USER 65532:65532
ENTRYPOINT ["/mf-egress-proxy"]
