FROM golang:1.26-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/lua-spider ./cmd/server \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/agent-worker ./cmd/agent-worker \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/information-worker ./cmd/information-worker \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/publisher ./cmd/publisher \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/worker ./cmd/worker \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/indexer ./cmd/indexer \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/agent-eval ./cmd/agent-eval \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ai-check ./cmd/ai-check

FROM alpine:3.21 AS runtime

ENV GORELAY_ENV=production

RUN apk add --no-cache ca-certificates
RUN addgroup -S crawler && adduser -S crawler -G crawler

WORKDIR /app
COPY --from=builder /out/lua-spider /app/lua-spider
COPY --from=builder /out/agent-worker /app/agent-worker
COPY --from=builder /out/information-worker /app/information-worker
COPY --from=builder /out/publisher /app/publisher
COPY --from=builder /out/worker /app/worker
COPY --from=builder /out/indexer /app/indexer
COPY --from=builder /out/agent-eval /app/agent-eval
COPY --from=builder /out/ai-check /app/ai-check
COPY configs /app/configs
COPY scripts /app/scripts
COPY web /app/web

RUN chown -R crawler:crawler /app
USER crawler

EXPOSE 8080
CMD ["/app/lua-spider"]
