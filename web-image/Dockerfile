FROM node:22-slim AS build

WORKDIR /web

COPY web/package.json web/package-lock.json* ./
RUN npm ci

COPY web/ ./
ENV NEXT_TELEMETRY_DISABLED=1
RUN npm run build

FROM nginx:alpine

COPY --from=build /web/out/ /usr/share/nginx/html/
COPY web-image/nginx.conf /etc/nginx/conf.d/default.conf
COPY web-image/security-headers.conf /etc/nginx/conf.d/security-headers.conf

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -qO- http://localhost/health || exit 1

EXPOSE 80
