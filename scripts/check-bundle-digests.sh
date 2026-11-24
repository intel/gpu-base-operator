#!/bin/bash

# Lists the images in a ClusterServiceVersion that are not pinned by digest and exits
# non-zero if there are any. Disconnected installs need every image the CSV deploys or lists
# in spec.relatedImages to be digest-pinned: oc-mirror mirrors by digest and
# ImageDigestMirrorSet only redirects digest references.

set -euo pipefail

csv=${1:-}

[ -f "$csv" ] || {
    echo "Usage: $0 <clusterserviceversion.yaml>" >&2
    echo "Example: $0 bundle/manifests/intel-gpu-base-operator.clusterserviceversion.yaml" >&2
    exit 2
}

unpinned=$(grep -E '^[[:space:]]*(- )?image:[[:space:]]' "$csv" |
    sed -E 's/^[[:space:]]*(- )?image:[[:space:]]*//; s/^"(.*)"$/\1/' |
    grep -v '@sha256:[0-9a-f]\{64\}$' | sort -u || true)

if [ -n "$unpinned" ]; then
    echo "Images in $csv not pinned by digest:" >&2
    echo "$unpinned" | sed 's/^/  /' >&2
    exit 1
fi

echo "All images in $csv are pinned by digest."
