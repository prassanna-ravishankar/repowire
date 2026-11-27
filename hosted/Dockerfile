# A workspace's Linux sandbox. The donor image ships sandbox-shim, the helper
# @cloudflare/sandbox runs for directory backups; keep its tag equal to the
# installed @cloudflare/sandbox version.
ARG SANDBOX_TOOLS_IMAGE=docker.io/cloudflare/sandbox:1.0.3
FROM ${SANDBOX_TOOLS_IMAGE} AS sandbox-tools

FROM debian:trixie-slim
COPY --from=sandbox-tools /usr/local/bin/sandbox-shim /usr/local/bin/sandbox-shim
# No /workspace here: the Worker creates it at start, in the writable layer,
# so a restore can swap it in (overlayfs will not rename an image directory).
CMD ["sleep", "infinity"]
