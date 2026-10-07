package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 双人审批（决策 303）。
//
// `--confirm-prod` 是一个布尔开关，而一个布尔开关的**签发者与检查者是同一个人**：
// 任何能敲这行命令的人都能自己确认自己。§4.2 此前把这一条记成
// "只有一个布尔开关，没有第二个人、没有审批记录、没有留痕"。
//
// 这一刀补上后三样，缺一不可：
//
//  1. **第二个人**：审批记录的 `approved_by` 必须不等于运行命令的人。
//     这一条是全部要害，其余两条都是为了让这一条**无法靠改一个字段绕过**。
//  2. **审批记录**：一条与「哪一个 case、在哪个环境」绑定的记录。
//     没有绑定，一张预先批好的空白批条可以拿去批任何东西——
//     而审批的价值恰恰在于"批的是这一件事"。
//  3. **留痕且不可事后改写**：记录带一个用**审批人自己的密钥**算出的 HMAC。
//     没有它，任何人拿一份旧记录改掉 `approved_by` 就能自称批过了。
//
// 密钥的来源是 `--approval-keys` 目录里的 `<identity>.key`。它必须是审批人
// **自己**持有的东西：如果发起人也持有对方的密钥，那这道闸门又变回布尔开关了。

// approvalRequest 是"这一次注入"本身。
type approvalRequest struct {
	caseID string
	env    string
	// operator 是运行这条命令的人，从 OPSKEEPER_HARNESS_OPERATOR 读。
	operator string
}

// ApprovalRecord 是一条双人审批记录。
//
// 字段名保持短小写是因为它直接落在一个 JSON 文件里，而那个文件是审批人和
// 发起人之间唯一的传递物——字段名一长，签字那一端就要照着文档抄。
type ApprovalRecord struct {
	Case        string    `json:"case"`
	Env         string    `json:"env"`
	RequestedBy string    `json:"requested_by"`
	ApprovedBy  string    `json:"approved_by"`
	ApprovedAt  time.Time `json:"approved_at"`
	// ExpiresAt 为零时按 approved_at + defaultApprovalAge 算。
	// 记录自己声称的有效期**不被信任到超过 --approval-max-age**——
	// 与决策 302 的时间窗同一个形状：默认值可以被显式抬高，
	// 但抬高是一次被记下来的决定，而不是记录里一个自己写的字段。
	ExpiresAt time.Time `json:"expires_at"`
	Note      string    `json:"note"`
	// HMAC 是 canonicalApproval(除 HMAC 外全部字段) 在审批人密钥下的
	// HMAC-SHA256，十六进制。
	HMAC string `json:"hmac"`
}

// defaultApprovalAge 是 ExpiresAt 为空时的有效期。
const defaultApprovalAge = time.Hour

// defaultApprovalMaxAge 是 --approval-max-age 的默认值。
//
// 它压过记录自己写的 ExpiresAt：一条写着"有效期到明年"的记录与一条
// 写着"有效期一小时"的记录，在没有上限时是同一样东西。
const defaultApprovalMaxAge = time.Hour

// canonicalApproval 拼出被签名的字节序列。
//
// 它必须是**无歧义**的拼接：字段之间要有长度或者分隔符，否则
// `case="a" env="bc"` 与 `case="ab" env="c"` 会算出同一个摘要，
// 而那正是"一条记录批了另一件事"。用 `\x00` 作分隔并显式带上每段的
// 长度，是为了让分隔符出现在内容里也不会造成歧义。
func canonicalApproval(r ApprovalRecord) []byte {
	fields := []string{
		r.Case, r.Env, r.RequestedBy, r.ApprovedBy,
		r.ApprovedAt.UTC().Format(time.RFC3339),
		r.expiry().UTC().Format(time.RFC3339),
		r.Note,
	}
	var b strings.Builder
	for _, f := range fields {
		b.WriteString(strconv.Itoa(len(f)))
		b.WriteByte(':')
		b.WriteString(f)
		b.WriteByte('\x00')
	}
	return []byte(b.String())
}

// expiry 是这条记录真正的到期时刻。
func (r ApprovalRecord) expiry() time.Time {
	if !r.ExpiresAt.IsZero() {
		return r.ExpiresAt
	}
	return r.ApprovedAt.Add(defaultApprovalAge)
}

// Sign 用审批人的密钥给记录签名（写回 r.HMAC）。
func (r *ApprovalRecord) Sign(key []byte) error {
	if len(key) == 0 {
		return fmt.Errorf("signing key is empty")
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(canonicalApproval(*r))
	r.HMAC = hex.EncodeToString(mac.Sum(nil))
	return nil
}

// approvalKeysDir 解析密钥目录。
func approvalKeysDir(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	return os.Getenv(approvalKeysEnv)
}

const approvalKeysEnv = "OPSKEEPER_HARNESS_APPROVAL_KEYS"

// operatorEnv 是"运行这条命令的人"从哪来。
//
// 它是一个环境变量而不是一个 flag：flag 的话，敲命令的人会顺手写上
// 别人的名字，而这道闸门的全部意义就是那个名字不能由他自己填。
const operatorEnv = "OPSKEEPER_HARNESS_OPERATOR"

// loadApprovalKey 读一个身份的密钥。
func loadApprovalKey(keysDir, identity string) ([]byte, error) {
	if keysDir == "" {
		return nil, fmt.Errorf("没有配置审批密钥目录（%s 或 --approval-keys）", approvalKeysEnv)
	}
	if !validIdentity(identity) {
		return nil, fmt.Errorf("identity %q is not a plain identifier; refusing to build a "+
			"key file name out of it", identity)
	}
	raw, err := os.ReadFile(filepath.Join(keysDir, identity+".key"))
	if err != nil {
		// 密钥不在不是"签名无效"，是"我们无法确认是谁签的"——两者的
		// 处置不同：前者是记录坏了，后者是这道闸门没被真正打开。
		return nil, fmt.Errorf("读不到 %s 的审批密钥（我们无法确认这条记录是谁签的）: %w", identity, err)
	}
	key := strings.TrimSpace(string(raw))
	if key == "" {
		return nil, fmt.Errorf("%s 的审批密钥是空的", identity)
	}
	return []byte(key), nil
}

// checkProdApproval 校验一条 prod 注入的双人审批。
//
// 它**拒绝而不降级**：任何一步过不去就是过不去，不会退回到
// "那就算了吧，按没审批处理"——那正好是一个开关能做的事。
func checkProdApproval(req approvalRequest, recordPath, keysDirFlag string, now time.Time, maxAge time.Duration) error {
	if req.operator == "" {
		return fmt.Errorf("%s is not set; without it we cannot tell whether the approver is "+
			"a second person or the person at the keyboard", operatorEnv)
	}
	if recordPath == "" {
		return fmt.Errorf("refusing to inject in prod without --approval <record.json>: " +
			"--confirm-prod is one person saying yes, which is the thing this gate exists to " +
			"replace")
	}
	raw, err := os.ReadFile(recordPath)
	if err != nil {
		return fmt.Errorf("read approval record: %w", err)
	}
	var rec ApprovalRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return fmt.Errorf("parse approval record %s: %w", recordPath, err)
	}

	// 1. 绑定：这一条批的是"这个 case、这个环境"，不是"随便什么"。
	if rec.Case != req.caseID {
		return fmt.Errorf("approval record is for case %q, not %q; an approval that is not "+
			"bound to one case approves everything", rec.Case, req.caseID)
	}
	if rec.Env != req.env {
		return fmt.Errorf("approval record is for env %q, not %q", rec.Env, req.env)
	}
	if rec.RequestedBy != req.operator {
		return fmt.Errorf("approval record names requester %q but this command is run by %q; "+
			"a record for someone else's request is not this request", rec.RequestedBy, req.operator)
	}
	// 2. 第二个人。**这一条是全部要害**，其余两条都是为了让改掉
	//    approved_by 会立刻被 HMAC 抓住。
	if rec.ApprovedBy == req.operator {
		return fmt.Errorf("%q both requested and approved this injection; two-person approval "+
			"with one person is --confirm-prod with extra steps", rec.ApprovedBy)
	}
	if rec.ApprovedBy == "" {
		return fmt.Errorf("approval record has no approved_by")
	}
	// 3. 时效。
	if rec.ApprovedAt.IsZero() {
		return fmt.Errorf("approval record has no approved_at")
	}
	if rec.ApprovedAt.After(now) {
		return fmt.Errorf("approval record is dated %s, in the future relative to %s",
			rec.ApprovedAt.Format(time.RFC3339), now.Format(time.RFC3339))
	}
	if age := now.Sub(rec.ApprovedAt); age > maxAge {
		return fmt.Errorf("approval is %s old, over the %s ceiling; re-approve rather than "+
			"reusing a stale yes", age.Round(time.Second), maxAge)
	}

	// 4. 留痕：签名必须由**审批人自己的密钥**验出来。
	keysDir := approvalKeysDir(keysDirFlag)
	key, err := loadApprovalKey(keysDir, rec.ApprovedBy)
	if err != nil {
		return err
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(canonicalApproval(rec))
	want := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(rec.HMAC)) {
		return fmt.Errorf("approval record does not verify under %s's key: someone edited it "+
			"after it was signed, or it was signed with a different key", rec.ApprovedBy)
	}
	return nil
}

// validIdentity 判断一个身份能不能当文件名与 HMAC 键名。
func validIdentity(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

// listApprovalKeys 列出密钥目录里有哪些身份——给"为什么我的审批没过"用。
func listApprovalKeys(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".key") {
			continue
		}
		out = append(out, strings.TrimSuffix(e.Name(), ".key"))
	}
	sort.Strings(out)
	return out
}
