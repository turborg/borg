# syntax=docker/dockerfile:1.7

# Build stage: pinned Go for reproducibility. Bump in lockstep with go.mod's
# go directive (and CI once it exists). Building here — not on the host — keeps
# third-party module downloads and the build toolchain off the host machine.
# Combined with `make docker-test`, dependency code never executes on the host.
FROM golang:1.26-alpine AS builder

WORKDIR /src

# Cache module downloads independently of source changes so iterative builds
# only re-resolve when go.mod / go.sum change.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0 + -trimpath produces a static binary that runs on the
# distroless/static base. -s -w drops the symbol + debug tables.
ARG VERSION=dev
RUN CGO_ENABLED=0 go build \
    -trimpath \
    -ldflags="-s -w -X github.com/turborg/borg/internal/version.Version=${VERSION}" \
    -o /borg ./cmd/borg

# Runtime stage: distroless/static — a CA bundle + tzdata only, no shell or
# package manager. borg is normally run from a host install; this image is the
# clean, minimal output of the isolated build stage.
FROM gcr.io/distroless/static-debian12:nonroot

LABEL org.opencontainers.image.source="https://github.com/turborg/borg"
LABEL org.opencontainers.image.description="borg — authenticated, metered AI coding-agent CLI"

COPY --from=builder /borg /borg

USER nonroot:nonroot
ENTRYPOINT ["/borg"]
