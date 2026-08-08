package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/robfig/cron/v3"
	_ "time/tzdata"
)

func main() {
	cfg := loadConfig()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.schedule != "" {
		fmt.Printf("正在按计划 '%s' 运行备份。\n", cfg.schedule)
		runScheduled(ctx, cfg)
		return
	}

	if err := backup(ctx, cfg, time.Now()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runScheduled(ctx context.Context, cfg *config) {
	schedule, err := cron.ParseStandard(cfg.schedule)
	if err != nil {
		fmt.Printf("无效的备份计划: %s\n", cfg.schedule)
		return
	}
	for {
		next := schedule.Next(time.Now())
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if err := backup(ctx, cfg, time.Now()); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
}
