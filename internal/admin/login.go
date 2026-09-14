package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
)

// ── 设备码登录 ──────────────────────────────────────────────────────
//
// 复用现成的 login 工具（cmd/login，设备码 OAuth），不重写登录逻辑：
//
//	login url [cn|global] → stdout 为授权 URL，state 落盘
//	login poll            → stdout 为凭证 JSON（成功时自删 state）
//
// 两次调用必须同 cwd：login 的 state 路径是盘符相对的，换目录就找不到。
// 故这里统一把工作目录钉在仓库根。

// loginStart 生成授权链接。
func (h *Handler) loginStart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Region string `json:"region"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	region := "cn"
	if body.Region == "global" {
		region = "global"
	}
	if err := h.ensureLoginBin(); err != nil {
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	stdout, stderr, code, err := h.runLogin("url", region)
	if err != nil {
		fail(w, http.StatusInternalServerError, "启动登录工具失败：%v", err)
		return
	}
	if code != 0 {
		msg := strings.TrimSpace(stderr)
		if msg == "" {
			msg = "exit " + strconv.Itoa(code)
		}
		fail(w, http.StatusBadGateway, "login url 失败：%s", msg)
		return
	}
	authURL := lastLine(stdout)
	if !strings.HasPrefix(authURL, "http") {
		fail(w, http.StatusBadGateway, "login url 未返回授权链接：%s", strings.TrimSpace(stdout))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"region": region, "authUrl": authURL})
}

// loginPoll 查询登录结果。未完成不是错误，回 200 + pending 让前端继续轮询。
func (h *Handler) loginPoll(w http.ResponseWriter, r *http.Request) {
	// 单次性操作：成功即消费掉 state，并发调用会互相抢
	if !h.pollMu.TryLock() {
		writeJSON(w, http.StatusConflict, map[string]any{"status": "busy", "message": "上一次查询还没结束"})
		return
	}
	defer h.pollMu.Unlock()

	if err := h.ensureLoginBin(); err != nil {
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	stdout, stderr, code, err := h.runLogin("poll")
	if err != nil {
		fail(w, http.StatusInternalServerError, "启动登录工具失败：%v", err)
		return
	}
	if code != 0 {
		// stderr 原文面向交互式流程（提到「按 y」）且会暴露内部路径，这里改写成人话
		detail := strings.TrimSpace(stderr)
		message := "等待上游签发凭证"
		switch {
		case strings.Contains(detail, "read state"):
			// 没有 state 文件 = 还没点「生成授权链接」，或 state 已被上一次成功消费
			message = "当前没有进行中的登录会话，请先点「生成授权链接」"
		case strings.Contains(detail, "未完成"):
			message = "尚未在浏览器完成登录"
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "pending", "message": message})
		return
	}

	var t struct {
		UID          string `json:"uid"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Domain       string `json:"domain"`
		EnterpriseID string `json:"enterprise_id"`
		Nickname     string `json:"nickname"`
	}
	if err := json.Unmarshal([]byte(lastLine(stdout)), &t); err != nil || t.UID == "" || t.AccessToken == "" {
		writeJSON(w, http.StatusOK, map[string]any{"status": "pending", "message": "凭证不完整，等待上游签发"})
		return
	}

	nested, err := json.Marshal(map[string]any{
		"account": map[string]any{
			"uid":          t.UID,
			"enterpriseId": t.EnterpriseID,
			"nickname":     t.Nickname,
		},
		"auth": map[string]any{
			"accessToken":  t.AccessToken,
			"refreshToken": t.RefreshToken,
			"expiresAt":    time.Now().Unix() + t.ExpiresIn,
			"domain":       t.Domain,
		},
	})
	if err != nil {
		fail(w, http.StatusInternalServerError, "组装凭证失败：%v", err)
		return
	}
	file, err := h.writeAuthFile(t.UID, nested)
	if err != nil {
		fail(w, http.StatusInternalServerError, "写入凭证失败：%v", err)
		return
	}
	loaded, err := h.reloadAccounts()
	if err != nil {
		fail(w, http.StatusInternalServerError, "凭证已写入，但重新加载失败：%v", err)
		return
	}
	logf("登录成功 uid=%s file=%s（池中现有 %d 个）", t.UID, file, loaded)
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"file":    file,
		"account": map[string]any{"uid": t.UID, "nickname": t.Nickname, "region": regionOf(t.Domain)},
		"loaded":  loaded,
	})
}

// regionOf 与 internal/auth 的 Region() 同口径：domain 后缀决定区域。
func regionOf(domain string) string {
	return string((&auth.Auth{Domain: domain}).Region())
}

// runLogin 调 login 工具并取输出。
// 返回的 code 是 login 自己的退出码；err 只在「根本没跑起来」时非 nil（ENOENT 等），
// 两者语义不同：前者是业务结果，后者是环境问题。
func (h *Handler) runLogin(args ...string) (stdout, stderr string, code int, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, h.cfg.LoginBin, args...)
	// login 的 state 文件路径是盘符相对的，url 与 poll 必须同盘同目录
	cmd.Dir = h.cfg.RepoRoot
	hideWindow(cmd) // Windows 下不加会每轮弹一个黑框
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if runErr := cmd.Run(); runErr != nil {
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			return out.String(), errb.String(), ee.ExitCode(), nil
		}
		return out.String(), errb.String(), 0, runErr
	}
	return out.String(), errb.String(), 0, nil
}

// ensureLoginBin login 工具不存在时现场编译一次（与 login.sh 的做法一致）。
func (h *Handler) ensureLoginBin() error {
	if _, err := os.Stat(h.cfg.LoginBin); err == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", h.cfg.LoginBin, "./cmd/login")
	cmd.Dir = h.cfg.RepoRoot
	hideWindow(cmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("找不到 %s，且自动编译失败：%v（%s）", h.cfg.LoginBin, err, truncate(string(out), 300))
	}
	return nil
}

// lastLine 取输出的最后一行。login 工具会打进度日志，真正的结果在最后一行。
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) == 0 {
		return ""
	}
	return strings.TrimSpace(lines[len(lines)-1])
}
