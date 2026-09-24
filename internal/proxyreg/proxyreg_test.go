package proxyreg

import "testing"

// TestResolveThreeDirectCases Resolve 的三种「直连」情形必须都回落空串：
// 未绑定 / 槽位不存在 / 槽位被禁用。禁用回落直连是刻意的——让运维能临时摘掉
// 一个坏代理而不必改绑定。
func TestResolveThreeDirectCases(t *testing.T) {
	r := New(
		[]Slot{
			{ID: "on", Name: "可用", URL: "http://a:1", Enabled: true},
			{ID: "off", Name: "停用", URL: "http://b:2", Enabled: false},
		},
		map[string]string{
			"u-on":    "on",
			"u-off":   "off",
			"u-ghost": "no-such-slot",
		},
	)
	cases := []struct {
		uid, want string
		why       string
	}{
		{"u-on", "http://a:1", "绑定了启用槽位 → 返回其 URL"},
		{"u-off", "", "槽位被禁用 → 回落直连"},
		{"u-ghost", "", "槽位不存在 → 回落直连（绑定保留不静默清除）"},
		{"u-unbound", "", "未绑定 → 直连"},
		{"", "", "空 uid → 直连"},
	}
	for _, c := range cases {
		if got := r.Resolve(c.uid); got != c.want {
			t.Errorf("Resolve(%q)=%q want %q（%s）", c.uid, got, c.want, c.why)
		}
	}
}

// TestOrphanBindPreserved 指向不存在槽位的绑定必须**保留**（不静默清除）：
// 槽位可能是被临时删掉又加回来的，静默清掉会让用户丢配置。
func TestOrphanBindPreserved(t *testing.T) {
	r := New([]Slot{{ID: "a", URL: "http://a:1", Enabled: true}},
		map[string]string{"u1": "a", "u2": "ghost"})
	binds := r.Binds()
	if binds["u2"] != "ghost" {
		t.Errorf("失效绑定被清除了：%v", binds)
	}
	// 界面据此提示：SlotByID 查不到即失效
	if _, ok := r.SlotByID("ghost"); ok {
		t.Error("ghost 不该存在")
	}
}

// TestSetReplacesAtomically Set 是整体替换语义（面板保存入口）。
func TestSetReplacesAtomically(t *testing.T) {
	r := New([]Slot{{ID: "old", URL: "http://old:1", Enabled: true}}, map[string]string{"u": "old"})
	r.Set([]Slot{{ID: "new", URL: "http://new:1", Enabled: true}}, map[string]string{"u": "new"})
	if got := r.Resolve("u"); got != "http://new:1" {
		t.Errorf("Set 后 Resolve=%q want 新槽位", got)
	}
	if _, ok := r.SlotByID("old"); ok {
		t.Error("旧槽位应被替换掉")
	}
}

// TestSlotsKeepInsertionOrder 槽位顺序须稳定（map 迭代顺序随机，界面会跳动）。
func TestSlotsKeepInsertionOrder(t *testing.T) {
	in := []Slot{
		{ID: "z", URL: "http://z:1", Enabled: true},
		{ID: "a", URL: "http://a:1", Enabled: true},
		{ID: "m", URL: "http://m:1", Enabled: true},
	}
	r := New(in, nil)
	got := r.Slots()
	for i, want := range []string{"z", "a", "m"} {
		if got[i].ID != want {
			t.Errorf("顺序[%d]=%q want %q（应保持插入顺序）", i, got[i].ID, want)
		}
	}
}

// TestUsageCount 统计每槽位被多少账号绑定。
func TestUsageCount(t *testing.T) {
	r := New(
		[]Slot{{ID: "a", URL: "http://a:1", Enabled: true}, {ID: "b", URL: "http://b:2", Enabled: true}},
		map[string]string{"u1": "a", "u2": "a", "u3": "b"},
	)
	u := r.UsageCount()
	if u["a"] != 2 || u["b"] != 1 {
		t.Errorf("usage=%v want a:2 b:1", u)
	}
}

// TestNextSlotIDAvoidsCollision 新建槽位 id 不与现有冲突。
func TestNextSlotIDAvoidsCollision(t *testing.T) {
	r := New([]Slot{{ID: "slot-1", URL: "http://a:1", Enabled: true},
		{ID: "slot-2", URL: "http://b:2", Enabled: true}}, nil)
	if got := r.NextSlotID(); got != "slot-3" {
		t.Errorf("NextSlotID=%q want slot-3", got)
	}
}

// TestEmptyRegistryIsAllDirect 零值/空表 = 全部直连（不 panic）。
func TestEmptyRegistryIsAllDirect(t *testing.T) {
	var r Registry
	if got := r.Resolve("any"); got != "" {
		t.Errorf("零值 Registry Resolve=%q want 空", got)
	}
	if n := len(r.Slots()); n != 0 {
		t.Errorf("零值 Slots=%d want 0", n)
	}
}

// TestSetDropsInvalidSlots 无 id 的槽位被丢弃（无法被绑定），空绑定不持久化。
func TestSetDropsInvalidSlots(t *testing.T) {
	r := New([]Slot{
		{ID: "", URL: "http://x:1", Enabled: true}, // 无 id → 丢
		{ID: "ok", URL: "http://y:1", Enabled: true},
	}, map[string]string{"u1": "ok", "u2": ""})
	if n := len(r.Slots()); n != 1 {
		t.Errorf("Slots=%d want 1（无 id 的应被丢弃）", n)
	}
	if _, ok := r.Binds()["u2"]; ok {
		t.Error("空绑定不该被持久化")
	}
}
