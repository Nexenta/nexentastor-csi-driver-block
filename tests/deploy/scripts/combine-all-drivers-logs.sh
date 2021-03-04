#!/usr/bin/env bash

#
# Combine all driver logs into single stdout
#

{ \
    kubectl logs -f --all-containers=true $(kubectl get pods | awk '/nexentastor-block-csi-controller/ {print $1;exit}') & \
    kubectl logs -f --all-containers=true $(kubectl get pods | awk '/nexentastor-block-csi-node/ {print $1;exit}'); \
}