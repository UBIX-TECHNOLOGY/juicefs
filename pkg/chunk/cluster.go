/*
 * JuiceFS, Copyright 2025 Juicedata, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package chunk

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/juicedata/juicefs/pkg/object"
)

// Routing relies on the slice id itself: KV engines compose it as 8+56, so
// the top 8 bits carry its owning object sub-cluster (see pkg/meta/tkv.go).
// Active paths route directly on the slice id (zero string parsing); replay
// paths that only hold a key recover the same id from the block key, which
// looks like "chunks/{a}/{b}/{sliceID}_{indx}_{size}". Ids without cluster
// bits (raw counters, non-KV engines) belong to the implicit cluster 0,
// i.e. the top-level storage.

// clusterFromSliceID extracts the owning object sub-cluster from a slice id.
func clusterFromSliceID(id uint64) uint8 { return uint8(id >> 56) }

// clusterFromKey recovers the slice id embedded in the given block key and
// returns its cluster. It is used only on replay paths that hold a key but
// no slice id (staging replay, prefetch). It returns 0 for keys that do not
// follow the block key layout (e.g. non-block keys), routing them to the
// top-level storage.
func clusterFromKey(key string) uint8 {
	i := strings.LastIndexByte(key, '/')
	tail := key[i+1:]
	if j := strings.IndexByte(tail, '_'); j > 0 {
		if id, err := strconv.ParseUint(tail[:j], 10, 64); err == nil {
			return clusterFromSliceID(id)
		}
	}
	return 0
}

// SetClusterResolver wires the lazy connector for extra object storage
// sub-clusters. Blocks whose slice id points to cluster >= 1 are routed
// to the storage returned by the resolver; connections are established
// on first use and cached. Clients without a resolver (gateway, gc, ...)
// fail such IO with a clear error instead of silently touching the wrong
// bucket.
func (store *cachedStore) SetClusterResolver(resolver func(cluster uint8) (object.ObjectStorage, error)) {
	store.clusterMu.Lock()
	defer store.clusterMu.Unlock()
	store.resolver = resolver
}

// storageForBlock selects the object storage that owns the given block.
// Callers with the full slice id pass it and route directly on its top 8
// bits (no string parsing); replay paths that only hold a key pass 0 and
// routing falls back to recovering the id from the key.
func (store *cachedStore) storageForBlock(id uint64, key string) object.ObjectStorage {
	if id == 0 {
		return store.storageForKey(key)
	}
	cluster := clusterFromSliceID(id)
	if cluster == 0 {
		return store.storage
	}
	return store.clusterStorage(cluster)
}

// storageForKey selects the object storage that owns the given key.
func (store *cachedStore) storageForKey(key string) object.ObjectStorage {
	cluster := clusterFromKey(key)
	if cluster == 0 {
		return store.storage
	}
	return store.clusterStorage(cluster)
}

func (store *cachedStore) clusterStorage(cluster uint8) object.ObjectStorage {
	store.clusterMu.Lock()
	defer store.clusterMu.Unlock()
	if store.resolver == nil {
		return failedStorage{fmt.Errorf("object cluster %d is not supported by this client", cluster)}
	}
	if st, ok := store.clusterStorages[cluster]; ok {
		return st
	}
	st, err := store.resolver(cluster)
	if err != nil {
		// not cached, so a later registration of the cluster can recover
		logger.Errorf("connect object storage cluster %d: %s", cluster, err)
		return failedStorage{err}
	}
	if store.clusterStorages == nil {
		store.clusterStorages = make(map[uint8]object.ObjectStorage)
	}
	store.clusterStorages[cluster] = st
	return st
}

// failedStorage errors on every operation, keeping routing failures
// observable at the actual IO site instead of panicking on a nil storage.
type failedStorage struct{ err error }

func (s failedStorage) String() string { return fmt.Sprintf("failed(%s)", s.err) }
func (s failedStorage) Limits() object.Limits {
	return object.Limits{IsSupportMultipartUpload: false, IsSupportUploadPartCopy: false}
}
func (s failedStorage) Create(ctx context.Context) error { return s.err }
func (s failedStorage) Get(ctx context.Context, key string, off, limit int64, getters ...object.AttrGetter) (io.ReadCloser, error) {
	return nil, s.err
}
func (s failedStorage) Put(ctx context.Context, key string, in io.Reader, getters ...object.AttrGetter) error {
	return s.err
}
func (s failedStorage) Copy(ctx context.Context, dst, src string) error { return s.err }
func (s failedStorage) Delete(ctx context.Context, key string, getters ...object.AttrGetter) error {
	return s.err
}
func (s failedStorage) Head(ctx context.Context, key string) (object.Object, error) {
	return nil, s.err
}
func (s failedStorage) List(ctx context.Context, prefix, startAfter, token, delimiter string, limit int64, followLink bool) ([]object.Object, bool, string, error) {
	return nil, false, "", s.err
}
func (s failedStorage) ListAll(ctx context.Context, prefix, marker string, followLink bool) (<-chan object.Object, error) {
	return nil, s.err
}
func (s failedStorage) CreateMultipartUpload(ctx context.Context, key string) (*object.MultipartUpload, error) {
	return nil, s.err
}
func (s failedStorage) UploadPart(ctx context.Context, key string, uploadID string, num int, body []byte) (*object.Part, error) {
	return nil, s.err
}
func (s failedStorage) UploadPartCopy(ctx context.Context, key string, uploadID string, num int, srcKey string, off, size int64) (*object.Part, error) {
	return nil, s.err
}
func (s failedStorage) AbortUpload(ctx context.Context, key string, uploadID string) {}
func (s failedStorage) CompleteUpload(ctx context.Context, key string, uploadID string, parts []*object.Part) error {
	return s.err
}
func (s failedStorage) ListUploads(ctx context.Context, marker string) ([]*object.PendingPart, string, error) {
	return nil, "", s.err
}
func (s failedStorage) Restore(ctx context.Context, key string, days int32) error { return s.err }

var _ object.ObjectStorage = failedStorage{}
