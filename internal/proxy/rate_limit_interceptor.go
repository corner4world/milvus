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
	"errors"
	"fmt"
	"reflect"

	"github.com/golang/protobuf/proto"
	"google.golang.org/grpc"

	"github.com/milvus-io/milvus-proto/go-api/commonpb"
	"github.com/milvus-io/milvus-proto/go-api/milvuspb"
	"github.com/milvus-io/milvus/internal/proto/internalpb"
	"github.com/milvus-io/milvus/internal/types"
)

// RateLimitInterceptor returns a new unary server interceptors that performs request rate limiting.
func RateLimitInterceptor(limiter types.Limiter) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		rt, n, err := getRequestInfo(req)
		if err != nil {
			return handler(ctx, req)
		}
		err = limiter.Check(rt, n)
		if errors.Is(err, ErrForceDeny) {
			rsp := getFailedResponse(req, commonpb.ErrorCode_ForceDeny, info.FullMethod, err)
			if rsp != nil {
				return rsp, nil
			}
		}
		if errors.Is(err, ErrRateLimit) {
			rsp := getFailedResponse(req, commonpb.ErrorCode_RateLimit, info.FullMethod, err)
			if rsp != nil {
				return rsp, nil
			}
		}
		return handler(ctx, req)
	}
}

// getRequestInfo returns rateType of request and return tokens needed.
func getRequestInfo(req interface{}) (internalpb.RateType, int, error) {
	switch r := req.(type) {
	case *milvuspb.InsertRequest:
		return internalpb.RateType_DMLInsert, proto.Size(r), nil
	case *milvuspb.DeleteRequest:
		return internalpb.RateType_DMLDelete, proto.Size(r), nil
	case *milvuspb.ImportRequest:
		return internalpb.RateType_DMLBulkLoad, proto.Size(r), nil
	case *milvuspb.SearchRequest:
		return internalpb.RateType_DQLSearch, int(r.GetNq()), nil
	case *milvuspb.QueryRequest:
		return internalpb.RateType_DQLQuery, 1, nil // think of the query request's nq as 1
	case *milvuspb.CreateCollectionRequest, *milvuspb.DropCollectionRequest:
		return internalpb.RateType_DDLCollection, 1, nil
	case *milvuspb.LoadCollectionRequest, *milvuspb.ReleaseCollectionRequest:
		return internalpb.RateType_DDLCollection, 1, nil
	case *milvuspb.CreatePartitionRequest, *milvuspb.DropPartitionRequest:
		return internalpb.RateType_DDLPartition, 1, nil
	case *milvuspb.LoadPartitionsRequest, *milvuspb.ReleasePartitionsRequest:
		return internalpb.RateType_DDLPartition, 1, nil
	case *milvuspb.CreateIndexRequest, *milvuspb.DropIndexRequest:
		return internalpb.RateType_DDLIndex, 1, nil
	case *milvuspb.FlushRequest:
		return internalpb.RateType_DDLFlush, 1, nil
	case *milvuspb.ManualCompactionRequest:
		return internalpb.RateType_DDLCompaction, 1, nil
		// TODO: support more request
	default:
		if req == nil {
			return 0, 0, fmt.Errorf("null request")
		}
		return 0, 0, fmt.Errorf("unsupported request type %s", reflect.TypeOf(req).Name())
	}
}

// failedStatus returns failed status.
func failedStatus(code commonpb.ErrorCode, reason string) *commonpb.Status {
	return &commonpb.Status{
		ErrorCode: code,
		Reason:    reason,
	}
}

// failedMutationResult returns failed mutation result.
func failedMutationResult(code commonpb.ErrorCode, reason string) *milvuspb.MutationResult {
	return &milvuspb.MutationResult{
		Status: failedStatus(code, reason),
	}
}

// failedBoolResponse returns failed boolean response.
func failedBoolResponse(code commonpb.ErrorCode, reason string) *milvuspb.BoolResponse {
	return &milvuspb.BoolResponse{
		Status: failedStatus(code, reason),
	}
}

// getFailedResponse returns failed response.
func getFailedResponse(req interface{}, code commonpb.ErrorCode, fullMethod string, err error) interface{} {
	reason := fmt.Sprintf("%s, req: %s", err, fullMethod)
	switch req.(type) {
	case *milvuspb.InsertRequest, *milvuspb.DeleteRequest:
		return failedMutationResult(code, reason)
	case *milvuspb.ImportRequest:
		return &milvuspb.ImportResponse{
			Status: failedStatus(code, reason),
		}
	case *milvuspb.SearchRequest:
		return &milvuspb.SearchResults{
			Status: failedStatus(code, reason),
		}
	case *milvuspb.QueryRequest:
		return &milvuspb.QueryResults{
			Status: failedStatus(code, reason),
		}
	case *milvuspb.CreateCollectionRequest, *milvuspb.DropCollectionRequest,
		*milvuspb.LoadCollectionRequest, *milvuspb.ReleaseCollectionRequest,
		*milvuspb.CreatePartitionRequest, *milvuspb.DropPartitionRequest,
		*milvuspb.LoadPartitionsRequest, *milvuspb.ReleasePartitionsRequest,
		*milvuspb.CreateIndexRequest, *milvuspb.DropIndexRequest:
		return failedStatus(code, reason)
	case *milvuspb.FlushRequest:
		return &milvuspb.FlushResponse{
			Status: failedStatus(code, reason),
		}
	case *milvuspb.ManualCompactionRequest:
		return &milvuspb.ManualCompactionResponse{
			Status: failedStatus(code, reason),
		}
	}
	return nil
}
