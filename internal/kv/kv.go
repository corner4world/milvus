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

package kv

import (
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/milvus-io/milvus/internal/util/typeutil"
)

// CompareFailedError is a helper type for checking MetaKv CompareAndSwap series func error type
type CompareFailedError struct {
	internalError error
}

// Error implements error interface
func (e *CompareFailedError) Error() string {
	return e.internalError.Error()
}

// NewCompareFailedError wraps error into NewCompareFailedError
func NewCompareFailedError(err error) error {
	return &CompareFailedError{internalError: err}
}

// BaseKV contains base operations of kv. Include save, load and remove.
type BaseKV interface {
	Load(key string) (string, error)
	MultiLoad(keys []string) ([]string, error)
	LoadWithPrefix(key string) ([]string, []string, error)
	Save(key, value string) error
	MultiSave(kvs map[string]string) error
	Remove(key string) error
	MultiRemove(keys []string) error
	RemoveWithPrefix(key string) error
	Close()
}

//go:generate mockery --name=TxnKV --with-expecter
// TxnKV contains extra txn operations of kv. The extra operations is transactional.
type TxnKV interface {
	BaseKV
	MultiSaveAndRemove(saves map[string]string, removals []string) error
	MultiRemoveWithPrefix(keys []string) error
	MultiSaveAndRemoveWithPrefix(saves map[string]string, removals []string) error
}

//go:generate mockery --name=MetaKv --with-expecter
// MetaKv is TxnKV for metadata. It should save data with lease.
type MetaKv interface {
	TxnKV
	GetPath(key string) string
	LoadWithPrefix(key string) ([]string, []string, error)
	LoadWithPrefix2(key string) ([]string, []string, []int64, error)
	LoadWithRevisionAndVersions(key string) ([]string, []string, []int64, int64, error)
	LoadWithRevision(key string) ([]string, []string, int64, error)
	Watch(key string) clientv3.WatchChan
	WatchWithPrefix(key string) clientv3.WatchChan
	WatchWithRevision(key string, revision int64) clientv3.WatchChan
	SaveWithLease(key, value string, id clientv3.LeaseID) error
	SaveWithIgnoreLease(key, value string) error
	Grant(ttl int64) (id clientv3.LeaseID, err error)
	KeepAlive(id clientv3.LeaseID) (<-chan *clientv3.LeaseKeepAliveResponse, error)
	CompareValueAndSwap(key, value, target string, opts ...clientv3.OpOption) (bool, error)
	CompareVersionAndSwap(key string, version int64, target string, opts ...clientv3.OpOption) (bool, error)
	WalkWithPrefix(prefix string, paginationSize int, fn func([]byte, []byte) error) error
}

//go:generate mockery --name=SnapShotKV --with-expecter
// SnapShotKV is TxnKV for snapshot data. It must save timestamp.
type SnapShotKV interface {
	Save(key string, value string, ts typeutil.Timestamp) error
	Load(key string, ts typeutil.Timestamp) (string, error)
	MultiSave(kvs map[string]string, ts typeutil.Timestamp) error
	LoadWithPrefix(key string, ts typeutil.Timestamp) ([]string, []string, error)
	MultiSaveAndRemoveWithPrefix(saves map[string]string, removals []string, ts typeutil.Timestamp) error
}
