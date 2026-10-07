package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 这一组测试守的是决策 303：**prod 注入需要第二个人**。
//
// `--confirm-prod` 是一个布尔开关，而一个布尔开关的签发者与检查者是同一个人。
// 补上的三样里，**第二个人**是全部要害，其余两样（记录、签名）存在的意义
// 只是让"把 approved_by 改成另一个人"这件事改完就立刻暴露。

// twoPersonKeys 造一个密钥目录，放 alice 与 bob 两个身份。
func twoPersonKeys(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for id, secret := range map[string]string{"alice": "alice-secret-do-not-share", "bob": "bob-secret-do-not-share"} {
		if err := os.WriteFile(filepath.Join(dir, id+".key"), []byte(secret), 0o600); err != nil {
			t.Fatalf("write %s key: %v", id, err)
		}
	}
	return dir
}

// signedRecord 造一条 alice 请求、bob 批准、且**用 bob 的密钥签过**的记录。
func signedRecord(t *testing.T, keysDir string) ApprovalRecord {
	t.Helper()
	rec := ApprovalRecord{
		Case:        longCase,
		Env:         "prod",
		RequestedBy: "alice",
		ApprovedBy:  "bob",
		ApprovedAt:  time.Now().Add(-5 * time.Minute),
		Note:        "已在变更窗口内与值班同学确认",
	}
	key, err := loadApprovalKey(keysDir, "bob")
	if err != nil {
		t.Fatalf("load bob key: %v", err)
	}
	if err := rec.Sign(key); err != nil {
		t.Fatalf("sign: %v", err)
	}
	return rec
}

// writeRecord 把记录落盘并返回路径。
func writeRecord(t *testing.T, rec ApprovalRecord) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "approval.json")
	blob, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatalf("write record: %v", err)
	}
	return path
}

func aliceRequest() approvalRequest {
	return approvalRequest{caseID: longCase, env: "prod", operator: "alice"}
}

// --confirm-prod 不再是充分条件：它只是一个布尔开关。
func TestProdRefusesWithoutAnApprovalRecord(t *testing.T) {
	pinNoBackend(t)
	t.Setenv(operatorEnv, "alice")
	_, err := runInject(t,
		"--case", longCase, "--cases-dir", shippedCasesDir,
		"--env", "prod", "--confirm-prod")
	if err == nil {
		t.Fatal("prod injection with only --confirm-prod returned nil; the boolean is still " +
			"being accepted as two-person approval")
	}
	if !strings.Contains(err.Error(), "--approval") {
		t.Fatalf("error = %q, want it to point at --approval", err)
	}
}

// 不知道操作者是谁，就没法判断审批人是不是第二个人——所以缺身份必须拒绝。
//
// 这一条看起来是"配置问题"，但它挡的正是"随便设一个 OPSKEEPER_HARNESS_OPERATOR
// 就过去"这件事：没有这个环境变量，闸门是关着的，不是开的。
func TestProdRefusesWhenTheOperatorIsUnknown(t *testing.T) {
	pinNoBackend(t)
	t.Setenv(operatorEnv, "")
	keys := twoPersonKeys(t)
	_, err := runInject(t,
		"--case", longCase, "--cases-dir", shippedCasesDir,
		"--env", "prod", "--confirm-prod",
		"--approval", writeRecord(t, signedRecord(t, keys)), "--approval-keys", keys)
	if err == nil {
		t.Fatal("prod injection with no known operator returned nil")
	}
	if !strings.Contains(err.Error(), operatorEnv) {
		t.Fatalf("error = %q, want it to name %s", err, operatorEnv)
	}
}

// **全部要害**：同一个人既发起又批准。
func TestProdRefusesWhenTheSamePersonRequestedAndApproved(t *testing.T) {
	pinNoBackend(t)
	t.Setenv(operatorEnv, "alice")
	keys := twoPersonKeys(t)
	rec := signedRecord(t, keys)
	rec.ApprovedBy = "alice"
	key, err := loadApprovalKey(keys, "alice")
	if err != nil {
		t.Fatalf("load alice key: %v", err)
	}
	if err := rec.Sign(key); err != nil {
		t.Fatalf("sign: %v", err)
	}
	_, err = runInject(t,
		"--case", longCase, "--cases-dir", shippedCasesDir,
		"--env", "prod", "--confirm-prod",
		"--approval", writeRecord(t, rec), "--approval-keys", keys)
	if err == nil {
		t.Fatal("a self-approved prod injection returned nil")
	}
	if !strings.Contains(err.Error(), "both requested and approved") {
		t.Fatalf("error = %q, want it to say the approver is the requester", err)
	}
}

// 记录必须**绑定到这一件事**。一张没绑定的批条批的是所有东西。
func TestProdRefusesARecordForADifferentCase(t *testing.T) {
	pinNoBackend(t)
	t.Setenv(operatorEnv, "alice")
	keys := twoPersonKeys(t)
	rec := signedRecord(t, keys)
	rec.Case = "pg/table-bloat"
	_, err := runInject(t,
		"--case", longCase, "--cases-dir", shippedCasesDir,
		"--env", "prod", "--confirm-prod",
		"--approval", writeRecord(t, rec), "--approval-keys", keys)
	if err == nil {
		t.Fatal("an approval for a different case was accepted")
	}
	if !strings.Contains(err.Error(), "pg/table-bloat") {
		t.Fatalf("error = %q, want it to name the case the record is actually for", err)
	}
}

func TestProdRefusesARecordForADifferentEnv(t *testing.T) {
	pinNoBackend(t)
	t.Setenv(operatorEnv, "alice")
	keys := twoPersonKeys(t)
	rec := signedRecord(t, keys)
	rec.Env = "staging"
	_, err := runInject(t,
		"--case", longCase, "--cases-dir", shippedCasesDir,
		"--env", "prod", "--confirm-prod",
		"--approval", writeRecord(t, rec), "--approval-keys", keys)
	if err == nil {
		t.Fatal("an approval for staging was accepted for a prod injection")
	}
}

// 时效：一条半年前的"是"不该还能用。
func TestProdRefusesAStaleApproval(t *testing.T) {
	pinNoBackend(t)
	t.Setenv(operatorEnv, "alice")
	keys := twoPersonKeys(t)
	rec := signedRecord(t, keys)
	rec.ApprovedAt = time.Now().Add(-90 * time.Minute)
	rec.ExpiresAt = rec.ApprovedAt.Add(time.Hour)
	key, _ := loadApprovalKey(keys, "bob")
	if err := rec.Sign(key); err != nil {
		t.Fatalf("sign: %v", err)
	}
	_, err := runInject(t,
		"--case", longCase, "--cases-dir", shippedCasesDir,
		"--env", "prod", "--confirm-prod",
		"--approval", writeRecord(t, rec), "--approval-keys", keys)
	if err == nil {
		t.Fatal("a 90-minute-old approval was accepted under a 1h ceiling")
	}
	if !strings.Contains(err.Error(), "ceiling") {
		t.Fatalf("error = %q, want it to name the age ceiling", err)
	}
}

// 记录自己声称的到期时刻**不被信任到超过 --approval-max-age**。
//
// 一条写着"有效期到明年"的记录，与一条写着"有效期一小时"的记录，
// 在没有上限时是同一样东西。
func TestARecordCannotOutliveTheMaxAgeByClaimingSo(t *testing.T) {
	pinNoBackend(t)
	t.Setenv(operatorEnv, "alice")
	keys := twoPersonKeys(t)
	rec := signedRecord(t, keys)
	rec.ApprovedAt = time.Now().Add(-2 * time.Hour)
	rec.ExpiresAt = rec.ApprovedAt.Add(365 * 24 * time.Hour)
	key, _ := loadApprovalKey(keys, "bob")
	if err := rec.Sign(key); err != nil {
		t.Fatalf("sign: %v", err)
	}
	_, err := runInject(t,
		"--case", longCase, "--cases-dir", shippedCasesDir,
		"--env", "prod", "--confirm-prod",
		"--approval", writeRecord(t, rec), "--approval-keys", keys)
	if err == nil {
		t.Fatal("a record that declares a one-year validity overrode the operator's ceiling")
	}
}

// **签完再改 approved_by 必须立刻暴露。**
//
// 这是"留痕"这两个字唯一有意义的检验方式：改字段 → 签名对不上。
func TestTamperingWithASignedRecordIsCaught(t *testing.T) {
	pinNoBackend(t)
	t.Setenv(operatorEnv, "alice")
	keys := twoPersonKeys(t)
	rec := signedRecord(t, keys)
	rec.ApprovedBy = "carol" // 没签过 carol 的名字
	_, err := runInject(t,
		"--case", longCase, "--cases-dir", shippedCasesDir,
		"--env", "prod", "--confirm-prod",
		"--approval", writeRecord(t, rec), "--approval-keys", keys)
	if err == nil {
		t.Fatal("a record edited after signing was accepted")
	}
	// carol 没有密钥，所以这里先撞上的是"我们无法确认是谁签的"——
	// 两条路都拒绝，而这一条更早、更可行动。
	if !strings.Contains(err.Error(), "carol") {
		t.Fatalf("error = %q, want it to name the identity whose key is missing", err)
	}
}

// **拿自己的密钥签一份写着别人名字的记录，必须被拒。**
//
// 这一条才是 HMAC 那部分的全部意义：签名不是"有人签过"，而是
// "**这个人**签过"。少了它，改一个名字就够了。
func TestASignatureStolenFromAnotherKeyIsRejected(t *testing.T) {
	pinNoBackend(t)
	t.Setenv(operatorEnv, "alice")
	keys := twoPersonKeys(t)
	rec := signedRecord(t, keys)
	// 用 alice 的密钥重签一份，名字仍然写 bob。
	aliceKey, err := loadApprovalKey(keys, "alice")
	if err != nil {
		t.Fatalf("load alice key: %v", err)
	}
	if err := rec.Sign(aliceKey); err != nil {
		t.Fatalf("sign: %v", err)
	}
	// 绕过 runInject 直接打 checkProdApproval：走命令行时 bob 的密钥存在，
	// 会先撞上"签名对不上"，而这里要验的是**同一段逻辑在函数层也成立**。
	if err := checkProdApproval(aliceRequest(), writeRecord(t, rec), keys,
		time.Now(), defaultApprovalMaxAge); err == nil {
		t.Fatal("a record signed with alice's key but claiming bob approved it was accepted")
	}
}

// 一条**真的**双人记录必须过得去——否则这道门是"谁也过不去"的门。
//
// 判据是失败必须来自**别的地方**（没有后端），而不是来自审批。
func TestAProperTwoPersonRecordPassesTheApprovalGate(t *testing.T) {
	pinNoBackend(t)
	t.Setenv(operatorEnv, "alice")
	keys := twoPersonKeys(t)
	out, err := runInject(t,
		"--case", longCase, "--cases-dir", shippedCasesDir,
		"--env", "prod", "--confirm-prod",
		"--approval", writeRecord(t, signedRecord(t, keys)), "--approval-keys", keys)
	if err == nil {
		t.Fatal("expected the missing backend to be reported")
	}
	if strings.Contains(err.Error(), "approval") {
		t.Fatalf("a properly signed two-person record still failed the approval gate: %v", err)
	}
	if strings.Contains(out, "inject_id=") {
		t.Fatalf("something was staged despite the missing backend; output:\n%s", out)
	}
}

// 审批校验必须在**碰目标环境之前**，与决策 302 的时间窗同一个顺序。
func TestTheApprovalGateRunsBeforeTheEnvironmentIsTouched(t *testing.T) {
	pinNoBackend(t)
	t.Setenv(operatorEnv, "alice")
	keys := twoPersonKeys(t)
	rec := signedRecord(t, keys)
	rec.Env = "staging" // 错的那一条
	out, err := runInject(t,
		"--case", longCase, "--cases-dir", shippedCasesDir,
		"--env", "prod", "--confirm-prod",
		"--approval", writeRecord(t, rec), "--approval-keys", keys)
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if strings.Contains(out, "inject_id=") || strings.Contains(out, "inject: case=") {
		t.Fatalf("the approval gate ran after the command started working; output:\n%s", out)
	}
}

// **签名的字节序列必须无歧义。**
//
// 用分隔符裸拼字段的话，`case="a", env="bc"` 与 `case="ab", env="c"`
// 会算出同一个摘要——那正是"一条记录批了另一件事"，只是发生在字节层面。
// 每段带长度就是为了关掉这条路。
func TestCanonicalApprovalIsUnambiguous(t *testing.T) {
	at := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	a := ApprovalRecord{Case: "a", Env: "bc", RequestedBy: "alice", ApprovedBy: "bob", ApprovedAt: at}
	b := ApprovalRecord{Case: "ab", Env: "c", RequestedBy: "alice", ApprovedBy: "bob", ApprovedAt: at}
	key := []byte("k")
	_ = a.Sign(key)
	_ = b.Sign(key)
	if a.HMAC == b.HMAC {
		t.Fatal("two different records produced the same signature; the canonical form is " +
			"ambiguous and one approval can be replayed as another")
	}
	// 改任何一个字段都必须改掉签名。
	//
	// 比的是**重新签一次**的结果：记录里存的那个 HMAC 在 mutate 之后当然不变
	// （它就是被改坏的那个旧值），而"它验不过"正是靠它与新算出来的值
	// 不相等来表达的。第一版比的是 mutate 前后那个陈旧的字段，
	// 于是它对每一个字段都成立——一条恒为真的断言。
	for _, mutate := range []struct {
		name string
		fn   func(*ApprovalRecord)
	}{
		{"case", func(r *ApprovalRecord) { r.Case += "x" }},
		{"env", func(r *ApprovalRecord) { r.Env += "x" }},
		{"requested_by", func(r *ApprovalRecord) { r.RequestedBy = "carol" }},
		{"approved_by", func(r *ApprovalRecord) { r.ApprovedBy = "carol" }},
		{"note", func(r *ApprovalRecord) { r.Note += "x" }},
		{"expires_at", func(r *ApprovalRecord) { r.ExpiresAt = r.ApprovedAt.Add(2 * time.Hour) }},
	} {
		rec := a
		rec.HMAC = ""
		if err := rec.Sign(key); err != nil {
			t.Fatalf("%s: sign: %v", mutate.name, err)
		}
		before := rec.HMAC
		mutate.fn(&rec)
		rec.HMAC = ""
		if err := rec.Sign(key); err != nil {
			t.Fatalf("%s: re-sign: %v", mutate.name, err)
		}
		if rec.HMAC == before {
			t.Errorf("changing %s left the signature unchanged; that field is not covered, so "+
				"a signed record can be edited without anyone noticing", mutate.name)
		}
	}
}

// `approve`  refuses 给自己的请求签字——它自己就把这一条做掉，
// 而不是等到 inject 的时候才发现。
func TestApproveRefusesToSignForYourself(t *testing.T) {
	keys := twoPersonKeys(t)
	out := filepath.Join(t.TempDir(), "rec.json")
	err := cmdApprove([]string{
		"--case", longCase, "--env", "prod",
		"--request-by", "alice", "--approve-as", "alice",
		"--approval-keys", keys, "--out", out,
	})
	if err == nil {
		t.Fatal("approve signed a record where requester == approver")
	}
	if !strings.Contains(err.Error(), "two-person") {
		t.Fatalf("error = %q, want it to explain what two-person approval means", err)
	}
	if _, serr := os.Stat(out); !os.IsNotExist(serr) {
		t.Fatal("approve wrote a file even though it refused to sign it")
	}
}

// `approve` 签出来的东西必须真的过得了 inject 那道门——
// 否则这是一道谁也过不去的门，而它看起来是实现了的。
func TestApproveProducesARecordThatVerifies(t *testing.T) {
	keys := twoPersonKeys(t)
	out := filepath.Join(t.TempDir(), "rec.json")
	if err := cmdApprove([]string{
		"--case", longCase, "--env", "prod",
		"--request-by", "alice", "--approve-as", "bob",
		"--approval-keys", keys, "--note", "变更窗口内已确认", "--out", out,
	}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if err := checkProdApproval(aliceRequest(), out, keys, time.Now(), defaultApprovalMaxAge); err != nil {
		t.Fatalf("a record produced by `approve` did not pass the gate: %v", err)
	}
}

// 审批人自己的密钥不在目录里 → 拒绝，并且说清是"我们无法确认是谁签的"，
// 而不是"签名无效"：两者的处置不同，前者是把闸门接上，后者是记录坏了。
func TestAMissingApproverKeyIsNotReportedAsABadSignature(t *testing.T) {
	keys := twoPersonKeys(t)
	rec := signedRecord(t, keys)
	path := writeRecord(t, rec)
	// 把 bob 的密钥挪走，只留 alice 的。
	if err := os.Remove(filepath.Join(keys, "bob.key")); err != nil {
		t.Fatalf("remove bob key: %v", err)
	}
	err := checkProdApproval(aliceRequest(), path, keys, time.Now(), defaultApprovalMaxAge)
	if err == nil {
		t.Fatal("a record whose approver key is absent was accepted")
	}
	if !strings.Contains(err.Error(), "无法确认") {
		t.Fatalf("error = %q, want it to say we cannot confirm who signed it", err)
	}
	if strings.Contains(err.Error(), "does not verify") {
		t.Fatalf("error = %q, want a missing key reported as a missing key, not as a bad signature",
			err)
	}
}
