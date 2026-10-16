# Copyright (c) 2026 Intel Corporation
# SPDX-License-Identifier: Apache-2.0

FROM debian:stable-slim

# FWFILES_DIR lets the e2e suite build a content image from dummy files instead of real firmware.
ARG FWFILES_DIR=fwfiles

COPY ${FWFILES_DIR}/* /fwupdate/
COPY scripts/update.sh /fwupdate/update.sh
