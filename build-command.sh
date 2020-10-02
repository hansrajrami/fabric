#!/bin/bash 

## Build orderer rootless image
docker build --build-arg GO_VER=1.15.2 --build-arg ALPINE_VER=3.12 --build-arg GO_TAGS=pkcs11 -t hansrajrami/fabric-orderer:2.2.1-rootless -f images/orderer/Dockerfile . --no-cache

## Build peer rootless image
docker build --build-arg GO_VER=1.15.2 --build-arg ALPINE_VER=3.12 --build-arg GO_TAGS=pkcs11 -t hansrajrami/fabric-peer:2.2.1-rootless -f images/peer/Dockerfile . --no-cache
