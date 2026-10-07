package device

import (
	"reflect"
	"testing"
)

// Which edge owns a device *as its host* is this domain's own fact, and the
// arity of LookupEdgeForDevice is where that fact is kept.
//
// Measured before decision 248: the HTTP handler that opens a shell declared
// its own port as `LookupEdgeForDevice(ctx, deviceID, t EdgeDeviceRelationType)`
// and passed Host on its one call site, and the test beside it asserted that
// the value arriving was Host. The parameter was a fact wearing a parameter's
// clothes, and it forced every caller to restate a decision this package had
// already made.
//
// Two callers now use the two-argument form: this package's own usecase
// resolves the host relation internally, and biz/aiops/tools/database asks
// the same question of a device and of each of its replicas
// (analyze_database_status.go). The arity is therefore the shared shape
// rather than one consumer's preference, which is why it is worth pinning
// here — this package is where the fact lives, so this is where the pin
// belongs.
//
// It also has to live here to be measurable at all. The mutation that puts
// the relation back is a one-line change to this file's package, and it
// breaks two call sites in biz/aiops/tools/database; a test for it placed in
// any of those packages cannot run, because the mutation takes their
// compilation down with it. A guard that cannot be exercised is a comment.

func TestTheDeviceUsecaseKeepsTheRelationToItself(t *testing.T) {
	method, ok := reflect.TypeOf((*Usecase)(nil)).MethodByName("LookupEdgeForDevice")
	if !ok {
		t.Fatal("the device usecase has no LookupEdgeForDevice, so every caller that asks " +
			"which edge owns a device has nothing to call")
	}
	// Three is the receiver plus two declared parameters. A third declared
	// parameter is the relation type, and it would be this package's own fact
	// handed back to callers that never had a choice about it.
	if got := method.Type.NumIn(); got != 3 {
		t.Errorf("LookupEdgeForDevice takes %d parameters, want 2. Which relation a lookup "+
			"means is decided here — this method resolves Host — and a relation parameter "+
			"puts that decision back in the hands of every caller, one of which already "+
			"had to be taught to pass a constant",
			got-1)
	}
}

// TestTheJunctionPortKeepsTheRelationBecauseThatIsWhereItIsStored is the
// other half, and it is the half that is allowed to take a relation.
//
// The junction row has a type column with two values, so the store genuinely
// does have two questions to answer and this package genuinely does have two
// callers of them. Keeping the parameter here — and only here — is what makes
// the usecase's two-argument method possible, and it is the reason the
// earlier assertion is a decision rather than a coincidence.
func TestTheJunctionPortKeepsTheRelationBecauseThatIsWhereItIsStored(t *testing.T) {
	method, ok := reflect.TypeOf((*EdgeDeviceRepo)(nil)).Elem().MethodByName("LookupEdgeForDevice")
	if !ok {
		t.Fatal("the junction port has no LookupEdgeForDevice")
	}
	// Three declared parameters: the context, the device id and the relation.
	// Narrowing this one is not this decision's business — it is the storage
	// shape, and two of its questions are real. (reflect reports an interface
	// method without the receiver, unlike the usecase above.)
	if got := method.Type.NumIn(); got != 3 {
		t.Errorf("the junction's LookupEdgeForDevice takes %d parameters, want 3 (context, "+
			"device id, relation). If this is being narrowed, the two relations it really "+
			"answers are being collapsed, and the assertion above is guarding a shape "+
			"that no longer exists underneath it", got)
	}
}
