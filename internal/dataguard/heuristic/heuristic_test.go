package heuristic

import (
	"context"
	"testing"

	"github.com/vincent-wuhan/opskeeper/internal/dataguard"
)

func TestPGEngine_PII(t *testing.T) {
	e := &pgEngine{}
	tests := []struct {
		name      string
		res       Resource
		wantSens  dataguard.Sensitivity
		wantConf  float64
		wantMatch bool
	}{
		{
			name: "PG table user_pii",
			res: Resource{
				Type:  ResourcePostgres,
				Name:  "user_pii",
				Extra: map[string]string{},
			},
			wantSens:  dataguard.Confidential,
			wantConf:  0.85,
			wantMatch: true,
		},
		{
			name: "PG column id_card",
			res: Resource{
				Type:  ResourcePostgres,
				Name:  "users",
				Extra: map[string]string{"column": "id_card"},
			},
			wantSens:  dataguard.Restricted,
			wantConf:  0.95,
			wantMatch: true,
		},
		{
			name: "PG table unknown",
			res: Resource{
				Type:  ResourcePostgres,
				Name:  "logs",
				Extra: map[string]string{},
			},
			wantMatch: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, matched := e.Match(context.Background(), tt.res)
			if matched != tt.wantMatch {
				t.Fatalf("matched = %v, want %v", matched, tt.wantMatch)
			}
			if matched {
				if got.Sensitivity != tt.wantSens {
					t.Errorf("Sensitivity = %v, want %v", got.Sensitivity, tt.wantSens)
				}
				if got.Confidence != tt.wantConf {
					t.Errorf("Confidence = %v, want %v", got.Confidence, tt.wantConf)
				}
			}
		})
	}
}

func TestK8sEngine_Secret(t *testing.T) {
	e := &k8sEngine{}
	res := Resource{
		Type:  ResourceK8s,
		Name:  "default",
		Extra: map[string]string{"kind": "Secret"},
	}
	got, matched := e.Match(context.Background(), res)
	if !matched {
		t.Fatal("Secret resource should match")
	}
	if got.Sensitivity != dataguard.TopSecret {
		t.Errorf("Sensitivity = %v, want TopSecret", got.Sensitivity)
	}
	if got.Confidence != 1.00 {
		t.Errorf("Confidence = %v, want 1.00", got.Confidence)
	}
}

func TestCompositeEngine(t *testing.T) {
	c := NewCompositeEngine()
	tests := []struct {
		name string
		res  Resource
		want bool
	}{
		{"PG sensitive column", Resource{Type: ResourcePostgres, Name: "users", Extra: map[string]string{"column": "ssn"}}, true},
		{"K8s Secret", Resource{Type: ResourceK8s, Name: "x", Extra: map[string]string{"kind": "Secret"}}, true},
		{"Redis payment key", Resource{Type: ResourceRedis, Name: "payment:order:1"}, true},
		{"Git payment repo", Resource{Type: ResourceGit, Name: "payment-service"}, true},
		{"Unknown resource type", Resource{Type: "unknown", Name: "x"}, false},
		{"PG normal table", Resource{Type: ResourcePostgres, Name: "logs", Extra: map[string]string{}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, matched := c.Match(context.Background(), tt.res)
			if matched != tt.want {
				t.Errorf("matched = %v, want %v", matched, tt.want)
			}
		})
	}
}
