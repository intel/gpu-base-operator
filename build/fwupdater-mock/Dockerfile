# Copyright (c) 2026 Intel Corporation
# SPDX-License-Identifier: Apache-2.0
#
# Mock firmware updater image.
# Installs test/fake-xpu-smi as 'xpu-smi'. Firmware updates are simulated without
# touching any device; recovery resets and the FDO reflash rebind the target GPU.
# See test/fake-xpu-smi/README.md.
#
# Reference in a GPUFirmwareUpdate CR:
#   spec:
#     updaterImage: ghcr.io/intel/gpu-fwupdater-mock:devel

FROM golang:1.26 AS builder

WORKDIR /workspace

COPY test/fake-xpu-smi/ .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-w -s" -o xpu-smi .

# update.sh, which the firmware update Job runs, needs a shell and coreutils.
FROM debian:stable-slim

COPY --from=builder /workspace/xpu-smi /usr/local/bin/xpu-smi

ENTRYPOINT ["/bin/sh"]
