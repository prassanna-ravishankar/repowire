# Kept for compatibility with older build commands. The canonical image is
# built from the repository-root Dockerfile.
FROM node:22-slim AS web-builder
WORKDIR /src/web
COPY web/package.json web/package-lock.json* ./
RUN npm ci
COPY web/ ./
ENV NEXT_PUBLIC_API_BASE=""
RUN npm run build

FROM golang:1.25-bookworm AS go-builder
WORKDIR /src/daemon-go
COPY daemon-go/go.mod daemon-go/go.sum ./
RUN go mod download
COPY daemon-go/ ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /repowire .

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --system --uid 10001 --home-dir /app repowire
WORKDIR /app
COPY --from=go-builder /repowire /usr/local/bin/repowire
COPY --from=web-builder /src/web/out /app/web/out
USER repowire
EXPOSE 8000
ENTRYPOINT ["repowire"]
CMD ["relay", "start", "--host", "0.0.0.0", "--port", "8000"]
