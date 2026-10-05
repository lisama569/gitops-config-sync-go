package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func writeSource(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestMergePriorityOverride(t *testing.T) {
	dir := t.TempDir()
	base := writeSource(t, dir, "base.json", `{"resources":{"db":{"replicas":1,"config":{"image":"pg:15"}}}}`)
	overlay := writeSource(t, dir, "overlay.json", `{"resources":{"db":{"replicas":3}}}`)

	lo, err := LoadSource(base, 10)
	if err != nil {
		t.Fatal(err)
	}
	hi, err := LoadSource(overlay, 20)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("输入: base(prio=10)=%v overlay(prio=20)=%v", lo.Resources, hi.Resources)

	merged, err := Merge([]Source{lo, hi})
	if err != nil {
		t.Fatal(err)
	}
	db := merged["db"]
	if db.Replicas != 3 {
		t.Fatalf("高优先级来源应覆盖 replicas, got %d", db.Replicas)
	}
	if db.Config["image"] != "pg:15" {
		t.Fatalf("低优先级来源未被覆盖的字段应保留, got %v", db.Config)
	}
	t.Logf("合并结果: %+v", db)
}

func TestMergeConflictingOverride(t *testing.T) {
	dir := t.TempDir()
	a := writeSource(t, dir, "a.json", `{"resources":{"db":{"replicas":2}}}`)
	b := writeSource(t, dir, "b.json", `{"resources":{"db":{"replicas":5}}}`)

	sa, _ := LoadSource(a, 10)
	sb, _ := LoadSource(b, 10) // 同优先级、同字段、不同值
	t.Logf("输入: a(prio=10).replicas=2 b(prio=10).replicas=5")

	_, err := Merge([]Source{sa, sb})
	var rej *RejectError
	if !errors.As(err, &rej) {
		t.Fatalf("应返回 *RejectError, got %v", err)
	}
	if rej.Reason != ReasonConflictingOverride {
		t.Fatalf("拒绝原因应为 %s, got %s", ReasonConflictingOverride, rej.Reason)
	}
	t.Logf("冲突被拒绝: %v", rej)
}

func TestMergeSamePrioritySameValueOK(t *testing.T) {
	dir := t.TempDir()
	a := writeSource(t, dir, "a.json", `{"resources":{"db":{"replicas":2}}}`)
	b := writeSource(t, dir, "b.json", `{"resources":{"db":{"replicas":2}}}`)
	sa, _ := LoadSource(a, 10)
	sb, _ := LoadSource(b, 10)

	merged, err := Merge([]Source{sa, sb})
	if err != nil {
		t.Fatalf("同优先级同值不应视为冲突: %v", err)
	}
	if merged["db"].Replicas != 2 {
		t.Fatalf("got %d", merged["db"].Replicas)
	}
}

func TestMergeInvalidFieldType(t *testing.T) {
	dir := t.TempDir()
	bad := writeSource(t, dir, "bad.json", `{"resources":{"db":{"replicas":"three"}}}`)
	s, _ := LoadSource(bad, 10)
	t.Logf("输入: db.replicas=%q (字符串, 非法)", "three")

	_, err := Merge([]Source{s})
	var rej *RejectError
	if !errors.As(err, &rej) {
		t.Fatalf("应返回 *RejectError, got %v", err)
	}
	if rej.Reason != ReasonInvalidFieldType {
		t.Fatalf("拒绝原因应为 %s, got %s", ReasonInvalidFieldType, rej.Reason)
	}
	t.Logf("类型错误被拒绝: %v", rej)
}

func TestLoadSourceMissing(t *testing.T) {
	_, err := LoadSource(filepath.Join(t.TempDir(), "nope.json"), 10)
	var rej *RejectError
	if !errors.As(err, &rej) {
		t.Fatalf("应返回 *RejectError, got %v", err)
	}
	if rej.Reason != ReasonSourceMissing {
		t.Fatalf("拒绝原因应为 %s, got %s", ReasonSourceMissing, rej.Reason)
	}
	t.Logf("缺失来源被拒绝: %v", rej)
}

func TestRenderReproducible(t *testing.T) {
	dir := t.TempDir()
	p := writeSource(t, dir, "s.json", `{"resources":{
		"web":{"dependsOn":["db"],"replicas":2,"config":{"image":"web:1","mode":"ro"}},
		"db":{"replicas":1,"config":{"image":"pg:15"}}
	}}`)
	s, err := LoadSource(p, 10)
	if err != nil {
		t.Fatal(err)
	}

	render := func() DesiredState {
		merged, err := Merge([]Source{s})
		if err != nil {
			t.Fatal(err)
		}
		ds, err := Render("prod", merged)
		if err != nil {
			t.Fatal(err)
		}
		return ds
	}
	d1, d2 := render(), render()
	t.Logf("第一次渲染 hash=%s 资源序=%v", d1.Hash, []string{d1.Resources[0].Name, d1.Resources[1].Name})
	t.Logf("第二次渲染 hash=%s", d2.Hash)
	if d1.Hash != d2.Hash {
		t.Fatalf("同一环境下渲染必须可复现: %s != %s", d1.Hash, d2.Hash)
	}
	if d1.Resources[0].Name != "db" || d1.Resources[1].Name != "web" {
		t.Fatalf("资源应按名称排序: %v", d1.Resources)
	}
}

func TestMergeConfigKeyLevelOverride(t *testing.T) {
	dir := t.TempDir()
	base := writeSource(t, dir, "base.json", `{"resources":{"db":{"config":{"image":"pg:15","mode":"rw","pool":"8"}}}}`)
	overlay := writeSource(t, dir, "overlay.json", `{"resources":{"db":{"config":{"mode":"ro"}}}}`)

	lo, err := LoadSource(base, 10)
	if err != nil {
		t.Fatal(err)
	}
	hi, err := LoadSource(overlay, 20)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("输入: base(prio=10).config=%v overlay(prio=20).config=%v", lo.Resources["db"], hi.Resources["db"])

	merged, err := Merge([]Source{lo, hi})
	if err != nil {
		t.Fatal(err)
	}
	cfg := merged["db"].Config
	t.Logf("合并结果 config: %v", cfg)
	if cfg["mode"] != "ro" {
		t.Fatalf("高优先级来源触及的键应被覆盖, got mode=%q", cfg["mode"])
	}
	if cfg["image"] != "pg:15" || cfg["pool"] != "8" {
		t.Fatalf("高优先级来源未触及的键必须从低优先级原样保留, got %v", cfg)
	}
}

func TestMergeConfigDisjointKeysSamePriority(t *testing.T) {
	dir := t.TempDir()
	a := writeSource(t, dir, "a.json", `{"resources":{"db":{"config":{"image":"pg:15"}}}}`)
	b := writeSource(t, dir, "b.json", `{"resources":{"db":{"config":{"pool":"8"}}}}`)

	sa, _ := LoadSource(a, 10)
	sb, _ := LoadSource(b, 10) // 同优先级, 但 config 键互不相交
	t.Logf("输入: a(prio=10).config={image} b(prio=10).config={pool} (键互不相交)")

	merged, err := Merge([]Source{sa, sb})
	if err != nil {
		t.Fatalf("同优先级来源设置互不相交的键不应视为冲突: %v", err)
	}
	cfg := merged["db"].Config
	t.Logf("合并结果 config: %v", cfg)
	if cfg["image"] != "pg:15" || cfg["pool"] != "8" {
		t.Fatalf("互不相交的键应全部保留, got %v", cfg)
	}
}

func TestMergeConfigSameKeyConflictSamePriority(t *testing.T) {
	dir := t.TempDir()
	a := writeSource(t, dir, "a.json", `{"resources":{"db":{"config":{"mode":"ro"}}}}`)
	b := writeSource(t, dir, "b.json", `{"resources":{"db":{"config":{"mode":"rw"}}}}`)

	sa, _ := LoadSource(a, 10)
	sb, _ := LoadSource(b, 10) // 同优先级、同一个 config 键、不同值
	t.Logf("输入: a(prio=10).config.mode=ro b(prio=10).config.mode=rw")

	_, err := Merge([]Source{sa, sb})
	var rej *RejectError
	if !errors.As(err, &rej) {
		t.Fatalf("应返回 *RejectError, got %v", err)
	}
	if rej.Reason != ReasonConflictingOverride {
		t.Fatalf("拒绝原因应为 %s, got %s", ReasonConflictingOverride, rej.Reason)
	}
	if rej.Field != "db.config.mode" {
		t.Fatalf("冲突应定位到具体键, got field=%q", rej.Field)
	}
	// 拒绝原因必须能与来源缺失、类型非法区分。
	if rej.Reason == ReasonSourceMissing || rej.Reason == ReasonInvalidFieldType {
		t.Fatalf("冲突原因被错误归类: %s", rej.Reason)
	}
	t.Logf("键级冲突被拒绝: %v", rej)
}

func TestMergeConfigKeyLevelRenderReproducible(t *testing.T) {
	dir := t.TempDir()
	base := writeSource(t, dir, "base.json", `{"resources":{"db":{"replicas":1,"config":{"image":"pg:15","pool":"8"}}}}`)
	overlay := writeSource(t, dir, "overlay.json", `{"resources":{"db":{"config":{"pool":"16","mode":"ro"}}}}`)
	lo, _ := LoadSource(base, 10)
	hi, _ := LoadSource(overlay, 20)

	render := func() DesiredState {
		merged, err := Merge([]Source{lo, hi})
		if err != nil {
			t.Fatal(err)
		}
		ds, err := Render("prod", merged)
		if err != nil {
			t.Fatal(err)
		}
		return ds
	}
	d1, d2 := render(), render()
	t.Logf("键级合并渲染: hash1=%s hash2=%s config=%v", d1.Hash, d2.Hash, d1.Resources[0].Config)
	if d1.Hash != d2.Hash {
		t.Fatalf("同一环境下反复渲染必须一致: %s != %s", d1.Hash, d2.Hash)
	}
}
