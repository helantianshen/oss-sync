package cron

import (
	"context"
	"log"
	"os"

	"github.com/robfig/cron/v3"

	"gorm.io/gorm"

	"github.com/oss/oss-server/internal/config"
)

type Scheduler struct {
	cron *cron.Cron
	cl   *Cleanup
}

func NewScheduler(db *gorm.DB, cfg *config.Config) *Scheduler {
	logger := log.New(os.Stdout, "[OSS cron] ", log.LstdFlags)
	c := cron.New(cron.WithLogger(cron.PrintfLogger(logger)))
	return &Scheduler{
		cron: c,
		cl:   NewCleanup(db, cfg),
	}
}

func (s *Scheduler) Register() {
	spec := "0 0 3 * * *"
	_, _ = s.cron.AddFunc(spec, func() {
		if err := s.cl.PurgeRecycleBin(); err != nil {
			log.Printf("[OSS cron] PurgeRecycleBin error: %v", err)
		}
	})
	_, _ = s.cron.AddFunc(spec, func() {
		if err := s.cl.PurgeOrphanAttachments(); err != nil {
			log.Printf("[OSS cron] PurgeOrphanAttachments error: %v", err)
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

var _ = config.Env
