// login.go — WorkBuddy CN OAuth 登录（设备授权流程，CN realm only）。
//
// 两个子命令，由 login.sh 顺序驱动：
//
//	login url   → POST /v2/plugin/auth/state?platform=CLI 拿 state+authUrl，
//	              state 落 /tmp/wb2api-login-state.json，stdout 打印授权 URL
//	login poll  → 读 state，GET /v2/plugin/auth/token?state= 一次，
//	              成功再 GET /v2/plugin/login/account?state= 拿 uid/nickname，
//	              stdout 打印完整 token+account JSON
//
// 无 PKCE（workbuddy 设备流由服务端签发 state）。
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
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
	raw, _ := io.ReadAll(resp.Body)
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

type loginState struct {
	State  string `json:"state"`
	Region string `json:"region"` // "cn"（缺省）或 "global"；poll 据此选上游 base
}

func main() {
	if len(os.Args) < 2 {
		fatal("usage: login <url [cn|global] | poll>")
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
		if err := os.WriteFile(stateFile, raw, 0o600); err != nil {
			fatal("write state: %v", err)
		}
		fmt.Println(st.AuthURL)

	case "poll":
		raw, err := os.ReadFile(stateFile)
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
		os.Remove(stateFile)

	default:
		fatal("unknown subcommand %q (want url|poll)", os.Args[1])
	}
}
