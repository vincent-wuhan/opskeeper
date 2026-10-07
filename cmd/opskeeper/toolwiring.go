package main

// This file exists because of one specific thing: before it, the manager's
// tool registry was wired by twenty-five lines sitting inside main(), and
// nothing outside main() could run them.
//
// That is not a style complaint. A capability that exists in the tree, is
// built, is migrated, has tests, and is listed in ROADMAP.md with a ☑ — and
// is nevertheless absent from every deployment — is a thing that only an
// executable check can catch, and an executable check needs a seam. The
// missing twenty lines are exactly what a test cannot reach. ROADMAP C.1's
// chat_to_query BaseTool shipped as "delivered" for that reason: the
// registry registers the tool when the translator has an LLM client, the one
// setter that hands it one lived inside main(), and the tool therefore never
// appeared in any tool list this product has ever served.
//
// So the wiring moves here, takes its dependencies as a struct, and is
// called from the same point in main() it always ran at. main() is not
// rearranged; what changes is that these setters are now reachable by a
// test, which is the only property that makes "delivered" checkable.

import (
	"log/slog"
	"os"

	"gorm.io/gorm"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/notify"
	"github.com/vincent-wuhan/opskeeper/core/domain"
	repairpreviewcontrol "github.com/vincent-wuhan/opskeeper/core/domains/control/repairpreview"
	managerbizaiopstools "github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools"
	managerbizaiopstoolsalerting "github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/alerting"
	managerbizaiopstoolschat2query "github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/chat2query"
	managerbizaiopstoolsconfigchange "github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/configchange"
	managerbizaiopstoolsdatabase "github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/database"
	managerbizaiopstoolshost "github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/host"
	managerbizaiopstoolsrecovery "github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/recovery"
	managerbizaiopstoolstopology "github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/topology"
	managerbizapproval "github.com/vincent-wuhan/opskeeper/core/manager/biz/approval"
	devicebiz "github.com/vincent-wuhan/opskeeper/core/manager/biz/device"
	manageraiopsdata "github.com/vincent-wuhan/opskeeper/core/manager/data/aiops/store"
	manageralertdata "github.com/vincent-wuhan/opskeeper/core/manager/data/alert/store"
)

// toolRegistryWiring carries every dependency the registry's
// post-construction setters need.
//
// It is a struct rather than eleven parameters because the alternative —
// repeating the call sequence in a test — is the thing that let C.1 drift:
// a test that wires the registry its own way tests the test's wiring, not
// the binary's. One sequence, two callers.
type toolRegistryWiring struct {
	// Caller, Edges, Devices, Prom, Logs, Trace and Alerts are the
	// constructor dependencies. They are here rather than passed to
	// NewRegistry at the call site for one reason: a registry built with
	// nils registers a seventh of the tools a deployment registers, so a
	// check that builds its own registry would be checking a tool bag no
	// operator has ever been served. The narrow slice of
	// "the binary's registry" that a test can build is "the registry this
	// struct builds", and that is only the binary's registry if both the
	// construction and the setters live in one place.
	Caller  managerbizaiopstools.Caller
	Edges   domain.EdgeCatalog
	Devices *devicebiz.Usecase
	Prom    managerbizaiopstools.PromQuerier
	Logs    managerbizaiopstools.LogQuerier
	Trace   managerbizaiopstools.TraceQuerier
	Alerts  managerbizaiopstoolsalerting.AlertUsecase

	// Approval is the human-in-the-loop queue that mutating shell tools
	// block on. nil-safe in the sense that it is always non-nil here; the
	// approval shims are what make cloud_bash and host_bash registerable
	// at all.
	Approval *managerbizapproval.Usecase

	// Channels is the alert channel store send_im_message reads its
	// recipient list from, and Router is the notifier it pushes through —
	// the same BuildSenderFromChannel path the alert notifier and the flow
	// notify node use, so an IM message from the assistant and an IM message
	// from an alert rule are formatted by one implementation.
	Channels *manageralertdata.Repo
	Router   *notify.Router

	// DB is opened and migrated before this runs, and is what backs the
	// chat_to_query translation cache. The table is created by
	// manageraiopsdata.Migrate, which runs earlier in startup order.
	DB *gorm.DB

	// LLM is the completer the chat_to_query translator issues its
	// natural-language-to-query calls through. A nil here is the
	// documented way to turn NL→Query off entirely: the tool is then not
	// registered, rather than registered and failing every call.
	LLM managerbizaiopstoolschat2query.LLMClient

	// PagesDir is where serve_page writes hosted reports. An empty value
	// selects the default volume path.
	PagesDir string

	// The remaining setters, all collected here for the same reason as the
	// ones above: the registry's shape is decided by nineteen calls spread
	// across two thousand lines of startup, and nothing could see all
	// nineteen at once. D.4's apply_config_change is gated on ConfigManager
	// and A.2's query_change_events on AuditLister, both of which are set
	// nine hundred lines before the LLM client exists — so a check that
	// only knew about the late setters would report a roadmap item as
	// missing when the wiring is present and correct.
	RecoveryAudit  managerbizaiopstoolsrecovery.MutatingProposalAuditRepo
	RepairPreview  repairpreviewcontrol.Gate
	HostTerminator managerbizaiopstoolshost.HostProcessTerminator
	PoolRecovery   managerbizaiopstoolsrecovery.PoolRecoveryExecutor
	PluginConfig   managerbizaiopstoolsdatabase.PluginConfigLister
	ConfigManager  managerbizaiopstoolsconfigchange.ConfigManager
	AuditLister    managerbizaiopstoolsalerting.AuditLister
	EdgeChanges    managerbizaiopstoolsalerting.EdgeChangeLister
	TopologyInfo   managerbizaiopstools.TopologyInfo
	TopologyGraph  managerbizaiopstoolstopology.Graph
	Knowledge      managerbizaiopstools.KnowledgeSearcher
	WorkerSpawner  managerbizaiopstools.WorkerSpawner
	Subagents      managerbizaiopstools.SubagentRegistry

	// Log is required; apply falls back to slog.Default otherwise.
	Log *slog.Logger
}

// defaultPagesDir is the path on the persistent volume that serve_page uses
// when nothing overrides it. The directory has to be a volume rather than
// /tmp: a hosted report that disappears on restart is a report nobody can
// link a colleague to.
const defaultPagesDir = "/var/lib/opskeeper/pages"

// buildRegistry constructs the registry from the fields that exist early in
// startup.
//
// It is separate from apply because startup order is real: the tunnel client,
// the edge and device usecases and the three query clients all exist long
// before the approval queue, the alert channel store and the LLM client do.
// Splitting construction from injection is what lets one struct carry the
// whole wiring across that gap, and it is why a test can produce the same
// registry a deployment does: it fills every field and calls both.
func (w toolRegistryWiring) buildRegistry() *managerbizaiopstools.Registry {
	log := w.Log
	if log == nil {
		log = slog.Default()
	}
	return managerbizaiopstools.NewRegistry(w.Caller, w.Edges, w.Devices,
		w.Prom, w.Logs, w.Trace, w.Alerts, log)
}

// apply runs every post-construction setter the manager's tool bag depends
// on, in the order the tools were added, and hands back the page store so
// the caller can mount the /pages route that serves what serve_page wrote.
//
// It hands the store back so the caller can mount the /pages routes that
// serve what serve_page wrote. When the directory cannot be created the
// zero store is returned and serve_page is left unregistered; the routes
// are mounted either way, which is the behaviour this block had before it
// moved, and it is the safer of the two — a route that answers "nothing
// here" is a visible absence, while a tool the model may call and that
// always fails is not.
func (w toolRegistryWiring) apply(reg *managerbizaiopstools.Registry) filePageStore {
	if reg == nil {
		return filePageStore{}
	}
	log := w.Log
	if log == nil {
		log = slog.Default()
	}

	// The setters that run earlier in startup, replayed here in the same
	// order main() used them. They are nil-safe on the far side: a nil
	// here is the same nil the registry held between that call and this
	// one, and BuildBaseTools is not called in between.
	if w.RecoveryAudit != nil {
		reg.SetRecoveryAuditRepo(w.RecoveryAudit)
	}
	if w.RepairPreview != nil {
		reg.SetRepairPreviewGate(w.RepairPreview)
	}
	if w.HostTerminator != nil {
		reg.SetHostFixtureTerminator(w.HostTerminator)
	}
	if w.PoolRecovery != nil {
		reg.SetPoolRecoveryExecutor(w.PoolRecovery)
	}
	if w.PluginConfig != nil {
		reg.SetPluginConfigLister(w.PluginConfig)
	}
	if w.ConfigManager != nil {
		reg.SetConfigManager(w.ConfigManager)
	}
	if w.AuditLister != nil {
		reg.SetAuditLister(w.AuditLister)
	}
	if w.EdgeChanges != nil {
		reg.SetEdgeChangeLister(w.EdgeChanges)
	}
	reg.SetTopologyInfo(w.TopologyInfo)
	if w.TopologyGraph != nil {
		reg.SetTopologyGraph(w.TopologyGraph)
	}
	if w.Knowledge != nil {
		reg.SetKnowledgeSearcher(w.Knowledge)
	}
	if w.WorkerSpawner != nil {
		reg.SetWorkerSpawner(w.WorkerSpawner, w.Subagents)
	}

	// cloud_bash / host_bash: mutating shell commands block on an approval
	// card rather than running, and the kernel's boot check fails on any
	// mutating tool that has not declared an approval owner.
	reg.SetCloudBashProposer(cloudBashProposerShim{uc: w.Approval})
	reg.SetHostBashProposer(hostBashProposerShim{uc: w.Approval})

	// chat_to_query: NL → PromQL / LogQL / TraceQL with a dry-run preview
	// and a template cache for repeat questions. ROADMAP C.1, and the tool
	// that section names was absent from every manager that ever ran,
	// because these two calls did not exist.
	reg.SetChatToQueryLLM(w.LLM)
	reg.SetChatToQueryTemplateStore(manageraiopsdata.NewQueryTemplateStore(w.DB))

	// send_im_message: the assistant can proactively push to a configured
	// channel (飞书 / 钉钉 / …).
	reg.SetIMSender(imSenderShim{channels: w.Channels, router: w.Router})

	// serve_page: the assistant can host a generated HTML report at an
	// internal /pages/<token> URL. A directory that cannot be created
	// disables the tool rather than failing startup, because a missing
	// page host is not a reason the manager should refuse to come up.
	pagesDir := w.PagesDir
	if pagesDir == "" {
		pagesDir = defaultPagesDir
	}
	pageStore := filePageStore{dir: pagesDir, log: log.With(slog.String("comp", "serve_page"))}
	if err := os.MkdirAll(pagesDir, 0o755); err != nil {
		log.Warn("serve_page: mkdir pages dir failed; serve_page disabled",
			slog.String("dir", pagesDir), slog.Any("err", err))
		return filePageStore{}
	}
	reg.SetPageStore(pageStore)
	return pageStore
}
