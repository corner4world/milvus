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

// Package datanode implements data persistence logic.
//
// Data node persists insert logs into persistent storage like minIO/S3.
package datanode

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strconv"

	"github.com/milvus-io/milvus-proto/go-api/commonpb"
	"github.com/milvus-io/milvus-proto/go-api/milvuspb"
	"github.com/milvus-io/milvus-proto/go-api/schemapb"
	"go.uber.org/zap"

	"github.com/milvus-io/milvus/internal/proto/datapb"
	"github.com/milvus-io/milvus/internal/proto/etcdpb"
	"github.com/milvus-io/milvus/internal/proto/internalpb"
	"github.com/milvus-io/milvus/internal/proto/rootcoordpb"

	"github.com/milvus-io/milvus/internal/common"
	"github.com/milvus-io/milvus/internal/log"
	"github.com/milvus-io/milvus/internal/metrics"
	"github.com/milvus-io/milvus/internal/storage"
	"github.com/milvus-io/milvus/internal/util/commonpbutil"
	"github.com/milvus-io/milvus/internal/util/importutil"
	"github.com/milvus-io/milvus/internal/util/metautil"
	"github.com/milvus-io/milvus/internal/util/metricsinfo"
	"github.com/milvus-io/milvus/internal/util/paramtable"
	"github.com/milvus-io/milvus/internal/util/retry"
	"github.com/milvus-io/milvus/internal/util/timerecord"
	"github.com/milvus-io/milvus/internal/util/tsoutil"
	"github.com/milvus-io/milvus/internal/util/typeutil"
)

// WatchDmChannels is not in use
func (node *DataNode) WatchDmChannels(ctx context.Context, in *datapb.WatchDmChannelsRequest) (*commonpb.Status, error) {
	log.Warn("DataNode WatchDmChannels is not in use")

	return &commonpb.Status{
		ErrorCode: commonpb.ErrorCode_Success,
		Reason:    "watchDmChannels do nothing",
	}, nil
}

// GetComponentStates will return current state of DataNode
func (node *DataNode) GetComponentStates(ctx context.Context) (*milvuspb.ComponentStates, error) {
	log.Debug("DataNode current state", zap.Any("State", node.stateCode.Load()))
	nodeID := common.NotRegisteredID
	if node.session != nil && node.session.Registered() {
		nodeID = node.session.ServerID
	}
	states := &milvuspb.ComponentStates{
		State: &milvuspb.ComponentInfo{
			// NodeID:    Params.NodeID, // will race with DataNode.Register()
			NodeID:    nodeID,
			Role:      node.Role,
			StateCode: node.stateCode.Load().(commonpb.StateCode),
		},
		SubcomponentStates: make([]*milvuspb.ComponentInfo, 0),
		Status:             &commonpb.Status{ErrorCode: commonpb.ErrorCode_Success},
	}
	return states, nil
}

// FlushSegments packs flush messages into flowGraph through flushChan.
//
//	 DataCoord calls FlushSegments if the segment is seal&flush only.
//		If DataNode receives a valid segment to flush, new flush message for the segment should be ignored.
//		So if receiving calls to flush segment A, DataNode should guarantee the segment to be flushed.
func (node *DataNode) FlushSegments(ctx context.Context, req *datapb.FlushSegmentsRequest) (*commonpb.Status, error) {
	metrics.DataNodeFlushReqCounter.WithLabelValues(
		fmt.Sprint(paramtable.GetNodeID()),
		MetricRequestsTotal).Inc()

	errStatus := &commonpb.Status{
		ErrorCode: commonpb.ErrorCode_UnexpectedError,
	}

	if !node.isHealthy() {
		errStatus.Reason = "dataNode not in HEALTHY state"
		return errStatus, nil
	}

	if req.GetBase().GetTargetID() != node.session.ServerID {
		log.Warn("flush segment target id not matched",
			zap.Int64("targetID", req.GetBase().GetTargetID()),
			zap.Int64("serverID", node.session.ServerID),
		)
		status := &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_NodeIDNotMatch,
			Reason:    common.WrapNodeIDNotMatchMsg(req.GetBase().GetTargetID(), node.session.ServerID),
		}
		return status, nil
	}

	log.Info("receiving FlushSegments request",
		zap.Int64("collectionID", req.GetCollectionID()),
		zap.Int64s("sealedSegments", req.GetSegmentIDs()),
	)

	segmentIDs := req.GetSegmentIDs()
	var flushedSeg []UniqueID
	for _, segID := range segmentIDs {
		// if the segment in already being flushed, skip it.
		if node.segmentCache.checkIfCached(segID) {
			logDupFlush(req.GetCollectionID(), segID)
			continue
		}
		// Get the flush channel for the given segment ID.
		// If no flush channel is found, report an error.
		flushCh, err := node.flowgraphManager.getFlushCh(segID)
		if err != nil {
			errStatus.Reason = "no flush channel found for the segment, unable to flush"
			log.Error(errStatus.Reason, zap.Int64("segmentID", segID), zap.Error(err))
			return errStatus, nil
		}

		// Double check that the segment is still not cached.
		// Skip this flush if segment ID is cached, otherwise cache the segment ID and proceed.
		exist := node.segmentCache.checkOrCache(segID)
		if exist {
			logDupFlush(req.GetCollectionID(), segID)
			continue
		}
		// flushedSeg is only for logging purpose.
		flushedSeg = append(flushedSeg, segID)
		// Send the segment to its flush channel.
		flushCh <- flushMsg{
			msgID:        req.GetBase().GetMsgID(),
			timestamp:    req.GetBase().GetTimestamp(),
			segmentID:    segID,
			collectionID: req.GetCollectionID(),
		}
	}

	// Log success flushed segments.
	if len(flushedSeg) > 0 {
		log.Info("sending segments to flush channel",
			zap.Int64("collectionID", req.GetCollectionID()),
			zap.Int64s("sealedSegments", flushedSeg))
	}

	metrics.DataNodeFlushReqCounter.WithLabelValues(
		fmt.Sprint(paramtable.GetNodeID()),
		MetricRequestsSuccess).Inc()
	return &commonpb.Status{
		ErrorCode: commonpb.ErrorCode_Success,
	}, nil
}

// ResendSegmentStats resend un-flushed segment stats back upstream to DataCoord by resending DataNode time tick message.
// It returns a list of segments to be sent.
func (node *DataNode) ResendSegmentStats(ctx context.Context, req *datapb.ResendSegmentStatsRequest) (*datapb.ResendSegmentStatsResponse, error) {
	log.Info("start resending segment stats, if any",
		zap.Int64("DataNode ID", paramtable.GetNodeID()))
	segResent := node.flowgraphManager.resendTT()
	log.Info("found segment(s) with stats to resend",
		zap.Int64s("segment IDs", segResent))
	return &datapb.ResendSegmentStatsResponse{
		Status: &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_Success,
		},
		SegResent: segResent,
	}, nil
}

// GetTimeTickChannel currently do nothing
func (node *DataNode) GetTimeTickChannel(ctx context.Context) (*milvuspb.StringResponse, error) {
	return &milvuspb.StringResponse{
		Status: &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_Success,
		},
	}, nil
}

// GetStatisticsChannel currently do nothing
func (node *DataNode) GetStatisticsChannel(ctx context.Context) (*milvuspb.StringResponse, error) {
	return &milvuspb.StringResponse{
		Status: &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_Success,
		},
	}, nil
}

// ShowConfigurations returns the configurations of DataNode matching req.Pattern
func (node *DataNode) ShowConfigurations(ctx context.Context, req *internalpb.ShowConfigurationsRequest) (*internalpb.ShowConfigurationsResponse, error) {
	log.Debug("DataNode.ShowConfigurations", zap.String("pattern", req.Pattern))
	if !node.isHealthy() {
		log.Warn("DataNode.ShowConfigurations failed",
			zap.Int64("nodeId", paramtable.GetNodeID()),
			zap.String("req", req.Pattern),
			zap.Error(errDataNodeIsUnhealthy(paramtable.GetNodeID())))

		return &internalpb.ShowConfigurationsResponse{
			Status: &commonpb.Status{
				ErrorCode: commonpb.ErrorCode_UnexpectedError,
				Reason:    msgDataNodeIsUnhealthy(paramtable.GetNodeID()),
			},
			Configuations: nil,
		}, nil
	}
	configList := make([]*commonpb.KeyValuePair, 0)
	for key, value := range Params.GetComponentConfigurations(ctx, "datanode", req.Pattern) {
		configList = append(configList,
			&commonpb.KeyValuePair{
				Key:   key,
				Value: value,
			})
	}

	return &internalpb.ShowConfigurationsResponse{
		Status: &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_Success,
			Reason:    "",
		},
		Configuations: configList,
	}, nil
}

// GetMetrics return datanode metrics
func (node *DataNode) GetMetrics(ctx context.Context, req *milvuspb.GetMetricsRequest) (*milvuspb.GetMetricsResponse, error) {
	if !node.isHealthy() {
		log.Warn("DataNode.GetMetrics failed",
			zap.Int64("nodeID", paramtable.GetNodeID()),
			zap.String("req", req.Request),
			zap.Error(errDataNodeIsUnhealthy(paramtable.GetNodeID())))

		return &milvuspb.GetMetricsResponse{
			Status: &commonpb.Status{
				ErrorCode: commonpb.ErrorCode_UnexpectedError,
				Reason:    msgDataNodeIsUnhealthy(paramtable.GetNodeID()),
			},
		}, nil
	}

	metricType, err := metricsinfo.ParseMetricType(req.Request)
	if err != nil {
		log.Warn("DataNode.GetMetrics failed to parse metric type",
			zap.Int64("nodeID", paramtable.GetNodeID()),
			zap.String("req", req.Request),
			zap.Error(err))

		return &milvuspb.GetMetricsResponse{
			Status: &commonpb.Status{
				ErrorCode: commonpb.ErrorCode_UnexpectedError,
				Reason:    fmt.Sprintf("datanode GetMetrics failed, nodeID=%d, err=%s", paramtable.GetNodeID(), err.Error()),
			},
		}, nil
	}

	if metricType == metricsinfo.SystemInfoMetrics {
		systemInfoMetrics, err := node.getSystemInfoMetrics(ctx, req)
		if err != nil {
			log.Warn("DataNode GetMetrics failed", zap.Int64("nodeID", paramtable.GetNodeID()), zap.Error(err))
			return &milvuspb.GetMetricsResponse{
				Status: &commonpb.Status{
					ErrorCode: commonpb.ErrorCode_UnexpectedError,
					Reason:    fmt.Sprintf("datanode GetMetrics failed, nodeID=%d, err=%s", paramtable.GetNodeID(), err.Error()),
				},
			}, nil
		}

		return systemInfoMetrics, nil
	}

	log.RatedWarn(60, "DataNode.GetMetrics failed, request metric type is not implemented yet",
		zap.Int64("nodeID", paramtable.GetNodeID()),
		zap.String("req", req.Request),
		zap.String("metric_type", metricType))

	return &milvuspb.GetMetricsResponse{
		Status: &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_UnexpectedError,
			Reason:    metricsinfo.MsgUnimplementedMetric,
		},
	}, nil
}

// Compaction handles compaction request from DataCoord
// returns status as long as compaction task enqueued or invalid
func (node *DataNode) Compaction(ctx context.Context, req *datapb.CompactionPlan) (*commonpb.Status, error) {
	status := &commonpb.Status{
		ErrorCode: commonpb.ErrorCode_UnexpectedError,
	}

	ds, ok := node.flowgraphManager.getFlowgraphService(req.GetChannel())
	if !ok {
		log.Warn("illegel compaction plan, channel not in this DataNode", zap.String("channel name", req.GetChannel()))
		status.Reason = errIllegalCompactionPlan.Error()
		return status, nil
	}

	if !node.compactionExecutor.channelValidateForCompaction(req.GetChannel()) {
		log.Warn("channel of compaction is marked invalid in compaction executor", zap.String("channel name", req.GetChannel()))
		status.Reason = "channel marked invalid"
		return status, nil
	}

	binlogIO := &binlogIO{node.chunkManager, ds.idAllocator}
	task := newCompactionTask(
		node.ctx,
		binlogIO, binlogIO,
		ds.channel,
		ds.flushManager,
		ds.idAllocator,
		req,
		node.chunkManager,
	)

	node.compactionExecutor.execute(task)

	return &commonpb.Status{
		ErrorCode: commonpb.ErrorCode_Success,
	}, nil
}

// GetCompactionState called by DataCoord
// return status of all compaction plans
func (node *DataNode) GetCompactionState(ctx context.Context, req *datapb.CompactionStateRequest) (*datapb.CompactionStateResponse, error) {
	if !node.isHealthy() {
		return &datapb.CompactionStateResponse{
			Status: &commonpb.Status{
				ErrorCode: commonpb.ErrorCode_UnexpectedError,
				Reason:    "DataNode is unhealthy",
			},
		}, nil
	}
	results := make([]*datapb.CompactionStateResult, 0)
	node.compactionExecutor.executing.Range(func(k, v any) bool {
		results = append(results, &datapb.CompactionStateResult{
			State:  commonpb.CompactionState_Executing,
			PlanID: k.(UniqueID),
		})
		return true
	})
	node.compactionExecutor.completed.Range(func(k, v any) bool {
		results = append(results, &datapb.CompactionStateResult{
			State:  commonpb.CompactionState_Completed,
			PlanID: k.(UniqueID),
			Result: v.(*datapb.CompactionResult),
		})
		node.compactionExecutor.completed.Delete(k)
		return true
	})

	if len(results) > 0 {
		log.Info("Compaction results", zap.Any("results", results))
	}
	return &datapb.CompactionStateResponse{
		Status:  &commonpb.Status{ErrorCode: commonpb.ErrorCode_Success},
		Results: results,
	}, nil
}

// SyncSegments called by DataCoord, sync the compacted segments' meta between DC and DN
func (node *DataNode) SyncSegments(ctx context.Context, req *datapb.SyncSegmentsRequest) (*commonpb.Status, error) {
	log.Ctx(ctx).Info("DataNode receives SyncSegments",
		zap.Int64("planID", req.GetPlanID()),
		zap.Int64("target segmentID", req.GetCompactedTo()),
		zap.Int64s("compacted from", req.GetCompactedFrom()),
		zap.Int64("numOfRows", req.GetNumOfRows()),
	)
	status := &commonpb.Status{ErrorCode: commonpb.ErrorCode_UnexpectedError}

	if !node.isHealthy() {
		status.Reason = "DataNode is unhealthy"
		return status, nil
	}

	if len(req.GetCompactedFrom()) <= 0 {
		status.Reason = "invalid request, compacted from segments shouldn't be empty"
		return status, nil
	}

	getChannel := func() (int64, Channel) {
		for _, segmentFrom := range req.GetCompactedFrom() {
			channel, err := node.flowgraphManager.getChannel(segmentFrom)
			if err != nil {
				log.Warn("invalid segmentID", zap.Int64("segment_from", segmentFrom), zap.Error(err))
				continue
			}
			return segmentFrom, channel
		}
		return 0, nil
	}
	oneSegment, channel := getChannel()
	if channel == nil {
		log.Warn("no available channel")
		status.ErrorCode = commonpb.ErrorCode_Success
		return status, nil
	}

	ds, ok := node.flowgraphManager.getFlowgraphService(channel.getChannelName(oneSegment))
	if !ok {
		status.Reason = fmt.Sprintf("failed to find flow graph service, segmentID: %d", oneSegment)
		return status, nil
	}

	// oneSegment is definitely in the channel, guaranteed by the check before.
	collID, partID, _ := channel.getCollectionAndPartitionID(oneSegment)
	targetSeg := &Segment{
		collectionID: collID,
		partitionID:  partID,
		segmentID:    req.GetCompactedTo(),
		numRows:      req.GetNumOfRows(),
	}

	err := channel.InitPKstats(ctx, targetSeg, req.GetStatsLogs(), tsoutil.GetCurrentTime())
	if err != nil {
		status.Reason = fmt.Sprintf("init pk stats fail, err=%s", err.Error())
		return status, nil
	}

	// block all flow graph so it's safe to remove segment
	ds.fg.Blockall()
	defer ds.fg.Unblock()
	if err := channel.mergeFlushedSegments(targetSeg, req.GetPlanID(), req.GetCompactedFrom()); err != nil {
		status.Reason = err.Error()
		return status, nil
	}

	status.ErrorCode = commonpb.ErrorCode_Success
	return status, nil
}

// Import data files(json, numpy, etc.) on MinIO/S3 storage, read and parse them into sealed segments
func (node *DataNode) Import(ctx context.Context, req *datapb.ImportTaskRequest) (*commonpb.Status, error) {
	log.Info("DataNode receive import request",
		zap.Int64("task ID", req.GetImportTask().GetTaskId()),
		zap.Int64("collection ID", req.GetImportTask().GetCollectionId()),
		zap.Int64("partition ID", req.GetImportTask().GetPartitionId()),
		zap.Strings("channel names", req.GetImportTask().GetChannelNames()),
		zap.Int64s("working dataNodes", req.WorkingNodes))
	defer func() {
		log.Info("DataNode finish import request", zap.Int64("task ID", req.GetImportTask().GetTaskId()))
	}()

	importResult := &rootcoordpb.ImportResult{
		Status: &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_Success,
		},
		TaskId:     req.GetImportTask().TaskId,
		DatanodeId: paramtable.GetNodeID(),
		State:      commonpb.ImportState_ImportStarted,
		Segments:   make([]int64, 0),
		AutoIds:    make([]int64, 0),
		RowCount:   0,
	}

	// Spawn a new context to ignore cancellation from parental context.
	newCtx, cancel := context.WithTimeout(context.TODO(), ImportCallTimeout)
	defer cancel()
	// func to report import state to RootCoord.
	reportFunc := func(res *rootcoordpb.ImportResult) error {
		status, err := node.rootCoord.ReportImport(ctx, res)
		if err != nil {
			log.Error("fail to report import state to RootCoord", zap.Error(err))
			return err
		}
		if status != nil && status.ErrorCode != commonpb.ErrorCode_Success {
			return errors.New(status.GetReason())
		}
		return nil
	}

	if !node.isHealthy() {
		log.Warn("DataNode import failed",
			zap.Int64("collection ID", req.GetImportTask().GetCollectionId()),
			zap.Int64("partition ID", req.GetImportTask().GetPartitionId()),
			zap.Int64("task ID", req.GetImportTask().GetTaskId()),
			zap.Error(errDataNodeIsUnhealthy(paramtable.GetNodeID())))

		return &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_UnexpectedError,
			Reason:    msgDataNodeIsUnhealthy(paramtable.GetNodeID()),
		}, nil
	}

	// get a timestamp for all the rows
	// Ignore cancellation from parent context.
	rep, err := node.rootCoord.AllocTimestamp(newCtx, &rootcoordpb.AllocTimestampRequest{
		Base: commonpbutil.NewMsgBase(
			commonpbutil.WithMsgType(commonpb.MsgType_RequestTSO),
			commonpbutil.WithMsgID(0),
			commonpbutil.WithSourceID(paramtable.GetNodeID()),
		),
		Count: 1,
	})

	if rep.Status.ErrorCode != commonpb.ErrorCode_Success || err != nil {
		msg := "DataNode alloc ts failed"
		log.Warn(msg)
		importResult.State = commonpb.ImportState_ImportFailed
		importResult.Infos = append(importResult.Infos, &commonpb.KeyValuePair{Key: importutil.FailedReason, Value: msg})
		if reportErr := reportFunc(importResult); reportErr != nil {
			log.Warn("fail to report import state to RootCoord", zap.Error(reportErr))
		}
		if err != nil {
			return &commonpb.Status{
				ErrorCode: commonpb.ErrorCode_UnexpectedError,
				Reason:    msg,
			}, nil
		}
	}

	ts := rep.GetTimestamp()

	// get collection schema and shard number
	metaService := newMetaService(node.rootCoord, req.GetImportTask().GetCollectionId())
	colInfo, err := metaService.getCollectionInfo(newCtx, req.GetImportTask().GetCollectionId(), 0)
	if err != nil {
		log.Warn("failed to get collection info for collection ID",
			zap.Int64("task ID", req.GetImportTask().GetTaskId()),
			zap.Int64("collection ID", req.GetImportTask().GetCollectionId()),
			zap.Error(err))
		importResult.State = commonpb.ImportState_ImportFailed
		importResult.Infos = append(importResult.Infos, &commonpb.KeyValuePair{Key: importutil.FailedReason, Value: err.Error()})
		reportErr := reportFunc(importResult)
		if reportErr != nil {
			log.Warn("fail to report import state to RootCoord", zap.Error(err))
		}
		return &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_UnexpectedError,
			Reason:    err.Error(),
		}, nil
	}

	returnFailFunc := func(inputErr error) (*commonpb.Status, error) {
		log.Warn("import wrapper failed to parse import request",
			zap.Int64("task ID", req.GetImportTask().GetTaskId()),
			zap.Error(inputErr))
		importResult.State = commonpb.ImportState_ImportFailed
		importResult.Infos = append(importResult.Infos, &commonpb.KeyValuePair{Key: importutil.FailedReason, Value: inputErr.Error()})
		reportErr := reportFunc(importResult)
		if reportErr != nil {
			log.Warn("fail to report import state to RootCoord", zap.Error(inputErr))
		}
		return &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_UnexpectedError,
			Reason:    inputErr.Error(),
		}, nil
	}

	// parse files and generate segments
	segmentSize := Params.DataCoordCfg.SegmentMaxSize.GetAsInt64() * 1024 * 1024
	importWrapper := importutil.NewImportWrapper(newCtx, colInfo.GetSchema(), colInfo.GetShardsNum(), segmentSize, node.rowIDAllocator,
		node.chunkManager, importResult, reportFunc)
	importWrapper.SetCallbackFunctions(assignSegmentFunc(node, req),
		createBinLogsFunc(node, req, colInfo.GetSchema(), ts),
		saveSegmentFunc(node, req, importResult, ts))
	// todo: pass tsStart and tsStart after import_wrapper support
	tsStart, tsEnd, err := importutil.ParseTSFromOptions(req.GetImportTask().GetInfos())
	isBackup := importutil.IsBackup(req.GetImportTask().GetInfos())
	if err != nil {
		return returnFailFunc(err)
	}
	log.Info("import time range", zap.Uint64("start_ts", tsStart), zap.Uint64("end_ts", tsEnd))
	err = importWrapper.Import(req.GetImportTask().GetFiles(),
		importutil.ImportOptions{OnlyValidate: false, TsStartPoint: tsStart, TsEndPoint: tsEnd, IsBackup: isBackup})
	if err != nil {
		return returnFailFunc(err)
	}

	resp := &commonpb.Status{
		ErrorCode: commonpb.ErrorCode_Success,
	}
	return resp, nil
}

// AddImportSegment adds the import segment to the current DataNode.
func (node *DataNode) AddImportSegment(ctx context.Context, req *datapb.AddImportSegmentRequest) (*datapb.AddImportSegmentResponse, error) {
	log.Info("adding segment to DataNode flow graph",
		zap.Int64("segment ID", req.GetSegmentId()),
		zap.Int64("collection ID", req.GetCollectionId()),
		zap.Int64("partition ID", req.GetPartitionId()),
		zap.String("channel name", req.GetChannelName()),
		zap.Int64("# of rows", req.GetRowNum()))
	// Fetch the flow graph on the given v-channel.
	var ds *dataSyncService
	// Retry in case the channel hasn't been watched yet.
	err := retry.Do(ctx, func() error {
		var ok bool
		ds, ok = node.flowgraphManager.getFlowgraphService(req.GetChannelName())
		if !ok {
			return errors.New("channel not found")
		}
		return nil
	}, retry.Attempts(getFlowGraphServiceAttempts))
	if err != nil {
		log.Error("channel not found in current DataNode",
			zap.String("channel name", req.GetChannelName()),
			zap.Int64("node ID", paramtable.GetNodeID()))
		return &datapb.AddImportSegmentResponse{
			Status: &commonpb.Status{
				// TODO: Add specific error code.
				ErrorCode: commonpb.ErrorCode_UnexpectedError,
				Reason:    "channel not found in current DataNode",
			},
		}, nil
	}
	// Get the current dml channel position ID, that will be used in segments start positions and end positions.
	posID, err := ds.getChannelLatestMsgID(context.Background(), req.GetChannelName(), req.GetSegmentId())
	if err != nil {
		return &datapb.AddImportSegmentResponse{
			Status: &commonpb.Status{
				// TODO: Add specific error code.
				ErrorCode: commonpb.ErrorCode_UnexpectedError,
				Reason:    "failed to get channel position",
			},
		}, nil
	}
	// Add the new segment to the channel.
	if !ds.channel.hasSegment(req.GetSegmentId(), true) {
		log.Info("adding a new segment to channel",
			zap.Int64("segment ID", req.GetSegmentId()))
		// Add segment as a flushed segment, but set `importing` to true to add extra information of the segment.
		// By 'extra information' we mean segment info while adding a `SegmentType_Flushed` typed segment.
		if err := ds.channel.addSegment(
			addSegmentReq{
				segType:      datapb.SegmentType_Flushed,
				segID:        req.GetSegmentId(),
				collID:       req.GetCollectionId(),
				partitionID:  req.GetPartitionId(),
				numOfRows:    req.GetRowNum(),
				statsBinLogs: req.GetStatsLog(),
				startPos: &internalpb.MsgPosition{
					ChannelName: req.GetChannelName(),
					MsgID:       posID,
					Timestamp:   req.GetBase().GetTimestamp(),
				},
				endPos: &internalpb.MsgPosition{
					ChannelName: req.GetChannelName(),
					MsgID:       posID,
					Timestamp:   req.GetBase().GetTimestamp(),
				},
				recoverTs: req.GetBase().GetTimestamp(),
				importing: true,
			}); err != nil {
			log.Error("failed to add segment to flow graph",
				zap.Error(err))
			return &datapb.AddImportSegmentResponse{
				Status: &commonpb.Status{
					// TODO: Add specific error code.
					ErrorCode: commonpb.ErrorCode_UnexpectedError,
					Reason:    err.Error(),
				},
			}, nil
		}
	}
	ds.flushingSegCache.Remove(req.GetSegmentId())
	return &datapb.AddImportSegmentResponse{
		Status: &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_Success,
		},
		ChannelPos: posID,
	}, nil
}

func assignSegmentFunc(node *DataNode, req *datapb.ImportTaskRequest) importutil.AssignSegmentFunc {
	return func(shardID int) (int64, string, error) {
		chNames := req.GetImportTask().GetChannelNames()
		importTaskID := req.GetImportTask().GetTaskId()
		if shardID >= len(chNames) {
			log.Error("import task returns invalid shard ID",
				zap.Int64("task ID", importTaskID),
				zap.Int("shard ID", shardID),
				zap.Int("# of channels", len(chNames)),
				zap.Strings("channel names", chNames),
			)
			return 0, "", fmt.Errorf("syncSegmentID Failed: invalid shard ID %d", shardID)
		}

		tr := timerecord.NewTimeRecorder("import callback function")
		defer tr.Elapse("finished")

		colID := req.GetImportTask().GetCollectionId()
		partID := req.GetImportTask().GetPartitionId()
		segmentIDReq := composeAssignSegmentIDRequest(1, shardID, chNames, colID, partID)
		targetChName := segmentIDReq.GetSegmentIDRequests()[0].GetChannelName()
		log.Info("target channel for the import task",
			zap.Int64("task ID", importTaskID),
			zap.Int("shard ID", shardID),
			zap.String("target channel name", targetChName))
		resp, err := node.dataCoord.AssignSegmentID(context.Background(), segmentIDReq)
		if err != nil {
			return 0, "", fmt.Errorf("syncSegmentID Failed:%w", err)
		}
		if resp.Status.ErrorCode != commonpb.ErrorCode_Success {
			return 0, "", fmt.Errorf("syncSegmentID Failed:%s", resp.Status.Reason)
		}
		segmentID := resp.SegIDAssignments[0].SegID
		log.Info("new segment assigned",
			zap.Int64("task ID", importTaskID),
			zap.Int64("segmentID", segmentID),
			zap.Int("shard ID", shardID),
			zap.String("target channel name", targetChName))
		return segmentID, targetChName, nil
	}
}

func createBinLogsFunc(node *DataNode, req *datapb.ImportTaskRequest, schema *schemapb.CollectionSchema, ts Timestamp) importutil.CreateBinlogsFunc {
	return func(fields map[storage.FieldID]storage.FieldData, segmentID int64) ([]*datapb.FieldBinlog, []*datapb.FieldBinlog, error) {
		var rowNum int
		for _, field := range fields {
			rowNum = field.RowNum()
			break
		}

		chNames := req.GetImportTask().GetChannelNames()
		importTaskID := req.GetImportTask().GetTaskId()
		if rowNum <= 0 {
			log.Info("fields data is empty, no need to generate binlog",
				zap.Int64("task ID", importTaskID),
				zap.Int("# of channels", len(chNames)),
				zap.Strings("channel names", chNames),
			)
			return nil, nil, nil
		}

		colID := req.GetImportTask().GetCollectionId()
		partID := req.GetImportTask().GetPartitionId()

		fieldInsert, fieldStats, err := createBinLogs(rowNum, schema, ts, fields, node, segmentID, colID, partID)
		if err != nil {
			log.Error("failed to create binlogs",
				zap.Int64("task ID", importTaskID),
				zap.Int("# of channels", len(chNames)),
				zap.Strings("channel names", chNames),
				zap.Any("err", err),
			)
			return nil, nil, err
		}

		log.Info("new binlog created",
			zap.Int64("task ID", importTaskID),
			zap.Int64("segmentID", segmentID),
			zap.Int("insert log count", len(fieldInsert)),
			zap.Int("stats log count", len(fieldStats)))

		return fieldInsert, fieldStats, err
	}
}

func saveSegmentFunc(node *DataNode, req *datapb.ImportTaskRequest, res *rootcoordpb.ImportResult, ts Timestamp) importutil.SaveSegmentFunc {
	importTaskID := req.GetImportTask().GetTaskId()
	return func(fieldsInsert []*datapb.FieldBinlog, fieldsStats []*datapb.FieldBinlog, segmentID int64, targetChName string, rowCount int64) error {
		log.Info("adding segment to the correct DataNode flow graph and saving binlog paths",
			zap.Int64("task ID", importTaskID),
			zap.Int64("segmentID", segmentID),
			zap.String("targetChName", targetChName),
			zap.Int64("rowCount", rowCount),
			zap.Uint64("ts", ts))

		err := retry.Do(context.Background(), func() error {
			// Ask DataCoord to save binlog path and add segment to the corresponding DataNode flow graph.
			resp, err := node.dataCoord.SaveImportSegment(context.Background(), &datapb.SaveImportSegmentRequest{
				Base: commonpbutil.NewMsgBase(
					commonpbutil.WithTimeStamp(ts), // Pass current timestamp downstream.
					commonpbutil.WithSourceID(paramtable.GetNodeID()),
				),
				SegmentId:    segmentID,
				ChannelName:  targetChName,
				CollectionId: req.GetImportTask().GetCollectionId(),
				PartitionId:  req.GetImportTask().GetPartitionId(),
				RowNum:       rowCount,
				SaveBinlogPathReq: &datapb.SaveBinlogPathsRequest{
					Base: commonpbutil.NewMsgBase(
						commonpbutil.WithMsgType(0),
						commonpbutil.WithMsgID(0),
						commonpbutil.WithTimeStamp(ts),
						commonpbutil.WithSourceID(paramtable.GetNodeID()),
					),
					SegmentID:           segmentID,
					CollectionID:        req.GetImportTask().GetCollectionId(),
					Field2BinlogPaths:   fieldsInsert,
					Field2StatslogPaths: fieldsStats,
					// Set start positions of a SaveBinlogPathRequest explicitly.
					StartPositions: []*datapb.SegmentStartPosition{
						{
							StartPosition: &internalpb.MsgPosition{
								ChannelName: targetChName,
								Timestamp:   ts,
							},
							SegmentID: segmentID,
						},
					},
					Importing: true,
				},
			})
			// Only retrying when DataCoord is unhealthy or err != nil, otherwise return immediately.
			if err != nil {
				return fmt.Errorf(err.Error())
			}
			if resp.ErrorCode != commonpb.ErrorCode_Success && resp.ErrorCode != commonpb.ErrorCode_DataCoordNA {
				return retry.Unrecoverable(fmt.Errorf("failed to save import segment, reason = %s", resp.Reason))
			} else if resp.ErrorCode == commonpb.ErrorCode_DataCoordNA {
				return fmt.Errorf("failed to save import segment: %s", resp.GetReason())
			}
			return nil
		})
		if err != nil {
			log.Warn("failed to save import segment", zap.Error(err))
			return err
		}
		log.Info("segment imported and persisted",
			zap.Int64("task ID", importTaskID),
			zap.Int64("segmentID", segmentID))
		res.Segments = append(res.Segments, segmentID)
		res.RowCount += rowCount
		return nil
	}
}

func composeAssignSegmentIDRequest(rowNum int, shardID int, chNames []string,
	collID int64, partID int64) *datapb.AssignSegmentIDRequest {
	// use the first field's row count as segment row count
	// all the fields row count are same, checked by ImportWrapper
	// ask DataCoord to alloc a new segment
	log.Info("import task flush segment",
		zap.Any("channel names", chNames),
		zap.Int("shard ID", shardID))
	segReqs := []*datapb.SegmentIDRequest{
		{
			ChannelName:  chNames[shardID],
			Count:        uint32(rowNum),
			CollectionID: collID,
			PartitionID:  partID,
			IsImport:     true,
		},
	}
	segmentIDReq := &datapb.AssignSegmentIDRequest{
		NodeID:            0,
		PeerRole:          typeutil.ProxyRole,
		SegmentIDRequests: segReqs,
	}
	return segmentIDReq
}

func createBinLogs(rowNum int, schema *schemapb.CollectionSchema, ts Timestamp,
	fields map[storage.FieldID]storage.FieldData, node *DataNode, segmentID, colID, partID UniqueID) ([]*datapb.FieldBinlog, []*datapb.FieldBinlog, error) {

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tsFieldData := make([]int64, rowNum)
	for i := range tsFieldData {
		tsFieldData[i] = int64(ts)
	}
	fields[common.TimeStampField] = &storage.Int64FieldData{
		Data:    tsFieldData,
		NumRows: []int64{int64(rowNum)},
	}

	if status, _ := node.dataCoord.UpdateSegmentStatistics(context.TODO(), &datapb.UpdateSegmentStatisticsRequest{
		Stats: []*datapb.SegmentStats{{
			SegmentID: segmentID,
			NumRows:   int64(rowNum),
		}},
	}); status.GetErrorCode() != commonpb.ErrorCode_Success {
		return nil, nil, fmt.Errorf(status.GetReason())
	}

	data := BufferData{buffer: &InsertData{
		Data: fields,
	}}
	data.updateSize(int64(rowNum))
	meta := &etcdpb.CollectionMeta{
		ID:     colID,
		Schema: schema,
	}
	binLogs, statsBinLogs, err := storage.NewInsertCodec(meta).Serialize(partID, segmentID, data.buffer)
	if err != nil {
		return nil, nil, err
	}

	var alloc allocatorInterface = newAllocator(node.rootCoord)
	start, _, err := alloc.allocIDBatch(uint32(len(binLogs)))
	if err != nil {
		return nil, nil, err
	}

	field2Insert := make(map[UniqueID]*datapb.Binlog, len(binLogs))
	kvs := make(map[string][]byte, len(binLogs))
	field2Logidx := make(map[UniqueID]UniqueID, len(binLogs))
	for idx, blob := range binLogs {
		fieldID, err := strconv.ParseInt(blob.GetKey(), 10, 64)
		if err != nil {
			log.Error("Flush failed ... cannot parse string to fieldID ..", zap.Error(err))
			return nil, nil, err
		}

		logidx := start + int64(idx)

		// no error raise if alloc=false
		k := metautil.JoinIDPath(colID, partID, segmentID, fieldID, logidx)

		key := path.Join(node.chunkManager.RootPath(), common.SegmentInsertLogPath, k)
		kvs[key] = blob.Value[:]
		field2Insert[fieldID] = &datapb.Binlog{
			EntriesNum:    data.size,
			TimestampFrom: ts,
			TimestampTo:   ts,
			LogPath:       key,
			LogSize:       int64(len(blob.Value)),
		}
		field2Logidx[fieldID] = logidx
	}

	field2Stats := make(map[UniqueID]*datapb.Binlog)
	// write stats binlog
	for _, blob := range statsBinLogs {
		fieldID, err := strconv.ParseInt(blob.GetKey(), 10, 64)
		if err != nil {
			log.Error("Flush failed ... cannot parse string to fieldID ..", zap.Error(err))
			return nil, nil, err
		}

		logidx := field2Logidx[fieldID]

		// no error raise if alloc=false
		k := metautil.JoinIDPath(colID, partID, segmentID, fieldID, logidx)

		key := path.Join(node.chunkManager.RootPath(), common.SegmentStatslogPath, k)
		kvs[key] = blob.Value
		field2Stats[fieldID] = &datapb.Binlog{
			EntriesNum:    data.size,
			TimestampFrom: ts,
			TimestampTo:   ts,
			LogPath:       key,
			LogSize:       int64(len(blob.Value)),
		}
	}

	err = node.chunkManager.MultiWrite(ctx, kvs)
	if err != nil {
		return nil, nil, err
	}
	var (
		fieldInsert []*datapb.FieldBinlog
		fieldStats  []*datapb.FieldBinlog
	)
	for k, v := range field2Insert {
		fieldInsert = append(fieldInsert, &datapb.FieldBinlog{FieldID: k, Binlogs: []*datapb.Binlog{v}})
	}
	for k, v := range field2Stats {
		fieldStats = append(fieldStats, &datapb.FieldBinlog{FieldID: k, Binlogs: []*datapb.Binlog{v}})
	}
	return fieldInsert, fieldStats, nil
}

func logDupFlush(cID, segID int64) {
	log.Info("segment is already being flushed, ignoring flush request",
		zap.Int64("collection ID", cID),
		zap.Int64("segment ID", segID))
}
