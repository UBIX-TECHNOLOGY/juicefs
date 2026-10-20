# 128 位 inode 设计（32+32+64）与 FUSE 64 位压缩索引

> 状态：设计稿
> 前置文档：`docs/zh_cn/design/inode-layout-6-8-50.md`（inode 6+8+50 /
> slice id 8+56 POC，本方案在其思路上演进）
> 适用范围：仅 TiKV 及 KV 族元数据引擎（`pkg/meta/tkv.go`），新建卷专用
> 参考系统：Lustre FID（128-bit File Identifier）与 FID→inode 压缩

## 1. 背景与目标

前置 POC（6+8+50）在 64 位 inode 内划出 6 位保留 + 8 位 object_ID +
50 位 inode ID。该方案的两个结构性限制随着后续需求暴露出来：

1. **50 位 inode ID 上限太小**。`2^50 ≈ 1.1e15`，海量小文件、
   快照树、多代垃圾回收等场景下会触及封顶 `ENOSPC`，且未来在
   保留位上启用快照/克隆后，每个克隆树都要消耗独立的 inode 编号
   子空间，50 位更加紧张。
2. **保留位与 trash 的冲突**（前置文档 §7.1）。64 位空间内
   保留位（bit 58~63）与 `TrashInode` 的数值范围判定互相踩踏，
   导致保留位只能恒 0，快照/克隆永远无法启用。

本方案把 inode 从 64 位扩展为 **128 位**，位域重新划分为
**32 + 32 + 64**；同时借鉴 Lustre 的做法：**元数据面全程使用
128 位 inode，FUSE 内核接口只接受 64 位 inode number**，两者之间
由一条「压缩 / 索引」通道连接。

### 1.1 设计目标

1. 128 位 inode 位域为 **保留(32) | object_ID(32) | inode ID(64)**，
   仅修改 TiKV / KV 族引擎，Redis / SQL 引擎行为不变。
2. FUSE 返回的 inode number 恒为 64 位，且**从 128 位 inode
   无损压缩而来**；内核传入 64 位 inode number 后，vfs 能
   **快速索引**（O(1)，常规路径无磁盘 IO）还原出 128 位 inode。
3. 顺带解决前置文档 §7.1 的 trash 冲突：trash 迁入 128 位空间，
   64 位保留位冲突不复存在。
4. 不考虑存量数据：仅适用于新建集群 / 全新卷，全集群统一版本。

### 1.2 Lustre 设计参考

Lustre 的分布式对象标识是 128 位 FID（`struct lu_fid`）：

```
63                 0 63      32 31      0
┌────────────────────┬───────────┬──────────┐
│ f_seq（序列号）     │ f_oid     │ f_ver    │
│    64 bit          │  32 bit   │  32 bit  │
└────────────────────┴───────────┴──────────┘
```

与 FID 相关的两个机制是本方案直接借鉴的对象：

- **FID→inode number 压缩**：POSIX / FUSE 接口只暴露 64 位
  inode number，Lustre 把 FID 打包压缩成 64 位呈现给用户
  （如 IDIF：当 `f_seq < 2^32` 且 `f_oid` 可让位时，用
  `f_seq << 32 | f_oid` 直接合成 64 位；旧式 inode+generation
  则走 IGIF 特殊序列段）。压缩的核心原则是：
  **可结构化压缩时无损直译，不可压缩时借助持久/缓存映射**。
- **客户端 FID 缓存与 ESTALE**：客户端不维护全局的
  inode number→FID 反查表，而是靠 dentry/inode 缓存持有
  （inode number → FID）绑定；缓存缺失或对象已被删除时，
  向上层返回 `ESTALE`，由 VFS 通过父目录重新 lookup 重建。

本方案的取舍：**压缩规则更简单、反查表更明确**——128 位 inode
的低 64 位（inode ID）由全局计数器分配、天然全局唯一，直接
作为 FUSE inode number（等价于 Lustre IDIF 的「结构化直译」
思路，但无需 `f_oid` 让位条件）；vfs 维护显式的
inode number→128 位 inode 映射表承担反查（等价于 Lustre
客户端的 FID 缓存），淘汰后用 `ESTALE` 收敛。

## 2. 128 位 inode 位域划分（32+32+64）

```
127           96 95           64 63                             0
┌────────────────┬───────────────┬───────────────────────────────┐
│ gen / 保留      │ object_ID     │ inode ID（全局唯一，单调分配） │
│  32 bit        │  32 bit       │  64 bit                       │
│ 恒 0（第一阶段）│ 0 ~ 2^32-1    │ 计数器分配，上限 2^63-1        │
└────────────────┴───────────────┴───────────────────────────────┘
```

- **gen / 保留位（32 bit，bit 96~127）**：代数（generation），
  为快照 / 克隆等需要同名派生对象的特性预留。第一阶段**恒为 0**
  ——128 位空间足够宽裕，trash 已迁入 object_ID 保留段（§4.4），
  前置文档 §7.1 的数值范围冲突在 128 位布局下天然消失，
  保留位的启用不再有历史包袱。
- **object_ID（32 bit，bit 64~95）**：数据归属对象子集群 ID，
  语义与前置 POC 相同（父目录继承、创建时定型、slice 路由由
  slice id 自带，见 §6）。容量从 8 位扩至 32 位，支撑
  `2^32` 个子集群。
- **inode ID（64 bit，bit 0~63）**：全局唯一 inode 编号，
  沿用 `nextInode` 计数器批量分配，单调递增。**取值范围限制在
  `[0, 2^63)`**（见 §4.1：最高段让位给内部节点，这是 FUSE 压缩
  无碰撞的前提）。相比前置 POC 的 50 位，可用空间扩大 16384 倍。

### 2.1 Go 类型表示

Go 无原生 128 位整数（`math/big` 不适合热路径），定义为双字结构：

```go
// pkg/meta/interface.go
type Ino128 struct {
    Gen      uint32 // bit 96~127，第一阶段恒 0
    ObjectID uint32 // bit 64~95
    ID       uint64 // bit 0~63，全局唯一
}
```

定长 struct（16 字节）而非 `math.Uint256` 式位打包：
字段拆取是直接取成员，无移位掩码；作为 map key / value 可比较、
可拷贝；序列化按固定字段序编码。

## 3. FUSE 64 位压缩与快速索引（本方案核心）

### 3.1 压缩规则：fuse_ino = inode ID

```
128 位 inode:  gen(32,恒0) | object_ID(32) | inode ID(64)
                                    │
                                    ▼ 压缩（截取低 64 位，零成本）
FUSE inode number = inode ID （uint64）
```

- **无损性**：inode ID 由全局计数器分配，单调递增、永不复用，
  因此低 64 位自身即是全局键——两个不同的 128 位 inode
  不可能有相同的 inode ID。压缩是「截取」而非「哈希」，
  无碰撞概率、无需校验。
- **正向转换零成本**：返回 attr / entry 给 FUSE 时取
  `.ID` 即可，一次字段访问。
- **与内部节点不碰撞**：`pkg/vfs/internal.go` 中内部节点
  （`.accesslog` / `.control` / `.stats` / `.config` / `.trash`）
  占用 `[0x7FFFFFFF00000000, 0x7FFFFFFF00000000+n]`，
  trash inode 为 `TrashInode = 0x7FFFFFFF10000000 + next`。
  因此约定 **inode ID 计数上限为 `2^63 - 1`**：`[2^63,
  0x7FFFFFFFFFFFFFFF]` 是数值区间上的空段（作为内部节点与
  128 位常规 inode 的隔离带），计数器越过 `2^63-1` 时返回
  `ENOSPC`，严禁向上进位或越过隔离带。

### 3.2 反向索引：vfs inodeMap

内核回调（`GetAttr` / `Open` / `Read` / `Write` / `SetAttr` / …）
只携带 64 位 inode number，vfs 必须还原 128 位 inode 才能访问
元数据引擎。实现为 vfs 内显式双向映射表：

```go
// pkg/vfs/internal.go（新增）
type inodeEntry struct {
    ino128 meta.Ino128
    attr   *Attr   // 顺带缓存 attr，供 GetAttr 命中
}

// VFS 成员
fuseInodes sync.Map // fuse ino (uint64) -> *inodeEntry
```

**生命周期（借鉴 Lustre 客户端 FID 缓存语义）**：

| 时机 | 动作 |
| --- | --- |
| `Lookup` / `MkNod` / `Mkdir` / `Symlink` / `Create` / `Link` 成功 | meta 返回 128 位 inode → 写入 `fuseInodes[ino128.ID]` |
| `GetAttr`（未命中时） | 通过内核携带的 64 位 ino 反查失败 → 走慢路径（§3.3）回填 |
| `readdir` 枚举每项 | meta 返回 128 位 inode → 批量写入（子树预热） |
| `Unlink` / `Rmdir`（链接数归零）与内核 `Forget` | 删除对应表项 |
| `Rename` / `Link` | 表项不变（inode 不因改名/链接变化） |

**表项规模**：一条 16 字节 struct + map 开销 ≈ 50 字节；
表项生命周期由内核 dentry/inode 缓存持有（`Forget` 驱动回收），
规模与内核缓存中的活跃 inode 数一致，不会随文件总数无限增长。
另加软上限（如 `--fuse-inode-map-cap`，默认无上限），超限后
按 LRU 淘汰，淘汰后命中缺失走 §3.3 慢路径。

### 3.3 慢路径：缺失时的重建

`fuseInodes` 未命中（进程重启后内核仍持有旧 inode number、
LRU 淘汰、或 inode 从未经过本挂载点 lookup）时的处理：

1. **持久映射兜底（meta 层反向索引）**：kvMeta 维护
   `fuse_ino → Ino128` 的辅助索引，key 为 `'Z' + 原始 8 字节
   LittleEndian inode`（value 为 16 字节 Ino128）。前缀选用
   `'Z'` 而非直觉的 `"FI"`，原因有二：其一，**编码通道防递归**
   ——节点 key 的 16 字节 inode 段由 `encodeInode` 写出（见
   §4.2），而 trash 等场景下 `encodeInode` 自身需要读该索引，
   索引 key 必须绕开 `fmtKey`（否则递归构造）；其二，TiKV key
   空间中 `"F"` / `"P"` 前缀被 flock/plock 扫描整体遍历，
   `"FI"` 键会被误捞进会话锁解析。分配 inode 时与主记录
   同事务写入；删除时同事务清除。慢路径是一次点查，O(log n)。
2. 点查仍 miss（inode 已删除）→ 返回 `ESTALE`，由内核 VFS
   按标准 POSIX 语义处理（与 Lustre 行为一致）。

> **是否可以省掉持久索引？** 若挂载点能保证「任何 fuse ino
> 到达前必先经过 Lookup/readdir」（多数 POSIX 路径成立），
> 纯内存表即可。但 FUSE 场景下内核缓存跨进程重启保留
 （graceful restart / `--force-owner` 场景），且 NFS 导出
 （filehandle 直含 inode number）会凭空到达，因此保留
 持久兜底索引是必要的。fsck 需校验 `'Z'` 索引与主记录的
 一致性（§8）。

### 3.4 内部节点与根目录

根目录（`RootInode = 1`）与内部节点不走压缩通道：
其 fuse ino 即 meta 侧 inode，`fuseInodes` 中恒不写入，
`vfs` 对 `ino >= minInternalNode` 的请求直接走现有 internal
node 路径。常规 inode ID 被限制在 `[0, 2^63)`，与
`minInternalNode = 0x7FFFFFFF00000000` 起的内部节点段
无交叠，分流判断只需一次 `>= minInternalNode` 比较
（现有 `IsSpecialNode()` 逻辑复用）。

## 4. 元数据层改造（TiKV / KV 族）

### 4.1 计数器与分配

- `nextInode` 计数器语义不变（批量 `incrCounter`），管理
  64 位 inode ID，**封顶 `2^63-1` 返回 `ENOSPC`**。
- 组合钩子 `inodeComposer` 保留，签名改为返回 `Ino128`：

```go
func (m *kvMeta) composeInode(ctx Context, id uint64, parent Ino128) (Ino128, error) {
    if id >= maxInodeID { // maxInodeID = 1<<63 - 1
        return Ino128{}, syscall.ENOSPC // 禁止越过内部节点隔离带
    }
    objectID := parent.ObjectID // 继承父目录
    if v := ctx.Value(ObjectIDCtxKey{}); v != nil {
        objectID = v.(uint32) // mkdir --object-id 显式指定
    }
    if m.getFormat().ObjectClusterByID(objectID) == nil {
        return Ino128{}, syscall.EINVAL
    }
    return Ino128{ObjectID: objectID, ID: id}, nil // Gen 恒 0
}
```

- `CloneFile`：新 inode 继承**源 inode 的 ObjectID 与 Gen**
  （克隆语义），普通创建仅继承 ObjectID。

### 4.2 TiKV key 编码：inode 变 16 字节

`encodeInode` / `decodeInode` 从 8 字节 LittleEndian 改为
**16 字节 128 位编码**，字段序 `ID(8, LE) | ObjectID(4, LE) |
Gen(4, LE)`（即「低 64 位在前」的 128 位小端）。kvMeta 内所有
含 inode 段的 key（节点 / dentry / chunk / xattr / parent /
flock / plock / detached / trashEntry 等）与全部原始 key 偏移
常量同步 +8，两种编码通道明确分工：

| 编码 | 形态 | 用途 |
| --- | --- | --- |
| **key 编码（16 字节）** | `ID\|ObjectID\|Gen` 完整 128 位 | 一切 TiKV key 中的 inode 段；`objectID` 随 key 自证数据归属 |
| **value 编码（8 字节）** | 压缩 64 位 inode ID | dentry value（`packEntry`）、pb/DumpedDelFile 等跨引擎兼容格式 |

- **防写热点保持**：inode ID（低位、顺序变化的部分）位于
  key 前 8 字节，小端编码下顺序分配的 key 依然散布于整个
  key 空间，前置文档 §3.2 的打散论证逐条成立；
- **key 排序语义**：数值序（ID 主序 → ObjectID → Gen）与
  字节序一致，range scan（ListXattr、批量遍历、Trash 扫描等）
  的排序假设不受影响；同 ID 不同 ObjectID 的 key 相邻，
  语义自洽；
- **解码即回填**：从 key 解析 inode（`decodeInode`）时把完整
  128 位形式写入进程内 `inoCache`——key 自身携带全部 128 位
  信息，扫描路径（compaction、gc、dump）无需额外查询即可
  恢复归属；
- **编码只走缓存（不 IO）**：`encodeInode` 在事务闭包内被
  `fmtKey` 调用，只查内存 `inoCache` 与根 / trash 派生规则，
  **严禁读存储**（会嵌套事务死锁）。缓存由 `decodeInode` 与
  分配钩子预热；cache miss（删除后 tombstone 等罕见场景）
  回退 objectID=0，保证 key 确定性；
- **trash 编码**：trash inode 的 64 位 ID 为完整
  `TrashInode + seq`（非裸 seq），与常规 inode 的 ID 空间
  不相交，key 中 objectID 为 `ReservedTrashObjectID`；
- 根目录 / 内部节点编码为 `ID|0|0`，与常规 inode 同一编码
  通道。

同事务内主记录与持久反向索引（§3.3 `'Z'` 索引）原子写入。

### 4.3 trash 迁入 128 位空间（解决前置 §7.1）

128 位空间宽裕，trash 不再借用常规 inode 数值区间的顶端：

- 预留 **object_ID 高端段**为特殊对象命名空间：
  `objectID >= 0xFFFFFF00` 保留（约 2^8 个特殊值，第一阶段
  定义 `ReservedTrashObjectID = 0xFFFFFF00`）；
- trash inode = `Ino128{ObjectID: ReservedTrashObjectID,
  ID: TrashInode + nextTrash}`（Gen 恒 0）：64 位 ID 保持
  **完整的旧派生值**（非裸 seq），与常规 inode 的 ID 空间以
  §3.1 隔离带天然分离，key 编码（§4.2）零碰撞；`nextTrash`
  计数器语义不变，此前「双计数器分段 / 单计数器」的悬置
  选项就此定案——沿用旧派生，无需任何迁移；
- `IsTrash()` / `IsNormal()` 从数值范围比较改为
  **显式 ObjectID 段判断**（`ObjectID == ReservedTrashObjectID`
  / `< ReservedTrashObjectID && >= minNormal`），前置文档
  §7.1 的两个冲突点（范围判定污染、trash 派生公式占用保留位）
  同时消除，快照/克隆启用 gen 位不再有障碍；
- 内部节点判定（vfs 侧 `minInternalNode`）不变。

### 4.4 dump / backup / 协议

- `Attr.Ino`、`Entry.Inode` 等元数据接口类型由 `Ino` 改为
  `Ino128`（`pkg/meta/interface.go` 及全部引擎实现）；
- dump JSON：inode 主字段保持 uint64（压缩值）；新增可选
  `DumpedAttr.ObjectID` 字段随 attr 透出（`objectID`，
  omitempty），load 侧据此为每条记录种子化 `'Z'` 反向索引
  （§3.3）；Gen 不入 dump（恒 0）。旧 dump（无该字段）在
  KV 新卷上加载后 objectID 全 0：读写仍可进行，但数据归属
  语义丢失，load 对 KV 卷应校验并告警（§9 挂账）；新 dump
  被旧客户端读取时未知字段被忽略（JSON 宽容），不产生错误
  数据，但恢复后的卷不再携带归属信息，跨版本恢复需人工确认
  ——遵循「不静默丢正确性关键数据」；
- `pkg/meta/dump.go` / `backup.go` 计数器重建：inode 即压缩
  64 位 ID，`nextInode` 直接取最大 inode +1，`nextTrash`
  取 `inode - TrashInode`，无需剥离任何位域；
- `MinClientVersion` 需抬升至支持 128 位 inode 的版本
  （新卷专用，无需混布评估，但 format 时应显式校验并记录）。

## 5. vfs / FUSE 层改造

| 位置 | 改动 |
| --- | --- |
| `pkg/vfs/internal.go` | 新增 `fuseInodes` 映射表与回填/淘汰；`IsSpecialNode()` 分流不变 |
| `pkg/vfs/vfs.go` | Lookup/Create/Mkdir/GetAttr/readdir 等出口把 meta 返回的 `Ino128` 写入映射表、以 `.ID` 作为 fuse ino 返回；入口 `Ino(header.NodeId)` 经 `resolve()` 还原为 `Ino128` 再调用 meta |
| `pkg/vfs/handle.go` | handle 表键保持 fuse ino；Open/Read/Write 持有的 inode 均为解析后的 128 位 |
| `pkg/vfs/reader.go` / `writer.go` / `compact.go` | 内部以 128 位 inode 为主键（chunk 记录、`InoMap` 等），对外回调不变 |
| `pkg/fuse/fuse.go` | 接口层无感知（仍 64 位 `header.NodeId`），仅类型别名调整 |
| graceful restart | 内核缓存中的 fuse ino 在新进程中经持久索引 `"FI"` 重建映射，路径与 §3.3 慢路径共用 |

**Hadoop/Java SDK 与 S3 gateway**：`sdk/java` 与 `pkg/gateway`
以 64 位 inode 暴露/建键，需同样接入压缩/索引通道（gateway
可直用 inode ID 作对象键组成部分，Java SDK 沿用 fuse ino），
列入改动清单（§7），实现细节在各自阶段细化。

## 6. 与 slice id（8+56）的关系

slice id 路由方案（前置文档 §8）**完全不受影响**：数据面路由
依据是 slice id 自身的高 8 位，与 inode 位宽解耦。

差异挂账：slice id 的 object id 仅 8 位（255 子集群），
而 128 位 inode 的 ObjectID 为 32 位。子集群注册表扩容时，
slice id 8+56 布局需独立演进（如升级为 32+64 的 96 位 id，
chunk 记录 value 定长扩展），本方案不做，登记在 §9。

## 7. 改动清单

| 文件 | 改动 |
| --- | --- |
| `pkg/meta/interface.go` | `Ino128` 类型；`Attr` / `Entry` 等接口类型切换；`ReservedTrashObjectID`、`IsTrash()`/`IsNormal()` 改为段判断 |
| `pkg/meta/tkv.go` | `Ino128` 助手接入；`fmtKey`/`keyLen` Ino→16 字节；`encodeInode`（仅缓存解析、事务内零 IO）与 `decodeInode`（解码即回填 `inoCache`）；全部原始 key 偏移常量 +8 审计；`'Z'` 持久反向索引（raw 8 字节 key：防编码递归、防 `"F"`/`"P"` 前缀扫描误捞）；分配钩子（继承 / 指定 / 注册校验 / 封顶 + 索引同事务写入 + 三处删除点清理）；trash 派生 `trashIno128`（完整旧值 ID） |
| `pkg/meta/tkv_bak.go` | dump 备份路径的 key 解析偏移同步（`e.k[1:17]` / `e.k[17]` / `e.k[18:]`；SS / D 段 `Get(16)`） |
| `pkg/meta/dump.go` / `backup.go` | `DumpedAttr.ObjectID` 字段与 dumpEntry 缓存回填；loadEntry 种子化 `'Z'` 索引；计数器重建取裸 64 位 ID（格式版本化与 objectID 校验挂账 §9） |
| `pkg/meta/base.go` | `nextInode` 封顶 `2^63-1`；clone 继承 Gen |
| `pkg/meta/config.go` | `ObjectCluster.ID` 扩为 uint32；注册表校验调整 |
| `pkg/meta/dump.go` / `backup.go` | dump 格式版本化与 `DumpedAttr.ObjectID`；计数器重建取裸 ID；load 校验 objectID 已注册 |
| `pkg/vfs/internal.go` / `vfs.go` / `handle.go` | `fuseInodes` 映射表、resolve()、Forget 驱动回收、软上限 LRU |
| `pkg/vfs/reader.go` / `writer.go` / `compact.go` / `fill.go` | 128 位 inode 主键替换 |
| `pkg/fuse/fuse.go` / `pkg/winfsp/` | 类型适配（对外仍 64 位） |
| `sdk/java` / `pkg/gateway` | 压缩/索引通道接入 |
| `cmd/fsck.go` | `'Z'` 索引一致性校验 |
| `pkg/meta/tkv_test.go` / `pkg/vfs/*_test.go` | §8 用例 |

## 8. 测试计划

1. **压缩/还原往返**（`TestIno128Layout`）：随机与边界
  `Ino128`（ID ∈ {1, 2^49, maxFuseInoID}，ObjectID ∈ {0, 1,
  0x12345678}），16 字节编解码往返 + 压缩取 `.Ino()` 逐字段
  相等；ID=0 与 ID > maxFuseInoID 被 `composeIno128` 拒绝
  （ENOSPC）；trash 派生 `trashIno128` 落在保留段且 ID 为
  完整旧值。
2. **隔离带无碰撞**（`TestKVMetaInodeIDExhausted`）：计数器
  拨至 `maxFuseInoID`，最后一格分配成功、下一格 `ENOSPC`，
  fuse ino 永不越过 `minInternalNode` 段。
3. **inodeMap 生命周期**（`TestVFSInodeMap`）：mkdir 回填快表、
  清快表后持久索引慢路径解析、删除 inode 后解析返回 ESTALE
  （并回验 `inoCache` 失效，不残留陈旧映射）。
4. **组合语义**（`TestKVMetaInodeComposition`）：objectID
  继承 / 显式指定 / 未注册拒绝（EINVAL）、clone 继承源、
  rename 不变；`TestInodeKeyEncoding128` 进一步校验：
  节点 key 长 18、key 段 16 字节往返、冷解析（清缓存后
  encodeInode 经持久索引产出逐字节一致的 16 字节）、未知
  inode 与 trash 的确定性编码、entry value 仍为压缩 64 位。
5. **key 编码回归**（`TestInodeKeyEncoding128`）：16 字节编码
  顺序分配 key 的散布性（复用前置文档的防热点断言方式）、
  range scan 排序与 8 字节时代语义一致。
6. **dump/backup 往返**（`TestKVDumpLoadComposedInodeCounter`
  与既有 dump/load 用例）：计数器重建取裸 64 位 ID；
  `DumpedAttr.ObjectID` 随 dump 透出并在 load 时种子化
  `'Z'` 索引；旧 dump（无 objectID 字段）加载后归属语义
  缺失的告警校验挂账 §9。
7. **重启/ESTALE**：卸载重启后内核旧 inode number 仍可解析
  （持久索引）；删除 inode 后旧 number 返回 ESTALE。
8. **隔离回归**：object_ID=0 子树全部现有测试通过（前置 POC
  §8.8 的用例在 128 位布局下平移重跑）。

## 9. 遗留待处理问题

| 事项 | 说明 |
| --- | --- |
| slice id 8+56 与 ObjectID 32 位不匹配 | 子集群数超过 255 时 slice id 需独立升级（如 32+64），chunk 记录 value 定长变更，需单独方案与 MinClientVersion 联动 |
| trash 与常规 inode 的计数器区间 | 已定案（§4.3）：trash 沿用完整旧派生值 `TrashInode + seq` 作 64 位 ID，key 编码零碰撞，无需迁移；补 trash 编码不变量测试即可 |
| encodeInode 缓存 miss 的 tombstone 键 | 删除后的 delfile tombstone 若进程重启且缓存清空，16 字节 key 中 objectID 回退 0；同键两次写入需一致（当前以 ID+0 落键稳定成立），dump/scan 侧解码不受影响，补回归用例挂账 |
| 旧 dump 恢复 KV 卷的归属语义 | 旧格式无 `objectID` 字段，load 后全部落 cluster 0：读写可用但数据归属丢失，load 侧需按卷类型校验并告警（§4.4） |
| gen 位启用 | 快照/克隆：128 位布局已解除前置 §7.1 冲突，但 dump/load、跨客户端语义需独立方案 |
| NFS 导出 filehandle | filehandle 直含 64 位 inode number，依赖 `'Z'` 持久索引；跨挂载点 export 的一致性需在集成阶段验证 |
| `Ino128` 热路径开销 | struct 拷贝（16 字节）与 map key 比较、key 段 8→16 字节带来的存储放大（约 +8B/键），预期可忽略；bench 需覆盖 readdir/readdirplus 与 open/read 热路径 |
