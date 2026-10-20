/*
 * JuiceFS, Copyright 2026 Juicedata, Inc.
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

package meta

import (
	"encoding/binary"
	"syscall"
)

// Ino128 is the full 128-bit inode (KV engines only), laid out as
//
//	127           96 95           64 63                  0
//	┌────────────────┬───────────────┬────────────────────┐
//	│ gen / reserved │ object_ID     │ inode ID           │
//	│  32 bits       │  32 bits      │  64 bits           │
//	└────────────────┴───────────────┴────────────────────┘
//
// The 64-bit inode flowing through the meta / vfs / fuse interfaces is
// the COMPRESSED form: fuse_ino = Ino128.ID (the globally unique counter
// value), see docs/zh_cn/design/inode-layout-32-32-64.md. The full
// Ino128 is recoverable through the in-memory cache and the persistent
// "FI" reverse index kept by kvMeta.
type Ino128 struct {
	Gen      uint32 // bit 96~127; reserved for snapshot/clone, always 0 for now
	ObjectID uint32 // bit 64~95; object storage sub-cluster owning new data
	ID       uint64 // bit 0~63; globally unique, monotonic, never reused
}

const (
	// maxFuseInoID caps the 64-bit inode ID counter just below the vfs
	// internal node band (pkg/vfs/internal.go minInternalNode =
	// 0x7FFFFFFF00000000, trash = TrashInode + n): the compressed fuse
	// inode number must never collide with internal / trash nodes.
	maxFuseInoID = uint64(0x7FFFFFFEFFFFFFFF)

	// ReservedObjectIDBase starts the reserved object-ID segment used for
	// special objects; normal clusters must stay below it.
	ReservedObjectIDBase = uint32(0xFFFFFF00)
	// ReservedTrashObjectID marks trash inodes inside the 128-bit space,
	// replacing the legacy numeric-range check (see design doc §4.3).
	ReservedTrashObjectID = uint32(0xFFFFFF00)
)

// composeIno128 combines the generation, object cluster id and the 64-bit
// inode id into a full 128-bit inode. The id must fall inside the usable
// fuse range; the counter never carries into or crosses the internal node
// isolation band.
func composeIno128(gen, objectID uint32, id uint64) (Ino128, error) {
	if id == 0 || id > maxFuseInoID {
		return Ino128{}, syscall.ENOSPC
	}
	return Ino128{Gen: gen, ObjectID: objectID, ID: id}, nil
}

// trashIno128 derives the 128-bit form of a trash inode: the full legacy
// inode value (TrashInode + seq) IS the globally unique 64-bit id, and the
// reserved trash object id tags it inside the 128-bit space, so its key
// encoding never collides with normal inodes' ids.
func trashIno128(seq uint64) Ino128 {
	return Ino128{ObjectID: ReservedTrashObjectID, ID: uint64(TrashInode) + seq}
}

func (i Ino128) IsTrash() bool { return i.ObjectID >= ReservedTrashObjectID }

// IsNormal reports whether this is an ordinary (non-trash) allocated inode.
func (i Ino128) IsNormal() bool {
	return i.ID >= uint64(RootInode) && i.ID <= maxFuseInoID && !i.IsTrash()
}

// Ino returns the compressed 64-bit fuse inode number. For normal inodes
// it equals the globally unique inode ID; for trash inodes the compressed
// value is the legacy TrashInode + seq number, so callers holding a trash
// Ino128 must map it back through the engine instead of using this method.
func (i Ino128) Ino() Ino { return Ino(i.ID) }

// encodeIno128 writes the 16-byte little-endian form used by the "FI"
// reverse index: ID(8) | ObjectID(4) | Gen(4), low counter bits first.
func encodeIno128(i Ino128, buf []byte) {
	binary.LittleEndian.PutUint64(buf[0:8], i.ID)
	binary.LittleEndian.PutUint32(buf[8:12], i.ObjectID)
	binary.LittleEndian.PutUint32(buf[12:16], i.Gen)
}

// decodeIno128 is the inverse of encodeIno128.
func decodeIno128(buf []byte) Ino128 {
	return Ino128{
		ID:       binary.LittleEndian.Uint64(buf[0:8]),
		ObjectID: binary.LittleEndian.Uint32(buf[8:12]),
		Gen:      binary.LittleEndian.Uint32(buf[12:16]),
	}
}

// Ino128Resolver resolves a compressed 64-bit fuse inode back to its full
// 128-bit inode. Implemented by KV engines; returns ESTALE when the inode
// no longer exists. Engines not implementing it keep using raw counter
// values as inodes.
type Ino128Resolver interface {
	ResolveIno128(ino Ino) (Ino128, error)
}
