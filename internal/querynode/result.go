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

package querynode

import (
	"context"
	"fmt"
	"math"
	"strconv"

	"github.com/golang/protobuf/proto"
	"go.uber.org/zap"

	"github.com/milvus-io/milvus-proto/go-api/commonpb"
	"github.com/milvus-io/milvus-proto/go-api/schemapb"

	"github.com/milvus-io/milvus/internal/log"
	"github.com/milvus-io/milvus/internal/proto/internalpb"
	"github.com/milvus-io/milvus/internal/proto/segcorepb"
	"github.com/milvus-io/milvus/internal/util/funcutil"
	"github.com/milvus-io/milvus/internal/util/typeutil"
)

var _ typeutil.ResultWithID = &internalpb.RetrieveResults{}
var _ typeutil.ResultWithID = &segcorepb.RetrieveResults{}

func reduceStatisticResponse(results []*internalpb.GetStatisticsResponse) (*internalpb.GetStatisticsResponse, error) {
	mergedResults := map[string]interface{}{
		"row_count": int64(0),
	}
	fieldMethod := map[string]func(string) error{
		"row_count": func(str string) error {
			count, err := strconv.ParseInt(str, 10, 64)
			if err != nil {
				return err
			}
			mergedResults["row_count"] = mergedResults["row_count"].(int64) + count
			return nil
		},
	}

	for _, partialResult := range results {
		for _, pair := range partialResult.Stats {
			fn, ok := fieldMethod[pair.Key]
			if !ok {
				return nil, fmt.Errorf("unknown statistic field: %s", pair.Key)
			}
			if err := fn(pair.Value); err != nil {
				return nil, err
			}
		}
	}

	stringMap := make(map[string]string)
	for k, v := range mergedResults {
		stringMap[k] = fmt.Sprint(v)
	}

	ret := &internalpb.GetStatisticsResponse{
		Status: &commonpb.Status{ErrorCode: commonpb.ErrorCode_Success},
		Stats:  funcutil.Map2KeyValuePair(stringMap),
	}
	return ret, nil
}

func reduceSearchResults(ctx context.Context, results []*internalpb.SearchResults, nq int64, topk int64, metricType string) (*internalpb.SearchResults, error) {
	searchResultData, err := decodeSearchResults(results)
	if err != nil {
		log.Ctx(ctx).Warn("decode search results errors", zap.Error(err))
		return nil, err
	}
	log.Ctx(ctx).Debug("reduceSearchResultData",
		zap.Int("numbers", len(searchResultData)), zap.Int64("targetNq", nq), zap.Int64("targetTopk", topk))

	reducedResultData, err := reduceSearchResultData(ctx, searchResultData, nq, topk)
	if err != nil {
		log.Ctx(ctx).Warn("reduce search results error", zap.Error(err))
		return nil, err
	}
	searchResults, err := encodeSearchResultData(reducedResultData, nq, topk, metricType)
	if err != nil {
		log.Ctx(ctx).Warn("encode search results error", zap.Error(err))
		return nil, err
	}
	//if searchResults.SlicedBlob == nil {
	//	log.Debug("shard leader send nil results to proxy",
	//		zap.String("shard", q.channel))
	//} else {
	//	log.Debug("shard leader send non-nil results to proxy",
	//		zap.String("shard", q.channel))
	//}
	// printSearchResultData(reducedResultData, q.channel)

	return searchResults, nil
}

func reduceSearchResultData(ctx context.Context, searchResultData []*schemapb.SearchResultData, nq int64, topk int64) (*schemapb.SearchResultData, error) {
	if len(searchResultData) == 0 {
		return &schemapb.SearchResultData{
			NumQueries: nq,
			TopK:       topk,
			FieldsData: make([]*schemapb.FieldData, 0),
			Scores:     make([]float32, 0),
			Ids:        &schemapb.IDs{},
			Topks:      make([]int64, 0),
		}, nil
	}
	ret := &schemapb.SearchResultData{
		NumQueries: nq,
		TopK:       topk,
		FieldsData: make([]*schemapb.FieldData, len(searchResultData[0].FieldsData)),
		Scores:     make([]float32, 0),
		Ids:        &schemapb.IDs{},
		Topks:      make([]int64, 0),
	}

	resultOffsets := make([][]int64, len(searchResultData))
	for i := 0; i < len(searchResultData); i++ {
		resultOffsets[i] = make([]int64, len(searchResultData[i].Topks))
		for j := int64(1); j < nq; j++ {
			resultOffsets[i][j] = resultOffsets[i][j-1] + searchResultData[i].Topks[j-1]
		}
	}

	var skipDupCnt int64
	for i := int64(0); i < nq; i++ {
		offsets := make([]int64, len(searchResultData))

		var idSet = make(map[interface{}]struct{})
		var j int64
		for j = 0; j < topk; {
			sel := selectSearchResultData(searchResultData, resultOffsets, offsets, i)
			if sel == -1 {
				break
			}
			idx := resultOffsets[sel][i] + offsets[sel]

			id := typeutil.GetPK(searchResultData[sel].GetIds(), idx)
			score := searchResultData[sel].Scores[idx]

			// remove duplicates
			if _, ok := idSet[id]; !ok {
				typeutil.AppendFieldData(ret.FieldsData, searchResultData[sel].FieldsData, idx)
				typeutil.AppendPKs(ret.Ids, id)
				ret.Scores = append(ret.Scores, score)
				idSet[id] = struct{}{}
				j++
			} else {
				// skip entity with same id
				skipDupCnt++
			}
			offsets[sel]++
		}

		// if realTopK != -1 && realTopK != j {
		// 	log.Warn("Proxy Reduce Search Result", zap.Error(errors.New("the length (topk) between all result of query is different")))
		// 	// return nil, errors.New("the length (topk) between all result of query is different")
		// }
		ret.Topks = append(ret.Topks, j)
	}

	if skipDupCnt > 0 {
		log.Ctx(ctx).Debug("skip duplicated search result", zap.Int64("count", skipDupCnt))
	}
	return ret, nil
}

func selectSearchResultData(dataArray []*schemapb.SearchResultData, resultOffsets [][]int64, offsets []int64, qi int64) int {
	var (
		sel                 = -1
		maxDistance         = -1 * float32(math.MaxFloat32)
		resultDataIdx int64 = -1
	)
	for i, offset := range offsets { // query num, the number of ways to merge
		if offset >= dataArray[i].Topks[qi] {
			continue
		}

		idx := resultOffsets[i][qi] + offset
		distance := dataArray[i].Scores[idx]

		if distance > maxDistance {
			sel = i
			maxDistance = distance
			resultDataIdx = idx
		} else if distance == maxDistance {
			if sel == -1 {
				// A bad case happens where knowhere returns distance == +/-maxFloat32
				// by mistake.
				log.Error("a bad distance is found, something is wrong here!", zap.Float32("score", distance))
			} else if typeutil.ComparePK(
				typeutil.GetPK(dataArray[i].GetIds(), idx),
				typeutil.GetPK(dataArray[sel].GetIds(), resultDataIdx)) {
				sel = i
				maxDistance = distance
				resultDataIdx = idx
			}
		}
	}
	return sel
}

func decodeSearchResults(searchResults []*internalpb.SearchResults) ([]*schemapb.SearchResultData, error) {
	results := make([]*schemapb.SearchResultData, 0)
	for _, partialSearchResult := range searchResults {
		if partialSearchResult.SlicedBlob == nil {
			continue
		}

		var partialResultData schemapb.SearchResultData
		err := proto.Unmarshal(partialSearchResult.SlicedBlob, &partialResultData)
		if err != nil {
			return nil, err
		}

		results = append(results, &partialResultData)
	}
	return results, nil
}

func encodeSearchResultData(searchResultData *schemapb.SearchResultData, nq int64, topk int64, metricType string) (searchResults *internalpb.SearchResults, err error) {
	searchResults = &internalpb.SearchResults{
		Status: &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_Success,
		},
		NumQueries: nq,
		TopK:       topk,
		MetricType: metricType,
		SlicedBlob: nil,
	}
	slicedBlob, err := proto.Marshal(searchResultData)
	if err != nil {
		return nil, err
	}
	if searchResultData != nil && searchResultData.Ids != nil && typeutil.GetSizeOfIDs(searchResultData.Ids) != 0 {
		searchResults.SlicedBlob = slicedBlob
	}
	return
}

func mergeInternalRetrieveResult(ctx context.Context, retrieveResults []*internalpb.RetrieveResults, limit int64) (*internalpb.RetrieveResults, error) {
	log.Ctx(ctx).Debug("mergeInternelRetrieveResults",
		zap.Int64("limit", limit),
		zap.Int("len(retrieveResults)", len(retrieveResults)),
	)
	var (
		ret = &internalpb.RetrieveResults{
			Ids: &schemapb.IDs{},
		}
		skipDupCnt int64
		loopEnd    int
	)

	validRetrieveResults := []*internalpb.RetrieveResults{}
	for _, r := range retrieveResults {
		size := typeutil.GetSizeOfIDs(r.GetIds())
		if r == nil || len(r.GetFieldsData()) == 0 || size == 0 {
			continue
		}
		validRetrieveResults = append(validRetrieveResults, r)
		loopEnd += size
	}

	if len(validRetrieveResults) == 0 {
		return ret, nil
	}

	if limit != typeutil.Unlimited {
		loopEnd = int(limit)
	}

	ret.FieldsData = make([]*schemapb.FieldData, len(validRetrieveResults[0].GetFieldsData()))
	idTsMap := make(map[interface{}]uint64)
	cursors := make([]int64, len(validRetrieveResults))
	for j := 0; j < loopEnd; j++ {
		sel := typeutil.SelectMinPK(validRetrieveResults, cursors)
		if sel == -1 {
			break
		}

		pk := typeutil.GetPK(validRetrieveResults[sel].GetIds(), cursors[sel])
		ts := typeutil.GetTS(validRetrieveResults[sel], cursors[sel])
		if _, ok := idTsMap[pk]; !ok {
			typeutil.AppendPKs(ret.Ids, pk)
			typeutil.AppendFieldData(ret.FieldsData, validRetrieveResults[sel].GetFieldsData(), cursors[sel])
			idTsMap[pk] = ts
		} else {
			// primary keys duplicate
			skipDupCnt++
			if ts != 0 && ts > idTsMap[pk] {
				idTsMap[pk] = ts
				typeutil.DeleteFieldData(ret.FieldsData)
				typeutil.AppendFieldData(ret.FieldsData, validRetrieveResults[sel].GetFieldsData(), cursors[sel])
			}
		}
		cursors[sel]++
	}

	if skipDupCnt > 0 {
		log.Ctx(ctx).Debug("skip duplicated query result while reducing internal.RetrieveResults", zap.Int64("count", skipDupCnt))
	}

	return ret, nil
}

func mergeSegcoreRetrieveResults(ctx context.Context, retrieveResults []*segcorepb.RetrieveResults, limit int64) (*segcorepb.RetrieveResults, error) {
	log.Ctx(ctx).Debug("mergeSegcoreRetrieveResults",
		zap.Int64("limit", limit),
		zap.Int("len(retrieveResults)", len(retrieveResults)),
	)
	var (
		ret = &segcorepb.RetrieveResults{
			Ids: &schemapb.IDs{},
		}

		skipDupCnt int64
		loopEnd    int
	)

	validRetrieveResults := []*segcorepb.RetrieveResults{}
	for _, r := range retrieveResults {
		size := typeutil.GetSizeOfIDs(r.GetIds())
		if r == nil || len(r.GetOffset()) == 0 || size == 0 {
			continue
		}
		validRetrieveResults = append(validRetrieveResults, r)
		loopEnd += size
	}

	if len(validRetrieveResults) == 0 {
		return ret, nil
	}

	if limit != typeutil.Unlimited {
		loopEnd = int(limit)
	}

	ret.FieldsData = make([]*schemapb.FieldData, len(validRetrieveResults[0].GetFieldsData()))
	idSet := make(map[interface{}]struct{})
	cursors := make([]int64, len(validRetrieveResults))
	for j := 0; j < loopEnd; j++ {
		sel := typeutil.SelectMinPK(validRetrieveResults, cursors)
		if sel == -1 {
			break
		}

		pk := typeutil.GetPK(validRetrieveResults[sel].GetIds(), cursors[sel])
		if _, ok := idSet[pk]; !ok {
			typeutil.AppendPKs(ret.Ids, pk)
			typeutil.AppendFieldData(ret.FieldsData, validRetrieveResults[sel].GetFieldsData(), cursors[sel])
			idSet[pk] = struct{}{}
		} else {
			// primary keys duplicate
			skipDupCnt++
		}
		cursors[sel]++
	}

	if skipDupCnt > 0 {
		log.Ctx(ctx).Debug("skip duplicated query result while reducing segcore.RetrieveResults", zap.Int64("count", skipDupCnt))
	}

	return ret, nil
}

func mergeSegcoreRetrieveResultsAndFillIfEmpty(
	ctx context.Context,
	retrieveResults []*segcorepb.RetrieveResults,
	limit int64,
	outputFieldsID []int64,
	schema *schemapb.CollectionSchema,
) (*segcorepb.RetrieveResults, error) {

	mergedResult, err := mergeSegcoreRetrieveResults(ctx, retrieveResults, limit)
	if err != nil {
		return nil, err
	}

	if err := typeutil.FillRetrieveResultIfEmpty(typeutil.NewSegcoreResults(mergedResult), outputFieldsID, schema); err != nil {
		return nil, fmt.Errorf("failed to fill segcore retrieve results: %s", err.Error())
	}

	return mergedResult, nil
}

func mergeInternalRetrieveResultsAndFillIfEmpty(
	ctx context.Context,
	retrieveResults []*internalpb.RetrieveResults,
	limit int64,
	outputFieldsID []int64,
	schema *schemapb.CollectionSchema,
) (*internalpb.RetrieveResults, error) {

	mergedResult, err := mergeInternalRetrieveResult(ctx, retrieveResults, limit)
	if err != nil {
		return nil, err
	}

	if err := typeutil.FillRetrieveResultIfEmpty(typeutil.NewInternalResult(mergedResult), outputFieldsID, schema); err != nil {
		return nil, fmt.Errorf("failed to fill internal retrieve results: %s", err.Error())
	}

	return mergedResult, nil
}

// func printSearchResultData(data *schemapb.SearchResultData, header string) {
// 	size := len(data.Ids.GetIntId().Data)
// 	if size != len(data.Scores) {
// 		log.Error("SearchResultData length mis-match")
// 	}
// 	log.Debug("==== SearchResultData ====",
// 		zap.String("header", header), zap.Int64("nq", data.NumQueries), zap.Int64("topk", data.TopK))
// 	for i := 0; i < size; i++ {
// 		log.Debug("", zap.Int("i", i), zap.Int64("id", data.Ids.GetIntId().Data[i]), zap.Float32("score", data.Scores[i]))
// 	}
// }
