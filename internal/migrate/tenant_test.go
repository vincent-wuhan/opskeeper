package migrate

import (
	"strings"
	"testing"
)

func TestParseTenantMapping_Valid(t *testing.T) {
	cases := []struct {
		in   string
		want map[int64]int64
	}{
		{"42=1", map[int64]int64{42: 1}},
		{"42=1,100=2,200=3", map[int64]int64{42: 1, 100: 2, 200: 3}},
		{" 42 = 1 , 100 = 2 ", map[int64]int64{42: 1, 100: 2}}, // 容忍空格
		{"", map[int64]int64{}},
	}
	for _, c := range cases {
		got, err := ParseTenantMapping(c.in)
		if err != nil {
			t.Errorf("ParseTenantMapping(%q) err=%v", c.in, err)
			continue
		}
		if got.Size() != len(c.want) {
			t.Errorf("ParseTenantMapping(%q) size=%d want %d", c.in, got.Size(), len(c.want))
			continue
		}
		for src, dst := range c.want {
			mapped, err := got.Map(src)
			if err != nil {
				t.Errorf("Map(%d): %v", src, err)
				continue
			}
			if mapped != dst {
				t.Errorf("Map(%d)=%d want %d", src, mapped, dst)
			}
		}
	}
}

func TestParseTenantMapping_Invalid(t *testing.T) {
	cases := []string{
		"abc=1",     // 源非数字
		"42=abc",    // 目标非数字
		"42=1,42=2", // 重复源
		"42=1,",     // 末尾多余逗号
		"42",        // 缺少 =
		"=1",        // 缺少源
		"42=",       // 缺少目标
	}
	for _, in := range cases {
		_, err := ParseTenantMapping(in)
		if err == nil {
			t.Errorf("ParseTenantMapping(%q) must fail", in)
		}
	}
}

func TestTenantMapper_Map_Undeclared(t *testing.T) {
	m, err := ParseTenantMapping("42=1")
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Map(99)
	if err == nil {
		t.Error("Map(99) must fail on undeclared")
	}
	if !strings.Contains(err.Error(), "未在 --tenant-mapping 中声明") {
		t.Errorf("error message should mention --tenant-mapping, got: %v", err)
	}
}

func TestTenantMapper_ValidateTenant(t *testing.T) {
	m, _ := ParseTenantMapping("42=1,100=2")
	cases := []struct {
		tid  int64
		want bool
	}{
		{1, true},
		{2, true},
		{3, false},
		{0, false},
	}
	for _, c := range cases {
		if got := m.ValidateTenant(c.tid); got != c.want {
			t.Errorf("ValidateTenant(%d)=%v want %v", c.tid, got, c.want)
		}
	}
}

func TestTenantMapper_MustMap(t *testing.T) {
	m, _ := ParseTenantMapping("42=1")
	if got := m.MustMap(42); got != 1 {
		t.Errorf("MustMap(42)=%d want 1", got)
	}
	defer func() {
		if r := recover(); r == nil {
			t.Error("MustMap(99) must panic")
		}
	}()
	m.MustMap(99)
}
