package main

// ---------------------------------------------------------------------------
// UC 临时转存文件清扫（照搬 quark_sweep.go 的成熟模式）
//
// 转存到 /bigpan_tmp/uc 的文件不能永远躺着：302 场景下峰值占用与
// 「看过哪些剧」成正比。规则与夸克一致：
//
//	① 内存记账：本次启动转存/复用过的 fid → 记录时间（复用即续期）；
//	② 定时清扫：每 10 分钟一轮，删记账超过 2 小时的；
//	③ 启动补漏：进程重启后记账清空，等账号就绪先盘扫一轮（删 mtime 超 2h 的）；
//	④ 周期补漏：每轮都盘扫固定目录，兜住「上次启动转存 + 本次没认领」。
//
// 注意：本文件所有清理只动 /bigpan_tmp/uc，绝不碰用户其他文件。
// ---------------------------------------------------------------------------

import (
	"log"
	"sync"
	"time"
)

const ucTmpTTL = 2 * time.Hour

var (
	ucTempMu   sync.Mutex
	ucTempFIDs = map[string]time.Time{}
)

// ucRecordTemp 记账一个转存文件（转存或复用时调；复用即续期）。
func ucRecordTemp(fid string) {
	if fid == "" {
		return
	}
	ucTempMu.Lock()
	ucTempFIDs[fid] = time.Now()
	ucTempMu.Unlock()
}

// ucTempHas fid 是否在内存记账里（盘扫时跳过有主文件，避免误伤连看）。
func ucTempHas(fid string) bool {
	ucTempMu.Lock()
	defer ucTempMu.Unlock()
	_, ok := ucTempFIDs[fid]
	return ok
}

// ucTempForget 从记账里移除（删除成功后调，避免 map 无限增长）。
func ucTempForget(fid string) {
	ucTempMu.Lock()
	delete(ucTempFIDs, fid)
	ucTempMu.Unlock()
}

// ucTempReset 清空记账 + 丢弃临时目录 fid 缓存（账号切换时调）。
func ucTempReset() {
	ucTempMu.Lock()
	ucTempFIDs = map[string]time.Time{}
	ucTempMu.Unlock()
	ucTmpMu.Lock()
	ucTmpFID = ""
	ucTmpMu.Unlock()
	ucLinkMu.Lock()
	ucLinkCache = map[string]ucLinkEntry{}
	ucLinkMu.Unlock()
}

// ucDeleteOwn 删除自己盘里的单个文件。
func ucDeleteOwn(pc *UC, fid string) error {
	return pc.Delete([]string{fid})
}

// ucStartSweeper 启动后台清扫（在 init() 里调用）。
func ucStartSweeper() {
	goSafeLoop("UC临时文件清扫", 10*time.Minute, func() {
		// 等账号库就绪（init 阶段 globalAccounts 还是 nil，直接调会永远跳过）
		for i := 0; i < 60; i++ {
			if pc := ucClient(globalAccounts); pc != nil {
				ucPurgeDir(pc, ucTmpTTL)
				log.Printf("[uc-tv] 清扫器就绪：每 10 分钟清理 %s 中超过 %s 的转存文件", ucTmpDirName(), ucTmpTTL)
				break
			}
			time.Sleep(2 * time.Second)
		}
		for {
			time.Sleep(10 * time.Minute)
			pc := ucClient(globalAccounts)
			if pc == nil {
				continue
			}
			// ① 记账到期的先删
			ucTempMu.Lock()
			expired := make([]string, 0)
			for fid, t := range ucTempFIDs {
				if time.Since(t) > ucTmpTTL {
					expired = append(expired, fid)
				}
			}
			ucTempMu.Unlock()
			for _, fid := range expired {
				if err := ucDeleteOwn(pc, fid); err != nil {
					log.Printf("[uc-tv] 清扫临时文件失败 fid=%s: %v", fid, err)
					continue
				}
				ucTempForget(fid)
				log.Printf("[uc-tv] 已清扫临时文件 fid=%s", fid)
			}
			// ② 盘扫补漏（只扫 /bigpan_tmp/uc）
			ucPurgeDir(pc, ucTmpTTL)
		}
	})
}

// ucTmpDirLookup 按路径查 /bigpan_tmp/uc 的 fid（只查不建）。
// 进程重启后内存里的 ucTmpFID 丢了，靠它把老目录认回来，否则启动清扫形同虚设。
func ucTmpDirLookup(pc *UC) string {
	root, err := pc.ListDir(ucRootFID)
	if err != nil {
		return ""
	}
	tmp := ""
	for _, f := range root {
		if f.IsDir && f.Name == ucTmpRoot {
			tmp = f.FID
			break
		}
	}
	if tmp == "" {
		return ""
	}
	list, err := pc.ListDir(tmp)
	if err != nil {
		return ""
	}
	for _, f := range list {
		if f.IsDir && f.Name == ucTmpName {
			return f.FID
		}
	}
	return ""
}

// ucPurgeDir 清扫临时目录：删 mtime 超过 minAge 且不在记账里的文件。
func ucPurgeDir(pc *UC, minAge time.Duration) {
	ucTmpMu.Lock()
	fid := ucTmpFID
	ucTmpMu.Unlock()
	if fid == "" {
		// 重启后内存缓存为空：按路径认回老目录；没建过就跳过
		//（绝不能退回 ListDir(0) 去扫用户根目录）。
		fid = ucTmpDirLookup(pc)
		if fid == "" {
			return
		}
		ucTmpMu.Lock()
		ucTmpFID = fid
		ucTmpMu.Unlock()
	}
	list, err := pc.ListDir(fid)
	if err != nil {
		// 用户手动删过目录：丢弃缓存，等下次转存重建。
		ucTmpMu.Lock()
		ucTmpFID = ""
		ucTmpMu.Unlock()
		return
	}
	var freed int64
	n := 0
	for _, f := range list {
		if f.IsDir || f.FID == "" {
			continue
		}
		if ucTempHas(f.FID) {
			continue
		}
		if f.Updated > 0 && time.Since(time.UnixMilli(f.Updated)) < minAge {
			continue
		}
		if err := ucDeleteOwn(pc, f.FID); err != nil {
			log.Printf("[uc-tv] 清扫遗留文件失败 %s: %v", f.Name, err)
			continue
		}
		freed += f.Size
		n++
	}
	if n > 0 {
		log.Printf("[uc-tv] 盘扫清扫：删除 %d 个遗留转存文件，释放 %.2f GB", n, float64(freed)/(1<<30))
	}
}

func init() {
	ucStartSweeper()
}
