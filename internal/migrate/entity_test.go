package migrate

import (
	"sort"
	"testing"
)

func TestParseEntityType(t *testing.T) {
	cases := []struct {
		in      string
		want    EntityType
		wantErr bool
	}{
		{"users", EntityUsers, false},
		{"projects", EntityProjects, false},
		{"pg_connections", EntityPGConnections, false},
		{"redis_connections", EntityRedisConns, false},
		{"mq_connections", EntityMQConnections, false},
		{"k8s_clusters", EntityK8sClusters, false},
		{"git_repos", EntityGitRepos, false},
		{"inspection_schedules", EntityInspectionSched, false},
		{"alert_rules", EntityAlertRules, false},
		{"unknown", "", true},
		{"", "", true},
	}
	for _, c := range cases {
		got, err := ParseEntityType(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("ParseEntityType(%q) err=%v wantErr=%v", c.in, err, c.wantErr)
			continue
		}
		if !c.wantErr && got != c.want {
			t.Errorf("ParseEntityType(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestAllEntityTypes_Sorted(t *testing.T) {
	all := AllEntityTypes()
	if len(all) != 9 {
		t.Errorf("AllEntityTypes len=%d want 9", len(all))
	}
	sorted := make([]string, len(all))
	for i, e := range all {
		sorted[i] = string(e)
	}
	if !sort.StringsAreSorted(sorted) {
		t.Errorf("AllEntityTypes not sorted: %v", sorted)
	}
}

func TestMigrationOrder_DependsOn(t *testing.T) {
	order := MigrationOrder()
	if len(order) != 9 {
		t.Fatalf("MigrationOrder len=%d want 9", len(order))
	}

	// users 必在最前（无依赖）
	if order[0] != EntityUsers {
		t.Errorf("order[0]=%s want users", order[0])
	}

	// projects 必在 users 之后
	usersIdx := indexOf(order, EntityUsers)
	projectsIdx := indexOf(order, EntityProjects)
	if projectsIdx <= usersIdx {
		t.Errorf("projects(%d) must come after users(%d)", projectsIdx, usersIdx)
	}

	// pg_connections 必在 projects 之后
	pgIdx := indexOf(order, EntityPGConnections)
	if pgIdx <= projectsIdx {
		t.Errorf("pg_connections(%d) must come after projects(%d)", pgIdx, projectsIdx)
	}
}

func TestGetEntityMeta(t *testing.T) {
	meta := GetEntityMeta(EntityUsers)
	if meta == nil {
		t.Fatal("GetEntityMeta(users) nil")
	}
	if meta.Source != "users" || meta.Target != "users" {
		t.Errorf("users meta wrong: src=%s dst=%s", meta.Source, meta.Target)
	}

	pgMeta := GetEntityMeta(EntityPGConnections)
	if pgMeta == nil {
		t.Fatal("GetEntityMeta(pg_connections) nil")
	}
	if !pgMeta.Encryption {
		t.Error("pg_connections must have Encryption=true")
	}
	if pgMeta.Target != "middleware_resources" {
		t.Errorf("pg_connections target=%s want middleware_resources", pgMeta.Target)
	}

	if GetEntityMeta("not_exist") != nil {
		t.Error("GetEntityType(unknown) must return nil")
	}
}

func indexOf(order []EntityType, target EntityType) int {
	for i, e := range order {
		if e == target {
			return i
		}
	}
	return -1
}
