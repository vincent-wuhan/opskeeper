package runtimemgmt

import (
	"encoding/json"
	"testing"
	"time"
)

// registeredAt 是一个固定基准时刻；所有用例以它推算窗口边界。
var healthBase = time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)

func hbAt(d time.Duration) *time.Time {
	t := healthBase.Add(d)
	return &t
}

// TestEvaluateHealthTransitionTable 逐格验证 spec「Runtime health has
// bounded transitions」的转移表。
func TestEvaluateHealthTransitionTable(t *testing.T) {
	cases := []struct {
		name string
		rec  RuntimeRecord
		at   time.Time
		want RuntimeHealth
	}{
		{
			name: "fresh registration without any heartbeat stays starting",
			rec:  RuntimeRecord{Health: RuntimeHealthStarting, RegisteredAt: healthBase},
			at:   healthBase.Add(DefaultHeartbeatInterval),
			want: RuntimeHealthStarting,
		},
		{
			name: "registration that never heartbeats goes offline past the window",
			rec:  RuntimeRecord{Health: RuntimeHealthStarting, RegisteredAt: healthBase},
			at:   healthBase.Add(DefaultOfflineAfter + time.Second),
			want: RuntimeHealthOffline,
		},
		{
			name: "one missed heartbeat interval does not downgrade (spec scenario)",
			rec: RuntimeRecord{
				Health: RuntimeHealthOnline, RegisteredAt: healthBase,
				LastHeartbeatAt: hbAt(0),
			},
			at:   healthBase.Add(DefaultHeartbeatInterval),
			want: RuntimeHealthOnline,
		},
		{
			name: "beyond reconnect grace becomes recently_lost",
			rec: RuntimeRecord{
				Health: RuntimeHealthOnline, RegisteredAt: healthBase,
				LastHeartbeatAt: hbAt(0),
			},
			at:   healthBase.Add(DefaultReconnectGrace + time.Second),
			want: RuntimeHealthRecentlyLost,
		},
		{
			name: "still recently_lost between grace and offline ceiling",
			rec: RuntimeRecord{
				Health: RuntimeHealthOnline, RegisteredAt: healthBase,
				LastHeartbeatAt: hbAt(0),
			},
			at:   healthBase.Add(DefaultOfflineAfter - time.Second),
			want: RuntimeHealthRecentlyLost,
		},
		{
			name: "beyond offline ceiling goes offline",
			rec: RuntimeRecord{
				Health: RuntimeHealthOnline, RegisteredAt: healthBase,
				LastHeartbeatAt: hbAt(0),
			},
			at:   healthBase.Add(DefaultOfflineAfter + time.Second),
			want: RuntimeHealthOffline,
		},
		{
			name: "degraded stays degraded while heartbeats continue (drift is not time-healed)",
			rec: RuntimeRecord{
				Health: RuntimeHealthDegraded, RegisteredAt: healthBase,
				LastHeartbeatAt: hbAt(0), HealthReason: "observed version drift",
			},
			at:   healthBase.Add(time.Second),
			want: RuntimeHealthDegraded,
		},
		{
			name: "degraded degrades further to recently_lost once grace passes",
			rec: RuntimeRecord{
				Health: RuntimeHealthDegraded, RegisteredAt: healthBase,
				LastHeartbeatAt: hbAt(0), HealthReason: "observed version drift",
			},
			at:   healthBase.Add(DefaultReconnectGrace + time.Second),
			want: RuntimeHealthRecentlyLost,
		},
		{
			name: "recovery heartbeat inside grace returns online",
			rec: RuntimeRecord{
				Health: RuntimeHealthRecentlyLost, RegisteredAt: healthBase,
				LastHeartbeatAt: hbAt(DefaultOfflineAfter),
			},
			at:   healthBase.Add(DefaultOfflineAfter + time.Second),
			want: RuntimeHealthOnline,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.rec.EvaluateHealth(tc.at); got != tc.want {
				t.Fatalf("EvaluateHealth = %q; want %q", got, tc.want)
			}
		})
	}
}

// TestRuntimeKindClosedSet 保证注册闭集恰好六类：首切片五类 + pig
// （pig-runtime-adapter 2.2 归一层写入的第六类）。
func TestRuntimeKindClosedSet(t *testing.T) {
	if len(AllRuntimeKinds) != 6 {
		t.Fatalf("registry admits exactly six runtime kinds, got %d", len(AllRuntimeKinds))
	}
	for _, k := range []RuntimeKind{"node_edge", "chat_worker", "agentteams_worker", "plugin_manager", "pool_preview", "pig"} {
		if !IsRuntimeKind(k) {
			t.Errorf("IsRuntimeKind(%q) = false; want true", k)
		}
	}
	for _, k := range []RuntimeKind{"", "virtual_machine", "container", "k8s_node"} {
		if IsRuntimeKind(k) {
			t.Errorf("IsRuntimeKind(%q) = true; want false", k)
		}
	}
}

// TestVersionSourceClosedSet 是 spec「version provenance」的模型面。
func TestVersionSourceClosedSet(t *testing.T) {
	if !IsVersionSource(VersionSourceManifest) || !IsVersionSource(VersionSourceReleaseMetadata) {
		t.Fatal("manifest and release_metadata must both be valid sources")
	}
	for _, s := range []VersionSource{"", "hardcoded", "script_constant", "guess"} {
		if IsVersionSource(s) {
			t.Errorf("IsVersionSource(%q) = true; want false", s)
		}
	}
}

// TestRuntimeRecordTableShape 断言表名与无 TenantID 的刻意差异。
func TestRuntimeRecordTableShape(t *testing.T) {
	if got := (RuntimeRecord{}).TableName(); got != "runtimemgmt_runtimes" {
		t.Fatalf("TableName = %q; want runtimemgmt_runtimes", got)
	}
	raw, err := json.Marshal(RuntimeRecord{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var probe map[string]any
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := probe["tenant_id"]; ok {
		t.Fatal("RuntimeRecord must not carry TenantID: a runtime is a platform asset, " +
			"not a tenant asset (ADR §4.1 deliberate difference from Task)")
	}
}
