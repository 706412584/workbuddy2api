// Package proxyreg 出站代理槽位与账号绑定的运行期注册表。
//
// 为什么单独一个包（而不是放 upstream 或 admin）：
//   - upstream 只管「给定代理 URL，怎么建 Transport」，不该知道槽位/绑定的业务模型；
//   - admin 是 HTTP 层，不持有运行期状态；
//   - 本包是**唯一事实来源**：槽位定义 + 账号绑定 + 「账号 → 生效代理 URL」的解析，
//     三者在同一把锁下保持一致，避免分开维护时出现「绑定指向已删除的槽位」这类不一致。
//
// 热重载：Set 整体替换（面板保存后调用），进程内立即生效，不重启。
package proxyreg

import (
	"sort"
	"strings"
	"sync"
)

// Slot 一个代理槽位。
type Slot struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	URL     string `json:"url"`
	Enabled bool   `json:"enabled"`
}

// Registry 槽位与绑定的运行期注册表。零值可用（空表 = 全部直连）。
type Registry struct {
	mu sync.RWMutex
	// slots 按 id 索引；顺序另存 order 以保持界面稳定（map 迭代顺序随机）。
	slots map[string]Slot
	order []string
	// binds uid → slotID
	binds map[string]string
}

// New 构造注册表。
func New(slots []Slot, binds map[string]string) *Registry {
	r := &Registry{}
	r.Set(slots, binds)
	return r
}

// Set 整体替换槽位与绑定（面板保存后调用）。热重载入口。
//
// 绑定指向不存在的槽位时**保留该绑定不动**（不静默清除）：槽位可能是被临时删掉
// 又加回来的，静默清掉会让用户丢失配置。解析时按「查不到槽位 = 直连」处理，
// 界面据 Slots() 与 Binds() 的差异提示「该绑定已失效」。
func (r *Registry) Set(slots []Slot, binds map[string]string) {
	next := make(map[string]Slot, len(slots))
	order := make([]string, 0, len(slots))
	for _, s := range slots {
		id := strings.TrimSpace(s.ID)
		if id == "" {
			continue // 无 id 的槽位无法被绑定，丢弃（保存端已校验，此处防御）
		}
		s.ID = id
		s.URL = strings.TrimSpace(s.URL)
		next[id] = s
		order = append(order, id)
	}
	nb := make(map[string]string, len(binds))
	for uid, sid := range binds {
		uid = strings.TrimSpace(uid)
		sid = strings.TrimSpace(sid)
		if uid == "" || sid == "" {
			continue // 空绑定 = 直连，不必存
		}
		nb[uid] = sid
	}
	r.mu.Lock()
	r.slots, r.order, r.binds = next, order, nb
	r.mu.Unlock()
}

// Slots 返回槽位快照（保持插入顺序，供界面稳定展示）。
func (r *Registry) Slots() []Slot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Slot, 0, len(r.order))
	for _, id := range r.order {
		if s, ok := r.slots[id]; ok {
			out = append(out, s)
		}
	}
	return out
}

// Binds 返回 uid → slotID 的副本。
func (r *Registry) Binds() map[string]string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]string, len(r.binds))
	for k, v := range r.binds {
		out[k] = v
	}
	return out
}

// Resolve 返回该 uid 生效的代理 URL；空串 = 直连。
//
// 三种「直连」情形：未绑定 / 绑定的槽位不存在 / 槽位被禁用。
// 槽位禁用回落直连而非报错 —— 让运维能临时摘掉一个坏代理而不必改绑定。
func (r *Registry) Resolve(uid string) string {
	if uid == "" {
		return ""
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	sid, ok := r.binds[uid]
	if !ok {
		return ""
	}
	s, ok := r.slots[sid]
	if !ok || !s.Enabled {
		return ""
	}
	return s.URL
}

// SlotByID 按 id 取槽位（供校验与界面回显）。
func (r *Registry) SlotByID(id string) (Slot, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.slots[id]
	return s, ok
}

// UsageCount 统计每个槽位被多少账号绑定（供界面显示「N 个账号在用」）。
func (r *Registry) UsageCount() map[string]int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := map[string]int{}
	for _, sid := range r.binds {
		out[sid]++
	}
	return out
}

// NextSlotID 生成一个未被占用的槽位 id（形如 slot-1、slot-2…）。
// 供界面「新建槽位」用；用户也可自带 id。
func (r *Registry) NextSlotID() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	used := make(map[string]bool, len(r.slots))
	for id := range r.slots {
		used[id] = true
	}
	for i := 1; ; i++ {
		id := "slot-" + itoa(i)
		if !used[id] {
			return id
		}
	}
}

// itoa 小整数转字符串（避免为一处引 strconv 依赖；id 位数极少）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// SortSlotsByID 按 id 排序（供需要确定性输出的场景，如导出）。
func SortSlotsByID(slots []Slot) {
	sort.Slice(slots, func(i, j int) bool { return slots[i].ID < slots[j].ID })
}
