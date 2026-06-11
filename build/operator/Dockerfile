# Copyright (c) 2025 Intel Corporation
# SPDX-License-Identifier: Apache-2.0

ARG BASE_IMAGE=gcr.io/distroless/static:nonroot

# Build the manager binary
FROM golang:1.26 AS builder
ARG TARGETOS
ARG TARGETARCH
ARG GITINFO
ARG BUILDDATE
ARG PKGNAME
ENV GO111MODULE=on
ENV CGOFLAGS="-trimpath"
ENV GCFLAGS="all=-spectre=all"
ENV ASMFLAGS="-spectre=all"
ENV LDFLAGS="all=-w -s -X ${PKGNAME}/internal/version.gitInfo=${GITINFO} -X ${PKGNAME}/internal/version.buildDate=${BUILDDATE}"

WORKDIR /workspace

# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
COPY LICENSE LICENSE
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the go source
COPY cmd/main.go cmd/main.go
COPY api/ api/
COPY internal/ internal/
COPY config/deployments config/deployments

# Build
# the GOARCH has not a default value to allow the binary be built according to the host where the command
# was called. For example, if we call make docker-build in a local env which has the Apple Silicon M1 SO
# the docker BUILDPLATFORM arg will be linux/arm64 when for Apple x86 it will be linux/amd64. Therefore,
# by leaving it empty we can ensure that the container and binary shipped on it will have the same platform.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} GO111MODULE=${GO111MODULE} \
    go build $CGOFLAGS --gcflags="$GCFLAGS" --asmflags="$ASMFLAGS" --ldflags="$LDFLAGS"  -a -o manager \
    ./cmd/main.go

# Verify no test hooks or debug endpoints are linked into the production binary
RUN go tool nm /workspace/manager | awk '{print $3}' | \
    grep -E '^(testing\.Main|net/http/pprof\.|expvar\.)' && \
    echo "ERROR: debug/test symbols found in binary" && exit 1 || true

# Verify binary build specs with checksec
ENV CHECKSEC_REF="No RELRO,No Canary found,NX enabled,No PIE,N/A,N/A,No Symbols,N/A,0,0"
RUN apt-get update -y && apt-get --no-install-recommends -y install file && \
    wget -q https://raw.githubusercontent.com/slimm609/checksec/refs/heads/main/checksec.bash -O checksec && \
    chmod +x checksec && \
    ./checksec --file=/workspace/manager --output=csv | grep -q "$CHECKSEC_REF"

RUN go get github.com/google/go-licenses && \
    go run github.com/google/go-licenses save ./cmd/ --save_path licenses && \
    cp /workspace/LICENSE /workspace/licenses/LICENSE.intel-gpu-base-operator

ARG BASE_IMAGE

# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM ${BASE_IMAGE} AS runtime
WORKDIR /
COPY --from=builder /workspace/manager .
COPY --from=builder /workspace/licenses /licenses
USER 65532:65532

ENTRYPOINT ["/manager"]
