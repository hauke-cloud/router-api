# syntax=docker/dockerfile:1

# Build stage. Cross-compilation is done by Go rather than by emulation, so a
# multi-arch build runs at native speed on a single builder.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS builder

WORKDIR /src

# Dependencies first: this layer is cached until go.mod or go.sum changes.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown

# One image carries the three managers. They are released together and embed
# the same CRDs; the chart picks the binary per Deployment.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build \
      -trimpath \
      -ldflags="-w -s \
        -X github.com/hauke-cloud/router-api/internal/version.version=${VERSION} \
        -X github.com/hauke-cloud/router-api/internal/version.commit=${COMMIT} \
        -X github.com/hauke-cloud/router-api/internal/version.date=${DATE}" \
      -o /out/ \
      ./cmd/core ./cmd/infrastructure-hetzner ./cmd/config-vyos

# Runtime stage. distroless/static carries CA certificates and a nonroot user
# and nothing else. The CRDs the managers install are embedded in the
# binaries, so nothing has to be copied alongside them.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /out/core /usr/local/bin/core
COPY --from=builder /out/infrastructure-hetzner /usr/local/bin/infrastructure-hetzner
COPY --from=builder /out/config-vyos /usr/local/bin/config-vyos

USER nonroot:nonroot
EXPOSE 8080 8081

# No ENTRYPOINT on purpose: the image holds three managers, and starting the
# wrong one silently would be worse than having to say which. The chart sets
# the command.
