package dataguard

import (
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/domain"
)

// The mapping the vocabulary promised before any code implemented it. Written
// as one table so a level added to the enum without a row here is a failure
// rather than a silence.
func TestEveryLevelThatPromisesAnEscalationGetsOne(t *testing.T) {
	cases := map[Sensitivity]struct {
		want  domain.ToolClass
		raise bool
	}{
		Public:       {"", false},
		Internal:     {"", false},
		Confidential: {domain.ClassWrite, true},
		Restricted:   {domain.ClassDestructive, true},
		TopSecret:    {domain.ClassDestructive, true},
	}
	// An unknown level is the safe half of "no opinion": Parse refuses to
	// store one, so this is the path a value takes arriving from elsewhere.
	cases[Sensitivity("something-new")] = struct {
		want  domain.ToolClass
		raise bool
	}{"", false}

	for level, want := range cases {
		got, raised := RequiredClass(level)
		if got != want.want || raised != want.raise {
			t.Errorf("RequiredClass(%q) = %q, %v; want %q, %v", level, got, raised, want.want, want.raise)
		}
	}
}

// A label may only ever raise a class. If it could lower one, an Internal
// label on a resource would be enough to turn a destructive tool back into a
// 下面四条曾经在这里：它们测的是一个叫 RaisedClass 的函数，而那个函数
// 一个生产调用方都没有（决策 368）。它们断言的性质没有消失，只是搬到了
// 真正执行它的地方：
//
//	"标签只能抬不能压"          → biz/approval 的 TestALabelCanNeverLowerTheProposedClass
//	"未声明的提议取标签的说法"   → biz/approval 的 TestAnUndeclaredProposalTakesTheLabelsWordForIt
//	"Public/Internal 不表态"    → 本文件上面的 RequiredClass 用例
//	"HighestClass 会吞掉标签"   → escalate() 里那段 ClassUnknown 分支的注释
//
// **一份只被自己测试证明正确、没有任何调用方用的实现，是这个仓库反复记录的
// 那一类东西**：它从包内读像"已实现"，从包外读像不存在。
