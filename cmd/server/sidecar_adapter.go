// sidecar_adapter.go 把 cmd/server 的 Sidecar 编排适配成 admin.SidecarController。
//
// 为什么需要适配层：admin 是 HTTP 层，不应依赖 cmd/server 的具体编排类型；
// 两边各自声明同形结构（admin.SidecarStatus / SidecarSyncResult / SidecarSlot），
// 本文件负责翻译。字段一一对应，不做语义转换。
package main

import (
	"workbuddy2api/internal/admin"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/server"
)

// sidecarAdapter 实现 admin.SidecarController。
type sidecarAdapter struct {
	s *Sidecar
	p *pool.Pool
}

func newSidecarAdapter(s *Sidecar, p *pool.Pool) *sidecarAdapter {
	return &sidecarAdapter{s: s, p: p}
}

func (a *sidecarAdapter) Enabled() bool { return a.s.Enabled() }

func (a *sidecarAdapter) Status() admin.SidecarStatus {
	st := a.s.Status()
	out := admin.SidecarStatus{
		Enabled:    st.Enabled,
		NodeCount:  st.NodeCount,
		SlotCount:  st.SlotCount,
		LastSync:   st.LastSync,
		LastError:  st.LastError,
		BasePort:   st.BasePort,
		MaxNodes:   st.MaxNodes,
		BinaryOK:   st.BinaryOK,
		BinaryPath: st.BinaryPath,
	}
	for _, e := range st.Engines {
		out.Engines = append(out.Engines, map[string]any{
			"id":          e.ID,
			"running":     e.Running,
			"config_hash": e.ConfigHash,
			"last_error":  e.LastError,
		})
	}
	return out
}

// Sync body 非 nil 时用其作为订阅内容（面板粘贴/上传路径），否则按配置 URL 拉取。
// 注：长度 0 的 non-nil 切片也走粘贴路径 —— 语义是「用我给你的内容，别再拉 URL」。
func (a *sidecarAdapter) Sync(body []byte) (admin.SidecarSyncResult, error) {
	var res *SyncResult
	var err error
	if body != nil {
		res, err = a.s.ApplySubscriptionBody(body)
	} else {
		res, err = a.s.Sync()
	}
	if err != nil {
		return admin.SidecarSyncResult{}, err
	}
	out := admin.SidecarSyncResult{
		NodeCount: res.NodeCount,
		FetchedAt: res.FetchedAt.Format("2006-01-02T15:04:05Z07:00"),
		Warnings:  res.Warnings,
	}
	for _, sl := range res.Slots {
		out.Slots = append(out.Slots, admin.SidecarSlot{
			ID: sl.ID, Name: sl.Name, URL: sl.URL, Enabled: sl.Enabled,
		})
	}
	return out, nil
}

func (a *sidecarAdapter) Assign(uids []string) (map[string]string, error) {
	return a.s.Assign(uids)
}

// AccountUIDs 返回池内账号 uid（供「全部账号自动分配」）。
func (a *sidecarAdapter) AccountUIDs() []string {
	if a.p == nil {
		return nil
	}
	return a.p.AvailableUIDs()
}

// sidecarController 返回可注入 admin 的控制器；sidecar 为 nil（未启用）时也返回
// 非 nil 适配器 —— 让端点能给出「未启用」的明确提示，而不是「功能不存在」。
// 但若连池都没有（极端），返回 nil。
func sidecarController(s *Sidecar, p *pool.Pool) admin.SidecarController {
	if s == nil {
		return nil
	}
	return newSidecarAdapter(s, p)
}

// ── 账号分组控制器（注入 admin 的 GroupsController）──────────────────────

type groupsAdapter struct{}

func (groupsAdapter) Snapshot() map[string]string { return server.GroupsSnapshot() }
func (groupsAdapter) SetAll(m map[string]string)  { server.SetAccountGroups(m) }
func (groupsAdapter) Names() []string             { return server.GroupNames() }
func (groupsAdapter) GroupOf(uid string) string   { return server.GroupOf(uid) }
