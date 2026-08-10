FROM python:3.12-slim AS build

RUN pip install --no-cache-dir zensical==0.0.43

WORKDIR /src
COPY docs ./docs
COPY mkdocs.yml ./
RUN zensical build --strict

FROM nginx:alpine

COPY --from=build /src/site/ /usr/share/nginx/html/
COPY docs-image/nginx.conf /etc/nginx/conf.d/default.conf
COPY docs-image/security-headers.conf /etc/nginx/conf.d/security-headers.conf

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -qO- http://localhost/health || exit 1

EXPOSE 80
