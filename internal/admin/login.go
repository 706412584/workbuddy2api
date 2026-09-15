package admin

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
)

// ── 设备码登录 ──────────────────────────────────────────────────────
//
// 复用现成的 login 工具（cmd/login，设备码 OAuth），不重写登录逻辑：
//
//	login url [cn|global] [sessionId] → stdout 为授权 URL，state 落盘
//	login poll [sessionId]            → stdout 为凭证 JSON（成功时自删 state）
//
// 两次调用必须同 cwd：login 的 state 路径是盘符相对的，换目录就找不到。
// 故这里统一把工作目录钉在仓库根。
//
// sessionId 由本层生成并原样回给前端，前端轮询时带回来，从而把并发登录
// 隔离开（见 newSessionID）。不带 sid 的调用走 login 的固定 state 路径，
// 那是 login.sh 与手工调用的老路径。

// sessionIDRE 合法 sessionId 的形状：纯字母数字，长度受限。
// 它会被 login 拼进 state 文件名，故必须挡住路径分隔符与 ..。
// 与 cmd/login 的 sessionIDRE 是同一条规则，两侧都校验。
var sessionIDRE = regexp.MustCompile(`^[A-Za-z0-9]{1,64}$`)

// newSessionID 生成一次性登录会话标识（16 字节随机 → 32 位 hex）。
func newSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

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
	sid, err := newSessionID()
	if err != nil {
		fail(w, http.StatusInternalServerError, "生成登录会话失败：%v", err)
		return
	}
	if err := h.ensureLoginBin(); err != nil {
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	stdout, stderr, code, err := h.runLogin("url", region, sid)
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
	writeJSON(w, http.StatusOK, map[string]any{"region": region, "authUrl": authURL, "sessionId": sid})
}

// loginPoll 查询登录结果。未完成不是错误，回 200 + pending 让前端继续轮询。
func (h *Handler) loginPoll(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SessionID string `json:"sessionId"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	// 没有会话标识 = 没先点「生成授权链接」（或页面刷新丢了），或 sid 被伪造。
	// 这种请求不能退化成 pending：前端会无限「等待浏览器完成登录」而看不出原因。
	if !sessionIDRE.MatchString(body.SessionID) {
		fail(w, http.StatusBadRequest, "没有进行中的登录会话，请先点「生成授权链接」")
		return
	}
	// 单次性操作：成功即消费掉 state，并发调用会互相抢
	if !h.pollMu.TryLock() {
		// 回 200：busy 是「稍后再问」而非错误，前端据此静默跳过本轮。
		// 回 409 会被前端的 adminJSON 当错误抛出并终止轮询。
		writeJSON(w, http.StatusOK, map[string]any{"status": "busy", "message": "上一次查询还没结束"})
		return
	}
	defer h.pollMu.Unlock()

	if err := h.ensureLoginBin(); err != nil {
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	stdout, stderr, code, err := h.runLogin("poll", body.SessionID)
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
	// 测试注入缝：替掉子进程，避免依赖真实二进制与网络。
	if h.cfg.RunLogin != nil {
		return h.cfg.RunLogin(args...)
	}
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
	// 已注入 RunLogin 时不需要真实二进制（测试环境）。
	if h.cfg.RunLogin != nil {
		return nil
	}
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
