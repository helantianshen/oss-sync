package cron

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"gorm.io/gorm"

	"github.com/helantianshen/oss-sync/internal/config"
	"github.com/helantianshen/oss-sync/internal/reconcile"
	"github.com/helantianshen/oss-sync/internal/serverbackup"
)

// Scheduler 提供后端周期性任务的注册与生命周期管理
type Scheduler struct {
	cron          *cron.Cron
	cl            *Cleanup
	rc            *reconcile.Reconciler
	mu            sync.Mutex
	pluginEntries map[string][]cron.EntryID
	backups       *serverbackup.Manager
}

func (s *Scheduler) SetBackups(backups *serverbackup.Manager) { s.backups = backups }

func (s *Scheduler) runMaintenance(task func()) {
	if s.backups != nil {
		if !s.backups.TryRead() {
			return
		}
		defer s.backups.EndRead()
	}
	task()
}

// AddPluginTask 向宿主调度器注册受信可执行插件任务
func (s *Scheduler) AddPluginTask(pluginID, name, schedule string, task func()) error {
	if strings.TrimSpace(schedule) == "" || task == nil {
		return errors.New("plugin task schedule and callback are required")
	}
	entryID, err := s.cron.AddFunc(schedule, func() { s.runMaintenance(task) })
	if err != nil {
		return fmt.Errorf("add plugin task %s/%s: %w", pluginID, name, err)
	}
	s.mu.Lock()
	s.pluginEntries[pluginID] = append(s.pluginEntries[pluginID], entryID)
	s.mu.Unlock()
	return nil
}

func (s *Scheduler) RemovePluginTasks(pluginID string) {
	s.mu.Lock()
	entries := s.pluginEntries[pluginID]
	delete(s.pluginEntries, pluginID)
	s.mu.Unlock()
	for _, entryID := range entries {
		s.cron.Remove(entryID)
	}
}

// NewScheduler 创建周期任务调度器
func NewScheduler(db *gorm.DB, cfg *config.Config) *Scheduler {
	logger := log.New(os.Stdout, "[OSS cron] ", log.LstdFlags)
	c := cron.New(cron.WithLogger(cron.PrintfLogger(logger)))
	return &Scheduler{
		cron:          c,
		cl:            NewCleanup(db, cfg),
		rc:            reconcile.New(db, cfg),
		pluginEntries: make(map[string][]cron.EntryID),
	}
}

func (s *Scheduler) Register() {
	if s.backups != nil {
		_, _ = s.cron.AddJob("@every 1m", cron.SkipIfStillRunning(cron.DiscardLogger)(cron.FuncJob(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			if err := s.backups.RunMirrors(ctx); err != nil {
				log.Printf("[OSS cron] mirror configuration error: %v", err)
			}
		})))
	}
	spec := "0 3 * * *"
	add := func(schedule string, task func()) {
		_, _ = s.cron.AddFunc(schedule, func() { s.runMaintenance(task) })
	}
	add(spec, func() {
		if err := s.cl.CompactTombstones(); err != nil {
			log.Printf("[OSS cron] CompactTombstones error: %v", err)
		}
	})
	reconcileSpec := fmt.Sprintf("@every %dh", s.cl.Cfg.Sync.EffectiveReconcileIntervalHours())
	add(reconcileSpec, func() {
		_, err := s.rc.Run(false)
		if err != nil {
			log.Printf("[OSS cron] storage reconciliation error: %v", err)
			return
		}
	})
	add(spec, func() {
		if err := s.cl.PurgeOrphanAttachments(); err != nil {
			log.Printf("[OSS cron] PurgeOrphanAttachments error: %v", err)
		}
	})
	add(spec, func() {
		if err := s.cl.PurgeExpiredHistory(); err != nil {
			log.Printf("[OSS cron] PurgeExpiredHistory error: %v", err)
		}
	})
}

func (s *Scheduler) Start() {
	s.cron.Start()
}

func (s *Scheduler) Stop(ctx context.Context) error {
	if s.cron == nil {
		return nil
	}
	stopCtx := s.cron.Stop()
	if ctx == nil {
		<-stopCtx.Done()
		return nil
	}
	select {
	case <-stopCtx.Done():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Scheduler) Cleanup() *Cleanup { return s.cl }

func (s *Scheduler) Reconciler() *reconcile.Reconciler { return s.rc }
