# 多阶段构建：静态二进制 + 最小运行镜像
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/cmdc2api .

FROM alpine:3.22
RUN adduser -D -u 10001 proxy && mkdir -p /data && chown proxy:proxy /data
COPY --from=build /out/cmdc2api /usr/local/bin/cmdc2api
ENV HOST=0.0.0.0 \
    PORT=3050 \
    CC_STATE_FILE=/data/state.json
USER proxy
EXPOSE 3050
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget --spider -q http://127.0.0.1:3050/health || exit 1
ENTRYPOINT ["cmdc2api"]
