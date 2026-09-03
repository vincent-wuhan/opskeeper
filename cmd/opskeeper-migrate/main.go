// Command opskeeper-migrate 提供 ops-keeper → opskeeper 数据迁移 CLI。
//
// 子命令：
//
//	export        从 ops-keeper 导出全部数据到 snapshot
//	import        从 snapshot 导入到 opskeeper（dry-run 支持）
//	rollback      一键回滚（用 rollback snapshot）
//	verify        对比源/目标，输出 diff 报告
//	list-entities 列出全部支持的实体类型 + 依赖关系
//
// 设计依据：docs/superpowers/specs/2026-07-13-unified-platform-path-a-design.md §2.5
// 关联 spec：docs/integration-guide.md §四 数据迁移
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/vincent-wuhan/opskeeper/internal/migrate"
)

const version = "1.0.0-dev"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}
	sub := os.Args[1]
	args := os.Args[2:]

	if sub == "--version" || sub == "-v" {
		fmt.Printf("opskeeper-migrate %s\n", version)
		return
	}
	if sub == "--help" || sub == "-h" {
		printUsage()
		return
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	var err error
	switch sub {
	case "export":
		err = cmdExport(ctx, args)
	case "import":
		err = cmdImport(ctx, args)
	case "rollback":
		err = cmdRollback(ctx, args)
	case "verify":
		err = cmdVerify(ctx, args)
	case "list-entities":
		err = cmdListEntities(args)
	default:
		fmt.Fprintf(os.Stderr, "未知子命令: %s\n\n", sub)
		printUsage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Print(`opskeeper-migrate — ops-keeper → opskeeper 数据迁移 CLI

USAGE:
  opskeeper-migrate <subcommand> [flags]

SUBCOMMANDS:
  export         从 ops-keeper 导出到 snapshot
  import         从 snapshot 导入到 opskeeper（支持 dry-run）
  rollback       用 rollback snapshot 一键回滚
  verify         验证导入结果（diff 报告）
  list-entities  列出全部 9 类支持的实体

示例：
  # 1. 导出
  opskeeper-migrate export \
    --source opskeeper://user:pass@ops-keeper.internal:3000 \
    --output snapshot-2026-07-13.json

  # 2. 校验（dry-run）
  opskeeper-migrate import \
    --source snapshot-2026-07-13.json \
    --target opskeeper://ops.example.com \
    --tenant-mapping "ops-proj-id=opskeeper-tenant-id,..." \
    --dry-run

  # 3. 实际导入
  opskeeper-migrate import \
    --source snapshot-2026-07-13.json \
    --target opskeeper://ops.example.com \
    --tenant-mapping "42=1,100=2" \
    --rate 1000

  # 4. 验证
  opskeeper-migrate verify \
    --source snapshot-2026-07-13.json \
    --target opskeeper://ops.example.com \
    --tenant-mapping "42=1,100=2" \
    --report verify-2026-07-13.html

  # 5. 回滚（如需要）
  opskeeper-migrate rollback \
    --rollback-snapshot rollback-snapshot-2026-07-13T10-30-00.json \
    --target opskeeper://ops.example.com
`)
}

func cmdExport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	var source, token, output string
	var entities multiFlag
	fs.StringVar(&source, "source", "", "ops-keeper base URL（必填）")
	fs.StringVar(&token, "token", "", "ops-keeper 认证 token")
	fs.StringVar(&output, "output", "", "snapshot 输出路径（.json 或 .json.gz）")
	fs.Var(&entities, "entity", "限定实体类型（可多次指定；缺省全部）")
	if err := fs.Parse(args); err != nil {
		return err
	}

	selectedEntities, err := parseEntities(entities)
	if err != nil {
		return err
	}

	snap, err := migrate.Export(ctx, migrate.ExportOptions{
		Entities: selectedEntities,
		Output:   output,
		Source:   source,
		Token:    token,
	})
	if err != nil {
		return err
	}
	fmt.Printf("✅ 导出完成\n")
	fmt.Printf("   路径: %s\n", output)
	fmt.Printf("   实体类型: %d\n", len(snap.Entities))
	fmt.Printf("   总记录数: %d\n", snap.TotalRows())
	for et, rows := range snap.Entities {
		fmt.Printf("     %s: %d\n", et, len(rows))
	}
	return nil
}

func cmdImport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	var source, target, token, tenantMapping, snapshot string
	var dryRun bool
	var rate int
	var entities multiFlag
	fs.StringVar(&snapshot, "source", "", "snapshot 文件路径")
	fs.StringVar(&target, "target", "", "opskeeper base URL（必填）")
	fs.StringVar(&token, "token", "", "opskeeper 认证 token")
	fs.StringVar(&tenantMapping, "tenant-mapping", "", "ops-keeper project_id → opskeeper tenant_id 映射（必填，多租户隔离）")
	fs.BoolVar(&dryRun, "dry-run", false, "仅校验 + 报告，不实际写入")
	fs.IntVar(&rate, "rate", 1000, "限速（行/秒）")
	fs.StringVar(&source, "opskeeper-url", "", "（可选）实时 ops-keeper URL，替代 --source snapshot")
	fs.Var(&entities, "entity", "限定实体类型（可多次指定；缺省全部）")
	if err := fs.Parse(args); err != nil {
		return err
	}

	selectedEntities, err := parseEntities(entities)
	if err != nil {
		return err
	}

	result, err := migrate.Import(ctx, migrate.ImportOptions{
		Snapshot:      snapshot,
		Target:        target,
		Token:         token,
		TenantMapping: tenantMapping,
		Entities:      selectedEntities,
		DryRun:        dryRun,
		RatePerSec:    rate,
	})
	if err != nil {
		return err
	}
	fmt.Printf("%s导入完成\n", map[bool]string{true: "🧪 [DRY-RUN] ", false: "✅ "}[dryRun])
	fmt.Printf("   总记录数: %d\n", result.Total)
	if dryRun {
		fmt.Printf("   假设可导入: %d\n", result.Imported)
	} else {
		fmt.Printf("   新建: %d\n", result.Imported)
		fmt.Printf("   跳过（幂等命中）: %d\n", result.Skipped)
		fmt.Printf("   失败: %d\n", result.Failed)
	}
	if len(result.Failures) > 0 {
		fmt.Println("\n失败详情（前 20 条）：")
		for i, f := range result.Failures {
			if i >= 20 {
				break
			}
			fmt.Printf("  [%s] %s: %v\n", f.Entity, f.SourceID, f.Reason)
		}
		if result.Failed > 20 {
			fmt.Printf("  ... 还有 %d 条\n", result.Failed-20)
		}
	}
	return nil
}

func cmdRollback(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("rollback", flag.ExitOnError)
	var snapshot, target, token string
	var dryRun bool
	fs.StringVar(&snapshot, "rollback-snapshot", "", "rollback snapshot 文件路径")
	fs.StringVar(&target, "target", "", "opskeeper base URL（必填）")
	fs.StringVar(&token, "token", "", "opskeeper 认证 token")
	fs.BoolVar(&dryRun, "dry-run", false, "仅报告将删除的实体")
	if err := fs.Parse(args); err != nil {
		return err
	}

	result, err := migrate.Rollback(ctx, migrate.RollbackOptions{
		SnapshotPath: snapshot,
		Target:       target,
		Token:        token,
		DryRun:       dryRun,
	})
	if err != nil {
		return err
	}
	fmt.Printf("%s回滚完成\n", map[bool]string{true: "🧪 [DRY-RUN] ", false: "✅ "}[dryRun])
	fmt.Printf("   总计: %d\n", result.Total)
	fmt.Printf("   删除: %d\n", result.Deleted)
	fmt.Printf("   失败: %d\n", result.Failed)
	if len(result.Failures) > 0 {
		fmt.Println("\n失败详情：")
		for _, f := range result.Failures {
			fmt.Printf("  [%s] %s: %v\n", f.Entity, f.ID, f.Reason)
		}
	}
	return nil
}

func cmdVerify(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	var source, sourceToken, target, targetToken, tenantMapping, snapshot, output string
	var entities multiFlag
	fs.StringVar(&snapshot, "source", "", "snapshot 文件路径")
	fs.StringVar(&source, "opskeeper-url", "", "（可选）实时 ops-keeper URL")
	fs.StringVar(&sourceToken, "source-token", "", "ops-keeper 认证 token")
	fs.StringVar(&target, "target", "", "opskeeper base URL（必填）")
	fs.StringVar(&targetToken, "target-token", "", "opskeeper 认证 token")
	fs.StringVar(&tenantMapping, "tenant-mapping", "", "映射字符串（必填）")
	fs.StringVar(&output, "report", "", "HTML 报告输出路径（可选）")
	fs.Var(&entities, "entity", "限定实体类型")
	if err := fs.Parse(args); err != nil {
		return err
	}

	selectedEntities, err := parseEntities(entities)
	if err != nil {
		return err
	}

	result, err := migrate.Verify(ctx, migrate.VerifyOptions{
		SnapshotPath:  snapshot,
		Source:        source,
		SourceToken:   sourceToken,
		Target:        target,
		TargetToken:   targetToken,
		TenantMapping: tenantMapping,
		Output:        output,
		Entities:      selectedEntities,
	})
	if err != nil {
		return err
	}
	fmt.Print(result.String())
	if output != "" {
		// 简化 HTML 输出（JSON）
		raw, _ := json.MarshalIndent(result, "", "  ")
		if err := os.WriteFile(output, raw, 0o644); err != nil {
			return err
		}
		fmt.Printf("\n📄 详细报告: %s\n", output)
	}
	return nil
}

func cmdListEntities(_ []string) error {
	fmt.Println("=== opskeeper-migrate 支持的实体类型 ===")
	fmt.Printf("\n按依赖顺序（迁移执行顺序）：\n")
	order := migrate.MigrationOrder()
	for i, et := range order {
		meta := migrate.GetEntityMeta(et)
		if meta == nil {
			continue
		}
		fmt.Printf("  %d. %s\n", i+1, et)
		fmt.Printf("     源: ops-keeper %s\n", meta.Source)
		fmt.Printf("     目标: opskeeper %s\n", meta.Target)
		if len(meta.DependsOn) > 0 {
			deps := make([]string, len(meta.DependsOn))
			for j, d := range meta.DependsOn {
				deps[j] = string(d)
			}
			fmt.Printf("     依赖: %s\n", strings.Join(deps, ", "))
		}
		if meta.Encryption {
			fmt.Printf("     加密: ✓（凭据加密重存）\n")
		}
	}
	fmt.Printf("\n共 %d 类\n", len(order))
	return nil
}

// parseEntities 解析命令行 --entity 标志（多次）。
func parseEntities(values []string) ([]migrate.EntityType, error) {
	if len(values) == 0 {
		return nil, nil
	}
	out := make([]migrate.EntityType, 0, len(values))
	for _, s := range values {
		t, err := migrate.ParseEntityType(s)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

// multiFlag 让 flag 支持多次出现的同名参数。
type multiFlag []string

func (m *multiFlag) String() string {
	return strings.Join(*m, ",")
}

func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}
