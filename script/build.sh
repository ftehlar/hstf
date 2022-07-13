#!/usr/bin/env bash

if [ -f ~/.hstf.conf ]; then
    . ~/.hstf.conf
else
    echo "no ~/.hstf.conf found; set VPP_WS to vpp root"
    exit 1
fi

bin=vpp-data/bin
lib=vpp-data/lib

mkdir -p ${bin} ${lib} || true

cp ${VPP_WS}/build-root/build-vpp_debug-native/vpp/bin/{vpp,vppctl} ${bin}
cp -r ${VPP_WS}/build-root/build-vpp_debug-native/vpp/lib/x86_64-linux-gnu/* ${lib}

docker build -t hstf/vpp -f Dockerfile.vpp .
