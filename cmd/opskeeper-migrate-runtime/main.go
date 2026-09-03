// Command opskeeper-migrate-runtime 提供 opskeeper 数据库 schema 的版本化迁移 CLI（Task 3.4）。
//
// 子命令：
//
//	up      应用所有 pending 迁移
//	down    回滚 N 步（默认 1）
//	status  显示 applied / pending 状态
//	create  创建新迁移骨架（文件名 + Up/Down 占位）
//
// 设计依据：docs/superpowers/specs/2026-07-13-unified-platform-path-a-design.md §2.5
// 关联包：internal/migrator
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/vincent-wuhan/opskeeper/internal/migrator"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const version = "1.0.0-dev"

// 全局注册表（通过 init() 函数由各业务包注册）。
//
// v1 实现：注册一个示例迁移（建 schema_migrations 表本身）。
// v2 计划：让业务包通过 import 副作用自动注册。
var registry = migrator.NewRegistry()

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}
	sub := os.Args[1]
	args := os.Args[2:]

	if sub == "--version" || sub == "-v" {
		fmt.Printf("opskeeper-migrate-runtime %s\n", version)
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
	case "up":
		err = cmdUp(ctx, args)
	case "down":
		err = cmdDown(ctx, args)
	case "status":
		err = cmdStatus(ctx, args)
	case "create":
		err = cmdCreate(args)
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n\n", sub)
		printUsage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Print(`opskeeper-migrate-runtime — opskeeper 数据库 schema 版本化迁移 CLI

USAGE:
  opskeeper-migrate-runtime <subcommand> [flags]

SUBCOMMANDS:
  up         应用所有 pending 迁移
  down N     回滚 N 步（默认 1）
  status     显示 applied / pending 状态
  create     创建新迁移骨架

FLAGS（全局）:
  --dsn         数据库 DSN（必填，如 "postgres://${DB_USER}:${DB_PASSWORD}@host:5432/db"）
  --driver      数据库驱动（postgres / mysql / sqlite；默认从 dsn 推断）
  --dry-run     仅报告，不实际写入
  --steps N     限制步数（up/down）

示例：
  # 应用所有 pending
  opskeeper-migrate-runtime up \
    --dsn "postgres://opskeeper:pwd@localhost:5432/opskeeper?sslmode=disable"

  # 回滚 1 步
  opskeeper-migrate-runtime down \
    --dsn "postgres://..." --steps 1

  # 查看状态
  opskeeper-migrate-runtime status \
    --dsn "postgres://..."

  # 创建新迁移骨架
  opskeeper-migrate-runtime create --name add_users_table
`)
}

// cmdUp 应用 pending 迁移。
func cmdUp(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("up", flag.ExitOnError)
	var dsn, driver string
	var dryRun bool
	fs.StringVar(&dsn, "dsn", "", "数据库 DSN")
	fs.StringVar(&driver, "driver", "", "数据库驱动（postgres / mysql / sqlite）")
	fs.BoolVar(&dryRun, "dry-run", false, "仅报告，不实际写入")
	if err := fs.Parse(args); err != nil {
		return err
	}

	db, err := openDB(dsn, driver)
	if err != nil {
		return err
	}
	runner := migrator.NewRunner(db, registry)
	result, err := runner.Run(ctx, migrator.RunOptions{
		Direction: migrator.Up,
		DryRun:    dryRun,
		LockKey:   100420260713, // arbitrary fixed key
	})
	if err != nil {
		return err
	}
	fmt.Printf("%sup 完成\n", map[bool]string{true: "🧪 [DRY-RUN] ", false: "✅ "}[dryRun])
	fmt.Printf("   应用: %d\n", len(result.Applied))
	for _, v := range result.Applied {
		fmt.Printf("     %s\n", v)
	}
	if len(result.Failed) > 0 {
		fmt.Printf("   失败: %d\n", len(result.Failed))
		for _, v := range result.Failed {
			fmt.Printf("     %s\n", v)
		}
	}
	fmt.Printf("   耗时: %s\n", result.EndedAt.Sub(result.StartedAt))
	return nil
}

// cmdDown 回滚 N 步（默认 1）。
func cmdDown(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("down", flag.ExitOnError)
	var dsn, driver string
	var dryRun bool
	var steps int
	fs.StringVar(&dsn, "dsn", "", "数据库 DSN")
	fs.StringVar(&driver, "driver", "", "数据库驱动")
	fs.BoolVar(&dryRun, "dry-run", false, "仅报告")
	fs.IntVar(&steps, "steps", 1, "回滚步数")
	if err := fs.Parse(args); err != nil {
		return err
	}

	db, err := openDB(dsn, driver)
	if err != nil {
		return err
	}
	runner := migrator.NewRunner(db, registry)
	result, err := runner.Run(ctx, migrator.RunOptions{
		Direction: migrator.Down,
		Steps:     steps,
		DryRun:    dryRun,
		LockKey:   100420260713,
	})
	if err != nil {
		return err
	}
	fmt.Printf("%sdown 完成\n", map[bool]string{true: "🧪 [DRY-RUN] ", false: "✅ "}[dryRun])
	fmt.Printf("   回滚: %d\n", len(result.Applied))
	for _, v := range result.Applied {
		fmt.Printf("     %s\n", v)
	}
	if len(result.Failed) > 0 {
		fmt.Printf("   失败: %d\n", len(result.Failed))
	}
	return nil
}

// cmdStatus 显示 applied / pending 状态。
func cmdStatus(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	var dsn, driver string
	fs.StringVar(&dsn, "dsn", "", "数据库 DSN")
	fs.StringVar(&driver, "driver", "", "数据库驱动")
	if err := fs.Parse(args); err != nil {
		return err
	}

	db, err := openDB(dsn, driver)
	if err != nil {
		return err
	}
	runner := migrator.NewRunner(db, registry)
	status, err := runner.GetStatus(ctx)
	if err != nil {
		return err
	}

	fmt.Println("=== Migration Status ===")
	fmt.Printf("Total registered: %d\n", status.Total)
	fmt.Printf("Applied: %d\n", len(status.Applied))
	fmt.Printf("Pending: %d\n", len(status.Pending))

	if len(status.Applied) > 0 {
		fmt.Println("\nApplied:")
		for _, r := range status.Applied {
			fmt.Printf("  %s | %s | %s | %dms\n",
				r.Version, r.Status, r.AppliedAt.Format(time.RFC3339), r.DurationMs)
		}
	}
	if len(status.Pending) > 0 {
		fmt.Println("\nPending:")
		for _, m := range status.Pending {
			fmt.Printf("  %s | %s\n", m.Version(), m.Description())
		}
	}
	return nil
}

// cmdCreate 创建新迁移骨架文件。
func cmdCreate(args []string) error {
	fs := flag.NewFlagSet("create", flag.ExitOnError)
	var name string
	fs.StringVar(&name, "name", "", "迁移名称（如 add_users_table）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if name == "" {
		return fmt.Errorf("--name 必填")
	}

	// 时间戳格式 YYYYMMDDHHMMSS
	version := time.Now().UTC().Format("20060102150405")
	filename := fmt.Sprintf("migrations/%s_%s.go", version, name)

	// 确保目录存在
	if err := os.MkdirAll("migrations", 0o755); err != nil {
		return err
	}

	body := fmt.Sprintf(`package migrations

import (
	"context"

	"github.com/vincent-wuhan/opskeeper/internal/migrator"
	"gorm.io/gorm"
)

// Migration_%s 迁移：%s
//
// 在这里写迁移说明。
type Migration_%s struct{}

// Version 实现 migrator.Migration 接口。
func (m *Migration_%s) Version() string { return %q }

// Description 实现 migrator.Migration 接口。
func (m *Migration_%s) Description() string { return %q }

// Up 应用迁移。
func (m *Migration_%s) Up(ctx context.Context, db *gorm.DB) error {
	// TODO: 在这里写 Up 逻辑
	return nil
}

// Down 回滚迁移（如不可回滚返回 migrator.ErrIrreversible）。
func (m *Migration_%s) Down(ctx context.Context, db *gorm.DB) error {
	return migrator.ErrIrreversible
}

func init() {
	migrator.MustRegister(&Migration_%s{})
}
`,
		name, name, name, name, version, name, name, name, name, name)
	if err := os.WriteFile(filename, []byte(body), 0o644); err != nil {
		return err
	}
	fmt.Printf("✅ 创建迁移: %s\n", filename)
	fmt.Printf("   Version: %s\n", version)
	fmt.Printf("   接下来：编辑 %s 实现 Up/Down，然后通过 import _ \"<your-pkg>\" 注册到 opskeeper-migrate-runtime\n", filename)
	return nil
}

// openDB 根据 DSN 自动推断驱动并打开数据库。
func openDB(dsn, driver string) (*gorm.DB, error) {
	if dsn == "" {
		return nil, fmt.Errorf("--dsn 必填")
	}

	if driver == "" {
		driver = inferDriver(dsn)
	}

	cfg := &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	}

	switch driver {
	case "postgres", "postgresql", "pg":
		return gorm.Open(postgres.Open(dsn), cfg)
	case "mysql":
		return gorm.Open(mysql.Open(dsn), cfg)
	case "sqlite", "file":
		return gorm.Open(sqlite.Open(dsn), cfg)
	default:
		return nil, fmt.Errorf("未知驱动: %q", driver)
	}
}

// inferDriver 从 DSN 前缀推断驱动。
func inferDriver(dsn string) string {
	low := strings.ToLower(dsn)
	switch {
	case strings.HasPrefix(low, "postgres://"), strings.HasPrefix(low, "postgresql://"):
		return "postgres"
	case strings.HasPrefix(low, "mysql://"):
		return "mysql"
	case strings.HasSuffix(low, ".db"), strings.HasSuffix(low, ".sqlite"), strings.HasSuffix(low, ".sqlite3"):
		return "sqlite"
	case strings.Contains(low, "host="):
		// postgres keyword-style DSN
		return "postgres"
	default:
		return "sqlite"
	}
}
