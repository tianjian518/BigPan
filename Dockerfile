# BigPan —— 网盘聚合播放服务（多架构）
# 二进制是静态编译（CGO_ENABLED=0），scratch 即可跑，镜像约 9MB。
# 构建（多架构，需 buildx）：
#   CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o bigpan-arm64 .
#   CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o bigpan-amd64 .
#   docker buildx build --platform linux/amd64,linux/arm64 -t tianjian518/bigpan:latest --push .
FROM scratch
ARG TARGETARCH
COPY bigpan-${TARGETARCH} /app/panboost
ENV TZ=Asia/Shanghai
EXPOSE 8099
ENTRYPOINT ["/app/panboost"]
