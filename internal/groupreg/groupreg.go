// Package groupreg 账号分组的运行期注册表（uid → 分组名）。
//
// 用途：把账号按用途隔离，让不同 API key 只看得到自己那组账号。当前的直接动机是
// **把被上游风控封禁（403/11140）的账号放进「受限」分组**：普通 key 不会再用它们，
// 而管理员可以定期对它们做连通性测试、看是否已解封，解封后移回正常分组。
//
// 为什么单独一个包（同 proxyreg 的分工）：server 只管「给定 uid 查分组」，
// 不该知道分组是配置来的还是别处来的；admin 只管展示与编辑。
//
// 语义（对齐 sub2api 的 isAccountInGroup）：
//
//	key 未绑分组 → 只能用**未分组**账号；
//	key 绑了分组 → 只能用**该组**账号。
//
// 两者互不通用，且**不互相回退** —— 若回退，把封禁号隔离到「受限」组就失去意义
// （普通 key 仍会兜底选中它们）。
//
// 热重载：Set 整体替换（面板保存后调用），进程内立即生效，不重启。
package groupreg

import (
	"sort"
	"strings"
	"sync"
)

// Registry 账号 → 分组 的运行期注册表。零值可用（空表 = 全部未分组）。
type Registry struct {
	mu sync.RWMutex
	// groups uid → 分组名（空串不存，等价未分组）
	groups map[string]string
}

// New 构造注册表。
func New(m map[string]string) *Registry {
	r := &Registry{}
	r.Set(m)
	return r
}

// Set 整体替换（面板保存后调用）。热重载入口。
func (r *Registry) Set(m map[string]string) {
	next := make(map[string]string, len(m))
	for uid, g := range m {
		uid = strings.TrimSpace(uid)
		g = strings.TrimSpace(g)
		if uid == "" || g == "" {
			continue // 空分组 = 未分组，不必存
		}
		next[uid] = g
	}
	r.mu.Lock()
	r.groups = next
	r.mu.Unlock()
}

// GroupOf 返回该账号的分组名；空串 = 未分组。
func (r *Registry) GroupOf(uid string) string {
	if r == nil || uid == "" {
		return ""
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.groups[uid]
}

// All 返回 uid → 分组的副本（供面板展示）。
func (r *Registry) All() map[string]string {
	if r == nil {
		return map[string]string{}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]string, len(r.groups))
	for k, v := range r.groups {
		out[k] = v
	}
	return out
}

// Names 返回出现过的分组名（去重、排序；供面板下拉框）。
func (r *Registry) Names() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	set := make(map[string]bool, len(r.groups))
	for _, g := range r.groups {
		set[g] = true
	}
	out := make([]string, 0, len(set))
	for g := range set {
		out = append(out, g)
	}
	sort.Strings(out)
	return out
}

// Matches 报告「账号 uid 是否属于 group」。
//
// group 为空（key 未绑分组）→ 只匹配**未分组**账号；
// group 非空 → 只匹配属于该组的账号。
// 与 sub2api 的 isAccountInGroup 同语义，且**不回退**。
func (r *Registry) Matches(uid, group string) bool {
	group = strings.TrimSpace(group)
	have := r.GroupOf(uid)
	if group == "" {
		return have == "" // 未分组 key：只看未分组账号
	}
	return have == group
}

// UsageCount 统计每组有多少账号（供面板显示）。
func (r *Registry) UsageCount() map[string]int {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := map[string]int{}
	for _, g := range r.groups {
		out[g]++
	}
	return out
}
