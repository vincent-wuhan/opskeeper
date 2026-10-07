package dataguard

import "github.com/vincent-wuhan/opskeeper/core/domain"

// RequiredClass is the tool class a resource's label demands.
//
// This is the mapping the vocabulary has been promising since before any code
// implemented it, and the reason it lives here rather than in the hitl
// package that used to hold a copy is that two copies of a security mapping
// are one copy too many: the day they disagree, one of them is the one an
// operator is reading.
//
// The direction is deliberately one-way and deliberately loud. A label can
// only ever raise the class, never lower it, so a resource nobody has
// classified cannot make an action look safer than the tool that proposed it,
// and a mislabelled Internal cannot suppress the escalation a Restricted
// resource deserves.
//
// The thresholds:
//
//	Public / Internal   → no opinion; the tool's own class stands
//	Confidential        → write      (a read is fine; a change needs a human)
//	Restricted          → destructive
//	TopSecret           → destructive, and the row is unreadable anyway
//
// The last clause is the one that matters most: TopSecret is not "destructive
// if you catch it", it is a resource whose reader tier nobody holds by
// accident, so the write is the least of what the label buys.
func RequiredClass(s Sensitivity) (domain.ToolClass, bool) {
	switch s {
	case Confidential:
		return domain.ClassWrite, true
	case Restricted, TopSecret:
		return domain.ClassDestructive, true
	default:
		// Public, Internal, and anything unparseable. "Anything
		// unparseable" is the safe half of this: an unknown level is
		// treated as no opinion rather than as the highest one, because
		// Parse already refuses to store one and this is the path a value
		// takes when it arrives from somewhere other than the store.
		return "", false
	}
}

// 曾经这里还有一个 RaisedClass(proposed, sensitivity)，它做的是"取更严者"，
// 并且带着一整段关于 ClassUnknown 为什么必须单独分支的说明。
// **它一个生产调用方都没有。** 生产路径是另外两条：
//
//	cmd/opskeeper/sensitivityescalator.go  →  RequiredClass（这里）
//	core/manager/biz/approval.escalate      →  core/domain 的 HighestClass
//
// 后者按架构不能 import dataguard——那正是它必须把自己那份写全的原因，
// 也是决策 363 特意在两处各写一遍 ClassUnknown 分支的原因。
// 于是"两份"变成了"三份"，而第三份只有它自己的测试在调。
//
// 一份只被自己测试证明正确、没有任何调用方用的实现，是这个仓库反复记录的
// 那一类东西：它从包内读像"已实现"，从包外读像不存在。而它自己的注释写着
// "It is the one function callers should use"——一句没有任何人核对过的话。
//
// 删掉它不丢覆盖：等级→类别的映射由 TestEveryLevelThatPromisesAnEscalation
// 守着，"标签只能抬不能压"与 ClassUnknown 那两条由真正执行它们的
// biz/approval 侧用例守着（决策 363 的变异验证证明过：移除那条分支即红）。
//
// 而"不能有第三份"的判据已经变成守卫：TestEveryExportedSymbolHereHasA
// ProductionCaller 会让下一份只被测试调用的导出符号当场失败。
