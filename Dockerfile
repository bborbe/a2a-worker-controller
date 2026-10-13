FROM golang:1.27.1 AS build
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
COPY --from=alpine /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /usr/local/go/lib/time/zoneinfo.zip /
ENV ZONEINFO=/zoneinfo.zip
ENV BUILD_GIT_VERSION=${BUILD_GIT_VERSION}
ENV BUILD_GIT_COMMIT=${BUILD_GIT_COMMIT}
ENV BUILD_DATE=${BUILD_DATE}
ENTRYPOINT ["/main"]
