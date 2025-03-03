package rootcoord

import (
	"context"
	"errors"
	"fmt"

	"github.com/milvus-io/milvus/internal/metastore/model"
	"github.com/milvus-io/milvus/internal/util/typeutil"

	"github.com/milvus-io/milvus-proto/go-api/milvuspb"

	"github.com/milvus-io/milvus/internal/util/commonpbutil"

	"github.com/milvus-io/milvus/internal/proto/datapb"

	"github.com/milvus-io/milvus-proto/go-api/commonpb"
	"github.com/milvus-io/milvus-proto/go-api/schemapb"
	"github.com/milvus-io/milvus/internal/log"
	"github.com/milvus-io/milvus/internal/proto/querypb"
	"go.uber.org/zap"
)

type watchInfo struct {
	ts             Timestamp
	collectionID   UniqueID
	partitionID    UniqueID
	vChannels      []string
	startPositions []*commonpb.KeyDataPair
	schema         *schemapb.CollectionSchema
}

// Broker communicates with other components.
type Broker interface {
	ReleaseCollection(ctx context.Context, collectionID UniqueID) error
	GetQuerySegmentInfo(ctx context.Context, collectionID int64, segIDs []int64) (retResp *querypb.GetSegmentInfoResponse, retErr error)

	WatchChannels(ctx context.Context, info *watchInfo) error
	UnwatchChannels(ctx context.Context, info *watchInfo) error
	Flush(ctx context.Context, cID int64, segIDs []int64) error
	Import(ctx context.Context, req *datapb.ImportTaskRequest) (*datapb.ImportTaskResponse, error)
	UnsetIsImportingState(context.Context, *datapb.UnsetIsImportingStateRequest) (*commonpb.Status, error)
	MarkSegmentsDropped(context.Context, *datapb.MarkSegmentsDroppedRequest) (*commonpb.Status, error)
	GetSegmentStates(context.Context, *datapb.GetSegmentStatesRequest) (*datapb.GetSegmentStatesResponse, error)

	DropCollectionIndex(ctx context.Context, collID UniqueID, partIDs []UniqueID) error
	GetSegmentIndexState(ctx context.Context, collID UniqueID, indexName string, segIDs []UniqueID) ([]*datapb.SegmentIndexState, error)
	DescribeIndex(ctx context.Context, colID UniqueID) (*datapb.DescribeIndexResponse, error)

	BroadcastAlteredCollection(ctx context.Context, req *milvuspb.AlterCollectionRequest) error
}

type ServerBroker struct {
	s *Core
}

func newServerBroker(s *Core) *ServerBroker {
	return &ServerBroker{s: s}
}

func (b *ServerBroker) ReleaseCollection(ctx context.Context, collectionID UniqueID) error {
	log.Info("releasing collection", zap.Int64("collection", collectionID))

	resp, err := b.s.queryCoord.ReleaseCollection(ctx, &querypb.ReleaseCollectionRequest{
		Base:         commonpbutil.NewMsgBase(commonpbutil.WithMsgType(commonpb.MsgType_ReleaseCollection)),
		CollectionID: collectionID,
		NodeID:       b.s.session.ServerID,
	})
	if err != nil {
		return err
	}

	if resp.GetErrorCode() != commonpb.ErrorCode_Success {
		return fmt.Errorf("failed to release collection, code: %s, reason: %s", resp.GetErrorCode(), resp.GetReason())
	}

	log.Info("done to release collection", zap.Int64("collection", collectionID))
	return nil
}

func (b *ServerBroker) GetQuerySegmentInfo(ctx context.Context, collectionID int64, segIDs []int64) (retResp *querypb.GetSegmentInfoResponse, retErr error) {
	resp, err := b.s.queryCoord.GetSegmentInfo(ctx, &querypb.GetSegmentInfoRequest{
		Base: commonpbutil.NewMsgBase(
			commonpbutil.WithMsgType(commonpb.MsgType_GetSegmentState),
			commonpbutil.WithSourceID(b.s.session.ServerID),
		),
		CollectionID: collectionID,
		SegmentIDs:   segIDs,
	})
	return resp, err
}

func toKeyDataPairs(m map[string][]byte) []*commonpb.KeyDataPair {
	ret := make([]*commonpb.KeyDataPair, 0, len(m))
	for k, data := range m {
		ret = append(ret, &commonpb.KeyDataPair{
			Key:  k,
			Data: data,
		})
	}
	return ret
}

func (b *ServerBroker) WatchChannels(ctx context.Context, info *watchInfo) error {
	log.Info("watching channels", zap.Uint64("ts", info.ts), zap.Int64("collection", info.collectionID), zap.Strings("vChannels", info.vChannels))

	resp, err := b.s.dataCoord.WatchChannels(ctx, &datapb.WatchChannelsRequest{
		CollectionID:   info.collectionID,
		ChannelNames:   info.vChannels,
		StartPositions: info.startPositions,
		Schema:         info.schema,
	})
	if err != nil {
		return err
	}

	if resp.GetStatus().GetErrorCode() != commonpb.ErrorCode_Success {
		return fmt.Errorf("failed to watch channels, code: %s, reason: %s", resp.GetStatus().GetErrorCode(), resp.GetStatus().GetReason())
	}

	log.Info("done to watch channels", zap.Uint64("ts", info.ts), zap.Int64("collection", info.collectionID), zap.Strings("vChannels", info.vChannels))
	return nil
}

func (b *ServerBroker) UnwatchChannels(ctx context.Context, info *watchInfo) error {
	// TODO: release flowgraph on datanodes.
	return nil
}

func (b *ServerBroker) Flush(ctx context.Context, cID int64, segIDs []int64) error {
	resp, err := b.s.dataCoord.Flush(ctx, &datapb.FlushRequest{
		Base: commonpbutil.NewMsgBase(
			commonpbutil.WithMsgType(commonpb.MsgType_Flush),
			commonpbutil.WithSourceID(b.s.session.ServerID),
		),
		DbID:         0,
		SegmentIDs:   segIDs,
		CollectionID: cID,
	})
	if err != nil {
		return errors.New("failed to call flush to data coordinator: " + err.Error())
	}
	if resp.GetStatus().GetErrorCode() != commonpb.ErrorCode_Success {
		return errors.New(resp.Status.Reason)
	}
	log.Info("flush on collection succeed", zap.Int64("collection ID", cID))
	return nil
}

func (b *ServerBroker) Import(ctx context.Context, req *datapb.ImportTaskRequest) (*datapb.ImportTaskResponse, error) {
	return b.s.dataCoord.Import(ctx, req)
}

func (b *ServerBroker) UnsetIsImportingState(ctx context.Context, req *datapb.UnsetIsImportingStateRequest) (*commonpb.Status, error) {
	return b.s.dataCoord.UnsetIsImportingState(ctx, req)
}

func (b *ServerBroker) MarkSegmentsDropped(ctx context.Context, req *datapb.MarkSegmentsDroppedRequest) (*commonpb.Status, error) {
	return b.s.dataCoord.MarkSegmentsDropped(ctx, req)
}

func (b *ServerBroker) GetSegmentStates(ctx context.Context, req *datapb.GetSegmentStatesRequest) (*datapb.GetSegmentStatesResponse, error) {
	return b.s.dataCoord.GetSegmentStates(ctx, req)
}

func (b *ServerBroker) DropCollectionIndex(ctx context.Context, collID UniqueID, partIDs []UniqueID) error {
	rsp, err := b.s.dataCoord.DropIndex(ctx, &datapb.DropIndexRequest{
		CollectionID: collID,
		PartitionIDs: partIDs,
		IndexName:    "",
		DropAll:      true,
	})
	if err != nil {
		return err
	}
	if rsp.ErrorCode != commonpb.ErrorCode_Success {
		return fmt.Errorf(rsp.Reason)
	}
	return nil
}

func (b *ServerBroker) GetSegmentIndexState(ctx context.Context, collID UniqueID, indexName string, segIDs []UniqueID) ([]*datapb.SegmentIndexState, error) {
	resp, err := b.s.dataCoord.GetSegmentIndexState(ctx, &datapb.GetSegmentIndexStateRequest{
		CollectionID: collID,
		IndexName:    indexName,
		SegmentIDs:   segIDs,
	})
	if err != nil {
		return nil, err
	}
	if resp.Status.ErrorCode != commonpb.ErrorCode_Success {
		return nil, errors.New(resp.Status.Reason)
	}

	return resp.GetStates(), nil
}

func (b *ServerBroker) BroadcastAlteredCollection(ctx context.Context, req *milvuspb.AlterCollectionRequest) error {
	log.Info("broadcasting request to alter collection", zap.String("collection name", req.GetCollectionName()), zap.Int64("collection id", req.GetCollectionID()))

	colMeta, err := b.s.meta.GetCollectionByID(ctx, req.GetCollectionID(), typeutil.MaxTimestamp, false)
	if err != nil {
		return err
	}

	partitionIDs := make([]int64, len(colMeta.Partitions))
	for _, p := range colMeta.Partitions {
		partitionIDs = append(partitionIDs, p.PartitionID)
	}
	dcReq := &datapb.AlterCollectionRequest{
		CollectionID: req.GetCollectionID(),
		Schema: &schemapb.CollectionSchema{
			Name:        colMeta.Name,
			Description: colMeta.Description,
			AutoID:      colMeta.AutoID,
			Fields:      model.MarshalFieldModels(colMeta.Fields),
		},
		PartitionIDs:   partitionIDs,
		StartPositions: colMeta.StartPositions,
		Properties:     req.GetProperties(),
	}

	resp, err := b.s.dataCoord.BroadcastAlteredCollection(ctx, dcReq)
	if err != nil {
		return err
	}

	if resp.ErrorCode != commonpb.ErrorCode_Success {
		return errors.New(resp.Reason)
	}
	log.Info("done to broadcast request to alter collection", zap.String("collection name", req.GetCollectionName()), zap.Int64("collection id", req.GetCollectionID()))
	return nil
}

func (b *ServerBroker) DescribeIndex(ctx context.Context, colID UniqueID) (*datapb.DescribeIndexResponse, error) {
	return b.s.dataCoord.DescribeIndex(ctx, &datapb.DescribeIndexRequest{
		CollectionID: colID,
	})
}
