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

package proxy

import (
	"context"
	"fmt"
	"testing"

	"github.com/golang/protobuf/proto"
	"github.com/stretchr/testify/assert"
	"google.golang.org/grpc"

	"github.com/milvus-io/milvus-proto/go-api/commonpb"
	"github.com/milvus-io/milvus-proto/go-api/milvuspb"
	"github.com/milvus-io/milvus/internal/proto/internalpb"
)

type limiterMock struct {
	limit             bool
	rate              float64
	quotaStates       []milvuspb.QuotaState
	quotaStateReasons []string
}

func (l *limiterMock) Check(rt internalpb.RateType, n int) error {
	if l.rate == 0 {
		return ErrForceDeny
	}
	if l.limit {
		return ErrRateLimit
	}
	return nil
}

func (l *limiterMock) GetReadStateReason() string {
	for i := range l.quotaStates {
		if l.quotaStates[i] == milvuspb.QuotaState_DenyToRead {
			return l.quotaStateReasons[i]
		}
	}
	return ""
}

func (l *limiterMock) GetWriteStateReason() string {
	for i := range l.quotaStates {
		if l.quotaStates[i] == milvuspb.QuotaState_DenyToWrite {
			return l.quotaStateReasons[i]
		}
	}
	return ""
}

func TestRateLimitInterceptor(t *testing.T) {
	t.Run("test getRequestInfo", func(t *testing.T) {
		rt, size, err := getRequestInfo(&milvuspb.InsertRequest{})
		assert.NoError(t, err)
		assert.Equal(t, proto.Size(&milvuspb.InsertRequest{}), size)
		assert.Equal(t, internalpb.RateType_DMLInsert, rt)

		rt, size, err = getRequestInfo(&milvuspb.DeleteRequest{})
		assert.NoError(t, err)
		assert.Equal(t, proto.Size(&milvuspb.DeleteRequest{}), size)
		assert.Equal(t, internalpb.RateType_DMLDelete, rt)

		rt, size, err = getRequestInfo(&milvuspb.ImportRequest{})
		assert.NoError(t, err)
		assert.Equal(t, proto.Size(&milvuspb.ImportRequest{}), size)
		assert.Equal(t, internalpb.RateType_DMLBulkLoad, rt)

		rt, size, err = getRequestInfo(&milvuspb.SearchRequest{})
		assert.NoError(t, err)
		assert.Equal(t, proto.Size(&milvuspb.SearchRequest{}), size)
		assert.Equal(t, internalpb.RateType_DQLSearch, rt)

		rt, size, err = getRequestInfo(&milvuspb.QueryRequest{})
		assert.NoError(t, err)
		assert.Equal(t, 1, size)
		assert.Equal(t, internalpb.RateType_DQLQuery, rt)

		rt, size, err = getRequestInfo(&milvuspb.CreateCollectionRequest{})
		assert.NoError(t, err)
		assert.Equal(t, 1, size)
		assert.Equal(t, internalpb.RateType_DDLCollection, rt)

		rt, size, err = getRequestInfo(&milvuspb.CreatePartitionRequest{})
		assert.NoError(t, err)
		assert.Equal(t, 1, size)
		assert.Equal(t, internalpb.RateType_DDLPartition, rt)

		rt, size, err = getRequestInfo(&milvuspb.CreateIndexRequest{})
		assert.NoError(t, err)
		assert.Equal(t, 1, size)
		assert.Equal(t, internalpb.RateType_DDLIndex, rt)

		rt, size, err = getRequestInfo(&milvuspb.FlushRequest{})
		assert.NoError(t, err)
		assert.Equal(t, 1, size)
		assert.Equal(t, internalpb.RateType_DDLFlush, rt)

		rt, size, err = getRequestInfo(&milvuspb.ManualCompactionRequest{})
		assert.NoError(t, err)
		assert.Equal(t, 1, size)
		assert.Equal(t, internalpb.RateType_DDLCompaction, rt)
	})

	t.Run("test getFailedResponse", func(t *testing.T) {
		testGetFailedResponse := func(req interface{}) {
			rsp := getFailedResponse(req, commonpb.ErrorCode_UnexpectedError, "method", fmt.Errorf("mock err"))
			assert.NotNil(t, rsp)
		}

		testGetFailedResponse(&milvuspb.DeleteRequest{})
		testGetFailedResponse(&milvuspb.ImportRequest{})
		testGetFailedResponse(&milvuspb.SearchRequest{})
		testGetFailedResponse(&milvuspb.QueryRequest{})
		testGetFailedResponse(&milvuspb.CreateCollectionRequest{})
		testGetFailedResponse(&milvuspb.FlushRequest{})
		testGetFailedResponse(&milvuspb.ManualCompactionRequest{})

		// test illegal
		rsp := getFailedResponse(&milvuspb.SearchResults{}, commonpb.ErrorCode_UnexpectedError, "method", fmt.Errorf("mock err"))
		assert.Nil(t, rsp)
		rsp = getFailedResponse(nil, commonpb.ErrorCode_UnexpectedError, "method", fmt.Errorf("mock err"))
		assert.Nil(t, rsp)
	})

	t.Run("test RateLimitInterceptor", func(t *testing.T) {
		limiter := limiterMock{rate: 100}
		handler := func(ctx context.Context, req interface{}) (interface{}, error) {
			return &milvuspb.MutationResult{
				Status: &commonpb.Status{
					ErrorCode: commonpb.ErrorCode_Success,
				},
			}, nil
		}
		serverInfo := &grpc.UnaryServerInfo{FullMethod: "MockFullMethod"}

		limiter.limit = true
		interceptorFun := RateLimitInterceptor(&limiter)
		rsp, err := interceptorFun(context.Background(), &milvuspb.InsertRequest{}, serverInfo, handler)
		assert.Equal(t, commonpb.ErrorCode_RateLimit, rsp.(*milvuspb.MutationResult).GetStatus().GetErrorCode())
		assert.NoError(t, err)

		limiter.limit = false
		interceptorFun = RateLimitInterceptor(&limiter)
		rsp, err = interceptorFun(context.Background(), &milvuspb.InsertRequest{}, serverInfo, handler)
		assert.Equal(t, commonpb.ErrorCode_Success, rsp.(*milvuspb.MutationResult).GetStatus().GetErrorCode())
		assert.NoError(t, err)

		// test 0 rate, force deny
		limiter.rate = 0
		interceptorFun = RateLimitInterceptor(&limiter)
		rsp, err = interceptorFun(context.Background(), &milvuspb.InsertRequest{}, serverInfo, handler)
		assert.Equal(t, commonpb.ErrorCode_ForceDeny, rsp.(*milvuspb.MutationResult).GetStatus().GetErrorCode())
		assert.NoError(t, err)
	})
}
