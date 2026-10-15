package gatename

// NotRun is every check-shaped Makefile target CI does not invoke, each with
// the reason it is not there.
//
// It is a separate table from NotInCI because NotInCI is about the plan's
// acceptance lines -- promises written in prose, which may or may not have a
// make target at all -- while this is about targets that exist and go unused.
// Merging them would lose the distinction that matters here: a target nobody
// runs is a decision somebody has not made yet, not a decision somebody made
// and wrote down elsewhere.
var NotRun = map[string]string{
	"version-check":         "a release-time assertion, not a per-push one: it compares RELEASE_VERSION.json's web_hash and teamharness_source_tree against `git rev-parse HEAD:<tree>`, so it can only be green on the commit that was actually signed. It is not unwired, it is wired in .github/workflows/release.yml where those comparisons mean something; NotInCI already carries the same reasoning in prose (decisions 166, 348)",
	"mysql-migration-check": "it needs a live MySQL to migrate and roll back against (OPSKEEPER_TEST_MYSQL_DSN), and the per-push job deliberately runs no database container; the same property is covered for the other engines by the gates that do run. Wiring it into a job with a MySQL service is a real change to the pipeline, not a line in this table (decision 348)",
}

// SelfExempt is what checks gates without being a gate the plan promises.
//
// Recorded rather than skipped by a name rule, because "this check is about
// the wiring of gates, not itself a promised gate" and "somebody added a
// -check target and forgot the table" are the same shape from the outside,
// and the second is how this exemption becomes a hole.
var SelfExempt = map[string]string{
	"ci-gate-check": "this checker: it answers whether the promised gates run, so it is not one of them",
}
