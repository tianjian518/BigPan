package main

import "net/http"

// ---------------------------------------------------------------------------
// 浏览器能力自检页 /test
//
// 目的：一次把「到底是浏览器解不开，还是服务端没给到数据」这两件事分开。
// 之前的判断都是靠猜（我这边无头浏览器不支持 HEVC，不代表用户手机不支持），
// 所以做一个页面，让真实设备自己报答案。
// ---------------------------------------------------------------------------

func testPage() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			w.Header().Set("WWW-Authenticate", `Basic realm="BigPan"`)
			http.Error(w, "需要登录", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(testHTML))
	}
}

const testHTML = `<!DOCTYPE html><html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>浏览器能力自检 · BigPan</title>
<style>
body{font-family:-apple-system,"PingFang SC","Microsoft YaHei",sans-serif;background:#0b1220;
 color:#e2e8f0;margin:0;padding:16px;font-size:15px;line-height:1.6}
h2{font-size:16px;margin:18px 0 10px;color:#93c5fd}
.card{background:#111827;border:1px solid #1f2937;border-radius:10px;padding:14px}
table{width:100%;border-collapse:collapse;font-size:14px}
td{padding:7px 4px;border-bottom:1px solid #1f2937}
tr:last-child td{border-bottom:0}
.ok{color:#4ade80;font-weight:600}.no{color:#f87171;font-weight:600}.maybe{color:#fbbf24;font-weight:600}
pre{white-space:pre-wrap;word-break:break-all;font-size:12px;color:#94a3b8;margin:10px 0 0;
 background:#0b1220;padding:10px;border-radius:6px;max-height:260px;overflow:auto}
video{width:100%;background:#000;border-radius:8px;margin-top:12px}
button{background:#2563eb;color:#fff;border:0;border-radius:8px;padding:11px 18px;font-size:15px}
a{color:#38bdf8;font-size:13px}
</style></head><body>
<h2>① 这台设备的浏览器能解哪些格式</h2>
<div class="card"><table id="t"></table><pre id="ua"></pre></div>

<h2>② 实测播一个（用盒子里的第一个文件）</h2>
<div class="card">
  <button onclick="go()">开始试播</button>
  <pre id="log">点按钮开始…</pre>
  <video id="v" controls playsinline></video>
</div>
<div style="margin:6px 0 24px"><a href="/">← 返回管理界面</a></div>
<script>
var probe=document.createElement('video');
var checks=[
 ['MKV 容器','video/x-matroska'],
 ['MKV + H.264','video/x-matroska; codecs="avc1.640028"'],
 ['MP4 + H.264','video/mp4; codecs="avc1.42E01E"'],
 ['MP4 + H.265/HEVC (hvc1)','video/mp4; codecs="hvc1.1.6.L93.B0"'],
 ['MP4 + H.265/HEVC (hev1)','video/mp4; codecs="hev1.1.6.L93.B0"']
];
var html='';
checks.forEach(function(c){
  var r=probe.canPlayType(c[1]);
  var cls = r ? (r==='probably'?'ok':'maybe') : 'no';
  html+='<tr><td>'+c[0]+'</td><td class="'+cls+'">'+(r?r:'不支持')+'</td></tr>';
});
document.getElementById('t').innerHTML=html;
document.getElementById('ua').textContent='UA: '+navigator.userAgent;

var el=document.getElementById('v');
function log(s){var e=document.getElementById('log');e.textContent+= s+'\n';}
var t0=0, firstByte=false;

el.addEventListener('error',function(){
  var c=el.error?el.error.code:0;
  var m={1:'加载被中止',2:'网络错误',3:'解码失败',4:'源不可用/格式不支持'}[c]||('码 '+c);
  log('❌ video 报错 → code='+c+' ('+m+')');
});
el.addEventListener('loadedmetadata',function(){log('✅ 读到元数据，时长 '+el.duration+'s');});
el.addEventListener('progress',function(){
  if(!firstByte){firstByte=true;log('⏱ 首批数据到达：'+(Date.now()-t0)+' ms');}
});
el.addEventListener('playing',function(){log('▶ 开始播放');});

function go(){
  document.getElementById('log').textContent='';
  firstByte=false;
  fetch('/api/items',{credentials:'include'})
   .then(function(r){return r.json();})
   .then(function(d){
      var it=(d.items||[])[0];
      if(!it){log('没有条目');return;}
      log('文件: '+it.title);
      log('扩展名: '+(it.ext||'(无，默认 .mp4)'));
      log('大小: '+((it.size||0)/1048576).toFixed(1)+' MB');
      var src='/play?id='+it.id;
      log('地址: '+location.origin+src);
      t0=Date.now();
      el.src=src; el.load();
      var p=el.play();
      if(p&&p.catch){p.catch(function(e){log('play() 被拒绝: '+e.name+' — '+e.message);});}
      setTimeout(function(){
        log('— 12 秒后 —');
        log('readyState='+el.readyState+' (0没数据 1元数据 2当前帧 3可播 4流畅)');
        log('networkState='+el.networkState+' (0空 1空闲 2下载中 3无源)');
        log('已缓冲='+(el.buffered.length?el.buffered.end(0).toFixed(2):'0')+' 秒');
        log('已播='+el.currentTime.toFixed(2)+' 秒');
      },12000);
   })
   .catch(function(e){log('取条目失败: '+e);});
}
</script></body></html>`
