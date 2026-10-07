package hitl

import (
	"reflect"
	"strings"
	"testing"

	hitlmodel "github.com/vincent-wuhan/opskeeper/core/manager/model/hitl"
)

// The hitl proposal table is still one signature wide, and saying so is the
// point: the day it grows a signer list, this goes red and the question
// "does ADR-019 cover this path too" gets asked.
func TestTheHitlProposalTableIsStillSingleSigner(t *testing.T) {
	row := hitlmodel.Proposal{}
	tp := reflect.TypeOf(row)
	for i := 0; i < tp.NumField(); i++ {
		f := tp.Field(i)
		name := strings.ToLower(f.Name)
		if !strings.Contains(name, "approvedby") && !strings.Contains(name, "resumedby") {
			continue
		}
		switch f.Type.Kind() {
		case reflect.Slice, reflect.Array, reflect.Map:
			t.Errorf("hitl.Proposal.%s can hold more than one identity; the approval inbox "+
				"gained one in decision 362 and this path has not", f.Name)
		}
	}
}
