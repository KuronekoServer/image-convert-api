# ビルドステージ
FROM golang:1.27-bookworm AS builder

RUN apt-get update && apt-get install -y --no-install-recommends \
    libvips-dev \
    librsvg2-dev \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY go.mod ./
RUN go mod download

COPY main.go openapi.yml ./
RUN CGO_ENABLED=1 go build -mod=mod -ldflags="-s -w" -o server .

# 実行ステージ
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    libvips42 \
    librsvg2-2 \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY --from=builder /app/server .

# コンテナのポートを公開
EXPOSE 12901

# アプリケーションの起動コマンドを指定
CMD ["./server"]
