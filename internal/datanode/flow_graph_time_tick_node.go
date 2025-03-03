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

package datanode

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"go.uber.org/zap"

	"github.com/milvus-io/milvus/internal/log"
	"github.com/milvus-io/milvus/internal/proto/datapb"
	"github.com/milvus-io/milvus/internal/proto/internalpb"
	"github.com/milvus-io/milvus/internal/types"
	"github.com/milvus-io/milvus/internal/util/commonpbutil"
	"github.com/milvus-io/milvus/internal/util/flowgraph"
	"github.com/milvus-io/milvus/internal/util/funcutil"
	"github.com/milvus-io/milvus/internal/util/paramtable"
	"github.com/milvus-io/milvus/internal/util/tsoutil"
)

const (
	updateChanCPInterval = 1 * time.Minute
	updateChanCPTimeout  = 10 * time.Second
)

// make sure ttNode implements flowgraph.Node
var _ flowgraph.Node = (*ttNode)(nil)

type ttNode struct {
	BaseNode
	vChannelName   string
	channel        Channel
	lastUpdateTime time.Time
	dataCoord      types.DataCoord
}

// Name returns node name, implementing flowgraph.Node
func (ttn *ttNode) Name() string {
	return fmt.Sprintf("ttNode-%s", ttn.vChannelName)
}

// Operate handles input messages, implementing flowgraph.Node
func (ttn *ttNode) Operate(in []Msg) []Msg {
	if in == nil {
		log.Warn("type assertion failed for flowGraphMsg because it's nil")
		return []Msg{}
	}

	if len(in) != 1 {
		log.Warn("Invalid operate message input in ttNode", zap.Int("input length", len(in)))
		return []Msg{}
	}

	fgMsg, ok := in[0].(*flowGraphMsg)
	if !ok {
		log.Warn("type assertion failed for flowGraphMsg", zap.String("name", reflect.TypeOf(in[0]).Name()))
		return []Msg{}
	}

	curTs, _ := tsoutil.ParseTS(fgMsg.timeRange.timestampMax)
	if curTs.Sub(ttn.lastUpdateTime) >= updateChanCPInterval {
		ttn.updateChannelCP(fgMsg.endPositions[0])
		ttn.lastUpdateTime = curTs
	}

	return []Msg{}
}

func (ttn *ttNode) updateChannelCP(ttPos *internalpb.MsgPosition) {
	channelPos := ttn.channel.getChannelCheckpoint(ttPos)
	if channelPos == nil || channelPos.MsgID == nil {
		log.Warn("updateChannelCP failed, get nil check point", zap.String("vChannel", ttn.vChannelName))
		return
	}
	channelCPTs, _ := tsoutil.ParseTS(channelPos.Timestamp)

	ctx, cancel := context.WithTimeout(context.Background(), updateChanCPTimeout)
	defer cancel()
	resp, err := ttn.dataCoord.UpdateChannelCheckpoint(ctx, &datapb.UpdateChannelCheckpointRequest{
		Base: commonpbutil.NewMsgBase(
			commonpbutil.WithSourceID(paramtable.GetNodeID()),
		),
		VChannel: ttn.vChannelName,
		Position: channelPos,
	})
	if err = funcutil.VerifyResponse(resp, err); err != nil {
		log.Warn("UpdateChannelCheckpoint failed", zap.String("channel", ttn.vChannelName),
			zap.Time("channelCPTs", channelCPTs), zap.Error(err))
		return
	}

	log.Info("UpdateChannelCheckpoint success", zap.String("channel", ttn.vChannelName), zap.Time("channelCPTs", channelCPTs))
}

func newTTNode(config *nodeConfig, dc types.DataCoord) (*ttNode, error) {
	baseNode := BaseNode{}
	baseNode.SetMaxQueueLength(Params.DataNodeCfg.FlowGraphMaxQueueLength.GetAsInt32())
	baseNode.SetMaxParallelism(Params.DataNodeCfg.FlowGraphMaxParallelism.GetAsInt32())

	tt := &ttNode{
		BaseNode:       baseNode,
		vChannelName:   config.vChannelName,
		channel:        config.channel,
		lastUpdateTime: time.Time{}, // set to Zero to update channel checkpoint immediately after fg started
		dataCoord:      dc,
	}

	return tt, nil
}
