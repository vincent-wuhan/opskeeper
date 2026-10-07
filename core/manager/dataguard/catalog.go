package dataguard

import "strings"

// 这个文件回答一个问题：**控制台按下"一键加载"时，它列出来的那一串控制项，
// 这个构建到底强制了哪几条？**
//
// 它存在的理由不是"把目录发出去"这么朴素。DefaultFrameworkControls 是一个
// 16 条控制名的函数，其中 14 条在整个仓库里只出现在那个函数与登记表那一行里，
// 一条只出现在一个表单往返测试里，一条只出现在对外站点上——也就是说，
// **一份不加说明地发出去的目录，就是同一个谎的第二个来源**，只是这次它穿着
// API 的外衣，比注释更难被人驳回。
//
// 所以每一项都带着状态，而状态是从登记表**算**出来的，不是另写一份清单：
// 一个控制项在本构建里是不是被强制，只有 enforcement.go 知道。
// 枚举在这里重写一遍，目录与登记表就会各说各话——而各说各话的那一份，
// 永远是操作员读不到的那一份。
//
// 顺带解决了一个悬案：`compliance.control-catalog` 此前记 declared，理由是
// 「目录函数只被登记表自己调用，没有请求路径提供它」。给目录加状态不是为了让
// 那一行变成 enforced（它的措辞说的是"发出一份推荐控制列表"，此前确实没人发），
// 而是让这份列表在被发出时是**诚实的**。

// ControlStatus 是这个构建对某一条控制项的真实状态。
type ControlStatus string

const (
	// ControlEnforced：登记表里有一行 enforced 且点名了它。
	ControlEnforced ControlStatus = "enforced"
	// ControlDeclared：有行点名了它，但那一行没在说它被强制。
	ControlDeclared ControlStatus = "declared"
	// ControlUnclassified：五个目录里有、登记表里没有。
	//
	// 它不该发生——`make compliance-claims-check` 的
	// TestEveryAdvertisedControlIsClassified 就是为了让它不发生——所以它被
	// 单独列成一个值而不是并进 declared。两者在界面上必须能被区分：
	// declared 是"有人想过，没做"，unclassified 是"没人想过"，
	// 而**后者的下一步是补一行声明，前者的下一步是接线或撤回**。
	ControlUnclassified ControlStatus = "unclassified"
)

// CatalogControl 是一条控制项在本构建里的真实状态。
type CatalogControl struct {
	// Name 是控制项名，与 DefaultFrameworkControls 里写的字面量相同。
	Name string `json:"name"`
	// Status 是本构建对它做了什么。
	Status ControlStatus `json:"status"`
	// RegistryRef 是决定这个状态的那一行登记表 id。
	//
	// unclassified 时为空——**没有行可指**，把一个空引用渲染成一句
	// "见 compliance.enforced-tag" 比不渲染更糟。
	RegistryRef string `json:"registry_ref,omitempty"`
}

// FrameworkCatalog 是一个框架的推荐控制项，以及每一条的真实状态。
type FrameworkCatalog struct {
	Framework Framework        `json:"framework"`
	Controls  []CatalogControl `json:"controls"`
	// Recommended 保留原来的措辞，因为目录函数自己的注释写的就是"不强制"。
	// 它与 Status 是两件事：前者是目录的意图，后者是这个构建的实况。
	Recommended bool `json:"recommended"`
}

// ControlCatalog 是五个框架的完整目录，每一条都带着它在登记表里的下落。
//
// 顺序与 AllFrameworks 一致，目录内部的顺序与 DefaultFrameworkControls 一致：
// 这两个列表被人并排读过，而顺序不同会让"对照着看"这件事变成一件需要
// 动脑的事。
func ControlCatalog() []FrameworkCatalog {
	defaults := DefaultFrameworkControls()
	out := make([]FrameworkCatalog, 0, len(AllFrameworks))
	for _, f := range AllFrameworks {
		fc := FrameworkCatalog{Framework: f, Recommended: true}
		for _, name := range defaults[f] {
			status, ref := ControlStatusOf(name)
			fc.Controls = append(fc.Controls, CatalogControl{
				Name: name, Status: status, RegistryRef: ref,
			})
		}
		out = append(out, fc)
	}
	return out
}

// ControlStatusOf 回答"这一条在本构建里到底是什么状态"，并给出决定它的那一行。
//
// 判据是**登记表点没点名**，所以这一行的答案会随登记表变化而变化，
// 不需要有人记得回来改这里。这是有意的：一个控制项从 declared 变成
// enforced，目录端点会自己开始这么说。
//
// 多行同时点名时 enforced 胜出，且顺序按登记表自身的顺序——
// 一个"被两行提到"的控制项不该因为谁写在前面而得到不同的答案。
func ControlStatusOf(name string) (ControlStatus, string) {
	return ControlStatusFrom(name, Claims())
}

// ControlStatusFrom 是 ControlStatusOf 的可注入版本。
//
// 它单独存在是为了让"状态是算出来的"这句话**可测**：参数化之后，一条测试
// 可以自己造一份登记表、把某一行的状态翻过来，然后断言目录的答案跟着翻。
// 否则那句话只能靠读代码相信——而一个把 16 条状态手抄在目录旁边、常量
// 恰好与今天相同的实现，会一直通过任何针对当前树的断言，直到真有人把
// 某一行的状态改掉的那一天。
func ControlStatusFrom(name string, claims []ControlClaim) (ControlStatus, string) {
	if strings.TrimSpace(name) == "" {
		return ControlUnclassified, ""
	}
	fallbackStatus, fallbackRef := ControlUnclassified, ""
	for _, claim := range claims {
		mentioned := strings.Contains(claim.Claim, name) || strings.Contains(claim.Note, name)
		if !mentioned {
			continue
		}
		if claim.Status == StatusEnforced {
			return ControlEnforced, claim.ID
		}
		if fallbackStatus == ControlUnclassified {
			fallbackStatus, fallbackRef = ControlDeclared, claim.ID
		}
	}
	if fallbackStatus == ControlUnclassified {
		return ControlUnclassified, ""
	}
	return fallbackStatus, fallbackRef
}
