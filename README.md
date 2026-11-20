# HerbariumGraph

一个用 Go 写的原生图存储引擎,面向标本馆这类"采集人—标本—分类单元—文献"关系错综复杂的数
据。点(节点)和关系都是定长记录,直接按记录号存放在文件里,不在内存里另建索引;页面把存储
结构和查询的展开过程都画出来。

```
go run ./cmd/herbarium-server          # 打开 http://localhost:8080
go test ./...                          # 单元测试 + 压力对照
go run ./cmd/stress                    # 15 万点 / 40 万关系 / 4 万次随机操作
```

## 存储结构

两个定长记录文件,记录号就是文件里的位置,没有记录索引、没有页间接。

| 文件 | 记录 | 大小 |
|---|---|---|
| `nodes.dat` | 节点记录 | 512 B |
| `rels.dat` | 关系记录 + 链描述符记录 | 512 B |

512 字节正好 8 条一页(8 KiB),记录不会跨页。所有内部指针按 `记录号+1` 存,0 表示"无",
所以全零记录就是空闲记录。

```
NODE RECORD (512 B)                          RELATIONSHIP RECORD (512 B)
 0  kind            0 空闲 1 活             0  kind
 1  flags  bit0 出向分组 bit1 入向分组       4  relID        稳定逻辑 id,不复用
 4  nodeID                                   12 startNode
12  outCount       出向关系数缓存            20 endNode
16  inCount        入向关系数缓存            28 startPrev   起点 OUT 链前驱
20  outGroupHead   出向链目录首描述符        36 startNext   起点 OUT 链后继
28  inGroupHead    入向链目录首描述符        44 endPrev     终点 IN 链前驱
36  labelLen                                  52 endNext     终点 IN 链后继
40  label                                     60 outChain    跳到本关系 OUT 链描述符
100 propsLen                                  68 inChain     跳到本关系 IN 链描述符
104 props                                     76 typeID      关系类型表下标
364 typeCountN     按类型计数缓存条数         80 propsLen
368 typeCounts     16 × (类型,方向,计数)      84 props
```

**链描述符**是 `rels.dat` 里 kind=2 的独立记录,不随关系增删或整理移动:

```
 0 kind=2   4 descID   12 node   20 dir   22 typeID   24 size(权威链长)
28 first   36 last    44 dirPrev  52 dirNext   ← 目录是环形
```

### 双向链(需求 1)

每条关系同时挂在两条链上:起点节点的 **OUT 链** 和终点节点的 **IN 链**,都是双向链表。
从一个点出发只顺着 `startNext` / `endNext` 走,不查任何索引就能走遍它的全部关系。链的
头尾和长度存在描述符里,关系记录里存的是前驱/后继。

### 稠密点按类型分组(需求 2)

某个方向的关系数超过阈值(`-threshold`,默认 32)时,这个方向从"一条链"改成"每个关系
类型一条链":描述符按 `(节点,方向,类型)` 各一个,用环形目录串起来。遍历时带类型过滤
就直接进对应那条链,**完全不碰别的类型的关系**。阈值以下仍然是一条链。分组开关
(`-group=false`)只改变链的数量,不改变任何查询结果——`VerifyGroupingEquivalence` 和
`TestGroupingOnOffSameResults` 对同一份逻辑图分别开关两种布局,逐条比对遍历和路径结果。

### 长链从头删不用走(需求 3)

每条关系记录里存着 `outChain` / `inChain`,即它所在链的描述符记录号。删除中间的关系时:
顺着跳转指针找到描述符 → 描述符里的 `size` 减一 → 把自己前后两个邻居互相接上。全程
O(1),不用从链头走。`TestLongChainMiddleDeleteReadsFewPages` 在 4000 条关系的单链正中
删一条,只读 5 页。

### 空闲回收 + 两条链同时断开(需求 4)

删点或删关系时,记录被标成 kind=0 并压入对应文件的空闲表,下一次创建优先复用。关系的
两条链(起点 OUT、终点 IN)在一次事务里一起断开再接上;删点时先沿两条链收集全部关系,再
逐条删除,自环只会删一次。`CheckIntegrity` 会验证每条关系**恰好**出现在两条链上。

### 按关系条数过滤(需求 5)

节点记录里缓存 `outCount` / `inCount`,以及最多 16 组"按类型计数"。增删关系时同步加减。
按条数过滤时只读节点记录,不走进任何关系链;`TestCountFilterAvoidsChains` 验证过滤一个
没有出边的点时只读 1 条记录。

### 唯一约束(需求 6)

`AddUniqueConstraint(label, property)` 声明"同一标签下该属性值唯一"。约束检查与写入在
**同一次提交**里:`Update` 开事务 → 检查索引 → 写记录,任一步失败整笔回滚(撤销日志还原
记录和被改动的内存索引),不会出现"约束没过但记录已经写进去"。违反约束返回
`ErrConstraint`(HTTP 409)。删除持有该值的节点后值可复用。

### 平行关系算不同路径(需求 7)

路径用**关系序列**定义:同一对点之间两条平行关系是两条不同的路径。`FindPaths` 可选
`WithRels`,返回"只给点序列"或"点序列 + 经过的关系"。压力对照里对随机点对逐一比对本引擎
和暴力邻接表的路径条数。

### 离线整理(需求 8)

`Compact()` 把同一个起点的关系记录挪到文件里相邻的位置,重建两条链和所有描述符,并压掉
空闲槽。节点的 id 就是它的槽位,但整理**不移动节点**;关系 id 是独立逻辑号,与槽位无关,
所以整理前后的所有公开 id、所有查询结果完全相同。新文件先写在旁边再原子改名,整理中途
崩溃不会破坏原库。

## 页面

四个标签页:

- **图浏览** — 按标签/搜索挑节点,看它的出入关系、按类型计数、每条链的描述符槽和首尾。
- **记录文件** — 逐页看 `nodes.dat` / `rels.dat`:每页 16 个槽的占用前缀(N 点、R 关系、
  C 描述符、· 空闲),点进一页看每条定长记录解码后的所有字段。
- **稠密点关系分组** — 输入节点看它被拆成几条链,每条链的类型、长度、首尾关系;选一种
  类型遍历,对比"只扫该类型链"和"扫全方向"的读页数。
- **查询展开过程** — 点对之间枚举路径,列出每层展开进入了哪些节点的哪条链、链长、本次
  访问了多少条关系,以及整次查询读了哪些页。

## 验证

`go test ./internal/graph` 覆盖:

| 测试 | 验证什么 |
|---|---|
| `TestStressSmall` / `TestStressMixedGrouping` | 随机增删改查,遍历/路径/度数/约束全部对照暴力邻接表 |
| `TestIntegrityAfterRandomWorkload` | 随机工作负载后结构完整 |
| `TestGroupingOnOffSameResults` | 分组开/关结果一致 |
| `TestCompactPreservesResults` / `TestCompactReducesPageReads` | 整理前后结果一致、读页下降 |
| `TestLongChainMiddleDeleteReadsFewPages` | 长链中间删除 O(1) 读页 |
| `TestDenseTypeFilterReadsOnlyMatchingChain` | 类型过滤只扫对应链 |
| `TestCountFilterAvoidsChains` | 条数过滤不走进链 |
| `TestRollbackOnConstraintLeavesNoTrace` | 约束失败整笔回滚、不残留 |

`cmd/stress` 做全量跑,输出 JSON 和汇总:

```
nodes=150000 rels=400000 ops=40000
traversals=8692 pathQueries=7156 degreeFilters=4799 constraintViolationsBlocked=1191
workload failures: 0
grouping on/off identical: true
compaction identical: true
page reads: traverse 3.40 -> 2.75 avg (-18.9% change), path 349.00 -> 224.00 avg
records moved=355555 freed slots=48056 contiguous runs=134188 avg run=2.65
elapsed=1m40s
```

## 已知限制

- 单个写入者:所有操作在一把锁下串行,读也取同一把锁,所以读永远看不到半提交状态。
- 定长记录的内联容量:节点属性 264 B、关系属性 300 B、标签 60 B;超出会被拒绝而不是溢出
  到别处。
- 关系类型、节点标签不可修改(只能删点重建)。
- 整理需要离线:它重建整个 `rels.dat`,期间不应有其他进程写同一个库。
