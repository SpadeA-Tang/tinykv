// Copyright 2017 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package schedulers

import (
	"github.com/pingcap-incubator/tinykv/scheduler/server/core"
	"github.com/pingcap-incubator/tinykv/scheduler/server/schedule"
	"github.com/pingcap-incubator/tinykv/scheduler/server/schedule/operator"
	"github.com/pingcap-incubator/tinykv/scheduler/server/schedule/opt"
	"sort"
)

func init() {
	schedule.RegisterSliceDecoderBuilder("balance-region", func(args []string) schedule.ConfigDecoder {
		return func(v interface{}) error {
			return nil
		}
	})
	schedule.RegisterScheduler("balance-region", func(opController *schedule.OperatorController, storage *core.Storage, decoder schedule.ConfigDecoder) (schedule.Scheduler, error) {
		return newBalanceRegionScheduler(opController), nil
	})
}

const (
	// balanceRegionRetryLimit is the limit to retry schedule for selected store.
	balanceRegionRetryLimit = 10
	balanceRegionName       = "balance-region-scheduler"
)

type balanceRegionScheduler struct {
	*baseScheduler
	name         string
	opController *schedule.OperatorController
}

// newBalanceRegionScheduler creates a scheduler that tends to keep regions on
// each store balanced.
func newBalanceRegionScheduler(opController *schedule.OperatorController, opts ...BalanceRegionCreateOption) schedule.Scheduler {
	base := newBaseScheduler(opController)
	s := &balanceRegionScheduler{
		baseScheduler: base,
		opController:  opController,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// BalanceRegionCreateOption is used to create a scheduler with an option.
type BalanceRegionCreateOption func(s *balanceRegionScheduler)

func (s *balanceRegionScheduler) GetName() string {
	if s.name != "" {
		return s.name
	}
	return balanceRegionName
}

func (s *balanceRegionScheduler) GetType() string {
	return "balance-region"
}

func (s *balanceRegionScheduler) IsScheduleAllowed(cluster opt.Cluster) bool {
	return s.opController.OperatorCount(operator.OpRegion) < cluster.GetRegionScheduleLimit()
}

func getRegion(cluster opt.Cluster, fromIdx, toIdx uint64) *core.RegionInfo {
	var region *core.RegionInfo
	cluster.GetPendingRegionsWithLock(fromIdx, func(container core.RegionsContainer) {
		region = container.RandomRegion([]byte(""), []byte(""))
	})
	if region != nil {
		// not nil means the region is already in the toStore
		if region.GetStorePeer(toIdx) == nil {
			return region
		}
	}
	cluster.GetFollowersWithLock(fromIdx, func(container core.RegionsContainer) {
		region = container.RandomRegion([]byte(""), []byte(""))
	})
	if region != nil {
		if region.GetStorePeer(toIdx) == nil {
			return region
		}
	}
	cluster.GetLeadersWithLock(fromIdx, func(container core.RegionsContainer) {
		region = container.RandomRegion([]byte(""), []byte(""))
	})
	if region != nil {
		if region.GetStorePeer(toIdx) == nil {
			return region
		}
	}
	return nil
}

func (s *balanceRegionScheduler) Schedule(cluster opt.Cluster) *operator.Operator {
	// Your Code Here (3C).
	stores := (SortStoreInfo)(cluster.GetStores())
	sort.Sort(&stores)
	for fromStoreIdx := stores.Len() - 1; fromStoreIdx >= 0; fromStoreIdx-- {
		fromStore := stores[fromStoreIdx]
		if !isSuitable(fromStore, cluster) {
			continue
		}

		var region *core.RegionInfo
		var targetIdx uint64
		// find a store to move region to
		for toStoreIdx := 0; toStoreIdx < fromStoreIdx; toStoreIdx++ {
			toStore := stores[toStoreIdx]

			if !isSuitable(toStore, cluster) {
				continue
			}
			// select a qualified region to transfer
			region = getRegion(cluster, fromStore.GetID(), toStore.GetID())
			if region != nil {
				if len(region.GetPeers()) < cluster.GetMaxReplicas() {
					region = nil
					continue
				}
				if fromStore.GetRegionSize()-toStore.GetRegionSize() > 2*region.GetApproximateSize() {
					targetIdx = toStore.GetID()
					break
				}
			}
		}

		if region != nil {
			pr, err := cluster.AllocPeer(targetIdx)
			if err != nil {
				panic(err)
			}
			op, err := operator.CreateMovePeerOperator("",
				cluster, region, operator.OpBalance, fromStore.GetID(), targetIdx, pr.GetId())
			if err != nil {
				panic(err)
			}
			return op
		}
	}
	return nil
}

func isSuitable(store *core.StoreInfo, cluster opt.Cluster) bool {
	if !store.IsUp() {
		return false
	}
	if store.DownTime() >= cluster.GetMaxStoreDownTime() {
		return false
	}
	return true
}

type SortStoreInfo []*core.StoreInfo

func (s SortStoreInfo) Less(i int, j int) bool {
	return s[i].GetRegionSize() < s[j].GetRegionSize()
}

func (s SortStoreInfo) Len() int {
	return len(s)
}

func (s SortStoreInfo) Swap(i int, j int) {
	s[i], s[j] = s[j], s[i]
}
