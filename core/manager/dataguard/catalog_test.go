package dataguard

import "testing"

// 这个文件的断言可以一句话说完：**目录端点返回的每一条，都必须能说出自己在
// 登记表里的下落。**
//
// 16 条控制名里，14 条在整个仓库里只出现在 DefaultFrameworkControls 与登记表的
// 那一行里。一份不带状态的目录一旦发出去，就是同一个谎换了个更硬的外壳——
// 而"这里有 16 条推荐控制项"这句话，比代码注释更难被驳回。

// 目录里不能有条目说不出自己是什么状态。
// 这一条同时是 compliance-claims-check 那道闸门在本包的镜像：unclassified
// 意味着"目录里有、登记表里没有"，而那正是闸门要防的新承诺。
func TestEveryCatalogEntrySaysWhatThisBuildDoesWithIt(t *testing.T) {
	for _, fc := range ControlCatalog() {
		for _, c := range fc.Controls {
			if c.Status == "" {
				t.Errorf("%s/%s: no status", fc.Framework, c.Name)
			}
			if c.Status == ControlUnclassified && c.RegistryRef != "" {
				t.Errorf("%s/%s: unclassified but points at %q — a reference to nothing",
					fc.Framework, c.Name, c.RegistryRef)
			}
			if c.Status != ControlUnclassified && c.RegistryRef == "" {
				t.Errorf("%s/%s: status %q with no registry row to back it",
					fc.Framework, c.Name, c.Status)
			}
		}
	}
}

// 目录覆盖五个框架，且一条不多一条不少。
//
// 「一条不少」是关键的一半：只断言"五个框架都在"的话，一个返回空控制项列表
// 的实现照样通过，而那正是"目录"这个词的全部内容之所在。
func TestTheCatalogCoversEveryFrameworkAndEveryControl(t *testing.T) {
	got := ControlCatalog()
	if len(got) != len(AllFrameworks) {
		t.Fatalf("catalog has %d framework(s), want %d", len(got), len(AllFrameworks))
	}
	defaults := DefaultFrameworkControls()
	for i, fc := range got {
		if fc.Framework != AllFrameworks[i] {
			t.Errorf("framework %d = %q, want %q (the order is read side by side)",
				i, fc.Framework, AllFrameworks[i])
		}
		want := defaults[fc.Framework]
		if len(fc.Controls) != len(want) {
			t.Errorf("%s: %d controls, want %d", fc.Framework, len(fc.Controls), len(want))
			continue
		}
		for j, c := range fc.Controls {
			if c.Name != want[j] {
				t.Errorf("%s: control %d = %q, want %q", fc.Framework, j, c.Name, want[j])
			}
		}
	}
	if !ControlCatalog()[0].Recommended {
		t.Error("the catalog dropped the `recommended` flag; the catalog function's own " +
			"comment says these are not enforced, and that sentence has to survive")
	}
}

// 状态是**算**出来的，不是另写一份的。
//
// 这一条是本文件里唯一能真正抓住"手抄一份"的断言：它自己造一份登记表，
// 把点名某个控制项的那一行从 declared 翻成 enforced，再问一次。如果目录的
// 状态不是从这份登记表算出来的，它会继续给出 declared —— 而任何针对
// 当前树的断言都抓不到这件事，因为手抄的那份与今天的登记表恰好一致。
func TestTheStatusFollowsTheRegistryRatherThanASecondList(t *testing.T) {
	const control = "encryption-at-rest"

	// 今天真实登记表下的答案。
	got, ref := ControlStatusOf(control)
	if ref == "" {
		t.Fatalf("%q is not named by any registry row, so there is nothing to test", control)
	}

	// 造一份同样的登记表，把点名它的那一行翻成 enforced。
	flipped := make([]ControlClaim, 0, len(Claims()))
	turned := false
	for _, claim := range Claims() {
		c := claim
		if c.ID == ref && c.Status != StatusEnforced {
			c.Status = StatusEnforced
			turned = true
		}
		flipped = append(flipped, c)
	}
	if !turned {
		t.Fatalf("no claim naming %q was below enforced, so the flip cannot be observed", control)
	}

	afterStatus, afterRef := ControlStatusFrom(control, flipped)
	if afterStatus != ControlEnforced {
		t.Errorf("after flipping row %q to enforced the control still reports %q (ref %q); "+
			"the status is being restated somewhere, not computed", ref, afterStatus, afterRef)
	}
	if afterRef != ref {
		t.Errorf("registry ref changed to %q when the status changed; the reference has to "+
			"keep pointing at the row that decided it", afterRef)
	}
	if got == ControlEnforced {
		t.Skip("the control is already enforced today; the flip proves nothing")
	}
}

// 没被提到就是没被提到，而且不许编一个引用出来。
func TestAnUnknownControlHasNoStatusAndNoReference(t *testing.T) {
	for _, name := range []string{
		"a-control-nobody-has-written-down",
		"",
		"   ",
	} {
		status, ref := ControlStatusOf(name)
		if status != ControlUnclassified {
			t.Errorf("%q -> %q, want unclassified", name, status)
		}
		if ref != "" {
			t.Errorf("%q -> registry ref %q; nothing names it, so there is nothing to point at",
				name, ref)
		}
	}
}
