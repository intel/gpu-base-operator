# Copyright (c) 2026 Intel Corporation
# SPDX-License-Identifier: Apache-2.0
#
# GPU hang image for the GPU recovery e2e tests.
# Builds test/gpu-hang against a pinned IGT. It wedges an xe GPU, so run it
# privileged and only against a dedicated test GPU. See test/gpu-hang/README.md.
#
# The resulting gpu-hang binary is linked against libpci3 and other GPLv2+ libraries.
# It is intended for testing use only. Publishing the container requires attention to
# GPLv2+ compliance.

FROM debian:trixie AS builder

ARG IGT_REPO=https://gitlab.freedesktop.org/drm/igt-gpu-tools.git
ARG IGT_REF=18811c476bfde62ffbb4c0af55cd6f57e36e0dca

RUN apt-get update && apt-get install -y --no-install-recommends \
	bison ca-certificates flex gcc git meson ninja-build pkg-config python3 \
	libcairo2-dev libdrm-dev libdw-dev libglib2.0-dev libkmod-dev liblzma-dev \
	libpci-dev libpciaccess-dev libpixman-1-dev libudev-dev zlib1g-dev \
	&& rm -rf /var/lib/apt/lists/*

WORKDIR /igt
RUN git init -q . && git remote add origin ${IGT_REPO} \
	&& git fetch -q --depth 1 origin ${IGT_REF} && git checkout -q FETCH_HEAD

# Build gpu-hang as one of IGT's tools, so it gets IGT's config.h and flags.
COPY test/gpu-hang/gpu-hang.c tools/
RUN sed -i "s/^tools_progs = \[/tools_progs = [\n\t'gpu-hang',/" tools/meson.build \
	&& grep -q "'gpu-hang'" tools/meson.build

RUN meson setup build -Dtests=disabled -Drunner=disabled -Doverlay=disabled \
		-Dchamelium=disabled -Dvalgrind=disabled -Dlibunwind=disabled \
		-Dman=disabled -Ddocs=disabled -Dtestplan=disabled -Dsphinx=disabled \
		-Dlibdrm_drivers=intel \
	&& ninja -C build tools/gpu-hang

FROM debian:trixie-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
	libcairo2 libdrm-intel1 libdrm2 libdw1t64 libglib2.0-0t64 libkmod2 liblzma5 \
	libpci3 libpciaccess0 libpixman-1-0 libudev1 zlib1g \
	&& rm -rf /var/lib/apt/lists/*

COPY --from=builder /igt/build/lib/libigt.so* /usr/local/lib/
COPY --from=builder /igt/build/tools/gpu-hang /usr/local/bin/gpu-hang
RUN ldconfig && ! ldd /usr/local/bin/gpu-hang | grep 'not found'

ENTRYPOINT ["/usr/local/bin/gpu-hang"]
