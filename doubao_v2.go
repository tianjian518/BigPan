package main

// ---------------------------------------------------------------------------
// 豆包新链路（feishu drive API + DPoP）
//
// 老链路（/samantha/aispace/*）只认 sessionid，30 天硬限制且无法续期。
// 新链路走 my.feishu.cn，鉴权用 Authorization + DPoP：
//
//	DPoP proof = ES256 JWT（P-256 私钥，15 秒有效，每个请求签一次）
//	token      = Cookie 里的 LARK_SUITE_ACCESS_TOKEN，可随时用 DPoP 调
//	             /passport/user/biz_auth/ 换新的
//
// feishu_dpop_keypair 是加密的 P-256 私钥，口令是飞书前端硬编码的
// "passport-dpop-token-generator"。keypair 不过期 → token 随时能换 →
// 这就是"配一次长期用"的根据。
//
// 参考：OpenList drivers/doubao_new 与 tianjian518/139cas 的修复版。
// ---------------------------------------------------------------------------

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	dbnBase     = "https://my.feishu.cn"
	dbnDownload = "https://internal-api-drive-stream.feishu.cn"
	dbnDpopSalt = "fixed-salt"
	dbnSecret   = "passport-dpop-token-generator"
	dbnClientID = "cli_a872ee858eae100e"
	dbnBizAuth  = "https://www.doubao.com/passport/user/biz_auth/"
)

// ---- PBKDF2-HMAC-SHA256（标准库没有，自己实现，这里只需 1 块 32 字节）----

func pbkdf2Sha256(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	hashLen := prf.Size()
	numBlocks := (keyLen + hashLen - 1) / hashLen
	out := make([]byte, 0, numBlocks*hashLen)
	buf := make([]byte, 4)
	for block := 1; block <= numBlocks; block++ {
		prf.Reset()
		binary.BigEndian.PutUint32(buf, uint32(block))
		prf.Write(salt)
		prf.Write(buf)
		u := prf.Sum(nil)
		t := make([]byte, len(u))
		copy(t, u)
		for i := 1; i < iter; i++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(nil)
			for j := range t {
				t[j] ^= u[j]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}

func b64urlEncode(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
func b64urlDecode(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

// ---- keypair 解密 ----

type dbnJWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
	D   string `json:"d"`
}

// decryptDPoPKeyPair 解出加密的 P-256 私钥，返回可直接签名的 key。
func decryptDPoPKeyPair(raw, secret string) (*ecdsa.PrivateKey, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("keypair 为空")
	}
	ciphertext := raw
	if strings.HasPrefix(raw, "{") {
		var env struct {
			Data       string `json:"data"`
			Ciphertext string `json:"ciphertext"`
			Encrypted  string `json:"encrypted"`
		}
		if err := json.Unmarshal([]byte(raw), &env); err != nil {
			return nil, err
		}
		switch {
		case env.Data != "":
			ciphertext = env.Data
		case env.Ciphertext != "":
			ciphertext = env.Ciphertext
		case env.Encrypted != "":
			ciphertext = env.Encrypted
		default:
			return nil, fmt.Errorf("keypair 里没有密文字段")
		}
	}
	ct, err := base64.StdEncoding.DecodeString(strings.TrimSpace(ciphertext))
	if err != nil {
		ct, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(strings.TrimSpace(ciphertext), "="))
		if err != nil {
			return nil, fmt.Errorf("keypair base64 解码失败：%w", err)
		}
	}
	if len(ct) <= 12 {
		return nil, fmt.Errorf("keypair 密文过短")
	}
	key := pbkdf2Sha256([]byte(secret), []byte(dbnDpopSalt), 100000, 32)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	plain, err := aead.Open(nil, ct[:12], ct[12:], nil)
	if err != nil {
		return nil, fmt.Errorf("解密失败（口令不匹配）：%w", err)
	}
	var env2 struct {
		PrivateKey *dbnJWK `json:"privateKey"`
		KeyPair    *dbnJWK `json:"keyPair"`
		JWK        *dbnJWK `json:"jwk"`
	}
	var jwk dbnJWK
	if err := json.Unmarshal(plain, &env2); err == nil {
		switch {
		case env2.PrivateKey != nil:
			jwk = *env2.PrivateKey
		case env2.KeyPair != nil:
			jwk = *env2.KeyPair
		case env2.JWK != nil:
			jwk = *env2.JWK
		}
	} else {
		if err := json.Unmarshal(plain, &jwk); err != nil {
			return nil, fmt.Errorf("keypair 明文不是 JWK：%w", err)
		}
	}
	if jwk.D == "" || jwk.X == "" || jwk.Y == "" {
		return nil, fmt.Errorf("JWK 缺 x/y/d")
	}
	dB, err := b64urlDecode(jwk.D)
	if err != nil {
		return nil, err
	}
	d := new(big.Int).SetBytes(dB)
	sk := new(ecdsa.PrivateKey)
	sk.PublicKey.Curve = elliptic.P256()
	sk.D = d
	sk.PublicKey.X, sk.PublicKey.Y = sk.PublicKey.Curve.ScalarBaseMult(dB)
	if !sk.PublicKey.Curve.IsOnCurve(sk.PublicKey.X, sk.PublicKey.Y) {
		return nil, fmt.Errorf("keypair 公钥不在 P-256 曲线上")
	}
	_ = jwk.X
	_ = jwk.Y
	return sk, nil
}

// ---- DPoP proof ----

func (d *Doubao) dbnSignDPoP(method, rawURL string) (string, error) {
	if d.v2Key == nil {
		return "", fmt.Errorf("DPoP 私钥未就绪")
	}
	if i := strings.Index(rawURL, "?"); i >= 0 {
		rawURL = rawURL[:i] // htu 不含 query
	}
	now := time.Now().Unix()
	hdr := map[string]any{
		"typ": "dpop+jwt",
		"alg": "ES256",
		"jwk": map[string]string{
			"kty": "EC", "crv": "P-256",
			"x": b64urlEncode(d.v2Key.PublicKey.X.Bytes()),
			"y": b64urlEncode(d.v2Key.PublicKey.Y.Bytes()),
		},
	}
	pl := map[string]any{
		"jti":   dbnUUID(),
		"htm":   strings.ToUpper(method),
		"htu":   rawURL,
		"iat":   now,
		"nonce": dbnUUID(),
		"exp":   now + 15,
	}
	si := b64urlEncode(dbnJSON(hdr)) + "." + b64urlEncode(dbnJSON(pl))
	hash := sha256.Sum256([]byte(si))
	r, s, err := ecdsa.Sign(rand.Reader, d.v2Key, hash[:])
	if err != nil {
		return "", err
	}
	rs := make([]byte, 64)
	r.FillBytes(rs[:32])
	s.FillBytes(rs[32:])
	return si + "." + b64urlEncode(rs), nil
}

func dbnJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func dbnUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hexEnc(b)
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[:8], h[8:12], h[12:16], h[16:20], h[20:])
}

func hexEnc(b []byte) string {
	const hexc = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[i*2] = hexc[v>>4]
		out[i*2+1] = hexc[v&0x0f]
	}
	return string(out)
}

// ---- token 刷新（biz_auth）----

// dbnRefreshAuthorization 用 DPoP 走 biz_auth 换一个新 token。
func (d *Doubao) dbnRefreshAuthorization() error {
	d.v2Mu.Lock()
	defer d.v2Mu.Unlock()
	HTU := dbnBizAuth
	proof, err := d.dbnSignDPoP(http.MethodPost, HTU)
	if err != nil {
		return err
	}
	form := url.Values{}
	form.Set("client_id", dbnClientID)
	form.Set("client_type", "Lark")
	form.Set("scope", "internal")
	form.Set("d_pop", proof)
	u := HTU + "?aid=497858&account_sdk_source=web&sdk_version=2.1.10"
	req, err := http.NewRequest(http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("accept", "application/json, text/javascript")
	req.Header.Set("origin", "https://www.doubao.com")
	req.Header.Set("referer", "https://www.doubao.com/")
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	req.Header.Set("user-agent", doubaoUA)
	req.Header.Set("cookie", d.cookie)
	req.Header.Set("x-tt-passport-csrf-token", d.v2CSRF)
	req.Header.Set("authorization", d.v2Token)
	req.Header.Set("dpop", proof)
	resp, err := d.hc.Do(req)
	if err != nil {
		return err
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var out struct {
		Message string `json:"message"`
		Data    struct {
			AccessToken string `json:"access_token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Message != "success" || out.Data.AccessToken == "" {
		return fmt.Errorf("biz_auth 刷新失败：%s", dbSnippet(raw))
	}
	d.v2Token = out.Data.AccessToken
	d.v2TokenAt = time.Now()
	log.Printf("[豆包v2] token 已通过 DPoP 刷新")
	return nil
}

// dbnEnsureToken 保证 token 可用：过期前 3 天自动刷新。
func (d *Doubao) dbnEnsureToken() (string, error) {
	d.v2Mu.Lock()
	tok := d.v2Token
	at := d.v2TokenAt
	d.v2Mu.Unlock()
	if tok == "" {
		return "", fmt.Errorf("v2 token 未初始化")
	}
	if !at.IsZero() && time.Since(at) < 4*24*time.Hour {
		return tok, nil
	}
	if err := d.dbnRefreshAuthorization(); err != nil {
		// 刷新失败但旧 token 可能还活着，先用旧的
		if tok != "" {
			log.Printf("[豆包v2] 刷新失败，继续用旧 token：%v", err)
			return tok, nil
		}
		return "", err
	}
	d.v2Mu.Lock()
	tok = d.v2Token
	d.v2Mu.Unlock()
	return tok, nil
}

// ---- 列目录（feishu drive）----

type dbnNode struct {
	Token     string `json:"token"`
	NodeToken string `json:"node_token"`
	ObjToken  string `json:"obj_token"`
	Name      string `json:"name"`
	Type      int    `json:"type"` // 0=文件夹
	NodeType  int    `json:"node_type"`
	Extra     struct {
		Size string `json:"size"`
	} `json:"extra"`
}

type dbnListData struct {
	HasMore   bool     `json:"has_more"`
	LastLabel string   `json:"last_label"`
	NodeList  []string `json:"node_list"`
	Entities  struct {
		Nodes map[string]dbnNode `json:"nodes"`
	} `json:"entities"`
}

// dbnListChildren 列一个目录，自动翻页（last_label）。
// parentToken 为空 = 根目录。
func (d *Doubao) dbnListChildren(parentToken string) ([]dbnNode, error) {
	var all []dbnNode
	last := ""
	for page := 0; page < 200; page++ {
		tok, err := d.dbnEnsureToken()
		if err != nil {
			return nil, err
		}
		htu := dbnBase + "/space/api/explorer/doubao/children/list/"
		q := url.Values{}
		// defaultObjTypes（OpenList doubao_new types.go）：0=文件夹 12/22/30/123/124=各类文件
		for _, t := range []string{"124", "0", "12", "30", "123", "22"} {
			q.Add("obj_type", t)
		}
		q.Set("length", "50")
		q.Set("rank", "0")
		q.Set("asc", "0")
		q.Set("min_length", "40")
		q.Set("thumbnail_width", "1028")
		q.Set("thumbnail_height", "1028")
		q.Set("thumbnail_policy", "4")
		if parentToken != "" {
			q.Set("token", parentToken)
		}
		if last != "" {
			q.Set("last_label", last)
		}
		full := htu + "?" + q.Encode()
		proof, err := d.dbnSignDPoP(http.MethodGet, htu)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequest(http.MethodGet, full, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("accept", "*/*")
		req.Header.Set("origin", "https://www.doubao.com")
		req.Header.Set("referer", "https://www.doubao.com/")
		req.Header.Set("user-agent", doubaoUA)
		req.Header.Set("cookie", d.cookie) // 新接口实测必须带 Cookie
		req.Header.Set("authorization", "Bearer "+tok)
		req.Header.Set("dpop", proof)
		resp, err := d.hc.Do(req)
		if err != nil {
			return nil, err
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var out struct {
			Code int         `json:"code"`
			Msg  string      `json:"msg"`
			Data dbnListData `json:"data"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("feishu 列目录返回异常：%s", dbSnippet(raw))
		}
		if out.Code != 0 {
			return nil, fmt.Errorf("feishu 列目录错误 %d：%s", out.Code, out.Msg)
		}
		got := out.Data
		added := 0
		if os.Getenv("DBN_DEBUG") != "" {
			log.Printf("[v2调试] page=%d nodelist=%d nodes=%d has_more=%v last=%q",
				page, len(got.NodeList), len(got.Entities.Nodes), got.HasMore, got.LastLabel)
		}
		if len(got.NodeList) > 0 {
			for _, tk := range got.NodeList {
				if n, ok := got.Entities.Nodes[tk]; ok {
					all = append(all, n)
					added++
				}
			}
		} else {
			for _, n := range got.Entities.Nodes {
				all = append(all, n)
				added++
			}
		}
		if !got.HasMore || got.LastLabel == "" || got.LastLabel == last {
			break
		}
		last = got.LastLabel
		if added == 0 {
			break
		}
	}
	return all, nil
}

// dbnToOldNode 把新链路节点映射成老的 dbNode，下游扫描/STRM/UI 全部复用。
func dbnToOldNode(n dbnNode) dbNode {
	isDir := n.Type == 0
	size, _ := strconv.ParseInt(n.Extra.Size, 10, 64)
	nt := 2
	if isDir {
		nt = 1
	}
	return dbNode{ID: n.NodeToken, Name: n.Name, NodeType: nt, Size: size, Key: n.ObjToken}
}

// dbnNodeName 取一个节点（含别人分享的根）的名字。
// 浏览器分享页第二跳 GET /space/api/explorer/v2/obj/path/?obj_token=xx&need_path=true
// 返回 data.entities.nodes[token].name；失败不影响主流程。
func (d *Doubao) dbnNodeName(token string) (string, error) {
	tok, err := d.dbnEnsureToken()
	if err != nil {
		return "", err
	}
	htu := dbnBase + "/space/api/explorer/v2/obj/path/"
	proof, err := d.dbnSignDPoP(http.MethodGet, htu)
	if err != nil {
		return "", err
	}
	u := htu + "?obj_token=" + url.QueryEscape(token) + "&obj_type=0&need_path=true"
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("accept", "application/json, text/plain, */*")
	req.Header.Set("origin", "https://www.doubao.com")
	req.Header.Set("referer", "https://www.doubao.com/")
	req.Header.Set("user-agent", doubaoUA)
	req.Header.Set("cookie", d.cookie)
	req.Header.Set("authorization", "Bearer "+tok)
	req.Header.Set("dpop", proof)
	resp, err := d.hc.Do(req)
	if err != nil {
		return "", err
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(bytes.TrimSpace(raw)) == 0 {
		return "", fmt.Errorf("obj/path HTTP %d", resp.StatusCode)
	}
	var out struct {
		Code int `json:"code"`
		Data struct {
			Entities struct {
				Nodes map[string]struct {
					Name string `json:"name"`
				} `json:"nodes"`
			} `json:"entities"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", err
	}
	if out.Code != 0 {
		return "", fmt.Errorf("obj/path code %d", out.Code)
	}
	if n, ok := out.Data.Entities.Nodes[token]; ok {
		return strings.TrimSpace(n.Name), nil
	}
	return "", fmt.Errorf("obj/path 无此节点")
}

type dbnFileInfo struct {
	Version string `json:"version"`
}

// dbnFileInfo 取文件信息（OpenList previewLink 前会调它，可能兼有激活作用）。
func (d *Doubao) dbnFileInfo(fileToken string) (dbnFileInfo, error) {
	var out dbnFileInfo
	tok, err := d.dbnEnsureToken()
	if err != nil {
		return out, err
	}
	htu := dbnBase + "/space/api/box/file/info/"
	proof, err := d.dbnSignDPoP(http.MethodPost, htu)
	if err != nil {
		return out, err
	}
	body, _ := json.Marshal(map[string]any{
		"caller": "explorer", "file_token": fileToken,
		"mount_point": "explorer", "option_params": []string{"preview_meta", "check_cipher"},
	})
	req, err := http.NewRequest(http.MethodPost, htu, bytes.NewReader(body))
	if err != nil {
		return out, err
	}
	req.Header.Set("accept", "*/*")
	req.Header.Set("origin", "https://www.doubao.com")
	req.Header.Set("referer", "https://www.doubao.com/")
	req.Header.Set("user-agent", doubaoUA)
	req.Header.Set("cookie", d.cookie)
	req.Header.Set("authorization", "Bearer "+tok)
	req.Header.Set("dpop", proof)
	req.Header.Set("content-type", "application/json")
	resp, err := d.hc.Do(req)
	if err != nil {
		return out, err
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var w struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Version string `json:"version"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &w); err != nil {
		return out, err
	}
	if w.Code != 0 {
		return out, fmt.Errorf("file/info %d：%s", w.Code, w.Msg)
	}
	return w.Data, nil
}

// dbnDownloadURL 拼播放直链：鉴权放在 query 里，302 后播放器无需任何特殊头。
//
// 两个缺一不可的细节（实测）：
//  1. token 必须是 biz_auth 刷新后的（Cookie 里的原始 LARK token 放 query 是 401）
//  2. 值必须带 "DPoP " 前缀（裸 token 和 "Bearer " 前缀都是 401，
//     对应 OpenList resolveAuthorization() 的返回格式 "DPoP "+token）
func (d *Doubao) dbnDownloadURL(objToken string) (string, error) {
	tok, err := d.dbnEnsureToken()
	if err != nil {
		return "", err
	}
	proof, err := d.dbnSignDPoP(http.MethodGet, dbnDownload+"/space/api/box/stream/download/all/"+objToken+"/")
	if err != nil {
		return "", err
	}
	q := url.Values{}
	q.Set("authorization", "DPoP "+tok)
	q.Set("dpop", proof)
	return dbnDownload + "/space/api/box/stream/download/all/" + objToken + "/?" + q.Encode(), nil
}
