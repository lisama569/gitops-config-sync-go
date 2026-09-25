// gitops-sync 是本地多环境配置编排与漂移检测的演示入口。
//
// 用法：
//
//	gitops-sync reconcile --config <root> --state <dir> --env <env> [--coordinator id] [--policy Mark|Alert|AlertAndMark]
//	gitops-sync rollback  --config <root> --state <dir> --env <env> [--to-revision n]
//
// 状态与配置全部是本地 JSON 文件，退出码非 0 表示协调失败（错误原因会打印）。
package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/lisama569/gitops-config-sync-go/internal/coordinator"
	"github.com/lisama569/gitops-config-sync-go/internal/drift"
	"github.com/lisama569/gitops-config-sync-go/internal/types"
)

type stdAlerter struct{}

func (stdAlerter) Alert(env string, records []types.DriftRecord) {
	for _, r := range records {
		fmt.Printf("ALERT drift env=%s resource=%s kind=%s\n", env, r.ID, r.Kind)
		for _, d := range r.Diffs {
			fmt.Printf("ALERT   path=%s expected=%v observed=%v\n", d.Path, d.Expected, d.Observed)
		}
	}
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	configRoot := fs.String("config", "config", "config sources root directory")
	stateDir := fs.String("state", ".state", "state directory")
	env := fs.String("env", "", "target environment")
	coordID := fs.String("coordinator", "cli", "coordinator id")
	policy := fs.String("policy", "AlertAndMark", "drift policy: Mark|Alert|AlertAndMark")
	toRevision := fs.Int64("to-revision", 0, "rollback target revision (0 = previous stable)")
	_ = fs.Parse(os.Args[2:])

	if *env == "" {
		fmt.Fprintln(os.Stderr, "--env is required")
		os.Exit(2)
	}
	logger := log.New(os.Stdout, "", log.LstdFlags|log.Lmicroseconds)
	c := coordinator.New(coordinator.Config{
		ID:         *coordID,
		ConfigRoot: *configRoot,
		StateDir:   *stateDir,
		Policy:     drift.Policy(*policy),
		Logger:     logger,
		Alerter:    stdAlerter{},
	})

	switch os.Args[1] {
	case "reconcile":
		res, err := c.Reconcile(*env)
		if err != nil {
			fmt.Fprintf(os.Stderr, "reconcile failed: %v\n", err)
			os.Exit(1)
		}
		switch {
		case res.Skipped:
			fmt.Printf("SKIPPED env=%s lock held by %s\n", *env, res.LockHolder)
		case res.Drifted:
			fmt.Printf("DRIFTED env=%s manual changes marked/alerted, not corrected\n", *env)
		case res.Applied:
			fmt.Printf("APPLIED env=%s stable revision=%d\n", *env, res.Revision)
		default:
			fmt.Printf("IN-SYNC env=%s no changes\n", *env)
		}
	case "rollback":
		res, err := c.Rollback(*env, *toRevision)
		if err != nil {
			fmt.Fprintf(os.Stderr, "rollback failed: %v\n", err)
			os.Exit(1)
		}
		if res.Skipped {
			fmt.Printf("SKIPPED env=%s lock held by %s\n", *env, res.LockHolder)
			os.Exit(1)
		}
		fmt.Printf("ROLLED BACK env=%s stable revision=%d\n", *env, res.Revision)
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: gitops-sync {reconcile|rollback} [flags]")
}
