# BigPan

网盘分享聚合播放服务：把夸克 / 百度 / 115 / 移动 / 天翼的分享，统一成 WebDAV + STRM + 302 直链。

## 运行

```bash
docker run -d --name bigpan --restart unless-stopped \
  -p 8099:8099 \
  -v /你的数据目录:/data \
  -v /你的STRM目录:/strm \
  -v /etc/localtime:/etc/localtime:ro \
  tianjian518/bigpan:latest \
  -port=8099 -data=/data/items.json -strm=/strm \
  -user=admin -pass=你的密码
```

打开 `http://<ip>:8099`，扫码登录网盘，粘分享链接即可。

## 播放链路

- WebDAV：`http://<ip>:8099/dav`
- 直链 302：`http://<ip>:8099/direct`（数据直连网盘 CDN，不占服务器带宽）
- STRM：由程序生成到 `/strm`，供 Emby / 飞牛 / Kodi / 爆米花 扫描

## 支持

天翼云盘（302 全链路 / 扫码登录 / 订阅号追更）、夸克、百度、115、移动云盘。
