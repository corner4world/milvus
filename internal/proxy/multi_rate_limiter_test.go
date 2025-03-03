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
	"errors"
	"fmt"
	"math"
	"testing"

	"github.com/milvus-io/milvus-proto/go-api/milvuspb"
	"github.com/milvus-io/milvus/internal/proto/internalpb"
	"github.com/milvus-io/milvus/internal/util/paramtable"
	"github.com/milvus-io/milvus/internal/util/ratelimitutil"
	"github.com/stretchr/testify/assert"
)

func TestMultiRateLimiter(t *testing.T) {
	t.Run("test multiRateLimiter", func(t *testing.T) {
		bak := Params.QuotaConfig.QuotaAndLimitsEnabled
		paramtable.Get().Save(Params.QuotaConfig.QuotaAndLimitsEnabled.Key, "true")
		multiLimiter := NewMultiRateLimiter()
		for _, rt := range internalpb.RateType_value {
			multiLimiter.globalRateLimiter.limiters[internalpb.RateType(rt)] = ratelimitutil.NewLimiter(ratelimitutil.Limit(1000), 1)
		}
		for _, rt := range internalpb.RateType_value {
			err := multiLimiter.Check(internalpb.RateType(rt), 1)
			assert.NoError(t, err)
			err = multiLimiter.Check(internalpb.RateType(rt), math.MaxInt)
			assert.NoError(t, err)
			err = multiLimiter.Check(internalpb.RateType(rt), math.MaxInt)
			assert.True(t, errors.Is(err, ErrRateLimit))
		}
		Params.QuotaConfig.QuotaAndLimitsEnabled = bak
	})

	t.Run("not enable quotaAndLimit", func(t *testing.T) {
		multiLimiter := NewMultiRateLimiter()
		bak := Params.QuotaConfig.QuotaAndLimitsEnabled
		paramtable.Get().Save(Params.QuotaConfig.QuotaAndLimitsEnabled.Key, "false")
		for _, rt := range internalpb.RateType_value {
			err := multiLimiter.Check(internalpb.RateType(rt), 1)
			assert.NoError(t, err)
		}
		Params.QuotaConfig.QuotaAndLimitsEnabled = bak
	})

	t.Run("test limit", func(t *testing.T) {
		run := func(insertRate float64) {
			bakInsertRate := Params.QuotaConfig.DMLMaxInsertRate
			paramtable.Get().Save(Params.QuotaConfig.DMLMaxInsertRate.Key, fmt.Sprintf("%f", insertRate))
			multiLimiter := NewMultiRateLimiter()
			bak := Params.QuotaConfig.QuotaAndLimitsEnabled
			paramtable.Get().Save(Params.QuotaConfig.QuotaAndLimitsEnabled.Key, "true")
			err := multiLimiter.Check(internalpb.RateType_DMLInsert, 1*1024*1024)
			assert.NoError(t, err)
			Params.QuotaConfig.QuotaAndLimitsEnabled = bak
			Params.QuotaConfig.DMLMaxInsertRate = bakInsertRate
		}
		run(math.MaxFloat64)
		run(math.MaxFloat64 / 1.2)
		run(math.MaxFloat64 / 2)
		run(math.MaxFloat64 / 3)
		run(math.MaxFloat64 / 10000)
	})

	t.Run("test GetReadStateReason and GetWriteStateReason", func(t *testing.T) {
		multiLimiter := NewMultiRateLimiter()
		states := []milvuspb.QuotaState{milvuspb.QuotaState_DenyToWrite, milvuspb.QuotaState_DenyToRead}
		writeReason := "memory quota exhausted"
		readReason := "manually deny to read"
		reasons := []string{writeReason, readReason}
		multiLimiter.SetQuotaStates(states, reasons)
		assert.Equal(t, writeReason, multiLimiter.GetWriteStateReason())
		assert.Equal(t, readReason, multiLimiter.GetReadStateReason())
	})
}

func TestRateLimiter(t *testing.T) {
	t.Run("test limit", func(t *testing.T) {
		limiter := newRateLimiter()
		limiter.registerLimiters()
		for _, rt := range internalpb.RateType_value {
			limiter.limiters[internalpb.RateType(rt)] = ratelimitutil.NewLimiter(ratelimitutil.Limit(1000), 1)
		}
		for _, rt := range internalpb.RateType_value {
			ok, _ := limiter.limit(internalpb.RateType(rt), 1)
			assert.False(t, ok)
			ok, _ = limiter.limit(internalpb.RateType(rt), math.MaxInt)
			assert.False(t, ok)
			ok, _ = limiter.limit(internalpb.RateType(rt), math.MaxInt)
			assert.True(t, ok)
		}
	})

	t.Run("test setRates", func(t *testing.T) {
		limiter := newRateLimiter()
		for _, rt := range internalpb.RateType_value {
			limiter.limiters[internalpb.RateType(rt)] = ratelimitutil.NewLimiter(ratelimitutil.Limit(1000), 1)
		}

		zeroRates := make([]*internalpb.Rate, 0, len(internalpb.RateType_value))
		for _, rt := range internalpb.RateType_value {
			zeroRates = append(zeroRates, &internalpb.Rate{
				Rt: internalpb.RateType(rt), R: 0,
			})
		}
		err := limiter.setRates(zeroRates)
		assert.NoError(t, err)
		for _, rt := range internalpb.RateType_value {
			for i := 0; i < 100; i++ {
				ok, _ := limiter.limit(internalpb.RateType(rt), 1)
				assert.True(t, ok)
			}
		}
	})
}
