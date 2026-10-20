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
	"bytes"
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juicedata/juicefs/pkg/object"
)

func TestClusterFromKey(t *testing.T) {
	// plain key without cluster bits maps to cluster 0
	if got := clusterFromKey("chunks/0/0/1_0_1024"); got != 0 {
		t.Fatalf("plain key must map to cluster 0, got %d", got)
	}
	// a slice id with the cluster bits set (8+56 layout) routes to its cluster
	id := uint64(1)<<56 | 100
	key := fmt.Sprintf("chunks/%v/%v/%v_%v_%v", id/1000/1000, id/1000, id, 0, 1024)
	if got := clusterFromKey(key); got != 1 {
		t.Fatalf("composed key %s must map to cluster 1, got %d", key, got)
	}
	// hash-prefixed keys parse the same way
	hkey := fmt.Sprintf("chunks/%02X/%v/%v_%v_%v", id%256, id/1000/1000, id, 0, 1024)
	if got := clusterFromKey(hkey); got != 1 {
		t.Fatalf("hash-prefixed key %s must map to cluster 1, got %d", hkey, got)
	}
	if got := clusterFromKey("chunks/no/valid/block"); got != 0 {
		t.Fatalf("non-block key must fall back to cluster 0, got %d", got)
	}
}

// newClusterTestStore returns a store whose cluster 1 blocks land in a
// separate in-memory storage.
func newClusterTestStore(t *testing.T) (ChunkStore, *int32) {
	t.Helper()
	mem, err := object.CreateStorage("mem", "", "", "", "")
	if err != nil {
		t.Fatalf("create mem storage: %s", err)
	}
	var connected int32
	store := NewCachedStore(mem, Config{
		BlockSize:   1 << 20,
		MaxUpload:   1,
		MaxDownload: 10,
		PutTimeout:  time.Second,
		GetTimeout:  time.Second,
	}, nil)
	store.SetClusterResolver(func(cluster uint8) (object.ObjectStorage, error) {
		if cluster != 1 {
			t.Errorf("unexpected cluster %d", cluster)
		}
		atomic.AddInt32(&connected, 1)
		return object.CreateStorage("mem", "", "", "", "")
	})
	return store, &connected
}

func TestClusterRouting(t *testing.T) {
	store, connected := newClusterTestStore(t)
	ctx := context.Background()

	// the same bare slice id in cluster 0 and cluster 1 composes to two
	// different slice ids; data must land in separate storages
	const bare = uint64(100)
	id0 := bare                 // cluster 0
	id1 := uint64(1)<<56 | bare // cluster 1

	write := func(id uint64, fill byte) {
		w := store.NewWriter(id, 0)
		buf := bytes.Repeat([]byte{fill}, 4096)
		if _, err := w.WriteAt(buf, 0); err != nil {
			t.Fatalf("write slice %d: %s", id, err)
		}
		if err := w.Finish(4096); err != nil {
			t.Fatalf("finish slice %d: %s", id, err)
		}
	}
	write(id0, 'a')
	write(id1, 'b')
	if *connected != 1 {
		t.Fatalf("resolver should be called once, got %d", *connected)
	}

	read := func(id uint64, want byte) {
		p := NewOffPage(4096)
		defer p.Release()
		r := store.NewReader(id, 4096)
		if _, err := r.ReadAt(ctx, p, 0); err != nil {
			t.Fatalf("read slice %d: %s", id, err)
		}
		if !bytes.Equal(p.Data, bytes.Repeat([]byte{want}, 4096)) {
			t.Fatalf("slice %d returned wrong data", id)
		}
	}
	read(id0, 'a')
	read(id1, 'b')

	// repeated IO reuses the cached connection
	write(id1, 'c')
	if *connected != 1 {
		t.Fatalf("resolver should still be called once, got %d", *connected)
	}

	if err := store.Remove(id1, 4096); err != nil {
		t.Fatalf("remove cluster 1 slice: %s", err)
	}
}

func TestClusterRoutingWithoutResolver(t *testing.T) {
	mem, err := object.CreateStorage("mem", "", "", "", "")
	if err != nil {
		t.Fatalf("create mem storage: %s", err)
	}
	store := NewCachedStore(mem, Config{
		BlockSize:   1 << 20,
		MaxUpload:   1,
		MaxDownload: 10,
		MaxRetries:  1,
		PutTimeout:  time.Second,
		GetTimeout:  time.Second,
	}, nil)
	w := store.NewWriter(uint64(2)<<56|5, 0)
	buf := bytes.Repeat([]byte{0x1}, 1024)
	if _, werr := w.WriteAt(buf, 0); werr != nil {
		t.Fatalf("write: %s", werr)
	}
	// without a resolver the upload fails loudly instead of hitting cluster 0
	if err := w.Finish(1024); err == nil {
		t.Fatal("expected error when no resolver is registered for the cluster")
	}
}
