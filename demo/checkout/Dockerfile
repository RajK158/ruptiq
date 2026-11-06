# syntax=docker/dockerfile:1

# One image definition for every checkout demo service. Choose the service
# with the SERVICE build argument, the name of its directory under
# demo/checkout/: order-service, inventory-service, or payment-service.
#
#   docker build -f demo/checkout/Dockerfile --build-arg SERVICE=order-service .
#
# The build context is the repository root (for go.mod and go.sum). The root
# .dockerignore sends only those and demo/checkout/ to the builder, so Ruptiq
# Core is never part of these images.

ARG GO_VERSION=1.27
ARG ALPINE_VERSION=3.24

FROM golang:${GO_VERSION}-alpine AS build
WORKDIR /src

# Download modules before copying the source, so this layer is reused until
# go.mod or go.sum change. go.sum pins every module's checksum.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY demo/checkout/ demo/checkout/

ARG SERVICE
RUN test -n "${SERVICE}" || { echo "build arg SERVICE is required" >&2; exit 1; }
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
        -o /out/service "./demo/checkout/${SERVICE}"

# Alpine rather than a shell-less image: its busybox wget runs the Compose
# health checks without adding code or packages.
FROM alpine:${ALPINE_VERSION}
ARG SERVICE
LABEL org.opencontainers.image.title="ruptiq-checkout-${SERVICE}" \
      org.opencontainers.image.description="Ruptiq checkout demo target (not Ruptiq Core)" \
      org.opencontainers.image.source="https://github.com/RajK158/ruptiq"

RUN addgroup -S -g 10001 app && adduser -S -D -H -u 10001 -G app app
COPY --from=build /out/service /usr/local/bin/service
USER 10001:10001

ENTRYPOINT ["/usr/local/bin/service"]
