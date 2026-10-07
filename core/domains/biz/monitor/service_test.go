package monitor

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
	"github.com/vincent-wuhan/opskeeper/core/domain"
	model "github.com/vincent-wuhan/opskeeper/core/domains/model/monitor"
)

// fakeRepo is an in-memory Repo implementation just rich enough for the
// service-level tests. It serialises mutations under a mutex so the
// async sync goroutine can race with the API call without sliding into
// undefined behaviour.
type fakeRepo struct {
	mu     sync.Mutex
	rows   map[uint64]*model.Panel
	nextID uint64
	syncs  []string // op log ("set:1:msg" / "delete:1") to assert on
}

func newFakeRepo() *fakeRepo { return &fakeRepo{rows: map[uint64]*model.Panel{}, nextID: 0} }

func (r *fakeRepo) List(_ context.Context) ([]*model.Panel, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*model.Panel, 0, len(r.rows))
	for _, p := range r.rows {
		cp := *p
		out = append(out, &cp)
	}
	return out, nil
}

func (r *fakeRepo) Get(_ context.Context, id uint64) (*model.Panel, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.rows[id]
	if !ok {
		return nil, errs.ErrNotFound
	}
	cp := *p
	return &cp, nil
}

func (r *fakeRepo) MaxOrdinal(_ context.Context) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	max := 0
	for _, p := range r.rows {
		if p.Ordinal > max {
			max = p.Ordinal
		}
	}
	return max, nil
}

func (r *fakeRepo) Create(_ context.Context, p *model.Panel) (*model.Panel, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	p.ID = r.nextID
	cp := *p
	r.rows[p.ID] = &cp
	return p, nil
}

func (r *fakeRepo) Update(_ context.Context, id uint64, fields map[string]any) (*model.Panel, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.rows[id]
	if !ok {
		return nil, errs.ErrNotFound
	}
	if v, ok := fields["title"].(string); ok {
		p.Title = v
	}
	if v, ok := fields["promql"].(string); ok {
		p.PromQL = v
	}
	if v, ok := fields["ordinal"].(int); ok {
		p.Ordinal = v
	}
	cp := *p
	return &cp, nil
}

func (r *fakeRepo) SetSyncResult(_ context.Context, id uint64, msg string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.syncs = append(r.syncs, msg) // empty = ok, non-empty = err
	if p, ok := r.rows[id]; ok {
		p.LastSyncError = msg
	}
	return nil
}

func (r *fakeRepo) Delete(_ context.Context, id uint64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.rows[id]; !ok {
		return errs.ErrNotFound
	}
	delete(r.rows, id)
	return nil
}

// fakeSyncer counts SyncMonitorPanels invocations and optionally returns
// a fixed error so the failure path is exercised.
//
// It also keeps the last specs it was handed. It used to discard the argument
// with `_`, which is the ordinary way to write a fake and the reason the
// projection in panelSpecs had no test at all for the two years the port
// existed: a fake that throws away its input cannot tell you the input was
// translated. The field is read by TestPanelSpecsCopiesExactlyTheSixColumnsADashboardReads.
type fakeSyncer struct {
	mu       sync.Mutex
	calls    int
	failWith error
	done     chan struct{}
	last     []domain.MonitorPanelSpec
}

func (s *fakeSyncer) SyncMonitorPanels(_ context.Context, panels []domain.MonitorPanelSpec) error {
	s.mu.Lock()
	s.calls++
	s.last = panels
	s.mu.Unlock()
	if s.done != nil {
		// Non-blocking signal; drained by the test once it sees calls > 0.
		select {
		case s.done <- struct{}{}:
		default:
		}
	}
	return s.failWith
}

// lastSpecs is the projection the syncer was handed on its most recent call.
func (s *fakeSyncer) lastSpecs() []domain.MonitorPanelSpec {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

func (s *fakeSyncer) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// TestCreateValidatesInputs checks the obvious bad-input rejections so a
// regression doesn't accidentally let through empty PromQL / unknown
// types (both would silently break the Grafana mirror).
func TestCreateValidatesInputs(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	svc := New(repo, nil, nil)

	cases := []struct {
		name string
		in   CreateInput
	}{
		{"empty title", CreateInput{Title: "", PromQL: "up"}},
		{"empty promql", CreateInput{Title: "x", PromQL: "  "}},
		{"bad type", CreateInput{Title: "x", PromQL: "up", Type: "table"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := svc.Create(context.Background(), c.in); !errors.Is(err, errs.ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
		})
	}
}

// TestCreateAssignsOrdinalAndAsyncSync verifies the create flow:
//   - returns 200 immediately (sync runs in a goroutine)
//   - assigns ordinal = max+1 when not provided
//   - eventually invokes the syncer
func TestCreateAssignsOrdinalAndAsyncSync(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	syncer := &fakeSyncer{done: make(chan struct{}, 4)}
	svc := New(repo, syncer, nil)

	first, err := svc.Create(context.Background(), CreateInput{Title: "A", PromQL: "up"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if first.Ordinal != 1 {
		t.Fatalf("first ordinal = %d, want 1", first.Ordinal)
	}
	second, err := svc.Create(context.Background(), CreateInput{Title: "B", PromQL: "up"})
	if err != nil {
		t.Fatalf("create 2: %v", err)
	}
	if second.Ordinal != 2 {
		t.Fatalf("second ordinal = %d, want 2", second.Ordinal)
	}

	// Wait for the goroutines to drain. Two creates → at least two syncs.
	deadline := time.After(2 * time.Second)
	got := 0
	for got < 2 {
		select {
		case <-syncer.done:
			got++
		case <-deadline:
			t.Fatalf("syncer never invoked twice; calls = %d", syncer.callCount())
		}
	}
}

// TestCreateAPI200WhenSyncFails confirms the sync-failure invariant:
// the API call still returns 200 (no error from Create) and the failure
// is recorded via SetSyncResult so the UI can surface it.
func TestCreateAPI200WhenSyncFails(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	syncer := &fakeSyncer{failWith: errors.New("grafana down"), done: make(chan struct{}, 1)}
	svc := New(repo, syncer, nil)

	if _, err := svc.Create(context.Background(), CreateInput{Title: "A", PromQL: "up"}); err != nil {
		t.Fatalf("Create returned err despite sync failure: %v", err)
	}
	select {
	case <-syncer.done:
	case <-time.After(2 * time.Second):
		t.Fatal("syncer never ran")
	}
	// Give the goroutine a beat to write the result row.
	time.Sleep(20 * time.Millisecond)
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if len(repo.syncs) == 0 {
		t.Fatal("SetSyncResult never called")
	}
	if repo.syncs[len(repo.syncs)-1] == "" {
		t.Fatalf("expected non-empty err message, got %v", repo.syncs)
	}
}

// TestCreateNoSyncerSkipsBackground exercises the nil-syncer branch —
// no goroutine, no panic.
func TestCreateNoSyncerSkipsBackground(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	svc := New(repo, nil, nil)
	if _, err := svc.Create(context.Background(), CreateInput{Title: "A", PromQL: "up"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
}

// TestPanelSpecsCopiesExactlyTheSixColumnsADashboardReads is the test the port
// did not have.
//
// `panelSpecs` is twelve lines that decide which of the entity's eleven
// columns cross into the dashboard writer. It was written without one, because
// the fake syncer discarded its argument — so the projection could have copied
// three columns, or copied PromQL into Legend, or dropped rows, and every test
// in this file would still have passed. Only the count of calls was ever
// asserted.
//
// Both halves matter and they fail differently. The five columns that must NOT
// cross are the ones that make the boundary wider every time somebody adds a
// column to the entity; the six that must cross are the ones the dashboard is
// made of. A guard that only checked the second would pass on a projection
// that also carried last_sync_at, which is a column whose value is the answer
// to the question the mirror is being asked.
func TestPanelSpecsCopiesExactlyTheSixColumnsADashboardReads(t *testing.T) {
	now := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	rows := []*model.Panel{
		{
			ID: 7, Title: "CPU", Type: model.PanelTypeTimeseries,
			PromQL: "cpu_pct", Legend: "{{device_id}}", Unit: "percent",
			Ordinal:       3,
			LastSyncError: "grafana down",
			LastSyncAt:    &now,
			UpdatedAt:     now,
			CreatedAt:     now,
		},
		{ID: 8, Title: "Mem", Type: model.PanelTypeStat, PromQL: "mem_pct", Unit: "percent"},
	}

	got := panelSpecs(rows)
	if len(got) != 2 {
		t.Fatalf("panelSpecs returned %d specs, want 2", len(got))
	}

	// The six, each pinned. A projection that swaps two columns compiles, and
	// a dashboard that shows one series labelled with another's unit is not a
	// failure anybody would trace back to here.
	want := domain.MonitorPanelSpec{
		ID: 7, Title: "CPU", Type: model.PanelTypeTimeseries,
		PromQL: "cpu_pct", Legend: "{{device_id}}", Unit: "percent",
	}
	if got[0] != want {
		t.Errorf("spec 0 = %+v, want %+v. The renderer reads all six and a swap "+
			"between any two of them produces a dashboard that is wrong rather than broken",
			got[0], want)
	}

	// The five, by absence. They cannot be asserted by value because the spec
	// has nowhere to put them, which is the property being relied on: the
	// type has six fields, so there is no column to widen into. The test that
	// says so is TestMonitorPanelSpecHasExactlySixFields in core/domain.
	if got[1].Legend != "" || got[1].Unit != "percent" {
		t.Errorf("spec 1 = %+v; a row with an empty legend must project an empty legend, "+
			"not a neighbour's", got[1])
	}
}

// TestPanelSpecsDropsNilRows says what happens to a hole in the slice.
//
// The entity slice is []*Panel, so a nil is representable; the spec slice is
// []MonitorPanelSpec, so it is not. The translation has to decide, and
// deciding by panicking would be a new failure mode on data that a store
// query cannot currently produce.
func TestPanelSpecsDropsNilRows(t *testing.T) {
	got := panelSpecs([]*model.Panel{nil, {ID: 1, Title: "only"}, nil})
	if len(got) != 1 || got[0].ID != 1 {
		t.Errorf("panelSpecs([nil, row, nil]) = %+v, want the one row", got)
	}
	if panelSpecs(nil) != nil {
		t.Error("panelSpecs(nil) should stay nil: the syncer appends to a slice of its own " +
			"core panels, and an empty non-nil slice is not the same thing to a caller " +
			"that checks len() == 0 against a nil one that checks == nil")
	}
}

// TestPanelSpecsSetsEveryFieldOfTheSpec is the guard on the direction the
// field-count guard cannot see.
//
// core/domain asserts that MonitorPanelSpec has exactly six fields. That stops
// somebody *adding* a seventh and re-widening the boundary. It cannot stop the
// opposite failure, which is the one that actually happens: somebody adds a
// column to the panel entity, decides the dashboard should show it, adds a
// seventh field to the spec — and the spec guard goes red, so they fix the
// spec — and then the projection in panelSpecs is never updated, so the field
// is silently empty and the dashboard shows a blank. Nothing breaks. The
// dashboard is just wrong, and the wrongness is a missing value in a struct
// literal, which is the single most common way a projection rots.
//
// So the projection is read rather than run. The composite literal inside
// panelSpecs is parsed and its keys are compared against the spec's fields
// with reflection. A field the spec has and the literal does not set is a
// field the dashboard will render as its zero value.
func TestPanelSpecsSetsEveryFieldOfTheSpec(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "service.go", nil, 0)
	if err != nil {
		t.Fatalf("parse service.go: %v", err)
	}

	// The keys the projection sets, gathered from every
	// domain.MonitorPanelSpec{...} literal in the file.
	set := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		lit, isLit := n.(*ast.CompositeLit)
		if !isLit {
			return true
		}
		sel, isSel := lit.Type.(*ast.SelectorExpr)
		if !isSel || sel.Sel.Name != "MonitorPanelSpec" {
			return true
		}
		// A qualified type means the same struct only if the qualifier is the
		// domain package; the local test package declares its own.
		if id, isIdent := sel.X.(*ast.Ident); !isIdent || id.Name != "domain" {
			return true
		}
		for _, elt := range lit.Elts {
			kv, isKV := elt.(*ast.KeyValueExpr)
			if !isKV {
				continue
			}
			if key, isKey := kv.Key.(*ast.Ident); isKey {
				set[key.Name] = true
			}
		}
		return true
	})
	if len(set) == 0 {
		t.Fatal("no domain.MonitorPanelSpec composite literal was found in service.go, so " +
			"this guard is looking at nothing — the projection was probably renamed")
	}

	typ := reflect.TypeOf(domain.MonitorPanelSpec{})
	var missing []string
	for i := 0; i < typ.NumField(); i++ {
		if name := typ.Field(i).Name; !set[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) != 0 {
		t.Errorf("panelSpecs does not set %v, and MonitorPanelSpec has %d fields. "+
			"An unset field is not a compile error, not a test failure, and not visible "+
			"in the dashboard JSON — it is a blank the renderer fills with a zero value, "+
			"which is the shape of bug that gets reported as 'the Grafana panel is blank' "+
			"and traced to Grafana", missing, typ.NumField())
	}

	// And the other direction: a key the spec does not have is a column that
	// would have to be dropped for this to compile, so it cannot happen here —
	// but saying so costs one line and catches the day someone "fixes" the
	// build by adding the field to the spec instead of the literal.
	fields := map[string]bool{}
	for i := 0; i < typ.NumField(); i++ {
		fields[typ.Field(i).Name] = true
	}
	var extra []string
	for name := range set {
		if !fields[name] {
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	if len(extra) != 0 {
		t.Errorf("panelSpecs sets %v, which MonitorPanelSpec does not declare. That "+
			"compiles only if the field was added to the spec in the same edit — and the "+
			"right fix for a column the dashboard should read is to add it to the spec "+
			"AND keep the six-field guard honest, not to widen the projection", extra)
	}
}
