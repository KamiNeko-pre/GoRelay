FROM golang:1.26-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/gorelay ./cmd/server \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/publisher ./cmd/publisher \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/worker ./cmd/worker \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mcp-check ./cmd/mcp-check \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/bench-mcp ./cmd/bench-mcp

FROM alpine:3.21 AS runtime

ENV GORELAY_ENV=production

RUN apk add --no-cache ca-certificates
RUN addgroup -S gorelay && adduser -S gorelay -G gorelay

WORKDIR /app
COPY --from=builder /out/gorelay /app/gorelay
COPY --from=builder /out/publisher /app/publisher
COPY --from=builder /out/worker /app/worker
COPY --from=builder /out/mcp-check /app/mcp-check
COPY --from=builder /out/bench-mcp /app/bench-mcp
COPY configs /app/configs
COPY scripts /app/scripts
COPY web /app/web

RUN mkdir -p /app/data/snapshots && chown -R gorelay:gorelay /app
USER gorelay

EXPOSE 8080
CMD ["/app/gorelay"]
