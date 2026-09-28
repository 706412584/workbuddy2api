// toggle_test.go 面板「启用/禁用账号」端点的行为契约。
//
// 这个端点改的是**调度状态**而非凭证：禁用必须让账号立刻退出轮换，但文件要原样留着
// （误禁用一键可回，误删除要重新登录）。同时它必须与自动禁用（12153 session dead）
// 共用同一个 disabled 字段却互不误伤 —— 这两条是本文件的重点。
package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
)

// newToggleFixture 造一个「磁盘有 u1 凭证 + 池里已加载 u1」的面板，返回二者。
// stateFp 用 t.TempDir() 下的路径：Flush 会写它，不能落到仓库里。
func newToggleFixture(t *testing.T) (*Handler, *pool.Pool, string) {
	t.Helper()
	dir := t.TempDir()
	writeFullAuth(t, dir, "u1", "AT1", "RT1")
	p := pool.New(filepath.Join(dir, "state.json"))
	p.Add(&auth.Auth{UID: "u1", Nickname: "n1", AccessToken: "AT1", RefreshToken: "RT1"})
	return New(Config{AuthDir: dir, Pool: p}), p, dir
}

func doToggle(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/__admin/accounts/toggle", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345" // admin 有本机限制
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestToggleDisablesAndRevives 禁用 → 退出轮换且文件仍在；启用 → 回到轮换。
// 「文件仍在」是这条测试的核心：禁用绝不能是删除的别名。
func TestToggleDisablesAndRevives(t *testing.T) {
	h, p, dir := newToggleFixture(t)

	if p.Pick() == nil {
		t.Fatal("前置条件：u1 应可选")
	}

	rec := doToggle(t, h, `{"uid":"u1","disabled":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("禁用 code=%d body=%s", rec.Code, rec.Body)
	}
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Error("禁用后池中仍非 disabled")
	}
	if st.DisabledReason == "" {
		t.Error("禁用后没有 reason —— 面板无法区分人工禁用与自动禁用")
	}
	if p.Pick() != nil {
		t.Error("禁用后账号仍被 Pick 选中 —— 没有真正退出轮换")
	}
	// 凭证文件必须原样保留，且不含任何状态字段（状态在池里，不在文件里）
	raw, err := os.ReadFile(filepath.Join(dir, "workbuddy-u1.json"))
	if err != nil {
		t.Fatalf("禁用把凭证文件删了：%v", err)
	}
	if !strings.Contains(string(raw), "AT1") {
		t.Errorf("凭证内容被改动：%s", raw)
	}

	rec = doToggle(t, h, `{"uid":"u1","disabled":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("启用 code=%d body=%s", rec.Code, rec.Body)
	}
	st, _ = p.Status("u1")
	if st.Disabled || st.DisabledReason != "" {
		t.Errorf("启用后残留禁用态: disabled=%v reason=%q", st.Disabled, st.DisabledReason)
	}
	if p.Pick() == nil {
		t.Error("启用后仍不可选 —— 没有回到轮换")
	}
}

// TestToggleResponseEchoesAction 响应回显 action，面板据此提示「已禁用/已启用」。
func TestToggleResponseEchoesAction(t *testing.T) {
	h, _, _ := newToggleFixture(t)
	for _, tc := range []struct {
		body   string
		action string
		flag   bool
	}{
		{`{"uid":"u1","disabled":true}`, "禁用", true},
		{`{"uid":"u1","disabled":false}`, "启用", false},
	} {
		rec := doToggle(t, h, tc.body)
		var res struct {
			UID      string `json:"uid"`
			Disabled bool   `json:"disabled"`
			Action   string `json:"action"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatalf("%s 响应非 JSON: %v", tc.body, err)
		}
		if res.UID != "u1" || res.Action != tc.action || res.Disabled != tc.flag {
			t.Errorf("%s → %+v，want uid=u1 action=%s disabled=%v", tc.body, res, tc.action, tc.flag)
		}
	}
}

// TestToggleWithoutPoolIsNotImplemented Pool 未接线时回 501 而不是 panic 成 500。
// 面板按这个状态码区分「本进程不支持」与「服务端炸了」。
func TestToggleWithoutPoolIsNotImplemented(t *testing.T) {
	dir := t.TempDir()
	writeFullAuth(t, dir, "u1", "AT1", "RT1")
	h := New(Config{AuthDir: dir}) // 无 Pool

	rec := doToggle(t, h, `{"uid":"u1","disabled":true}`)
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("code=%d want 501 body=%s", rec.Code, rec.Body)
	}
}

// TestToggleUnknownUID404 不在池中的账号回 404 而非静默成功。
//
// 面板会因此把按钮置灰，但接口自身也必须拒绝 —— 否则「启用」一个不存在的号
// 会回 200，用户以为生效了，实际什么都没发生。
func TestToggleUnknownUID404(t *testing.T) {
	h, _, _ := newToggleFixture(t)
	for _, body := range []string{`{"uid":"nope","disabled":true}`, `{"uid":"nope","disabled":false}`} {
		rec := doToggle(t, h, body)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s code=%d want 404 body=%s", body, rec.Code, rec.Body)
		}
	}
}

// TestToggleMissingUID400 缺 uid / uid 全是空白 → 400，不能当成「空 uid 的账号」。
func TestToggleMissingUID400(t *testing.T) {
	h, _, _ := newToggleFixture(t)
	for _, body := range []string{`{}`, `{"uid":""}`, `{"uid":"   "}`} {
		rec := doToggle(t, h, body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s code=%d want 400 body=%s", body, rec.Code, rec.Body)
		}
	}
}

// TestToggleUIDIsTrimmed uid 前后空白被裁掉：前端回显的 uid 可能带不可见空白，
// 直接拿去查池会 404，用户看到「账号明明在列表里却切不了」。
func TestToggleUIDIsTrimmed(t *testing.T) {
	h, p, _ := newToggleFixture(t)
	rec := doToggle(t, h, `{"uid":" u1 ","disabled":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s（uid 未裁空白？）", rec.Code, rec.Body)
	}
	if st, _ := p.Status("u1"); !st.Disabled {
		t.Error("裁空白后的 uid 未生效")
	}
}

// TestTogglePersistsAcrossReload 禁用必须落盘：面板禁用后重启网关，禁用态不能丢。
// 否则重启就成了「静默解除人工禁用」的后门。
func TestTogglePersistsAcrossReload(t *testing.T) {
	h, p, dir := newToggleFixture(t)
	stateFp := filepath.Join(dir, "state.json")

	if rec := doToggle(t, h, `{"uid":"u1","disabled":true}`); rec.Code != http.StatusOK {
		t.Fatalf("禁用失败 code=%d", rec.Code)
	}
	p.Flush() // 面板端点内部已 Flush，这里显式再来一次确保文件写完

	// 新池从同一个 state.json 恢复（pool.New 内部即 load），再 Add 一次凭证
	// （与真实启动顺序一致：先恢复状态，再同步 auths 目录）。
	p2 := pool.New(stateFp)
	p2.Add(&auth.Auth{UID: "u1", Nickname: "n1", AccessToken: "AT1", RefreshToken: "RT1"})

	st, ok := p2.Status("u1")
	if !ok {
		t.Fatal("重启后池里没有 u1")
	}
	if !st.Disabled {
		t.Error("重启后禁用态丢失 —— 人工禁用必须是持久的")
	}
	if st.DisabledReason == "" {
		t.Error("重启后禁用原因丢失")
	}
}

// TestListAccountsMergesPoolState 列表接口要带上池中的禁用态与「是否在池中」。
//
// 面板状态列与「禁用/启用」按钮的可用性都靠这两个字段。缺了它们，面板只能
// 按「文件在不在磁盘上」判断，把禁用号显示成正常号 —— 用户会以为账号在用。
func TestListAccountsMergesPoolState(t *testing.T) {
	h, _, dir := newToggleFixture(t)
	// 再放一个只在磁盘上、没加载进池的账号：它必须 inPool=false。
	writeFullAuth(t, dir, "u2", "AT2", "RT2")

	if rec := doToggle(t, h, `{"uid":"u1","disabled":true}`); rec.Code != http.StatusOK {
		t.Fatalf("禁用失败 code=%d", rec.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/__admin/accounts", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var res struct {
		Accounts []struct {
			UID            string `json:"uid"`
			InPool         bool   `json:"inPool"`
			Disabled       bool   `json:"disabled"`
			DisabledReason string `json:"disabledReason"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("响应非 JSON: %v", err)
	}
	if len(res.Accounts) != 2 {
		t.Fatalf("accounts=%d want 2", len(res.Accounts))
	}
	byUID := map[string]int{}
	for i, a := range res.Accounts {
		byUID[a.UID] = i
	}

	u1 := res.Accounts[byUID["u1"]]
	if !u1.InPool {
		t.Error("u1 在池中却 inPool=false")
	}
	if !u1.Disabled || u1.DisabledReason == "" {
		t.Errorf("u1 已被禁用，列表却没带出禁用态: %+v", u1)
	}

	u2 := res.Accounts[byUID["u2"]]
	if u2.InPool {
		t.Error("u2 不在池中却 inPool=true —— 面板会把它显示成正常号")
	}
	// 未加载的账号 disabled 必须为 false：它没有调度状态可言，
	// 若顺手填 true，面板会对一个根本不在池里的号显示「已禁用」。
	if u2.Disabled {
		t.Error("未加载账号不该带 disabled=true")
	}
}

// TestListAccountsWithoutPoolIsSafe Pool 未注入（配置缺失/测试环境）时列表仍要出得来，
// 只是没有池状态。宁可少两个字段，也不能让整张账号表 500。
func TestListAccountsWithoutPoolIsSafe(t *testing.T) {
	dir := t.TempDir()
	writeFullAuth(t, dir, "u1", "AT1", "RT1")
	h := New(Config{AuthDir: dir}) // 无 Pool

	req := httptest.NewRequest(http.MethodGet, "/__admin/accounts", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("无池时列表 code=%d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"uid":"u1"`) {
		t.Errorf("无池时账号缺失: %s", rec.Body)
	}
}

//
// 签到只证明余额恢复与 billing 通道健康，与「用户明确要求别用这个号」无关。
// 若签到能把人工禁用号放回池子，用户每天早上 9 点都会看到它复活。
func TestManualDisableSurvivesCheckin(t *testing.T) {
	h, p, _ := newToggleFixture(t)
	if rec := doToggle(t, h, `{"uid":"u1","disabled":true}`); rec.Code != http.StatusOK {
		t.Fatalf("禁用失败 code=%d", rec.Code)
	}

	p.ReenableIfCredits("u1", 9999) // 签到成功 + 余额充足

	if st, _ := p.Status("u1"); !st.Disabled {
		t.Error("签到解冻把人工禁用解除了 —— 用户的显式意图被定时任务覆盖")
	}
}
