// login.go — WorkBuddy CN OAuth 登录（设备授权流程，CN realm only）。
//
// 两个子命令，由 login.sh 顺序驱动：
//
//	login url  [cn|global] [sessionId] → POST /v2/plugin/auth/state?platform=CLI
//	              拿 state+authUrl，state 落盘，stdout 打印授权 URL
//	login poll [sessionId]             → 读 state，GET /v2/plugin/auth/token?state= 一次，
//	              成功再 GET /v2/plugin/login/account?state= 拿 uid/nickname，
//	              stdout 打印完整 token+account JSON
//
// 无 PKCE（workbuddy 设备流由服务端签发 state）。
//
// sessionId 可选，由调用方（管理面板）生成，用来把并发的登录会话隔离开：
// 不带时 state 落固定的 stateFile，带时落 wb2api-login-<sid>.json。
// 面板与 login.sh 因此天然各用各的文件，不会再互相覆盖 state。
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// upstreamBaseCN/Global 两区上游 base；区域由 url 子命令的参数选择，
// 并随 state 文件传给 poll（登录流程跨进程，无法用内存传递）。
const (
	upstreamBaseCN     = "https://copilot.tencent.com"
	upstreamBaseGlobal = "https://www.workbuddy.ai"
	clientUA           = "CLI/2.63.2 CodeBuddy/2.63.2"
	originReferer      = "https://www.codebuddy.cn"
	originRefererGlob  = "https://www.workbuddy.ai"
	stateFile          = "/tmp/wb2api-login-state.json"
)

// sessionIDRE 合法 sessionId 的形状：纯字母数字，长度受限。
// 它会被拼进文件名，故必须挡住路径分隔符与 .. —— 与 internal/admin 侧同一条规则。
var sessionIDRE = regexp.MustCompile(`^[A-Za-z0-9]{1,64}$`)

// statePath 按 sessionId 取 state 文件路径。
// 空 sid 沿用固定的 stateFile（login.sh 与手工调用的老路径，行为不变）；
// 非空则落到同目录的 wb2api-login-<sid>.json，使并发登录互不覆盖。
func statePath(sid string) string {
	if sid == "" {
		return stateFile
	}
	return filepath.Join(filepath.Dir(stateFile), "wb2api-login-"+sid+".json")
}

// parseSessionID 取可选的位置参数并校验；未提供时返回空串。
func parseSessionID(args []string, idx int) (string, error) {
	if len(args) <= idx {
		return "", nil
	}
	sid := args[idx]
	if !sessionIDRE.MatchString(sid) {
		return "", fmt.Errorf("invalid sessionId %q", sid)
	}
	return sid, nil
}

// regionEndpoints 一个区域的上游 base 与 Origin/Referer。
type regionEndpoints struct {
	base   string
	origin string
}

// regionEndpoint 按区域名取上游端点；仅接受 "cn"/"global"。
func regionEndpoint(region string) (regionEndpoints, error) {
	switch region {
	case "cn", "":
		return regionEndpoints{base: upstreamBaseCN, origin: originReferer}, nil
	case "global":
		return regionEndpoints{base: upstreamBaseGlobal, origin: originRefererGlob}, nil
	default:
		return regionEndpoints{}, fmt.Errorf("unknown region %q (want cn|global)", region)
	}
}

func (e regionEndpoints) authStateURL() string { return e.base + "/v2/plugin/auth/state?platform=CLI" }
func (e regionEndpoints) loginAcctURL() string { return e.base + "/v2/plugin/login/account?state=" }
func (e regionEndpoints) authTokenURL() string { return e.base + "/v2/plugin/auth/token?state=" }

// commonHeaders 通用请求头（Origin/Referer 按区域取值）。
func commonHeaders(req *http.Request, e regionEndpoints) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", e.origin)
	req.Header.Set("Referer", e.origin+"/")
	req.Header.Set("User-Agent", clientUA)
}

// apiEnvelope 与 main.go:429-433 一致
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// doJSON 与 oauth.go:33-66 一致：{code,msg,data} 信封，code!=0 → error
// headers 为 nil 时用该区域的 commonHeaders 兜底。
func doJSON(client *http.Client, method, fullURL string, headers func(*http.Request), body io.Reader) (json.RawMessage, int, error) {
	req, err := http.NewRequest(method, fullURL, body)
	if err != nil {
		return nil, 0, err
	}
	if headers != nil {
		headers(req)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream %d", resp.StatusCode)
	}
	if resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream redirect %d", resp.StatusCode)
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("parse failed: %w", err)
	}
	if env.Code != 0 {
		return nil, resp.StatusCode, fmt.Errorf("code=%d msg=%s", env.Code, env.Msg)
	}
	return env.Data, resp.StatusCode, nil
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "login: "+format+"\n", args...)
	os.Exit(1)
}

// ---------------------------------------------------------------------------
// 账号激活（仅 global）
//
// 背景：登录只拿到 token，账号仍是「未激活」状态，chat 会回 429 code=14017
// （"The trial version is not yet activated"）。网页登录会多做三步，网关不做。
// 2026-09-13 用 peter-knee 实测跑通，14017 → 200：
//
//  1. POST /console/login/account        提交注册地（areaInfoComplete 翻 true）
//  2. GET  /auth/realms/copilot/overseas/user/register?userId=   registerCloud
//  3. POST /billing/ide/trial            试用激活（翻转 14017 的那一步）
//
// 顺序不可颠倒：跳过 1 直接调 2 会回 {"code":500,"msg":"register failed:register region required"}。
//
// 只对 global 做：CN 的同名路径语义不同 —— get-user-area-info 返回 WAF 拦截 HTML
// 而非 JSON，billing/ide/trial 对已激活账号回 code=14051 "has applied trial"。
// 没有 CN 未激活账号可供实测，故不猜，直接跳过。
//
// 失败一律不阻断登录：token 已经拿到，激活是尽力而为；未激活的账号仍能被导入网关，
// 只是 chat 会 14017。用 stderr 说明原因，避免静默。
func activateAccount(client *http.Client, ep regionEndpoints, accessToken, uid, region string) {
	if region != "global" {
		return
	}
	if uid == "" {
		fmt.Fprintln(os.Stderr, "login: 跳过激活（未取到 uid）")
		return
	}
	authHeaders := func(r *http.Request) {
		commonHeaders(r, ep)
		r.Header.Set("Authorization", "Bearer "+accessToken)
		r.Header.Set("X-User-Id", uid)
		r.Header.Set("X-No-Enterprise-Id", "1")
		r.Header.Set("X-Domain", ep.origin[len("https://"):])
		r.Header.Set("X-Product", "SaaS")
	}

	// 1. 取检测到的注册地（IOS2）。data 是嵌套的 JSON 字符串，要解两层。
	//    取不到就退回 SG —— intl 账号的常见归属，且第 2 步只要求「有个区域」。
	countryName, countryCode, countryFullName := "SG", "65", "Singapore"
	areaRaw, _, err := doJSON(client, http.MethodPost, ep.base+"/billing/area/get-user-area-info",
		authHeaders, bytes.NewReader([]byte(`{"action":"getUserAreaInfo"}`)))
	if err != nil {
		fmt.Fprintf(os.Stderr, "login: 激活：查询注册地失败(%v)，按默认 SG 提交\n", err)
	} else {
		var outer string
		if json.Unmarshal(areaRaw, &outer) == nil {
			var inner struct {
				Data struct {
					IOS2   string `json:"IOS2"`
					Name   string `json:"name"`
					EnName string `json:"enName"`
					Code   string `json:"code"`
				} `json:"data"`
			}
			if json.Unmarshal([]byte(outer), &inner) == nil && inner.Data.IOS2 != "" {
				countryName = inner.Data.IOS2
				if inner.Data.EnName != "" {
					countryFullName = inner.Data.EnName
				}
				if inner.Data.Code != "" {
					countryCode = inner.Data.Code
				}
			}
		}
	}

	// 2. 提交注册地。attributes 的值都是数组（与网页一致）。
	body, _ := json.Marshal(map[string]any{
		"attributes": map[string]any{
			"countryCode":     []string{countryCode},
			"countryFullName": []string{countryFullName},
			"countryName":     []string{countryName},
		},
	})
	if _, _, err := doJSON(client, http.MethodPost, ep.base+"/console/login/account",
		authHeaders, bytes.NewReader(body)); err != nil {
		fmt.Fprintf(os.Stderr, "login: 激活：提交注册地失败(%v)\n", err)
		return
	}

	// 3. registerCloud。注意它成功时返回 code=200 而非 0，会被 doJSON 当错误 —— 故只记不报。
	if _, _, err := doJSON(client, http.MethodGet,
		ep.base+"/auth/realms/copilot/overseas/user/register?userId="+uid,
		authHeaders, nil); err != nil {
		fmt.Fprintf(os.Stderr, "login: 激活：registerCloud 返回非 0（%v），继续尝试 trial\n", err)
	}

	// 4. trial。14051「has applied trial」是已领过的正常结果，不算失败。
	if _, _, err := doJSON(client, http.MethodPost, ep.base+"/billing/ide/trial",
		authHeaders, bytes.NewReader([]byte("{}"))); err != nil {
		if strings.Contains(err.Error(), "14051") {
			fmt.Fprintln(os.Stderr, "login: 激活：该账号已领过试用（14051），无需重复")
			return
		}
		fmt.Fprintf(os.Stderr, "login: 激活：trial 失败(%v)\n", err)
		return
	}
	fmt.Fprintf(os.Stderr, "login: 激活完成（注册地 %s）\n", countryName)
}

type loginState struct {
	State  string `json:"state"`
	Region string `json:"region"` // "cn"（缺省）或 "global"；poll 据此选上游 base
}

func main() {
	if len(os.Args) < 2 {
		fatal("usage: login <url [cn|global] [sessionId] | poll [sessionId]>")
	}
	// 每个流程独立 cookie jar（oauth.go:22-29：多账号登录互不串会话）
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Timeout: 30 * time.Second, Jar: jar}

	switch os.Args[1] {
	case "url":
		region := ""
		if len(os.Args) >= 3 {
			region = strings.ToLower(os.Args[2])
		}
		sid, err := parseSessionID(os.Args, 3)
		if err != nil {
			fatal("%v", err)
		}
		ep, err := regionEndpoint(region)
		if err != nil {
			fatal("%v", err)
		}
		// handleStartLogin (oauth.go:68-87)
		data, _, err := doJSON(client, http.MethodPost, ep.authStateURL(),
			func(r *http.Request) { commonHeaders(r, ep) }, bytes.NewReader([]byte("{}")))
		if err != nil {
			fatal("auth state failed: %v", err)
		}
		var st struct {
			State   string `json:"state"`
			AuthURL string `json:"authUrl"`
		}
		if err := json.Unmarshal(data, &st); err != nil || st.State == "" || st.AuthURL == "" {
			fatal("auth state: missing state or authUrl")
		}
		raw, _ := json.Marshal(loginState{State: st.State, Region: region})
		if err := os.WriteFile(statePath(sid), raw, 0o600); err != nil {
			fatal("write state: %v", err)
		}
		fmt.Println(st.AuthURL)

	case "poll":
		sid, err := parseSessionID(os.Args, 2)
		if err != nil {
			fatal("%v", err)
		}
		raw, err := os.ReadFile(statePath(sid))
		if err != nil {
			fatal("read state: %v (先跑 login url)", err)
		}
		var ls loginState
		if err := json.Unmarshal(raw, &ls); err != nil {
			fatal("parse state: %v", err)
		}
		ep, err := regionEndpoint(ls.Region)
		if err != nil {
			fatal("%v", err)
		}
		// handlePollLogin (oauth.go:108-162)：auth/token 是权威登录状态端点，
		// pending 时业务 code 非 0（"login ing"），完成时 code=0 + token bundle
		tokRaw, status, errTok := doJSON(client, http.MethodGet, ep.authTokenURL()+ls.State,
			func(r *http.Request) { commonHeaders(r, ep) }, nil)
		if errTok != nil {
			if status == 0 || status >= 500 {
				fatal("token endpoint error: %v", errTok)
			}
			fatal("登录未完成（waiting for login）。请确认已在浏览器完成登录再按 y")
		}
		var tok struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresIn    int64  `json:"expiresIn"`
			Domain       string `json:"domain"`
		}
		if err := json.Unmarshal(tokRaw, &tok); err != nil || tok.AccessToken == "" {
			fatal("登录未完成（waiting for login）。请确认已在浏览器完成登录再按 y")
		}
		// login/account 拿 uid/nickname（带 Bearer）
		var acct struct {
			UID          string `json:"uid"`
			EnterpriseID string `json:"enterpriseId"`
			Nickname     string `json:"nickname"`
		}
		acctHeaders := func(r *http.Request) {
			commonHeaders(r, ep)
			r.Header.Set("Authorization", "Bearer "+tok.AccessToken)
		}
		if acctRaw, _, errAcct := doJSON(client, http.MethodGet, ep.loginAcctURL()+ls.State, acctHeaders, nil); errAcct == nil {
			_ = json.Unmarshal(acctRaw, &acct)
		}
		// 拿到 uid 后补激活（仅 global）。失败不阻断：token 已到手，凭证照常输出。
		activateAccount(client, ep, tok.AccessToken, acct.UID, ls.Region)
		out := map[string]any{
			"access_token":  tok.AccessToken,
			"refresh_token": tok.RefreshToken,
			"expires_in":    tok.ExpiresIn,
			"domain":        tok.Domain,
			"uid":           acct.UID,
			"enterprise_id": acct.EnterpriseID,
			"nickname":      acct.Nickname,
		}
		oraw, _ := json.Marshal(out)
		fmt.Println(string(oraw))
		os.Remove(statePath(sid))

	default:
		fatal("unknown subcommand %q (want url|poll)", os.Args[1])
	}
}
