package alert

import (
	"context"
	"testing"
	"time"

	model "github.com/vincent-wuhan/opskeeper/internal/manager/model/alert"
)

// 验收（BENY-915）：同设备 10min 窗口内、不同规则的告警风暴归成一个组；
// 窗口外/跨设备不归并。
func TestAggregationStormCollapsesToOneGroup(t *testing.T) {
	now := time.Date(2026, 11, 6, 3, 0, 0, 0, time.UTC)
	var created []*model.Incident
	inc := func(rule string, at time.Time, dev uint64) *model.Incident {
		i := &model.Incident{
			ID: uint64(len(created) + 1), Rule: rule, Severity: "warning",
			Status: model.IncidentStatusOpen, DeviceID: &dev,
			FirstFiredAt: at, LastFiredAt: at, EventCount: 1,
		}
		created = append(created, i)
		return i
	}
	dev := uint64(7)
	// 50+ 条同因不同名的告警风暴（同一窗口）
	for k := 0; k < 50; k++ {
		rule := []string{"cpu_high", "latency_p99", "error_rate", "disk_io", "conn_pool"}[k%5]
		i := inc(rule, now.Add(time.Duration(k)*time.Second), dev)
		if _, err := AssignAggregation(context.Background(), &fakeAggRepo{created: created}, i, now.Add(time.Duration(k)*time.Second)); err != nil {
			t.Fatalf("assign %d: %v", k, err)
		}
	}
	g := BuildAggregationGroup(created)
	if g == nil || len(g.Members) != 50 {
		t.Fatalf("storm should collapse to one group of 50, got %+v", g)
	}
	if g.GroupID != 1 {
		t.Fatalf("group id should be earliest incident (1), got %d", g.GroupID)
	}
	// 跨设备不归并
	other := inc("cpu_high", now, 99)
	if _, err := AssignAggregation(context.Background(), &fakeAggRepo{created: created}, other, now); err != nil {
		t.Fatal(err)
	}
	if *other.AggGroupID != other.ID {
		t.Fatalf("cross-device must start its own group, got %d", *other.AggGroupID)
	}
	// 窗口外不归并
	late := inc("cpu_high", now.Add(30*time.Minute), dev)
	if _, err := AssignAggregation(context.Background(), &fakeAggRepo{created: created}, late, now.Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if *late.AggGroupID != late.ID {
		t.Fatalf("out-of-window must start its own group, got %d", *late.AggGroupID)
	}
}

// fakeAggRepo 只实现聚合所需方法。
type fakeAggRepo struct {
	created []*model.Incident
	updates map[uint64]uint64
}

func (r *fakeAggRepo) ListIncidents(_ context.Context, f IncidentFilter) ([]*model.Incident, error) {
	var out []*model.Incident
	for _, i := range r.created {
		if f.Status != "" && i.Status != f.Status {
			continue
		}
		if f.DeviceID != nil && (i.DeviceID == nil || *i.DeviceID != *f.DeviceID) {
			continue
		}
		out = append(out, i)
	}
	return out, nil
}

func (r *fakeAggRepo) UpdateIncidentAggGroup(_ context.Context, id, groupID uint64) error {
	if r.updates == nil {
		r.updates = map[uint64]uint64{}
	}
	r.updates[id] = groupID
	return nil
}
