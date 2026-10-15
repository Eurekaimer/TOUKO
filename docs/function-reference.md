# TOUKO 生产函数导航与实现详解

本文按生产源码逐文件解释函数和方法，包含非导出的关键 helper；不把测试辅助函数算作产品 API。适合在阅读 [代码导读](code-guide.md) 后，带着具体符号搜索这里，再回源码逐行理解。实际操作顺序见 [使用教程](usage.md)。格式、状态和发布契约分别见 [repository-format.md](repository-format.md)、[local-state.md](local-state.md)、[publication.md](publication.md)。

本文描述代码职责和失败语义，不是检查执行记录，也不表示已用真实 GitHub token 完成线上验收。当前没有 GUI、R2、缩略图或插件系统；delivery 只推导链接，不负责上传到 CDN、探测链接或清除 CDN 缓存。

## 1. 如何用这份导航

表格中符号省略当前包名前缀，例如 domain 章节的 `ParseAssetID` 指 `domain.ParseAssetID`；`(*Service).Commit` 表示接收者为 `*app.Service` 的方法。方法名可能跨包重复，务必结合文件路径搜索。机械的文本编码方法可以共用一行，但名字全部列出。Cobra 的匿名 `RunE`、排序比较器、`defer` 和 goroutine 没有独立符号，职责并入创建它们的具名函数。

推荐的调用链阅读顺序：

1. **身份与格式**：`domain/identity.go`、`domain/asset.go` → `repository/format.go`、`path.go`。
2. **本地暂存**：`app/state.go` → `image/ingest.go` → `staging/files.go` → `index/store.go` → `app/stage.go`。
3. **一次发布**：`repository/planner.go` → `app/commit.go` → 所选 Git adapter。
4. **恢复与重建**：`app/sync.go`、`app/repo.go` → `app/commit.go` 的 journal helper。
5. **用户入口**：`cmd/touko/main.go` → `cli/root.go` → 各命令构造函数。

三个不同身份不能混用：Asset UUID 是用户记录身份；SHA-256 `ObjectHash` 是图片内容身份；`RepositoryHead`、`BlobID`、`TreeID` 是 Git 提交或对象身份。相同 bytes 可以有不同 Asset ID；跨 namespace 仍有相同内容 hash，但物理 placement 不同。

## 2. `internal/domain`：不依赖 Git、SQL 或 CLI 的规则

### [`identity.go`](../internal/domain/identity.go)

此文件由 metadata 编解码、selector 解析、stage 新记录创建共同使用。UUID/hash 的文本方法由 `encoding/json` 等编码器间接调用；不需要额外保存一份字符串字段。

| 精确符号 | 作用、输入与输出 | 副作用、错误与设计理由 |
| --- | --- | --- |
| `NewAssetID` | 生成随机 UUID，返回 `AssetID, error`；由 `Service.Add` 创建记录时调用。 | 使用随机源，可能失败；将错误交给调用者，不 panic。每次 add 的记录身份独立于文件内容。 |
| `NewRepositoryID` | 生成 `RepositoryID, error`；由 `Service.Initialize` 为 manifest 创建身份。 | 同样保留随机源错误；用于防止已有 state-dir 被悄悄接到另一图库。 |
| `(AssetID).String`、`(RepositoryID).String` | 将 16 字节 UUID 转成标准小写、带连字符文本。 | 无 IO；用于路径、选择器、SQL 主键、人类输出。 |
| `parseUUID` | 内部严格解析器，输入文本，返回 UUID 或错误。 | 拒绝非规范小写写法和零 UUID；不只依赖宽松的第三方 UUID parser。 |
| `ParseAssetID`、`ParseRepositoryID` | 分别调用 `parseUUID` 并转换为不相混的领域类型。 | 无 IO；非法身份不会进入 metadata/index。 |
| `(AssetID).MarshalText`、`(RepositoryID).MarshalText` | 返回对应 `String` 的字节与 nil 错误。 | 供 JSON/text 编码复用同一规范表达。 |
| `(*AssetID).UnmarshalText`、`(*RepositoryID).UnmarshalText` | 通过各自 Parse 函数解码；成功才给接收者赋值。 | 错误时不把半解码值写入接收者。 |
| `(ObjectHash).String`、`(ObjectHash).MarshalText` | 32 字节 SHA-256 编为 64 位小写 hex 文本。 | 不代表 Git blob SHA-1；无 IO。 |
| `(*ObjectHash).UnmarshalText` | 校验长度、小写和 hex 字符，成功才写入 32 字节值。 | 拒绝大写、长度不符及非 hex；失败不改接收者。 |
| `ResolveAsset` | 从 `map[AssetID]Asset` 中按完整 UUID 或 8–36 字符前缀选出唯一 ID。 | 零匹配/长度非法为 `ErrAssetNotFound`，多个匹配为 `ErrAmbiguousSelector`。不按标题、文件名或 hash 猜测身份；调用方决定传入可见集合还是包含 staged-delete 的集合。 |

### [`asset.go`](../internal/domain/asset.go)、[`change.go`](../internal/domain/change.go)、[`errors.go`](../internal/domain/errors.go)

`Asset` 内嵌唯一的 `ContentObject` 描述；`Change` 的 add/update 携带完整新 Asset，delete 不携带 Asset。`change.go` 只有类型和常量，没有函数。结构合法性和状态转换合法性分开：下表的 Validate 不负责判定某次 update 是否对应已有记录，这由 stage/planner 处理。

| 精确符号 | 作用、输入与输出 | 副作用、错误与设计理由 |
| --- | --- | --- |
| `ParseNamespace` | 空串变成 `default`；其余必须匹配单段 `[a-z0-9][a-z0-9_-]{0,62}`，返回 `Namespace`。 | 无 IO；拒绝路径、点段、Unicode namespace。原文件名和标题仍可用 Unicode。 |
| `(ContentObject).Validate` | 校验 `png/jpg/gif` 与 MIME 对应关系，`0 < Size <= 100000000`，宽高为正。 | 只验证描述，不读取图片、不重新计算 hash；返回普通校验错误，由 repository 层按需包装 corruption。 |
| `(Asset).Validate` | 校验非零 ID、规范 namespace、非零时间及 `UpdatedAt >= CreatedAt`，再调用对象 Validate。 | 不检查远程路径存在性；让纯领域规则可在 ingest、格式和 planner 中共享。 |
| `(*ConflictError).Error` | 把 `Reason`、`Base`、`Remote` 拼为错误文本。 | `AssetID` 可供结构化调用者进一步查看；文本不承担全部冲突信息。 |
| `(*ConflictError).Unwrap` | 返回 `ErrConflict`。 | 保持 `errors.Is(err, domain.ErrConflict)` 可用；调用者可用 `errors.As` 取出 `delete_modify` 或 `head_changed`。 |

## 3. `internal/repository`：格式、快照、纯计划与深度校验

### [`git.go`](../internal/repository/git.go)：adapter 必须实现的窄端口

这里没有函数实现，只有接口方法。application 只依赖这些 Git 原语，不依赖 GitHub HTTP 或子进程。读操作固定到 immutable ID；写 blob/tree/commit 本身不会移动分支。

| 接口方法 | 输入 → 输出及调用者 | 重要契约 |
| --- | --- | --- |
| `Reader.Head` | 读取所选分支 → `RepositoryHead`；sync/commit/recovery 使用。 | local 空分支可返回空 HEAD；GitHub 空库会明确报错。 |
| `Reader.Commit` | 固定 commit ID → `GitCommit{Tree, Parents}`。 | 不再次解析可变分支；快照和 candidate parent 校验使用。 |
| `Reader.Tree` | 固定 tree ID → 完整 `[]GitEntry`。 | 不允许用截断列表构造部分图库；条目保留 mode/type/SHA/size。 |
| `Reader.OpenBlob` | 固定 blob ID → `io.ReadCloser`。 | 调用者必须 Close；图片使用流，不由接口强迫读入内存。 |
| `Reader.IsAncestor` | ancestor/head → bool 或错误。 | 恢复时用来证明 candidate 已发布；查询错误不能当作 false 后丢 journal。 |
| `Writer.CreateBlob` | reader 与声明 size → `BlobID`。 | 可能留下未被分支引用的 Git 对象；不代表发布完成。 |
| `Writer.CreateTree` | base tree 与 `[]TreeChange` → 新 `TreeID`。 | 基于 base 保留无关文件；`Blob == nil` 是删除，不是省略操作。 |
| `Writer.CreateCommit` | tree、单一 parent、message → candidate HEAD。 | local 初始化可无 parent；GitHub 要求已有 parent。 |
| `Writer.UpdateRef` | expected/next → error。 | 唯一分支发布点。localgit 有原子 expected-old-SHA CAS；GitHub `force:false` 只有 fast-forward 保护，不能宣称等价严格 CAS。 |

### [`format.go`](../internal/repository/format.go)

调用关系是 `Encode/DecodeManifest` 或 `Encode/DecodeAsset` → 通用 encode/decode → domain Validate。小 JSON 可以驻内存；这里的大小限制不是图片读取策略。

| 精确符号 | 作用、输入与输出 | 副作用、错误与设计理由 |
| --- | --- | --- |
| `corrupt` | 将底层格式问题包装为 `ErrRepositoryCorrupt`。 | `%w` 保留领域错误，内部原始原因以文本呈现；便于统一拒绝损坏快照。 |
| `decode` | 对给定 max 的 byte slice 严格解码单个 JSON 值到目标。 | 拒绝超限、未知字段、尾随 JSON；不跳过坏字段构造部分对象。 |
| `encode` | JSON 编码并追加换行。 | 无 IO；稳定字段结构来自 Go struct，而不是 map。 |
| `ValidateManifest` | 仅接受 format v1、`sha256`、非零 RepositoryID。 | 未知版本作为 corruption 失败；没有自动迁移或重写。 |
| `EncodeManifest` | 验证后输出 manifest JSON bytes。 | 无 IO；初始化和 SQL 保存共享同一编码。 |
| `DecodeManifest` | 以 64 KiB 上限解码并验证 manifest。 | 超限/不支持格式包装 corruption。 |
| `EncodeAsset` | 验证 Asset；时间转 UTC；nil tags/collections 编成 `[]`；输出带换行 JSON。 | 不改调用者 struct 本身；输出超过 1 MiB 为 corruption。planner 与 index 使用同一编码比较最终 metadata。 |
| `DecodeAsset` | 以 1 MiB 上限解码，验证字段和 `<ID>.json` 文件名，补齐空数组。 | ID/文件名不符直接 corruption；不会按文件名重新赋予身份。 |

### [`path.go`](../internal/repository/path.go)、[`location.go`](../internal/repository/location.go)

| 精确符号 | 作用、输入与输出 | 副作用、错误与设计理由 |
| --- | --- | --- |
| `AssetPath` | AssetID → `.touko/assets/<uuid>.json`。 | 纯函数；元数据路径与标题、原文件名无关。 |
| `BuildObjectPath` | namespace/object → `objects/<namespace>/sha256/<前2位>/<后2位>/<hash>.<ext>`。 | 纯拼接，调用者应先验证 namespace/object；同 namespace 同 bytes 共 placement，跨 namespace 分开路由。 |
| `(Location).Validate` | 检查非空安全 branch、支持 backend 和 backend-specific 字段。 | localgit 必须绝对 path 且无 owner/name；github 必须 owner/name 且无 path。这里不是完整 Git branch validator，adapter 继续验证；不保存 token。 |

### [`snapshot.go`](../internal/repository/snapshot.go)

`ReadSnapshot` 是 sync、commit、verify、candidate recovery 的共同入口。它先确定 commit/tree，之后只按固定 ID 读取，避免一次扫描混进两个分支版本。

| 精确符号 | 作用、输入与输出 | 副作用、错误与设计理由 |
| --- | --- | --- |
| `readMetadata` | Reader、tree entry、上限 → metadata bytes。 | 必须是 `100644/blob`、大小合法；限量读并 Close，校验实际长度等于 tree size。读取/关闭失败向上传递。 |
| `ReadSnapshot` | Reader、固定 HEAD → 包含 manifest、Assets、tree、完整 Entries 的 Snapshot。 | 会读 Git metadata，不下载全部图片。拒绝不安全/重复路径、半个 `.touko`、坏 metadata、重复 Asset ID、同 hash 描述冲突、缺失或 mode/type/size 不符的 canonical object。空 HEAD/无 manifest 为 uninitialized；失败时不写 SQLite。 |

### [`planner.go`](../internal/repository/planner.go)

关键思路是“先求最终 Asset 集合，再算 placement 引用差”，不是按照 changes 顺序边删边加。输入/输出都在内存里，无 clock、随机数、文件、网络或 SQL IO。

| 精确符号 | 作用、输入与输出 | 副作用、错误与设计理由 |
| --- | --- | --- |
| `(CommitPlan).Empty` | 判断 metadata writes/new objects/deletions 是否都空。 | reused objects 本身不是一次写入；空计划不值得创建 commit。 |
| `placements` | Asset map → 按完整 canonical path 去重的 ObjectPlacement map。 | 验证 map key 与 Asset.ID、Asset 描述及同 hash 描述一致；不是只按 hash 计引用，因此不会误删另一 namespace 的副本。也由 VerifySnapshot 复用。 |
| `PlanCommit` | base Snapshot + normalized ChangeSet → CommitPlan。 | stale base 先 conflict；拒绝重复操作、非法 add/update/delete、缺失或不匹配 metadata。复制最终 Asset map，不改 base。只输出变化 metadata；最后引用消失才删 binary，delete+same-bytes-add 保留 placement；已有 canonical path 不允许被不相容条目占用。只删除本次移除的 base 引用，不清未知 orphan；所有输出列表按 path 排序。 |

`NewObjects` 后续从 stage 上传；`ReusedObjects` 后续必须验证已有远端 bytes，不能仅凭路径相信 hash；`Deletions` 同时包含应删 metadata 与最后引用的 object path。`FinalAssets` 让发布后校验能验证整个最终图库，而不是只数行数。

### [`verify.go`](../internal/repository/verify.go)

| 精确符号 | 作用、输入与输出 | 副作用、错误与设计理由 |
| --- | --- | --- |
| `(contextReader).Read` | 先检查 ctx，再从底层 reader 读。 | 为流式验证增加取消检查；底层阻塞 IO 仍依赖 adapter 的 context/Close。 |
| `(*countedHash).Write` | 写入 hash 并累计实际字节数。 | 修改局部摘要状态，不保存 bytes。 |
| `VerifyObject` | 固定 BlobID 与预期 ContentObject → error。 | OpenBlob 后通过 TeeReader 同时计算 SHA-256/size，DecodeConfig 读取图片 header，再 drain 剩余数据，最后 Close。size/hash 不符为 `ErrHashMismatch`；格式/尺寸不符为 corruption。最多读预期 size+1，避免错误对象无限流入。 |
| `VerifySnapshot` | Snapshot → `VerifyResult{Head, Assets, Objects}`。 | 验证每个 unique placement；相同 Git BlobID 已验且描述一致时可复用结果。Objects 数的是物理 placement，不是 Git blob 数。逐个流式处理，不修改远端或本地 stage。 |

## 4. `internal/image` 与 `internal/staging`：从输入到可靠 payload

### [`image/ingest.go`](../internal/image/ingest.go)

| 精确符号 | 作用、输入与输出 | 副作用、错误与设计理由 |
| --- | --- | --- |
| `Ingest` | ctx、source、tempDir → `Result{Object, TempPath}`。 | source 只读一次，经 MultiWriter 同时写临时文件和 SHA-256；读到第 100000001 字节即可拒绝超限。Sync/Close 后从临时文件重新读 header，不解码完整图片；扩展名来自真实 PNG/JPEG/GIF 格式。空输入、格式错误、取消、IO 失败均移除临时文件；成功后临时文件由调用者接管，source 始终由调用者管理。 |
| `(contextReader).Read` | 读取前后检查 ctx，将取消传播给复制/DecodeConfig。 | 不把读完时已取消的输入视作成功；不自行 Close source。 |

### [`staging/files.go`](../internal/staging/files.go)

调用链：`Service.Add` 调用 Ingest → Install；取消 add、rm、reset 或发布成功后由 `Service.cleanup` → Cleanup 释放无引用 payload。文件安装与 SQL 提交不是同一个事务，因此 app 层必须以旧引用集合补偿失败。

| 精确符号 | 作用、输入与输出 | 副作用、错误与设计理由 |
| --- | --- | --- |
| `PayloadPath` | stateDir、namespace、object → 尽可能绝对化的 `stage/objects/<ns>/<hash>.<ext>` 路径。 | 无文件写入；这是本地 payload 路径，不是远程 canonical object path。 |
| `Install` | Ingest Result → `(path, created, error)`。 | 调用者必须持有操作锁。验证 namespace、描述和 temp bytes；目录 0700、payload 0600；已有目的文件先验证 size/hash，不覆盖不同 bytes；新文件原子 Rename。通常消费并删除 temp，成功 rename 后由目标持有 bytes；无法解析 temp 绝对路径或发现 source 就是 destination 时先拒绝，不将 destination 删除。created=false 表示复用已存在 payload。 |
| `verifyFile` | 文件路径、对象描述、是否 sync → error。 | Lstat 拒绝非 regular file/尺寸不符；限量流式 SHA-256 校验。syncFile=true 时以读写方式打开、chmod 0600、Sync 后 Close；hash/size 错误为 `ErrHashMismatch`。Install 用它验证新旧文件。 |
| `Cleanup` | stateDir、绝对 payload 引用集合 → error。 | 只遍历 `stage/objects`，只删除无引用 regular files；不触碰 `.lock`、config、DB、ingest tmp，不跟随文件符号链接去删其他区域。目录不存在视为无事可做；取消/删除失败向上传递。 |
| `(contextReader).Read` | 读取前后检查取消。 | 供 verifyFile 使用；避免清理/验证在取消后继续长时间计算。 |

## 5. `internal/index`：SQLite 持久状态与事务边界

### [`store.go`](../internal/index/store.go)

`State.Assets` 与 Head/Tree/Manifest 是 committed cache；`Changes`、BaseHead、Publication 是本机未发布工作与恢复资料。它们虽然在同一 DB 中，但**不能把整个 DB 都叫作可以随意删除的 cache**。删除 DB 只能从 Git 恢复已提交状态，未发布 stage/journal 不保证可恢复。

| 精确符号 | 作用、输入与输出 | 副作用、错误与设计理由 |
| --- | --- | --- |
| `Open` | ctx、stateDir → `*Store`。 | 创建/收紧 state 目录 0700、index.db 0600；用 file URL 打开纯 Go SQLite；只允许一个打开连接和一个 idle 连接；初始化失败关闭 DB。单连接使 PRAGMA 和事务使用同一连接约束。 |
| `(*Store).initialize` | 读取 user_version，准备 schema。 | 只接受 0/1；设置 WAL、foreign_keys；version=0 在事务里建表并设 version=1，失败 rollback。不是对未知 schema 自动迁移。 |
| `(*Store).Close` | 关闭 `sql.DB`。 | 由 Service.Close/CLI.Run 结束一次调用时释放连接。 |
| `corrupt` | 格式化 index 状态错误并包装 `ErrRepositoryCorrupt`。 | 与 repository 包的同名 helper 分属不同包。 |
| `(*Store).Load` | 在一个只读事务里构建完整 `State`。 | 初始化非 nil maps，读取 singleton、manifest、assets、changes、可选 journal；所有 decode/SQL/Commit 错误均返回失败而非半份 State。事务保证一次 Load 不混读两个本地状态。 |
| `loadAssets` | 当前事务、目标 Asset map → error。 | 逐行读取 `id,metadata`，借 DecodeAsset 验证主键/JSON ID 对应；关闭 rows 并检查 rows.Err，不跳过坏记录。由 Load 调用。 |
| `loadChanges` | 当前事务、目标 Change map → error。 | 严格解析 UUID/kind；add/update 解完整 Asset；delete 必须 SQL NULL metadata；未知 kind 或不合法行按 corruption 失败。由 Load 调用。 |
| `(*Store).Save` | 完整 State → error。 | 一个写事务先编码 manifest，替换 assets/changes，写 journal，再 UPDATE singleton 的 head/tree/manifest/base；验证 map key、Change.AssetID 与 metadata ID，检查 singleton 恰有一行。任一 SQL/编码/Commit 失败全部 rollback；绝不会留下删了一半的 gallery 或清了一半的 stage。 |
| `(*Store).SavePublication` | `*Publication` 或 nil → error。 | 独立短事务只替换 journal，不动 assets/changes/head；coordinator 必须在 UpdateRef **之前**成功调用它，保留“远端可能已发布”的 durable 证据。nil 用于明确清 journal。 |
| `savePublication` | 已有事务与可选 journal → error。 | DELETE 单例再按需 INSERT；由 Save 和 SavePublication 共用，复用同一原子写法。 |

### 为什么 Save 与 SavePublication 不能合并成一个普通“保存”调用

1. 创建 candidate 只生成未引用的 Git commit；这时本地 gallery 还必须留在旧 HEAD。
2. `SavePublication` 成功后才允许 UpdateRef；进程随后崩溃，journal 仍知道应检查哪个 candidate。
3. 发布确定后，`Save(snapshotState(..., true))` 原子推进 cache，同时清 Changes/BaseHead/Publication。
4. 如果第 3 步失败，SQLite rollback 保留旧 stage/journal，而远端 commit 可能已经存在；重跑 sync 从 journal 恢复，不重复发布。

## 6. `internal/app`：跨 Git、SQL 和文件的协调者

### [`state.go`](../internal/app/state.go)

`Service` 只持有 state-dir 与 Store，不持有 GUI/HTTP 框架。CLI 负责选择 adapter，业务方法接收 `repository.Git`。

| 精确符号 | 作用、输入与输出 | 副作用、错误与设计理由 |
| --- | --- | --- |
| `StateDir` | 显式目录 → TOUKO_HOME → XDG_DATA_HOME/touko → HOME/.local/share/touko，返回绝对路径。 | 读取环境/用户 HOME，不创建目录；保持每台设备状态隔离。 |
| `Open` | ctx、dir → Service。 | 绝对化并创建/收紧目录，再 index.Open；失败不返回半初始化 Service。 |
| `(*Service).Close` | 关闭 Store。 | 由 CLI 的一次调用生命周期管理。 |
| `(*Service).Lock` | ctx → unlock 函数或 error。 | 创建稳定 `.lock` 文件并 TryLock；占用立即 `ErrBusy`。不删除 lock 文件，不判断“过期锁”，不把本地锁冒充远端分布式锁。返回的释放函数只 Unlock。 |
| `(*Service).Location` | 读取 `config.json` → 验证后的 Location。 | 无选择时提示 repo init/use；不查询远端，不读取 token。 |
| `(*Service).Select` | Location → error。 | 获取本地操作锁后调用 selectLocked；不写远端。 |
| `(*Service).selectLocked` | 已持锁时验证并保存选择。 | 同 Location 幂等；已有其他 config 或 snapshot/stage/journal 则拒绝混用 state-dir。临时文件写 JSON、Sync/Close 后 Rename，不覆盖损坏 config 来掩盖问题。 |

### [`stage.go`](../internal/app/stage.go)

`Input.Reader` 所有权在调用者；`AddOptions.Title` 和 EditOptions 的指针区分“没提供”和“明确设置空值”。stage 存最终 normalized change，不保存每次按键历史。

| 精确符号 | 作用、输入与输出 | 副作用、错误与设计理由 |
| --- | --- | --- |
| `normalize` | tags/collections → trim、去空、排序、去重后的新 slice。 | 不改变大小写；让重复编辑/比较结果稳定。 |
| `mutable` | 检查 State 是否允许 stage mutation。 | 有未解 publication 返回 conflict；没有 committed manifest/head 提示先 sync。禁止一边恢复旧候选一边修改其 durable stage。 |
| `bindBase` | 按 Changes 更新 State.BaseHead。 | 空 stage 清 base；第一条 change 绑定当前 Head，后续 change 不漂移 base。只改内存。 |
| `(*Service).cleanup` | State → 清理无 staged 引用 payload。 | 从带 Asset 的 changes 构造绝对 PayloadPath 集合，再调用 staging.Cleanup；共享 payload 被任一 staged 引用时保留。 |
| `(*Service).Add` | 一批 Input 与 AddOptions → added AssetView 列表。 | 持锁、Load/mutable、逐个 Ingest/Install、新 UUID/UTC 时间、规范 tags/collections，整批最后只 Save 一次。任一输入/SQL 失败不留下本批 stage rows；defer 按旧引用用 WithoutCancel context 尝试清理新 payload，清理错误不替代原始失败。相同 bytes 新增独立 ID、共享同 placement payload。 |
| `union` | base Assets + staged adds → selector 候选 map。 | 刻意保留 staged-delete 的 base ID；供 Edit/Remove 区分 delete→edit 冲突和重复 rm 幂等，而非将两者都误报 not found。 |
| `overlay` | State → 按 CreatedAt/ID 排序的 AssetView 列表。 | 在 union 上应用 update/delete，隐藏已删记录，标 `added/modified/committed`；纯内存视图，不移动 base。 |
| `(*Service).List` | Load 后返回 overlay。 | 只读 SQLite；CLI ls/find 使用，不隐式 sync。 |
| `(*Service).Show` | selector → 一个可见 AssetView。 | 在 overlay 的可见集合中 ResolveAsset；staged-delete 不可见，不按文件名选。 |
| `(*Service).Edit` | selector 与可选 title/tags/collections → error。 | 持锁后在 union 解析；已 delete 返回 `delete_modify`。未提供字段保留、真实字段不变直接返回；真实变化更新 UpdatedAt，add→edit 仍为 add，其余为 update；一次事务保存。仅支持 metadata edit，不替换图片或移动 namespace。 |
| `(*Service).Remove` | 多 selectors → error。 | 先把全部 selector 成功解析，再改 map/Save；去重同批 ID，add→rm 取消 add，committed/update→delete，重复已 delete 仍 delete。Save 后 cleanup；清理失败时 SQL mutation 已成功，不能误认为 stage 未变。 |

### [`sync.go`](../internal/app/sync.go)

| 精确符号 | 作用、输入与输出 | 副作用、错误与设计理由 |
| --- | --- | --- |
| `sameRepository` | 本地 State 与 remote Snapshot → identity 校验结果。 | 本地已有 manifest 且 RepositoryID 不同时 conflict；不悄悄把一台设备切到另一图库。 |
| `snapshotState` | 旧 State、Snapshot、clear → 新 State 值。 | 替换 committed Head/Tree/Manifest/Assets；clear=true 还清 Changes/BaseHead/Publication。自身不写 SQL。 |
| `(*Service).Sync` | Git adapter → SyncResult。 | 持锁后调用 syncLocked，避免与 staging mutation 并发。 |
| `(*Service).syncLocked` | 已持锁时加载状态、恢复 journal 或扫描远端。 | journal 优先走 recoverPublication(retry=false)；无 journal 且 stage 非空、HEAD 已变则 conflict，保留旧状态。扫描/identity 全通过才 Save；HEAD 未变可保留 stage。普通 sync 不下载全部图片。 |

### [`repo.go`](../internal/app/repo.go)

| 精确符号 | 作用、输入与输出 | 副作用、错误与设计理由 |
| --- | --- | --- |
| `reservedPath` | 路径 → 是否属于 `.touko`/`objects` 保留区域。 | Initialize 与 candidate 初始化验证共用；不是清理规则。 |
| `(*Service).Initialize` | Git adapter → SyncResult。 | 持锁；已有 journal 则恢复/重试；已有合法 TOUKO 则 sync、无额外 commit。无格式时拒绝半损坏/被占用的保留路径；生成 manifest blob、base tree candidate 和 journal 后走 publishJournal。保留已有 README 等非 TOUKO 文件。GitHub 空库限制由 adapter 明确拒绝。 |
| `(*Service).Reset` | Git adapter → error；显式丢弃本地未发布工作。 | 持锁。有 journal 先查 publicationRelation，查询失败保留全部并报 outcome unknown；已发布则 finishPublication，不把发布误丢。确认不属已发布后清 stage/base/journal，Save 后 cleanup；不更新远程 ref，也不自动 sync 新远程。 |

### [`commit.go`](../internal/app/commit.go)：主流程和流式上传

| 精确符号 | 作用、输入与输出 | 副作用、错误与设计理由 |
| --- | --- | --- |
| `stagedChanges` | State.Changes map → 按 AssetID 排序的 ChangeSet，附 BaseHead。 | 纯转换；planner 和恢复重建使用相同输入次序。 |
| `(*Service).Commit` | ctx、Git adapter、message → CommitResult。 | 持锁；优先恢复 journal，空 stage 则 sync、不创建 commit。先比较 remote HEAD/stage base，远端删掉本机待 update 的 Asset 时报告 `delete_modify`，其他 stale base 为 `head_changed`，均不上传。读取 base/PlanCommit，验证 reuse，串行上传 objects/metadata，创建 tree/单 parent candidate，SavePublication 后 publishJournal。失败阶段不同，可能留下 detached Git 对象或 journal，但不会把它们说成已发布。 |
| `(*stagedReader).Read` | reader → 读出 bytes，同时更新 digest/size。 | 检查 ctx；仅保留摘要和计数，不缓存图片。uploadStaged 用它确认 adapter 真正消费的流。 |
| `(*Service).uploadStaged` | ObjectPlacement → BlobID。 | 打开本地 PayloadPath，先查 size，再限量流式 CreateBlob，Close 后比 SHA-256/size；磁盘损坏在 UpdateRef 前失败。失败可能已创建不可达 blob，但 stage rows 和远端分支不因此推进。 |

### 同文件：publication journal 的恢复状态机

publication 的三个字段是 `BaseHead`、`CandidateHead`、`CandidateTree`。它不是“最后一次请求成功”的布尔值，而是跨进程恢复所需的不可变候选证据。

| 精确符号 | 作用、输入与输出 | 失败时保留什么、为何存在 |
| --- | --- | --- |
| `publicationRelation` | 读当前 HEAD；等于 candidate 为 published，等于 base 为未发布；其余非空 HEAD 用 IsAncestor(candidate, head) 判断。返回 `(head, published, error)`。 | 只有成功祖先查询才能证明后代已包含 candidate；网络/adapter 错误原样返回，不能删 journal。空 HEAD 不是已发布。 |
| `unknownPublication` | journal 与底层 error → 含 candidate ID、`run sync` 的包装错误。 | 保留底层错误链；告诉用户结果未知，不宣称远端失败，不鼓励盲目重传。 |
| `(*Service).recoverPublication` | State、retry → SyncResult。 | 已发布走 finishPublication；HEAD 既非 base 又无 candidate 祖先关系则 conflict 且保留 stage/journal。HEAD 仍 base：retry=true（commit/init）重用原 candidate 发布；retry=false（sync）只清 journal、保留 stage，不新建第二个 commit。 |
| `(*Service).publishJournal` | durable journal 对应 State → 发布/收尾结果。 | 先 validateCandidate，再复读 HEAD；若不再是 base，转 recovery，绝不盲目写 ref。UpdateRef 报错后 read-back：已 candidate/后代则照常 finalize；关系查询失败为 unknown；其他 HEAD 为 conflict；仍 base 返回原发布错误。journal 在成功收尾前不丢。 |
| `(*Service).finishPublication` | 已被证明发布的候选 → SyncResult。 | validateCandidate 后 finalizeCandidate，不再次 UpdateRef；处理超时丢响应、进程退出或 SQLite 落账失败。 |
| `(*Service).finalizeCandidate` | State 与已验 candidate Snapshot → 最新有效 SyncResult。 | 再证 candidate 可见；HEAD 为后代则重新 ReadSnapshot 后代并校验身份。Save 一次更新 committed cache 并清 stage/journal，之后 cleanup。SQL 失败保留旧 stage/journal且报 `published head ... local index update failed`；cleanup 失败时 DB 已前进，报 published head 与本地清理错误，不伪装为远端没提交。 |
| `candidateCorrupt` | 失败原因 → 带 publication candidate 上下文的 `ErrRepositoryCorrupt`。 | 与普通 head conflict 分开：候选本身不符合 durable 意图，不可重试发布。 |
| `validateCandidate` | Reader、含 journal 的 State → 完整验证过的候选 Snapshot。 | 只读 Git，不改 SQL/ref；验证失败保留 stage/journal，阻止发布或清账。不是只比 Asset 数；详见下节。 |

#### `validateCandidate` 逐段理解

1. **绑定 journal 与 commit**：按 CandidateHead 读 Snapshot，tree 必须等于 CandidateTree；root 候选不能有 parent，其他候选必须只有 BaseHead 这一个 parent；检查 RepositoryID。
2. **恢复初始化意图**：本地无 manifest、无 changes 时，candidate 必须无 Asset。读取已有 base tree（若有），不允许 base 已占 `.touko`/`objects`；唯一计划 metadata write 是编码后的 manifest。
3. **恢复普通 batch 意图**：staging BaseHead 必须等于 journal BaseHead。重新读取 immutable base，再用 durable stage 调 PlanCommit，逐 Asset 比较最终集合和编码 metadata。重新计划比信任一次进程内临时 plan 更可靠。
4. **构造预期完整 leaf tree**：从 base 的非 tree 条目出发，应用计划删除/metadata/object；目录 tree ID 会因后代变化而变，因此不拿祖先目录 SHA 当独立用户内容比较。
5. **核对写入 bytes 与复用对象**：metadata blob 必须 mode/type/size 正确且 bytes 完全相等；objects 必须在应在的 canonical path，复用 blob 不能换 SHA，每个新增/复用 placement 再 VerifyObject。
6. **拒绝所有计划外差异**：实际 leaf 数量和每个 GitEntry 必须与预期相等；已删除 binary 不能残留，无关 README、symlink、executable、gitlink 等不能被悄悄改动。

#### 用失败位置理解状态保留

| 失败位置 | 远程可见状态 | 本地可恢复依据 |
| --- | --- | --- |
| stage/计划/上传/tree/commit 创建时 | UpdateRef 尚未执行，分支仍旧版本；可能有未引用 Git 对象。 | 原 committed cache 与 stage 保留。 |
| SavePublication 时 | 不允许接着 UpdateRef。 | stage 保留；失败事务不产生半个 journal。 |
| UpdateRef 请求失败或超时 | 必须 read-back；不能只看请求 error 判断结果。 | durable journal 保留，直到已发布收尾或明确放弃未发布 candidate。 |
| 已发布后 SQLite Save 失败 | candidate 或合法后代已可见。 | 旧 stage/journal 仍在，sync 校验后补账。 |
| 已落账后 payload cleanup 失败 | 已发布，DB 已更新。 | 返回 published head 与清理错误；不应重新发布同一 batch。 |

### [`verify.go`](../internal/app/verify.go)

| 精确符号 | 作用、输入与输出 | 副作用、错误与设计理由 |
| --- | --- | --- |
| `(*Service).Verify` | 读当前 HEAD，固定 Snapshot，再 VerifySnapshot → VerifyResult。 | 校验远端 committed state，不使用 staged overlay，也不清 stage、写 cache 或改远端；结果只证明那个固定 HEAD，不声称清除历史或全球 CDN 缓存。 |

## 7. `internal/localgit`：真实 Git 子进程 adapter

### [`repository.go`](../internal/localgit/repository.go)：构造、环境和输出边界

| 精确符号 | 作用、输入与输出 | 副作用、错误与设计理由 |
| --- | --- | --- |
| `Open` | path/branch → Repository。 | 绝对化、验证 branch、调用 Git 确认 sha1 object format 与真实 git-dir；不创建/改动仓库。不偷偷支持 Git SHA-256 对象格式。 |
| `Init` | path/branch → 新 bare Repository。 | 创建前先校验 branch；拒绝非目录、非空目录；运行 `git init --bare --object-format=sha1 --initial-branch=...` 后 Open。Git 创建失败不擅自删除目标内容。 |
| `(*Repository).validateBranch` | 用 `check-ref-format --branch` 检查 branch。 | 拒绝空 branch 与 Git 自动展开 shorthand 后不同于输入的名字；避免路径/选项语义混淆。 |
| `gitEnvironment` | 返回隔离的子进程环境。 | 过滤继承 `GIT_*`；禁 system/global config、交互提示，设置 TOUKO 固定 author/committer。不修改用户全局配置。 |
| `command` | ctx、可选 path、独立 argv → `*exec.Cmd`。 | `exec.CommandContext`，不用 shell 拼接；加入 `--no-replace-objects`、可选 `-C`，装配隔离环境；本身尚未启动进程。 |
| `(*cappedOutput).Write` | 把小型 Git metadata stdout 写 Buffer。 | 超过 32 MiB 返回错误，不用于图片 blob；给异常 metadata 输出设置上界。 |
| `(*errorOutput).Write` | 保留最多 64 KiB stderr。 | 返回原输入长度，超出部分丢弃而不是让 child 因 stderr writer 错误中断；避免无限错误输出占内存。 |
| `runGit` | ctx、path、stdin、argv → 小型 stdout bytes。 | command 后 `cmd.Run` 完整启动/等待/回收；取消优先返回 ctx.Err，其余错误带命令名与受限 stderr。 |
| `(*Repository).run` | 将 Repository.path 绑定到 runGit。 | 不是额外发布层；统一所有短命令执行环境。 |
| `validateID` | 输入 string → 校验错误或 nil。 | 要求 40 位小写 hex、非全零；避免把用户文本当任意 Git revision 表达式。 |
| `parseID` | Git stdout → trim 后 ID 与校验结果。 | 所有创建/解析命令复用同一 ID 检查。 |

### 同文件：读端口与 blob 子进程生命周期

| 精确符号 | 作用、输入与输出 | 副作用、错误与设计理由 |
| --- | --- | --- |
| `(*Repository).Head` | 查 `refs/heads/<branch>` → commit HEAD。 | `show-ref --exists` 的 exit 2 才视为不存在；其他查找错误不伪装空库，再 `rev-parse ...^{commit}` 确认 commit。 |
| `(*Repository).Commit` | 固定 HEAD → tree 与所有 parents。 | `cat-file commit` 解析 header，验证每个 ID，拒绝多个/缺失 tree；不读可变分支。 |
| `(*Repository).Tree` | 固定 tree → 递归含目录的 GitEntry 列表。 | `ls-tree -r -t -z -l`，NUL 分隔安全保留路径；检查记录结束符、header、SHA、size。不是按换行拆文件名。 |
| `(*Repository).OpenBlob` | BlobID → 流式 `blobReader`。 | `cat-file blob` 走 StdoutPipe/Start；Start 失败关闭 pipe。返回后由读取者负责 Close，不能用 runGit 将整图缓冲。 |
| `(*blobReader).wait` | 等待 Git 并保存结果。 | sync.Once 保证只 Wait 一次；取消返回 ctx.Err，子进程失败附受限 stderr；EOF 与 Close 共用回收逻辑。 |
| `(*blobReader).Read` | 从 stdout pipe 读 bytes。 | EOF 时标记已读尽并 wait，确保 child 退出错误不被 EOF 吃掉；其他 pipe error 触发 Close 回收。 |
| `(*blobReader).Close` | 关闭流，必要时 kill child，始终 wait。 | sync.Once 幂等；早停流时 kill 是正常清理，不把主动放弃读当失败；已 EOF 或 ctx 取消时保留等待错误。防止只关 pipe 不 reap 的僵尸进程。 |
| `(*Repository).IsAncestor` | 两个 HEAD → bool/error。 | `merge-base --is-ancestor`：exit 0=true，exit 1=false，其余是错误；恢复流程不能把调用失败误当无祖先关系。 |

生命周期要点：短命令用 Run 自带 Wait；大 blob 用 Start/pipe，完整读到 EOF 时 Wait，提前 Close 时 Kill+Wait。ctx 取消可终止 Git，但使用 OpenBlob 的调用者仍应 Close 释放 pipe 和回收状态。

### 同文件：写端口与 base-tree 保留

| 精确符号 | 作用、输入与输出 | 副作用、错误与设计理由 |
| --- | --- | --- |
| `(*Repository).CreateBlob` | reader、size → BlobID。 | `hash-object -w --stdin` 流式写；最多读 size+1 并核对真实消费长度，负 size 拒绝、加一防溢出。错误可能留下不可达 blob，但不改 ref。 |
| `directoryNode` | 创建空的内存 treeNode。 | mode=`040000`、kind=`tree`，供 CreateTree 构造/新增层级。 |
| `validatePath` | tree change path → error。 | 拒绝 NUL、反斜线、空段、`.`、`..`、大小写等价 `.git`，避免不安全树写入。 |
| `(*Repository).CreateTree` | base tree 与 changes → TreeID。 | 先拒绝重复/父子重叠 paths；载入 base hierarchy，保留既有 mode/type/SHA。仅给变更祖先标 dirty；新增固定 100644/blob，nil 删除，缺失删除幂等；拒绝穿过非目录/替换目录；删最后子项移除空目录，不清无关路径。最后 writeTree，不移动分支。 |
| `(*Repository).writeTree` | 内存 treeNode → TreeID。 | 未 dirty 且有 SHA 直接复用；递归写变化子树，按名字排序，NUL 记录送 `mktree -z`。保留无关 symlink/executable/gitlink 的 mode/type，而不是重写整个仓库为普通文件。 |
| `(*Repository).CreateCommit` | tree、可空 parent、message → candidate HEAD。 | `commit-tree` 创建 detached commit；禁签名，message 从 stdin 传入，不作为选项；parent 非空只有一个 `-p`。仍不发布。 |
| `(*Repository).UpdateRef` | expected/next → error。 | `update-ref refs/heads/... next expected` 是本地原子 CAS；root 用全零 expected 表示必须尚不存在。失败后读取实际 HEAD，确实变化才转 `ConflictError`，取消保留原错误；不会 force 覆盖并发提交。 |

## 8. `internal/github`：真实 Git Data API adapter

### [`client.go`](../internal/github/client.go)：认证、传输、错误脱敏

凭据只在 Repository 实例内，CLI 从 `GITHUB_TOKEN` 读取；不进 config、SQLite、metadata、URL 或 argv。API metadata JSON 可限量驻内存；图片走独立 raw/pipe 通道。

| 精确符号 | 作用、输入与输出 | 副作用、错误与设计理由 |
| --- | --- | --- |
| `(*APIError).Error` | 格式化 Endpoint、Status、受限 Message。 | 不保留原 request/Authorization；供用户看到 HTTP 分类与上下文。 |
| `(*APIError).APIStatus` | 返回 HTTP 状态码。 | CLI 用小接口分类为 api_error，不要求 CLI 依赖完整错误结构。 |
| `(*APIError).Unwrap` | unauthorized=true 时返回 `ErrUnauthorized`，否则 nil。 | 401 与非限流 403 走权限错误；限流不是“token 失效”。 |
| `(*redactedError).Error`、`(*redactedError).Unwrap` | 显示脱敏 message，同时保留原 cause 错误链。 | `errors.Is/As` 仍能判断取消/传输错误；对外打印应打印包装错误，不另行 dump cause/request。 |
| `New` | Location/token → production Repository。 | 固定 `https://api.github.com` 与 45 秒 HTTP timeout；CLI 不能借任意 API URL 把 token 发走。 |
| `NewWithClient` | Location/token/API URL/HTTP client → Repository。 | 测试注入入口；校验 endpoint/凭据无控制字符；复制 client，补有限 timeout，禁 redirect，避免转发 bearer 或重放上传。不会改调用者 client。 |
| `validLocation` | 在 Location.Validate 之上检查 github backend、owner/name 和完整 branch 约束。 | adapter 级校验不影响其他 backend。 |
| `validName` | owner/repo 文本 → bool。 | 只允许 ASCII 字母数字及 `-_.`，拒绝空/点段；用于路径编码前。 |
| `validBranch` | branch 文本 → bool。 | 拒绝控制符、Git 保留语法、空段、点开头段、`.lock` 等；允许正常带 `/` 的分支。 |
| `(*Repository).redact` | 将当前 token 在文本中替换为 `[redacted]`。 | 无日志或持久化；错误显示的最后防线之一。 |
| `(*Repository).safeError` | error → 脱敏包装，nil/EOF 原样返回。 | 保持读取 EOF 语义和底层错误链；用于 transport、blob body 和 JSON 错误。 |
| `(*Repository).path` | Location → `/repos/<owner>/<name>`。 | 分别 PathEscape，避免任意路径拼接。 |
| `escapeBranch` | 逐分段编码 branch，再用 `/` 连接。 | 用于 Git ref endpoint；与 delivery 把 branch 作为 URL 组件编码不是同一用途。 |
| `(*Repository).writeAllowed` | 检查实例 token 非空。 | 无 token 的写入或 User 查询返回 `ErrUnauthorized`；public 读取不强制 token。 |
| `(*Repository).request` | ctx/method/endpoint/Accept/body/size → 成功 HTTP response。 | 设置 Bearer、JSON Content-Type/Length、API version `2026-03-10`；非 2xx 关闭 body，最多读 64 KiB 错误响应、message 最多 4096 bytes并脱敏。用状态/header/message 区分 rate limit 与权限；无隐藏重试。成功 body 交由上层关闭。 |
| `(*Repository).json` | 小 JSON input/output 与 HTTP 请求参数 → error。 | 编码 input，调用 request，defer Close；output=nil drain body，否则受限 64 MiB 解码并拒绝尾随/过大 JSON。只用于 API metadata，不用于图片 base64。 |
| `(*Repository).Discover` | owner/name/可选 branch → 已确认 Location。 | GET repo 得 default_branch/private；未指定 branch 用默认值；临时实例校验该 ref 的 Head。只读网络，不更改当前 Repository.location 或远端。repo use 将结果保存本地。 |
| `(*Repository).User` | 使用 token GET `/user` → login。 | token 缺失、权限失败或响应无 login 均失败；只返回登录名，不显示 token，不实现 OAuth login。 |

### [`repository.go`](../internal/github/repository.go)：读 Git 数据

| 精确符号 | 作用、输入与输出 | 副作用、错误与设计理由 |
| --- | --- | --- |
| `validID` | 文本 → 是否 40 位小写 hex。 | 本地判断，无网络；这里只描述语法，不查询对象存在。 |
| `checkID` | 校验调用者提供的 Git ID。 | 输入不合法返回普通参数错误，避免将 revision 表达式送 endpoint。 |
| `responseID` | 校验 GitHub 返回的 Git ID。 | 形状不合法包装 `ErrRepositoryCorrupt`，与本机输入错误区分。 |
| `(*Repository).Head` | GET ref → branch commit HEAD。 | 验证 ref type/ID；404/409 加“空库/分支不存在，请先 README 初始化”的说明，保留 APIError 链；不假造空 root。 |
| `(*Repository).Commit` | GET 固定 commit → tree/parents。 | 验证所有返回 ID；不在每次 blob 请求重新读 branch。 |
| `(*Repository).Tree` | GET `git/trees/<id>?recursive=1` → Entries。 | `truncated:true` 直接 corruption，拒绝不完整快照，不能更新一半 SQLite；验证返回 SHA。 |
| `(*Repository).OpenBlob` | GET 固定 blob，raw Accept → io.ReadCloser。 | 不读取整张图 JSON/base64；调用者 Close HTTP body。 |
| `(*blobBody).Read`、`(*blobBody).Close` | 转发 body 读取/关闭并 safeError 包装错误。 | 避免 raw 流错误绕过 token 脱敏；不缓存 bytes。 |
| `(*Repository).IsAncestor` | GET compare `ancestor...head?per_page=1` → bool。 | 用 status 的 ahead/identical=true、behind/diverged=false，不依赖分页 commit 列表；未知 status 为 corruption。恢复需要的是祖先关系，不是“列表里看见了某 commit”。 |

### 同文件：流式上传、取消与发布

| 精确符号 | 作用、输入与输出 | 副作用、错误与设计理由 |
| --- | --- | --- |
| `(*Repository).CreateBlob` | source/声明 size → Git BlobID。 | 要 token，size 在 0–100000000；通过 io.Pipe+base64 encoder 流式组成 JSON，按 size 计算 Content-Length。只读声明字节再探一字节，拒绝短/长流。HTTP/取消错误会关闭 pipe，并对实现 io.Closer 的 source 主动 Close；成功保留 caller 的 source 所有权。等待编码 goroutine 退出后才返回，详见下节。 |
| `(*Repository).CreateTree` | base_tree 与 TreeChanges → TreeID。 | POST tree，保留 base 非 TOUKO 内容；mode/type 固定 100644/blob；删除 pointer=nil 编成真正 `"sha":null`，不能 omitempty。校验 path/ID，不更新 ref。 |
| `(*Repository).CreateCommit` | tree、非空单 parent、message → candidate HEAD。 | POST commits 的 parents 只有 base；空 parent 明确拒绝并提示先 README 初始化，不能用 Git Data API 创空库首 ref。 |
| `(*Repository).UpdateRef` | expected/next → error。 | 写前再次 Head==expected，再 PATCH ref `force:false`。409/422 后 read-back，仅 Head 实变才归 conflict，其他保留原 APIError。不会自动重试/绕保护。GitHub API 没有 expected-old-SHA 参数，对任意外部 force/reset race **不是严格 CAS**。 |

#### `CreateBlob` 为什么不只是 `go io.Copy` 加一个 HTTP 请求

- JSON 是常量 prefix + 增量 base64 + suffix；Content-Length 由 `prefix/suffix 长度 + ((size+2)/3)*4` 算出，不生成整张图片的 base64 字符串。
- encoder goroutine 可能阻塞在 **pipe 写入**，也可能阻塞在 **source.Read**。只关 pipe 无法中断后者。
- `uploadCtx` 除父 ctx 外再有 HTTP client timeout。`context.AfterFunc` 独立于 `Client.Do` 返回执行：关闭 pipe 两端，并用 `sync.OnceFunc` 最多关闭一次可关闭 source。这样不必等 Do 返回才能解除上传端阻塞。
- HTTP 提前拒绝后立即关闭 pipe reader；请求错误还会 closeSource，再等 `done`。成功 response 也不能忽略编码 goroutine 的短流/超量/encoder 错误。
- 清理 defer 若发现取消回调已开始，会等其 `aborted` channel 完成，避免函数返回后回调仍碰调用者状态。
- 通用 `io.Reader` 不能被 Go 强制打断任意阻塞 Read；能被主动关闭的 source 才有这条取消解除路径，其他 source 需要自身配合 ctx/返回。application 的 stagedReader 检查 ctx，读取常规本地文件；不要把接口解释成对所有自定义阻塞 Reader 的强制终止保证。

## 9. `internal/delivery`：由内容路径推导链接

### [`url.go`](../internal/delivery/url.go)

| 精确符号 | 作用、输入与输出 | 副作用、错误与设计理由 |
| --- | --- | --- |
| `BuildURL` | Location、canonical objectPath、Provider → URL string。 | 纯函数、无网络。仅 public github；local/private 返回 `ErrDeliveryUnavailable`。严格检查 namespace、sha256 路径六段、hash 分桶、扩展名，再安全编码 owner/name/branch/path；支持 jsdelivr 与 github raw，其他 provider 错误。不把 URL 写入 Asset，不证明对象已 commit 或 CDN 已传播。 |

同一 metadata 可以重新推导不同 provider 的 URL；换 delivery 不需要重写 committed Asset。URL 使用 branch，不是不可变 commit permalink；最新 HEAD 删除保证不等于 Git 历史抹除。

## 10. `internal/cli`：命令装配、输入约束和输出契约

Cobra 命令构造函数返回 `*cobra.Command`；真正业务发生在其匿名 RunE。下面把两者一起解释。CLI 不直接 UpdateRef，所有远端 mutation 都通过 Service coordinator。

### [`root.go`](../internal/cli/root.go)

| 精确符号 | 作用、输入与输出 | 副作用、错误与设计理由 |
| --- | --- | --- |
| `(*usageError).Error`、`(*usageError).Unwrap` | 显示并保留用法错误 cause。 | 将“输入用错”与 IO/domain 错误分开，最终 exit 2。 |
| `usage` | format/args → usageError。 | 各 handler 在 mutation 前返回参数冲突/缺失错误。 |
| `arguments` | Cobra PositionalArgs checker → 包装后的 checker。 | 把位置参数错误标成 usageError，不误报 io_error。 |
| `Run` | ctx、args、stdin/stdout/stderr → 退出码。 | 每次创建 runtime、执行 Cobra、关闭已开的 Service；错误只写 stderr，支持 JSON error envelope；未进入 handler 的解析错误归 usage。成功 0、usage 2、conflict 3，其余 1。不依赖全局 argv/streams，main 才取 os 资源。 |
| `requestsJSON` | 原始 args → bool。 | 解析提前失败时仍识别 `--json`/true/false；遇 `--` 停止，不把文件名当 flag。 |
| `errorCode` | error 链 → JSON code/exit。 | 先 usage，再 domain errors；APIStatus 接口错误为 api_error，其余 io_error。权限 APIError 可先被 errors.Is 归 unauthorized；不吞底层错误。 |
| `(*runtime).service` | ctx → 本次调用惰性打开并复用的 Service。 | StateDir→app.Open，只有需要本地状态的命令才开 DB；Run 最终关闭。 |
| `(*runtime).backend` | Service.Location → repository.Git adapter。 | localgit.Open 或 github.New；token 只从 GITHUB_TOKEN 读入实例。后端选择留在装配层，application 不导入 CLI。 |
| `(*runtime).encode` | 任意结果 → stdout 单个 JSON 值。 | 输出错误返回给 Run，不写调试日志。 |
| `(*runtime).command` | 创建 root 与全部命令树。 | 注册全局 state-dir/json，禁自动 completion 子命令；Cobra 只解析，业务在子命令；PersistentPreRun 标记已开始，统一用法错误包装。 |
| `(*runtime).printAssets` | AssetView 列表 → JSON envelope 或人类行。 | nil 规范为空数组；人类输出 ID/state/namespace/title/hash；不改变 stage。 |
| `(*runtime).statusCommand` | status → 本地 head/base/排序 changes。 | 只 Load，不隐式 sync；JSON 空 changes 为 []。人类删除条目的 metadata 从 committed Assets 取。 |
| `(*runtime).resetCommand` | 构造 reset；要求显式 `--staged`。 | 调 backend 与 Service.Reset 后输出 mutationResult；不是 Git reset，不动远端分支。 |
| `(*runtime).mutationResult` | mutation 成功 → JSON `{"ok":true}` 或无额外人类输出。 | 给 edit/rm/reset 共用；输出失败不回滚已完成业务。 |
| `matches` | AssetView、已小写 query → bool。 | title/original_name/tags/collections 做不区分大小写子串匹配；find handler 负责拒绝空 query，不支持 filename 身份选择或复杂查询语言。 |

### [`repo.go`](../internal/cli/repo.go)

| 精确符号 | 作用、输入与输出 | 副作用、错误与设计理由 |
| --- | --- | --- |
| `(*runtime).repoCommand` | 构造 repo/init/use 子树，RunE 装配仓库和 Service。 | init --local 先 localgit.Init/main 再 Select/Initialize；普通 init 对已选 adapter Initialize。use 二选一 `--local PATH` 或 `OWNER/REPO`；local 默认 main，github Discover 取 default_branch/private 并查 ref，再 Select。use 无远端写；选择冲突不会复用其他图库 stage。 |
| `(*runtime).syncCommand` | 构造 sync，打开 Service/backend 后调 Service.Sync。 | 成功 printSync；任何校验/恢复错误交 Run，不能输出假成功。 |
| `(*runtime).printSync` | SyncResult → JSON 或 HEAD/Assets 人类输出。 | 只负责结果呈现，不重新读取分支。 |

### [`assets.go`](../internal/cli/assets.go)

| 精确符号 | 作用、输入与输出 | 副作用、错误与设计理由 |
| --- | --- | --- |
| `(*runtime).addCommand` | 文件列表或单一 stdin `-` → stage，一般输出 AssetViews，可选 id/markdown。 | mutation 前拒绝 output/json 冲突、非法 output、stdin 混合和错误 --name 用法；打开文件由 handler defer Close。namespace 先验证；Markdown 在 Add 前检查 public github，否则不写 stage。Add 后 ID 每行一个；Markdown 用 BuildURL/markdownAlt，并在 stderr 标 staged/not published。输出失败时 stage 可能已保存，不是“add 没发生”。 |
| `(*runtime).editCommand` | SELECTOR 与 title/tag/collection flags → EditOptions。 | 用 Changed 与指针区分没提供/明确清空；clear-tags/clear-collections 不得和对应设置并用。Service.Edit 后 mutationResult；CLI 不自行改 SQLite rows。 |
| `(*runtime).removeCommand` | 多 selectors → Service.Remove。 | 最低一个参数；批量解析原子性由 Service 实现。 |
| `(*runtime).listCommand` | ls 可选 namespace → filtered overlay。 | 先校验 filter，Service.List 后按 namespace 筛选并 printAssets；无隐式网络同步。 |
| `(*runtime).findCommand` | 非空 QUERY → overlay 中匹配的 Assets。 | 拒绝纯空白 query，转小写，再 List/matches；保留原排序、无匹配输出 []。 |
| `(*runtime).showCommand` | SELECTOR → 单个 AssetView。 | Service.Show；JSON 用 `{"asset":...}`，人类模式复用 printAssets；不会因重名文件选错 UUID。 |

### [`publication.go`](../internal/cli/publication.go)

| 精确符号 | 作用、输入与输出 | 副作用、错误与设计理由 |
| --- | --- | --- |
| `(*runtime).commitCommand` | 要求非空 `-m MESSAGE`，调 Service.Commit。 | JSON 输出 CommitResult；ChangeCount=0 人类输出 nothing staged；出现业务/网络错误不打印成功。journal、重试和 ref 全由 app 负责。 |
| `(*runtime).verifyCommand` | 调 Service.Verify → VerifyResult。 | JSON 或 head/assets/objects 人类输出；校验失败不返回 exit 0，不把 staged bytes 当远程已提交 bytes。 |

### [`delivery.go`](../internal/cli/delivery.go)

| 精确符号 | 作用、输入与输出 | 副作用、错误与设计理由 |
| --- | --- | --- |
| `markdownAlt` | title → 可放进 `![alt]` 的文本。 | 用共享 Replacer 转义反斜杠/方括号，CR/LF 换为空格；不修改 Asset.Title。 |
| `(*runtime).authCommand` | 构造 auth/status；读取 GITHUB_TOKEN、GET /user。 | 只输出 login/authenticated，不打开本地图库 DB，不显示 token；没有 auth login。构造 Location 只是满足 client 校验，User 请求实际 endpoint 是 `/user`。 |
| `(*runtime).urlCommand` | SELECTOR、provider → 推导链接与 committed 标记。 | provider 先校验；从本地 Location/Show 推导，不探测网络。view.State==added 时 committed=false，其余包括 metadata-only modified 为 true（图片本体早已 committed）。JSON 用布尔值表达，人类模式 prospective 链接在 stderr 警告。deleted 不可见；local/private 不输出假公网链接。 |

## 11. `cmd/touko`：进程边界

### [`main.go`](../cmd/touko/main.go)

| 精确符号 | 作用、输入与输出 | 副作用、错误与设计理由 |
| --- | --- | --- |
| `main` | 将真实 os.Args/stdin/stdout/stderr 接到 cli.Run。 | 只在进程入口建立 Background 根 context，通过 NotifyContext 处理 SIGINT/SIGTERM；Run 返回后 stop 通知并 os.Exit(code)。深层长 IO 继续使用传入 ctx，不自行另造 Background 截断取消链。 |

## 12. 按问题定位，而不是随意跨层改代码

| 你观察到的问题 | 应先看哪些符号 | 应避免的误修复 |
| --- | --- | --- |
| 同图上传两次为何两个 ID、一个对象？ | NewAssetID → Service.Add → PayloadPath → placements/PlanCommit | 不把 UUID 改成 hash，否则独立 metadata 记录合并。 |
| 删除第一张同图却还保留 binary？ | Service.Remove → PlanCommit 的 before/after placement | 不按单条 rm 直接删文件；末引用才应删除。 |
| GitHub timeout 后到底发出去了没有？ | SavePublication → publishJournal → publicationRelation → recoverPublication | 不仅凭 HTTP error 清 stage，不创建第二个候选冒充重试。 |
| 远端发布了，本机仍旧列表？ | finalizeCandidate → Store.Save → syncLocked/recovery | 不先删除 DB；它可能正保存唯一的未发布 stage/journal。 |
| sync 看见损坏 metadata？ | ReadSnapshot → readMetadata/DecodeAsset → Store.Save | 不跳过坏条目保存部分 gallery。 |
| 内存/取消相关问题？ | Ingest、OpenBlob/blobReader、CreateBlob pipe、VerifyObject | 不为图像加 ReadAll/base64 string，不只关 pipe 却忘掉阻塞 source 或 Wait。 |
| 非 TOUKO 文件丢失？ | CreateTree(base_tree)、local writeTree、validateCandidate 完整 leaf 比较 | 不用空 tree 重建只含图片的仓库，也不忽略原 mode/type。 |
| 链接存在但打不开？ | BuildURL、urlCommand 的 committed、add Markdown 警告 | 推导成功不等于已 commit/CDN 传播；local/private 不能用假公网 URL 兜底。 |

阅读完本表后，可回到 [代码导读](code-guide.md) 复盘模块边界，再照 [使用教程](usage.md) 用自己的 state-dir 操作。涉及协议变化时同时维护格式/状态/发布契约；不要在 CLI 里绕过 coordinator 修补表面现象。

## 13. `examples/demo.sh`：可观察 CLI 验收，不是第二套业务实现

Bash 部分负责参数/依赖检查、创建默认临时目录、关闭网络下载并启动 Python；真正图片操作全部通过新 CLI 子进程。Python 3.8+ 仅用于断言与 JSON 处理，CLI 本身没有 Python 依赖。

| 精确函数 | 作用与关键边界 |
| --- | --- |
| `require` | 用明确异常断言行为，不受 Python `-O` 禁用 assert 影响。 |
| `run` | 启动真实进程，捕获 stdout/stderr 并检查退出码；失败保留命令与诊断。 |
| `cli` | 为一次真实 binary 调用附加独立 device state-dir；每次进程退出体现磁盘持久性。 |
| `document` | 追加 --json；成功读 stdout，预期错误要求 stdout 为空并解码 stderr。 |
| `git` | 只读检查实际 bare repository，不依赖 CLI 人类措辞。 |
| `head` / `paths` | 从真实 ref/tree 读取当前发布 SHA 与 NUL 分隔 leaf paths。 |
| `tree_counts` | 精确检查 metadata 与物理 object placement 数量，发现部分发布或误删。 |
| `assets` | 检查全为 committed，并按 UUID 比较完整 Asset metadata，而非只比较长度。 |
| `clean` | 要求 status changes=[]，判断没有未发布修改。 |
| `verify` | 要求 verifier 的 head/assets/objects 与真实 Git 和预期一致。 |
| `placement` | 依照 format v1 从 Asset 描述派生路径，以检查末引用删除。 |
| `filesystem_state` | 记录 scratch 树条目、mode、size、mtime，证明非空目标拒绝没有写入。 |
| `main` | 按四图单批提交→fresh device→stale edit conflict→末引用删除→scratch SQLite 重建→Unicode stdin→delivery preflight→拒绝重跑的顺序完成验收。保留结果并打印可继续操作的命令；不接触 token、不清历史。 |

`examples/images/` 三张 64×48 RGB PNG 为自生成红/绿/蓝图案，hash 不同；重复 add red 证明同内容去重，不额外复制 fixture。用户 `logo/TOUKO.png` 不属于脚本生成物，只有用户执行打印的 add 命令时才被读取。
