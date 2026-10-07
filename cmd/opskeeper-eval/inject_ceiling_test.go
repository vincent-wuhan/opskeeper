package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/harness/schema"
)

// 这一组测试守的是决策 302：**注入有时间窗**。
//
// 4.2 那一节此前写着"注入时间窗：没有。`--max-duration` 不存在，
// 命令行不限制时长"。那不是一个可以一直挂着的 TODO——
// `host.fill_disk` 会往真磁盘上写真实块，而一个被 `kill -9` 掉的进程
// 不会把它撤回来；kafka 与 rabbitmq 的记录与队列同样活过进程本身。
// 一个没有上限的注入器，在一台运维机上就是一个"忘了关的定时炸弹"。

// 长 case：pg/lock-waits 的 duration 远长于下面这些测试给的秒级上限。
const longCase = "pg/lock-waits"

// runInject 跑一次 cmdInject，并把它的 stdout 抓回来。
//
// 抓 stdout 不是为了好看：下面最重要的一条断言是"拒绝发生在碰环境之前"，
// 而"碰没碰环境"正是靠输出里有没有 inject_id 判的。
func runInject(t *testing.T, args ...string) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	saved := os.Stdout
	os.Stdout = w

	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()

	runErr := cmdInject(context.Background(), args)
	_ = w.Close()
	os.Stdout = saved
	return <-done, runErr
}

// --hold 超过时间窗必须被拒绝。
//
// 这一条量的是**故障真正活着的时间**：进程按住的时候，故障活 max(duration, hold)，
// 而 hold 是唯一由人随手敲出来的那个数。
func TestTheTimeWindowRefusesALongHold(t *testing.T) {
	pinNoBackend(t)
	_, err := runInject(t,
		"--case", longCase, "--cases-dir", shippedCasesDir,
		"--hold", "2h", "--max-duration", "1m")
	if err == nil {
		t.Fatal("--hold 2h against a 1m time window returned nil; the flag is a no-op")
	}
	if !strings.Contains(err.Error(), "--hold") || !strings.Contains(err.Error(), "1m") {
		t.Fatalf("error = %q, want it to name the hold and the ceiling", err)
	}
	if !strings.Contains(err.Error(), "--max-duration") {
		t.Errorf("error = %q, want it to say how to proceed (raise --max-duration)", err)
	}
}

// case 自带的 duration 超过时间窗同样必须被拒绝——
// 那不是人随手敲的数，但它是**故障真正活着的时限**。
func TestTheTimeWindowRefusesACaseThatAsksForTooLong(t *testing.T) {
	pinNoBackend(t)
	_, err := runInject(t,
		"--case", longCase, "--cases-dir", shippedCasesDir, "--max-duration", "1s")
	if err == nil {
		t.Fatal("a case asking for far longer than the time window returned nil")
	}
	if !strings.Contains(err.Error(), "step 1") {
		t.Errorf("error = %q, want it to name the step that is over the window", err)
	}
	if !strings.Contains(err.Error(), "raise --max-duration") {
		t.Errorf("error = %q, want it to say how to proceed", err)
	}
}

// **拒绝必须发生在碰目标环境之前。**
//
// 这一条是这一刀的全部要害：注入是有副作用的。如果先注入再拒绝，
// 命令行返回了非零，而一个刚被写满的磁盘不会因为返回码非零就自己恢复。
// 而"碰没碰过环境"在这里看不出来——所以判据是输出：
// 一个 inject_id 都没有，连 case 头都没打。
func TestTheTimeWindowRefusesBeforeTouchingTheEnvironment(t *testing.T) {
	pinNoBackend(t)
	out, err := runInject(t,
		"--case", longCase, "--cases-dir", shippedCasesDir, "--max-duration", "1s")
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if strings.Contains(out, "inject_id=") {
		t.Fatalf("the refusal came after an injection was staged; output was:\n%s", out)
	}
	if strings.Contains(out, "inject: case=") {
		t.Fatalf("the command printed its header before deciding, so the gate is not the "+
			"first thing it does; output was:\n%s", out)
	}
}

// 时间窗**拒绝而不截断**。
//
// 截断看起来更"好用"，但它会让一条 600s 的 case 悄悄变成 300s 的故障，
// 而 harness 会拿那个 300s 的结果去判一份诊断结论——报告出来的时间与真实
// 发生的时间不一样。这比直接报错坏得多，而它是那种没人会发现的坏。
func TestTheTimeWindowRefusesRatherThanTruncates(t *testing.T) {
	steps := []schema.InjectStep{{Type: "pg.inject_lock_chain", Duration: "10m"}}
	if err := checkInjectCeiling(9*time.Minute, 0, steps); err == nil {
		t.Fatal("a 10m step under a 9m window returned nil")
	}
	if err := checkInjectCeiling(10*time.Minute, 0, steps); err != nil {
		t.Fatalf("a 10m step under a 10m window was refused: %v", err)
	}
	if err := checkInjectCeiling(11*time.Minute, 0, steps); err != nil {
		t.Fatalf("a 10m step under an 11m window was refused: %v", err)
	}
}

// prod 的窗口必须比 staging 严。
//
// 不是"prod 更危险"这种修辞：`host.fill_disk` 在 prod 上写满的是别人的节点，
// 在 staging 上写满的是一块一次性磁盘。
func TestProdHasAStricterTimeWindowThanStaging(t *testing.T) {
	prod, staging := defaultInjectCeiling("prod"), defaultInjectCeiling("staging")
	if prod >= staging {
		t.Fatalf("prod window %s is not stricter than staging %s", prod, staging)
	}
	if prod <= 0 || staging <= 0 {
		t.Fatalf("windows prod=%s staging=%s, want both positive: a zero window would "+
			"refuse every case", prod, staging)
	}
}

// **语料里每一条 case 都必须落在默认窗口之内。**
//
// 这一条是防止有人加一条 2 小时的 case 而没人发现：它读的是真的 case 目录。
func TestEveryShippedCaseFitsInsideTheDefaultTimeWindow(t *testing.T) {
	cases, err := schema.NewLoader(shippedCasesDir).LoadAll()
	if err != nil {
		t.Fatalf("load shipped cases: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("no shipped cases loaded; the loop below would pass on an empty list")
	}
	// 用最宽的那个环境：staging 装不下的话，prod 更装不下。
	ceiling := defaultInjectCeiling("staging")
	for _, c := range cases {
		if len(c.Inject) == 0 {
			continue
		}
		if err := checkInjectCeiling(ceiling, 0, c.Inject); err != nil {
			t.Errorf("case %s does not fit the default %s window: %v", c.ID, ceiling, err)
		}
	}
}

// 时间窗是**默认**而不是**强制**：显式抬高它必须真的抬高，
// 否则这个 flag 就是一个只能拒绝、不能放行的死路。
func TestTheTimeWindowCanBeRaisedDeliberately(t *testing.T) {
	pinNoBackend(t)
	// 抬到 2h：hold 在窗口内，剩下的失败只可能来自环境（没有后端），
	// 而那正是"注入器拒绝"应有的结果。
	_, err := runInject(t,
		"--case", longCase, "--cases-dir", shippedCasesDir,
		"--hold", "2h", "--max-duration", "2h")
	if err == nil {
		t.Fatal("expected the missing backend to be reported, not a nil error")
	}
	if strings.Contains(err.Error(), "time window") {
		t.Fatalf("raising --max-duration to 2h still hit the time window: %v", err)
	}
}
