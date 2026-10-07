package alertdraft

import "testing"

// The guard is a product rule with two sides that must not be confused: it
// blocks prose that presents itself as a confirmable draft, and it must not
// touch an answer that does not. Each test below names which side it is
// holding down, because a guard that only ever blocks is indistinguishable
// from a product that refuses to answer.

func TestTheGuardBlocksAProseDraftWhenTheTurnAskedForARule(t *testing.T) {
	g := NewGuard()
	const ask = "帮我创建一条 MySQL 连接数告警规则"
	prose := "规则 key: mysql_conn_high\n触发条件: conn_used > 80%\n草案哈希: sha256:abc\n需要确认后创建。"

	got := g.Sanitize("s1", ask, prose)
	if got != BlockedMessage {
		t.Fatalf("content = %q, want the blocked message", got)
	}
}

func TestTheGuardStandsDownOnceTheRealDraftSucceeded(t *testing.T) {
	g := NewGuard()
	const ask = "创建 MySQL 连接数告警规则"
	const result = `{"kind":"config_draft","draft_hash":"sha256:abc","payload":{"rule":{"kind":"metric_threshold"}}}`
	prose := "规则 key: mysql_conn_high\n触发条件: conn_used > 80%\n草案哈希: sha256:abc\n需要确认后创建。"

	g.ObserveTool("s1", ask, DraftConfigChangeToolName, result)
	if got := g.Sanitize("s1", ask, prose); got != prose {
		t.Fatalf("content = %q, want the draft left alone", got)
	}
}

func TestTheGuardStandsDownOnATurnThatNeverAskedForARule(t *testing.T) {
	g := NewGuard()
	// The same prose, asked as an explanation rather than a creation. A
	// guard that answered this with the blocked message would be refusing a
	// question it was never asked.
	prose := "规则 key: mysql_conn_high\n触发条件: conn_used > 80%\n草案哈希: sha256:abc\n需要确认后创建。"
	if got := g.Sanitize("s1", "解释一下这条告警规则为什么这么配", prose); got != prose {
		t.Fatalf("content = %q, want the answer left alone", got)
	}
}

func TestTheGuardStandsDownOnATurnThatIsNotADraftTurnAtAll(t *testing.T) {
	g := NewGuard()
	prose := "主机 CPU 使用率 91%，负载 12.4。"
	if got := g.Sanitize("s1", "现在哪台机器最忙？", prose); got != prose {
		t.Fatalf("content = %q, want the answer left alone", got)
	}
}

// A tool call from a previous turn must not license prose in the next one:
// the model could otherwise answer an unrelated question with a draft and
// have it pass the guard.
//
// The turn boundary is (session, user text) rather than a counter, because
// the transcript write path and the frame emitter can only see what the
// runtime stamped on ctx — and a turn is exactly what that text is. Two
// consecutive turns that repeat the same text therefore share a licence,
// which is the intended reading: the user asked the same question twice and
// the draft produced for the first one is still the draft for that question.
func TestStateFromAPreviousTurnDoesNotCarryOver(t *testing.T) {
	g := NewGuard()
	g.ObserveTool("s1", "创建告警规则", DraftConfigChangeToolName, `{"kind":"config_draft","draft_hash":"sha256:abc"}`)

	prose := "规则 key: other\n触发条件: cpu > 90%\n草案哈希: sha256:def\n需要确认后创建。"
	if got := g.Sanitize("s1", "那再看看磁盘空间", prose); got != prose {
		t.Fatalf("content = %q, want the answer left alone on a turn that asked for no draft", got)
	}
	if got := g.Sanitize("s1", "创建磁盘告警规则", prose); got != BlockedMessage {
		t.Fatalf("content = %q, want the stale draft licence ignored", got)
	}
}

// A draft built from a source the user never named must not have that
// invented source printed in the reply: the console would show a rule scoped
// to a database nobody chose.
func TestInventedSourceNamesAreRewrittenOutOfTheAnswer(t *testing.T) {
	g := NewGuard()
	const ask = "创建一条慢查询告警规则"
	const result = `{"kind":"config_draft","draft_hash":"sha256:abc","payload":{"rule":{"kind":"metric_raw","spec":{"expr":"db:mysql_slow{db:\"orders\"} > 0"}}}}`
	prose := "草稿已生成，expr 用了 db:mysql_slow。草案哈希: sha256:abc"

	g.ObserveTool("s1", ask, DraftConfigChangeToolName, result)
	got := g.Sanitize("s1", ask, prose)
	if got == prose {
		t.Fatal("the invented source name survived the reply")
	}
	if containsAnyNeedle(got, got, []string{"db:mysql_slow", "custom:"}) {
		t.Fatalf("reply still names a source: %q", got)
	}
	if !containsAnyNeedle(got, got, []string{"某个数据库采集源"}) {
		t.Fatalf("reply = %q, want the source replaced with a generic phrase", got)
	}
}

// An explicit source in the rule is the user's own choice and must be left
// alone — the rewrite exists to hide what WE invented, not what they picked.
func TestAnExplicitSourceIsNotRewritten(t *testing.T) {
	g := NewGuard()
	const ask = "创建一条慢查询告警规则"
	const result = `{"kind":"config_draft","draft_hash":"sha256:abc","payload":{"rule":{"kind":"metric_raw","spec":{"source_explicit":true,"expr":"db:mysql_slow > 0"}}}}`
	prose := "草稿已生成，expr 用了 db:mysql_slow。"

	g.ObserveTool("s1", ask, DraftConfigChangeToolName, result)
	if got := g.Sanitize("s1", ask, prose); got != prose {
		t.Fatalf("content = %q, want the explicit source kept", got)
	}
}

// A tool result that is not a confirmable draft must not arm the guard: the
// tool can fail, return prose, or be answered from cache, and none of those
// produce a hash apply_config_change would verify.
func TestAToolResultWithoutADraftHashArmsNothing(t *testing.T) {
	g := NewGuard()
	const ask = "创建 MySQL 连接数告警规则"
	g.ObserveTool("s1", ask, DraftConfigChangeToolName, `{"error":"catalog not configured"}`)

	prose := "规则 key: mysql_conn_high\n触发条件: conn_used > 80%\n草案哈希: sha256:abc"
	if got := g.Sanitize("s1", ask, prose); got != BlockedMessage {
		t.Fatalf("content = %q, want the failed tool call not to arm the guard", got)
	}
}

// A tool the model called under a different case is the same tool: the name
// comes from the provider, and providers are not case-stable across models.
func TestTheToolNameIsMatchedCaseInsensitively(t *testing.T) {
	g := NewGuard()
	const ask = "创建 MySQL 连接数告警规则"
	g.ObserveTool("s1", ask, " Draft_Config_Change ", `{"kind":"config_draft","draft_hash":"sha256:abc"}`)

	prose := "规则 key: mysql_conn_high\n触发条件: conn_used > 80%\n草案哈希: sha256:abc"
	if got := g.Sanitize("s1", ask, prose); got != prose {
		t.Fatalf("content = %q, want the draft left alone", got)
	}
}

// Two sessions in the same guard are two independent turns: one session's
// real draft must not license the other's prose.
func TestSessionsDoNotShareState(t *testing.T) {
	g := NewGuard()
	const ask = "创建告警规则"
	g.ObserveTool("a", ask, DraftConfigChangeToolName, `{"kind":"config_draft","draft_hash":"sha256:abc"}`)

	prose := "规则 key: x\n触发条件: cpu > 90%\n草案哈希: sha256:def\n需要确认后创建。"
	if got := g.Sanitize("b", ask, prose); got != BlockedMessage {
		t.Fatalf("content = %q, want session b blocked by its own state", got)
	}
}

func TestForgetDropsTheSession(t *testing.T) {
	g := NewGuard()
	const ask = "创建告警规则"
	g.ObserveTool("s1", ask, DraftConfigChangeToolName, `{"kind":"config_draft","draft_hash":"sha256:abc"}`)
	g.Forget("s1")

	prose := "规则 key: x\n触发条件: cpu > 90%\n草案哈希: sha256:def\n需要确认后创建。"
	if got := g.Sanitize("s1", ask, prose); got != BlockedMessage {
		t.Fatalf("content = %q, want the forgotten session to start unarmed", got)
	}
}

// A nil guard is what a deployment without the feature gets, and every
// method has to survive it: a panic on the write path would take the turn
// down rather than skip a rule.
func TestANilGuardIsAnOrdinaryNoOp(t *testing.T) {
	var g *Guard
	g.ObserveTool("s1", "创建告警规则", DraftConfigChangeToolName, `{"kind":"config_draft","draft_hash":"x"}`)
	if got := g.Sanitize("s1", "创建告警规则", "prose"); got != "prose" {
		t.Fatalf("content = %q, want passthrough", got)
	}
	g.Forget("s1")
}

func TestTheBlockedMessageIsRecognisedWhenReplayed(t *testing.T) {
	// The history heuristics look for the block on a later turn; a message
	// that is not recognised turns "try again" into "start over".
	if !LooksLikeBlockedMessage(BlockedMessage) {
		t.Fatal("BlockedMessage is not recognised by LooksLikeBlockedMessage")
	}
	if LooksLikeBlockedMessage("今天没有告警。") {
		t.Fatal("an unrelated answer read as the blocked message")
	}
}
