# Build the controller binary.
# The builder runs on the build host's platform and cross-compiles for TARGETOS/TARGETARCH,
# so multi-arch builds never need QEMU emulation for the Go toolchain.
# Base images are pinned to multi-arch index digests; Dependabot (docker ecosystem) bumps them.
FROM --platform=$BUILDPLATFORM golang:1.26@sha256:6e59e17a20f13c102e49d82dffab66d41cffe6c266b225d82794328aea859ab5 AS builder
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN --mount=type=cache,target=/go/pkg/mod go mod download

# Copy the Go source (relies on .dockerignore to filter)
COPY . .

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE}" \
      -o /out/manager ./cmd/controller \
    && ln -s manager /out/controller

# distroless/static:nonroot ships CA certificates, tzdata and a 65532 "nonroot" user; no shell.
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM gcr.io/distroless/static:nonroot@sha256:8ce6dd87e9823c5b48dcfb2c8604714ccb351ce59607327bcc4957448da279b5
WORKDIR /
# /manager is the binary; /controller is a symlink kept for older manifests.
COPY --from=builder /out/ /
USER 65532:65532

ENTRYPOINT ["/manager"]
