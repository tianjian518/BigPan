# ---- 构建（始终在构建机架构运行，交叉编译目标架构，无需 QEMU）----
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src
COPY . .
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -mod=vendor -trimpath -ldflags="-s -w" -o /out/bigpan .

# ---- 证书与时区（数据文件与架构无关）----
FROM --platform=$BUILDPLATFORM alpine:3.19 AS certs
RUN apk add --no-cache ca-certificates tzdata

# ---- 运行（scratch：体积极小，无多余组件）----
FROM scratch
COPY --from=build /out/bigpan /bigpan
COPY --from=certs /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=certs /usr/share/zoneinfo /usr/share/zoneinfo
ENV TZ=Asia/Shanghai
VOLUME ["/data"]
EXPOSE 8099
ENTRYPOINT ["/bigpan"]
CMD ["-port", "8099", "-data", "/data/items.json"]
