package client

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// CheckAccessCodeResponse checkAccessCode 的响应。
// 成功：{"res_code":0,"res_message":"成功","shareId":12325116478799}
// 访问码错误：HTTP 200 + XML <successResp><success>false</success></successResp>
// 缺 uuid：空 body
type CheckAccessCodeResponse struct {
	ResCode    int    `json:"res_code"`
	ResMessage string `json:"res_message"`
	ShareId    int64  `json:"shareId"`
}

// CheckAccessCode 校验分享访问码并取回 shareId。
//
// 背景（2026-10 实测）：天翼已经不再在 getShareInfoByCode 的响应里返回
// shareId 了，但 H5 分享页的 checkAccessCode.action 仍然会返回。
// 三个硬性条件，缺一不可：
//  1. 参数必须走 query/form（JSON body 会 InvalidArgument）；
//  2. 必须带 uuid（缺了返回空 body）；
//  3. 接口挂在 api.cloud.189.cn 上即可匿名调用，不需要 token、不需要签名。
//
// 访问码错误时天翼返回 XML 而非 JSON，这里统一转成可读错误。
func (c *client) CheckAccessCode(ctx context.Context, shareCode, accessCode string) (int64, error) {
	resp, err := c.getHttpRequest(ctx).
		SetQueryParams(map[string]string{
			"shareCode":  shareCode,
			"accessCode": accessCode,
			"uuid":       uuid.NewString(),
		}).
		Get("/open/share/checkAccessCode.action")
	if err != nil {
		return 0, err
	}

	body := resp.Bytes()
	var out CheckAccessCodeResponse
	if json.Unmarshal(body, &out) == nil && out.ShareId > 0 {
		return out.ShareId, nil
	}

	raw := strings.TrimSpace(string(body))
	if raw == "" {
		return 0, fmt.Errorf("访问码校验失败（响应为空）")
	}
	if strings.Contains(raw, "<success>false</success>") {
		return 0, fmt.Errorf("访问码错误，请检查后重试")
	}
	return 0, fmt.Errorf("访问码校验失败：%s", raw)
}
