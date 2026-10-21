// Package auth 解析 WorkBuddy auth 文件（嵌套形/扁平形双形态），
// 提供 refresh 后的原子写回。
package auth

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Auth 是归一化后的账号凭证（来源可以是插件 OAuth 嵌套形或手写扁平形）。
type Auth struct {
	// mu 串行化 RefreshToken 写与 SaveAtomic 读，防止并发写回半更新 token。
	mu sync.Mutex

	AccessToken  string
	RefreshToken string
	ExpiresAt    int64 // Unix 秒
	Domain       string
	UID          string
	EnterpriseID string
	Nickname     string
	FilePath     string // 来源文件；refresh 后原子写回此处

	// DeviceToken 设备风控 Token（X-Device-Token 头），来源 auth 文件的 device_token 键。
	// 缺省为空 = 不注入该头（容器内无桌面端 Turing SDK 的常见部署）。
	// 手写扁平形 auth 文件可直接写 "device_token": "..."；插件 OAuth 嵌套形
	// 顶层 device_token 也会被解析（与桌面端共用状态文件的部署方式）。
	DeviceToken string

	// CreatedAt 本账号**首次入库**的 Unix 秒（登录成功 / 导入凭证落盘的时刻）。
	//
	// 为什么单独记一个字段：上游不暴露账号真实注册时间（实测 /v2/accounts 只返回
	// uid / nickname / uin / type / lastLogin 等，无创建时间），而 auths/*.json 的
	// **文件时间会被 token 刷新整份重写覆盖**（见 SaveAtomic），因此无法从任何既有
	// 来源回溯「这个号是什么时候进池的」。故在首次落盘时记一次，之后由 SaveAtomic
	// 原样保留。0 = 未知（早于本字段引入的旧凭证，**不做回溯猜测**）。
	CreatedAt int64
}

// Region 账号所属的上游区域。区域由凭证的 domain 字段推导，
// 因此同一账号池可混放国内版与国外版账号，无需全局配置。
type Region string

const (
	// RegionCN 国内版（codebuddy.cn / copilot.tencent.com）。
	RegionCN Region = "cn"
	// RegionGlobal 国外版（workbuddy.ai）。
	RegionGlobal Region = "global"
)

// globalSuffix 判定国外版账号的域名后缀；子域（如 www./api.）也属于国外版。
const globalSuffix = ".workbuddy.ai"

// Region 返回账号所属区域。domain 为空视为 CN（向后兼容旧凭证）。
//
// 持 a.mu：Domain 由 RefreshToken 在锁内改写，锁外读构成数据竞争
// （见 AccessTokenValue 注释）。调用方**不得**已持 a.mu（sync.Mutex 不可重入）。
func (a *Auth) Region() Region {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.regionLocked()
}

// regionLocked Region 的无锁内部实现：仅限**已持 a.mu** 的调用方使用
// （sync.Mutex 不可重入，锁内再调 Region() 会自锁）。
func (a *Auth) regionLocked() Region {
	d := strings.ToLower(strings.TrimSpace(a.Domain))
	if d == strings.TrimPrefix(globalSuffix, ".") || strings.HasSuffix(d, globalSuffix) {
		return RegionGlobal
	}
	return RegionCN
}

// Lock 供同进程内其他包（upstream.RefreshToken）在改写 Auth 字段期间加锁。
func (a *Auth) Lock() { a.mu.Lock() }

// Unlock 释放 a.Lock 获取的锁。
func (a *Auth) Unlock() { a.mu.Unlock() }

// RefreshTokenValue 加锁读取 RefreshToken（出站/守卫一律经此取值，勿直读字段）。
//
// 为什么必须加锁：RefreshToken 在 a.mu 内改写 RefreshToken/AccessToken/Domain/ExpiresAt
// （client.go「第 2 段（锁内）：校验快照一致后写回」），而调度器的「有无凭证」前置守卫
// （checkin/keepalive/travel 的 `a.RefreshToken == ""`）在锁外直读该字段。生产上两侧
// 真会并发：Scheduler.RunKeepaliveNow 定时对**每个**非禁用账号刷新，与是否有在途请求
// 无关。无同步直读构成数据竞争，go test -race 实证（回归测试
// scheduler.TestKeepaliveGuardRacesRefreshToken）：
//
//	WARNING: DATA RACE
//	Read at ... by goroutine:
//	  (*Scheduler).RunKeepaliveNow()  internal/scheduler/scheduler.go:654
//	Previous write at ... by goroutine:
//	  (*Client).RefreshToken()        internal/upstream/client.go:931
func (a *Auth) RefreshTokenValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.RefreshToken
}

// AccessTokenValue 加锁读取 AccessToken（出站请求头一律经此取值，勿直读字段）。
//
// 为什么必须加锁：RefreshToken 在 a.mu 内改写 AccessToken/RefreshToken/Domain/ExpiresAt，
// 而所有出站请求头构造（ChatHeaders / BillingHeaders / FetchModels / RefreshHeaders）
// 与调度器的 token 检查都在锁外直读这些字段。生产上两侧真会并发：
// Scheduler.RunKeepaliveNow 定时对**每个**非禁用账号刷新（与是否有在途请求无关），
// 而 handler 正基于**同一个** *auth.Auth 指针构造请求头（Pool.AuthByUID/List 返回的
// 就是池内同一个对象）。无同步直读构成数据竞争，go test -race 实证：
//
//	WARNING: DATA RACE
//	Write at ... by goroutine:
//	  (*Client).RefreshToken()  internal/upstream/client.go
//	Previous read at ... by goroutine:
//	  (*Client).ChatHeaders()   internal/upstream/headers.go
func (a *Auth) AccessTokenValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.AccessToken
}

// DomainValue 加锁读取 Domain（同 AccessTokenValue：RefreshToken 在锁内改写它）。
func (a *Auth) DomainValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Domain
}

// TokenSource 从 AccessToken 的 JWT payload 里读 token_source 声明（官方客户端
// 用它填出站 X-Auth-Method 与 X-Userinfo.token_source）。
//
// 非 JWT / 无该声明 / 解码失败一律返回空串——空表示「这个账号没有该维度」，
// 调用方随之不发对应头（不伪造）。
//
// 每次现解码而不缓存：RefreshToken 会整体换发 token，token_source 可能随重新授权
// 翻转。成本是一次 base64 + 小 JSON，相对一次上游请求可忽略。
func (a *Auth) TokenSource() string {
	if a == nil {
		return ""
	}
	return jwtClaim(a.AccessTokenValue(), "token_source")
}

// jwtClaim 解出 JWT payload 里的指定字符串声明；非 JWT / 解码失败返回空。
// 先按无填充 base64url 解（JWT 标准），失败再试带填充形态（兼容非标准签发方）。
func jwtClaim(token, name string) string {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		if raw, err = base64.URLEncoding.DecodeString(parts[1]); err != nil {
			return ""
		}
	}
	var payload map[string]any
	if json.Unmarshal(raw, &payload) != nil {
		return ""
	}
	v, _ := payload[name].(string)
	return strings.TrimSpace(v)
}

// NeedsRefresh 报告 token 是否将在 within 内过期（或已过期/无 expiry）。
// 持 a.mu：ExpiresAt 由 RefreshToken 在锁内改写。
func (a *Auth) NeedsRefresh(within time.Duration) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ExpiresAt <= 0 {
		return true
	}
	return time.Now().Add(within).Unix() >= a.ExpiresAt
}

// Parse 兼容两种磁盘形态：
//
//	嵌套形 {"auth":{...},"account":{...}}  （插件 OAuth 输出）
//	扁平形 {"accessToken":...,"uid":...}   （手写/旧版）
func Parse(raw []byte) (*Auth, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty auth storage")
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	var a Auth
	if _, nested := probe["auth"]; nested {
		var n struct {
			Auth struct {
				AccessToken  string `json:"accessToken"`
				RefreshToken string `json:"refreshToken"`
				ExpiresAt    int64  `json:"expiresAt"`
				Domain       string `json:"domain"`
			} `json:"auth"`
			Account struct {
				UID          string `json:"uid"`
				EnterpriseID string `json:"enterpriseId"`
				Nickname     string `json:"nickname"`
			} `json:"account"`
			// DeviceToken 顶层 device_token（嵌套形与扁平形共用）。
			// 放在 auth 段之外，手写时无需嵌进 auth 对象，降低配置门槛。
			DeviceToken string `json:"device_token"`
			// CreatedAt 顶层 created_at（首次入库时间，Unix 秒；缺省 0 = 未知）。
			CreatedAt int64 `json:"created_at"`
		}
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		a = Auth{
			AccessToken:  n.Auth.AccessToken,
			RefreshToken: n.Auth.RefreshToken,
			ExpiresAt:    n.Auth.ExpiresAt,
			Domain:       n.Auth.Domain,
			UID:          n.Account.UID,
			EnterpriseID: n.Account.EnterpriseID,
			Nickname:     n.Account.Nickname,
			DeviceToken:  n.DeviceToken,
			CreatedAt:    n.CreatedAt,
		}
	} else {
		var f struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    int64  `json:"expiresAt"`
			Domain       string `json:"domain"`
			UID          string `json:"uid"`
			EnterpriseID string `json:"enterpriseId"`
			Nickname     string `json:"nickname"`
			DeviceToken  string `json:"device_token"`
			CreatedAt    int64  `json:"created_at"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		a = Auth{
			AccessToken:  f.AccessToken,
			RefreshToken: f.RefreshToken,
			ExpiresAt:    f.ExpiresAt,
			Domain:       f.Domain,
			UID:          f.UID,
			EnterpriseID: f.EnterpriseID,
			Nickname:     f.Nickname,
			DeviceToken:  f.DeviceToken,
			CreatedAt:    f.CreatedAt,
		}
	}
	if strings.TrimSpace(a.AccessToken) == "" {
		return nil, fmt.Errorf("parse_error: missing accessToken")
	}
	return &a, nil
}

// SaveAtomic 以嵌套形原子写回 FilePath（tmp + rename），保持嵌套形（插件可读）格式。
// 全程持 a.mu：防止与 RefreshToken 修改 token 字段并发，杜绝写回半更新。
// 防御：accessToken 为空时拒绝写回，避免误用空凭证覆盖有效文件。
func (a *Auth) SaveAtomic() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if strings.TrimSpace(a.AccessToken) == "" {
		return fmt.Errorf("save refused: empty accessToken (uid=%s)", a.UID)
	}
	if a.FilePath == "" {
		return fmt.Errorf("no FilePath set")
	}
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken":  a.AccessToken,
			"refreshToken": a.RefreshToken,
			"expiresAt":    a.ExpiresAt,
			"domain":       a.Domain,
		},
		"account": map[string]any{
			"uid":          a.UID,
			"enterpriseId": a.EnterpriseID,
			"nickname":     a.Nickname,
		},
	}
	// DeviceToken 非空才写回顶层 device_token：避免在无该字段的旧文件里引入空键
	// （保持与插件 OAuth 输出形状一致，插件读取忽略未知键）。
	if a.DeviceToken != "" {
		doc["device_token"] = a.DeviceToken
	}
	// CreatedAt 非 0 才写回：0 = 未知（旧凭证），不写空键、也不伪造时间。
	// 由此首次入库时间在后续每次 token 刷新（整份重写）中被保留。
	if a.CreatedAt > 0 {
		doc["created_at"] = a.CreatedAt
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.FilePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.FilePath)
}

// LoadDir 扫描并解析 dir 下 workbuddy*.json；解析失败的文件静默跳过（启动日志由调用方统计）。
func LoadDir(dir string) ([]*Auth, error) {
	files, err := filepath.Glob(filepath.Join(dir, "workbuddy*.json"))
	if err != nil {
		return nil, err
	}
	var out []*Auth
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		a, err := Parse(raw)
		if err != nil {
			continue
		}
		a.FilePath = f
		out = append(out, a)
	}
	return out, nil
}
