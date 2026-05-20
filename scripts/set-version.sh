#!/bin/bash

# Sets version tags in various places

VERSION=$1
shift

[ "x$VERSION" = "x" ] && {
    echo "Usage: $0 <version-tag>"
    exit 1
}

ROOT=$PWD

# Makefile
sed -i "s/^TAG ?= .*/TAG ?= $VERSION/" $ROOT/Makefile

# Charts
sed -i "s/^version: .*/version: $VERSION/" $ROOT/charts/gpu-base-operator/Chart.yaml
sed -i "s/^appVersion: .*/appVersion: \"$VERSION\"/" $ROOT/charts/gpu-base-operator/Chart.yaml
sed -i "s/^version: .*/version: $VERSION/" $ROOT/charts/gpu-base-operator-policy/Chart.yaml
sed -i "s/^appVersion: .*/appVersion: \"$VERSION\"/" $ROOT/charts/gpu-base-operator-policy/Chart.yaml
sed -i "s|intel/intel-gpu-base-operator:.*|intel/intel-gpu-base-operator:$VERSION|" $ROOT/charts/gpu-base-operator/values.yaml

# Manager yaml
sed -i "s|intel/intel-gpu-base-operator:.*|intel/intel-gpu-base-operator:$VERSION|" $ROOT/config/manager/manager.yaml
