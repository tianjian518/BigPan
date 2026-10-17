package main

import (
	"testing"
	"time"
)

// baiduTempHas 是盘扫误伤防护的开关：内存 map 里有的路径（本次启动转存/复用的）
// 必须被盘扫跳过，否则按网盘修改时间删会误删正在观看的转存文件。
func TestBaiduTempHas(t *testing.T) {
	baiduTempMu.Lock()
	saved := baiduTempPaths
	baiduTempPaths = map[string]time.Time{"/bigpan_tmp/a.mp4": time.Now()}
	baiduTempMu.Unlock()
	defer func() {
		baiduTempMu.Lock()
		baiduTempPaths = saved
		baiduTempMu.Unlock()
	}()

	if !baiduTempHas("/bigpan_tmp/a.mp4") {
		t.Fatal("记录过的路径应判定为存在")
	}
	if baiduTempHas("/bigpan_tmp/b.mp4") {
		t.Fatal("没记录过的路径不应判定为存在")
	}
	if baiduTempHas("") || baiduTempHas("/") {
		t.Fatal("空路径不应命中")
	}
}
