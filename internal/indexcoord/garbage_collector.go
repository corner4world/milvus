// Licensed to the LF AI & Data foundation under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package indexcoord

import (
	"context"
	"path"
	"sync"
	"time"

	"github.com/milvus-io/milvus/internal/util/metautil"

	"go.uber.org/zap"

	"github.com/milvus-io/milvus-proto/go-api/commonpb"
	"github.com/milvus-io/milvus/internal/common"
	"github.com/milvus-io/milvus/internal/log"
	"github.com/milvus-io/milvus/internal/metastore/model"
	"github.com/milvus-io/milvus/internal/proto/datapb"
	"github.com/milvus-io/milvus/internal/storage"
)

type garbageCollector struct {
	ctx    context.Context
	cancel context.CancelFunc

	wg             sync.WaitGroup
	gcFileDuration time.Duration
	gcMetaDuration time.Duration

	metaTable        *metaTable
	chunkManager     storage.ChunkManager
	indexCoordClient *IndexCoord
}

func newGarbageCollector(ctx context.Context, meta *metaTable, chunkManager storage.ChunkManager, ic *IndexCoord) *garbageCollector {
	ctx, cancel := context.WithCancel(ctx)
	return &garbageCollector{
		ctx:              ctx,
		cancel:           cancel,
		gcFileDuration:   Params.IndexCoordCfg.GCInterval.GetAsDuration(time.Second),
		gcMetaDuration:   time.Minute,
		metaTable:        meta,
		chunkManager:     chunkManager,
		indexCoordClient: ic,
	}
}

func (gc *garbageCollector) Start() {
	//gc.wg.Add(1)
	//go gc.recycleUnusedIndexes()
	//
	//gc.wg.Add(1)
	//go gc.recycleUnusedSegIndexes()
	//
	//gc.wg.Add(1)
	//go gc.recycleUnusedIndexFiles()
}

func (gc *garbageCollector) Stop() {
	gc.cancel()
	gc.wg.Wait()
}

func (gc *garbageCollector) recycleUnusedIndexes() {
	defer gc.wg.Done()
	log.Ctx(gc.ctx).Info("IndexCoord garbageCollector recycleUnusedIndexes start")

	ticker := time.NewTicker(gc.gcMetaDuration)
	defer ticker.Stop()
	for {
		select {
		case <-gc.ctx.Done():
			log.Ctx(gc.ctx).Info("IndexCoord garbageCollector recycleUnusedMetaLoop context has done")
			return
		case <-ticker.C:
			deletedIndexes := gc.metaTable.GetDeletedIndexes()
			for _, index := range deletedIndexes {
				buildIDs := gc.metaTable.GetBuildIDsFromIndexID(index.IndexID)
				if len(buildIDs) == 0 {
					if err := gc.metaTable.RemoveIndex(index.CollectionID, index.IndexID); err != nil {
						log.Ctx(gc.ctx).Warn("IndexCoord remove index on collection fail", zap.Int64("collID", index.CollectionID),
							zap.Int64("indexID", index.IndexID), zap.Error(err))
						continue
					}
				} else {
					for _, buildID := range buildIDs {
						segIdx, ok := gc.metaTable.GetMeta(buildID)
						if !ok {
							log.Ctx(gc.ctx).Debug("IndexCoord get segment index is not exist", zap.Int64("buildID", buildID))
							continue
						}
						if segIdx.NodeID != 0 {
							// wait for releasing reference lock
							continue
						}
						if err := gc.metaTable.RemoveSegmentIndex(segIdx.CollectionID, segIdx.PartitionID, segIdx.SegmentID, segIdx.BuildID); err != nil {
							log.Ctx(gc.ctx).Warn("delete index meta from etcd failed, wait to retry", zap.Int64("buildID", segIdx.BuildID),
								zap.Int64("nodeID", segIdx.NodeID), zap.Error(err))
							continue
						}
						log.Ctx(gc.ctx).Info("IndexCoord remove segment index meta success", zap.Int64("buildID", segIdx.BuildID),
							zap.Int64("nodeID", segIdx.NodeID))
					}
					log.Ctx(gc.ctx).Info("garbageCollector remove index success", zap.Int64("collID", index.CollectionID),
						zap.Int64("indexID", index.IndexID))
				}
			}
		}
	}
}

func (gc *garbageCollector) recycleSegIndexesMeta() {
	gc.indexCoordClient.indexGCLock.Lock()
	segIndexes := gc.metaTable.GetAllSegIndexes()
	gc.indexCoordClient.indexGCLock.Unlock()

	collID2segID := make(map[int64]map[int64]struct{})
	for segID, segIdx := range segIndexes {
		if _, ok := collID2segID[segIdx.CollectionID]; !ok {
			collID2segID[segIdx.CollectionID] = make(map[int64]struct{})
		}
		collID2segID[segIdx.CollectionID][segID] = struct{}{}
	}
	for collID, segIDs := range collID2segID {
		resp, err := gc.indexCoordClient.dataCoordClient.GetFlushedSegments(gc.ctx, &datapb.GetFlushedSegmentsRequest{
			CollectionID:     collID,
			PartitionID:      -1,
			IncludeUnhealthy: true,
		})
		if err != nil {
			log.Ctx(gc.ctx).Warn("IndexCoord garbageCollector get flushed segments from DataCoord fail",
				zap.Int64("collID", collID), zap.Error(err))
			return
		}
		if resp.Status.ErrorCode != commonpb.ErrorCode_Success {
			log.Ctx(gc.ctx).Warn("IndexCoord garbageCollector get flushed segments from DataCoord fail", zap.Int64("collID", collID),
				zap.String("fail reason", resp.Status.Reason))
			return
		}
		flushedSegments := make(map[int64]struct{})
		for _, segID := range resp.Segments {
			flushedSegments[segID] = struct{}{}
		}
		for segID := range segIDs {
			if segIndexes[segID].IsDeleted {
				continue
			}
			if _, ok := flushedSegments[segID]; !ok {
				log.Ctx(gc.ctx).Info("segment is already not exist, mark it deleted", zap.Int64("collID", collID),
					zap.Int64("segID", segID))
				if err := gc.metaTable.MarkSegmentsIndexAsDeleted(func(segIndex *model.SegmentIndex) bool {
					return segIndex.SegmentID == segID
				}); err != nil {
					continue
				}
			}
		}
	}
	//segIndexes := gc.metaTable.GetDeletedSegmentIndexes()
	for _, meta := range segIndexes {
		if meta.IsDeleted || gc.metaTable.IsIndexDeleted(meta.CollectionID, meta.IndexID) {
			if meta.NodeID != 0 {
				// wait for releasing reference lock
				continue
			}
			if err := gc.metaTable.RemoveSegmentIndex(meta.CollectionID, meta.PartitionID, meta.SegmentID, meta.BuildID); err != nil {
				log.Ctx(gc.ctx).Warn("delete index meta from etcd failed, wait to retry", zap.Int64("buildID", meta.BuildID),
					zap.Int64("nodeID", meta.NodeID), zap.Error(err))
				continue
			}
			log.Ctx(gc.ctx).Info("index meta recycle success", zap.Int64("buildID", meta.BuildID),
				zap.Int64("segID", meta.SegmentID))
		}
	}
}

func (gc *garbageCollector) recycleUnusedSegIndexes() {
	defer gc.wg.Done()
	log.Ctx(gc.ctx).Info("IndexCoord garbageCollector recycleUnusedSegIndexes start")

	ticker := time.NewTicker(gc.gcMetaDuration)
	defer ticker.Stop()

	for {
		select {
		case <-gc.ctx.Done():
			log.Ctx(gc.ctx).Info("IndexCoord garbageCollector recycleUnusedMetaLoop context has done")
			return
		case <-ticker.C:
			gc.recycleSegIndexesMeta()
		}
	}
}

// recycleUnusedIndexFiles is used to delete those index files that no longer exist in the meta.
func (gc *garbageCollector) recycleUnusedIndexFiles() {
	defer gc.wg.Done()
	log.Ctx(gc.ctx).Info("IndexCoord garbageCollector start recycleUnusedIndexFiles loop")

	ticker := time.NewTicker(gc.gcFileDuration)
	defer ticker.Stop()

	for {
		select {
		case <-gc.ctx.Done():
			return
		case <-ticker.C:
			prefix := path.Join(gc.chunkManager.RootPath(), common.SegmentIndexPath) + "/"
			// list dir first
			keys, _, err := gc.chunkManager.ListWithPrefix(gc.ctx, prefix, false)
			if err != nil {
				log.Ctx(gc.ctx).Error("IndexCoord garbageCollector recycleUnusedIndexFiles list keys from chunk manager failed", zap.Error(err))
				continue
			}
			for _, key := range keys {
				log.Ctx(gc.ctx).Debug("indexFiles keys", zap.String("key", key))
				buildID, err := parseBuildIDFromFilePath(key)
				if err != nil {
					log.Ctx(gc.ctx).Error("IndexCoord garbageCollector recycleUnusedIndexFiles parseIndexFileKey", zap.String("key", key), zap.Error(err))
					continue
				}
				log.Ctx(gc.ctx).Info("IndexCoord garbageCollector will recycle index files", zap.Int64("buildID", buildID))
				if !gc.metaTable.HasBuildID(buildID) {
					// buildID no longer exists in meta, remove all index files
					log.Ctx(gc.ctx).Info("IndexCoord garbageCollector recycleUnusedIndexFiles find meta has not exist, remove index files",
						zap.Int64("buildID", buildID))
					err = gc.chunkManager.RemoveWithPrefix(gc.ctx, key)
					if err != nil {
						log.Ctx(gc.ctx).Warn("IndexCoord garbageCollector recycleUnusedIndexFiles remove index files failed",
							zap.Int64("buildID", buildID), zap.String("prefix", key), zap.Error(err))
						continue
					}
					continue
				}
				log.Ctx(gc.ctx).Info("index meta can be recycled, recycle index files", zap.Int64("buildID", buildID))
				canRecycle, segIdx := gc.metaTable.GetSegmentIndexByBuildID(buildID)
				if !canRecycle {
					// Even if the index is marked as deleted, the index file will not be recycled, wait for the next gc,
					// and delete all index files about the buildID at one time.
					log.Ctx(gc.ctx).Warn("IndexCoord garbageCollector can not recycle index files", zap.Int64("buildID", buildID))
					continue
				}
				filesMap := make(map[string]struct{})
				for _, fileID := range segIdx.IndexFileKeys {
					filepath := metautil.BuildSegmentIndexFilePath(gc.chunkManager.RootPath(), segIdx.BuildID, segIdx.IndexVersion,
						segIdx.PartitionID, segIdx.SegmentID, fileID)
					filesMap[filepath] = struct{}{}
				}
				files, _, err := gc.chunkManager.ListWithPrefix(gc.ctx, key, true)
				if err != nil {
					log.Ctx(gc.ctx).Warn("IndexCoord garbageCollector recycleUnusedIndexFiles list files failed",
						zap.Int64("buildID", buildID), zap.String("prefix", key), zap.Error(err))
					continue
				}
				log.Ctx(gc.ctx).Info("recycle index files", zap.Int64("buildID", buildID), zap.Int("meta files num", len(filesMap)),
					zap.Int("chunkManager files num", len(files)))
				deletedFilesNum := 0
				for _, file := range files {
					if _, ok := filesMap[file]; !ok {
						if err = gc.chunkManager.Remove(gc.ctx, file); err != nil {
							log.Ctx(gc.ctx).Warn("IndexCoord garbageCollector recycleUnusedIndexFiles remove file failed",
								zap.Int64("buildID", buildID), zap.String("file", file), zap.Error(err))
							continue
						}
						deletedFilesNum++
					}
				}
				log.Ctx(gc.ctx).Info("index files recycle success", zap.Int64("buildID", buildID),
					zap.Int("delete index files num", deletedFilesNum))
			}
		}
	}
}
