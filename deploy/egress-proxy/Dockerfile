FROM golang:1.25-alpine AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /mf-egress-proxy ./cmd/mf-egress-proxy

FROM alpine:3.20
COPY --from=build /mf-egress-proxy /mf-egress-proxy
ENTRYPOINT ["/mf-egress-proxy"]
