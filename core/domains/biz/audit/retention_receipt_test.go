package audit

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	model "github.com/vincent-wuhan/opskeeper/core/domains/model/audit"
)

// 决策 329：**唯一能删掉证据的操作，必须在链上留下「我删过」的证据。**
//
// 这一刀之前，保留期把链的前缀截掉之后，留在世界上的只有一行日志。那不是记录：
// 它不被摘要覆盖、不跟着链走、而且**在从删除之前的备份恢复之后就不存在了**——
// 而那恰恰是最需要它的场景。一个运维看到一条链从第 40000 行开始，他问「这是保留期
// 干的，还是有人干的？」，而账本刚刚把能回答这个问题的行扔掉了。
//
// 所以截断要给自己开发一张收据。这一组用例钉的是那张收据的三个必要性质：
// 它存在、它自己也在链上（所以改不了）、以及**没有截断就不写**
// （每天凌晨往链里塞一行「今天没删任何东西」，几个月后这条链就没法看了）。

// 1. 截断之后，链上有且仅有一条收据，而且它自己也被封了。
func TestTruncatingTheChainLeavesAReceiptOnTheChain(t *testing.T) {
	ctx := context.Background()
	uc, db := newChainedUC(t, testKey)

	// Three rows old enough to go, one recent enough to stay.
	old := time.Now().UTC().Add(-90 * 24 * time.Hour)
	for i := 0; i < 3; i++ {
		emitAt(t, uc, old.Add(time.Duration(i)*time.Minute), "device.write")
	}
	emit(t, uc, "device.read")

	cutoff := time.Now().UTC().Add(-30 * 24 * time.Hour)
	removed, err := uc.sweep(ctx, cutoff)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if removed != 3 {
		t.Fatalf("removed %d rows, want 3", removed)
	}

	rows := allRows(t, db)
	var receipts []model.Log
	for _, r := range rows {
		if r.Action == model.ActionRetentionTruncate {
			receipts = append(receipts, r)
		}
	}
	if len(receipts) != 1 {
		t.Fatalf("%d truncation receipts, want exactly 1: a chain that cuts its front without saying so is the defect this row exists for", len(receipts))
	}
	receipt := receipts[0]
	if receipt.ResourceType != model.ResourceAuditChain {
		t.Errorf("the receipt is filed under %q, want %q — the record of the ledger's own mutation must not sit in a bucket with the entries", receipt.ResourceType, model.ResourceAuditChain)
	}
	if receipt.Hash == "" {
		t.Error("the receipt carries no seal: someone could delete or rewrite it, which is the one thing it must not be")
	}
	// The receipt has to say what a reader needs in order to ask the next
	// question: how much went, and where the chain now starts.
	var payload struct {
		RowsRemoved  int64  `json:"rows_removed"`
		NewAnchorSeq uint64 `json:"new_anchor_seq"`
		Cutoff       string `json:"cutoff"`
	}
	if err := json.Unmarshal([]byte(receipt.PayloadJSON), &payload); err != nil {
		t.Fatalf("decode the receipt payload %q: %v", receipt.PayloadJSON, err)
	}
	if payload.RowsRemoved != 3 {
		t.Errorf("rows_removed = %d, want 3", payload.RowsRemoved)
	}
	if payload.Cutoff == "" {
		t.Error("the receipt does not say what cutoff produced the cut, so nobody can reproduce the decision")
	}
	// And the chain must still verify after it. A receipt that broke the
	// chain would be worse than no receipt.
	if err := uc.VerifyChain(ctx); err != nil {
		t.Fatalf("the chain does not verify after a truncation that left a receipt: %v", err)
	}
}

// 2. 没有截断就没有收据。
//
// 这一条是上一条的另一半，也是更容易被忽略的那一半：**每天 03:00 跑一次的作业，
// 如果每次都往链里写一行「今天什么都没删」，一年之后这条链有一半是这个。**
// 收据记录的是一次真实的变化，不是一次心跳。
func TestASweepThatRemovesNothingLeavesNoReceipt(t *testing.T) {
	ctx := context.Background()
	uc, db := newChainedUC(t, testKey)
	emit(t, uc, "device.read")

	if _, err := uc.sweep(ctx, time.Now().UTC().Add(-24*time.Hour)); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	for _, r := range allRows(t, db) {
		if r.Action == model.ActionRetentionTruncate {
			t.Fatalf("a sweep that removed nothing still wrote a receipt: %+v", r)
		}
	}
}

// 3. 收据自己也要能被验证：这才是它能当证据的原因。
func TestTheReceiptIsCoveredByTheDigestLikeAnyOtherRow(t *testing.T) {
	ctx := context.Background()
	uc, db := newChainedUC(t, testKey)
	old := time.Now().UTC().Add(-90 * 24 * time.Hour)
	emitAt(t, uc, old, "device.write")
	emit(t, uc, "device.read")
	if _, err := uc.sweep(ctx, time.Now().UTC().Add(-30*24*time.Hour)); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	var receipt *model.Log
	for _, r := range allRows(t, db) {
		if r.Action == model.ActionRetentionTruncate {
			row := r
			receipt = &row
		}
	}
	if receipt == nil {
		t.Fatal("no receipt to tamper with")
	}
	// Rewrite what it claims happened. If the receipt were outside the
	// digest this edit would be invisible — which would make it a note
	// rather than a record.
	if err := db.Model(&model.Log{}).Where("seq = ?", receipt.Seq).
		Update("payload_json", `{"rows_removed":0,"new_anchor_seq":0,"cutoff":""}`).Error; err != nil {
		t.Fatalf("tamper: %v", err)
	}
	err := uc.VerifyChain(ctx)
	if err == nil {
		t.Fatal("a rewritten receipt still verifies; it is a note, not a record")
	}
}
