package main

import (
	"context"
	"errors"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/manager/dataguard"
)

// The adapter is the only place that holds both sides of this control: the
// label store is dataguard's, the class is approval's, and neither domain may
// import the other. That makes it the one place a mistake turns into "every
// approval in this deployment is classified by its proposer alone" — with no
// compile error and no boot log line to contradict it.

// stubLabeler answers per resource id, and remembers what it was asked.
type stubLabeler struct {
	byTarget map[string]dataguard.Sensitivity
	err      error
	asked    []string
}

func (s *stubLabeler) StrictestForResourceID(_ context.Context, resourceID string) (dataguard.Sensitivity, bool, error) {
	s.asked = append(s.asked, resourceID)
	if s.err != nil {
		return "", false, s.err
	}
	v, ok := s.byTarget[resourceID]
	return v, ok, nil
}

// 一个 Restricted 的目标把一次读提升成 destructive。
func TestTheAdapterRaisesReadToDestructiveOnARestrictedTarget(t *testing.T) {
	l := &stubLabeler{byTarget: map[string]dataguard.Sensitivity{
		"7": dataguard.Restricted,
	}}
	e := newSensitivityEscalator(l, nil)

	got, labelled, err := e.ClassFor(context.Background(), []string{"7"})
	if err != nil || !labelled {
		t.Fatalf("ClassFor: got=%q labelled=%v err=%v", got, labelled, err)
	}
	if got != domain.ClassDestructive {
		t.Errorf("Restricted → got %q，应为 destructive", got)
	}
}

// 没打标的目标不表态。调用方自己知道提议的类别，不该由适配器替它降级。
func TestTheAdapterSaysNothingAboutAnUnlabelledTarget(t *testing.T) {
	l := &stubLabeler{byTarget: map[string]dataguard.Sensitivity{}}
	e := newSensitivityEscalator(l, nil)

	got, labelled, err := e.ClassFor(context.Background(), []string{"7"})
	if err != nil || labelled || got != "" {
		t.Errorf("未打标应不表态：got=%q labelled=%v err=%v", got, labelled, err)
	}
}

// 查不到必须传上去，不能当成"没标签"。吞掉它等于让一次数据库抖动变成一张
// 少一个签名的审批单。
func TestTheAdapterPropagatesALookupFailure(t *testing.T) {
	l := &stubLabeler{err: errors.New("db down")}
	e := newSensitivityEscalator(l, nil)

	got, labelled, err := e.ClassFor(context.Background(), []string{"7"})
	if err == nil {
		t.Fatal("查询失败被吞掉了")
	}
	if labelled || got != "" {
		t.Errorf("失败时不该返回类别：got=%q labelled=%v", got, labelled)
	}
}

// 十二台设备里那台被标了 Restricted 的，只要排在第二位就会被一个只看第一个的
// 实现放过去。适配器这一层必须自己取最严。
func TestTheAdapterTakesTheStrictestAcrossTargets(t *testing.T) {
	l := &stubLabeler{byTarget: map[string]dataguard.Sensitivity{
		"1": dataguard.Public,
		"2": dataguard.Restricted,
		"3": dataguard.Confidential,
	}}
	e := newSensitivityEscalator(l, nil)

	got, labelled, err := e.ClassFor(context.Background(), []string{"1", "2", "3"})
	if err != nil || !labelled {
		t.Fatalf("ClassFor: got=%q labelled=%v err=%v", got, labelled, err)
	}
	if got != domain.ClassDestructive {
		t.Errorf("最严的 Restricted 应压过 Confidential 与 Public，got=%q", got)
	}
}

// Public 与 Internal 不表态：它们的意思是"工具自己的类别算数"，不是"降级"。
// 一个把它们当成 Internal 的实现，会让 Confidential 标签的升级在这里被吞掉。
func TestTheAdapterDoesNotTreatMildLabelsAsAnEscalation(t *testing.T) {
	l := &stubLabeler{byTarget: map[string]dataguard.Sensitivity{
		"1": dataguard.Public,
		"2": dataguard.Internal,
	}}
	e := newSensitivityEscalator(l, nil)

	got, labelled, err := e.ClassFor(context.Background(), []string{"1", "2"})
	if err != nil || labelled || got != "" {
		t.Errorf("Public/Internal 不该要求升级：got=%q labelled=%v err=%v", got, labelled, err)
	}
}

// 没有标签库时适配器是 nil，而不是一个"什么都不做"的对象——区别在于调用方
// 能看出这条控制线在本部署里根本不存在，而不是以为它跑过了。
func TestTheAdapterIsAbsentWithoutALabelStore(t *testing.T) {
	if got := newSensitivityEscalator(nil, nil); got != nil {
		t.Errorf("没有标签库时返回了 %v，应为 nil", got)
	}
}
