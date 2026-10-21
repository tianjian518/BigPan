// Package tianyi 是天翼云盘（cloud.189.cn）接入层。
//
// client/ 子目录来自开源项目 github.com/xxcheng123/cloudpan189-interface
// （MIT License，见同目录 LICENSE），原样搬运未改一行——日后要跟上游同步，
// 直接整目录覆盖即可。搬运时仅依赖 resty.dev/v3、go-querystring、uuid、
// pkg/errors 四个库，无平台相关代码。
//
// 本包（bigpan/internal/tianyi）只放「适配 BigPan 的薄封装」：
// token 管理、分享树扫描、取链（不跟随重定向）、订阅号拉取。
package tianyi
