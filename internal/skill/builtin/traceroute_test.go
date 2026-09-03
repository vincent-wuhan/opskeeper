package builtin

import (
	"context"
	"testing"
)

func TestTraceroute_ParamsRequired(t *testing.T) {
	_, err := Traceroute{}.Execute(context.Background(), []byte(`{}`))
	if err == nil {
		t.Error("expected error for missing host")
	}
}

func TestTraceroute_ParseOutput(t *testing.T) {
	out := `traceroute to 8.8.8.8 (8.8.8.8), 30 hops max
 1  10.0.0.1  1.123 ms
 2  10.0.0.2  5.456 ms
 3  8.8.8.8  12.789 ms`
	res := tracerouteResult{}
	parseTraceroute(out, &res)
	if res.TotalHops != 3 {
		t.Errorf("TotalHops = %d, want 3", res.TotalHops)
	}
	if res.Hops[0].Host != "10.0.0.1" {
		t.Errorf("Hops[0].Host = %q, want 10.0.0.1", res.Hops[0].Host)
	}
	if res.Hops[0].RTTMS != 1.123 {
		t.Errorf("Hops[0].RTTMS = %f, want 1.123", res.Hops[0].RTTMS)
	}
}

func TestMTR_ParamsRequired(t *testing.T) {
	_, err := MTR{}.Execute(context.Background(), []byte(`{}`))
	if err == nil {
		t.Error("expected error for missing host")
	}
}

func TestMTR_ParseJSON(t *testing.T) {
	raw := []byte(`{"report":{"hubs":[{"host":"10.0.0.1","count":1,"Loss%":0.0,"Avg":1.5,"Best":1.0,"Wrst":2.0,"StDev":0.5}]}}`)
	res := mtrResult{}
	parseMTRJSON(raw, &res)
	if len(res.Report.Hubs) != 1 {
		t.Fatalf("Hubs = %d, want 1", len(res.Report.Hubs))
	}
	if res.Report.Hubs[0].LossPct != 0.0 {
		t.Errorf("LossPct = %f", res.Report.Hubs[0].LossPct)
	}
	if res.Report.Hubs[0].AvgMS != 1.5 {
		t.Errorf("AvgMS = %f", res.Report.Hubs[0].AvgMS)
	}
}

func TestMTR_ParseBadJSON(t *testing.T) {
	res := mtrResult{}
	parseMTRJSON([]byte("not json"), &res)
	if res.Error == "" {
		t.Error("expected error for bad JSON")
	}
}
