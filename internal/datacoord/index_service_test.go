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

package datacoord

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/milvus-io/milvus-proto/go-api/commonpb"
	"github.com/milvus-io/milvus/internal/metastore/kv/datacoord"
	"github.com/milvus-io/milvus/internal/metastore/model"
	"github.com/milvus-io/milvus/internal/proto/datapb"
	"github.com/milvus-io/milvus/internal/storage"
)

func TestServer_CreateIndex(t *testing.T) {
	var (
		collID  = UniqueID(1)
		fieldID = UniqueID(10)
		//indexID    = UniqueID(100)
		indexName  = "default_idx"
		typeParams = []*commonpb.KeyValuePair{
			{
				Key:   "dim",
				Value: "128",
			},
		}
		indexParams = []*commonpb.KeyValuePair{
			{
				Key:   "index_type",
				Value: "IVF_FLAT",
			},
		}
		req = &datapb.CreateIndexRequest{
			CollectionID:    collID,
			FieldID:         fieldID,
			IndexName:       indexName,
			TypeParams:      typeParams,
			IndexParams:     indexParams,
			Timestamp:       100,
			IsAutoIndex:     false,
			UserIndexParams: indexParams,
		}
		ctx = context.Background()
	)
	s := &Server{
		meta: &meta{
			catalog: &datacoord.Catalog{Txn: &mockEtcdKv{}},
			indexes: map[UniqueID]map[UniqueID]*model.Index{},
		},
		allocator:       newMockAllocator(),
		notifyIndexChan: make(chan UniqueID, 1),
	}
	s.stateCode.Store(commonpb.StateCode_Healthy)
	t.Run("success", func(t *testing.T) {
		resp, err := s.CreateIndex(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_Success, resp.GetErrorCode())
	})

	t.Run("server not healthy", func(t *testing.T) {
		s.stateCode.Store(commonpb.StateCode_Abnormal)
		resp, err := s.CreateIndex(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_DataCoordNA, resp.GetErrorCode())
	})

	t.Run("index not consistent", func(t *testing.T) {
		s.stateCode.Store(commonpb.StateCode_Healthy)
		req.FieldID++
		resp, err := s.CreateIndex(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_UnexpectedError, resp.GetErrorCode())
	})

	t.Run("alloc ID fail", func(t *testing.T) {
		req.FieldID = fieldID
		s.allocator = &FailsAllocator{allocIDSucceed: false}
		s.meta.indexes = map[UniqueID]map[UniqueID]*model.Index{}
		resp, err := s.CreateIndex(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_UnexpectedError, resp.GetErrorCode())
	})

	t.Run("not support disk index", func(t *testing.T) {
		s.allocator = newMockAllocator()
		s.meta.indexes = map[UniqueID]map[UniqueID]*model.Index{}
		req.IndexParams = []*commonpb.KeyValuePair{
			{
				Key:   "index_type",
				Value: "DISKANN",
			},
		}
		s.indexNodeManager = NewNodeManager(ctx)
		resp, err := s.CreateIndex(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_UnexpectedError, resp.GetErrorCode())
	})

	t.Run("save index fail", func(t *testing.T) {
		s.meta.indexes = map[UniqueID]map[UniqueID]*model.Index{}
		s.meta.catalog = &datacoord.Catalog{Txn: &saveFailKV{}}
		req.IndexParams = []*commonpb.KeyValuePair{
			{
				Key:   "index_type",
				Value: "IVF_FLAT",
			},
		}
		resp, err := s.CreateIndex(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_UnexpectedError, resp.GetErrorCode())
	})
}

func TestServer_GetIndexState(t *testing.T) {
	var (
		collID     = UniqueID(1)
		partID     = UniqueID(2)
		fieldID    = UniqueID(10)
		indexID    = UniqueID(100)
		segID      = UniqueID(1000)
		indexName  = "default_idx"
		typeParams = []*commonpb.KeyValuePair{
			{
				Key:   "dim",
				Value: "128",
			},
		}
		indexParams = []*commonpb.KeyValuePair{
			{
				Key:   "index_type",
				Value: "IVF_FLAT",
			},
		}
		createTS = uint64(1000)
		ctx      = context.Background()
		req      = &datapb.GetIndexStateRequest{
			CollectionID: collID,
			IndexName:    "",
		}
	)
	s := &Server{
		meta: &meta{
			catalog: &datacoord.Catalog{Txn: &mockEtcdKv{}},
		},
		allocator:       newMockAllocator(),
		notifyIndexChan: make(chan UniqueID, 1),
	}

	t.Run("server not available", func(t *testing.T) {
		s.stateCode.Store(commonpb.StateCode_Initializing)
		resp, err := s.GetIndexState(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_DataCoordNA, resp.GetStatus().GetErrorCode())
	})

	s.stateCode.Store(commonpb.StateCode_Healthy)
	t.Run("index not exist", func(t *testing.T) {
		resp, err := s.GetIndexState(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_IndexNotExist, resp.GetStatus().GetErrorCode())
	})

	s.meta = &meta{
		catalog: &datacoord.Catalog{Txn: &mockEtcdKv{}},
		indexes: map[UniqueID]map[UniqueID]*model.Index{
			collID: {
				indexID: {
					TenantID:        "",
					CollectionID:    collID,
					FieldID:         fieldID,
					IndexID:         indexID,
					IndexName:       indexName,
					IsDeleted:       false,
					CreateTime:      createTS,
					TypeParams:      typeParams,
					IndexParams:     indexParams,
					IsAutoIndex:     false,
					UserIndexParams: nil,
				},
			},
		},
		segments: &SegmentsInfo{map[UniqueID]*SegmentInfo{
			segID: {
				SegmentInfo: &datapb.SegmentInfo{
					ID:             segID,
					CollectionID:   collID,
					PartitionID:    partID,
					InsertChannel:  "",
					NumOfRows:      10250,
					State:          commonpb.SegmentState_Flushed,
					MaxRowNum:      65536,
					LastExpireTime: createTS - 1,
				},
				segmentIndexes:  nil,
				currRows:        0,
				allocations:     nil,
				lastFlushTime:   time.Time{},
				isCompacting:    false,
				size:            0,
				lastWrittenTime: time.Time{},
			},
		}},
	}

	t.Run("index state is unissued", func(t *testing.T) {
		resp, err := s.GetIndexState(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_Success, resp.GetStatus().GetErrorCode())
	})

	t.Run("ambiguous index name", func(t *testing.T) {
		s.meta.indexes[collID][indexID+1] = &model.Index{
			TenantID:        "",
			CollectionID:    collID,
			FieldID:         fieldID,
			IndexID:         indexID + 1,
			IndexName:       "default_idx_1",
			IsDeleted:       false,
			CreateTime:      createTS,
			TypeParams:      typeParams,
			IndexParams:     indexParams,
			IsAutoIndex:     false,
			UserIndexParams: nil,
		}
		resp, err := s.GetIndexState(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_UnexpectedError, resp.GetStatus().GetErrorCode())
	})
}

func TestServer_GetSegmentIndexState(t *testing.T) {
	var (
		collID     = UniqueID(1)
		partID     = UniqueID(2)
		fieldID    = UniqueID(10)
		indexID    = UniqueID(100)
		segID      = UniqueID(1000)
		indexName  = "default_idx"
		typeParams = []*commonpb.KeyValuePair{
			{
				Key:   "dim",
				Value: "128",
			},
		}
		indexParams = []*commonpb.KeyValuePair{
			{
				Key:   "index_type",
				Value: "IVF_FLAT",
			},
		}
		createTS = uint64(1000)
		ctx      = context.Background()
		req      = &datapb.GetSegmentIndexStateRequest{
			CollectionID: collID,
			IndexName:    "",
			SegmentIDs:   []UniqueID{segID},
		}
	)
	s := &Server{
		meta: &meta{
			catalog:  &datacoord.Catalog{Txn: &mockEtcdKv{}},
			indexes:  map[UniqueID]map[UniqueID]*model.Index{},
			segments: &SegmentsInfo{map[UniqueID]*SegmentInfo{}},
		},
		allocator:       newMockAllocator(),
		notifyIndexChan: make(chan UniqueID, 1),
	}

	t.Run("server is not available", func(t *testing.T) {
		s.stateCode.Store(commonpb.StateCode_Abnormal)
		resp, err := s.GetSegmentIndexState(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_DataCoordNA, resp.GetStatus().GetErrorCode())
	})

	t.Run("no indexes", func(t *testing.T) {
		s.stateCode.Store(commonpb.StateCode_Healthy)
		resp, err := s.GetSegmentIndexState(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_IndexNotExist, resp.GetStatus().GetErrorCode())
	})

	t.Run("unfinished", func(t *testing.T) {
		s.meta.indexes[collID] = map[UniqueID]*model.Index{
			indexID: {
				TenantID:        "",
				CollectionID:    collID,
				FieldID:         fieldID,
				IndexID:         indexID,
				IndexName:       indexName,
				IsDeleted:       false,
				CreateTime:      createTS,
				TypeParams:      typeParams,
				IndexParams:     indexParams,
				IsAutoIndex:     false,
				UserIndexParams: nil,
			},
		}
		s.meta.segments.segments[segID] = &SegmentInfo{
			SegmentInfo: nil,
			segmentIndexes: map[UniqueID]*model.SegmentIndex{
				indexID: {
					SegmentID:     segID,
					CollectionID:  collID,
					PartitionID:   partID,
					NumRows:       10250,
					IndexID:       indexID,
					BuildID:       10,
					NodeID:        0,
					IndexVersion:  1,
					IndexState:    commonpb.IndexState_InProgress,
					FailReason:    "",
					IsDeleted:     false,
					CreateTime:    createTS,
					IndexFileKeys: []string{"file1", "file2"},
					IndexSize:     1025,
					WriteHandoff:  false,
				},
			},
			currRows:        0,
			allocations:     nil,
			lastFlushTime:   time.Time{},
			isCompacting:    false,
			size:            0,
			lastWrittenTime: time.Time{},
		}

		resp, err := s.GetSegmentIndexState(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_Success, resp.GetStatus().GetErrorCode())
	})

	t.Run("finish", func(t *testing.T) {
		s.meta.segments.segments[segID] = &SegmentInfo{
			SegmentInfo: nil,
			segmentIndexes: map[UniqueID]*model.SegmentIndex{
				indexID: {
					SegmentID:     segID,
					CollectionID:  collID,
					PartitionID:   partID,
					NumRows:       10250,
					IndexID:       indexID,
					BuildID:       10,
					NodeID:        0,
					IndexVersion:  1,
					IndexState:    commonpb.IndexState_Finished,
					FailReason:    "",
					IsDeleted:     false,
					CreateTime:    createTS,
					IndexFileKeys: []string{"file1", "file2"},
					IndexSize:     1025,
					WriteHandoff:  false,
				},
			},
			currRows:        0,
			allocations:     nil,
			lastFlushTime:   time.Time{},
			isCompacting:    false,
			size:            0,
			lastWrittenTime: time.Time{},
		}
		resp, err := s.GetSegmentIndexState(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_Success, resp.GetStatus().GetErrorCode())
	})
}

func TestServer_GetIndexBuildProgress(t *testing.T) {
	var (
		collID     = UniqueID(1)
		partID     = UniqueID(2)
		fieldID    = UniqueID(10)
		indexID    = UniqueID(100)
		segID      = UniqueID(1000)
		indexName  = "default_idx"
		typeParams = []*commonpb.KeyValuePair{
			{
				Key:   "dim",
				Value: "128",
			},
		}
		indexParams = []*commonpb.KeyValuePair{
			{
				Key:   "index_type",
				Value: "IVF_FLAT",
			},
		}
		createTS = uint64(1000)
		ctx      = context.Background()
		req      = &datapb.GetIndexBuildProgressRequest{
			CollectionID: collID,
			IndexName:    "",
		}
	)

	s := &Server{
		meta: &meta{
			catalog:  &datacoord.Catalog{Txn: &mockEtcdKv{}},
			indexes:  map[UniqueID]map[UniqueID]*model.Index{},
			segments: &SegmentsInfo{map[UniqueID]*SegmentInfo{}},
		},
		allocator:       newMockAllocator(),
		notifyIndexChan: make(chan UniqueID, 1),
	}
	t.Run("server not available", func(t *testing.T) {
		s.stateCode.Store(commonpb.StateCode_Initializing)
		resp, err := s.GetIndexBuildProgress(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_DataCoordNA, resp.GetStatus().GetErrorCode())
	})

	t.Run("no indexes", func(t *testing.T) {
		s.stateCode.Store(commonpb.StateCode_Healthy)
		resp, err := s.GetIndexBuildProgress(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_IndexNotExist, resp.GetStatus().GetErrorCode())
	})

	t.Run("unissued", func(t *testing.T) {
		s.meta.indexes[collID] = map[UniqueID]*model.Index{
			indexID: {
				TenantID:        "",
				CollectionID:    collID,
				FieldID:         fieldID,
				IndexID:         indexID,
				IndexName:       indexName,
				IsDeleted:       false,
				CreateTime:      createTS,
				TypeParams:      typeParams,
				IndexParams:     indexParams,
				IsAutoIndex:     false,
				UserIndexParams: nil,
			},
		}
		s.meta.segments = &SegmentsInfo{
			segments: map[UniqueID]*SegmentInfo{
				segID: {
					SegmentInfo: &datapb.SegmentInfo{
						ID:             segID,
						CollectionID:   collID,
						PartitionID:    partID,
						InsertChannel:  "",
						NumOfRows:      10250,
						State:          commonpb.SegmentState_Flushed,
						MaxRowNum:      65536,
						LastExpireTime: createTS,
					},
					segmentIndexes:  nil,
					currRows:        10250,
					allocations:     nil,
					lastFlushTime:   time.Time{},
					isCompacting:    false,
					size:            0,
					lastWrittenTime: time.Time{},
				},
			},
		}

		resp, err := s.GetIndexBuildProgress(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_Success, resp.GetStatus().GetErrorCode())
		assert.Equal(t, int64(10250), resp.GetTotalRows())
		assert.Equal(t, int64(0), resp.GetIndexedRows())
	})

	t.Run("finish", func(t *testing.T) {
		s.meta.segments = &SegmentsInfo{
			segments: map[UniqueID]*SegmentInfo{
				segID: {
					SegmentInfo: &datapb.SegmentInfo{
						ID:             segID,
						CollectionID:   collID,
						PartitionID:    partID,
						InsertChannel:  "",
						NumOfRows:      10250,
						State:          commonpb.SegmentState_Flushed,
						MaxRowNum:      65536,
						LastExpireTime: createTS,
					},
					segmentIndexes: map[UniqueID]*model.SegmentIndex{
						indexID: {
							SegmentID:     segID,
							CollectionID:  collID,
							PartitionID:   partID,
							NumRows:       10250,
							IndexID:       indexID,
							BuildID:       10,
							NodeID:        0,
							IndexVersion:  1,
							IndexState:    commonpb.IndexState_Finished,
							FailReason:    "",
							IsDeleted:     false,
							CreateTime:    createTS,
							IndexFileKeys: []string{"file1", "file2"},
							IndexSize:     0,
							WriteHandoff:  false,
						},
					},
					currRows:        10250,
					allocations:     nil,
					lastFlushTime:   time.Time{},
					isCompacting:    false,
					size:            0,
					lastWrittenTime: time.Time{},
				},
			},
		}

		resp, err := s.GetIndexBuildProgress(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_Success, resp.GetStatus().GetErrorCode())
		assert.Equal(t, int64(10250), resp.GetTotalRows())
		assert.Equal(t, int64(10250), resp.GetIndexedRows())
	})
}

func TestServer_DescribeIndex(t *testing.T) {
	var (
		collID     = UniqueID(1)
		partID     = UniqueID(2)
		fieldID    = UniqueID(10)
		indexID    = UniqueID(100)
		segID      = UniqueID(1000)
		buildID    = UniqueID(10000)
		indexName  = "default_idx"
		typeParams = []*commonpb.KeyValuePair{
			{
				Key:   "dim",
				Value: "128",
			},
		}
		indexParams = []*commonpb.KeyValuePair{
			{
				Key:   "index_type",
				Value: "IVF_FLAT",
			},
		}
		createTS = uint64(1000)
		ctx      = context.Background()
		req      = &datapb.DescribeIndexRequest{
			CollectionID: collID,
			IndexName:    "",
		}
	)

	s := &Server{
		meta: &meta{
			catalog: &datacoord.Catalog{Txn: &mockEtcdKv{}},
			indexes: map[UniqueID]map[UniqueID]*model.Index{
				collID: {
					//finished
					indexID: {
						TenantID:        "",
						CollectionID:    collID,
						FieldID:         fieldID,
						IndexID:         indexID,
						IndexName:       indexName,
						IsDeleted:       false,
						CreateTime:      createTS,
						TypeParams:      typeParams,
						IndexParams:     indexParams,
						IsAutoIndex:     false,
						UserIndexParams: nil,
					},
					// deleted
					indexID + 1: {
						TenantID:        "",
						CollectionID:    collID,
						FieldID:         fieldID + 1,
						IndexID:         indexID + 1,
						IndexName:       indexName + "_1",
						IsDeleted:       true,
						CreateTime:      createTS,
						TypeParams:      typeParams,
						IndexParams:     indexParams,
						IsAutoIndex:     false,
						UserIndexParams: nil,
					},
					// unissued
					indexID + 2: {
						TenantID:        "",
						CollectionID:    collID,
						FieldID:         fieldID + 2,
						IndexID:         indexID + 2,
						IndexName:       indexName + "_2",
						IsDeleted:       false,
						CreateTime:      createTS,
						TypeParams:      typeParams,
						IndexParams:     indexParams,
						IsAutoIndex:     false,
						UserIndexParams: nil,
					},
					// inProgress
					indexID + 3: {
						TenantID:        "",
						CollectionID:    collID,
						FieldID:         fieldID + 3,
						IndexID:         indexID + 3,
						IndexName:       indexName + "_3",
						IsDeleted:       false,
						CreateTime:      createTS,
						TypeParams:      typeParams,
						IndexParams:     indexParams,
						IsAutoIndex:     false,
						UserIndexParams: nil,
					},
					// failed
					indexID + 4: {
						TenantID:        "",
						CollectionID:    collID,
						FieldID:         fieldID + 4,
						IndexID:         indexID + 4,
						IndexName:       indexName + "_4",
						IsDeleted:       false,
						CreateTime:      createTS,
						TypeParams:      typeParams,
						IndexParams:     indexParams,
						IsAutoIndex:     false,
						UserIndexParams: nil,
					},
					// unissued
					indexID + 5: {
						TenantID:        "",
						CollectionID:    collID,
						FieldID:         fieldID + 5,
						IndexID:         indexID + 5,
						IndexName:       indexName + "_5",
						IsDeleted:       false,
						CreateTime:      createTS,
						TypeParams:      typeParams,
						IndexParams:     indexParams,
						IsAutoIndex:     false,
						UserIndexParams: nil,
					},
				},
			},
			segments: &SegmentsInfo{map[UniqueID]*SegmentInfo{
				segID: {
					SegmentInfo: &datapb.SegmentInfo{
						ID:             segID,
						CollectionID:   collID,
						PartitionID:    partID,
						NumOfRows:      10000,
						State:          commonpb.SegmentState_Flushed,
						MaxRowNum:      65536,
						LastExpireTime: createTS,
					},
					segmentIndexes: map[UniqueID]*model.SegmentIndex{
						indexID: {
							SegmentID:     segID,
							CollectionID:  collID,
							PartitionID:   partID,
							NumRows:       10000,
							IndexID:       indexID,
							BuildID:       buildID,
							NodeID:        0,
							IndexVersion:  1,
							IndexState:    commonpb.IndexState_Finished,
							FailReason:    "",
							IsDeleted:     false,
							CreateTime:    createTS,
							IndexFileKeys: nil,
							IndexSize:     0,
							WriteHandoff:  false,
						},
						indexID + 1: {
							SegmentID:     segID,
							CollectionID:  collID,
							PartitionID:   partID,
							NumRows:       10000,
							IndexID:       indexID + 1,
							BuildID:       buildID + 1,
							NodeID:        0,
							IndexVersion:  1,
							IndexState:    commonpb.IndexState_Finished,
							FailReason:    "",
							IsDeleted:     false,
							CreateTime:    createTS,
							IndexFileKeys: nil,
							IndexSize:     0,
							WriteHandoff:  false,
						},
						indexID + 3: {
							SegmentID:     segID,
							CollectionID:  collID,
							PartitionID:   partID,
							NumRows:       10000,
							IndexID:       indexID + 3,
							BuildID:       buildID + 3,
							NodeID:        0,
							IndexVersion:  1,
							IndexState:    commonpb.IndexState_InProgress,
							FailReason:    "",
							IsDeleted:     false,
							CreateTime:    createTS,
							IndexFileKeys: nil,
							IndexSize:     0,
							WriteHandoff:  false,
						},
						indexID + 4: {
							SegmentID:     segID,
							CollectionID:  collID,
							PartitionID:   partID,
							NumRows:       10000,
							IndexID:       indexID + 4,
							BuildID:       buildID + 4,
							NodeID:        0,
							IndexVersion:  1,
							IndexState:    commonpb.IndexState_Failed,
							FailReason:    "mock failed",
							IsDeleted:     false,
							CreateTime:    createTS,
							IndexFileKeys: nil,
							IndexSize:     0,
							WriteHandoff:  false,
						},
						indexID + 5: {
							SegmentID:     segID,
							CollectionID:  collID,
							PartitionID:   partID,
							NumRows:       10000,
							IndexID:       indexID + 5,
							BuildID:       buildID + 5,
							NodeID:        0,
							IndexVersion:  1,
							IndexState:    commonpb.IndexState_Unissued,
							FailReason:    "",
							IsDeleted:     false,
							CreateTime:    createTS,
							IndexFileKeys: nil,
							IndexSize:     0,
							WriteHandoff:  false,
						},
					},
				},
			}},
		},
		allocator:       newMockAllocator(),
		notifyIndexChan: make(chan UniqueID, 1),
	}

	t.Run("server not available", func(t *testing.T) {
		s.stateCode.Store(commonpb.StateCode_Initializing)
		resp, err := s.DescribeIndex(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_DataCoordNA, resp.GetStatus().GetErrorCode())
	})

	s.stateCode.Store(commonpb.StateCode_Healthy)

	t.Run("success", func(t *testing.T) {
		resp, err := s.DescribeIndex(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_Success, resp.GetStatus().GetErrorCode())
		assert.Equal(t, 5, len(resp.GetIndexInfos()))
	})

	t.Run("describe after drop index", func(t *testing.T) {
		status, err := s.DropIndex(ctx, &datapb.DropIndexRequest{
			CollectionID: collID,
			PartitionIDs: nil,
			IndexName:    "",
			DropAll:      true,
		})
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_Success, status.GetErrorCode())

		resp, err := s.DescribeIndex(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_IndexNotExist, resp.GetStatus().GetErrorCode())
	})
}

func TestServer_DropIndex(t *testing.T) {
	var (
		collID     = UniqueID(1)
		partID     = UniqueID(2)
		fieldID    = UniqueID(10)
		indexID    = UniqueID(100)
		segID      = UniqueID(1000)
		indexName  = "default_idx"
		typeParams = []*commonpb.KeyValuePair{
			{
				Key:   "dim",
				Value: "128",
			},
		}
		indexParams = []*commonpb.KeyValuePair{
			{
				Key:   "index_type",
				Value: "IVF_FLAT",
			},
		}
		createTS = uint64(1000)
		ctx      = context.Background()
		req      = &datapb.DropIndexRequest{
			CollectionID: collID,
			IndexName:    indexName,
		}
	)

	s := &Server{
		meta: &meta{
			catalog: &datacoord.Catalog{Txn: &mockEtcdKv{}},
			indexes: map[UniqueID]map[UniqueID]*model.Index{
				collID: {
					//finished
					indexID: {
						TenantID:        "",
						CollectionID:    collID,
						FieldID:         fieldID,
						IndexID:         indexID,
						IndexName:       indexName,
						IsDeleted:       false,
						CreateTime:      createTS,
						TypeParams:      typeParams,
						IndexParams:     indexParams,
						IsAutoIndex:     false,
						UserIndexParams: nil,
					},
					// deleted
					indexID + 1: {
						TenantID:        "",
						CollectionID:    collID,
						FieldID:         fieldID + 1,
						IndexID:         indexID + 1,
						IndexName:       indexName + "_1",
						IsDeleted:       true,
						CreateTime:      createTS,
						TypeParams:      typeParams,
						IndexParams:     indexParams,
						IsAutoIndex:     false,
						UserIndexParams: nil,
					},
					// unissued
					indexID + 2: {
						TenantID:        "",
						CollectionID:    collID,
						FieldID:         fieldID + 2,
						IndexID:         indexID + 2,
						IndexName:       indexName + "_2",
						IsDeleted:       false,
						CreateTime:      createTS,
						TypeParams:      typeParams,
						IndexParams:     indexParams,
						IsAutoIndex:     false,
						UserIndexParams: nil,
					},
					// inProgress
					indexID + 3: {
						TenantID:        "",
						CollectionID:    collID,
						FieldID:         fieldID + 3,
						IndexID:         indexID + 3,
						IndexName:       indexName + "_3",
						IsDeleted:       false,
						CreateTime:      createTS,
						TypeParams:      typeParams,
						IndexParams:     indexParams,
						IsAutoIndex:     false,
						UserIndexParams: nil,
					},
					// failed
					indexID + 4: {
						TenantID:        "",
						CollectionID:    collID,
						FieldID:         fieldID + 4,
						IndexID:         indexID + 4,
						IndexName:       indexName + "_4",
						IsDeleted:       false,
						CreateTime:      createTS,
						TypeParams:      typeParams,
						IndexParams:     indexParams,
						IsAutoIndex:     false,
						UserIndexParams: nil,
					},
				},
			},
			segments: &SegmentsInfo{map[UniqueID]*SegmentInfo{
				segID: {
					SegmentInfo: &datapb.SegmentInfo{
						ID:             segID,
						CollectionID:   collID,
						PartitionID:    partID,
						NumOfRows:      10000,
						State:          commonpb.SegmentState_Flushed,
						MaxRowNum:      65536,
						LastExpireTime: createTS,
					},
					segmentIndexes: nil,
				},
			}},
		},
		allocator:       newMockAllocator(),
		notifyIndexChan: make(chan UniqueID, 1),
	}

	t.Run("server not available", func(t *testing.T) {
		s.stateCode.Store(commonpb.StateCode_Initializing)
		resp, err := s.DropIndex(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_DataCoordNA, resp.GetErrorCode())
	})

	s.stateCode.Store(commonpb.StateCode_Healthy)

	t.Run("drop fail", func(t *testing.T) {
		s.meta.catalog = &datacoord.Catalog{Txn: &saveFailKV{}}
		resp, err := s.DropIndex(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_UnexpectedError, resp.GetErrorCode())
	})

	t.Run("drop one index", func(t *testing.T) {
		s.meta.catalog = &datacoord.Catalog{Txn: &mockEtcdKv{}}
		resp, err := s.DropIndex(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_Success, resp.GetErrorCode())
	})

	t.Run("drop one without indexName", func(t *testing.T) {
		req = &datapb.DropIndexRequest{
			CollectionID: collID,
			PartitionIDs: nil,
			IndexName:    "",
			DropAll:      false,
		}
		resp, err := s.DropIndex(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_UnexpectedError, resp.GetErrorCode())
	})

	t.Run("drop all indexes", func(t *testing.T) {
		req = &datapb.DropIndexRequest{
			CollectionID: collID,
			PartitionIDs: nil,
			IndexName:    "",
			DropAll:      true,
		}
		resp, err := s.DropIndex(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_Success, resp.GetErrorCode())
	})

	t.Run("drop not exist index", func(t *testing.T) {
		req = &datapb.DropIndexRequest{
			CollectionID: collID,
			PartitionIDs: nil,
			IndexName:    "",
			DropAll:      true,
		}
		resp, err := s.DropIndex(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_Success, resp.GetErrorCode())
	})
}

func TestServer_GetIndexInfos(t *testing.T) {
	var (
		collID     = UniqueID(1)
		partID     = UniqueID(2)
		fieldID    = UniqueID(10)
		indexID    = UniqueID(100)
		segID      = UniqueID(1000)
		buildID    = UniqueID(10000)
		indexName  = "default_idx"
		typeParams = []*commonpb.KeyValuePair{
			{
				Key:   "dim",
				Value: "128",
			},
		}
		indexParams = []*commonpb.KeyValuePair{
			{
				Key:   "index_type",
				Value: "IVF_FLAT",
			},
		}
		createTS = uint64(1000)
		ctx      = context.Background()
		req      = &datapb.GetIndexInfoRequest{
			CollectionID: collID,
			SegmentIDs:   []UniqueID{segID},
			IndexName:    indexName,
		}
	)

	chunkManagerFactory := storage.NewChunkManagerFactoryWithParam(Params)
	cli, err := chunkManagerFactory.NewPersistentStorageChunkManager(ctx)
	assert.NoError(t, err)

	s := &Server{
		meta: &meta{
			catalog: &datacoord.Catalog{Txn: &mockEtcdKv{}},
			indexes: map[UniqueID]map[UniqueID]*model.Index{
				collID: {
					//finished
					indexID: {
						TenantID:        "",
						CollectionID:    collID,
						FieldID:         fieldID,
						IndexID:         indexID,
						IndexName:       indexName,
						IsDeleted:       false,
						CreateTime:      createTS,
						TypeParams:      typeParams,
						IndexParams:     indexParams,
						IsAutoIndex:     false,
						UserIndexParams: nil,
					},
				},
			},
			segments: &SegmentsInfo{
				map[UniqueID]*SegmentInfo{
					segID: {
						SegmentInfo: &datapb.SegmentInfo{
							ID:             segID,
							CollectionID:   collID,
							PartitionID:    partID,
							NumOfRows:      10000,
							State:          commonpb.SegmentState_Flushed,
							MaxRowNum:      65536,
							LastExpireTime: createTS,
						},
						segmentIndexes: map[UniqueID]*model.SegmentIndex{
							indexID: {
								SegmentID:     segID,
								CollectionID:  collID,
								PartitionID:   partID,
								NumRows:       10000,
								IndexID:       indexID,
								BuildID:       buildID,
								NodeID:        0,
								IndexVersion:  1,
								IndexState:    commonpb.IndexState_Finished,
								FailReason:    "",
								IsDeleted:     false,
								CreateTime:    createTS,
								IndexFileKeys: nil,
								IndexSize:     0,
								WriteHandoff:  false,
							},
						},
					},
				},
			},
			chunkManager: cli,
		},
		allocator:       newMockAllocator(),
		notifyIndexChan: make(chan UniqueID, 1),
	}

	t.Run("server not available", func(t *testing.T) {
		s.stateCode.Store(commonpb.StateCode_Initializing)
		resp, err := s.GetIndexInfos(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_DataCoordNA, resp.GetStatus().GetErrorCode())
	})

	s.stateCode.Store(commonpb.StateCode_Healthy)
	t.Run("get segment index infos", func(t *testing.T) {
		resp, err := s.GetIndexInfos(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_Success, resp.GetStatus().GetErrorCode())
		assert.Equal(t, 1, len(resp.GetSegmentInfo()))
	})
}
