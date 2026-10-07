package approval

import (
	"context"
	"errors"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"

	model "github.com/vincent-wuhan/opskeeper/core/manager/model/approval"
)

// These are the tests for the promise the sensitivity vocabulary has been
// making since before there was code for it: a Restricted resource raises a
// call to dangerous. The read half of that promise landed in decision 361
// (SensitivityGate, five call sites). This file is the write half — the one
// that decides how many signatures a row needs — and it was the half with no
// implementation at all: enforcement.go listed `sensitivity.escalates-severity`
// as the last inert row, meaning the sentence existed in a doc comment and a
// status string while the only code that claimed to implement it lived in a
// PausePolicyImpl nobody constructed.

// fakeLabeler answers the only question an Escalator asks: what class do these
// targets' labels demand?
type fakeLabeler struct {
	byTarget map[string]domain.ToolClass
	err      error
	asked    []string
}

func (f *fakeLabeler) ClassFor(_ context.Context, targets []string) (domain.ToolClass, bool, error) {
	f.asked = append(f.asked, targets...)
	if f.err != nil {
		return "", false, f.err
	}
	// The strictest across every target, mirroring what the real adapter does
	// across resource types. An implementation that read the first entry would
	// pass every single-target test below and fail this one.
	worst, found := domain.ClassRead, false
	for _, t := range targets {
		c, ok := f.byTarget[t]
		if !ok {
			continue
		}
		found = true
		if (domain.Tools{{Class: worst}, {Class: c}}).HighestClass() == c {
			worst = c
		}
	}
	if !found {
		return "", false, nil
	}
	return worst, true, nil
}

func escalationUC(t *testing.T, e Escalator) *Usecase {
	t.Helper()
	repo := &countingRepo{rows: map[string]*model.Approval{}}
	return NewUsecase(repo, nil).WithEscalator(e)
}

// A labelled Restricted target raises a write proposal to destructive, and the
// raised class is what gets STORED — not what the caller happened to propose.
func TestALabelledTargetRaisesTheStoredRiskClass(t *testing.T) {
	e := &fakeLabeler{byTarget: map[string]domain.ToolClass{
		"web-1": domain.ClassDestructive,
	}}
	uc := escalationUC(t, e)

	row, err := uc.Propose(context.Background(), ProposeInput{
		Kind: "restart_service", Title: "t", Payload: map[string]string{},
		RiskClass: string(domain.ClassWrite),
		Target:    "web-1",
	})
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if row.RiskClass != string(domain.ClassDestructive) {
		t.Errorf("存储的 risk_class = %q，标签要求 destructive", row.RiskClass)
	}
}

// 一个只查过"标签比提议更严"的实现会通过上面每一条，而在这一条上把一次
// destructive 的重启降成 write。标签只能往上抬，永远不能往下压——否则给生产库
// 打一个 Internal 标签就成了把双签变成单签的办法。
func TestALabelCanNeverLowerTheProposedClass(t *testing.T) {
	e := &fakeLabeler{byTarget: map[string]domain.ToolClass{
		"web-1": domain.ClassWrite, // 比提议的 destructive 宽松
	}}
	uc := escalationUC(t, e)

	row, err := uc.Propose(context.Background(), ProposeInput{
		Kind: "restart_service", Title: "t", Payload: map[string]string{},
		RiskClass: string(domain.ClassDestructive),
		Target:    "web-1",
	})
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if row.RiskClass != string(domain.ClassDestructive) {
		t.Errorf("一个更宽松的标签把 risk_class 降成了 %q", row.RiskClass)
	}
}

// An unlabelled target must not move. A label can only ever raise a class; if
// it could lower one then labelling a production database `Internal` would be
// a way to get a single signature out of an approval that needs two.
func TestAnUnlabelledTargetLeavesTheProposalAlone(t *testing.T) {
	e := &fakeLabeler{byTarget: map[string]domain.ToolClass{}}
	uc := escalationUC(t, e)

	row, err := uc.Propose(context.Background(), ProposeInput{
		Kind: "restart_service", Title: "t", Payload: map[string]string{},
		RiskClass: string(domain.ClassDestructive),
		Target:    "web-1",
	})
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if row.RiskClass != string(domain.ClassDestructive) {
		t.Errorf("未被标注的目标把类别改成了 %q", row.RiskClass)
	}
}

// The one that would have been invisible: a proposal that declared NO class
// parses to ClassUnknown, and core/domain ranks ClassUnknown WITH destructive
// for plugin admission. Run through HighestClass, that outranks the label —
// so the row is stored with an empty risk class, and an empty class on an
// approval row is exactly the one-signature row this path exists to prevent.
// An undeclared proposal has to take the label's word for it.
func TestAnUndeclaredProposalTakesTheLabelsWordForIt(t *testing.T) {
	e := &fakeLabeler{byTarget: map[string]domain.ToolClass{
		"web-1": domain.ClassDestructive,
	}}
	uc := escalationUC(t, e)

	row, err := uc.Propose(context.Background(), ProposeInput{
		Kind: "restart_service", Title: "t", Payload: map[string]string{},
		RiskClass: "", // 生产者什么都没说
		Target:    "web-1",
	})
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if row.RiskClass == "" {
		t.Error("未声明类别的提议在有 Restricted 标签时仍存成空类别")
	}
	if row.RiskClass != string(domain.ClassDestructive) {
		t.Errorf("risk_class = %q，应取标签的 destructive", row.RiskClass)
	}
}

// 标签查不到 != 查出来是没标签。前者是数据库抖动，后者是资源真的没打标。
// 把两者混同，等于让一次数据库抖动变成一张少一个签名的审批单。查不到就必须
// 拒绝建行，而不是放行。
func TestALookupFailureRefusesToCreateTheRow(t *testing.T) {
	e := &fakeLabeler{err: errors.New("label store unavailable")}
	uc := escalationUC(t, e)

	row, err := uc.Propose(context.Background(), ProposeInput{
		Kind: "restart_service", Title: "t", Payload: map[string]string{},
		RiskClass: string(domain.ClassDestructive),
		Target:    "web-1",
	})
	if err == nil {
		t.Fatal("查询失败时不该建成审批行")
	}
	if row != nil {
		t.Errorf("失败时返回了一行：%+v", row)
	}
	if !errors.Is(err, errs.ErrInvalid) {
		t.Errorf("err = %v，应当可判定为 ErrInvalid", err)
	}
}

// 一个动作打到十二台设备时，它的安全性等于其中最敏感的那台。只看第一个的实现
// 会通过所有单目标的用例，然后被一个重新排序的参数列表绕过。
func TestTheStrictestOfManyTargetsWins(t *testing.T) {
	e := &fakeLabeler{byTarget: map[string]domain.ToolClass{
		"web-1":  domain.ClassWrite,
		"db-1":   domain.ClassDestructive,
		"cache-1": domain.ClassRead,
	}}
	uc := escalationUC(t, e)

	row, err := uc.Propose(context.Background(), ProposeInput{
		Kind: "restart_service", Title: "t", Payload: map[string]string{},
		RiskClass: string(domain.ClassRead),
		Target:    "web-1", // 卡片上只显示这一个
		EscalationTargets: []string{
			"web-1", "db-1", "cache-1",
		},
	})
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if row.RiskClass != string(domain.ClassDestructive) {
		t.Errorf("十二台设备里最敏感的一台是 db-1，risk_class = %q", row.RiskClass)
	}
	if row.Target != "web-1" {
		t.Errorf("卡片上的 target 变成了 %q，应当仍是操作员看到的那一个", row.Target)
	}
}

// 没有可升级的目标时，一次查询都不该发生；标了空串的也算没有。
func TestEscalationTargetsSkipsBlanksAndDuplicates(t *testing.T) {
	e := &fakeLabeler{byTarget: map[string]domain.ToolClass{}}
	uc := escalationUC(t, e)

	if _, err := uc.Propose(context.Background(), ProposeInput{
		Kind: "restart_service", Title: "t", Payload: map[string]string{},
		RiskClass: string(domain.ClassRead),
		Target:    "",
		EscalationTargets: []string{
			"  ", "", "   ",
		},
	}); err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if len(e.asked) != 0 {
		t.Errorf("全是空目标时仍去查了标签：%v", e.asked)
	}
}

// 卡片上的 target 为空、但显式列了 EscalationTargets 时，升级看的是后者。
// 这两件事分开，正是为了让"显示一个、判断全部"能同时成立。
func TestEscalationFallsBackToExplicitTargetsNotTheCard(t *testing.T) {
	e := &fakeLabeler{byTarget: map[string]domain.ToolClass{
		"db-1": domain.ClassDestructive,
	}}
	uc := escalationUC(t, e)

	row, err := uc.Propose(context.Background(), ProposeInput{
		Kind: "restart_service", Title: "t", Payload: map[string]string{},
		RiskClass: string(domain.ClassRead),
		EscalationTargets: []string{
			"db-1",
		},
	})
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if row.RiskClass != string(domain.ClassDestructive) {
		t.Errorf("risk_class = %q，显式列出的 db-1 没被看到", row.RiskClass)
	}
}
