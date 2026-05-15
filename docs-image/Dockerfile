FROM python:3.12-slim AS build

RUN pip install --no-cache-dir uv==0.5.4

WORKDIR /src
COPY pyproject.toml uv.lock README.md ./
COPY docs ./docs
COPY mkdocs.yml ./

# --no-install-project: docs build only needs mkdocs + mkdocs-material +
# pymdown-extensions from the [docs] extra. Skipping the project install
# avoids pulling repowire's hatch build, which force-includes web/out
# (a Next.js export artifact unrelated to the docs site).
RUN uv sync --extra docs --frozen --no-install-project
RUN .venv/bin/mkdocs build --strict

FROM nginx:alpine

COPY --from=build /src/site/ /usr/share/nginx/html/
COPY docs-image/nginx.conf /etc/nginx/conf.d/default.conf
COPY docs-image/security-headers.conf /etc/nginx/conf.d/security-headers.conf

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -qO- http://localhost/health || exit 1

EXPOSE 80
