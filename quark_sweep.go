package main

// ---------------------------------------------------------------------------
// 夸克临时转存文件清扫（照搬 provider_baidu.go 的成熟 sweeper 模式）
//
// 转存到 /bigpan_tmp/quark 的文件不能永远躺着：TV 302 场景下峰值占用
// 与「看过哪些剧」成正比，不清会把用户网盘占满。规则：
//
//	① 内存记账：本次启动转存/复用过的 fid → 记录时间（复用即续期）；
//	② 定时清扫：每 10 分钟一轮，删记账超过 2 小时的（TTL 见 quarkTmpTTL）；
//	③ 启动补漏：容器重启后内存记账清空，先等账号就绪再盘扫一轮，
//	   删盘上 mtime 超过 2 小时的遗留（记账里有的跳过，那是正在看的）；
//	④ 周期补漏：每轮都盘扫一次固定目录，兜住「上次启动转存 + 本次没认领」。
//
// TVFID 失效不在这里处理：播放时发现取链失败会自动重转存自愈
// （见 quarkTVPlayTarget 的自愈分支），用户无感。
// ---------------------------------------------------------------------------

import (
	"log"
	"sync"
	"time"
)

var (
	quarkTempMu    sync.Mutex
	quarkTempFIDs  = map[string]time.Time{} // 转存文件 fid → 记账时间
)

// quarkRecordTemp 记账一个转存文件（转存或复用时调；复用即续期）。
func quarkRecordTemp(fid string) {
	if fid == "" {
		return
	}
	quarkTempMu.Lock()
	quarkTempFIDs[fid] = time.Now()
	quarkTempMu.Unlock()
}

// quarkTempHas 判断 fid 是否在内存记账里（本次启动转存/复用过的）。
// 盘扫时用它跳过有主文件，避免误伤：复用时记账会续期，而网盘上的
// 修改时间还是转存那一刻，按 mtime 删会把正在连看的剧删掉。
func quarkTempHas(fid string) bool {
	quarkTempMu.Lock()
	defer quarkTempMu.Unlock()
	_, ok := quarkTempFIDs[fid]
	return ok
}

// quarkTempForget 从记账里移除（删除成功后调，避免 map 无限增长）。
func quarkTempForget(fid string) {
	quarkTempMu.Lock()
	delete(quarkTempFIDs, fid)
	quarkTempMu.Unlock()
}

// quarkTempReset 清空记账（账号切换时调：新号的盘里没有这些文件）。
func quarkTempReset() {
	quarkTempMu.Lock()
	quarkTempFIDs = map[string]time.Time{}
	quarkTempMu.Unlock()
}

// quarkStartSweeper 启动后台清扫（在 init() 里调用）。
func quarkStartSweeper() {
	go func() {
		// 启动自愈：清掉盘上 2 小时前的遗留文件。容器刚起这段时间不会有
		// 正在进行的播放；更近的留给常规清扫，避免打断刚拿到直链的 302 播放。
		//
		// 注意：本函数在 init() 里跑，那时 globalAccounts 还没初始化，
		// quarkClient() 返回 nil —— 必须等账号库就绪再清，否则启动清理
		// 永远静默跳过（百度那边实测踩过这个坑）。
		for i := 0; i < 60; i++ {
			if q := quarkClient(globalAccounts); q != nil {
				quarkPurgeDir(q, quarkTmpTTL)
				break
			}
			time.Sleep(2 * time.Second)
		}
		for {
			time.Sleep(10 * time.Minute)
			q := quarkClient(globalAccounts)
			if q == nil {
				continue
			}
			// ① 记账到期的先删（本次启动转存/复用的）
			quarkTempMu.Lock()
			expired := make([]string, 0)
			for fid, t := range quarkTempFIDs {
				if time.Since(t) > quarkTmpTTL {
					expired = append(expired, fid)
				}
			}
			quarkTempMu.Unlock()
			for _, fid := range expired {
				if err := quarkDeleteOwn(q, fid); err != nil {
					log.Printf("[quark-tv] 清扫临时文件失败 fid=%s: %v", fid, err)
					continue
				}
				quarkTempForget(fid)
				log.Printf("[quark-tv] 已清扫临时文件 fid=%s", fid)
			}
			// ② 盘扫补漏（只扫固定名 /bigpan_tmp/quark，不碰其他目录）：
			// 管「上次启动转存 + 本次启动没认领」的文件。误伤防护在
			// quarkPurgeDir 里：记账里有的 fid 直接跳过。
			quarkPurgeDir(q, quarkTmpTTL)
		}
	}()
}

// quarkPurgeDir 清扫临时目录：列目录 → 删 mtime 超过 minAge 且不在记账里的文件。
// 记账里的跳过（复用时续期、mtime 不变，按 mtime 删会误伤正在连看的剧）。
func quarkPurgeDir(q *Quark, minAge time.Duration) {
	fid := quarkTmpDirFID(q)
	if fid == "" {
		return // 目录还没建过（从没转存过），没什么可扫
	}
	list, err := q.ListDir(fid)
	if err != nil {
		// 目录不存在的场景：夸克 ListDir 对失效 fid 会报错，属于正常情况
		//（用户手动删过目录），重置缓存等下次转存时重建，不刷错误日志。
		quarkTmpDirReset()
		return
	}
	var freed int64
	n := 0
	for _, f := range list {
		if f.IsDir || f.FID == "" {
			continue
		}
		if quarkTempHas(f.FID) {
			continue
		}
		if f.UpdatedAt > 0 && time.Since(time.UnixMilli(f.UpdatedAt)) < minAge {
			continue
		}
		if err := quarkDeleteOwn(q, f.FID); err != nil {
			log.Printf("[quark-tv] 清扫遗留文件失败 %s: %v", f.Name, err)
			continue
		}
		freed += f.Size
		n++
	}
	if n > 0 {
		log.Printf("[quark-tv] 盘扫清扫：删除 %d 个遗留转存文件，释放 %.2f GB", n, float64(freed)/(1<<30))
	}
}

func init() {
	quarkStartSweeper()
}
