#!/bin/bash
set -e

# ClawManager 构建并推送镜像到阿里云
# 用法: ./push-image.sh [tag]

REGISTRY="crpi-p7zet5k9t1r8eu0j.cn-shenzhen.personal.cr.aliyuncs.com"
REPO="yangkin/clawmanager"
TAG="${1:-latest}"
PLATFORM="linux/amd64"

echo "==> Building ClawManager image..."
docker build --platform "$PLATFORM" -t clawmanager:"$TAG" -f Dockerfile .

echo "==> Tagging for Aliyun..."
docker tag clawmanager:"$TAG" "$REGISTRY/$REPO:$TAG"

echo "==> Pushing to Aliyun..."
docker push "$REGISTRY/$REPO:$TAG"

echo "==> Done. Image: $REGISTRY/$REPO:$TAG"
