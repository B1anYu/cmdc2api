# 多阶段构建：静态二进制 + 最小运行镜像
# 固定到 1.26.10+：CVE-2026-111943（crypto/x509）；浮动的 1.26-alpine 会命中构建缓存里的旧版本
FROM golang:1.26.10-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/cmdc2api .

FROM alpine:3.22
RUN adduser -D -u 10001 proxy && mkdir -p /data && chown proxy:proxy /data
COPY --from=build /out/cmdc2api /usr/local/bin/cmdc2api
ENV HOST=0.0.0.0 \
    PORT=8050 \
    CC_STATE_FILE=/data/state.json
USER proxy
EXPOSE 8050
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget --spider -q http://127.0.0.1:8050/health || exit 1
ENTRYPOINT ["cmdc2api"]
