/*
Copyright 2022 Huawei Cloud Computing Technologies Co., Ltd.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

 http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package engine

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/fasttime"
	"github.com/openGemini/openGemini/engine/immutable"
	"github.com/openGemini/openGemini/lib/statisticsPusher/statistics"
	"go.uber.org/zap"
)

var (
	compWorker           *Compactor
	fullCompColdDuration = uint64(time.Minute.Seconds() * 60)
)

func SetFullCompColdDuration(d time.Duration) {
	if d < time.Minute*2 {
		d = time.Minute * 2
	}

	atomic.StoreUint64(&fullCompColdDuration, uint64(d.Seconds()))
	log.Info("set fullCompColdDuration", zap.Duration("duration", d))
}

func init() {
	compWorker = NewCompactor()
}

type Compactor struct {
	mu sync.RWMutex
	wg sync.WaitGroup

	sources                  map[uint64]*shard
	compactShards            []*shard
	outOfOrderMergeNumberMin int
	outOfOrderMergeSizeMin   int

	plans map[uint64][immutable.CompactLevels]map[string][][]uint64
}

func NewCompactor() *Compactor {
	c := &Compactor{
		sources:                  make(map[uint64]*shard, 32),
		outOfOrderMergeNumberMin: 2,
		outOfOrderMergeSizeMin:   1 * 1024 * 1024,
		plans:                    make(map[uint64][immutable.CompactLevels]map[string][][]uint64, 8),
	}

	go c.run()

	return c
}

func (c *Compactor) RegisterShard(sh *shard) {
	c.wg.Add(1)
	c.mu.Lock()
	c.sources[sh.ident.ShardID] = sh
	c.mu.Unlock()
}

func (c *Compactor) UnregisterShard(shardId uint64) {
	c.wg.Done()
	c.mu.Lock()
	delete(c.sources, shardId)
	c.mu.Unlock()
}

func (c *Compactor) merger() {
	go c.statOutOfOrderFiles()
	if !immutable.EnableMergeOutOfOrder {
		return
	}

	c.compactShards = c.compactShards[:0]
	c.mu.RLock()
	for _, v := range c.sources {
		c.compactShards = append(c.compactShards, v)
	}
	c.mu.RUnlock()

	for _, sh := range c.compactShards {
		if !sh.immTables.MergeEnabled() {
			continue
		}

		id := sh.GetID()
		select {
		case <-sh.closed.Signal():
			log.Info("closed", zap.Uint64("shardId", id))
			return
		default:
			log.Info("begin merge out of order files", zap.Uint64("shardId", id))
			_ = sh.immTables.MergeOutOfOrder(id)
		}
	}
}

func (c *Compactor) compact() {
	c.compactShards = c.compactShards[:0]
	c.mu.RLock()
	for _, v := range c.sources {
		c.compactShards = append(c.compactShards, v)
	}
	c.mu.RUnlock()

	for _, sh := range c.compactShards {
		id := sh.GetID()
		select {
		case <-sh.closed.Signal():
			log.Info("closed", zap.Uint64("shardId", id))
			return
		default:
			if !sh.immTables.CompactionEnabled() {
				return
			}
			nowTime := fasttime.UnixTimestamp()
			lastWrite := sh.LastWriteTime()
			d := nowTime - lastWrite
			if d >= atomic.LoadUint64(&fullCompColdDuration) {
				if err := sh.immTables.FullCompact(id); err != nil {
					log.Error("full compact error", zap.Uint64("shid", id), zap.Error(err))
				}
				continue
			}

			for _, level := range immutable.LevelCompactRule {
				if err := sh.immTables.LevelCompact(level, id); err != nil {
					log.Error("level compact error", zap.Uint64("shid", id), zap.Error(err))
					continue
				}
			}
		}
	}
}

func (c *Compactor) run() {
	tm := time.NewTicker(time.Second * 30)
	defer tm.Stop()
	for range tm.C {
		c.merger()
		c.compact()
	}
}

func (c *Compactor) ShardCompactionSwitch(shid uint64, en bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	sh, ok := c.sources[shid]
	if !ok {
		return
	}

	if en {
		sh.immTables.CompactionEnable()
	} else {
		sh.immTables.CompactionDisable()
	}
}

func (c *Compactor) SetAllShardsCompactionSwitch(en bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for _, sh := range c.sources {
		if en {
			sh.immTables.CompactionEnable()
		} else {
			sh.immTables.CompactionDisable()
		}
	}
}

func (c *Compactor) SetAllOutOfOrderMergeSwitch(en bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	immutable.EnableMergeOutOfOrder = en

	for _, sh := range c.sources {
		if en {
			sh.immTables.MergeEnable()
		} else {
			sh.immTables.MergeDisable()
		}
	}
}

func (c *Compactor) ShardOutOfOrderMergeSwitch(shid uint64, en bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	sh, ok := c.sources[shid]
	if !ok {
		return
	}

	if en {
		sh.immTables.MergeEnable()
	} else {
		sh.immTables.MergeDisable()
	}
}

func (c *Compactor) SetSnapshotColdDuration(d time.Duration) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for _, sh := range c.sources {
		sh.writeColdDuration = d
	}
}

func (c *Compactor) statOutOfOrderFiles() {
	total := 0
	c.mu.RLock()
	for _, v := range c.sources {
		total += v.immTables.GetOutOfOrderFileNum()
	}
	c.mu.RUnlock()
	statistics.NewMergeStatistics().SetCurrentOutOfOrderFile(int64(total))
}
