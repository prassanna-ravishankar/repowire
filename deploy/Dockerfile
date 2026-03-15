# Stage 1: Build Next.js dashboard static export
FROM node:22-slim AS web-builder
WORKDIR /web
COPY web/package.json web/package-lock.json* ./
RUN npm ci
COPY web/ .
ENV NEXT_PUBLIC_API_BASE=""
RUN npm run build

# Stage 2: Python relay server
FROM ghcr.io/astral-sh/uv:python3.12-bookworm-slim

WORKDIR /app

ENV UV_COMPILE_BYTECODE=1
ENV UV_LINK_MODE=copy

# Dependency cache layer
COPY pyproject.toml uv.lock README.md ./
RUN --mount=type=cache,target=/root/.cache/uv uv sync --frozen --no-install-project --no-dev

# App code layer
COPY repowire/ repowire/
COPY --from=web-builder /web/out web/out
RUN --mount=type=cache,target=/root/.cache/uv uv sync --frozen --no-dev

ENV PATH="/app/.venv/bin:$PATH"
ENV PYTHONUNBUFFERED=1

EXPOSE 8000
CMD ["repowire", "relay", "start", "--host", "0.0.0.0", "--port", "8000"]
