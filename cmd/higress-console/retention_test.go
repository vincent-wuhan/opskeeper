package main

import "testing"

// 决策 330 的另一半：网关那条链此前没有保留期，而「保留期」这个词在小数点左边
// 就能被拼错。这条用例把三种读法钉死——
//
//	未设置 → 不清理（默认，且启动时会说一句）
//	数字   → 按它清理
//	乱码   → 不清理，但要吵
//
// 最后一条是重点。默认值选「不清理」而不是「随便清 30 天」，是因为默认开启等于
// 替运维决定多少天的审计历史可以不要；而一个拼错的变量如果安静地变成默认值，
// 磁盘会慢慢满，且没有人知道为什么。

func TestHigressAuditRetentionDaysReadsTheWholeRange(t *testing.T) {
	cases := []struct {
		name string
		set  bool
		raw  string
		want int
	}{
		{name: "unset means do not sweep", set: false, want: 0},
		{name: "explicit zero means do not sweep", set: true, raw: "0", want: 0},
		{name: "a number is honoured", set: true, raw: "90", want: 90},
		{name: "negative is refused rather than clamped", set: true, raw: "-1", want: -1},
		{name: "garbage does not become a number", set: true, raw: "30d", want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv("OPSKEEPER_HIGRESS_AUDIT_RETENTION_DAYS", tc.raw)
			}
			if got := higressAuditRetentionDays(); got != tc.want {
				t.Fatalf("retention days for %q = %d, want %d", tc.raw, got, tc.want)
			}
		})
	}
}
