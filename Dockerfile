# ---- 构建 ----
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/bigpan .

# ---- 运行 ----
FROM alpine:3.19
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=build /out/bigpan /app/bigpan
ENV TZ=Asia/Shanghai
VOLUME ["/data"]
EXPOSE 8099
ENTRYPOINT ["/app/bigpan"]
CMD ["-port", "8099", "-data", "/data/items.json"]
