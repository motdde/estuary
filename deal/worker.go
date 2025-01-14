package deal

import (
	"context"
	"time"

	"github.com/application-research/estuary/model"
	"github.com/application-research/estuary/util"
	"gorm.io/gorm"
)

func (m *manager) runWorkers(ctx context.Context) {
	m.log.Infof("starting up deal workers")

	go m.runDealBackFillWorker(ctx)

	go m.runDealCheckWorker(ctx)

	go m.runDealWorker(ctx)

	m.log.Infof("spun up deal workers")
}

func (m *manager) runDealBackFillWorker(ctx context.Context) {
	// init tracker before work starts
	tracker, err := m.getQueueTracker()
	if err != nil {
		m.log.Warnf("failed to get deal queue tracker - %s", err)
	}

	if tracker.BackfillDone {
		return
	}

	timer := time.NewTicker(m.cfg.WorkerIntervals.DealInterval)
	for {
		select {
		case <-ctx.Done():
			m.log.Info("shutting down deal backfill worker")
			return
		case <-timer.C:
			m.log.Debug("running deal backfill worker")

			tracker, err = m.getQueueTracker()
			if err != nil {
				m.log.Warnf("failed to get deal queue tracker - %s", err)
				continue
			}

			if tracker.BackfillDone {
				m.log.Info("deal queue backfill is done")
				return
			}

			m.log.Debugf("trying to start deal queue backfill, starting from content: %d", tracker.LastContID)

			var contents []*util.Content
			if err := m.db.Where("size >= ? and size <= ? and active", m.cfg.Content.MinSize, m.cfg.Content.MaxSize).Order("id asc").Limit(2000).Find(&contents).Error; err != nil {
				m.log.Warnf("failed to get contents for deal queue backfill - %s", err)
				continue
			}

			m.log.Debugf("trying to backfill deal queue for total of %d contents", len(contents))
			for _, c := range contents {
				if err := m.backfillQueue(c, tracker); err != nil {
					m.log.Warnf("failed to backfill deal queue for cont: %d - %s", c.ID, err)
					break
				}
			}

			// if there are no more to backfill set stop
			if len(contents) == 0 {
				if err := m.db.Model(model.DealQueueTracker{}).Where("id = ?", tracker.ID).UpdateColumn("stop_at", tracker.LastContID).Error; err != nil {
					m.log.Warnf("failed to set stop_at for deal queue tracker - %s", err)
				}
			}
		}
	}
}

func (m *manager) runDealWorker(ctx context.Context) {
	timer := time.NewTicker(m.cfg.WorkerIntervals.DealInterval)
	for {
		select {
		case <-ctx.Done():
			m.log.Info("shutting down deal check worker")
			return
		case <-timer.C:
			m.log.Debug("running deal worker")

			var tasks []*model.DealQueue
			if err := m.db.Where("commp_done and can_deal and deal_next_attempt_at < ?", time.Now().UTC()).Order("id asc").FindInBatches(&tasks, 2000, func(tx *gorm.DB, batch int) error {
				m.log.Debugf("trying to make deals for total of %d contents", len(tasks))
				for _, t := range tasks {
					m.log.Debugf("making %d deal(s) for content: %d", t.DealCount, t.ContID)
					if err := m.makeDealsForContent(ctx, t.ContID, t.DealCount); err != nil {
						m.log.Errorf("failed to make more deals for cont: %d - %s", t.ContID, err)
						m.dealQueueMgr.DealFailed(t.ContID, m.db)
						continue
					}
					m.dealQueueMgr.DealComplete(t.ContID, m.db)
				}
				return nil
			}).Error; err != nil {
				m.log.Warnf("failed to make content deals - %s", err)
			}
		}
	}
}

func (m *manager) runDealCheckWorker(ctx context.Context) {
	timer := time.NewTicker(m.cfg.WorkerIntervals.DealInterval)
	for {
		select {
		case <-ctx.Done():
			m.log.Info("shutting down deal check worker")
			return
		case <-timer.C:
			m.log.Debug("running deal check worker")

			var tasks []*model.DealQueue
			if err := m.db.Where("commp_done and not can_deal and deal_check_next_attempt_at < ?", time.Now().UTC()).Order("id asc").FindInBatches(&tasks, 2000, func(tx *gorm.DB, batch int) error {
				m.log.Debugf("trying to check %d deals", len(tasks))
				for _, t := range tasks {
					dealsToBeMade, err := m.checkContentDeals(ctx, t.ContID)
					if err != nil {
						m.log.Warnf("failed to check cont %d deals - %s", t.ContID, err)
						m.dealQueueMgr.DealCheckFailed(t.ContID, m.db)
						continue
					}
					m.dealQueueMgr.DealCheckComplete(t.ContID, dealsToBeMade, m.db)
				}
				return nil
			}).Error; err != nil {
				m.log.Warnf("failed to check content deals - %s", err)
			}
		}
	}
}

func (m *manager) getQueueTracker() (*model.DealQueueTracker, error) {
	var trks []*model.DealQueueTracker
	if err := m.db.Find(&trks).Error; err != nil {
		return nil, err
	}

	if len(trks) > 0 && trks[0].BackfillDone {
		return trks[0], nil
	}

	var trk *model.DealQueueTracker
	if len(trks) == 0 {
		var contents []*util.Content
		if err := m.db.Where("size > 0").Order("id desc").Limit(1).Find(&contents).Error; err != nil {
			return nil, err
		}

		stopAt := uint64(0)
		if len(contents) > 0 {
			stopAt = contents[0].ID
		}

		trk = &model.DealQueueTracker{LastContID: 0, StopAt: stopAt}
		if err := m.db.Create(&trk).Error; err != nil {
			return nil, err
		}
	} else {
		trk = trks[0]
	}

	if trk.LastContID >= trk.StopAt {
		m.log.Info("deal queue backfill is done")
		if err := m.db.Model(model.DealQueueTracker{}).Where("id = ?", trk.ID).UpdateColumn("backfill_done", true).Error; err != nil {
			m.log.Warnf("failed to mark deal queue backfill as done - %s", err)
			return nil, err
		}
		trk.BackfillDone = true
	}
	return trk, nil
}

func (m *manager) backfillQueue(cont *util.Content, tracker *model.DealQueueTracker) error {
	m.log.Debugf("trying to backfill deal queue for content %d", cont.ID)

	if err := m.dealQueueMgr.QueueContent(cont.ID, m.db); err != nil {
		return err
	}
	return m.db.Model(model.DealQueueTracker{}).Where("id = ?", tracker.ID).UpdateColumn("last_cont_id", cont.ID).Error
}
