#!/bin/bash
# 构建 Linux amd64 最小化二进制(含内嵌管理界面)
#
# 用法: ./build.sh
# 输出: build/icloud-hme

set -e

OUTPUT_DIR="build"
BINARY_NAME="icloud-hme"
VERSION="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
BUILD_TIME="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

echo "==> 安装前端依赖"
npm --prefix web ci

echo "==> 运行前端测试"
npm --prefix web run test:run

echo "==> 清理陈旧前端产物"
# 必须显式清理:Vite 的 emptyOutDir 在某些检出版本上不会删除历史 index-*.js，
# 而 //go:embed dist/* 是递归内嵌，陈旧产物会被一并打进二进制。
rm -rf internal/webui/dist

echo "==> 构建前端(输出到 internal/webui/dist)"
npm --prefix web run build

if [ ! -f internal/webui/dist/index.html ]; then
  echo "!! 前端构建未产出 index.html，内嵌界面会 503。请检查 npm run build 输出。" >&2
  exit 1
fi

echo "==> 静态检查"
go vet ./internal/... .

echo "==> 运行 Go 测试"
go test ./internal/... .

echo "==> 清理旧的构建文件"
rm -rf "$OUTPUT_DIR"
mkdir -p "$OUTPUT_DIR"

echo "==> 构建 Linux amd64 最小化二进制"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath \
    -ldflags="-s -w -buildid= -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildTime=${BUILD_TIME}" \
    -gcflags="-l=4" \
    -o "$OUTPUT_DIR/$BINARY_NAME" \
    .

echo "==> 压缩二进制(upx)"
# UPX 会显著提高杀软误报率，改为显式开关:UPX=1 ./build.sh
if [ "${UPX:-0}" = "1" ] && command -v upx >/dev/null 2>&1; then
  upx --best --lzma "$OUTPUT_DIR/$BINARY_NAME" || true
else
  echo "    (跳过 upx 压缩;如需启用请设置 UPX=1)"
fi

echo ""
echo "==> 构建完成"
echo "    版本: ${VERSION} (${COMMIT}) @ ${BUILD_TIME}"
echo "    文件: $OUTPUT_DIR/$BINARY_NAME"
ls -lh "$OUTPUT_DIR/$BINARY_NAME"
