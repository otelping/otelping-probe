FROM golang:1.23-alpine AS builder
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=1.0.0
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/otelping-probe ./cmd/otelping-probe

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/otelping-probe /usr/local/bin/otelping-probe
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/otelping-probe"]
