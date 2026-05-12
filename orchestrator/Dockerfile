FROM golang:1.26-alpine AS builder
WORKDIR /src
RUN apk add --no-cache git
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags='-s -w -extldflags=-static' \
    -o /out/orchestrator ./cmd/orchestrator

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/orchestrator /usr/local/bin/orchestrator
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/orchestrator"]
CMD ["--help"]
