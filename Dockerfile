FROM golang:1.27.2 AS build
WORKDIR /workspace
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    --mount=type=bind,target=. \
    GOCACHE=/root/.cache/go-build \
    GOMODCACHE=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags "-s -w" \
    -installsuffix cgo \
    -o /main
# teamvault-cli ships in the image because the k8s backend resolves a declared secret by shelling
# out to it, and a scratch image carries nothing it was not given. The docker backend does not
# need it here — its controller runs on the operator's Mac through the launchd wrapper, which
# already has the CLI on PATH — but one image serves both, and an image that could not resolve a
# secret would fail a k8s create at the first lookup rather than at startup.
#
# ⚠️ PINNED, NOT `@latest`. A build that resolved `latest` would produce a different image from
# the same commit on different days, so a release tag would stop naming a specific artifact.
#
# ⚠️ The module path is `github.com/Seibert-Data/teamvault-cli/v5` — the Seibert org, not bborbe —
# and the `/v5` suffix is part of it. The repo is public, so this fetch needs no credential.
#
# ⚠️ CGO_ENABLED=0 IS LOAD-BEARING, and its absence fails in a way that reads as a missing file.
# `go install` defaults CGO_ENABLED to 1, so the binary links against the system libc; the scratch
# stage has none, and exec reports `no such file or directory` for a file that is plainly present.
# Measured 2026-10-17: the build succeeded, the COPY succeeded, and the CLI still could not run
# until this was set. The controller's own build above already sets it for the same reason.
RUN CGO_ENABLED=0 GOBIN=/usr/local/bin go install github.com/Seibert-Data/teamvault-cli/v5@v5.10.1
CMD ["/bin/bash"]

FROM alpine:3.24 AS alpine
RUN apk --no-cache add ca-certificates

FROM scratch
ARG BUILD_GIT_VERSION=dev
ARG BUILD_GIT_COMMIT=none
ARG BUILD_DATE=unknown

LABEL org.opencontainers.image.title="a2a-worker-controller"
LABEL org.opencontainers.image.description="Stateless A2A worker controller"
LABEL org.opencontainers.image.vendor="Benjamin Borbe"
LABEL org.opencontainers.image.licenses="BSD-2-Clause"
LABEL org.opencontainers.image.source="https://github.com/bborbe/a2a-worker-controller"
LABEL org.opencontainers.image.documentation="https://github.com/bborbe/a2a-worker-controller"
LABEL org.opencontainers.image.version="${BUILD_GIT_VERSION}"
LABEL org.opencontainers.image.created="${BUILD_DATE}"
LABEL org.opencontainers.image.revision="${BUILD_GIT_COMMIT}"

COPY --from=build /main /main
# /usr/local/bin is on the container's default PATH (Docker sets
# /usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin even for a scratch base), which is
# what lets the controller find the CLI by the bare name it invokes.
COPY --from=build /usr/local/bin/teamvault-cli /usr/local/bin/teamvault-cli
COPY --from=alpine /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /usr/local/go/lib/time/zoneinfo.zip /
ENV ZONEINFO=/zoneinfo.zip
ENV BUILD_GIT_VERSION=${BUILD_GIT_VERSION}
ENV BUILD_GIT_COMMIT=${BUILD_GIT_COMMIT}
ENV BUILD_DATE=${BUILD_DATE}
ENTRYPOINT ["/main"]
