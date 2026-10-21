package main

import (
	"fmt"
)

// ---------------------------------------------------------------------------
// 管理界面
//
// 浅色 + 左侧导航，四个页面：概览 / 分享导入 / 媒体库 / 设置。
// 原生 HTML+JS，无构建步骤，跟随二进制一起发布。
// JS 里刻意不用模板字符串（反引号），避免和 Go 原始字符串冲突。
// 设计基准：OpenList / Naive UI —— 卡片、矢量图标、加载态、空状态、
// 自定义确认弹窗（不用浏览器原生 confirm）、过渡动画、移动端自适应。
// ---------------------------------------------------------------------------

// humanSize 把字节数格式化成易读字符串。
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// renderIndex 渲染管理界面。
func renderIndex(store *Store, as *AccountStore, self string) string {
	page := `<!doctype html>
<html lang="zh-CN"><head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1,viewport-fit=cover">
<title>BigPan · 无限空间，聚合分享！</title>
<link rel="icon" href="data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 24 24' fill='none' stroke='%232080f0' stroke-width='2' stroke-linecap='round' stroke-linejoin='round'%3E%3Cpath d='M5 17h14l-1.5-6.5a2 2 0 0 0-2-1.5h-7a2 2 0 0 0-2 1.5Z'/%3E%3Cpath d='M12 9V4m0 0 3 3m-3-3L9 7'/%3E%3C/svg%3E">
<style>
/* ============ 设计令牌（Naive UI 风格） ============ */
:root{
  --bg:#f5f6f8; --card:#fff; --line:#e7e9ee; --line2:#f0f1f4;
  --txt:#1f2329; --txt2:#4b5160; --mut:#767d8c;
  --pri:#2080f0; --pri-h:#1a6dd8; --pri2:#e9f2ff; --pri-line:#bcd7fb;
  --ok:#18a058; --warn:#f0a020; --danger:#d03050; --danger2:#fbe8ec;
  --sh1:0 1px 2px rgba(16,24,40,.05),0 1px 3px rgba(16,24,40,.04);
  --sh2:0 4px 14px rgba(16,24,40,.09);
  --r:10px;
}
*{box-sizing:border-box;margin:0;padding:0;-webkit-tap-highlight-color:transparent}
html{-webkit-text-size-adjust:100%}
body{font-family:-apple-system,BlinkMacSystemFont,"PingFang SC","HarmonyOS Sans SC","Microsoft YaHei",sans-serif;
  background:radial-gradient(1100px 560px at 12% -4%,#e4eeff 0%,rgba(228,238,255,0) 60%),
             radial-gradient(900px 520px at 92% 96%,#eef0ff 0%,rgba(238,240,255,0) 55%),var(--bg);
  background-attachment:fixed;color:var(--txt);font-size:14.5px;line-height:1.6}
::-webkit-scrollbar{width:8px;height:8px}
::-webkit-scrollbar-thumb{background:#d3d7de;border-radius:4px;border:2px solid var(--bg)}
::-webkit-scrollbar-thumb:hover{background:#b9bec7}
::selection{background:#cfe3ff}
input[type=checkbox]{accent-color:var(--pri);width:15px;height:15px;cursor:pointer;vertical-align:-2px}

/* ============ 布局 ============ */
.layout{display:flex;min-height:100vh}
.side{width:216px;background:#fff;border-right:1px solid var(--line);flex-shrink:0;position:sticky;top:0;height:100vh;overflow:auto;display:flex;flex-direction:column;padding:20px 12px 14px}
.brand{display:flex;align-items:center;gap:10px;padding:2px 8px 20px}
.bico{width:34px;height:34px;border-radius:9px;background:linear-gradient(135deg,#2080f0,#5a9cf8);display:flex;align-items:center;justify-content:center;flex-shrink:0;box-shadow:0 2px 6px rgba(32,128,240,.35)}
.bico svg{width:19px;height:19px;stroke:#fff}
.brand b{font-size:16.5px;font-weight:700;letter-spacing:.2px;display:block;line-height:1.2}
.brand small{display:block;font-size:11px;color:var(--mut);font-weight:400}
.nav{display:flex;flex-direction:column;gap:3px}
.nav .grp{font-size:11.5px;color:#a9afba;padding:12px 12px 5px;letter-spacing:.5px}
.nav a{display:flex;align-items:center;gap:10px;padding:10.5px 12px;border-radius:8px;color:var(--txt2);text-decoration:none;cursor:pointer;font-size:16px;font-weight:500;position:relative;transition:background .15s,color .15s}
.nav a svg{width:19px;height:19px;stroke:currentColor;flex-shrink:0;opacity:.85}
.nav a:hover{background:#f3f5f8;color:var(--txt)}
.nav a.on{background:var(--pri2);color:var(--pri);font-weight:600}
.nav a.on::before{content:'';position:absolute;left:-12px;top:20%;height:60%;width:3px;border-radius:0 3px 3px 0;background:var(--pri)}
.nav a:active{transform:scale(.985)}
.side-ft{margin-top:auto;padding:12px 10px 0;border-top:1px solid var(--line2);font-size:12px;color:var(--mut)}
.side-ft .dot{display:inline-block;width:7px;height:7px;border-radius:50%;background:var(--ok);margin-right:5px;vertical-align:1px;box-shadow:0 0 0 3px rgba(24,160,88,.15)}

.main{flex:1;padding:26px 30px;max-width:1100px;min-width:0}
.pagehd{display:flex;align-items:center;gap:12px;margin-bottom:20px;flex-wrap:wrap}
h2{font-size:21px;font-weight:700;letter-spacing:.2px}
.sub{color:var(--txt2);font-size:14px;margin-bottom:18px}
.pagehd .sub{margin:0}
.pg{animation:fadein .26s ease}
@keyframes fadein{from{opacity:0;transform:translateY(7px)}to{opacity:1;transform:none}}

/* ============ 卡片 ============ */
.card{background:var(--card);border:1px solid var(--line);border-radius:12px;padding:18px 20px;margin-bottom:16px;box-shadow:var(--sh1);transition:box-shadow .2s}
.card:hover{box-shadow:var(--sh2)}
.card>h3,.card>b{font-size:15px;font-weight:600}
/* 卡片头：标题 + 右侧说明，统一各卡片标题区的排版 */
.card .cd{display:block;color:var(--mut);font-size:13px;font-weight:400;margin-top:3px;line-height:1.6}
.card .chd{align-items:center}
/* 提示条：把散落各处的 inline style 提示收敛成统一组件 */
.tips{margin:12px 0 0;padding:11px 14px;background:var(--pri2);border:1px solid #d6e2ff;border-radius:9px;font-size:13px;line-height:1.75;color:#33507a}
.tips.warn{background:#fdf6ea;border-color:#f3ddb4;color:#8a5a12}
/* 开关行：设置项统一排版——左文右开关，原理解释默认折叠，点开才看 */
.swrow{display:flex;align-items:flex-start;gap:14px;padding:14px 0;border-bottom:1px solid var(--line2)}
.swrow:first-of-type{padding-top:10px}
.swrow:last-child{border-bottom:none;padding-bottom:2px}
.swmain{flex:1;min-width:0}
.swt{font-size:14px;font-weight:600}
.swd{color:var(--txt2);font-size:13px;line-height:1.65;margin-top:3px}
.swmore{margin-top:6px}
.swmore summary{color:var(--pri);font-size:12.5px;cursor:pointer;user-select:none;list-style:none;display:inline-block}
.swmore summary::-webkit-details-marker{display:none}
.swmore summary::before{content:'▸ '}
.swmore[open] summary::before{content:'▾ '}
.swmore>div{margin-top:7px;padding:10px 13px;background:#f6f8fc;border:1px solid var(--line);border-radius:9px;font-size:12.5px;line-height:1.8;color:var(--txt2)}
/* iOS 风开关：label 包 input + i，点整行都能切 */
.sw{position:relative;flex:none;display:inline-block;width:46px;height:26px;margin-top:1px;cursor:pointer}
.sw input{opacity:0;width:0;height:0;position:absolute}
.sw i{position:absolute;inset:0;background:#cbd5e1;border-radius:26px;transition:background .2s;pointer-events:none}
.sw i::before{content:'';position:absolute;width:20px;height:20px;left:3px;top:3px;background:#fff;border-radius:50%;transition:transform .2s;box-shadow:0 1px 3px rgba(0,0,0,.28)}
.sw input:checked+i{background:var(--pri)}
.sw input:checked+i::before{transform:translateX(20px)}
.tips b{color:inherit}
/* 警示横幅（地址变了等需要用户注意的场景） */
.banner{margin:12px 0 0;padding:12px 14px;background:var(--danger2);border:1px solid #f0c4cd;border-radius:9px;font-size:13.5px;line-height:1.75;color:#a12842}
/* 媒体库批量操作浮条（勾选后从底部升起） */
.selbar{position:fixed;left:50%;bottom:22px;transform:translate(-50%,14px);z-index:80;display:none;align-items:center;gap:10px;
  padding:10px 12px 10px 18px;background:rgba(255,255,255,.9);backdrop-filter:blur(10px);border:1px solid var(--line);
  border-radius:12px;box-shadow:0 10px 32px rgba(16,24,40,.16);font-size:13.5px;opacity:0;transition:opacity .18s,transform .18s}
.selbar.show{opacity:1;transform:translate(-50%,0)}
.selbar b{font-weight:600;white-space:nowrap}
.sp{flex:1}
.stat{display:flex;gap:14px;flex-wrap:wrap;margin-bottom:16px}
.stat .card{flex:1;min-width:140px;margin:0;padding:20px 14px 18px;text-align:center;cursor:default}
.stat b{font-size:28px;font-weight:700;color:var(--pri);font-variant-numeric:tabular-nums;display:block;line-height:1.2;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
/* 标签做成浅蓝底胶囊：蓝字加粗，比裸灰字醒目 */
.stat span{display:inline-block;color:var(--pri);font-size:14px;font-weight:600;margin-top:10px;padding:4px 15px;border-radius:20px;background:#e8f1ff;border:1px solid #c9dcf9;line-height:1.5;letter-spacing:.2px}
.stat span.plain{display:block;background:none;border:0;padding:0}
.stat .si{width:36px;height:36px;border-radius:10px;display:flex;align-items:center;justify-content:center;margin:0 auto 10px}
.stat .si svg{width:18px;height:18px;stroke:currentColor}
.si.b{background:#e8f1ff;color:#1d6fd8}.si.p{background:#f1ebff;color:#7c3aed}
.si.o{background:#fff3e2;color:#e8890c}.si.g{background:#e6f7ee;color:#18a058}

/* ============ 新用户引导（三步走） ============ */
.guide{display:flex;gap:0;flex-wrap:wrap}
.gstep{flex:1;min-width:200px;padding:6px 18px 6px 0;position:relative}
.gstep+.gstep{border-left:1px dashed var(--line);padding-left:22px}
.gn{display:flex;align-items:center;gap:9px;font-weight:600;font-size:14px;margin-bottom:5px}
.gn .no{width:22px;height:22px;border-radius:50%;background:var(--pri);color:#fff;font-size:12px;font-weight:700;display:flex;align-items:center;justify-content:center;flex-shrink:0;box-shadow:0 2px 6px rgba(32,128,240,.35)}
.gstep p{color:var(--mut);font-size:13px;line-height:1.7;margin:0}
.gstep .go{margin-top:9px}
.gstep .go button{padding:6px 15px;font-size:13px}

/* ============ 控件 ============ */
input,button,select{font:inherit}
input[type=text]{width:100%;padding:9px 13px;border:1px solid #dcdfe6;border-radius:8px;background:#fff;outline:none;transition:border-color .15s,box-shadow .15s;color:var(--txt)}
input[type=text]::placeholder{color:#aeb4bf}
input[type=text]:hover{border-color:#c2c7d0}
input[type=text]:focus{border-color:var(--pri);box-shadow:0 0 0 3px rgba(32,128,240,.13)}
select{padding:9px 13px;border:1px solid #dcdfe6;border-radius:8px;background:#fff;outline:none;cursor:pointer;transition:border-color .15s,box-shadow .15s;color:var(--txt)}
select:focus{border-color:var(--pri);box-shadow:0 0 0 3px rgba(32,128,240,.13)}
button{background:var(--pri);color:#fff;border:0;border-radius:8px;padding:9px 20px;cursor:pointer;font-weight:600;font-size:14px;transition:background .15s,box-shadow .15s,transform .06s;box-shadow:0 1px 2px rgba(32,128,240,.3)}
button:hover{background:var(--pri-h)}
button:active{transform:scale(.965)}
button.gray{background:#fff;color:var(--txt2);border:1px solid #dcdfe6;box-shadow:none;font-weight:500}
button.gray:hover{color:var(--pri);border-color:var(--pri-line);background:var(--pri2)}
/* 状态按钮（追更中）：浅蓝底，一眼是"状态"而非"操作"，不和行内操作按钮抢眼 */
button.ston{background:var(--pri2);color:var(--pri);border:1px solid var(--pri-line);box-shadow:none;font-weight:600}
button.ston:hover{background:#d8e8ff;color:var(--pri-h)}
button.danger{background:#fff;color:var(--danger);border:1px solid #f0c4cd;box-shadow:none;font-weight:500}
button.danger:hover{background:var(--danger2);border-color:var(--danger)}
button:disabled{opacity:.45;cursor:default;transform:none}
button.mini{padding:4.5px 12px;font-size:13.5px;border-radius:7px;font-weight:500;white-space:nowrap}
button:focus-visible,input:focus-visible,select:focus-visible,a:focus-visible{outline:2px solid var(--pri);outline-offset:1px}
/* 复制小按钮：贴在 code 地址右侧 */
.copybtn{display:inline-flex;align-items:center;gap:5px;margin-left:8px;padding:3px 11px;font-size:12.5px;font-weight:500;border-radius:7px;vertical-align:1px}
.copybtn svg{width:13px;height:13px;stroke:currentColor;flex-shrink:0}
.row{display:flex;gap:10px;flex-wrap:wrap;align-items:center}

/* 搜索框（媒体库筛选） */
.srch{position:relative;flex:1;min-width:170px;max-width:260px}
.srch svg{position:absolute;left:11px;top:50%;transform:translateY(-50%);width:15px;height:15px;stroke:#9aa1ad;pointer-events:none}
.srch input{padding-left:34px}

/* ============ 表格 ============ */
table{width:100%;border-collapse:collapse}
th{font-size:13.5px;color:var(--mut);text-align:left;padding:7px 12px;border-bottom:1px solid var(--line);font-weight:600;background:#fafbfc;white-space:nowrap}
td{padding:9px 13px;border-bottom:1px solid var(--line2);font-size:14px;color:var(--txt);vertical-align:middle}
td.brk{word-break:break-all}
td.num{text-align:right;font-variant-numeric:tabular-nums;white-space:nowrap}
td.tm{text-align:right;color:var(--mut);font-size:13px;white-space:nowrap;font-variant-numeric:tabular-nums}
table tr:last-child td{border-bottom:0}
tbody tr,table tr{transition:background .12s}
table tr:hover td{background:#f7f9fc}
.tdpath{display:inline-block;max-width:560px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;vertical-align:-2px}
.tmeta{font-size:12.5px;color:var(--mut);margin-top:2px;line-height:1.45;word-break:break-all}
.chd{display:flex;align-items:baseline;gap:9px;flex-wrap:wrap}
.chd b{font-size:14px}
.chd .csub{font-size:12.5px;color:var(--mut)}
.rowlk{color:var(--txt);text-decoration:none;font-weight:500}
.rowlk:hover{color:var(--pri)}
@media(max-width:720px){
  .rowlk{display:inline-block;max-width:42vw;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;vertical-align:bottom}
  th,td{padding:8px 8px}
  .tdpath{max-width:180px}}

/* ============ 媒体库（OpenList 风格：面包屑 + 逐层进入 + 勾选批量 + 整剧删除） ============ */
.crumb{display:flex;align-items:center;gap:2px;flex-wrap:wrap;padding:2px 8px 16px;font-size:14.5px}
.crumb .cb{display:inline-flex;align-items:center;gap:6px;color:var(--mut);text-decoration:none;padding:5px 10px;border-radius:8px;transition:background .13s,color .13s;max-width:46vw;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.crumb .cb:hover{background:var(--pri2);color:var(--pri)}
.crumb .cb.cur{color:var(--txt);font-weight:600;background:#f2f4f8}
.crumb .cb svg{width:15px;height:15px;stroke:currentColor;flex-shrink:0}
.crumb .csep{color:#c3c9d2;font-size:13.5px;padding:0 2px}
/* 列表视图 */
.lvt{width:100%;border-collapse:collapse;font-size:14px;table-layout:fixed}
.lvt thead th{font-size:13.5px;color:var(--mut);text-align:left;padding:9px 14px;border-bottom:1px solid var(--line);font-weight:600;background:#fafbfc;white-space:nowrap}
.lvt td{padding:11px 14px;border-bottom:1px solid var(--line2);font-size:14px;vertical-align:middle}
.lvt tbody tr:last-child td{border-bottom:0}
.lvt tbody tr{transition:background .12s}
.lvt tbody tr:hover td{background:#f6f9ff}
.lvt .ckc{width:40px;padding-left:14px;padding-right:0}
.fdi{width:21px;height:21px;flex-shrink:0;fill:none;stroke:#4c8bf5;stroke-width:1.8;stroke-linecap:round;stroke-linejoin:round}
.fdi.f{fill:#4c8bf5;stroke:none}
.fv{display:flex;align-items:center;gap:11px;cursor:pointer;width:100%;min-width:0;color:var(--txt)}
.fv b{flex:1;min-width:0;font-weight:600;font-size:14.5px;line-height:1.3;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.fv .nm{display:flex;align-items:baseline;gap:5px;min-width:0;flex:1}
.fv .nm b{flex:0 1 auto}
.fv:hover b{color:var(--pri)}
.fv.play .fdi{stroke:var(--pri)}
.libsz{color:var(--mut);font-variant-numeric:tabular-nums;white-space:nowrap;text-align:right}
.libtm{color:var(--mut);font-size:13.5px;white-space:nowrap;font-variant-numeric:tabular-nums;text-align:right}
.dircnt{color:var(--mut);font-size:12.5px;font-weight:500;white-space:nowrap;flex:0 0 auto}
.ops{text-align:right;width:130px;white-space:nowrap}
.acts{opacity:0;transition:opacity .12s;display:inline-flex;gap:6px}
.lvt tbody tr:hover .acts{opacity:1}
/* 网格视图 */
.gridview{display:grid;grid-template-columns:repeat(auto-fill,minmax(150px,1fr));gap:12px;padding:8px}
.gcard{border:1px solid var(--line2);border-radius:12px;padding:18px 14px;cursor:pointer;background:#fff;text-align:center;transition:border-color .15s,box-shadow .15s,transform .15s}
.gcard:hover{border-color:var(--pri-line);box-shadow:0 6px 18px rgba(32,128,240,.12);transform:translateY(-1px)}
.gcard .fdi{width:34px;height:34px;stroke-width:1.6;margin:0 auto;display:block}
.gcard .gn{font-size:13.5px;font-weight:500;line-height:1.5;margin-top:11px;max-height:3.9em;overflow:hidden;word-break:break-all}
.gcard .gs{font-size:12.5px;color:var(--mut);margin-top:6px}
.tag{display:inline-flex;align-items:center;background:var(--pri2);color:var(--pri);border-radius:6px;font-size:12px;padding:2.5px 9px;font-weight:600;white-space:nowrap}
.tag.q{background:#e8f1ff;color:#1d6fd8}.tag.n{background:#fff2e0;color:#d97706}.tag.b{background:#e8f1ff;color:#1d6fd8}
.tag.m{background:#e6f7ee;color:#18a058}.tag.t{background:#e8f8fb;color:#0891b2}
/* 设置页「网盘账号」：盘名放大一倍（12→24px），表格里一眼能认 */
#accts .tag{font-size:24px;padding:5px 16px;border-radius:9px;line-height:1.3}

/* ============ 提示 / 弹层 ============ */
.toast{position:fixed;top:26px;left:50%;transform:translate(-50%,-16px);background:rgba(28,32,38,.92);backdrop-filter:blur(6px);color:#fff;padding:11px 22px;border-radius:10px;opacity:0;transition:all .28s cubic-bezier(.2,.8,.3,1);pointer-events:none;z-index:99;font-size:14px;box-shadow:0 6px 20px rgba(0,0,0,.25);border-left:3px solid var(--pri);max-width:82vw}
.toast.show{opacity:1;transform:translate(-50%,0)}
/* 自定义确认弹窗（替代浏览器原生 confirm） */
.modal{position:fixed;inset:0;background:rgba(15,20,30,.42);backdrop-filter:blur(4px);display:none;align-items:center;justify-content:center;z-index:120;padding:20px;opacity:0;transition:opacity .18s}
.modal.show{opacity:1}
.mbox{background:#fff;border-radius:14px;width:100%;max-width:400px;padding:22px;box-shadow:0 24px 60px rgba(15,20,30,.28);transform:translateY(12px) scale(.97);transition:transform .22s cubic-bezier(.2,.9,.3,1.2)}
.modal.show .mbox{transform:none}
.mtitle{font-size:16px;font-weight:700;display:flex;align-items:center;gap:10px}
.mtitle .mi{width:30px;height:30px;border-radius:9px;background:var(--danger2);color:var(--danger);display:flex;align-items:center;justify-content:center;flex-shrink:0}
.mtitle .mi.ok{background:var(--pri2);color:var(--pri)}
.mtitle .mi svg{width:16px;height:16px;stroke:currentColor}
.mbody{color:var(--txt2);font-size:14px;margin:13px 0 20px;line-height:1.65;word-break:break-all;white-space:pre-wrap}
.mfoot{display:flex;justify-content:flex-end;gap:10px}
.mfoot button{min-width:76px}
/* 播放器 */
.player{position:fixed;inset:0;background:rgba(12,16,24,.66);backdrop-filter:blur(8px);display:none;align-items:center;justify-content:center;z-index:90;padding:20px}
.player .box{background:#0e1522;border-radius:16px;max-width:880px;width:100%;padding:16px;box-shadow:0 24px 60px rgba(0,0,0,.45);animation:pop .22s cubic-bezier(.2,.9,.3,1.2);position:relative}
@keyframes pop{from{opacity:0;transform:scale(.94) translateY(10px)}to{opacity:1;transform:none}}
.vwrap{position:relative;border-radius:10px;overflow:hidden;background:#000}
video{width:100%;max-height:66vh;display:block;background:#000}
.pvmsg{position:absolute;inset:0;display:none;align-items:center;justify-content:center;color:#e8ecf3;background:rgba(10,14,22,.78);font-size:13.5px;text-align:center;padding:24px;line-height:1.7}
.pvmsg .spin{border-color:rgba(255,255,255,.3);border-top-color:#fff;width:22px;height:22px;border-width:2.5px;display:block;margin:0 auto 12px}
.pvmsg small{color:#9aa4b3;font-size:12px}
.pbar{display:flex;gap:8px;margin-top:12px;align-items:center;color:#c4cbd8;font-size:12.5px;word-break:break-all}
.pbar button{background:#212b3d}
.pbar button:hover{background:#2c3a54}
code{background:#f2f4f8;border:1px solid var(--line2);padding:2.5px 9px;border-radius:6px;font-size:13px;word-break:break-all;color:var(--txt2);font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}
.spin{display:inline-block;width:15px;height:15px;border:2px solid var(--pri-line);border-top-color:var(--pri);border-radius:50%;animation:rot .7s linear infinite;vertical-align:-2px;margin-right:7px}
@keyframes rot{to{transform:rotate(360deg)}}
/* 加载态 / 空状态 */
.loading{padding:46px 10px;text-align:center;color:var(--mut);font-size:13.5px}
.loading .spin{width:22px;height:22px;border-width:2.5px;margin:0 auto 12px;display:block}
.skel{height:86px;border-radius:12px;flex:1;min-width:140px;border:1px solid var(--line);background:linear-gradient(90deg,#eef0f4 25%,#f6f7f9 37%,#eef0f4 63%);background-size:400% 100%;animation:sh 1.2s ease infinite}
@keyframes sh{from{background-position:100% 50%}to{background-position:0 50%}}
.empty{padding:48px 16px;text-align:center;color:var(--mut);font-size:13.5px}
.empty .ei{width:46px;height:46px;border-radius:12px;background:#f2f4f8;display:flex;align-items:center;justify-content:center;margin:0 auto 12px}
.empty .ei svg{width:22px;height:22px;stroke:#b6bcc7}
.empty b{display:block;color:var(--txt2);font-size:14.5px;margin-bottom:4px;font-weight:600}

/* ============ 移动端 ============ */
@media(max-width:720px){
  .layout{flex-direction:column}
  .side{width:100%;height:auto;position:static;display:flex;flex-direction:column;padding:10px 12px;border-right:0;border-bottom:1px solid var(--line)}
  .brand{padding:2px 6px 10px}
  .nav{flex-direction:row;gap:2px;overflow-x:auto}
  .nav .grp{display:none}
  .nav a{padding:9px 12px;white-space:nowrap;font-size:15px}
  .nav a.on::before{display:none}
  .side-ft{display:none}
  .main{padding:16px 14px}
  h2{font-size:18px}
  .stat b{font-size:24px}
  .pagehd{gap:8px}
  .srch{max-width:none}
  .lvt tbody tr .acts{opacity:1}
  /* 媒体库窄屏改为卡片列表：名称独占一行，不再被固定列宽压垮 */
  .lvt,.lvt tbody,.lvt tr{display:block;width:auto}
  .lvt thead{display:none}
  .lvt tbody tr{display:grid;grid-template-columns:24px minmax(0,1fr) auto auto;grid-template-areas:"ck name name ops" "ck sz tm ops";align-items:center;column-gap:10px;row-gap:4px;padding:11px 8px;border-bottom:1px solid var(--line2);position:relative}
  .lvt tbody tr:last-child{border-bottom:0}
  .lvt td{display:block;width:auto!important;padding:0;border:0;font-size:14px}
  .lvt td.ckc{grid-area:ck;width:auto;padding:0}
  .lvt td:nth-child(2){grid-area:name;min-width:0}
  .lvt td.libsz{grid-area:sz;font-size:12.5px;margin-right:10px}
  .lvt td.libtm{grid-area:tm;font-size:12.5px}
  .lvt td.ops{grid-area:ops;width:auto;text-align:right}
  /* 目录行窄屏收成单行：项数已内联在名称后，不再占第二行 */
  .lvt tbody tr.dirrow{grid-template-areas:"ck name name ops"}
  .lvt tbody tr.dirrow td.libsz,.lvt tbody tr.dirrow td.libtm{display:none}
  .lvt .ops .acts{opacity:1;gap:5px}
  .lvt .ops .acts .mini{padding:5px 11px;font-size:13px}
  .selbar{left:12px;right:12px;bottom:12px;transform:translateY(14px);justify-content:center}
  .selbar.show{transform:translateY(0)}
}
</style></head>
<body>
<div class="layout">
  <div class="side">
    <div class="brand">
      <span class="bico"><svg viewBox="0 0 24 24" fill="none" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M5 17h14l-1.5-6.5a2 2 0 0 0-2-1.5h-7a2 2 0 0 0-2 1.5Z"/><path d="M12 9V4m0 0 3 3m-3-3L9 7"/></svg></span>
      <span><b>BigPan</b><small>无限空间，聚合分享！</small></span>
    </div>
    <div class="nav">
      <div class="grp">总览</div>
      <a data-p="home" class="on"><svg viewBox="0 0 24 24" fill="none" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M3 10.5 12 3l9 7.5"/><path d="M5 9.5V21h14V9.5"/></svg><span>概览</span></a>
      <div class="grp">资源</div>
      <a data-p="import"><svg viewBox="0 0 24 24" fill="none" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 3v12m0 0 4.5-4.5M12 15 7.5 10.5"/><path d="M4 16.5V19a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2v-2.5"/></svg><span>分享导入</span></a>
      <a data-p="tianyi-sub"><svg viewBox="0 0 24 24" fill="none" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M4 6.5A2.5 2.5 0 0 1 6.5 4h11A2.5 2.5 0 0 1 20 6.5v7a2.5 2.5 0 0 1-2.5 2.5H12l-4.5 4v-4H6.5A2.5 2.5 0 0 1 4 13.5Z"/><path d="M8.5 8.5h7M8.5 11.5h4.5"/></svg><span>天翼订阅</span></a>
      <a data-p="lib"><svg viewBox="0 0 24 24" fill="none" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="4" width="18" height="16" rx="2.5"/><path d="M7.5 4v16M16.5 4v16M3 9.5h4.5M3 14.5h4.5M16.5 9.5H21M16.5 14.5H21"/></svg><span>媒体库</span></a>
      <div class="grp">系统</div>
      <a data-p="set"><svg viewBox="0 0 24 24" fill="none" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M4 7.5h9M17.5 7.5H20M4 16.5h3M11.5 16.5H20"/><circle cx="15" cy="7.5" r="2.4"/><circle cx="9" cy="16.5" r="2.4"/></svg><span>设置</span></a>
    </div>
    <div class="side-ft"><span class="dot"></span>服务运行中 · 夸克 / 115 / 移动</div>
  </div>
  <div class="main">
    <div id="pg-home" class="pg">
      <div class="pagehd"><h2>概览</h2><span class="sub">文件始终在网盘里，不占你一点空间</span></div>
      <div class="stat" id="stats"></div>
      <div class="card"><div class="chd"><b>WebDAV 挂载地址</b><span class="csub">填进爆米花 / rclone 等客户端，可列目录、可读文件</span></div>
        <div class="row" style="margin-top:12px;align-items:center">
          <code id="davUrl" style="flex:1;min-width:0;overflow:auto;white-space:nowrap"></code>
          <button class="gray copybtn" onclick="copyText('davUrl',this)" title="复制这个地址">复制</button>
        </div>
      </div>
      <div class="card" id="guide" style="display:none">
        <div class="chd"><b>第一次用？三步走完</b><span class="csub">不用看文档，跟着点就行</span></div>
        <div class="guide" style="margin-top:14px">
          <div class="gstep">
            <div class="gn"><span class="no">1</span>粘贴分享链接</div>
            <p>去网盘复制一个分享链接（夸克 / 115 / 天翼 / 移动 / 百度都支持），粘进「分享导入」页，解析后点「加入虚拟盘」。</p>
            <div class="go"><button class="gray" onclick="nav('import')">去导入 →</button></div>
          </div>
          <div class="gstep">
            <div class="gn"><span class="no">2</span>看看收进来了什么</div>
            <p>「媒体库」页按 网盘 / 分类 / 剧名 逐层点进去，和本地文件夹一样用；视频文件可以直接点开试播。</p>
            <div class="go"><button class="gray" onclick="nav('lib')">去媒体库 →</button></div>
          </div>
          <div class="gstep">
            <div class="gn"><span class="no">3</span>接上你的播放器</div>
            <p>想用飞牛影视 / Emby 看，去「设置」按「STRM 导出」的说明配一遍；文件始终在你的网盘里，不占本机空间。</p>
            <div class="go"><button class="gray" onclick="nav('set')">去设置 →</button></div>
          </div>
        </div>
      </div>
      <div class="card"><div class="chd"><b>最近导入</b><span class="csub">按分享聚合 · 一行就是一部剧或一部电影</span></div><table id="recent" style="margin-top:10px"></table></div>
    </div>
    <div id="pg-import" style="display:none">
      <div class="pagehd"><h2>分享导入</h2><span class="sub">粘贴链接 → 按目录树收进虚拟盘 → 自动追更</span></div>
      <div class="card">
        <div class="chd"><b>粘贴分享链接</b><span class="csub">一条多条都行，一行一条，可混着粘</span></div>
        <textarea id="batchText" placeholder="https://cloud.189.cn/t/xxxx（访问码：abcd）&#10;https://pan.quark.cn/s/xxxx&#10;https://pan.baidu.com/s/xxxx?pwd=abcd" style="width:100%;height:104px;margin-top:10px;box-sizing:border-box;font-size:13px;line-height:1.6;padding:8px;border-radius:8px;border:1px solid var(--line);background:var(--bg2);color:var(--fg)"></textarea>
        <div class="row" style="margin-top:10px;gap:10px;align-items:center;flex-wrap:wrap">
          <button onclick="batchParse()">解析</button>
          <input type="text" id="sharePwd" placeholder="提取码(选填)" style="width:140px">
          <span class="sub">支持 夸克 / 百度 / 115 / 移动 / 天翼。访问码直接跟在链接后面（如「（访问码：xx11）」）；只粘一条时也可以填在左边框里。</span>
        </div>
        <div id="batchOut" style="margin-top:12px"></div>
      </div>
      <div class="card">
        <b style="font-size:14px">已导入的分享（追更）<span class="cd">一个分享链接 = 一部剧 / 一部电影 · 追更中的会自动收新剧集</span></b>
        <table id="shareList" style="margin-top:8px"></table>
        <div id="sharePager" style="display:flex;align-items:center;justify-content:flex-end;gap:8px;margin-top:10px"></div>
        <div class="sub" style="margin:8px 0 0">分享主追更后新增了剧集，点「刷新」重扫目录树 —— 新文件自动入库，已入库的保持不变、路径绝不变化。</div>
        <div class="tips" style="margin:8px 0 0">行内按钮说明：<b>「刷新」</b>重扫这部剧有没有新剧集；<b>「分类」</b>事后补救——导入时分类选错了就在这里改，这部剧的所有文件会在媒体库里搬到新分类目录（已入库的剧集照常播放，不受影响），以后追更出来的新剧集也归到新分类；<b>「已失效」</b>分享挂了对它做失效清理；<b>「隐藏」</b>从列表拿掉但文件留在盘上。</div>
        <div class="sub" id="healthTip" style="margin:10px 0 0;display:flex;align-items:center;gap:8px;flex-wrap:wrap"></div>
      </div>
    </div>
    <div id="pg-tianyi-sub" style="display:none">
      <div class="pagehd"><h2>天翼订阅</h2><span class="sub">盯住资源号，新分享自动收进虚拟盘</span></div>
      <div class="card">
        <div class="chd"><b>添加订阅号</b><span class="csub">天翼 App → 订阅号主页 → 右上角分享 → 复制链接</span></div>
        <div class="row" style="margin-top:12px">
          <input type="text" id="tsLink" placeholder="粘贴订阅号链接（…?uuid=xxxx）或 32 位订阅号 ID" style="flex:1;min-width:240px">
          <input type="text" id="tsCat" placeholder="收录到分类(选填)" style="width:160px">
          <label style="display:flex;align-items:center;gap:6px;font-size:13px;white-space:nowrap"><input type="checkbox" id="tsAuto" checked>自动收录新分享</label>
          <button onclick="addTianyiSub()">添加订阅</button>
        </div>
        <div class="sub" style="margin-top:8px">添加后只收<b>之后新发布</b>的分享，不会把历史上千条全导进来；想补收老的，点下面「浏览分享」手动挑。天翼订阅号接口<b>不需要登录</b>，没扫过码也能收。</div>
      </div>
      <div class="card">
        <div class="chd"><b>已订阅</b><span class="csub">每 30 分钟自动检查一次新分享</span></div>
        <div class="row" style="margin-top:10px;align-items:center;gap:8px;flex-wrap:wrap">
          <button class="mini danger" id="tsBfStop" style="display:none" onclick="tsBfStop()">停止</button>
          <span id="tsBfMsg" class="sub"></span>
        </div>
        <table id="tsList" style="margin-top:10px"></table>
        <div id="tsShares" style="margin-top:16px"></div>
      </div>
    </div>
    <div id="pg-lib" style="display:none">
      <div class="pagehd">
        <h2>媒体库</h2>
        <span style="flex:1"></span>
        <div class="srch"><svg viewBox="0 0 24 24" fill="none" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="11" cy="11" r="7"/><path d="m21 21-4.3-4.3"/></svg><input type="text" id="libQ" placeholder="搜索全部文件" oninput="onLibQ()"></div>
        <button class="mini gray" id="refreshBtn" onclick="loadLib()">刷新</button>
        <button class="mini gray" id="viewBtn" onclick="toggleView()">网格视图</button>
      </div>
      <div class="card" style="padding:14px 6px 6px">
        <div class="crumb" id="crumb"></div>
        <div id="libBox"></div>
      </div>
      <div class="selbar" id="selbar" style="display:none">
        <b id="selInfo"></b><span class="sp"></span>
        <button class="mini danger" onclick="delSelected()">删除选中</button>
        <button class="mini gray" onclick="clearSel()">取消选择</button>
      </div>
    </div>
    <div id="pg-set" style="display:none">
      <div class="pagehd"><h2>设置</h2><span class="sub">网盘取流凭证与 STRM 导出</span></div>
      <div class="card"><b style="font-size:14px">网盘账号（取流凭证）<span class="cd">解析分享不需要账号；播放取流才需要，连一次就行</span></b>
        <div class="sub" style="margin:8px 0 0">下面列着所有能连的网盘，连没连都标着呢。要连哪个就点它后面的「<b>连接</b>」，照着弹窗里的步骤做，一分钟连一个。换号：点「退出/换号」再连新的——旧凭证自动作废，百度的旧临时文件后台清理，不用重启。</div>
        <table id="accts" style="margin-top:10px"></table>
      </div>
      <div class="card">
        <b style="font-size:14px">播放走法<span class="cd">数据从网盘到播放器怎么走——家用盒子按默认来就行，云服务器部署才需要动</span></b>
        <div class="swrow">
          <div class="swmain">
            <div class="swt">夸克 302 直连</div>
            <div class="swd">开启后播放器自己连夸克服务器拉数据，<b>不经过本机、不耗服务器流量</b>；关闭则数据走本机转发，省心。切换立即生效，已生成的 .strm 不用重建。</div>
            <details class="swmore"><summary>两种走法的区别 / 开启条件</summary>
              <div>
                <b>关闭</b>（默认）：数据经过本机转发。<b>家用盒子</b>用这个，省心，对账号名额也没要求。<br>
                <b>开启</b>：播放器直连夸克服务器拉数据，<b>不经过本机、不耗服务器流量</b>。<b>云服务器部署必须开</b>（否则流量全走服务器）。<br>
                开启的条件：上面「夸克网盘」<b>两种登录都连上</b>——Cookie 负责播放时把文件转存到临时目录，扫码负责取免认证直链，缺一个都放不了。<br>
                切换<b>立即生效</b>：已经生成的 .strm 一个都不用重建，两种走法互相兼容。
              </div>
            </details>
          </div>
          <label class="sw"><input type="checkbox" id="quarkTV302" onchange="saveQuarkTV302()"><i></i></label>
        </div>
        <div class="swrow">
          <div class="swmain">
            <div class="swt">百度浏览器代取</div>
            <div class="swd"><b>关闭</b>：保持 302，浏览器无法播放。<b>开启</b>：走本机代理，就可以在浏览器播放。</div>
          </div>
          <label class="sw"><input type="checkbox" id="baiduRelay"><i></i></label>
        </div>
        <div class="sub" style="margin:10px 0 0;font-size:13px">夸克开关<b>立即生效</b>；百度开关随下面「保存并重建」一起生效。</div>
      </div>
      <div class="card">
        <b style="font-size:14px">STRM 导出（媒体库用）<span class="cd">不用飞牛 / Emby 的话，这张卡可以完全不管</span></b>
        <table style="margin-top:8px">
          <tr><td style="width:130px;color:var(--mut);vertical-align:top">输出目录</td><td><input id="strmDir" placeholder="留空 = 启动参数 -strm" style="width:100%"><br><small style="color:var(--mut)">容器内路径；必须和 docker 挂载对应</small></td></tr>
          <tr><td style="color:var(--mut);vertical-align:top">对外地址</td><td><input id="strmBase" placeholder="留空 = 自动探测，如 http://192.168.1.10:8099" style="width:100%"><br><small style="color:var(--mut)">写进 .strm 的地址，客户端必须能访问到</small></td></tr>
          <tr><td style="color:var(--mut)">目录层级</td><td>
            <select id="strmLayout" style="padding:8px 10px;border:1px solid var(--line);border-radius:8px;background:#fbfcfe">
              <option value="full">网盘 / 分类 / 剧名</option>
              <option value="no_provider">分类 / 剧名</option>
              <option value="show_only">仅剧名</option>
            </select>
          </td></tr>
          <tr><td style="color:var(--mut)">视频后缀</td><td><input id="strmExts" placeholder=".mp4,.mkv（留空 = 全部常见格式）" style="width:100%"></td></tr>
        </table>
        <div id="strmStale" class="banner" style="display:none">
          <b>⚠️ 对外地址变了，存量 .strm 已失效</b><br>
          <span id="strmStaleMsg"></span>
        </div>
        <div class="row" style="margin-top:10px;align-items:center">
          <button onclick="saveStrm()">保存并重建</button>
          <button class="gray" onclick="rebuildSTRM()">全量重建 STRM</button>
          <span class="sub" id="strmHint" style="margin-left:8px"></span>
        </div>
        <div class="sub" style="margin:8px 0 0;font-size:13px">.strm 是给飞牛 / Emby / Jellyfin 扫描的索引文件，指向「对外地址 + /direct」；和 WebDAV 挂载各管各的，改这里不影响它。<br>
        「保存并重建」只补有变化的部分，地址没变时媒体库基本无感；「全量重建」<b>平时别点</b>——只有对外地址变了（上面弹红色横幅）才需要：它会重写所有 .strm，媒体库跟着重新刮削全库，片子多时要等一阵，属正常现象。</div>
      </div>
    </div>
  </div>
</div>
<div class="toast" id="toast"></div>
<div class="modal" id="modal" onclick="if(event.target===this)closeModal()">
  <div class="mbox">
    <div class="mtitle" id="mTitle"></div>
    <div class="mbody" id="mBody"></div>
    <div class="mfoot">
      <button class="gray" id="mNo" onclick="closeModal()">取消</button>
      <button class="danger" id="mYes">确定</button>
    </div>
  </div>
</div>
<div class="modal" id="catModal" onclick="if(event.target===this)closeCatModal()">
  <div class="mbox">
    <div class="mtitle" id="cmTitle"></div>
    <div class="mbody" style="white-space:normal">
      <div style="margin-bottom:10px;color:var(--mut);font-size:12.5px">导入时分类选错了？在这里事后补救：选个新分类（或新建一级/二级），这部剧就整体归过去。</div>
      <div style="margin-bottom:10px">当前分类：<b id="cmCur" style="color:var(--txt)"></b></div>
      <div class="row" style="margin-bottom:12px">
        <select id="cmSel1" style="flex:1;min-width:150px;padding:9px 12px;border:1px solid var(--line);border-radius:9px;background:#fbfcfe" onchange="onCat1ChangeModal()"></select>
        <select id="cmSel2" style="display:none;flex:1;min-width:150px;padding:9px 12px;border:1px solid var(--line);border-radius:9px;background:#fbfcfe" onchange="onCat2ChangeModal()"></select>
        <input type="text" id="cmNew1" placeholder="一级分类名，如：电视剧" style="display:none;flex:1;min-width:130px" oninput="onCatNew1Input('cmSel2','cmNew1')">
        <input type="text" id="cmNew2" placeholder="二级分类名，如：国产剧" style="display:none;flex:1;min-width:130px">
      </div>
      <div style="color:var(--mut);font-size:12.5px;line-height:1.7">改分类会把这个剧在媒体库里的位置搬过去（.strm 文件跟着搬家），已入库的剧集不受影响；以后追更出来的新剧集也归到新分类。片子多时媒体库会重新刮削这一部，属正常现象。</div>
    </div>
    <div class="mfoot">
      <button class="gray" onclick="closeCatModal()">取消</button>
      <button id="cmYes" onclick="doChangeCat()">确定</button>
    </div>
  </div>
</div>
<div class="modal" id="connModal" onclick="if(event.target===this)closeConn()">
  <div class="mbox" style="max-width:540px">
    <div class="mtitle" id="connTitle"></div>
    <div class="mbody" id="connBody" style="white-space:normal;text-align:left"></div>
  </div>
</div>
<div class="modal" id="askModal" onclick="if(event.target===this)closeAsk(false)">
  <div class="mbox" style="max-width:400px">
    <div class="mtitle" id="askTitle"></div>
    <div class="mbody" style="margin-bottom:14px">
      <input type="text" id="askInput" style="width:100%">
    </div>
    <div class="mfoot">
      <button class="gray" id="askNo" onclick="closeAsk(false)">取消</button>
      <button id="askYes" onclick="closeAsk(true)">确定</button>
    </div>
  </div>
</div>
<div class="player" id="player" onclick="if(event.target===this)closePlayer()">
  <div class="box">
    <div class="vwrap"><video id="pv" controls autoplay playsinline></video><div class="pvmsg" id="pvmsg"></div></div>
    <div class="pbar"><span id="pvName" style="flex:1"></span><button class="mini gray" onclick="copyLink()">复制直链</button><button class="mini gray" onclick="closePlayer()">关闭</button></div>
  </div>
</div>
<script>
var ITEMS=[], SELF='', CURID='', TREE={}, MOK=null;
var PVCN={quark:'夸克网盘','115':'115网盘',mobile139:'移动云盘',tianyi:'天翼云盘',baidu:'百度网盘'};
// 列表/概览里的短名：「夸克/电视剧/国产剧」比「夸克网盘/…」清爽
var PVSHORT={quark:'夸克','115':'115',mobile139:'移动',tianyi:'天翼',baidu:'百度'};
// 网盘名统一成短名：目录名可能是「夸克」「115网盘」「mobile139」等各种写法，
// 显示层一律收敛成 夸克/115/移动/天翼/百度（不动已有目录名，避免目录裂开）
function pvLabel(name){
  name=String(name||'');
  if(name in PVSHORT)return PVSHORT[name];
  if(name.indexOf('夸克')>=0)return '夸克';
  if(name.indexOf('115')>=0)return '115';
  if(name.indexOf('移动')>=0)return '移动';
  if(name.indexOf('天翼')>=0)return '天翼';
  if(name.indexOf('百度')>=0)return '百度';
  return name;
}
var PVCLS={quark:'q','115':'n',mobile139:'m',tianyi:'t',baidu:'b'};
var VIDEO_EXTS=/\.(mkv|mp4|ts|avi|mov|wmv|flv|webm|rmvb|iso|m2ts)$/i;
function $(x){return document.getElementById(x)}
function esc(s){var d=document.createElement('div');d.textContent=(s==null?'':String(s));return d.innerHTML}
function escA(s){return esc(s).replace(/"/g,'&quot;').replace(/'/g,'&#39;')}
function fmtTime(s){s=(s||'').replace('T',' ');return s.length>=16?s.slice(5,16):(s||'—')}
// 分享资源名常带 [tmdbid=xxx] / [tmdb-xxx] 之类的刮削标记，展示时剥掉尾部括号
function cleanShow(s){
  var o=String(s==null?'':s);
  var c=o.replace(/\s*\[[^\[\]]{0,48}\]\s*$/,'').replace(/\s*【[^【】]{0,48}】\s*$/,'').trim();
  return c||o;
}
function toast(m){var t=$('toast');t.textContent=m;t.classList.add('show');clearTimeout(t._h);t._h=setTimeout(function(){t.classList.remove('show')},2200)}
function humanSize(n){if(!n||n<=0)return '0 B';var u=['B','KB','MB','GB','TB'],i=0;while(n>=1024&&i<u.length-1){n/=1024;i++}return n.toFixed(i?2:0)+' '+u[i]}
var PAGES=['home','import','tianyi-sub','lib','set'];
function nav(p){
  PAGES.forEach(function(x){
    var el=$('pg-'+x);
    if(x===p){el.style.display='';el.classList.remove('pg');void el.offsetWidth;el.classList.add('pg')}
    else el.style.display='none';
  });
  var as=document.querySelectorAll('.nav a');
  for(var i=0;i<as.length;i++){as[i].className=(as[i].getAttribute('data-p')===p?'on':'')}
  if(p==='home')loadHome(); if(p==='lib')loadLib(); if(p==='set')loadSet(); if(p==='import')loadShares();
  if(p==='tianyi-sub'){loadTianyiSubs();tsBfPoll(false);}
}
(function(){var as=document.querySelectorAll('.nav a');for(var i=0;i<as.length;i++){as[i].onclick=function(){nav(this.getAttribute('data-p'))}}})();
(function(){var el=$('sharePwd');if(el)el.addEventListener('keydown',function(e){if(e.key==='Enter')batchParse()})})();

// ---- 自定义确认弹窗（替代原生 confirm） ----
function openModal(title,body,okText,onOk){
  $('mTitle').innerHTML='<span class="mi"><svg viewBox="0 0 24 24" fill="none" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 9v4m0 4h.01"/><path d="M10.3 3.9 1.8 18a2 2 0 0 0 1.7 3h17a2 2 0 0 0 1.7-3L13.7 3.9a2 2 0 0 0-3.4 0Z"/></svg></span>'+esc(title);
  $('mBody').textContent=body;
  $('mYes').textContent=okText||'确定';
  MOK=onOk;
  var m=$('modal');m.style.display='flex';requestAnimationFrame(function(){m.classList.add('show')});
}
function closeModal(){var m=$('modal');m.classList.remove('show');setTimeout(function(){m.style.display='none'},200);MOK=null}
$('mYes').onclick=function(){var f=MOK;closeModal();if(f)f()};
document.addEventListener('keydown',function(e){
  if(e.key!=='Escape')return;
  if($('modal').style.display==='flex')closeModal();
  else if($('askModal').style.display==='flex')closeAsk(false);
  else if($('player').style.display==='flex')closePlayer();
});
// 输入型模态框：替代原生 prompt，和整体设计语言一致（Enter 确认 / Esc 取消 / 空值禁确认）
var ASKOK=null;
function askText(title,placeholder,cb){
  $('askTitle').innerHTML='<span class="mi ok"><svg viewBox="0 0 24 24" fill="none" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M3 8a2 2 0 0 1 2-2h4.2a2 2 0 0 1 1.5.7l1.3 1.5H19a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2Z"/><path d="M12 11v6m-3-3h6"/></svg></span>'+esc(title);
  var inp=$('askInput');
  inp.value='';inp.placeholder=placeholder||'';
  $('askYes').disabled=true;
  ASKOK=cb;
  var m=$('askModal');m.style.display='flex';requestAnimationFrame(function(){m.classList.add('show')});
  setTimeout(function(){inp.focus()},90);
}
function closeAsk(ok){
  var f=ASKOK;ASKOK=null;
  var m=$('askModal');
  m.classList.remove('show');
  setTimeout(function(){m.style.display='none'},200);
  if(f)f(ok?$('askInput').value.trim():null);
}
$('askInput').oninput=function(){$('askYes').disabled=!this.value.trim()};
$('askInput').onkeydown=function(e){
  if(e.key==='Enter'){e.preventDefault();if(this.value.trim())closeAsk(true)}
};

// ---- 数据 ----
function loadItems(cb){
  fetch('/api/items').then(function(r){
    if(r.status===401){location.href='/login';return null}
    return r.json()
  }).then(function(d){
    if(!d)return;
    ITEMS=d.items||[]; SELF=d.self||location.origin; cb();
  }).catch(function(){toast('读取列表失败，请稍后重试')});
}

// ---- 概览 ----
var ICO={
  film:'<svg viewBox="0 0 24 24" fill="none" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="4" width="18" height="16" rx="2"/><path d="M7.5 4v16M16.5 4v16M3 9.5h4.5M3 14.5h4.5M16.5 9.5H21M16.5 14.5H21"/></svg>',
  db:'<svg viewBox="0 0 24 24" fill="none" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><ellipse cx="12" cy="6" rx="8" ry="3"/><path d="M4 6v6c0 1.7 3.6 3 8 3s8-1.3 8-3V6M4 12v6c0 1.7 3.6 3 8 3s8-1.3 8-3v-6"/></svg>',
  layers:'<svg viewBox="0 0 24 24" fill="none" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="m12 3 9 5-9 5-9-5 9-5Z"/><path d="m3 13 9 5 9-5"/></svg>',
  refresh:'<svg viewBox="0 0 24 24" fill="none" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 12a9 9 0 1 1-2.6-6.4"/><path d="M21 3v6h-6"/></svg>',
  folder:'<svg viewBox="0 0 24 24"><path fill="#4c8bf5" d="M3 6.6A2.6 2.6 0 0 1 5.6 4h3.1a1 1 0 0 1 .74.33L11 6h7.4A2.6 2.6 0 0 1 21 8.6v8.8a2.6 2.6 0 0 1-2.6 2.6H5.6A2.6 2.6 0 0 1 3 17.4Z"/></svg>',
  home:'<svg viewBox="0 0 24 24" fill="none" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M4 10.5 12 4l8 6.5V19a1.5 1.5 0 0 1-1.5 1.5h-13A1.5 1.5 0 0 1 4 19Z"/><path d="M9.5 20.5v-6h5v6"/></svg>',
  up:'<svg viewBox="0 0 24 24" fill="none" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 19V6m0 0-6 6m6-6 6 6"/></svg>',
  play:'<svg viewBox="0 0 24 24" fill="none" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="9"/><path d="m10 8.6 5.6 3.4-5.6 3.4Z"/></svg>',
  doc:'<svg viewBox="0 0 24 24" fill="none" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M14 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8Z"/><path d="M14 3v5h5"/></svg>',
  grid:'<svg viewBox="0 0 24 24" fill="none" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="3.5" y="3.5" width="7" height="7" rx="1.5"/><rect x="13.5" y="3.5" width="7" height="7" rx="1.5"/><rect x="3.5" y="13.5" width="7" height="7" rx="1.5"/><rect x="13.5" y="13.5" width="7" height="7" rx="1.5"/></svg>'
};
function loadHome(){
  $('stats').innerHTML='<div class="skel"></div><div class="skel"></div><div class="skel"></div><div class="skel"></div>';
  $('recent').innerHTML='<tr><td class="loading" colspan="3"><span class="spin"></span>加载中…</td></tr>';
  loadItems(function(){
    $('davUrl').textContent=SELF+'/dav'; // 必须在 loadItems 回调里：SELF 此时才有值
    // 空库时显示三步引导；有任何内容就收起，概览回归数据本身
    $('guide').style.display=ITEMS.length?'none':'';
    var total=0, byPv={}, shows={};
    ITEMS.forEach(function(it){
      total+=(it.size||0);
      // 统计「部」数：优先用入库时记录的 show 字段（两级分类下目录层数会变，
      // 按 parts[1] 猜会把一级分类当剧名，少算）；老数据没有字段才按目录猜
      var parts=(it.dir||'未分类').split('/');
      var pv=parts[0]||'未分类', show=it.show||parts[1]||'(根)';
      byPv[pv]=(byPv[pv]||0)+1; shows[pv+'/'+show]=1;
    });
    var n=0;for(var s in shows){n++}
    var h='<div class="card"><div class="si b">'+ICO.film+'</div><b>'+ITEMS.length+'</b><span>个文件</span></div>';
    h+='<div class="card"><div class="si o">'+ICO.layers+'</div><b>'+n+'</b><span>部影片/剧集</span></div>';
    h+='<div class="card"><div class="si p">'+ICO.db+'</div><b>'+humanSize(total)+'</b><span>总容量</span></div>';
    for(var p in byPv){h+='<div class="card"><div class="si g">'+ICO.db+'</div><b>'+byPv[p]+'</b><span>'+esc(pvLabel(p))+'</span></div>'}
    $('stats').innerHTML=h;
    fetch('/api/shares').then(function(r){return r.json()}).then(function(d){
      var arr=(d.shares||[]).filter(function(s){return s.auto_refresh});
      if(!arr.length)return;
      var el=document.createElement('div');el.className='card';
      el.innerHTML='<div class="si g">'+ICO.refresh+'</div><b>'+arr.length+'</b><span>分享追更中</span>';
      var box=$('stats');box.insertBefore(el,box.children[3]||null);
    }).catch(function(){});
    // 对外地址变过：媒体库里老片子实际已播不了，概览页就给个显眼提醒
    fetch('/api/strm/config').then(function(r){return r.json()}).then(function(d){
      if(!d.ok||!d.stale)return;
      var el=document.createElement('div');el.className='card';
      el.style.borderColor='#f0c4cd';el.style.background='var(--danger2)';
      el.innerHTML='<div class="si" style="background:#fff;color:var(--danger)">'+ICO.refresh+'</div>'+
        '<b style="color:var(--danger);font-size:16px">STRM 待重建</b>'+
        '<span class="plain" style="color:#a12842;font-weight:400">对外地址变了，老片子的 .strm 还指着旧地址、已播不了：去「设置」→ STRM 导出，点「全量重建 STRM」一次性修好</span>';
      var box=$('stats');if(box)box.appendChild(el);
    }).catch(function(){});
    // 按「分享」聚合：一个分享链接 = 一部剧 / 一部电影，绝不把每一集单独铺一行
    var agg={},order=[];
    ITEMS.forEach(function(it){
      var k=(it.provider||'手动')+'|'+(it.share_id||('item:'+it.id));
      var a=agg[k];
      if(!a){a={pv:it.provider||'',show:'',cat:'',dir:it.dir||'',n:0,size:0,last:''};agg[k]=a;order.push(k)}
      a.n++;a.size+=(it.size||0);
      var ts=it.added_at||'';if(ts>a.last)a.last=ts;
      if(it.show)a.show=it.show;
      if(it.category)a.cat=it.category;
    });
    order.sort(function(x,y){return (agg[y].last||'').localeCompare(agg[x].last||'')});
    var t='<tr><th>影片 / 剧集</th><th style="width:96px;text-align:right">总大小</th><th style="width:118px;text-align:right">最近添加</th></tr>';
    order.slice(0,8).forEach(function(k){
      var a=agg[k],p=(a.dir||'').split('/');
      var show=a.show||(p.length>=3?p[2]:(p[1]||'(未命名)'));
      var cat=a.cat||(p.length>=3?p[1]:'');
      var tag=PVCN[a.pv]?'<span class="tag '+(PVCLS[a.pv]||'')+'">'+esc(PVCN[a.pv])+'</span> ':'';
      var meta=(cat?esc(cat)+' · ':'')+a.n+(a.n>1?' 集':' 个文件');
      var disp=cleanShow(show);
      t+='<tr><td>'+tag+'<a href="#" class="rowlk" onclick="gotShow(this);return false" data-show="'+escA(disp)+'" title="在媒体库中筛选：'+escA(disp)+'">'+esc(disp)+'</a><div class="tmeta">'+meta+'</div></td>'+
         '<td class="num">'+humanSize(a.size)+'</td><td class="tm">'+fmtTime(a.last)+'</td></tr>';
    });
    if(!ITEMS.length)t+='<tr><td colspan="3" style="color:var(--mut);padding:16px 12px">还没有内容，先去「分享导入」粘贴一个链接。</td></tr>';
    $('recent').innerHTML=t;
  });
}
function gotoLib(){nav('lib')}
function gotShow(el){
  var name=el.getAttribute('data-show')||'';
  var q=$('libQ'); if(q)q.value=name;
  nav('lib');
  toast('已筛选：'+name);
}

// ---- 分享导入（单条 / 批量同一套流程） ----
// 一个粘贴框 → 解析 → 统一预览表：每行可改剧名、选两级分类（可选已有或新建）、
// 展开勾选要收的文件 → 一次导入。单条就是「只有一行」，不再有第二套界面。
var BATCH_ROWS=[];

function batchSplit(text){
  // 换行 / 分号 拆行；行首编号去掉；从 http 开始截取（丢掉前面的片名等中文）；
  // 一行里粘了多条链接也能拆开。这样「《成龙》70部…  https://…（访问码：xx11）」
  // 只会把 URL 部分传给后端，片名不会干扰提取码解析。
  var out=[];
  String(text||'').split(/[\n\r;；]+/).forEach(function(line){
    line=line.replace(/^\s*\d+[\.、．)）]\s*/,'');
    var parts=line.split(/(?=https?:\/\/)/);
    for(var i=0;i<parts.length;i++){
      var k=parts[i].indexOf('http');
      if(k<0)continue;
      var u=parts[i].slice(k).trim();
      if(u)out.push(u);
    }
  });
  return out;
}

function batchParse(){
  var links=batchSplit($('batchText').value);
  if(!links.length){toast('没识别到链接，一行一条，以 http 开头');return}
  var pwd=$('sharePwd')?$('sharePwd').value.trim():'';
  var box=$('batchOut');
  box.innerHTML='<div class="loading"><span class="spin"></span>解析中：共 '+links.length+' 条，大合集要十几秒…</div>';
  fetch('/api/share/batch/parse',{method:'POST',headers:{'Content-Type':'application/json'},
    body:JSON.stringify({links:links,pwd:pwd})})
  .then(function(r){return r.json()}).then(function(d){
    if(!d.ok){box.innerHTML='<div class="empty"><b>解析失败</b>'+esc(d.error||'')+'</div>';return}
    BATCH_ROWS=(d.rows||[]).map(function(r){
      r.orig_title=r.title||'';
      r.cat1='';r.cat2='';r.open=false;
      r.sel={};(r.files||[]).forEach(function(f){r.sel[f.id]=true});
      return r;
    });
    // 分类下拉要用「已导入的条目 + 分享列表」拼出来的分类树
    loadItems(function(){renderBatchRows()});
  }).catch(function(e){
    box.innerHTML='<div class="empty"><b>解析失败</b>网络错误：'+esc(String(e))+'</div>';
  });
}

// 一级下拉：不分类 / 已有顶级分类 / 新建
function bCat1Opts(cur){
  var h='<option value="">不分类</option>';
  var t=catTree();
  Object.keys(t).sort().forEach(function(p){
    h+='<option value="'+escA(p)+'"'+(p===cur?' selected':'')+'>'+esc(p)+'</option>';
  });
  h+='<option value="__new1__">➕ 新建一级分类…</option>';
  return h;
}
// 二级下拉：不分二级 / 该一级下已有的二级 / 新建
function bCat2Opts(parent,cur){
  var h='<option value="">不分二级</option>';
  var t=(catTree()[parent]||{});
  Object.keys(t).sort().forEach(function(c){
    h+='<option value="'+escA(c)+'"'+(c===cur?' selected':'')+'>'+esc(c)+'</option>';
  });
  h+='<option value="__new2__">➕ 新建二级分类…</option>';
  return h;
}
// 分类拼成入库用的 "一级/二级"
function bCatOf(r){
  if(!r.cat1)return '';
  return r.cat2?(r.cat1+'/'+r.cat2):r.cat1;
}
function bTitle(i,el){BATCH_ROWS[i].title=el.value}
function bCat1(i,el){
  var r=BATCH_ROWS[i],v=el.value;
  if(v==='__new1__'){
    askText('新建一级分类','如：电视剧 / 电影 / 动漫',function(n){
      if(!n){el.value=r.cat1||'';return} // 取消：下拉回原值
      ensureCatOpt(el,n);el.value=n;r.cat1=n;r.cat2='';
      bCat1Tail(i,r);
    });
    return;
  }
  var oldv=r.cat1;
  r.cat1=v;
  if(v!==oldv)r.cat2=''; // 换了一级，原来的二级不再适用
  bCat1Tail(i,r);
}
function bCat1Tail(i,r){
  if(!r.cat1)r.cat2='';
  var s2=document.getElementById('bc2_'+i);
  if(s2){s2.innerHTML=bCat2Opts(r.cat1,'')}
}
function bCat2(i,el){
  var r=BATCH_ROWS[i],v=el.value;
  if(v==='__new2__'){
    askText('新建二级分类','如：国产剧 / 欧美剧',function(n){
      if(!n){el.value=r.cat2||'';return}
      ensureCatOpt(el,n);el.value=n;r.cat2=n;
    });
    return;
  }
  r.cat2=v;
}
function bOpen(i){
  var r=BATCH_ROWS[i];if(!r)return;
  r.open=!r.open;
  var tr=document.getElementById('bf_'+i);
  if(tr)tr.style.display=r.open?'':'none';
}
function bCkFile(i,fid,el){
  var r=BATCH_ROWS[i];if(!r)return;
  r.sel[fid]=el.checked;
  var n=0;(r.files||[]).forEach(function(f){if(r.sel[f.id])n++});
  var el2=document.getElementById('bn_'+i);
  if(el2)el2.textContent=n+'/'+(r.files||[]).length;
}
function bCkAll(i,v){
  var r=BATCH_ROWS[i];if(!r)return;
  (r.files||[]).forEach(function(f){r.sel[f.id]=v});
  var box=document.getElementById('bf_'+i);
  if(box){
    var cks=box.querySelectorAll('input[type=checkbox]');
    for(var k=0;k<cks.length;k++)cks[k].checked=v;
  }
  var el2=document.getElementById('bn_'+i);
  if(el2)el2.textContent=(v?(r.files||[]).length:0)+'/'+(r.files||[]).length;
}
// 统一分类：给所有「待导入」行刷同一个分类
function bApplyAll(){
  var v1=$('ball1'),v2=$('ball2');
  var c1=v1?v1.value:'',c2=v2?v2.value:'';
  if(c1==='__new1__'){
    askText('新建一级分类','如：电视剧 / 电影 / 动漫',function(n){
      if(!n){toast('已取消');return}
      ensureCatOpt(v1,n);v1.value=n;
      bApplyAllGo(v2,n,c2);
    });
    return;
  }
  bApplyAllGo(v2,c1,c2);
}
function bApplyAllGo(v2,c1,c2){
  if(c2==='__new2__'){
    askText('新建二级分类','如：国产剧 / 欧美剧',function(n){
      if(!n){toast('已取消');return}
      ensureCatOpt(v2,n);v2.value=n;
      bApplyAllDo(c1,n);
    });
    return;
  }
  bApplyAllDo(c1,c2);
}
function bApplyAllDo(c1,c2){
  if(!c1)c2='';
  var n=0;
  BATCH_ROWS.forEach(function(r){
    if(!r.ok||r.exists)return;
    r.cat1=c1;r.cat2=c2;n++;
  });
  renderBatchRows();
  toast(n?('已把「'+(bCatOf({cat1:c1,cat2:c2})||'不分类')+'」应用到 '+n+' 条'):'没有可设置的条目');
}

function renderBatchRows(){
  var box=$('batchOut');
  var ok=0,dup=0,bad=0;
  BATCH_ROWS.forEach(function(r){
    if(!r.ok)bad++;else if(r.exists)dup++;else ok++;
  });
  var h='<table style="width:100%;border-collapse:collapse;font-size:13px">'
   +'<thead><tr style="text-align:left;color:var(--dim);font-size:12px">'
   +'<th style="padding:6px 6px 6px 0">剧名（可改）</th>'
   +'<th style="padding:6px 4px;width:86px">网盘</th>'
   +'<th style="padding:6px 4px;width:74px">文件</th>'
   +'<th style="padding:6px 4px;width:150px">一级分类</th>'
   +'<th style="padding:6px 4px;width:150px">二级分类</th>'
   +'<th style="padding:6px 4px;width:96px">状态</th></tr></thead><tbody>';

  BATCH_ROWS.forEach(function(r,i){
    var editable=(r.ok&&!r.exists);
    h+='<tr style="border-top:1px solid var(--line);vertical-align:top">';
    // 剧名
    h+='<td style="padding:7px 6px 7px 0">';
    if(editable){
      h+='<input type="text" value="'+escA(r.title||'')+'" oninput="bTitle('+i+',this)" style="width:100%;min-width:180px">';
      if(r.orig_title&&r.orig_title!==r.title){
        h+='<div class="cd" style="font-size:11px;margin-top:2px">原：'+esc(r.orig_title)+'</div>';
      }
    }else{
      h+='<div style="padding:5px 0">'+esc(r.title||String(r.link||'').slice(0,48))+'</div>';
    }
    h+='</td>';
    // 网盘
    h+='<td style="padding:7px 4px">'+(PVCN[r.provider]||r.provider||'-')+'</td>';
    // 文件数（可展开）
    h+='<td style="padding:7px 4px">';
    if(r.ok&&r.files&&r.files.length){
      h+='<a href="javascript:;" onclick="bOpen('+i+')" style="text-decoration:none"><span id="bn_'+i+'">'+
         (r.files.filter(function(f){return r.sel[f.id]}).length)+'/'+r.files.length+'</span> '+(r.open?'▴':'▾')+'</a>';
    }else{
      h+='<span class="cd">'+(r.count||0)+'</span>';
    }
    h+='</td>';
    // 一级 / 二级
    if(editable){
      h+='<td style="padding:7px 4px"><select id="bc1_'+i+'" onchange="bCat1('+i+',this)" style="width:100%">'+bCat1Opts(r.cat1)+'</select></td>';
      h+='<td style="padding:7px 4px"><select id="bc2_'+i+'" onchange="bCat2('+i+',this)" style="width:100%">'+bCat2Opts(r.cat1,r.cat2)+'</select></td>';
    }else{
      h+='<td style="padding:7px 4px"><span class="cd">-</span></td><td style="padding:7px 4px"><span class="cd">-</span></td>';
    }
    // 状态
    h+='<td style="padding:7px 4px">';
    if(!r.ok){h+='<span style="color:var(--danger)">'+esc(r.error||'失败')+'</span>';}
    else if(r.exists){h+='<span class="cd">已导入过</span>';}
    else {h+='<span style="color:var(--ok)">待导入</span>';}
    h+='</td></tr>';
    // 展开的文件勾选行
    if(r.ok&&r.files&&r.files.length){
      h+='<tr id="bf_'+i+'" style="display:'+(r.open?'':'none')+'"><td colspan="6" style="padding:0 0 10px 0;background:var(--bg2)">'
       +'<div style="padding:8px 10px">'
       +'<div style="margin-bottom:6px"><label style="font-size:12px;display:flex;align-items:center;gap:6px"><input type="checkbox" checked onchange="bCkAll('+i+',this.checked)">全选（取消勾选的文件不会收录）</label></div>'
       +'<table style="width:100%;font-size:12px;border-collapse:collapse">';
      r.files.forEach(function(f){
        if(!isVideoName(f.name||f.path||''))return;
        h+='<tr><td style="width:26px;padding:2px 0"><input type="checkbox" '+(r.sel[f.id]?'checked':'')+
           ' onchange="bCkFile('+i+',\''+escA(f.id)+'\',this)"></td>'
         +'<td class="brk" style="padding:2px 6px 2px 0">'+esc(f.path||f.name)+'</td>'
         +'<td class="num" style="width:90px;padding:2px 0">'+humanSize(f.size)+'</td></tr>';
      });
      h+='</table></div></td></tr>';
    }
  });
  h+='</tbody></table>';

  h+='<div class="row" style="margin-top:12px;gap:8px;align-items:center;flex-wrap:wrap">'
   +'<span class="sub" style="margin:0">统一分类</span>'
   +'<select id="ball1" style="width:140px" onchange="(function(v){var s=$(\'ball2\');if(s)s.innerHTML=bCat2Opts(v===\'__new1__\'?\'\':v,\'\')})(this.value)">'+bCat1Opts('')+'</select>'
   +'<select id="ball2" style="width:140px">'+bCat2Opts('','')+'</select>'
   +'<button class="mini gray" onclick="bApplyAll()">应用到全部</button>'
   +'<label style="display:flex;align-items:center;gap:6px;font-size:13px;margin-left:8px"><input type="checkbox" id="ckAuto" checked>导入后开启追更</label>'
   +'<button onclick="batchAdd()" style="margin-left:auto">开始导入</button>'
   +'</div>'
   +'<div class="row" style="margin-top:8px"><span id="batchMsg" class="sub" style="margin:0"></span></div>'
   +'<div class="sub" style="margin-top:6px">共 '+BATCH_ROWS.length+' 条：待导入 <b>'+ok+'</b> · 已导入过 <b>'+dup+'</b> · 失败 <b>'+bad+'</b>（失败的自动跳过，不影响其他）</div>'
   +'<div class="tips" style="margin-top:8px">「剧名」会显示在媒体库和飞牛影视里，可逐条改；「分类」支持两级，能选已有的，也能点「➕ 新建」现场加；点文件数可以展开、取消不想收录的集。选错了不用重来——导入后到下方分享列表点这行的「分类」随时改。</div>';

  box.innerHTML=h;
}

function batchAdd(){
  var items=[];
  BATCH_ROWS.forEach(function(r){
    if(!r.ok||r.exists)return;
    var ids=[];
    (r.files||[]).forEach(function(f){if(r.sel[f.id])ids.push(f.id)});
    if(!ids.length)return;
    items.push({provider:r.provider,share_id:r.share_id,category:bCatOf(r),
      title:(r.title||'').trim(),files:ids});
  });
  if(!items.length){toast('没有可导入的分享（检查是不是都已在库里，或文件全被取消勾选）');return}
  var msg=$('batchMsg');
  if(msg)msg.innerHTML='<span class="spin"></span>导入中：共 '+items.length+' 个分享…';
  fetch('/api/share/batch/add',{method:'POST',headers:{'Content-Type':'application/json'},
    body:JSON.stringify({items:items,auto_refresh:($('ckAuto')?$('ckAuto').checked:true)})})
  .then(function(r){return r.json()}).then(function(d){
    if(!d.ok){if(msg)msg.innerHTML='<span style="color:var(--danger)">'+esc(d.error||'导入失败')+'</span>';return}
    var rows=d.rows||[];
    rows.forEach(function(x){
      BATCH_ROWS.forEach(function(r){
        if(r.provider===x.provider&&r.share_id===x.share_id){
          if(x.ok){r.exists=true;r.ok=true;}
          else{r.ok=false;r.error=x.error||'失败';}
        }
      });
    });
    renderBatchRows();
    var m=$('batchMsg');
    if(m)m.innerHTML='完成：导入 <b>'+d.imported+'</b> 个分享，共 '+d.added+' 条';
    toast('导入完成，共 '+d.added+' 条');
    $('batchText').value='';
    SHAREPAGE=1;loadShares(1);
  }).catch(function(e){
    if(msg)msg.innerHTML='<span style="color:var(--danger)">导入失败：'+esc(String(e))+'</span>';
  });
}
// 判断文件是不是视频（和 Go 端 isVideo 对齐的简易版，仅用于预览过滤）
function isVideoName(name){
  var m=/\.([a-z0-9]+)$/i.exec(String(name||''));
  if(!m)return false;
  return ['mp4','mkv','avi','mov','wmv','flv','ts','m2ts','rmvb','rm','webm','mpg','mpeg','m4v','iso','3gp'].indexOf(m[1].toLowerCase())>=0;
}

// 分类两级树 {一级:{二级:1}}：来源 = 已导入条目 + 分享列表
function catTree(){
  var tree={};
  function add(c){
    c=(c||'').replace(/／/g,'/').trim();
    if(!c)return;
    var p,ch,i=c.indexOf('/');
    if(i<0){p=c;ch='';}else{p=c.slice(0,i).trim();ch=c.slice(i+1).trim();}
    if(!p)return;
    if(!tree[p])tree[p]={};
    if(ch)tree[p][ch]=1;
  }
  ITEMS.forEach(function(it){add(it.category)});
  (CATLIST||[]).forEach(add);
  return tree;
}
function ensureCatOpt(sel,val){
  for(var i=0;i<sel.options.length;i++)if(sel.options[i].value===val)return;
  var o=document.createElement('option');o.value=val;o.textContent=val;sel.appendChild(o);
}
// 填二级下拉：该一级下的现存二级 + 「新建二级」项
function fillCat2(sel2Id,parent,curC){
  var s=$(sel2Id);if(!s)return;
  var subs=(catTree()[parent]||{});
  var h='<option value="">（不分二级）</option>';
  Object.keys(subs).sort().forEach(function(c){h+='<option value="'+escA(c)+'">'+esc(c)+'</option>'});
  h+='<option value="__new2__">➕ 新建二级分类…</option>';
  s.innerHTML=h;
  if(curC)ensureCatOpt(s,curC);
  s.value=curC||'';
}
// 填两级下拉并回显当前分类（cur 可能是 "一级" 或 "一级/二级"，允许是刚手输还没保存的新值）
function fillCatSelects(i1,i2,n1,n2,cur){
  var s1=$(i1);if(!s1)return;
  var cp='',cc='';
  cur=(cur||'').replace(/／/g,'/').trim();
  if(cur){var i=cur.indexOf('/');if(i<0)cp=cur;else{cp=cur.slice(0,i).trim();cc=cur.slice(i+1).trim();}}
  var h='<option value="">不分类（直接放剧名）</option>';
  Object.keys(catTree()).sort().forEach(function(p){h+='<option value="'+escA(p)+'">'+esc(p)+'</option>'});
  h+='<option value="__new1__">➕ 新建一级分类…</option>';
  s1.innerHTML=h;
  if(cp)ensureCatOpt(s1,cp);else s1.value='';
  if(cp){ // 回显两级（新建中的一级没有二级可填，隐藏二级框）
    $(i2).style.display=(cp==='__new1__'?'none':'');
    if(cp!=='__new1__')fillCat2(i2,cp,cc);
  }else{
    $(i2).style.display='none';
  }
  $(n1).style.display=(s1.value==='__new1__'?'':'none');
  if(n2)$(n2).style.display='none';
}
// 取当前选中的分类：两级下拉 + 新建输入框 → "一级/二级" 或 "一级" 或 ""
function curCat(i1,i2,n1,n2){
  var v1=$(i1).value;
  if(v1==='__new1__')v1=($(n1).value||'').trim();
  if(!v1)return '';
  var v2=$(i2).value;
  if(v2==='__new2__')v2=($(n2).value||'').trim();
  if(!v2)return v1;
  return v1+'/'+v2;
}
// ---- 追更刷新 ----
var SHAREPAGE=1, SHARESIZE=50, SHARETOTAL=0;
function loadShares(page){
  if(page)SHAREPAGE=page;
  $('shareList').innerHTML='<tr><td colspan="5" class="loading"><span class="spin"></span>加载中…</td></tr>';
  fetch('/api/shares?page='+SHAREPAGE+'&size='+SHARESIZE).then(function(r){return r.json()}).then(function(d){
    var arr=d.shares||[];
    SHARETOTAL=d.total||arr.length;
    CATLIST=d.categories||[];
    // 删除/清理后当前页可能空了，自动回退一页
    if(!arr.length && SHAREPAGE>1){ loadShares(SHAREPAGE-1); return; }
    var t='<tr><th>剧名</th><th style="width:70px;text-align:right">文件数</th><th style="width:118px;text-align:right">最近入库</th><th style="width:90px">追更</th><th style="width:284px;text-align:right">操作</th></tr>';
    if(!arr.length)t+='<tr><td colspan=5><div class="empty" style="padding:30px 10px"><div class="ei"><svg viewBox="0 0 24 24" fill="none" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 3v12m0 0 4.5-4.5M12 15 7.5 10.5"/><path d="M4 16.5V19a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2v-2.5"/></svg></div><b>还没有导入过分享</b>粘贴一个网盘分享链接，几秒钟收进虚拟盘</div></td></tr>';
    for(var i=0;i<arr.length;i++){
      var s=arr[i];
      var st=s.auto_refresh, cat=s.category||'未分类';
      // 显示「网盘/分类」（如 夸克/电视剧/国产剧），一眼看出这部在哪个盘
      var pvShort=pvLabel(s.provider||'');
      var catPath=pvShort?pvShort+'/'+cat:cat;
      var warn='';
      if(s.fail_count>0){
        warn=' <span style="color:var(--danger);font-size:12px" title="'+esc(s.last_error||'')+'">⚠ 疑似失效 '+s.fail_count+' 轮</span>';
      }
      t+='<tr><td>'+esc(cleanShow(s.show_name))+warn+'<div class="tmeta">'+esc(catPath)+'</div></td><td class="num">'+s.files+'</td><td class="tm">'+fmtTime(s.last_added)+'</td>'+
        '<td><button class="mini '+(st?'ston':'gray')+'" onclick="toggleShare(\''+esc(s.provider)+'\',\''+esc(s.share_id)+'\','+(st?'true':'false')+')">'+(st?'追更中':'已完结')+'</button></td>'+
        '<td><button class="mini" onclick="refreshShare(\''+esc(s.provider)+'\',\''+esc(s.share_id)+'\',\''+esc(s.passcode||'')+'\')">刷新</button> '+
        '<button class="mini" data-p="'+esc(s.provider)+'" data-id="'+esc(s.share_id)+'" data-show="'+escA(cleanShow(s.show_name))+'" data-cur="'+escA(s.category||'')+'" onclick="openCatModal(this)" title="事后补救：导入时分类选错了就在这里改——这部剧的所有文件会在媒体库里搬到新分类，以后追更出来的新剧集也归新分类">分类</button> '+
        '<button class="mini danger" onclick="purgeShare(\''+esc(s.provider)+'\',\''+esc(s.share_id)+'\','+s.files+')">已失效</button> '+
        '<button class="mini gray" onclick="delShare(\''+esc(s.provider)+'\',\''+esc(s.share_id)+'\')">隐藏</button></td></tr>';
    }
    $('shareList').innerHTML=t;
    renderSharePager();
  }).catch(function(){$('shareList').innerHTML='<tr><td colspan="5" style="color:var(--mut)">读取失败</td></tr>'});
  loadHealth();
}
// 追更列表分页条：几百上千条时只渲染当前页，DOM 不会被撑爆
function renderSharePager(){
  var el=$('sharePager'); if(!el)return;
  var pages=Math.max(1,Math.ceil(SHARETOTAL/SHARESIZE));
  if(SHARETOTAL<=SHARESIZE){el.innerHTML='';return}
  el.innerHTML='<span class="sub" style="margin:0">共 '+SHARETOTAL+' 个分享 · 第 '+SHAREPAGE+' / '+pages+' 页</span>'+
    '<button class="mini gray" '+(SHAREPAGE<=1?'disabled':'')+' onclick="loadShares('+(SHAREPAGE-1)+')">上一页</button>'+
    '<button class="mini gray" '+(SHAREPAGE>=pages?'disabled':'')+' onclick="loadShares('+(SHAREPAGE+1)+')">下一页</button>';
}
// ---- 分享失效巡检 ----
function loadHealth(){
  var el=$('healthTip'); if(!el)return;
  fetch('/api/shares/health').then(function(r){return r.json()}).then(function(d){
    var s=d.state||{};
    var lr=s.last_run||'';
    if(lr.indexOf('0001-')===0)lr=''; // Go 零值时间，视为从未巡检
    var when=lr?fmtTime(lr):'尚未巡检';
    var txt='分享失效巡检：上次 '+when+' · 检查 '+(s.checked||0)+' 个分享';
    if(s.purged)txt+=' · 已清理 '+s.purged+' 个失效分享（'+(s.purged_eps||0)+' 个文件）';
    if(s.pending)txt+=' · '+s.pending+' 个待确认';
    if(s.note)txt+=' · '+s.note;
    el.innerHTML='<span>'+esc(txt)+'</span><button class="mini gray" onclick="runHealth(this)">扫描失效链接</button><span style="color:var(--mut)">（已知某个失效，直接点该行的「已失效」）</span>';
  }).catch(function(){});
}
function runHealth(btn){
  if(btn){btn.disabled=true;btn.textContent='扫描中…';}
  toast('正在全量扫描全部分享，数量多要一两分钟…');
  fetch('/api/shares/health',{method:'POST'}).then(function(r){return r.json()}).then(function(d){
    if(btn){btn.disabled=false;btn.textContent='扫描失效链接';}
    var s=d.state||{};
    loadHealth(); loadShares(); loadLib();
    var sus=s.suspected||[];
    if(s.purged)toast('已自动清理 '+s.purged+' 个失效分享，共 '+(s.purged_eps||0)+' 个文件');
    if(!sus.length){ if(!s.purged)toast('全部 '+(s.checked||0)+' 个分享正常'); return; }
    // 扫出疑似失效：列清单让用户拍板，一次点击就能清完
    var names=sus.map(function(x){return '· '+(x.show||x.share_id)+'（'+(PVCN[x.provider]||x.provider)+'）'+(x.fail<2?' · 首次发现':'')}).join('\n');
    openModal('发现 '+sus.length+' 个疑似失效的分享',
      '本次扫描全部 '+(s.checked||0)+' 个分享，以下 '+sus.length+' 个确认打不开：\n\n'+names+
      '\n\n立即清理？将删除它们的条目和 STRM 文件，不可恢复。\n取消则只做标记，定时巡检下一轮会按规则自动处理。',
      '全部清理',function(){ purgeMany(sus); });
  }).catch(function(){if(btn){btn.disabled=false;btn.textContent='扫描失效链接';}toast('扫描失败');});
}
// 批量清理扫描出的疑似失效分享
function purgeMany(list){
  toast('正在清理 '+list.length+' 个分享…');
  Promise.all(list.map(function(x){
    return fetch('/api/shares/purge',{method:'POST',headers:{'Content-Type':'application/json'},
      body:JSON.stringify({provider:x.provider,share_id:x.share_id})})
      .then(function(r){return r.json()}).then(function(d){return d.ok?1:0}).catch(function(){return 0});
  })).then(function(arr){
    var ok=arr.reduce(function(a,b){return a+b},0);
    toast('已清理 '+ok+' 个失效分享');
    loadShares(); loadLib(); loadHealth();
  });
}
function toggleShare(p,id,cur){
  fetch('/api/shares/toggle',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({provider:p,share_id:id,auto_refresh:!cur})})
  .then(function(r){return r.json()}).then(function(d){
    if(d.ok){toast(d.auto_refresh?'已开启追更':'已标记完结，不再追更');loadShares();}
    else toast(d.error||'操作失败');
  });
}
function delShare(p,id){
  openModal('移除分享','从追更列表移除这个分享？\n已入库的剧集保留在媒体库，仍可播放；重新导入同一分享可恢复追更。','移除',function(){
    fetch('/api/shares/'+encodeURIComponent(p)+'/'+encodeURIComponent(id),{method:'DELETE'})
    .then(function(r){return r.json()}).then(function(d){
      if(d.ok){toast('已移除');loadShares();}else toast(d.error||'操作失败');
    });
  });
}
function purgeShare(p,id,files){
  openModal('确认分享已失效','确认这一行的分享链接已经失效？\n\n将删除它的 '+files+' 个条目和对应的 STRM 文件，不可恢复。\n\n如果只是暂时打不开，请先点「刷新」重试，不要用这个。','清理',function(){
    fetch('/api/shares/purge',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({provider:p,share_id:id})})
    .then(function(r){return r.json()}).then(function(d){
      if(d.ok){toast('已清理「'+(d.show||p)+'」，删除 '+d.deleted+' 个条目');loadShares();loadLib();}
      else toast(d.error||'清理失败');
    });
  });
}
function refreshShare(p,id,pwd){
  toast('正在重新扫描目录树，剧集多要十几秒…');
  fetch('/api/shares/refresh',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({provider:p,share_id:id,passcode:pwd})})
  .then(function(r){return r.json()}).then(function(d){
    if(d.ok){toast(d.added>0?('「'+d.show+'」新增 '+d.added+' 个文件'):'「'+d.show+'」没有新文件，已是最新');loadShares();}
    else toast(d.error||'刷新失败');
  });
}
// ---- 事后改分类（两级：一级/二级联动下拉）----
var CATLIST=[], CATM=null;
function openCatModal(el){
  CATM={provider:el.getAttribute('data-p'),share_id:el.getAttribute('data-id'),show:el.getAttribute('data-show')||''};
  var cur=el.getAttribute('data-cur')||'';
  $('cmTitle').textContent='改分类：《'+(CATM.show||'')+'》';
  $('cmCur').textContent=cur||'不分类';
  fillCatSelects('cmSel1','cmSel2','cmNew1','cmNew2',cur);
  var m=$('catModal');m.style.display='flex';requestAnimationFrame(function(){m.classList.add('show')});
}
function closeCatModal(){var m=$('catModal');m.classList.remove('show');setTimeout(function(){m.style.display='none'},200);CATM=null}
// 弹窗里的一级/二级 onchange：与导入区同逻辑，只是 id 不同
function onCat1ChangeModal(){catSel1Change('cmSel1','cmSel2','cmNew1','cmNew2')}
function onCat2ChangeModal(){catSel2Change('cmSel2','cmNew2')}
function catSel1Change(i1,i2,n1,n2){
  var v=$(i1).value;
  $(n1).style.display=(v==='__new1__'?'':'none');
  $(n2).style.display='none';
  if(v==='__new1__'){$(i2).style.display='none';$(n1).focus();}
  else if(v){$(i2).style.display='';fillCat2(i2,v,'');}
  else $(i2).style.display='none';
}
// 新建一级时边输边把二级下拉挂到这个新一级下（输「电视剧」→ 二级可建「国产剧」）
function onCatNew1Input(i2,n1){
  var v=($(n1).value||'').trim();
  if(!v){$(i2).style.display='none';return;}
  $(i2).style.display='';
  fillCat2(i2,v,'');
}
function catSel2Change(i2,n2){
  var v=$(i2).value;
  $(n2).style.display=(v==='__new2__'?'':'none');
  if(v==='__new2__')$(n2).focus();
}
function doChangeCat(){
  if(!CATM)return;
  var cat=curCat('cmSel1','cmSel2','cmNew1','cmNew2');
  var m=CATM;closeCatModal();
  toast('正在改分类…');
  fetch('/api/shares/category',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({provider:m.provider,share_id:m.share_id,category:cat})})
  .then(function(r){return r.json()}).then(function(d){
    if(d.ok){
      toast('已改到「'+(d.category||'不分类')+'」，'+d.moved+' 个文件换了位置');
      loadShares();
      // 在媒体库里改的：目录结构变了，跳回根目录刷新一下
      var pl=document.getElementById('pg-lib');
      if(pl&&pl.style.display!=='none'){CURPATH=[];loadLib();}
    }
    else toast(d.error||'改分类失败');
  }).catch(function(){toast('改分类失败')});
}
// ---- 媒体库（OpenList 风格：面包屑 + 逐层进入 + 勾选批量 + 整剧删除） ----
var LIBT=null, CURPATH=[], VIEW='list', SEL=[], LIBQ='';
function svgFolder(){return '<svg class="fdi f" viewBox="0 0 24 24"><path d="M3 6.6A2.6 2.6 0 0 1 5.6 4h3.1a1 1 0 0 1 .74.33L11 6h7.4A2.6 2.6 0 0 1 21 8.6v8.8a2.6 2.6 0 0 1-2.6 2.6H5.6A2.6 2.6 0 0 1 3 17.4Z"/></svg>'}
function svgVideo(){return '<svg class="fdi" viewBox="0 0 24 24" fill="none" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="4" width="18" height="16" rx="2"/><path d="M7.5 4v16M16.5 4v16M3 9.5h4.5M3 14.5h4.5M16.5 9.5H21M16.5 14.5H21"/></svg>'}
function svgDoc(){return '<svg class="fdi" viewBox="0 0 24 24" fill="none" stroke-linecap="round" stroke-linejoin="round"><path d="M14 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8Z"/><path d="M14 3v5h5"/></svg>'}
function onLibQ(){clearTimeout(LIBT);LIBT=setTimeout(loadLib,250)}
function buildTree(){
  TREE={};
  ITEMS.forEach(function(it){
    var parts=((it.dir||'未分类')+'/'+(it.title||'未命名')+(it.ext||'')).split('/');
    var node=TREE;
    for(var i=0;i<parts.length-1;i++){ var k=parts[i]; node[k]=node[k]||{}; node=node[k]; }
    (node.__files=node.__files||[]).push(it);
  });
}
function nodeAt(path){
  var n=TREE;
  for(var i=0;i<path.length;i++){ if(!n||!n[path[i]]) return null; n=n[path[i]]; }
  return n;
}
function countFiles(node){
  var n=(node.__files||[]).length;
  for(var k in node){ if(k!=='__files') n+=countFiles(node[k]); }
  return n;
}
function collectIds(node){
  var out=[];
  (node.__files||[]).forEach(function(it){out.push(it.id)});
  for(var k in node){ if(k!=='__files') out=out.concat(collectIds(node[k])); }
  return out;
}
// 目录下所有文件若同属一个分享（= 一部剧/一部电影），返回分享信息，用于「分类」按钮。
// 网盘层、分类层会混多个分享，返回 null——那里不给分类入口。
function dirShare(node){
  var items=[];
  (function walk(n){
    (n.__files||[]).forEach(function(it){items.push(it)});
    for(var k in n){ if(k!=='__files') walk(n[k]); }
  })(node);
  if(!items.length) return null;
  var pv=items[0].provider, sid=items[0].share_id;
  if(!sid) return null;
  for(var i=1;i<items.length;i++){
    if(items[i].provider!==pv||items[i].share_id!==sid) return null;
  }
  return {provider:pv, share_id:sid, show:items[0].show||'', category:items[0].category||''};
}
function goPath(p){ CURPATH=p||[]; SEL=[]; paintLib(); }
function openLibPlayer(id){ openPlayer(id); }
function loadLib(){
  LIBQ=($('libQ')?$('libQ').value.trim().toLowerCase():'');
  $('libBox').innerHTML='<div class="loading"><span class="spin"></span>加载中…</div>';
  loadItems(function(){ buildTree(); paintLib(); });
}
function paintLib(){
  $('selbar').style.display='none';$('selbar').classList.remove('show');
  if(LIBQ){
    var hit=ITEMS.filter(function(it){return ((it.dir||'')+'/'+(it.title||'')+(it.ext||'')).toLowerCase().indexOf(LIBQ)>=0});
    if(!hit.length){ $('libBox').innerHTML='<div class="empty"><div class="ei">'+svgDoc()+'</div><b>没有匹配「'+esc(LIBQ)+'」的内容</b>换个关键词试试</div>'; $('crumb').innerHTML=''; return; }
    renderFlat(hit);
    $('crumb').innerHTML='<a class="cb" onclick="goPath([]);return false">'+ICO.home+' 根目录</a><span class="csep">/</span><span class="cb cur">搜索：'+esc(LIBQ)+'（'+hit.length+'）</span>';
    return;
  }
  renderLib();
}
function renderLib(){
  var node=nodeAt(CURPATH)||TREE;
  var cr='<a class="cb" onclick="goPath([]);return false">'+ICO.home+' 根目录</a>';
  var acc=[];
  for(var i=0;i<CURPATH.length;i++){ acc.push(CURPATH[i]); cr+='<span class="csep">/</span><a class="cb '+(i===CURPATH.length-1?'cur':'')+'" onclick="goPath('+escA(JSON.stringify(acc))+');return false">'+esc(cleanShow(CURPATH[i]))+'</a>'; }
  $('crumb').innerHTML=cr;
  var dirs=[], files=node.__files||[];
  for(var k in node){ if(k!=='__files') dirs.push(k); }
  dirs.sort();
  if(!dirs.length && !files.length){
    $('libBox').innerHTML='<div class="empty"><div class="ei">'+svgFolder()+'</div><b>媒体库还是空的</b>先去「分享导入」粘贴一个分享链接</div>'; return;
  }
  if(VIEW==='grid'){ renderGrid(node,dirs,files); return; }
  var h='<table class="lvt"><thead><tr>'+
    '<th class="ckc"><input type="checkbox" id="ckAllLib" onchange="toggleAllLib(this.checked)"></th>'+
    '<th>名称</th>'+
    '<th style="width:120px;text-align:right">大小</th>'+
    '<th style="width:150px;text-align:right">修改</th>'+
    '<th style="width:120px;text-align:right">操作</th>'+
    '</tr></thead><tbody>';
  dirs.forEach(function(name){
    var dn=node[name], cnt=countFiles(dn), ids=collectIds(dn), p=CURPATH.concat([name]);
    var si=dirShare(dn); // 单分享目录（剧名层）才给「分类」
    var ops=si
      ? '<button class="mini" data-act="cat">分类</button><button class="mini danger" data-act="del">删除</button>'
      : '<button class="mini danger" data-act="del">删除</button>';
    h+='<tr class="librow dirrow" data-type="dir" data-p="'+escA(JSON.stringify(p))+'" data-ids="'+escA(JSON.stringify(ids))+'" data-name="'+escA(name)+'" data-cnt="'+cnt+'"'+
      ' data-cat-p="'+escA(si?si.provider:'')+'" data-cat-id="'+escA(si?si.share_id:'')+'" data-cat-show="'+escA(si?cleanShow(si.show):'')+'" data-cat-cur="'+escA(si?si.category:'')+'">'+
      '<td class="ckc"><input type="checkbox" class="ckL" onchange="updLibSel()"></td>'+
      '<td><span class="fv" data-act="open">'+svgFolder()+'<span class="nm"><b>'+esc(cleanShow(name))+'</b><span class="dircnt">'+cnt+' 项</span></span></span></td>'+
      '<td class="libsz">—</td>'+
      '<td class="libtm">—</td>'+
      '<td class="ops"><span class="acts">'+ops+'</span></td>'+
      '</tr>';
  });
  files.forEach(function(it){
    var vi=VIDEO_EXTS.test(it.ext||'');
    h+='<tr class="librow" data-type="file" data-ids="'+escA(JSON.stringify([it.id]))+'" data-name="'+escA(it.title)+'" data-cnt="1">'+
      '<td class="ckc"><input type="checkbox" class="ckL" onchange="updLibSel()"></td>'+
      '<td><span class="fv '+(vi?'play':'')+'" data-act="play">'+ (vi?svgVideo():svgDoc()) +'<b>'+esc(it.title)+'</b></span></td>'+
      '<td class="libsz">'+humanSize(it.size||0)+'</td>'+
      '<td class="libtm">'+fmtTime(it.added_at)+'</td>'+
      '<td class="ops"><span class="acts"><button class="mini danger" data-act="del">删除</button></span></td>'+
      '</tr>';
  });
  h+='</tbody></table>';
  $('libBox').innerHTML=h;
  bindLibRows();
}
function renderFlat(hit){
  var h='<table class="lvt"><thead><tr>'+
    '<th class="ckc"><input type="checkbox" id="ckAllLib" onchange="toggleAllLib(this.checked)"></th>'+
    '<th>名称</th><th style="width:120px;text-align:right">大小</th><th style="width:150px;text-align:right">修改</th><th style="width:120px"></th></tr></thead><tbody>';
  hit.forEach(function(it){
    var vi=VIDEO_EXTS.test(it.ext||'');
    var path=(it.dir||'')+'/'+it.title;
    h+='<tr class="librow" data-type="file" data-ids="'+escA(JSON.stringify([it.id]))+'" data-name="'+escA(it.title)+'" data-cnt="1">'+
      '<td class="ckc"><input type="checkbox" class="ckL" onchange="updLibSel()"></td>'+
      '<td><span class="fv '+(vi?'play':'')+'" data-act="play" title="'+escA(path)+'">'+ (vi?svgVideo():svgDoc()) +'<b>'+esc(it.title)+'</b></span>'+
      '<div class="tmeta">'+esc(path)+'</div></td>'+
      '<td class="libsz">'+humanSize(it.size||0)+'</td>'+
      '<td class="libtm">'+fmtTime(it.added_at)+'</td>'+
      '<td class="ops"><span class="acts"><button class="mini danger" data-act="del">删除</button></span></td>'+
      '</tr>';
  });
  h+='</tbody></table>';
  $('libBox').innerHTML=h;
  bindLibRows();
}
function renderGrid(node,dirs,files){
  var h='<div class="gridview">';
  dirs.forEach(function(name){
    var dn=node[name], cnt=countFiles(dn), ids=collectIds(dn), p=CURPATH.concat([name]);
    h+='<div class="gcard" data-type="dir" data-p="'+escA(JSON.stringify(p))+'" data-ids="'+escA(JSON.stringify(ids))+'" data-name="'+escA(name)+'" data-cnt="'+cnt+'">'+svgFolder()+'<div class="gn">'+esc(cleanShow(name))+'</div><div class="gs">'+cnt+' 项</div></div>';
  });
  files.forEach(function(it){
    var vi=VIDEO_EXTS.test(it.ext||'');
    h+='<div class="gcard" data-type="file" data-ids="'+escA(JSON.stringify([it.id]))+'" data-name="'+escA(it.title)+'" data-cnt="1">'+ (vi?svgVideo():svgDoc()) +'<div class="gn">'+esc(it.title)+'</div><div class="gs">'+humanSize(it.size||0)+'</div></div>';
  });
  h+='</div>';
  $('libBox').innerHTML=h;
  var cards=document.querySelectorAll('#libBox .gcard');
  for(var i=0;i<cards.length;i++){
    (function(c){
      var type=c.getAttribute('data-type');
      var ids=JSON.parse(c.getAttribute('data-ids')||'[]');
      var p=JSON.parse(c.getAttribute('data-p')||'[]');
      c.onclick=function(){ if(type==='dir') goPath(p); else openLibPlayer(ids[0]); };
    })(cards[i]);
  }
}
function bindLibRows(){
  var rows=document.querySelectorAll('#libBox .librow');
  for(var i=0;i<rows.length;i++){
    (function(r){
      var type=r.getAttribute('data-type');
      var ids=JSON.parse(r.getAttribute('data-ids')||'[]');
      var p=JSON.parse(r.getAttribute('data-p')||'[]');
      var nm=r.getAttribute('data-name')||'';
      var cnt=+r.getAttribute('data-cnt');
      var fv=r.querySelector('.fv');
      if(fv){ fv.onclick=function(){ if(type==='dir') goPath(p); else openLibPlayer(ids[0]); }; }
      var acts=r.querySelectorAll('.acts [data-act]');
      for(var j=0;j<acts.length;j++){
        (function(b){
          var act=b.getAttribute('data-act');
          b.onclick=function(e){ e.stopPropagation();
            if(act==='open'){ if(type==='dir') goPath(p); else openLibPlayer(ids[0]); }
            else if(act==='play'){ openLibPlayer(ids[0]); }
            else if(act==='cat'){
              // 复用「改分类」弹窗：把行上的分享信息塞进一个临时元素
              var fake=document.createElement('button');
              fake.setAttribute('data-p',r.getAttribute('data-cat-p')||'');
              fake.setAttribute('data-id',r.getAttribute('data-cat-id')||'');
              fake.setAttribute('data-show',r.getAttribute('data-cat-show')||'');
              fake.setAttribute('data-cur',r.getAttribute('data-cat-cur')||'');
              openCatModal(fake);
            }
            else if(act==='del'){ delIds(ids,nm,cnt); }
          };
        })(acts[j]);
      }
      var cb=r.querySelector('.ckL');
      if(cb) cb.onchange=updLibSel;
    })(rows[i]);
  }
}
function updLibSel(){
  var cks=document.querySelectorAll('#libBox .ckL');
  var n=0; for(var i=0;i<cks.length;i++) if(cks[i].checked) n++;
  var ca=$('ckAllLib'); if(ca) ca.checked=(cks.length>0 && n===cks.length);
  if(n>0){
    var ids=[];
    var rows=document.querySelectorAll('#libBox .librow');
    for(var r=0;r<rows.length;r++){ var cb=rows[r].querySelector('.ckL'); if(cb&&cb.checked){ var a=JSON.parse(rows[r].getAttribute('data-ids')||'[]'); ids=ids.concat(a); } }
    SEL=ids;
    $('selInfo').textContent='已选 '+n+' 项';
    $('selbar').style.display='flex';requestAnimationFrame(function(){$('selbar').classList.add('show')});
  } else { SEL=[]; $('selbar').style.display='none';$('selbar').classList.remove('show'); }
}
function toggleAllLib(v){
  var cks=document.querySelectorAll('#libBox .ckL');
  for(var i=0;i<cks.length;i++) cks[i].checked=v;
  updLibSel();
}
function clearSel(){
  var cks=document.querySelectorAll('#libBox .ckL');
  for(var i=0;i<cks.length;i++) cks[i].checked=false;
  updLibSel();
}
function delIds(ids,name,cnt){
  openModal('删除'+(cnt>1?'目录':'文件'), '确定删除「'+name+'」'+(cnt>1?(' 及其下的 '+cnt+' 个文件'):'')+'？\n网盘里的文件不受影响，随时可重新导入。', '删除', function(){
    fetch('/api/shows/delete',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({ids:ids})})
    .then(function(r){return r.json()}).then(function(d){
      if(d.ok){ toast('已删除 '+d.deleted+' 个文件'); SEL=[]; loadLib(); }
      else toast(d.error||'删除失败');
    });
  });
}
function delSelected(){
  if(!SEL||!SEL.length){ toast('先勾选要删除的项'); return; }
  openModal('删除选中', '确定删除选中的 '+SEL.length+' 个文件？\n网盘里的文件不受影响。', '删除', function(){
    fetch('/api/shows/delete',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({ids:SEL})})
    .then(function(r){return r.json()}).then(function(d){
      if(d.ok){ toast('已删除 '+d.deleted+' 个文件'); SEL=[]; loadLib(); }
      else toast(d.error||'删除失败');
    });
  });
}
function toggleView(){
  VIEW=(VIEW==='list'?'grid':'list');
  var b=$('viewBtn'); if(b) b.textContent=(VIEW==='list'?'网格视图':'列表视图');
  paintLib();
}
// ---- 播放 ----
// 网页播放器走 /play 本机中转。哪些网盘能进，见 openPlayer 里的 relayable：
//   夸克、天翼 —— 可以；百度 —— 开了「浏览器代取」才可以；移动/115 —— 不行。
// 早期（115/百度时代）这里一刀切只放夸克，天翼接进来后忘了放开，导致
// 天翼条目点播放直接弹「不能在网页里播放」——2026-10-21 用户实测指出。
var PVHINT=false;
// 百度浏览器代取开关状态（设置页开关，影响网页播放器能否试播百度）
var BAIDURELAY=false;
function loadRelayFlag(){
  fetch('/api/strm/config').then(function(r){return r.json()}).then(function(d){
    if(d.ok&&d.config)BAIDURELAY=!!d.config.baidu_browser_relay;
  }).catch(function(){});
}
function openPlayer(id){
  var it=null;
  for(var i=0;i<ITEMS.length;i++){if(ITEMS[i].id===id)it=ITEMS[i]}
  if(!it)return;
  CURID=id;$('pvName').textContent=(it.dir?it.dir+'/':'')+(it.title||'')+(it.ext||'');
  $('player').style.display='flex';$('pvmsg').style.display='flex';
  // 哪些网盘能进网页播放器（走 /play 本机中转，见 handlePlay）：
  //   夸克      —— 一直可以（TV 302 / 中转）
  //   天翼      —— 直链支持 Range 且不绑会话，中转实测 206 正常（2026-10-21）
  //   百度      —— 默认 302 会被拒，只有打开「浏览器代取」才走中转
  //   移动/115  —— 只给 HLS/被封杀，不放开
  var relayable=(it.provider==='baidu'&&BAIDURELAY)||it.provider==='tianyi';
  if(it.provider&&it.provider!=='quark'&&!it.url&&!relayable){
    PVHINT=true;
    $('pv').removeAttribute('src');try{$('pv').load()}catch(e){}
    var tip='<b>'+(PVCN[it.provider]||it.provider)+'不能在网页里播放</b><br><small>直链只认播放器，浏览器会被网盘拒绝，这是正常现象。<br>请用飞牛影视 / Emby 通过 STRM 或 WebDAV 观看，<br>或点上方「复制直链」给外部播放器使用。</small>';
    if(it.provider==='baidu')tip='<b>百度默认 302 直连，浏览器会被拒</b><br><small>到「设置」打开「百度浏览器代取」后就能试播（流量经过本机）。<br>MKV / H.265 这类片源浏览器本身解不了，还是用飞牛 / Emby 看。</small>';
    if(it.provider==='mobile139')tip='<b>移动云盘只给 HLS(m3u8) 直链</b><br><small>浏览器不装 HLS 插件播不了，请用飞牛 / Emby 通过 STRM 或 WebDAV 观看。</small>';
    $('pvmsg').innerHTML=tip;
    return;
  }
  PVHINT=false;
  $('pvmsg').innerHTML='<span class="spin"></span>加载中…';
  $('pv').src='/play?id='+id;
}
function closePlayer(){PVHINT=false;$('pv').pause();$('pv').src='';$('player').style.display='none'}
$('pv').addEventListener('error',function(){
  if(PVHINT)return;
  $('pvmsg').innerHTML='加载失败<br><small>可能是取流链接过期或网盘限流，稍后重试；持续失败就去「设置」重新导入凭证</small>';
  $('pvmsg').style.display='flex';
});
$('pv').addEventListener('playing',function(){$('pvmsg').style.display='none'});
$('pv').addEventListener('canplay',function(){$('pvmsg').style.display='none'});
function copyLink(){
  fetch('/api/link?id='+CURID).then(function(r){return r.json()}).then(function(d){
    var u=d.url||'';
    function ok(){toast('直链已复制')}
    if(navigator.clipboard&&navigator.clipboard.writeText){
      navigator.clipboard.writeText(u).then(ok,function(){toast(u)});
    }else{
      var ta=document.createElement('textarea');ta.value=u;ta.style.position='fixed';ta.style.opacity='0';
      document.body.appendChild(ta);ta.select();
      try{document.execCommand('copy');ok()}catch(e){toast(u)}
      document.body.removeChild(ta);
    }
  }).catch(function(){toast('获取失败')});
}
// 一键复制设置页里的地址（带剪贴板降级，复制成功按钮上给个反馈）
function copyText(id,btn){
  var t=$(id).textContent;
  if(!t){toast('地址还没加载出来，稍等一下');return}
  function ok(){
    toast('已复制：'+t);
    if(btn){var o=btn.textContent;btn.textContent='已复制 ✓';setTimeout(function(){btn.textContent=o},1500)}
  }
  function fallback(){
    var ta=document.createElement('textarea');ta.value=t;ta.style.position='fixed';ta.style.opacity='0';
    document.body.appendChild(ta);ta.select();
    try{document.execCommand('copy')}catch(e){}
    document.body.removeChild(ta);
  }
  if(navigator.clipboard&&navigator.clipboard.writeText){
    navigator.clipboard.writeText(t).then(ok,function(){fallback();ok()});
  }else{fallback();ok()}
}
// ---- 设置 ----
function loadSet(){
  loadAccts();
  loadStrm();
  loadSettings();
}
// ---- 全局播放设置（夸克 TV 302 开关）----
function loadSettings(){
  fetch('/api/settings').then(function(r){return r.json()}).then(function(d){
    if(d.ok&&d.settings)$('quarkTV302').checked=!!d.settings.quark_tv_302;
  }).catch(function(){});
}
function saveQuarkTV302(){
  var on=$('quarkTV302').checked;
  fetch('/api/settings',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({quark_tv_302:on})})
  .then(function(r){return r.json()}).then(function(d){
    if(d.ok)toast(on?'已开启：夸克改走 302 直连（立即生效）':'已关闭：夸克改回本机中转（立即生效）');
    else{toast(d.error||'保存失败');$('quarkTV302').checked=!on}
  }).catch(function(){toast('网络错误，重试看看');$('quarkTV302').checked=!on});
}
// ---- STRM 导出配置 ----
var STRMC=null;
function loadStrm(){
  fetch('/api/strm/config').then(function(r){return r.json()}).then(function(d){
    if(!d.ok)return;
    STRMC=d.config||{};
    $('strmDir').value=STRMC.dir||'';
    $('strmBase').value=STRMC.base_url||'';
    $('strmLayout').value=STRMC.layout||'full';
    $('strmExts').value=(STRMC.exts||[]).join(',');
    $('baiduRelay').checked=!!STRMC.baidu_browser_relay;
    var hint='当前生效：'+(d.dir||'—')+' · '+(d.base_url||'未配置地址');
    var ba=d.built_at||'';
    if(ba&&ba.indexOf('0001-')!==0)hint+=' · 上次重建 '+fmtTime(ba);
    $('strmHint').textContent=hint;
    if(d.stale){
      $('strmStaleMsg').textContent='检测到对外地址从 '+(d.built_base||'（未知）')+' 变成了 '+(d.base_url||'（未配置）')+'：已经生成的 .strm 还指着旧地址，媒体库里的老片子会播不了。自动同步只补新增和删除、不重写旧文件（一次全重写会让媒体库把整个库重新刮削一遍，所以设成手动）。请点上面的「全量重建 STRM」一次性更新。';
      $('strmStale').style.display='block';
    }else{
      $('strmStale').style.display='none';
    }
  }).catch(function(){});
}
function rebuildSTRM(){
  openModal('全量重建 STRM','把 .strm 目录按当前配置重新同步一遍。\n\n· 对外地址没变：只补新增/删除，媒体库基本无感；\n· 对外地址变了：所有文件都会重写（旧地址已失效，必须换），媒体库会重新刮削全库——片子多时要等一会儿，属正常现象。\n\n追更、导入、删除走的都是自动增量同步，不受影响。','开始重建',function(){
    toast('正在全量重建 STRM…');
    fetch('/api/strm/rebuild',{method:'POST'}).then(function(r){return r.json()}).then(function(d){
      if(d.ok){toast('重建完成：写入 '+d.count+' 个 .strm');loadStrm();}
      else toast(d.error||'重建失败');
    }).catch(function(){toast('重建失败')});
  });
}
function saveStrm(){
  toast('正在保存并重建 STRM…');
  var body={
    dir:$('strmDir').value.trim(),
    base_url:$('strmBase').value.trim(),
    layout:$('strmLayout').value,
    exts:$('strmExts').value.split(',').map(function(s){return s.trim()}).filter(Boolean),
    providers:(STRMC&&STRMC.providers)||[],
    categories:(STRMC&&STRMC.categories)||[],
    auto_rebuild:STRMC?!!STRMC.auto_rebuild:true,
    baidu_browser_relay:$('baiduRelay').checked
  };
  fetch('/api/strm/config',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)})
  .then(function(r){return r.json()}).then(function(d){
    if(d.ok){toast('已重建 '+d.count+' 个 .strm');loadStrm();loadRelayFlag();}
    else toast(d.error||'保存失败');
  }).catch(function(){toast('保存失败')});
}
// ---- 网盘连接中心：全部网盘列表 + 引导式连接弹窗 ----
var PV_LIST=[
  {pv:'quark',name:'夸克网盘'},
  {pv:'115',name:'115网盘'},
  {pv:'baidu',name:'百度网盘'},
  {pv:'tianyi',name:'天翼云盘'},
  {pv:'mobile139',name:'移动云盘'}
];
// 连接方式配置。methods：scan=扫码 / cookie=粘贴。
// 夸克二期已接夸克TV扫码：同一「夸克网盘」入口下，
//   扫码 Tab → 夸克TV（302 直链取流），Cookie Tab → 夸克普通（分享转存取流）。
// 两个凭证分别以 provider="quark_tv" / "quark" 存入账号库，互不干扰。
var PV_CONN={
  quark:{name:'夸克网盘',methods:['scan','cookie'],importApi:'/api/quark/import',
    needBoth:true,
    scanStart:'/api/login/quark/start',scanPoll:'/api/login/quark/poll',
    scanSteps:['打开手机上的夸克 TV / 当贝 / 电视版 App（或夸克 App 的 TV 登录入口）','用 App 里的「扫码登录」扫下面的码','手机上点<b>「确认登录」</b>，本页面会提示还差 Cookie，接着做就行'],
    scanNote:'夸克 302 直连需要<b>两样凭证</b>：<b>扫码</b>走夸克TV，播放时给 302 直链（不耗本机流量）；<b>Cookie</b>走夸克普通版，分享转存才用它。在这个弹窗里依次做完——做完一样不会关掉，接着做另一样，两样都齐才提示连接成功。',
    cookiePh:'夸克 Cookie 整串',
    steps:['电脑浏览器打开 <code>pan.quark.cn</code>，登录你的夸克','按键盘 <b>F12</b> → 点上面「网络 / Network」','左边随便点一个请求 → 请求头里找到 <b>Cookie</b> → 整串复制','粘到下面框里，点「导入连接」']},
  '115':{name:'115网盘',methods:['scan','cookie'],importApi:'/api/accounts/import',
    scanStart:'/api/login/115/start',scanPoll:'/api/login/115/poll',
    scanSteps:['打开手机 <b>115 App</b> → 右上角「+」→「扫一扫」','扫下面的码 → 手机上点<b>「确认登录」</b>','本页面自动变成「登录成功」'],
    cookiePh:'115 Cookie 整串',
    steps:['电脑浏览器打开 <code>115.com</code>，登录','按 <b>F12</b> → 「网络 / Network」','随便点一个请求 → 复制请求头里的 <b>整串 Cookie</b>','粘到下面框里，点「导入连接」']},
  baidu:{name:'百度网盘',methods:['scan','cookie'],importApi:'/api/accounts/import',
    scanStart:'/api/login/baidu/start',scanPoll:'/api/login/baidu/poll',
    scanSteps:['打开手机<b>百度 App</b>（或百度网盘 App）→「扫一扫」','扫下面的码 → 手机上点<b>「确认登录」</b>','本页面自动变成「登录成功」'],
    cookiePh:'BDUSS，或整串 Cookie',
    steps:['电脑浏览器打开 <code>pan.baidu.com</code>，登录','按 <b>F12</b> → 「网络 / Network」','随便点一个请求 → 复制请求头里的 <b>整串 Cookie</b>（只有 BDUSS 也行）','粘到下面框里，点「导入连接」']},
  tianyi:{name:'天翼云盘',methods:['scan'],
    scanStart:'/api/login/tianyi/start',scanPoll:'/api/login/tianyi/poll',
    scanSteps:['打开手机上的<b>天翼云盘 App</b> → 右上角「+」→「扫一扫」','扫下面的码 → 手机上点<b>「确认登录」</b>','本页面自动变成「登录成功」'],
    scanNote:'天翼是 accessToken 登录，<b>没有 Cookie 可粘，只能扫码</b>。token 有效期约 7 天，过期了再来扫一次就行。<b>解析分享不需要登录</b>，只有播放取流才要凭证。想订阅资源号自动收新片，去左边「<b>天翼订阅</b>」页。'},
  mobile139:{name:'移动云盘',methods:['cookie'],importApi:'/api/accounts/import',
    cookiePh:'Authorization 里 Basic 后面那串',
    steps:['电脑浏览器打开 <code>yun.139.com</code>，登录移动云盘','按 <b>F12</b> → 「网络 / Network」','随便点一个请求 → 请求头里找 <b>Authorization</b>','复制 <code>Basic</code> 后面那一长串（整串粘也行）→ 点「导入连接」']}
};
// 渲染全部网盘列表：连了的亮状态+退出按钮，没连的出「连接」按钮
function loadAccts(){
  fetch('/api/accounts').then(function(r){return r.json()}).then(function(d){
    var arr=d.accounts||d||[];if(!Array.isArray(arr))arr=[];
    var map={};for(var i=0;i<arr.length;i++)map[arr[i].provider]=arr[i];
    ACCTS_MAP=map; // 缓存：连接弹窗要据此判断「另一样凭证连了没」
    var t='<tr><th style="width:150px">网盘</th><th>状态</th><th style="width:100px;text-align:right">操作</th></tr>';
    for(var j=0;j<PV_LIST.length;j++){
      var p=PV_LIST[j],a=map[p.pv];
      if(p.pv==='quark'){
        // 夸克一个入口两种凭证：quark=Cookie（转存取流），quark_tv=扫码（302 直链）
        var tv=map['quark_tv'];
        if(a&&tv){
          t+='<tr><td><span class="tag '+(PVCLS[p.pv]||'')+'">'+esc(p.name)+'</span></td>'+
            '<td><b style="color:var(--ok)">✓ 已连接</b> <span class="cd">Cookie + 扫码（302）</span></td>'+
            '<td style="text-align:right"><button class="mini danger" onclick="delAcc(\''+p.pv+'\')">退出/换号</button></td></tr>';
        }else if(a){
          t+='<tr><td><span class="tag '+(PVCLS[p.pv]||'')+'">'+esc(p.name)+'</span></td>'+
            '<td><b style="color:var(--ok)">✓ 已连接</b> <span class="cd">'+esc(a.label||'')+' · '+fmtTime(a.login_at)+'</span><br>'+
            '<span class="cd" style="color:var(--warn)">再扫码登录一次，可解锁 302 直链播放</span></td>'+
            '<td style="text-align:right"><button class="mini danger" onclick="delAcc(\''+p.pv+'\')">退出/换号</button></td></tr>';
        }else if(tv){
          t+='<tr><td><span class="tag '+(PVCLS[p.pv]||'')+'">'+esc(p.name)+'</span></td>'+
            '<td><b style="color:var(--ok)">✓ 已连接</b> <span class="cd">扫码登录 · '+fmtTime(tv.login_at)+'</span><br>'+
            '<span class="cd" style="color:var(--warn)">再导入 Cookie，分享转存才好用</span></td>'+
            '<td style="text-align:right"><button class="mini danger" onclick="delAcc(\''+p.pv+'\')">退出/换号</button></td></tr>';
        }else{
          t+='<tr><td><span class="tag '+(PVCLS[p.pv]||'')+'">'+esc(p.name)+'</span></td>'+
            '<td style="color:var(--mut)">未连接<span class="cd" style="margin-left:6px">播放取流前要连上</span></td>'+
            '<td style="text-align:right"><button class="mini" onclick="openConn(\''+p.pv+'\')">连接</button></td></tr>';
        }
      }else if(a){
        t+='<tr><td><span class="tag '+(PVCLS[p.pv]||'')+'">'+esc(p.name)+'</span></td>'+
          '<td><b style="color:var(--ok)">✓ 已连接</b> <span class="cd">'+esc(a.label||'')+' · '+fmtTime(a.login_at)+'</span></td>'+
          '<td style="text-align:right"><button class="mini danger" onclick="delAcc(\''+p.pv+'\')">退出/换号</button></td></tr>';
      }else{
        t+='<tr><td><span class="tag '+(PVCLS[p.pv]||'')+'">'+esc(p.name)+'</span></td>'+
          '<td style="color:var(--mut)">未连接<span class="cd" style="margin-left:6px">播放取流前要连上</span></td>'+
          '<td style="text-align:right"><button class="mini" onclick="openConn(\''+p.pv+'\')">连接</button></td></tr>';
      }
    }
    $('accts').innerHTML=t;
  }).catch(function(){$('accts').innerHTML='<tr><td colspan="3" style="color:var(--mut)">读取失败，刷新页面试试</td></tr>'});
}
var CONN_PV=null,CONN_SCAN_TIMER=null,CONN_SCAN_CD=null,CONN_DONE={scan:false,cookie:false};
var ACCTS_MAP={}; // 最近一次 /api/accounts 的 provider→账号 映射
function openConn(pv){
  var c=PV_CONN[pv];if(!c)return;
  CONN_PV=pv;
  // 从已装账号恢复进度（重开弹窗时勾不能丢）
  CONN_DONE={scan:!!ACCTS_MAP['quark_tv'],cookie:!!ACCTS_MAP['quark']};
  $('connTitle').innerHTML='连接 '+esc(c.name);
  var h='';
  if(c.needBoth)h+='<div id="connBanner" style="border:1px solid var(--line);border-radius:10px;padding:12px 14px;margin-bottom:14px;background:rgba(32,128,240,.04)"></div>';
  if(c.methods.length>1){
    h+='<div class="row" style="margin-bottom:14px">';
    h+='<button id="connTabScan" style="flex:1" onclick="renderConn(\'scan\')">📱 扫码登录（推荐）</button>';
    h+='<button id="connTabCookie" class="gray" style="flex:1" onclick="renderConn(\'cookie\')">📋 粘贴 Cookie</button>';
    h+='</div>';
  }
  h+='<div id="connPane"></div>';
  $('connBody').innerHTML=h;
  $('connModal').style.display='flex';requestAnimationFrame(function(){$('connModal').classList.add('show')});
  if(c.needBoth)updateConnBanner();
  renderConn(c.methods[0]);
}
// 夸克专属：进度横幅——两样凭证各自的状态 + 还差什么的指引。
function updateConnBanner(){
  var b=$('connBanner');if(!b)return;
  var s=CONN_DONE.scan,c=CONN_DONE.cookie;
  var okc='var(--ok)',mut='var(--mut)';
  var prog='<div style="font-size:13px;margin-bottom:8px">连接进度：<b style="color:'+(s?okc:mut)+'">'+(s?'✓ 扫码登录':'① 扫码登录')+'</b> ＋ <b style="color:'+(c?okc:mut)+'">'+(c?'✓ Cookie':'② Cookie')+'</b></div>';
  var msg;
  if(s&&c)msg='<div style="color:var(--ok);font-weight:600">🎉 两样都连好了，夸克 302 直链播放已解锁！</div>';
  else if(s)msg='<div class="sub" style="color:var(--warn)">还差 <b>Cookie</b>（分享转存用它）——已帮你切到「粘贴 Cookie」，贴完点「导入连接」。</div>';
  else if(c)msg='<div class="sub" style="color:var(--warn)">还差 <b>扫码登录</b>（302 直链播放用它）——已帮你切到「扫码登录」，用手机夸克扫一下。</div>';
  else msg='<div class="sub">夸克 302 直连需要<b>两样凭证</b>：扫码登录取直链、Cookie 管转存。在这个弹窗里依次做完，做完一样不会关掉，接着做另一样。</div>';
  b.innerHTML=prog+msg;
}
// tab 标签带完成勾
function markConnTabs(){
  if(!$('connTabScan'))return;
  $('connTabScan').innerHTML='📱 扫码登录'+(CONN_DONE.scan?' ✓':'');
  $('connTabCookie').innerHTML='📋 粘贴 Cookie'+(CONN_DONE.cookie?' ✓':'');
}
// 某一样凭证完成：夸克（needBoth）不关弹窗，切到还差的那样接着做；
// 两样都齐（或其他网盘单选）才收尾。
function connDone(m){
  var c=PV_CONN[CONN_PV];if(!c)return;
  CONN_DONE[m]=true;
  // 记到「当前网盘」的键上：夸克扫码记 quark_tv（与 Cookie 的 quark 分开存），
  // 其他网盘扫码记自己的 provider —— 不能一律写 quark_tv，会污染夸克弹窗状态。
  var scanKey=CONN_PV==='quark'?'quark_tv':CONN_PV;
  if(m==='scan')ACCTS_MAP[scanKey]={provider:scanKey};
  else ACCTS_MAP[CONN_PV]={provider:CONN_PV};
  updateConnBanner();
  if(c.needBoth&&!(CONN_DONE.scan&&CONN_DONE.cookie)){
    markConnTabs();
    renderConn(m==='scan'?'cookie':'scan'); // 自动切到还差的那样
    return;
  }
  if(c.needBoth){setTimeout(function(){closeConn();toast('连接成功，立即生效');loadSet()},1200);return}
  closeConn();toast('连接成功，立即生效');loadSet();
}
function renderConn(m){
  var c=PV_CONN[CONN_PV];if(!c)return;
  if(CONN_SCAN_TIMER){clearInterval(CONN_SCAN_TIMER);CONN_SCAN_TIMER=null}
  if(CONN_SCAN_CD){clearInterval(CONN_SCAN_CD);CONN_SCAN_CD=null}
  if($('connTabScan')){$('connTabScan').className=m==='scan'?'ston':'gray';$('connTabCookie').className=m==='cookie'?'ston':'gray'}
  markConnTabs();
  var h='';
  if(m==='cookie'){
    if(c.needBoth&&CONN_DONE.cookie)h+='<div style="color:var(--ok);font-size:13.5px;font-weight:600;margin-bottom:8px">✓ Cookie 已导入（换号直接贴新的覆盖）</div>';
    h+='<div style="font-size:13.5px;font-weight:600;margin-bottom:8px">怎么弄（一分钟搞定）：</div>';
    h+='<ol style="margin:0 0 14px;padding-left:20px;line-height:2;color:var(--txt);font-size:13.5px">';
    for(var i=0;i<c.steps.length;i++)h+='<li>'+c.steps[i]+'</li>';
    h+='</ol>';
    h+='<div class="row"><input type="text" id="connCk" placeholder="'+esc(c.cookiePh)+'" style="flex:1;min-width:200px">';
    h+='<button onclick="connImport()">导入连接</button></div>';
    h+='<div class="sub" style="margin-top:10px">解析分享不需要凭证，播放取流才要。导一次就行，换号直接再导。</div>';
  }else{
    h+='<div style="font-size:13.5px;font-weight:600;margin-bottom:8px">扫码登录 · 照着做：</div>';
    h+='<ol style="margin:0 0 14px;padding-left:20px;line-height:2;color:var(--txt);font-size:13.5px">';
    for(var i=0;i<c.scanSteps.length;i++)h+='<li>'+c.scanSteps[i]+'</li>';
    h+='</ol>';
    if(c.needBoth&&CONN_DONE.scan){
      // 已扫过：不重复起会话，给个完成态 + 换号重扫入口
      h+='<div style="text-align:center;color:var(--ok);font-size:14px;font-weight:600;padding:18px 0">✓ 扫码登录已完成<br><span style="font-size:13px;font-weight:400">要换号？点下面重新扫</span><br><button class="mini" style="margin-top:10px" onclick="CONN_DONE.scan=false;markConnTabs();updateConnBanner();renderConn(\'scan\')">重新扫码</button></div>';
    }else{
      h+='<div style="text-align:center"><img id="connQRImg" src="" style="width:200px;height:200px;border-radius:8px;background:#fff;border:1px solid var(--line)"></div>';
      h+='<div id="connQRMsg" style="margin-top:10px;text-align:center;font-size:13.5px;color:var(--mut)">正在获取二维码…</div>';
      h+='<div id="connQRTTL" style="margin-top:4px;text-align:center;font-size:12.5px;color:var(--mut)"></div>';
    }
    h+='<div class="sub" style="margin-top:12px">'+(c.scanNote||'扫码没反应或反复失败？切到「粘贴 Cookie」方式，基本不会失败。')+'</div>';
  }
  $('connPane').innerHTML=h;
  if(m==='cookie'){var el=$('connCk');if(el)el.focus()}
  if(m==='scan'&&!(c.needBoth&&CONN_DONE.scan))connScan();
}
function connScan(){
  var c=PV_CONN[CONN_PV];
  fetch(c.scanStart,{method:'POST'}).then(function(r){return r.json()}).then(function(d){
    if(!d.ok){$('connQRMsg').textContent=d.error||'获取二维码失败';return}
    $('connQRImg').src=d.qr;
    $('connQRMsg').textContent='等待扫码…';
    var expireAt=Date.now()+(d.ttl||600)*1000;
    if(CONN_SCAN_CD)clearInterval(CONN_SCAN_CD);
    CONN_SCAN_CD=setInterval(function(){
      var left=Math.max(0,Math.round((expireAt-Date.now())/1000));
      $('connQRTTL').textContent=left>0?('二维码 '+Math.floor(left/60)+' 分 '+(left%60)+' 秒后过期'):'二维码已过期，关掉弹窗重点「连接」即可';
      if(left<=0)clearInterval(CONN_SCAN_CD);
    },1000);
    if(CONN_SCAN_TIMER)clearInterval(CONN_SCAN_TIMER);
    CONN_SCAN_TIMER=setInterval(function(){
      fetch(c.scanPoll+'?sid='+d.sid).then(function(r){return r.json()}).then(function(p){
        if(!p.ok){clearInterval(CONN_SCAN_TIMER);$('connQRMsg').textContent=p.error||'失败';return}
        if(p.message)$('connQRMsg').textContent=p.message;
        if(p.done){
          clearInterval(CONN_SCAN_TIMER);if(CONN_SCAN_CD)clearInterval(CONN_SCAN_CD);
          if(p.error){$('connQRMsg').textContent=p.error;toast(p.error)}
          else{connDone('scan')}
        }
      }).catch(function(){});
    },2000);
  }).catch(function(){$('connQRMsg').textContent='网络错误，重试看看'});
}
function connImport(){
  var c=PV_CONN[CONN_PV],ck=$('connCk').value.trim();
  if(!ck){toast('先粘贴凭证再导入');return}
  var body=CONN_PV==='quark'?{cookie:ck}:{provider:CONN_PV,cookie:ck};
  fetch(c.importApi,{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)})
  .then(function(r){return r.json()}).then(function(d){
    if(d.ok){$('connCk').value='';connDone('cookie')}
    else toast(d.error||'导入失败');
  }).catch(function(){toast('网络错误，重试看看')});
}
function closeConn(){
  if(CONN_SCAN_TIMER){clearInterval(CONN_SCAN_TIMER);CONN_SCAN_TIMER=null}
  if(CONN_SCAN_CD){clearInterval(CONN_SCAN_CD);CONN_SCAN_CD=null}
  var m=$('connModal');m.classList.remove('show');setTimeout(function(){m.style.display='none'},200);
}
function delAcc(p){
  openModal('退出 / 换号','退出后该网盘会立即停止取流（正在播放的会中断）。\n\n随后直接导入新账号即可：系统会自动丢弃旧凭证的播放直链，百度的旧临时转存文件也会在后台清理——不需要重启，不需要任何额外操作。','退出',function(){
    fetch('/api/accounts/'+p,{method:'DELETE'}).then(function(r){return r.json()}).then(function(d){
      if(d.ok){toast(d.note||'已退出登录');loadSet();}else toast(d.error||'操作失败');
    });
  });
}
// ---- 天翼订阅号 ----
var TS_MAP={},TS_CUR=null,TS_PAGE=1,TS_SIZE=50;
function loadTianyiSubs(){
  fetch('/api/tianyi/subs').then(function(r){return r.json()}).then(function(d){
    var arr=d.subs||[];TS_MAP={};
    if(!arr.length){
      $('tsList').innerHTML='<tr><td style="color:var(--mut);padding:14px 0">还没有订阅号。把天翼 App 里资源号主页的分享链接粘到上面，点「添加订阅」。</td></tr>';
      return;
    }
    var t='<tr><th>订阅号</th><th>状态</th><th style="width:250px;text-align:right">操作</th></tr>';
    for(var i=0;i<arr.length;i++){
      var e=arr[i];TS_MAP[e.user_id]=e;
      var st=(e.auto_import?'<b style="color:var(--ok)">自动收录</b>':'<span class="cd">仅手动</span>')+
        ' <span class="cd">已收 '+e.imported+' 个分享'+
        (e.last_sync&&e.last_sync.indexOf('0001')!==0?' · 上次同步 '+esc(e.last_sync.slice(0,16).replace('T',' ')):'')+'</span>'+
        (e.last_error?'<br><span class="cd" style="color:var(--danger)">'+esc(e.last_error)+'</span>':'');
      t+='<tr><td><b>'+esc(e.name||e.user_id)+'</b><br><span class="cd">'+esc(e.user_id)+(e.category?' · 分类 '+esc(e.category):'')+'</span></td>'+
        '<td>'+st+'</td>'+
        '<td style="text-align:right;white-space:nowrap">'+
        '<button class="mini gray" onclick="tsBrowse(\''+e.user_id+'\',1)">浏览分享</button> '+
        '<button class="mini gray" onclick="tsBackfill(\''+e.user_id+'\')">导入历史</button> '+
        '<button class="mini" onclick="tsSync(\''+e.user_id+'\')">立即同步</button> '+
        '<button class="mini danger" onclick="tsDel(\''+e.user_id+'\')">删除</button></td></tr>';
    }
    $('tsList').innerHTML=t;
  }).catch(function(){toast('加载订阅失败')});
}
function addTianyiSub(){
  var link=$('tsLink').value.trim();
  if(!link){toast('先粘贴订阅号链接或 ID');return}
  fetch('/api/tianyi/subs',{method:'POST',headers:{'Content-Type':'application/json'},
    body:JSON.stringify({link:link,category:$('tsCat').value.trim(),auto_import:$('tsAuto').checked})})
  .then(function(r){return r.json()}).then(function(d){
    if(d.ok){$('tsLink').value='';$('tsCat').value='';toast('已添加订阅：'+(d.sub.name||d.sub.user_id));loadTianyiSubs();}
    else toast(d.error||'添加失败');
  }).catch(function(){toast('网络错误，重试看看')});
}
function tsDel(uid){
  var e=TS_MAP[uid]||{};
  openModal('删除订阅','确定不再关注「'+(e.name||uid)+'」吗？\n\n只删订阅登记，已经收进媒体库的影片不受影响，照常播放。','删除',function(){
    fetch('/api/tianyi/subs/'+encodeURIComponent(uid),{method:'DELETE'}).then(function(r){return r.json()}).then(function(d){
      if(d.ok){toast('已删除订阅');if(TS_CUR===uid)$('tsShares').innerHTML='';loadTianyiSubs();}else toast('删除失败');
    });
  });
}
function tsSync(uid){
  toast('同步中…');
  fetch('/api/tianyi/sub-sync',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({user_id:uid})})
  .then(function(r){return r.json()}).then(function(d){
    var r=(d.results||[])[0]||{};
    if(r.error)toast('同步失败：'+r.error);
    else toast('同步完成，新收录 '+r.imported+' 个分享');
    loadTianyiSubs();
    if(TS_CUR===uid)tsBrowse(uid,1);
  }).catch(function(){toast('同步失败')});
}
function tsBrowse(uid,page){
  TS_CUR=uid;TS_PAGE=page||1;
  fetch('/api/tianyi/sub-shares?user_id='+encodeURIComponent(uid)+'&page='+TS_PAGE+'&size='+TS_SIZE)
  .then(function(r){return r.json()}).then(function(d){
    if(!d.ok){toast(d.error||'拉取失败');return}
    var list=d.shares||[];
    var t='<div class="chd" style="margin-bottom:8px"><b>'+esc((TS_MAP[uid]||{}).name||uid)+' 的分享</b><span class="csub">共 '+d.total+' 条 · 第 '+d.page+' 页</span></div>'+
      '<table><tr><th>分享</th><th style="width:150px">发布时间</th><th style="width:130px">状态</th><th style="width:90px;text-align:right">操作</th></tr>';
    if(!list.length)t+='<tr><td colspan="4" style="color:var(--mut)">没有更多了</td></tr>';
    for(var i=0;i<list.length;i++){
      var s=list[i];
      var state=s.imported>0?'<b style="color:var(--ok)">已收录 '+s.imported+' 条</b>':(s.is_new?'<b style="color:var(--pri)">新的</b>':'<span class="cd">未收录</span>');
      t+='<tr><td>'+esc(s.name)+(s.is_folder?'':' <span class="cd">单文件</span>')+'</td>'+
        '<td class="cd">'+esc((s.share_date||'').slice(0,16))+'</td>'+
        '<td>'+state+'</td>'+
        '<td style="text-align:right"><button class="mini'+(s.imported>0?' gray':'')+'" onclick="tsImport(\''+uid+'\','+s.share_id+')">'+(s.imported>0?'重收':'收录')+'</button></td></tr>';
    }
    t+='</table><div style="display:flex;justify-content:flex-end;gap:8px;margin-top:10px">'+
      (d.page>1?'<button class="mini gray" onclick="tsBrowse(\''+uid+'\','+(d.page-1)+')">上一页</button>':'')+
      ((d.page*d.size<d.total)?'<button class="mini" onclick="tsBrowse(\''+uid+'\','+(d.page+1)+')">下一页</button>':'')+
      '</div>';
    $('tsShares').innerHTML=t;
  }).catch(function(){toast('拉取失败')});
}
function tsImport(uid,sid){
  toast('收录中，目录多的话要等一会儿…');
  fetch('/api/tianyi/sub-import',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({user_id:uid,share_id:sid})})
  .then(function(r){return r.json()}).then(function(d){
    if(d.ok){toast('已收录 '+(d.title||'')+'：'+d.added+' 条');tsBrowse(uid,TS_PAGE);}
    else toast(d.error||'收录失败');
  }).catch(function(){toast('收录失败')});
}
// ---- 天翼订阅号：历史导入（后台慢跑 + 进度轮询） ----
var TS_BF_TIMER=null,TS_BF_REFRESHED=false;
function tsBackfill(uid){
  var e=TS_MAP[uid]||{};
  openModal('一键导入历史','把「'+(e.name||uid)+'」的全部历史分享，一条一条慢慢收进媒体库。\n\n中途可以点「停止」，下次再点会从断点接着跑，不会重复。分享多的话可能要跑一两个小时，点完就可以关掉页面。\n\n现在开始？','开始导入',function(){
    fetch('/api/tianyi/sub-backfill',{method:'POST',headers:{'Content-Type':'application/json'},
      body:JSON.stringify({user_id:uid,max_shares:0,gap_ms:1000})})
    .then(function(r){return r.json()}).then(function(d){
      if(!d.ok){toast(d.error||'启动失败');return}
      TS_BF_REFRESHED=false;
      toast('已开始，后台慢慢跑，可以关页面');
      tsBfPoll(true);
    }).catch(function(){toast('启动失败')});
  });
}
function tsBfStop(){
  fetch('/api/tianyi/sub-backfill',{method:'DELETE'}).then(function(r){return r.json()}).then(function(){
    toast('已请求停止：当前这条处理完就停，断点已保存');
  }).catch(function(){});
}
function tsBfPoll(refreshOnFinish){
  if(TS_BF_TIMER)return;
  var tick=function(){
    fetch('/api/tianyi/sub-backfill').then(function(r){return r.json()}).then(function(d){
      var j=d.job||{},m=$('tsBfMsg'),sb=$('tsBfStop');
      if(!m)return;
      if(j.running){
        if(sb)sb.style.display='';
        m.innerHTML='导入中 '+j.done+'/'+j.total+' · 已收 '+j.imported+' 个分享（'+j.added+' 条）'+
          (j.current?'<br>当前：'+esc(j.current):'');
      }else{
        if(sb)sb.style.display='none';
        if(j.started&&String(j.started).indexOf('0001')!==0){
          m.innerHTML='上次导入结束：处理 '+j.done+' 条，收录 '+j.imported+' 个分享（'+j.added+' 条）'+
            (j.error?' · <span style="color:var(--danger)">'+esc(j.error)+'</span>':'');
        }
        if(TS_BF_TIMER){clearInterval(TS_BF_TIMER);TS_BF_TIMER=null;}
        if(refreshOnFinish&&!TS_BF_REFRESHED){TS_BF_REFRESHED=true;loadTianyiSubs();}
      }
    }).catch(function(){});
  };
  tick();
  TS_BF_TIMER=setInterval(tick,2000);
}
loadHome();
loadRelayFlag();
</script>
</body></html>`
	return page
}

// loginPageHTML 登录页：登录一次发 30 天 Cookie，浏览器不用反复输密码。
// defUser 非空 = 密码仍是出厂默认值，此时在登录页明示默认账号密码——
// 否则第一次用的人根本猜不到账号密码（用户实测踩坑）。密码一旦改过，
// 调用方传空串，页面上什么提示都不出现，不泄露任何信息。
func loginPageHTML(defUser string) string {
	hint := ""
	if defUser != "" {
		hint = `<div class="dft"><b>首次使用</b>：默认账号 <b>` + defUser + `</b> · 默认密码 <b>admin</b><br>登录后请用启动参数 <code>-pass 你的密码</code> 改掉，别一直用默认的</div>`
	}
	return `<!doctype html>
<html lang="zh-CN"><head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1,viewport-fit=cover">
<title>登录 · BigPan</title>
<link rel="icon" href="data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 24 24' fill='none' stroke='%232080f0' stroke-width='2' stroke-linecap='round' stroke-linejoin='round'%3E%3Cpath d='M5 17h14l-1.5-6.5a2 2 0 0 0-2-1.5h-7a2 2 0 0 0-2 1.5Z'/%3E%3Cpath d='M12 9V4m0 0 3 3m-3-3L9 7'/%3E%3C/svg%3E">
<style>
:root{--bg:#f5f6f8;--card:#fff;--line:#e7e9ee;--txt:#1f2329;--mut:#767d8c;--pri:#2080f0;--pri-h:#1a6dd8;--warn:#f0a020}
*{box-sizing:border-box;margin:0;padding:0;-webkit-tap-highlight-color:transparent}
body{font-family:-apple-system,BlinkMacSystemFont,"PingFang SC","HarmonyOS Sans SC","Microsoft YaHei",sans-serif;
  background:radial-gradient(1100px 560px at 12% 6%,#e2edff 0%,rgba(226,237,255,0) 60%),
             radial-gradient(900px 520px at 88% 94%,#eceeff 0%,rgba(236,238,255,0) 55%),var(--bg);
  color:var(--txt);font-size:14px;line-height:1.55;display:flex;align-items:center;justify-content:center;min-height:100vh;padding:18px}
.card{background:rgba(255,255,255,.92);backdrop-filter:blur(10px);border:1px solid rgba(231,233,238,.9);border-radius:16px;
  padding:32px 26px 22px;width:100%;max-width:372px;box-shadow:0 12px 40px rgba(16,24,40,.10)}
.brand{display:flex;flex-direction:column;align-items:center;gap:12px;margin-bottom:26px}
.bico{width:48px;height:48px;border-radius:14px;background:linear-gradient(135deg,#2080f0,#5a9cf8);display:flex;align-items:center;justify-content:center;box-shadow:0 4px 16px rgba(32,128,240,.42)}
.bico svg{width:25px;height:25px;stroke:#fff}
.brand b{font-size:19px;font-weight:700;letter-spacing:.2px}
.brand small{color:var(--mut);font-size:13px;margin-top:-6px}
label{display:block;font-size:13px;color:var(--mut);margin:15px 0 6px;font-weight:500}
.ig{position:relative}
.ig>svg{position:absolute;left:12px;top:50%;transform:translateY(-50%);width:15px;height:15px;stroke:#9aa1ad;pointer-events:none}
input{width:100%;padding:11px 13px;border:1px solid #dcdfe6;border-radius:10px;background:#fff;outline:none;font:inherit;transition:border-color .15s,box-shadow .15s;color:var(--txt)}
.ig input{padding-left:38px;padding-right:42px}
input::placeholder{color:#b7bcc6}
input:focus{border-color:var(--pri);box-shadow:0 0 0 3px rgba(32,128,240,.13)}
.eye{position:absolute;right:5px;top:50%;transform:translateY(-50%);background:none;border:0;box-shadow:none;width:auto;margin:0;padding:7px;color:#9aa1ad;cursor:pointer;border-radius:8px;display:flex}
.eye:hover{background:#f2f4f8;color:var(--pri)}
.eye svg{width:16px;height:16px;stroke:currentColor;display:block}
button{width:100%;margin-top:24px;background:var(--pri);color:#fff;border:0;border-radius:10px;padding:12px;cursor:pointer;font-weight:600;font-size:14px;font:inherit;
  transition:background .15s,transform .06s,box-shadow .15s;box-shadow:0 2px 10px rgba(32,128,240,.38);display:flex;align-items:center;justify-content:center;gap:8px}
button:hover{background:var(--pri-h)}
button:active{transform:scale(.98)}
button:disabled{opacity:.55;cursor:default;transform:none}
.spin{display:inline-block;width:14px;height:14px;border:2px solid rgba(255,255,255,.35);border-top-color:#fff;border-radius:50%;animation:rot .7s linear infinite}
@keyframes rot{to{transform:rotate(360deg)}}
.err{color:var(--warn);font-size:13px;margin-top:13px;min-height:16px;text-align:center}
/* 出厂默认账号密码提示：仅在密码仍是默认值时出现（见 loginPageHTML 注释） */
.dft{margin:0 0 2px;padding:10px 13px;background:#fdf6ea;border:1px solid #f3ddb4;border-radius:10px;font-size:13px;line-height:1.7;color:#8a5a12;text-align:left}
.dft b{color:#6f4a0e;font-weight:700}
.dft code{background:#f7ecd8;border-color:#eed9ac;font-size:12px}
@keyframes shake{10%,90%{transform:translateX(-2px)}20%,80%{transform:translateX(3px)}30%,50%,70%{transform:translateX(-4px)}40%,60%{transform:translateX(4px)}}
.shake{animation:shake .4s}
.ft{margin-top:20px;text-align:center;color:#a0a6b2;font-size:12px}
</style></head>
<body>
<div class="card" id="card">
  <div class="brand">
    <span class="bico"><svg viewBox="0 0 24 24" fill="none" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M5 17h14l-1.5-6.5a2 2 0 0 0-2-1.5h-7a2 2 0 0 0-2 1.5Z"/><path d="M12 9V4m0 0 3 3m-3-3L9 7"/></svg></span>
    <b>BigPan</b>
    <small>无限空间，聚合分享！</small>
  </div>` + hint + `<label>用户名</label>
  <div class="ig">
    <svg viewBox="0 0 24 24" fill="none" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="8" r="4"/><path d="M4 21c0-4 3.6-6.5 8-6.5s8 2.5 8 6.5"/></svg>
    <input type="text" id="u" placeholder="用户名" autocomplete="username">
  </div>
  <label>密码</label>
  <div class="ig">
    <svg viewBox="0 0 24 24" fill="none" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="4" y="10.5" width="16" height="10.5" rx="2.5"/><path d="M8 10.5V7a4 4 0 0 1 8 0v3.5"/></svg>
    <input type="password" id="p" placeholder="密码" autocomplete="current-password">
    <button type="button" class="eye" id="eye" onclick="toggleEye()" aria-label="显示或隐藏密码">
      <svg viewBox="0 0 24 24" fill="none" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M2.5 12S6 5.5 12 5.5 21.5 12 21.5 12 18 18.5 12 18.5 2.5 12 2.5 12Z"/><circle cx="12" cy="12" r="3"/></svg>
    </button>
  </div>
  <button id="btn" onclick="doLogin()"><span class="spin" id="lg" style="display:none"></span><span id="lgTxt">登 录</span></button>
  <div class="err" id="err"></div>
  <div class="ft">登录一次，30 天内免输入 · 文件始终在你的网盘里</div>
</div>
<script>
function toggleEye(){var i=document.getElementById('p');i.type=(i.type==='password'?'text':'password');document.getElementById('eye').style.color=(i.type==='text'?'var(--pri)':'');}
function shake(){var c=document.getElementById('card');c.classList.remove('shake');void c.offsetWidth;c.classList.add('shake')}
function doLogin(){
  var u=document.getElementById('u').value.trim(), p=document.getElementById('p').value;
  var e=document.getElementById('err');e.textContent='';
  if(!u||!p){e.textContent='账号密码都要填';shake();return}
  var b=document.getElementById('btn');b.disabled=true;
  document.getElementById('lg').style.display='inline-block';
  document.getElementById('lgTxt').textContent='登录中…';
  fetch('/login',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({user:u,pass:p})})
  .then(function(r){return r.json()}).then(function(d){
    if(d.ok){location.href='/';}
    else{
      e.textContent=d.error||'登录失败';b.disabled=false;
      document.getElementById('lg').style.display='none';
      document.getElementById('lgTxt').textContent='登 录';shake();
    }
  }).catch(function(){
    e.textContent='网络错误，请稍后重试';b.disabled=false;
    document.getElementById('lg').style.display='none';
    document.getElementById('lgTxt').textContent='登 录';
  });
}
document.getElementById('p').addEventListener('keydown',function(ev){if(ev.key==='Enter')doLogin()});
document.getElementById('u').addEventListener('keydown',function(ev){if(ev.key==='Enter')doLogin()});
</script>
</body></html>`
}
