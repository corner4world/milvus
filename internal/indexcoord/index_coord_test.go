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
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/milvus-io/milvus/internal/util/typeutil"

	"github.com/golang/protobuf/proto"

	"github.com/milvus-io/milvus-proto/go-api/commonpb"
	"github.com/milvus-io/milvus-proto/go-api/milvuspb"
	"github.com/milvus-io/milvus/internal/common"
	"github.com/milvus-io/milvus/internal/indexnode"
	etcdkv "github.com/milvus-io/milvus/internal/kv/etcd"
	"github.com/milvus-io/milvus/internal/metastore/kv/indexcoord"
	"github.com/milvus-io/milvus/internal/metastore/model"
	"github.com/milvus-io/milvus/internal/proto/datapb"
	"github.com/milvus-io/milvus/internal/proto/indexpb"
	"github.com/milvus-io/milvus/internal/proto/internalpb"
	"github.com/milvus-io/milvus/internal/util"
	"github.com/milvus-io/milvus/internal/util/dependency"
	"github.com/milvus-io/milvus/internal/util/etcd"
	"github.com/milvus-io/milvus/internal/util/metricsinfo"
	"github.com/milvus-io/milvus/internal/util/paramtable"
	"github.com/milvus-io/milvus/internal/util/sessionutil"
	"github.com/stretchr/testify/assert"
)

func TestMain(m *testing.M) {
	paramtable.Init()
	rand.Seed(time.Now().UnixNano())
	os.Exit(m.Run())
}

func TestMockEtcd(t *testing.T) {
	Params.InitOnce()
	Params.BaseTable.Save("etcd.rootPath", "/test/datanode/root/indexcoord-mock")

	etcdCli, err := etcd.GetEtcdClient(
		Params.EtcdCfg.UseEmbedEtcd.GetAsBool(),
		Params.EtcdCfg.EtcdUseSSL.GetAsBool(),
		Params.EtcdCfg.Endpoints.GetAsStrings(),
		Params.EtcdCfg.EtcdTLSCert.GetValue(),
		Params.EtcdCfg.EtcdTLSKey.GetValue(),
		Params.EtcdCfg.EtcdTLSCACert.GetValue(),
		Params.EtcdCfg.EtcdTLSMinVersion.GetValue())
	assert.NoError(t, err)
	etcdKV := etcdkv.NewEtcdKV(etcdCli, Params.EtcdCfg.MetaRootPath.GetValue())

	mockEtcd := NewMockEtcdKVWithReal(etcdKV)
	key := "foo"
	value := "foo-val"
	err = mockEtcd.Save(key, value)
	assert.NoError(t, err)

	fmt.Println(mockEtcd == nil)
	loadVal, err := mockEtcd.Load(key)
	assert.NoError(t, err)
	assert.Equal(t, value, loadVal)

	_, _, err = mockEtcd.LoadWithPrefix(key)
	assert.NoError(t, err)

	_, _, _, err = mockEtcd.LoadWithPrefix2(key)
	assert.NoError(t, err)

	_, _, _, err = mockEtcd.LoadWithRevision(key)
	assert.NoError(t, err)

	_, _, _, _, err = mockEtcd.LoadWithRevisionAndVersions(key)
	assert.NoError(t, err)

	err = mockEtcd.MultiSave(map[string]string{
		"TestMockEtcd-1": "mock-val",
		"TestMockEtcd-2": "mock-val",
	})
	assert.NoError(t, err)

	err = mockEtcd.RemoveWithPrefix("TestMockEtcd-")
	assert.NoError(t, err)

	err = mockEtcd.Remove(key)
	assert.NoError(t, err)

}

func testIndexCoord(t *testing.T) {
	ctx := context.Background()
	Params.BaseTable.Save("etcd.rootPath", "/test/datanode/root/indexcoord-ut")

	// first start an IndexNode
	inm0 := indexnode.NewIndexNodeMock()
	etcdCli, err := etcd.GetEtcdClient(
		Params.EtcdCfg.UseEmbedEtcd.GetAsBool(),
		Params.EtcdCfg.EtcdUseSSL.GetAsBool(),
		Params.EtcdCfg.Endpoints.GetAsStrings(),
		Params.EtcdCfg.EtcdTLSCert.GetValue(),
		Params.EtcdCfg.EtcdTLSKey.GetValue(),
		Params.EtcdCfg.EtcdTLSCACert.GetValue(),
		Params.EtcdCfg.EtcdTLSMinVersion.GetValue())
	assert.NoError(t, err)

	// start IndexCoord
	factory := dependency.NewDefaultFactory(true)
	ic, err := NewIndexCoord(ctx, factory)
	assert.NoError(t, err)

	rcm := NewRootCoordMock()
	err = ic.SetRootCoord(rcm)
	assert.NoError(t, err)

	dcm := &DataCoordMock{
		CallGetSegmentInfo: func(ctx context.Context, req *datapb.GetSegmentInfoRequest) (*datapb.GetSegmentInfoResponse, error) {
			segmentInfos := make([]*datapb.SegmentInfo, 0)
			for _, segID := range req.SegmentIDs {
				segmentInfos = append(segmentInfos, &datapb.SegmentInfo{
					ID:           segID,
					CollectionID: collID,
					PartitionID:  partID,
					NumOfRows:    10240,
					State:        commonpb.SegmentState_Flushed,
					StartPosition: &internalpb.MsgPosition{
						Timestamp: createTs,
					},
					Binlogs: []*datapb.FieldBinlog{
						{
							FieldID: fieldID,
							Binlogs: []*datapb.Binlog{
								{
									LogPath: "file1",
								},
								{
									LogPath: "file2",
								},
							},
						},
					},
				})
			}
			return &datapb.GetSegmentInfoResponse{
				Status: &commonpb.Status{
					ErrorCode: commonpb.ErrorCode_Success,
				},
				Infos: segmentInfos,
			}, nil
		},
		CallGetFlushedSegment: func(ctx context.Context, req *datapb.GetFlushedSegmentsRequest) (*datapb.GetFlushedSegmentsResponse, error) {
			return &datapb.GetFlushedSegmentsResponse{
				Status: &commonpb.Status{
					ErrorCode: commonpb.ErrorCode_Success,
				},
				Segments: []int64{segID},
			}, nil
		},
		CallAcquireSegmentLock: func(ctx context.Context, req *datapb.AcquireSegmentLockRequest) (*commonpb.Status, error) {
			return &commonpb.Status{
				ErrorCode: commonpb.ErrorCode_Success,
			}, nil
		},
		CallReleaseSegmentLock: func(ctx context.Context, req *datapb.ReleaseSegmentLockRequest) (*commonpb.Status, error) {
			return &commonpb.Status{
				ErrorCode: commonpb.ErrorCode_Success,
			}, nil
		},
	}
	err = ic.SetDataCoord(dcm)
	assert.Nil(t, err)

	ic.SetEtcdClient(etcdCli)

	err = ic.Init()
	assert.NoError(t, err)

	mockKv := NewMockEtcdKVWithReal(ic.etcdKV)
	ic.metaTable.catalog = &indexcoord.Catalog{
		Txn: mockKv,
	}
	assert.NoError(t, err)

	err = ic.Register()
	assert.NoError(t, err)

	err = ic.Start()
	assert.NoError(t, err)

	ic.UpdateStateCode(commonpb.StateCode_Healthy)

	ic.nodeManager.setClient(1, inm0)

	// Test IndexCoord function
	t.Run("GetComponentStates", func(t *testing.T) {
		states, err := ic.GetComponentStates(ctx)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.StateCode_Healthy, states.State.StateCode)
	})

	t.Run("GetStatisticsChannel", func(t *testing.T) {
		resp, err := ic.GetStatisticsChannel(ctx)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_Success, resp.Status.ErrorCode)
	})

	t.Run("CreateIndex", func(t *testing.T) {
		req := &indexpb.CreateIndexRequest{
			CollectionID: collID,
			FieldID:      fieldID,
			IndexName:    indexName,
			Timestamp:    createTs,
		}
		resp, err := ic.CreateIndex(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_Success, resp.ErrorCode)
	})

	t.Run("GetIndexState", func(t *testing.T) {
		req := &indexpb.GetIndexStateRequest{
			CollectionID: collID,
			IndexName:    indexName,
		}
		resp, err := ic.GetIndexState(ctx, req)
		assert.NoError(t, err)
		for resp.State != commonpb.IndexState_Finished {
			resp, err = ic.GetIndexState(ctx, req)
			assert.NoError(t, err)
			time.Sleep(time.Second)
		}
	})

	t.Run("GetSegmentIndexState", func(t *testing.T) {
		req := &indexpb.GetSegmentIndexStateRequest{
			CollectionID: collID,
			IndexName:    indexName,
			SegmentIDs:   []UniqueID{segID},
		}
		resp, err := ic.GetSegmentIndexState(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, len(req.SegmentIDs), len(resp.States))
	})

	t.Run("GetIndexInfos", func(t *testing.T) {
		req := &indexpb.GetIndexInfoRequest{
			CollectionID: collID,
			SegmentIDs:   []UniqueID{segID},
			IndexName:    indexName,
		}
		resp, err := ic.GetIndexInfos(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, len(req.SegmentIDs), len(resp.SegmentInfo))
	})

	getReq := func() *indexpb.DescribeIndexRequest {
		return &indexpb.DescribeIndexRequest{
			CollectionID: collID,
			IndexName:    indexName,
		}
	}

	t.Run("DescribeIndex NotExist", func(t *testing.T) {
		indexs := ic.metaTable.collectionIndexes
		ic.metaTable.collectionIndexes = make(map[UniqueID]map[UniqueID]*model.Index)
		defer func() {
			ic.metaTable.collectionIndexes = indexs
		}()

		resp, err := ic.DescribeIndex(ctx, getReq())
		assert.NoError(t, err)
		assert.Equal(t, resp.Status.ErrorCode, commonpb.ErrorCode_IndexNotExist)
	})

	t.Run("DescribeIndex State", func(t *testing.T) {
		req := getReq()
		res := ic.metaTable.GetIndexIDByName(collID, indexName)
		var indexIDTest int64
		for k := range res {
			indexIDTest = k
			break
		}

		updateSegmentIndexes := func(i map[UniqueID]map[UniqueID]*model.SegmentIndex) {
			ic.metaTable.indexLock.Lock()
			ic.metaTable.segmentIndexes = i
			ic.metaTable.indexLock.Unlock()
		}

		getSegmentIndexes := func() map[UniqueID]map[UniqueID]*model.SegmentIndex {
			ic.metaTable.indexLock.RLock()
			defer ic.metaTable.indexLock.RUnlock()
			return ic.metaTable.segmentIndexes
		}

		indexs := getSegmentIndexes()
		mockIndexs := make(map[UniqueID]map[UniqueID]*model.SegmentIndex)
		progressIndex := &model.SegmentIndex{
			IndexState: commonpb.IndexState_InProgress,
		}
		failedIndex := &model.SegmentIndex{
			IndexState: commonpb.IndexState_Failed,
			SegmentID:  333,
			FailReason: "mock fail",
		}
		finishedIndex := &model.SegmentIndex{
			IndexState: commonpb.IndexState_Finished,
			NumRows:    2048,
		}

		updateSegmentIndexes(mockIndexs)
		defer func() {
			updateSegmentIndexes(indexs)
		}()

		mockIndexs[111] = make(map[UniqueID]*model.SegmentIndex)
		mockIndexs[111][indexIDTest] = finishedIndex

		resp, err := ic.DescribeIndex(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_Success, resp.Status.ErrorCode)
		assert.Equal(t, commonpb.IndexState_Finished, resp.IndexInfos[0].State)

		originFunc1 := dcm.CallGetFlushedSegment
		originFunc2 := dcm.CallGetSegmentInfo
		defer func() {
			dcm.CallGetFlushedSegment = originFunc1
			dcm.SetFunc(func() {
				dcm.CallGetSegmentInfo = originFunc2
			})
		}()
		dcm.CallGetFlushedSegment = func(ctx context.Context, req *datapb.GetFlushedSegmentsRequest) (*datapb.GetFlushedSegmentsResponse, error) {
			return nil, errors.New("mock error")
		}

		mockIndexs[222] = make(map[UniqueID]*model.SegmentIndex)
		mockIndexs[222][indexIDTest] = progressIndex
		resp, err = ic.DescribeIndex(ctx, req)
		assert.Error(t, err)
		assert.NotEqual(t, commonpb.ErrorCode_Success, resp.Status.ErrorCode)

		dcm.CallGetFlushedSegment = func(ctx context.Context, req *datapb.GetFlushedSegmentsRequest) (*datapb.GetFlushedSegmentsResponse, error) {
			return &datapb.GetFlushedSegmentsResponse{
				Status: &commonpb.Status{
					ErrorCode: commonpb.ErrorCode_UnexpectedError,
					Reason:    "mock fail",
				},
			}, nil
		}
		resp, err = ic.DescribeIndex(ctx, req)
		assert.NoError(t, err)
		assert.NotEqual(t, commonpb.ErrorCode_Success, resp.Status.ErrorCode)

		dcm.CallGetFlushedSegment = func(ctx context.Context, req *datapb.GetFlushedSegmentsRequest) (*datapb.GetFlushedSegmentsResponse, error) {
			return &datapb.GetFlushedSegmentsResponse{
				Segments: []int64{111, 222, 333},
			}, nil
		}
		dcm.SetFunc(func() {
			dcm.CallGetSegmentInfo = func(ctx context.Context, req *datapb.GetSegmentInfoRequest) (*datapb.GetSegmentInfoResponse, error) {
				return nil, errors.New("mock error")
			}
		})
		resp, err = ic.DescribeIndex(ctx, req)
		assert.Error(t, err)
		assert.NotEqual(t, commonpb.ErrorCode_Success, resp.Status.ErrorCode)

		dcm.SetFunc(func() {
			dcm.CallGetSegmentInfo = func(ctx context.Context, req *datapb.GetSegmentInfoRequest) (*datapb.GetSegmentInfoResponse, error) {
				return &datapb.GetSegmentInfoResponse{
					Status: &commonpb.Status{
						ErrorCode: commonpb.ErrorCode_UnexpectedError,
						Reason:    "mock fail",
					},
				}, nil
			}
		})
		resp, err = ic.DescribeIndex(ctx, req)
		assert.NoError(t, err)
		assert.NotEqual(t, commonpb.ErrorCode_Success, resp.Status.ErrorCode)

		dcm.SetFunc(func() {
			dcm.CallGetSegmentInfo = func(ctx context.Context, req *datapb.GetSegmentInfoRequest) (*datapb.GetSegmentInfoResponse, error) {
				return &datapb.GetSegmentInfoResponse{
					Infos: []*datapb.SegmentInfo{
						{State: commonpb.SegmentState_Flushed, NumOfRows: 2048},
						{State: commonpb.SegmentState_Flushed, NumOfRows: 2048},
						{State: commonpb.SegmentState_Flushed, NumOfRows: 2048},
					},
				}, nil
			}
		})
		resp, err = ic.DescribeIndex(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_Success, resp.Status.ErrorCode)
		assert.Equal(t, commonpb.IndexState_InProgress, resp.IndexInfos[0].State)
		assert.Equal(t, int64(2048), resp.IndexInfos[0].IndexedRows)
		assert.Equal(t, int64(2048*3), resp.IndexInfos[0].TotalRows)

		mockIndexs[333] = make(map[UniqueID]*model.SegmentIndex)
		mockIndexs[333][indexIDTest] = failedIndex
		resp, err = ic.DescribeIndex(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_Success, resp.Status.ErrorCode)
		assert.Equal(t, commonpb.IndexState_Failed, resp.IndexInfos[0].State)
	})

	t.Run("DescribeIndex", func(t *testing.T) {
		req := &indexpb.DescribeIndexRequest{
			CollectionID: collID,
			IndexName:    indexName,
		}
		resp, err := ic.DescribeIndex(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, 1, len(resp.IndexInfos))
	})

	t.Run("FlushedSegmentWatcher", func(t *testing.T) {
		segmentID := segID + 1

		segment := &datapb.SegmentInfo{ID: segID}
		segBytes, err := proto.Marshal(segment)
		if err != nil {
			panic(err)
		}

		err = ic.etcdKV.Save(path.Join(util.FlushedSegmentPrefix, strconv.FormatInt(collID, 10), strconv.FormatInt(partID, 10), strconv.FormatInt(segmentID, 10)), string(segBytes))
		assert.NoError(t, err)

		req := &indexpb.GetSegmentIndexStateRequest{
			CollectionID: collID,
			IndexName:    indexName,
			SegmentIDs:   []UniqueID{segmentID},
		}
		resp, err := ic.GetSegmentIndexState(ctx, req)
		assert.NoError(t, err)
		for len(resp.States) != 1 || resp.States[0].State != commonpb.IndexState_Finished {
			resp, err = ic.GetSegmentIndexState(ctx, req)
			assert.NoError(t, err)
			time.Sleep(time.Second)
		}
	})

	t.Run("FlushedSegmentWatcher backward compatibility testing", func(t *testing.T) {
		segmentID := segID + 2
		err = ic.etcdKV.Save(path.Join(util.FlushedSegmentPrefix, strconv.FormatInt(collID, 10), strconv.FormatInt(partID, 10), strconv.FormatInt(segmentID, 10)), strconv.FormatInt(segmentID, 10))
		assert.NoError(t, err)

		req := &indexpb.GetSegmentIndexStateRequest{
			CollectionID: collID,
			IndexName:    indexName,
			SegmentIDs:   []UniqueID{segmentID},
		}
		resp, err := ic.GetSegmentIndexState(ctx, req)
		assert.NoError(t, err)
		for len(resp.States) != 1 || resp.States[0].State != commonpb.IndexState_Finished {
			resp, err = ic.GetSegmentIndexState(ctx, req)
			assert.NoError(t, err)
			time.Sleep(time.Second)
		}
	})

	t.Run("Showconfigurations, port", func(t *testing.T) {
		pattern := "indexcoord.Port"
		req := &internalpb.ShowConfigurationsRequest{
			Base: &commonpb.MsgBase{
				MsgType: commonpb.MsgType_WatchQueryChannels,
				MsgID:   rand.Int63(),
			},
			Pattern: pattern,
		}

		resp, err := ic.ShowConfigurations(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_Success, resp.Status.ErrorCode)
		assert.Equal(t, 1, len(resp.Configuations))
		assert.Equal(t, "indexcoord.port", resp.Configuations[0].Key)
	})

	t.Run("GetIndexBuildProgress", func(t *testing.T) {
		req := &indexpb.GetIndexBuildProgressRequest{
			CollectionID: collID,
			IndexName:    indexName,
		}
		resp, err := ic.GetIndexBuildProgress(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_Success, resp.Status.ErrorCode)
	})

	t.Run("DropIndex", func(t *testing.T) {
		req := &indexpb.DropIndexRequest{
			CollectionID: collID,
			PartitionIDs: nil,
			IndexName:    indexName,
			DropAll:      false,
		}
		resp, err := ic.DropIndex(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_Success, resp.ErrorCode)
	})

	t.Run("GetMetrics", func(t *testing.T) {
		req, err := metricsinfo.ConstructRequestByMetricType(metricsinfo.SystemInfoMetrics)
		assert.NoError(t, err)
		resp, err := ic.GetMetrics(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_Success, resp.Status.ErrorCode)
	})

	t.Run("WatchNodeState", func(t *testing.T) {
		allClients := ic.nodeManager.GetAllClients()
		nodeSession := sessionutil.NewSession(context.Background(), Params.EtcdCfg.MetaRootPath.GetValue(), etcdCli, sessionutil.WithResueNodeID(true))
		originNodeID := paramtable.GetNodeID()
		defer func() {
			paramtable.SetNodeID(originNodeID)
		}()
		paramtable.SetNodeID(100)
		nodeSession.Init(typeutil.IndexNodeRole, "127.0.0.1:11111", false, true)
		nodeSession.Register()

		addNodeChan := make(chan struct{})
		go func() {
			for {
				time.Sleep(200 * time.Millisecond)
				if len(ic.nodeManager.GetAllClients()) > len(allClients) {
					close(addNodeChan)
					break
				}
			}
		}()
		select {
		case <-addNodeChan:
		case <-time.After(10 * time.Second):
			assert.Fail(t, "fail to add node")
		}
		var newNodeID UniqueID = -1
		for id := range ic.nodeManager.GetAllClients() {
			if _, ok := allClients[id]; !ok {
				newNodeID = id
				break
			}
		}

		nodeSession.GoingStop()
		stoppingNodeChan := make(chan struct{})
		go func() {
			for {
				time.Sleep(200 * time.Millisecond)
				ic.nodeManager.lock.RLock()
				_, ok := ic.nodeManager.stoppingNodes[newNodeID]
				ic.nodeManager.lock.RUnlock()
				if ok {
					close(stoppingNodeChan)
					break
				}
			}
		}()
		select {
		case <-stoppingNodeChan:
		case <-time.After(10 * time.Second):
			assert.Fail(t, "fail to stop node")
		}

		nodeSession.Revoke(time.Second)
		deleteNodeChan := make(chan struct{})
		go func() {
			for {
				time.Sleep(200 * time.Millisecond)
				ic.nodeManager.lock.RLock()
				_, ok := ic.nodeManager.stoppingNodes[newNodeID]
				ic.nodeManager.lock.RUnlock()
				if !ok {
					close(deleteNodeChan)
					break
				}
			}
		}()
		select {
		case <-deleteNodeChan:
		case <-time.After(10 * time.Second):
			assert.Fail(t, "fail to stop node")
		}
	})

	// Stop IndexCoord
	err = ic.Stop()
	assert.NoError(t, err)

	etcdKV := etcdkv.NewEtcdKV(etcdCli, Params.EtcdCfg.MetaRootPath.GetValue())
	err = etcdKV.RemoveWithPrefix("")
	assert.NoError(t, err)
}

func TestIndexCoord_DisableActiveStandby(t *testing.T) {
	Params.InitOnce()
	// indexnode.Params.InitOnce()
	paramtable.Get().Save(Params.IndexCoordCfg.EnableActiveStandby.Key, "false")
	testIndexCoord(t)
}

// make sure the main functions work well when EnableActiveStandby=true
func TestIndexCoord_EnableActiveStandby(t *testing.T) {
	Params.InitOnce()
	// indexnode.Params.InitOnce()
	paramtable.Get().Save(Params.IndexCoordCfg.EnableActiveStandby.Key, "true")
	testIndexCoord(t)
}

func TestIndexCoord_GetComponentStates(t *testing.T) {
	ic := &IndexCoord{}
	ic.stateCode.Store(commonpb.StateCode_Healthy)
	resp, err := ic.GetComponentStates(context.Background())
	assert.NoError(t, err)
	assert.Equal(t, commonpb.ErrorCode_Success, resp.Status.ErrorCode)
	assert.Equal(t, common.NotRegisteredID, resp.State.NodeID)

	ic.session = &sessionutil.Session{}
	ic.session.UpdateRegistered(true)
	resp, err = ic.GetComponentStates(context.Background())
	assert.NoError(t, err)
	assert.Equal(t, commonpb.ErrorCode_Success, resp.Status.ErrorCode)
}

func TestIndexCoord_UnHealthy(t *testing.T) {
	ctx := context.Background()
	ic := &IndexCoord{}
	ic.stateCode.Store(commonpb.StateCode_Abnormal)

	// Test IndexCoord function
	t.Run("CreateIndex", func(t *testing.T) {
		req := &indexpb.CreateIndexRequest{
			CollectionID: collID,
			FieldID:      fieldID,
			IndexName:    indexName,
		}
		resp, err := ic.CreateIndex(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_UnexpectedError, resp.ErrorCode)
	})

	t.Run("GetIndexState", func(t *testing.T) {
		req := &indexpb.GetIndexStateRequest{
			CollectionID: collID,
			IndexName:    indexName,
		}
		resp, err := ic.GetIndexState(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_UnexpectedError, resp.Status.ErrorCode)
	})

	t.Run("GetSegmentIndexState", func(t *testing.T) {
		req := &indexpb.GetSegmentIndexStateRequest{
			CollectionID: collID,
			IndexName:    indexName,
			SegmentIDs:   []UniqueID{segID},
		}
		resp, err := ic.GetSegmentIndexState(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_UnexpectedError, resp.Status.ErrorCode)
	})

	t.Run("GetIndexInfos", func(t *testing.T) {
		req := &indexpb.GetIndexInfoRequest{
			CollectionID: collID,
			SegmentIDs:   []UniqueID{segID},
			IndexName:    indexName,
		}
		resp, err := ic.GetIndexInfos(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_UnexpectedError, resp.Status.ErrorCode)
	})

	t.Run("DescribeIndex", func(t *testing.T) {
		req := &indexpb.DescribeIndexRequest{
			CollectionID: collID,
			IndexName:    indexName,
		}
		resp, err := ic.DescribeIndex(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_UnexpectedError, resp.Status.ErrorCode)
	})

	t.Run("GetIndexBuildProgress", func(t *testing.T) {
		req := &indexpb.GetIndexBuildProgressRequest{
			CollectionID: collID,
			IndexName:    indexName,
		}
		resp, err := ic.GetIndexBuildProgress(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_UnexpectedError, resp.Status.ErrorCode)
	})

	t.Run("DropIndex", func(t *testing.T) {
		req := &indexpb.DropIndexRequest{
			CollectionID: collID,
			IndexName:    indexName,
		}
		resp, err := ic.DropIndex(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_UnexpectedError, resp.ErrorCode)
	})

	t.Run("ShowConfigurations when indexcoord is not healthy", func(t *testing.T) {
		pattern := ""
		req := &internalpb.ShowConfigurationsRequest{
			Base: &commonpb.MsgBase{
				MsgType: commonpb.MsgType_WatchQueryChannels,
				MsgID:   rand.Int63(),
			},
			Pattern: pattern,
		}

		resp, err := ic.ShowConfigurations(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_UnexpectedError, resp.Status.ErrorCode)
	})

	t.Run("GetMetrics", func(t *testing.T) {
		req, err := metricsinfo.ConstructRequestByMetricType(metricsinfo.SystemInfoMetrics)
		assert.NoError(t, err)
		resp, err := ic.GetMetrics(ctx, req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_UnexpectedError, resp.Status.ErrorCode)
	})

}

func TestIndexCoord_DropIndex(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		ic := &IndexCoord{
			metaTable: constructMetaTable(&indexcoord.Catalog{
				Txn: &mockETCDKV{
					multiSave: func(m map[string]string) error {
						return nil
					},
				},
			}),
		}
		ic.UpdateStateCode(commonpb.StateCode_Healthy)
		resp, err := ic.DropIndex(context.Background(), &indexpb.DropIndexRequest{
			CollectionID: collID,
			PartitionIDs: []int64{partID},
			IndexName:    indexName,
			DropAll:      false,
		})
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_Success, resp.GetErrorCode())

		resp, err = ic.DropIndex(context.Background(), &indexpb.DropIndexRequest{
			CollectionID: collID,
			PartitionIDs: []int64{partID},
			IndexName:    indexName,
			DropAll:      false,
		})
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_Success, resp.GetErrorCode())

		resp, err = ic.DropIndex(context.Background(), &indexpb.DropIndexRequest{
			CollectionID: collID,
			PartitionIDs: nil,
			IndexName:    indexName,
			DropAll:      false,
		})
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_Success, resp.GetErrorCode())

		resp, err = ic.DropIndex(context.Background(), &indexpb.DropIndexRequest{
			CollectionID: collID,
			PartitionIDs: nil,
			IndexName:    indexName,
			DropAll:      false,
		})
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_Success, resp.GetErrorCode())
	})

	t.Run("fail", func(t *testing.T) {
		ic := &IndexCoord{
			metaTable: constructMetaTable(&indexcoord.Catalog{
				Txn: &mockETCDKV{
					multiSave: func(m map[string]string) error {
						return errors.New("error")
					},
				},
			}),
		}
		ic.UpdateStateCode(commonpb.StateCode_Healthy)

		resp, err := ic.DropIndex(context.Background(), &indexpb.DropIndexRequest{
			CollectionID: collID,
			PartitionIDs: []int64{partID},
			IndexName:    indexName,
			DropAll:      false,
		})
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_UnexpectedError, resp.GetErrorCode())

		resp, err = ic.DropIndex(context.Background(), &indexpb.DropIndexRequest{
			CollectionID: collID,
			PartitionIDs: nil,
			IndexName:    indexName,
			DropAll:      false,
		})
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_UnexpectedError, resp.GetErrorCode())
	})

	t.Run("multiple index but no index name", func(t *testing.T) {
		ic := &IndexCoord{
			metaTable: &metaTable{
				catalog:          &indexcoord.Catalog{Txn: NewMockEtcdKV()},
				indexLock:        sync.RWMutex{},
				segmentIndexLock: sync.RWMutex{},
				collectionIndexes: map[UniqueID]map[UniqueID]*model.Index{
					collID: {
						indexID: {
							TenantID:        "",
							CollectionID:    collID,
							FieldID:         fieldID,
							IndexID:         indexID,
							IndexName:       indexName,
							IsDeleted:       false,
							CreateTime:      10,
							TypeParams:      nil,
							IndexParams:     nil,
							IsAutoIndex:     false,
							UserIndexParams: nil,
						},
						indexID + 1: {
							TenantID:        "",
							CollectionID:    collID,
							FieldID:         fieldID + 1,
							IndexID:         indexID + 1,
							IndexName:       indexName + "1",
							IsDeleted:       false,
							CreateTime:      20,
							TypeParams:      nil,
							IndexParams:     nil,
							IsAutoIndex:     false,
							UserIndexParams: nil,
						},
					},
				},
				segmentIndexes:       nil,
				buildID2SegmentIndex: nil,
			},
		}
		ic.UpdateStateCode(commonpb.StateCode_Healthy)
		resp, err := ic.DropIndex(context.Background(), &indexpb.DropIndexRequest{
			CollectionID: collID,
			PartitionIDs: []int64{partID},
			IndexName:    "",
			DropAll:      false,
		})
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_UnexpectedError, resp.GetErrorCode())
	})
}

// TODO @xiaocai2333: add ut for error occurred.

//func TestIndexCoord_watchNodeLoop(t *testing.T) {
//	ech := make(chan *sessionutil.SessionEvent)
//	in := &IndexCoord{
//		loopWg:    sync.WaitGroup{},
//		loopCtx:   context.Background(),
//		eventChan: ech,
//		session: &sessionutil.Session{
//			TriggerKill: true,
//			ServerID:    0,
//		},
//	}
//	in.loopWg.Add(1)
//
//	flag := false
//	closed := false
//	sigDone := make(chan struct{}, 1)
//	sigQuit := make(chan struct{}, 1)
//	sc := make(chan os.Signal, 1)
//	signal.Notify(sc, syscall.SIGINT)
//	defer signal.Reset(syscall.SIGINT)
//
//	go func() {
//		in.watchNodeLoop()
//		flag = true
//		sigDone <- struct{}{}
//	}()
//	go func() {
//		<-sc
//		closed = true
//		sigQuit <- struct{}{}
//	}()
//
//	close(ech)
//	<-sigDone
//	<-sigQuit
//	assert.True(t, flag)
//	assert.True(t, closed)
//}
//
//func TestIndexCoord_watchMetaLoop(t *testing.T) {
//	ctx, cancel := context.WithCancel(context.Background())
//	ic := &IndexCoord{
//		loopCtx: ctx,
//		loopWg:  sync.WaitGroup{},
//	}
//
//	watchChan := make(chan clientv3.WatchResponse, 1024)
//
//	client := &mockETCDKV{
//		watchWithRevision: func(s string, i int64) clientv3.WatchChan {
//			return watchChan
//		},
//	}
//	mt := &metaTable{
//		client:            client,
//		indexBuildID2Meta: map[UniqueID]*Meta{},
//		etcdRevision:      0,
//		lock:              sync.RWMutex{},
//	}
//	ic.metaTable = mt
//
//	t.Run("watch chan panic", func(t *testing.T) {
//		ic.loopWg.Add(1)
//		watchChan <- clientv3.WatchResponse{Canceled: true}
//
//		assert.Panics(t, func() {
//			ic.watchMetaLoop()
//		})
//		ic.loopWg.Wait()
//	})
//
//	t.Run("watch chan new meta table panic", func(t *testing.T) {
//		client = &mockETCDKV{
//			watchWithRevision: func(s string, i int64) clientv3.WatchChan {
//				return watchChan
//			},
//			loadWithRevisionAndVersions: func(s string) ([]string, []string, []int64, int64, error) {
//				return []string{}, []string{}, []int64{}, 0, fmt.Errorf("error occurred")
//			},
//		}
//		mt = &metaTable{
//			client:            client,
//			indexBuildID2Meta: map[UniqueID]*Meta{},
//			etcdRevision:      0,
//			lock:              sync.RWMutex{},
//		}
//		ic.metaTable = mt
//		ic.loopWg.Add(1)
//		watchChan <- clientv3.WatchResponse{CompactRevision: 10}
//		assert.Panics(t, func() {
//			ic.watchMetaLoop()
//		})
//		ic.loopWg.Wait()
//	})
//
//	t.Run("watch chan new meta success", func(t *testing.T) {
//		ic.loopWg = sync.WaitGroup{}
//		client = &mockETCDKV{
//			watchWithRevision: func(s string, i int64) clientv3.WatchChan {
//				return watchChan
//			},
//			loadWithRevisionAndVersions: func(s string) ([]string, []string, []int64, int64, error) {
//				return []string{}, []string{}, []int64{}, 0, nil
//			},
//		}
//		mt = &metaTable{
//			client:            client,
//			indexBuildID2Meta: map[UniqueID]*Meta{},
//			etcdRevision:      0,
//			lock:              sync.RWMutex{},
//		}
//		ic.metaTable = mt
//		ic.loopWg.Add(1)
//		watchChan <- clientv3.WatchResponse{CompactRevision: 10}
//		go ic.watchMetaLoop()
//		cancel()
//		ic.loopWg.Wait()
//	})
//}
//
//func TestIndexCoord_GetComponentStates(t *testing.T) {
//	n := &IndexCoord{}
//	n.stateCode.Store(commonpb.StateCode_Healthy)
//	resp, err := n.GetComponentStates(context.Background())
//	assert.NoError(t, err)
//	assert.Equal(t, commonpb.ErrorCode_Success, resp.Status.ErrorCode)
//	assert.Equal(t, common.NotRegisteredID, resp.State.NodeID)
//	n.session = &sessionutil.Session{}
//	n.session.UpdateRegistered(true)
//	resp, err = n.GetComponentStates(context.Background())
//	assert.NoError(t, err)
//	assert.Equal(t, commonpb.ErrorCode_Success, resp.Status.ErrorCode)
//}
//
//func TestIndexCoord_NotHealthy(t *testing.T) {
//	ic := &IndexCoord{}
//	ic.stateCode.Store(commonpb.StateCode_Abnormal)
//	req := &indexpb.BuildIndexRequest{}
//	resp, err := ic.BuildIndex(context.Background(), req)
//	assert.Error(t, err)
//	assert.Equal(t, commonpb.ErrorCode_UnexpectedError, resp.Status.ErrorCode)
//
//	req2 := &indexpb.DropIndexRequest{}
//	status, err := ic.DropIndex(context.Background(), req2)
//	assert.Nil(t, err)
//	assert.Equal(t, commonpb.ErrorCode_UnexpectedError, status.ErrorCode)
//
//	req3 := &indexpb.GetIndexStatesRequest{}
//	resp2, err := ic.GetIndexStates(context.Background(), req3)
//	assert.Nil(t, err)
//	assert.Equal(t, commonpb.ErrorCode_UnexpectedError, resp2.Status.ErrorCode)
//
//	req4 := &indexpb.GetIndexFilePathsRequest{
//		IndexBuildIDs: []UniqueID{1, 2},
//	}
//	resp4, err := ic.GetIndexFilePaths(context.Background(), req4)
//	assert.Nil(t, err)
//	assert.Equal(t, commonpb.ErrorCode_UnexpectedError, resp4.Status.ErrorCode)
//
//	req5 := &indexpb.RemoveIndexRequest{}
//	resp5, err := ic.RemoveIndex(context.Background(), req5)
//	assert.Nil(t, err)
//	assert.Equal(t, commonpb.ErrorCode_UnexpectedError, resp5.GetErrorCode())
//}
//
//func TestIndexCoord_GetIndexFilePaths(t *testing.T) {
//	ic := &IndexCoord{
//		metaTable: &metaTable{
//			indexBuildID2Meta: map[UniqueID]*Meta{
//				1: {
//					indexMeta: &indexpb.IndexMeta{
//						IndexBuildID:   1,
//						State:          commonpb.IndexState_Finished,
//						IndexFileKeys: []string{"indexFiles-1", "indexFiles-2"},
//					},
//				},
//				2: {
//					indexMeta: &indexpb.IndexMeta{
//						IndexBuildID: 2,
//						State:        commonpb.IndexState_Failed,
//					},
//				},
//			},
//		},
//	}
//
//	ic.stateCode.Store(commonpb.StateCode_Healthy)
//
//	t.Run("GetIndexFilePaths success", func(t *testing.T) {
//		resp, err := ic.GetIndexFilePaths(context.Background(), &indexpb.GetIndexFilePathsRequest{IndexBuildIDs: []UniqueID{1}})
//		assert.NoError(t, err)
//		assert.Equal(t, commonpb.ErrorCode_Success, resp.Status.ErrorCode)
//		assert.Equal(t, 1, len(resp.FilePaths))
//		assert.ElementsMatch(t, resp.FilePaths[0].IndexFileKeys, []string{"indexFiles-1", "indexFiles-2"})
//	})
//
//	t.Run("GetIndexFilePaths failed", func(t *testing.T) {
//		resp, err := ic.GetIndexFilePaths(context.Background(), &indexpb.GetIndexFilePathsRequest{IndexBuildIDs: []UniqueID{2}})
//		assert.NoError(t, err)
//		assert.Equal(t, commonpb.ErrorCode_Success, resp.Status.ErrorCode)
//		assert.Equal(t, 0, len(resp.FilePaths[0].IndexFileKeys))
//	})
//
//	t.Run("set DataCoord with nil", func(t *testing.T) {
//		err := ic.SetDataCoord(nil)
//		assert.Error(t, err)
//	})
//}
//
//func Test_tryAcquireSegmentReferLock(t *testing.T) {
//	ic := &IndexCoord{
//		session: &sessionutil.Session{
//			ServerID: 1,
//		},
//	}
//	dcm := &DataCoordMock{
//		Err:  false,
//		Fail: false,
//	}
//	cmm := &ChunkManagerMock{
//		Err:  false,
//		Fail: false,
//	}
//
//	ic.dataCoordClient = dcm
//	ic.chunkManager = cmm
//
//	t.Run("success", func(t *testing.T) {
//		err := ic.tryAcquireSegmentReferLock(context.Background(), 1, 1, []UniqueID{1})
//		assert.Nil(t, err)
//	})
//
//	t.Run("error", func(t *testing.T) {
//		dcmE := &DataCoordMock{
//			Err:  true,
//			Fail: false,
//		}
//		ic.dataCoordClient = dcmE
//		err := ic.tryAcquireSegmentReferLock(context.Background(), 1, 1, []UniqueID{1})
//		assert.Error(t, err)
//	})
//
//	t.Run("Fail", func(t *testing.T) {
//		dcmF := &DataCoordMock{
//			Err:  false,
//			Fail: true,
//		}
//		ic.dataCoordClient = dcmF
//		err := ic.tryAcquireSegmentReferLock(context.Background(), 1, 1, []UniqueID{1})
//		assert.Error(t, err)
//	})
//}
//
//func Test_tryReleaseSegmentReferLock(t *testing.T) {
//	ic := &IndexCoord{
//		session: &sessionutil.Session{
//			ServerID: 1,
//		},
//	}
//	dcm := &DataCoordMock{
//		Err:  false,
//		Fail: false,
//	}
//
//	ic.dataCoordClient = dcm
//
//	t.Run("success", func(t *testing.T) {
//		err := ic.tryReleaseSegmentReferLock(context.Background(), 1, 1)
//		assert.NoError(t, err)
//	})
//}
//
//func TestIndexCoord_RemoveIndex(t *testing.T) {
//	ic := &IndexCoord{
//		metaTable: &metaTable{},
//		indexBuilder: &indexBuilder{
//			notify: make(chan struct{}, 10),
//		},
//	}
//	ic.stateCode.Store(commonpb.StateCode_Healthy)
//	status, err := ic.RemoveIndex(context.Background(), &indexpb.RemoveIndexRequest{BuildIDs: []UniqueID{0}})
//	assert.Nil(t, err)
//	assert.Equal(t, commonpb.ErrorCode_Success, status.GetErrorCode())
//}

func TestIndexCoord_pullSegmentInfo(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		ic := &IndexCoord{
			dataCoordClient: NewDataCoordMock(),
		}
		info, err := ic.pullSegmentInfo(context.Background(), segID)
		assert.NoError(t, err)
		assert.NotNil(t, info)
	})

	t.Run("fail", func(t *testing.T) {
		ic := &IndexCoord{
			dataCoordClient: &DataCoordMock{
				CallGetSegmentInfo: func(ctx context.Context, req *datapb.GetSegmentInfoRequest) (*datapb.GetSegmentInfoResponse, error) {
					return nil, errors.New("error")
				},
			},
		}
		info, err := ic.pullSegmentInfo(context.Background(), segID)
		assert.Error(t, err)
		assert.Nil(t, info)
	})

	t.Run("not success", func(t *testing.T) {
		ic := &IndexCoord{
			dataCoordClient: &DataCoordMock{
				CallGetSegmentInfo: func(ctx context.Context, req *datapb.GetSegmentInfoRequest) (*datapb.GetSegmentInfoResponse, error) {
					return &datapb.GetSegmentInfoResponse{
						Status: &commonpb.Status{
							ErrorCode: commonpb.ErrorCode_UnexpectedError,
							Reason:    "fail reason",
						},
					}, nil
				},
			},
		}
		info, err := ic.pullSegmentInfo(context.Background(), segID)
		assert.Error(t, err)
		assert.Nil(t, info)
	})

	t.Run("failed to get segment", func(t *testing.T) {
		ic := &IndexCoord{
			dataCoordClient: &DataCoordMock{
				CallGetSegmentInfo: func(ctx context.Context, req *datapb.GetSegmentInfoRequest) (*datapb.GetSegmentInfoResponse, error) {
					return &datapb.GetSegmentInfoResponse{
						Status: &commonpb.Status{
							ErrorCode: commonpb.ErrorCode_UnexpectedError,
							Reason:    msgSegmentNotFound(segID),
						},
					}, nil
				},
			},
		}
		info, err := ic.pullSegmentInfo(context.Background(), segID)
		assert.Error(t, err)
		assert.Nil(t, info)
	})

	t.Run("seg not exist", func(t *testing.T) {
		ic := &IndexCoord{
			dataCoordClient: &DataCoordMock{
				CallGetSegmentInfo: func(ctx context.Context, req *datapb.GetSegmentInfoRequest) (*datapb.GetSegmentInfoResponse, error) {
					return &datapb.GetSegmentInfoResponse{
						Status: &commonpb.Status{
							ErrorCode: commonpb.ErrorCode_Success,
							Reason:    "",
						},
						Infos: []*datapb.SegmentInfo{},
					}, nil
				},
			},
		}
		info, err := ic.pullSegmentInfo(context.Background(), segID)
		assert.ErrorIs(t, err, ErrSegmentNotFound)
		assert.Nil(t, info)
	})
}

func TestIndexCoord_CheckHealth(t *testing.T) {
	t.Run("not healthy", func(t *testing.T) {
		ctx := context.Background()
		ic := &IndexCoord{session: &sessionutil.Session{ServerID: 1}}
		ic.stateCode.Store(commonpb.StateCode_Abnormal)
		resp, err := ic.CheckHealth(ctx, &milvuspb.CheckHealthRequest{})
		assert.NoError(t, err)
		assert.Equal(t, false, resp.IsHealthy)
		assert.NotEmpty(t, resp.Reasons)
	})

	t.Run("index node health check is ok", func(t *testing.T) {
		ctx := context.Background()
		indexNode := indexnode.NewIndexNodeMock()
		nm := NewNodeManager(ctx)
		nm.nodeClients[1] = indexNode
		ic := &IndexCoord{
			session:     &sessionutil.Session{ServerID: 1},
			nodeManager: nm,
		}
		ic.stateCode.Store(commonpb.StateCode_Healthy)
		resp, err := ic.CheckHealth(ctx, &milvuspb.CheckHealthRequest{})
		assert.NoError(t, err)
		assert.Equal(t, true, resp.IsHealthy)
		assert.Empty(t, resp.Reasons)
	})

	t.Run("index node health check is fail", func(t *testing.T) {
		ctx := context.Background()
		indexNode := indexnode.NewIndexNodeMock()
		indexNode.CallGetComponentStates = func(ctx context.Context) (*milvuspb.ComponentStates, error) {
			return &milvuspb.ComponentStates{
				State: &milvuspb.ComponentInfo{
					NodeID:    1,
					StateCode: commonpb.StateCode_Abnormal,
				},
				SubcomponentStates: nil,
				Status: &commonpb.Status{
					ErrorCode: commonpb.ErrorCode_Success,
				},
			}, nil
		}
		nm := NewNodeManager(ctx)
		nm.nodeClients[1] = indexNode
		ic := &IndexCoord{
			nodeManager: nm,
			session:     &sessionutil.Session{ServerID: 1},
		}
		ic.stateCode.Store(commonpb.StateCode_Healthy)

		resp, err := ic.CheckHealth(ctx, &milvuspb.CheckHealthRequest{})
		assert.NoError(t, err)
		assert.Equal(t, false, resp.IsHealthy)
		assert.NotEmpty(t, resp.Reasons)
	})
}

func TestIndexCoord_CreateIndex(t *testing.T) {
	ic := &IndexCoord{
		metaTable: &metaTable{
			catalog:          nil,
			indexLock:        sync.RWMutex{},
			segmentIndexLock: sync.RWMutex{},
			collectionIndexes: map[UniqueID]map[UniqueID]*model.Index{
				collID: {
					indexID: {
						TenantID:     "",
						CollectionID: collID,
						FieldID:      fieldID,
						IndexID:      indexID,
						IndexName:    indexName,
						IsDeleted:    false,
						CreateTime:   10,
						TypeParams: []*commonpb.KeyValuePair{
							{
								Key:   "dim",
								Value: "128",
							},
						},
						IndexParams: []*commonpb.KeyValuePair{
							{
								Key:   "index_type",
								Value: "IVF_FLAT",
							},
							{
								Key:   "metrics_type",
								Value: "HAMMING",
							},
							{
								Key:   "nlist",
								Value: "128",
							},
						},
						IsAutoIndex:     false,
						UserIndexParams: nil,
					},
				},
			},
			segmentIndexes: map[UniqueID]map[UniqueID]*model.SegmentIndex{
				segID: {
					indexID: {
						SegmentID:     segID,
						CollectionID:  collID,
						PartitionID:   partID,
						NumRows:       1025,
						IndexID:       indexID,
						BuildID:       buildID,
						NodeID:        nodeID,
						IndexVersion:  1,
						IndexState:    commonpb.IndexState_Unissued,
						FailReason:    "",
						IsDeleted:     false,
						CreateTime:    10,
						IndexFileKeys: nil,
						IndexSize:     0,
						WriteHandoff:  false,
					},
				},
			},
			buildID2SegmentIndex: map[UniqueID]*model.SegmentIndex{
				buildID: {
					SegmentID:     segID,
					CollectionID:  collID,
					PartitionID:   partID,
					NumRows:       1025,
					IndexID:       indexID,
					BuildID:       buildID,
					NodeID:        nodeID,
					IndexVersion:  1,
					IndexState:    commonpb.IndexState_Unissued,
					FailReason:    "",
					IsDeleted:     false,
					CreateTime:    10,
					IndexFileKeys: nil,
					IndexSize:     0,
					WriteHandoff:  false,
				},
			},
		},
	}
	ic.UpdateStateCode(commonpb.StateCode_Healthy)

	t.Run("index already exist, but params are inconsistent", func(t *testing.T) {
		req := &indexpb.CreateIndexRequest{
			CollectionID: collID,
			FieldID:      fieldID,
			IndexName:    indexName,
			TypeParams: []*commonpb.KeyValuePair{
				{
					Key:   "dim",
					Value: "128",
				},
			},
			IndexParams: []*commonpb.KeyValuePair{
				{
					Key:   "index_type",
					Value: "IVF_FLAT",
				},
				{
					Key:   "metrics_type",
					Value: "HAMMING",
				},
				{
					Key:   "params",
					Value: "{nlist: 128}",
				},
			},
			Timestamp:       0,
			IsAutoIndex:     false,
			UserIndexParams: nil,
		}

		resp, err := ic.CreateIndex(context.Background(), req)
		assert.NoError(t, err)
		assert.Equal(t, commonpb.ErrorCode_UnexpectedError, resp.GetErrorCode())
	})
}
