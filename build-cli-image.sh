#!/bin/bash
# Copyright (c) AppsCode Inc.
# SPDX-License-Identifier: LicenseRef-AppsCode-Free-Trial-1.0.0

# Build script for OpenBao with remote-db-plugin CLI tools

set -e

echo "==> Building OpenBao binary..."
cd /home/rudro25/go/src/github.com/openbao/openbao
make dev

echo "==> Building CLI tools..."
cd /home/rudro25/go/src/github.com/openbao/openbao/plugins/database/remote-db-plugin
make all

echo "==> Building Docker image..."
cd /home/rudro25/go/src/github.com/openbao/openbao

IMAGE_TAG="${1:-cli-v1}"

docker build \
  --build-arg BIN_NAME=bao \
  -t rudro25/openbao:${IMAGE_TAG} \
  -f Dockerfile.cli .

echo "==> Image built: rudro25/openbao:${IMAGE_TAG}"
echo ""
echo "To push to registry:"
echo "  docker push rudro25/openbao:${IMAGE_TAG}"
echo ""
echo "To test locally:"
echo "  docker run --rm rudro25/openbao:${IMAGE_TAG} version"
echo "  docker run --rm rudro25/openbao:${IMAGE_TAG} bao-cluster --help"
