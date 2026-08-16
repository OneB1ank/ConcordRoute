FROM golang:1.26.6-alpine

ARG MIHOMO_VERSION=1.19.30

WORKDIR /app

# 安装必要的工具
RUN apk add --no-cache git ca-certificates wget && \
    case "$(apk --print-arch)" in \
      x86_64) MIHOMO_ARCH=amd64; MIHOMO_SHA256=cf06ce2c7d1421bdbda14ee4a5b6046672dc35ebf8eecd8e77504ec3c0ed9a84 ;; \
      aarch64) MIHOMO_ARCH=arm64; MIHOMO_SHA256=58896873736d28628f66de3677c8654fa0f180662523148e136cff4f6e890069 ;; \
      *) echo "unsupported mihomo architecture" >&2; exit 1 ;; \
    esac && \
    wget -O /tmp/mihomo.gz "https://github.com/MetaCubeX/mihomo/releases/download/v${MIHOMO_VERSION}/mihomo-linux-${MIHOMO_ARCH}-v${MIHOMO_VERSION}.gz" && \
    echo "${MIHOMO_SHA256}  /tmp/mihomo.gz" | sha256sum -c - && \
    gunzip /tmp/mihomo.gz && chmod 0755 /tmp/mihomo && mv /tmp/mihomo /usr/local/bin/mihomo

# 复制go.mod和go.sum
COPY go.mod go.sum ./

# 下载依赖
RUN go mod download

# 复制源代码
COPY . .

# 构建应用
RUN VERSION_VALUE="$(./scripts/resolve-version.sh)" && \
    go build -ldflags="-s -w -X main.Version=${VERSION_VALUE}" -o main ./cmd/server/

# 暴露端口
EXPOSE 8080

# 运行应用
CMD ["./main"]
