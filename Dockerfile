FROM golang:1.23-alpine AS build

# 使用国内 Go 模块代理
ENV GOPROXY=https://goproxy.cn,direct \
    GOSUMDB=off \
    GO111MODULE=on

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/m365-copilot2api ./cmd/server

FROM alpine:3.20
RUN apk add --no-cache wget ca-certificates tzdata \
    && addgroup -S m365 && adduser -S -G m365 m365 \
    && mkdir -p /data /app

# Install sing-box for VLESS/VMess/SS subscription proxy support.
# TARGETARCH keeps the binary matched to the build platform; the previous
# hard-coded linux-amd64 produced "exec format error" on arm64 hosts, which
# silently killed the entire proxy path (→ mass 502s from that box).
ARG TARGETARCH=amd64
RUN wget -qO /tmp/sing-box.tar.gz "https://github.com/SagerNet/sing-box/releases/download/v1.10.7/sing-box-1.10.7-linux-${TARGETARCH}.tar.gz" \
    && tar -xzf /tmp/sing-box.tar.gz -C /tmp \
    && cp /tmp/sing-box-*/sing-box /usr/local/bin/sing-box \
    && chmod +x /usr/local/bin/sing-box \
    && rm -rf /tmp/sing-box*

ENV M365_SINGBOX_BINARY=/usr/local/bin/sing-box
WORKDIR /app
COPY --from=build /out/m365-copilot2api /app/m365-copilot2api
RUN chown -R m365:m365 /app /data
# 不切换非 root 用户，因为挂载的宿主机 data 目录权限由宿主机控制
# 容器内需要 root 权限才能写入挂载目录
USER root
EXPOSE 4141
ENV M365_LISTEN=0.0.0.0:4141 \
    M365_DATA_DIR=/data \
    M365_CONFIG=/data/accounts.json \
    M365_TOKEN_CACHE=/data/token-cache.json \
    M365_SESSION_CACHE=/data/sessions.json \
    M365_API_KEYS=/data/api-keys.json \
    M365_ADMIN_PASSWORD_FILE=/data/admin-password \
    M365_ADMIN_PASSWORD_BOOTSTRAP_FILE=/run/secrets/m365_admin_password
VOLUME ["/data"]
HEALTHCHECK --interval=30s --timeout=5s --retries=3 --start-period=10s \
    CMD wget -q --spider http://localhost:4141/api/health || exit 1
ENTRYPOINT ["/app/m365-copilot2api"]
