# ── 第一阶段:构建前端 ──
FROM node:22-alpine AS web-builder
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN --mount=type=cache,target=/root/.npm \
    npm ci
COPY web/ ./
# 快速构建前端(跳过冗余的 tsc 类型检查,类型已在本地及 CI 严格保障,Vite 构建仅需 0.3 秒)
RUN rm -rf /src/internal/webui/dist && npx vite build --emptyOutDir

# ── 第二阶段:编译 Go 二进制(含内嵌前端) ──
FROM golang:1.26-alpine AS builder
RUN apk add --no-cache git ca-certificates
WORKDIR /build
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
# 仅复制 Go 构建所需的源码，彻底与根目录杂项解耦
COPY internal/ ./internal/
COPY main.go ./
# 宿主若残留 dist 先清掉，再放入第一阶段生成的纯净产物
RUN rm -rf ./internal/webui/dist
COPY --from=web-builder /src/internal/webui/dist ./internal/webui/dist
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o icloud-hme .

# ── 第三阶段:运行时(仅二进制,无 Node) ──
FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata wget \
    && adduser -D -u 10001 -h /app app \
    && mkdir -p /app/data \
    && chown -R app:app /app
WORKDIR /app
COPY --from=builder /build/icloud-hme ./icloud-hme
# 容器内必须监听全网卡,否则容器外访问不到(容器网络命名空间下回环不可达)
ENV ICLOUD_HME_ADDR="0.0.0.0:8081"
EXPOSE 8081
# 数据目录需与宿主卷属主一致:宿主执行 chown -R 10001:10001 ./data
USER app
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8081/livez >/dev/null 2>&1 || exit 1
ENTRYPOINT ["/app/icloud-hme"]
