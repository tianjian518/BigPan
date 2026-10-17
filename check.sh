#!/bin/bash
# ============================================================================
# bigpan 网络自检
#
# 作用：检查这台机器到各家网盘 API 的连通性，判断"扫码登录"能不能用。
#
# 用法：  bash check.sh
#
# 说明：部分网盘对「机房 IP」有风控（云服务器尤其明显），
#       家里宽带、NAS、电视盒子一般不受影响。
#       所以如果你在云服务器上跑不通，拿回家里的设备再跑一次这个脚本。
# ============================================================================

UA="Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
TIMEOUT=10

green() { printf '\033[32m%s\033[0m' "$1"; }
red()   { printf '\033[31m%s\033[0m' "$1"; }
yellow(){ printf '\033[33m%s\033[0m' "$1"; }

check() {
  local name="$1" url="$2" ref="$3"
  local out code t

  if [ -n "$ref" ]; then
    out=$(curl -s -o /dev/null -m $TIMEOUT -w '%{http_code} %{time_total}' \
          -A "$UA" -e "$ref" "$url" 2>/dev/null)
  else
    out=$(curl -s -o /dev/null -m $TIMEOUT -w '%{http_code} %{time_total}' \
          -A "$UA" "$url" 2>/dev/null)
  fi

  code=$(echo "$out" | cut -d' ' -f1)
  t=$(echo "$out" | cut -d' ' -f2)

  if [ -z "$code" ] || [ "$code" = "000" ]; then
    printf '  %-42s %s\n' "$name" "$(red '不通 / 超时')"
    return 1
  fi

  # 时间太长（>5s）也算慢
  slow=$(echo "$t > 5" | bc 2>/dev/null || echo 0)
  if [ "$slow" = "1" ]; then
    printf '  %-42s %s  (%ss)\n' "$name" "$(yellow '能连但很慢')" "$t"
  else
    printf '  %-42s %s  (%ss)\n' "$name" "$(green "OK $code")" "$t"
  fi
  return 0
}

echo
echo "=============================================================="
echo " bigpan 网络自检  —  $(date '+%Y-%m-%d %H:%M:%S')"
echo "=============================================================="
echo

echo "【115 网盘】—— 扫码登录相关"
check "115 二维码接口 (token)"      "https://qrcodeapi.115.com/api/1.0/web/1.0/token/"
check "115 状态查询 (新端点)"       "https://qrcodeapi.115.com/api/1.0/web/1.0/status/?uid=x&time=1&sign=x"
check "115 状态查询 (老端点)"       "https://qrcodeapi.115.com/get/status/?uid=x&time=1&sign=x"
check "115 登录接口"                "https://passportapi.115.com/app/1.0/1/1.0/login/qrcode/"
check "115 主站"                    "https://115.com/"                  "https://115.com/"
echo

echo "【115 网盘】—— 文件操作与下载"
check "115 网页版 API"              "https://webapi.115.com/files"      "https://115.com/"
check "115 proapi"                  "https://proapi.115.com/app/1.0/1/1.0/user/info"
check "115 下载 CDN"                "https://cdnfhnedge.115.com/"
echo

echo "【阿里云盘】"
check "阿里开放平台 (扫码授权)"      "https://openapi.alipan.com/oauth/authorize/qrcode"
check "阿里网页版 API"              "https://api.aliyundrive.com/adrive/v3/share_link/get_share_by_anonymous" "https://www.alipan.com/"
echo

echo "【百度网盘】"
check "百度 passport (扫码)"         "https://passport.baidu.com/v2/api/getqrcode?lp=pc&qrloginfrom=pc"
check "百度网盘主站"                 "https://pan.baidu.com/"
echo

echo "【夸克 / 天翼】"
check "夸克网盘"                     "https://pan.quark.cn/"
check "天翼云盘"                     "https://cloud.189.cn/"
echo

echo "=============================================================="
echo " 怎么看结果"
echo "=============================================================="
echo "  · 全部 OK            → 扫码登录可用，直接在管理界面点「扫码登录」"
echo "  · 115 老端点不通      → 不影响，新版程序已改用新端点"
echo "  · 大量不通            → 这台机器的网络对网盘不友好（机房 IP 常见）"
echo "                          换个网络环境（家里宽带 / NAS）再试"
echo "  · 只有 CDN 不通       → 能登录、能解析，但播不了，需要检查出口"
echo
