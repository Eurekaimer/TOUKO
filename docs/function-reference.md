# TOUKO 生产函数导航与实现详解

本文按生产源码逐文件解释函数和方法，包含非导出的关键 helper；不把测试辅助函数算作产品 API。适合在阅读 [代码导读](code-guide.md) 后，带着具体符号搜索这里，再回源码逐行理解。实际操作顺序见 [使用教程](usage.md)。格式、状态和发布契约分别见 [repository-format.md](repository-format.md)、[local-state.md](local-state.md)、[publication.md](publication.md)。

本文描述代码职责和失败语义，不是检查执行记录，也不表示已用真实 GitHub token 完成线上验收。CLI 与本地 Webview 共用业务核心；当前没有 R2、缩略图服务或插件系统。delivery 只推导链接，不负责上传到 CDN、探测链接或清除 CDN 缓存。

## 1. 如何用这份导航

表格中符号省略当前包名前缀，例如 domain 章节的 `ParseAssetID` 指 `domain.ParseAssetID`；`(*Service).Commit` 表示接收者为 `*app.Service` 的方法。方法名可能跨包重复，务必结合文件路径搜索。机械的文本编码方法可以共用一行，但名字全部列出。Cobra 的匿名 `RunE`、排序比较器、`defer` 和 goroutine 没有独立符号，职责并入创建它们的具名函数。

推荐的调用链阅读顺序：

1. **身份与格式**：`domain/identity.go`、`domain/asset.go` → `repository/format.go`、`path.go`。
2. **本地暂存**：`app/state.go` → `image/ingest.go` → `staging/files.go` → `index/{store,workspace}.go` → `app/stage.go`；原路径导入另看 `app/import.go`。
3. **一次发布**：`repository/planner.go` → `app/commit.go` → 所选 Git adapter；显式 path 的 v2 manifest 升级也在这个批次内。
4. **恢复与重建**：`app/{sync,refresh,reindex}.go`、`index/{workspace,rebuild}.go` → `app/commit.go` journal helper。
5. **用户入口**：`cmd/touko/main.go` → `cli/root.go` → 各命令构造函数。

三个不同身份不能混用：Asset UUID是记录身份，ObjectHash是bytes身份，RepositoryHead/BlobID/TreeID是Git身份。相同bytes可以有不同Asset ID；canonical放置受namespace影响，但显式Path直接指定真实位置，完整ObjectPath才决定placement是否共享。

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
| `(Asset).Validate` | 校验非零 ID、规范 namespace、时间关系及可选 Path，再调用对象 Validate。 | 不检查路径存在性；显式 path 不改变内容身份，也不随 namespace 自动搬动。 |
| `ValidateRepositoryPath` | 检查规范相对 POSIX 文件/目录路径。 | 拒绝空/点/绝对路径、穿越、反斜杠、控制字符、大小写等价 `.git/.touko` 段；允许 Unicode、空格、无后缀。无 IO。 |
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
| `ValidateManifest` | 接受 format v1/v2、`sha256`、非零 RepositoryID。 | 未知版本按 corruption 失败；不会自动重写仓库。初始化仍 v1，必要升级由 planner 原子纳入发布。 |
| `EncodeManifest` | 验证后输出 manifest JSON bytes。 | 无 IO；初始化和 SQL 保存共享同一编码。 |
| `DecodeManifest` | 以 64 KiB 上限解码并验证 manifest。 | 超限/不支持格式包装 corruption。 |
| `EncodeAsset` | 验证 Asset；时间转 UTC；nil tags/collections 编成 `[]`；输出带换行 JSON。 | 不改调用者 struct 本身；输出超过 1 MiB 为 corruption。planner 与 index 使用同一编码比较最终 metadata。 |
| `DecodeAsset` | 以 1 MiB 上限解码，验证字段和 `<ID>.json` 文件名，补齐空数组。 | ID/文件名不符直接 corruption；不会按文件名重新赋予身份。 |

### [`path.go`](../internal/repository/path.go)、[`location.go`](../internal/repository/location.go)

| 精确符号 | 作用、输入与输出 | 副作用、错误与设计理由 |
| --- | --- | --- |
| `AssetPath` | AssetID → `.touko/assets/<uuid>.json`。 | 纯函数；元数据路径与标题、原文件名无关。 |
| `BuildObjectPath` | namespace/object → `objects/<namespace>/sha256/<前2位>/<后2位>/<hash>.<ext>`。 | 纯拼接，调用者应先验证 namespace/object；同 namespace 同 bytes 共 placement，跨 namespace 分开路由。 |
| `ObjectPath` | Asset → 实际图片路径；非空 Path 优先，否则 BuildObjectPath。 | 纯函数，调用方先验证 Asset；导入、preview、verify、引用计数、URL 均使用它，不能重新假设 canonical。 |
| `(Location).Validate` | 检查非空安全 branch、支持 backend 和 backend-specific 字段。 | localgit 必须绝对 path 且无 owner/name；github 必须 owner/name 且无 path。这里不是完整 Git branch validator，adapter 继续验证；不保存 token。 |
| `(Location).RemoteURL`、`CloneDir` | GitHub Location → HTTPS clone URL；stateDir → `<state-dir>/clone`。 | 不携带凭据，复用同一位置派生规则，避免 CLI/Web 各自拼地址。 |

### [`snapshot.go`](../internal/repository/snapshot.go)

`ReadSnapshot` 是 sync、commit、verify、candidate recovery 的共同入口。它先确定 commit/tree，之后只按固定 ID 读取，避免一次扫描混进两个分支版本。

| 精确符号 | 作用、输入与输出 | 副作用、错误与设计理由 |
| --- | --- | --- |
| `readMetadata` | Reader、tree entry、上限 → metadata bytes。 | 必须是 `100644/blob`、大小合法；限量读并 Close，校验实际长度等于 tree size。读取/关闭失败向上传递。 |
| `ReadSnapshot` | Reader、固定 HEAD → manifest、Assets、tree、完整 Entries。 | 验证安全/重复路径、完整 metadata、UUID/文件名、hash 描述、真实 ObjectPath 引用的 blob/mode/size；v1 非空 path 拒绝，v2 允许 canonical 与 explicit 混用。图片接受普通 `100644/100755`，不接受 symlink。无 manifest 为 uninitialized；失败不写 cache，不下载全部图片。 |

### [`planner.go`](../internal/repository/planner.go)

关键思路是“先求最终 Asset 集合，再算 placement 引用差”，不是按照 changes 顺序边删边加。输入/输出都在内存里，无 clock、随机数、文件、网络或 SQL IO。

| 精确符号 | 作用、输入与输出 | 副作用、错误与设计理由 |
| --- | --- | --- |
| `(CommitPlan).Empty` | 判断 metadata writes/new objects/deletions 是否都空。 | reused objects 本身不是一次写入；空计划不值得创建 commit。 |
| `placements` | Asset map → 按完整 ObjectPath 去重的 ObjectPlacement map。 | 验证 map key、metadata、同 hash 描述和同 path 描述一致性；不只按 namespace/hash，显式 path 可跨 namespace 共享引用。VerifySnapshot 复用。 |
| `PlanCommit` | base Snapshot + normalized ChangeSet → CommitPlan。 | stale base/重复操作/非法 change 全批拒绝。先复制求最终 Asset map，再按完整路径引用差生成稳定排序 writes/objects/deletions；末引用消失才删，same-path/same-bytes 新增保留。最终集合首次含显式 path 时把 v1→v2 manifest write 纳入同一批，普通 v1 操作不升级，v2 不降级。已有 regular placement 复用前需验证，不覆盖无关条目，不清 orphan。 |

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
| `Ingest`、`IngestWithOptions` | ctx、source、tempDir、可选 Options → `Result{Object, TempPath}`。 | source 只读一次，经 MultiWriter 同时写临时文件和 SHA-256；读到第 100000001 字节即可拒绝超限。Sync/Close 后从临时文件重新读 header；扩展名来自真实 PNG/JPEG/GIF/WebP 格式。可选转换只解码 PNG/JPEG，编码 WebP 后仅在更小时替换；GIF/WebP 保持原字节。转换受 50,000,000 像素上限约束。空输入、格式错误、取消、IO 失败、无效质量均移除临时文件；成功后临时文件由调用者接管，source 始终由调用者管理。 |
| `convertToWebP` | ctx、源临时文件、tempDir、quality → 新 `Result`。 | 完整解码、编码到独立临时文件并流式计算 SHA-256；输出尺寸来自解码 bounds，manifest 描述实际 WebP 字节。失败不留下候选文件。 |
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

## 5. `internal/index`：committed cache 与耐久工作区

### [`store.go`](../internal/index/store.go)

`State.Head/Tree/Manifest/Assets` 保存于 SQLite schema2 committed cache；`BaseHead/Changes/Publication` 保存于独立 `workspace.json`，所需 payload 在 stage 目录。删除 schema2 cache 不删除这些未发布状态；未迁移 schema1 则不能这样假定。workspace 的 `version:1` 与 SQLite schema/仓库 format 版本独立。

| 精确符号 | 作用、输入与输出 | 副作用、错误与设计理由 |
| --- | --- | --- |
| `Open` | ctx、stateDir → *Store。 | 创建/收紧目录0700、DB0600（Unix；Windows继承ACL），绝对路径转为正确转义的file URI，支持Windows盘符与中文/#/%路径；单连接 SQLite，初始化失败关闭连接。普通 app.Open 依赖它，损坏 cache 恢复应走独立 app.Reindex。 |
| `(*Store).initialize` | user_version → 新建/迁移/校验 schema。 | 接受0/1/2，设置WAL/foreign_keys；0新建 committed-only schema2并确保workspace，1调用migrate，2要求耐久工作区可读；未知版本拒绝。 |
| `(*Store).Close`、`corrupt` | 关闭 DB；构造 ErrRepositoryCorrupt。 | Close 不删除工作区；corrupt 不吞底层问题。 |
| `(*Store).Load` | Store mutex 下组合完整 State。 | 先LoadWorkspace，再只读SQL事务读 committed singleton/manifest/assets；核对两处 repository identity。错误不返回部分状态，不把坏 workspace 当空 stage。 |
| `loadAssets` | SQL事务 → committed Asset map。 | DecodeAsset 检查主键与metadata ID，检查rows.Err。 |
| `(*Store).Save` | 完整 State → cache-first 持久化。 | 先编码/验证两半，事务替换assets并upsert committed singleton，Commit后writeWorkspace。不是跨资源原子事务：cache可能已前进而旧stage/journal仍在；workspace最终同步失败必须报错。 |
| `(*Store).SavePublication` | publication或nil → 只更新耐久 journal。 | mutex下读workspace、改Publication、原子替换文件；不动cache与changes。发布ref前必须成功，不能只留进程内candidate。 |

### [`workspace.go`](../internal/index/workspace.go)

| 精确符号 | 作用、输入与输出 | 副作用、错误与设计理由 |
| --- | --- | --- |
| `validateChanges`、`validateWorkspace` | 验证change key/ID/kind/完整metadata，delete无metadata，journal candidate/tree完整，identity非零。 | 无IO；迁移和每次保存复用，坏旧stage不能被“修复”为丢失记录。 |
| `encodeWorkspace` | State → version1 durable JSON。 | 仅编码identity/base/changes/journal，不持久化第二份committed Assets。 |
| `LoadWorkspace` | ctx、dir → 不打开SQLite的durable State。 | 严格JSON、版本与change校验；Manifest若存在仅含恢复identity。缺失/损坏均报错，绝不等同空stage。 |
| `durableRename`（`rename_unix.go` / `rename_windows.go`） | source、target → 是否已移动及错误。 | Unix Rename→目标目录Sync/Close；Windows MoveFileEx(REPLACE_EXISTING\|WRITE_THROUGH)，不对只读目录句柄Sync。即使后续Sync失败也报告已移动，让重建能恢复该文件；错误显式返回。 |
| `saveWorkspace`、`writeWorkspace` | 校验编码后以临时文件替换workspace.json。 | CreateTemp0600→Write→Sync→Close→取消检查→durableRename；失败清理临时文件。Unix rename后目录Sync失败不等于旧文件仍在。 |
| `(*Store).migrate` | schema1 → schema2。 | SQL事务读旧base/changes/publication，先写耐久副本，再DROP旧changes/publication和base列并升版本。已有durable副本须完全一致；失败保留旧SQL源，不保留双写真相。 |
| `loadLegacyChanges` | 旧SQL rows → validated changes。 | 仅兼容迁移入口；严格UUID/kind/metadata，拒绝坏stage，非schema2日常读取路径。 |

### [`rebuild.go`](../internal/index/rebuild.go)

| 精确符号 | 作用、输入与输出 | 副作用、错误与设计理由 |
| --- | --- | --- |
| `physicalCorruption` | error → 是否SQLite CORRUPT/NOTADB。 | 不把权限/取消/未知schema等错误当作可替换坏库。 |
| `Rebuild` | 已验证State → 重建committed cache。 | 调用者持workspace锁、已验完整源。健康DB（包括逻辑metadata损坏）原地Save；物理坏库先构建/flush关闭替代库，再将原DB/WAL/SHM保存于index-corrupt-*并替换。移动失败尝试恢复，绝不静默删除原库；workspace stage/journal保留。 |

### 为什么 Save 与 SavePublication 不能合并成普通“保存”

1. detached candidate尚未发布，cache仍应表示旧gallery。
2. SavePublication先耐久记录candidate，成功后才能UpdateRef。
3. 发布确定后Save先推进cache，再耐久清Changes/BaseHead/Publication；最后清payload。
4. SQL失败保留旧cache/workspace；workspace替换前失败可能留下新cache配旧stage/journal。两种情况都由sync校验candidate后补账，不重复发布。rename后同步失败须重读，不能承诺文件回滚。
5. 健康schema1自动迁移可救出stage；旧库物理不可读且没有durable副本时，重建不能凭空还原未发布工作，须保留原库并找可读备份。

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
| `(*Service).Add` | 一批Input与AddOptions → added AssetViews。 | 持锁、Load/mutable、逐个Ingest/Install、新UUID/UTC时间、规范列表，最后一次Save；默认canonical，Directory非空校验并设置`<directory>/<hash>.<ext>`显式Path。整批失败不留下部分stage；按旧引用清理payload。内容重复仍独立UUID，placement是否共享看完整path。 |
| `union` | base Assets + staged adds → selector 候选 map。 | 刻意保留 staged-delete 的 base ID；供 Edit/Remove 区分 delete→edit 冲突和重复 rm 幂等，而非将两者都误报 not found。 |
| `overlay` | State → 按 CreatedAt/ID 排序的 AssetView 列表。 | 在 union 上应用 update/delete，隐藏已删记录，标 `added/modified/committed`；纯内存视图，不移动 base。 |
| `(*Service).List` | Load 后返回 overlay。 | 只读本地cache+workspace；CLI ls/find不隐式sync。 |
| `(*Service).Show` | selector → 一个可见 AssetView。 | 在 overlay 的可见集合中 ResolveAsset；staged-delete 不可见，不按文件名选。 |
| `(*Service).Edit` | selector与可选title/tags/collections → error。 | 持锁解析union；已delete返回delete_modify。nil保留、空值清空、相同值no-op，真实变化更新UpdatedAt，add→edit仍add，否则update；Save持久化workspace。不替换bytes或移动路径/namespace。 |
| `(*Service).Remove` | 多selectors → error。 | 先全部解析再改map/Save，同批ID去重；add→rm取消，committed/update→delete，重复delete幂等。Save后cleanup；清理失败不回滚已耐久保存的stage，不应误报删除未暂存。 |

### [`sync.go`](../internal/app/sync.go)

| 精确符号 | 作用、输入与输出 | 副作用、错误与设计理由 |
| --- | --- | --- |
| `sameRepository` | 本地 State 与 remote Snapshot → identity 校验结果。 | 本地已有 manifest 且 RepositoryID 不同时 conflict；不悄悄把一台设备切到另一图库。 |
| `snapshotState` | 旧 State、Snapshot、clear → 新 State 值。 | 替换 committed Head/Tree/Manifest/Assets；clear=true 还清 Changes/BaseHead/Publication。自身不写 SQL。 |
| `(*Service).Sync` | Git adapter → SyncResult。 | 持锁后调用 syncLocked，避免与 staging mutation 并发。 |
| `(*Service).syncLocked` | 已持锁时加载状态、恢复 journal 或扫描远端。 | journal 优先走 recoverPublication(retry=false)；无 journal 且 stage 非空、HEAD 已变则 conflict，保留旧状态。扫描/identity 全通过才 Save；HEAD 未变可保留 stage。普通 sync 不下载全部图片。 |

### [`refresh.go`](../internal/app/refresh.go)、[`reindex.go`](../internal/app/reindex.go)

| 精确符号 | 作用、输入与输出 | 副作用、错误与设计理由 |
| --- | --- | --- |
| `(*Service).SyncWith` | prepare(ctx,recovering)回调 → SyncResult。 | 持同一Service锁Load判journal，再prepare更新读取源后syncLocked；recovering=true必须返回权威API而非浅clone。恢复成功后仍在同锁内prepare(false)刷新clone并再sync；不允许fetch/读HEAD之间插入stage操作。 |
| `Reindex` | ctx、stateDir、Git → SyncResult。 | 不经app.Open，先锁、LoadWorkspace；没有durable副本时尝试健康schema1迁移，旧库不可读则拒绝并保留。reindexState全验后index.Rebuild，保留stage/journal；journal关系须随后API sync。 |
| `(*Service).Reindex` | 现有Service、Git → SyncResult。 | 锁内独立读workspace并全验源，再用现有Store.Save原地重建；Web不能替换仍被打开的DB文件。 |
| `reindexState` | durable State与固定reader HEAD → 重建State/结果。 | 无journal且stage base落后则conflict；ReadSnapshot、identity、VerifySnapshot均成功才替换cache。不依据浅clone清journal。 |
| `pendingInitializationState` | 含初始化journal的State → 保留未初始化cache表达。 | 清Head/Tree/Manifest/Assets但保留journal，避免后续validateCandidate误入普通batch分支；不意味着初始化发布失败。 |

### [`import.go`](../internal/app/import.go)

| 精确符号 | 作用、输入与输出 | 副作用、错误与设计理由 |
| --- | --- | --- |
| `(*Service).ImportLegacy` | ImportOptions{Directory,Namespace,Tags,Collections} → ImportResult{Imported,Skipped}。 | 持锁扫描固定已初始化HEAD；检查identity/base/journal，按完整ObjectPath跳过已管理（含stage）文件。普通PNG/JPEG/GIF建立新UUID、保留原Path，只在整批验证后Save metadata，不移动/复制/重传bytes；无manifest须先显式Initialize。重复导入幂等，不支持类型计skipped，真实读/校验失败拒绝全批。 |
| `inspectLegacy` | GitEntry → hash/size/MIME/ext/尺寸完整ContentObject。 | 固定blob流式读header并drain求SHA256，限MaxBytes+1、核对tree size、检查Validate/Close；不依赖文件后缀，无图片payload安装。 |
| `(*legacyReader).Read` | context-aware source → bytes/计数。 | 记录取消/读错误供DecodeConfig后区分“不支持格式”和真实IO失败。 |


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
| `(*Service).finalizeCandidate` | 已验证候选 → 最新合法SyncResult。 | 再证candidate可见；后代HEAD重读Snapshot并验identity。Save先committed cache后durable清stage/journal，再cleanup；SQL或workspace失败均报published head和本地记账错误，旧durable证据用于sync恢复，不能重发。cleanup失败时已记账，只报本地清理问题。 |
| `candidateCorrupt` | 失败原因 → 带 publication candidate 上下文的 `ErrRepositoryCorrupt`。 | 与普通 head conflict 分开：候选本身不符合 durable 意图，不可重试发布。 |
| `validateCandidate` | Reader、含 journal 的 State → 完整验证过的候选 Snapshot。 | 只读 Git，不改 SQL/ref；验证失败保留 stage/journal，阻止发布或清账。不是只比 Asset 数；详见下节。 |

#### `validateCandidate` 逐段理解

1. **绑定 journal 与 commit**：按 CandidateHead 读 Snapshot，tree 必须等于 CandidateTree；root 候选不能有 parent，其他候选必须只有 BaseHead 这一个 parent；检查 RepositoryID。
2. **恢复初始化意图**：本地无 manifest、无 changes 时，candidate 必须无 Asset。读取已有 base tree（若有），不允许 base 已占 `.touko`/`objects`；唯一计划 metadata write 是编码后的 manifest。
3. **恢复普通 batch 意图**：staging BaseHead 必须等于 journal BaseHead。重新读取 immutable base，再用 durable stage 调 PlanCommit，逐 Asset 比较最终集合和编码 metadata。重新计划比信任一次进程内临时 plan 更可靠。
4. **构造预期完整 leaf tree**：从 base 的非 tree 条目出发，应用计划删除/metadata/object；目录 tree ID 会因后代变化而变，因此不拿祖先目录 SHA 当独立用户内容比较。
5. **核对写入 bytes 与复用对象**：metadata（含必要v2 manifest）bytes/mode/type/size必须吻合；objects按真实ObjectPath核对，复用blob不可换SHA，每个新增/复用placement再VerifyObject。
6. **拒绝所有计划外差异**：实际 leaf 数量和每个 GitEntry 必须与预期相等；已删除 binary 不能残留，无关 README、symlink、executable、gitlink 等不能被悄悄改动。

#### 用失败位置理解状态保留

| 失败位置 | 远程可见状态 | 本地可恢复依据 |
| --- | --- | --- |
| stage/计划/上传/tree/commit 创建时 | UpdateRef 尚未执行，分支仍旧版本；可能有未引用 Git 对象。 | 原 committed cache 与 stage 保留。 |
| SavePublication时 | 不允许接着UpdateRef。 | stage保留，workspace同步错误须检查实际durable状态，不能承诺跨文件回滚。 |
| UpdateRef 请求失败或超时 | 必须 read-back；不能只看请求 error 判断结果。 | durable journal 保留，直到已发布收尾或明确放弃未发布 candidate。 |
| 已发布后Save失败 | candidate或合法后代已可见。 | cache可能旧或已推进；workspace替换前失败仍留旧stage/journal，sync重新校验后补账。 |
| 已落账后 payload cleanup 失败 | 已发布，DB 已更新。 | 返回 published head 与清理错误；不应重新发布同一 batch。 |

### [`verify.go`](../internal/app/verify.go)

| 精确符号 | 作用、输入与输出 | 副作用、错误与设计理由 |
| --- | --- | --- |
| `(*Service).Verify` | supplied Git reader当前HEAD → 固定Snapshot → VerifyResult。 | 不用staged overlay、不清stage/写cache/改ref。GitHub CLI/Web传当前clone，不fetch；成功只证明该HEAD，不保证最新远端或历史/CDN已清除。 |

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

凭据只在运行时实例/内存，CLI使用credential.Resolve；优先GITHUB_TOKEN、GH_TOKEN，再gh keyring。不进config、SQLite、workspace、metadata、URL或argv。API metadata JSON限量驻内存，图片走raw/pipe通道。

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

### [`repositories.go`](../internal/github/repositories.go)、[`credential/credential.go`](../internal/credential/credential.go)

| 精确符号 | 作用、输入与输出 | 副作用、错误与设计理由 |
| --- | --- | --- |
| `(*Repository).ListRepositories` | ctx → []RepositoryInfo。 | 认证后分页读取当前用户可访问仓库，返回full_name/default_branch/private/can_push；不是只有第一页，不由普通gallery读取触发。 |
| `(*Repository).ListBranches` | ctx → []BranchInfo。 | 分页读取该仓库全部分支，返回name/protected；公开仓库允许匿名读取。Link只作为分页信号，后续请求仍固定走受信任API origin，绝不把带凭据的请求发往Link里的第三方主机。 |
| `credential.Resolve` | ctx → token、source、error。 | 顺序GITHUB_TOKEN→GH_TOKEN→`gh auth token --hostname github.com`；source为github_token/gh_token/gh/anonymous。无gh/明确未登录允许匿名；显式env无效不回退，取消/意外执行失败报安全错误；token/stdout/stderr只留内存，不输出原始stderr。 |


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
| `BuildURL` | Location、安全真实仓库相对objectPath、Provider → URL。 | 无网络；public GitHub only，local/private为ErrDeliveryUnavailable。复用ValidateRepositoryPath，不要求canonical六段/hash分桶/扩展名，支持Unicode/空格/无后缀原路径；安全编码owner/name/branch/path，provider为github raw或jsdelivr。调用方传ObjectPath，不存URL，也不证明已发布/CDN传播。 |

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
| `(*runtime).backend` | Service.Location → localgit或GitHub API adapter。 | GitHub经credential.Resolve取得运行时凭据；用于写入与权威journal恢复，不以浅clone代替远端关系。localgit会实际Open仓库。 |
| `(*runtime).encode` | 任意结果 → stdout 单个 JSON 值。 | 输出错误返回给 Run，不写调试日志。 |
| `(*runtime).command` | 创建 root 与全部命令树。 | 注册全局 state-dir/json、`version` 与 `--version`，禁自动 completion 子命令；版本查询只读 buildinfo，不打开 Service。其他业务在子命令；PersistentPreRun 标记已开始，统一用法错误包装。 |
| `(*runtime).printAssets` | AssetView 列表 → JSON envelope 或人类行。 | nil 规范为空数组；人类输出 ID/state/namespace/title/hash；不改变 stage。 |
| `(*runtime).statusCommand` | status → 本地 head/base/排序 changes。 | 只 Load，不隐式 sync；JSON 空 changes 为 []。人类删除条目的 metadata 从 committed Assets 取。 |
| `(*runtime).resetCommand` | 要求显式--staged → backend + Service.Reset。 | GitHub构造APIclient但无journal不发HTTP；有journal用权威API恢复，不用浅clone。localgit仍会Open，因此本地仓库缺失时不能承诺离线reset成功。不修改远端ref。 |
| `(*runtime).mutationResult` | mutation 成功 → JSON `{"ok":true}` 或无额外人类输出。 | 给 edit/rm/reset 共用；输出失败不回滚已完成业务。 |
| `matches` | AssetView、已小写 query → bool。 | title/original_name/tags/collections 做不区分大小写子串匹配；find handler 负责拒绝空 query，不支持 filename 身份选择或复杂查询语言。 |

### [`repo.go`](../internal/cli/repo.go)

| 精确符号 | 作用、输入与输出 | 副作用、错误与设计理由 |
| --- | --- | --- |
| `(*runtime).repoCommand` | 构造 repo/init/use 子树，RunE 装配仓库和 Service。 | init --local 先 localgit.Init/main 再 Select/Initialize；普通 init 对已选 adapter Initialize。use 二选一 `--local PATH` 或 `OWNER/REPO`；local 默认 main，github Discover 取 default_branch/private 并查 ref，再 Select。use 无远端写；选择冲突不会复用其他图库 stage。 |
| `(*runtime).syncCommand` | sync → Service.SyncWith + printSync。 | 同一workspace锁内，GitHub普通路径先syncClone再reader读取更新HEAD；journal路径改APIbackend恢复，成功后同锁刷新clone/同步。网络/验证失败不输出假成功。 |
| `(*runtime).printSync` | SyncResult → JSON 或 HEAD/Assets 人类输出。 | 只负责结果呈现，不重新读取分支。 |
| `(*runtime).syncClone` | GitHub Location → Clone后Update。 | Resolve凭据、复用RemoteURL/CloneDir；更新后才允许读取snapshot，非后台fetch。 |
| `(*runtime).reindexCommand` | 独立stateDir/config → app.Reindex。 | 故意不调用runtime.service/app.Open，以恢复坏cache；GitHub先更新clone，reader固定源，再app.Reindex保留durable work。未决journal不在此证明发布，随后须sync。 |
| `(*runtime).importCommand` | directory/namespace/tag/collection → ImportLegacy结果。 | 使用reader，不重新上传已有图；输出imported/skipped并明确仅暂存，显式commit后发布metadata。 |

### [`assets.go`](../internal/cli/assets.go)

| 精确符号 | 作用、输入与输出 | 副作用、错误与设计理由 |
| --- | --- | --- |
| `(*runtime).addCommand` | files或单stdin → stage，输出assets/id/markdown。 | 校验output/json、stdin/name、namespace及directory，Markdown先校验public GitHub；--directory传AddOptions，链接使用ObjectPath。文件defer Close，新增输出仍标not published；stdout失败不回滚已保存stage。 |
| `(*runtime).editCommand` | SELECTOR 与 title/tag/collection flags → EditOptions。 | 用 Changed 与指针区分没提供/明确清空；clear-tags/clear-collections 不得和对应设置并用。Service.Edit 后 mutationResult；CLI 不自行改 SQLite rows。 |
| `(*runtime).removeCommand` | 多 selectors → Service.Remove。 | 最低一个参数；批量解析原子性由 Service 实现。 |
| `(*runtime).listCommand` | ls 可选 namespace → filtered overlay。 | 先校验 filter，Service.List 后按 namespace 筛选并 printAssets；无隐式网络同步。 |
| `(*runtime).findCommand` | 非空 QUERY → overlay 中匹配的 Assets。 | 拒绝纯空白 query，转小写，再 List/matches；保留原排序、无匹配输出 []。 |
| `(*runtime).showCommand` | SELECTOR → 单个 AssetView。 | Service.Show；JSON 用 `{"asset":...}`，人类模式复用 printAssets；不会因重名文件选错 UUID。 |

### [`publication.go`](../internal/cli/publication.go)

| 精确符号 | 作用、输入与输出 | 副作用、错误与设计理由 |
| --- | --- | --- |
| `(*runtime).commitCommand` | 要求非空 `-m MESSAGE`，调 Service.Commit。 | JSON 输出 CommitResult；ChangeCount=0 人类输出 nothing staged；出现业务/网络错误不打印成功。journal、重试和 ref 全由 app 负责。 |
| `(*runtime).verifyCommand` | reader → Service.Verify结果。 | GitHub验证现有clone而不fetch（缺失clone的创建可能联网）；不把staged bytes当committed，不保证最新远端。校验失败不返回成功。 |

### [`delivery.go`](../internal/cli/delivery.go)

| 精确符号 | 作用、输入与输出 | 副作用、错误与设计理由 |
| --- | --- | --- |
| `markdownAlt` | title → 可放进 `![alt]` 的文本。 | 用共享 Replacer 转义反斜杠/方括号，CR/LF 换为空格；不修改 Asset.Title。 |
| `(*runtime).authCommand` | 构造auth/login/status。 | login委托真实`gh auth login --hostname github.com --web --git-protocol https`；status用Resolve和GET /user输出login/authenticated/source，不打开图库DB，不显示token。没有TOUKO自建OAuth/token存储。 |
| `(*runtime).urlCommand` | SELECTOR、provider → 推导链接与 committed 标记。 | provider 先校验；从本地 Location/Show 推导，不探测网络。view.State==added 时 committed=false，其余包括 metadata-only modified 为 true（图片本体早已 committed）。JSON 用布尔值表达，人类模式 prospective 链接在 stderr 警告。deleted 不可见；local/private 不输出假公网链接。 |

### [`serve.go`](../internal/cli/serve.go)

| 精确符号 | 作用、输入与输出 | 副作用、错误与设计理由 |
| --- | --- | --- |
| `(*runtime).serveCommand` | serve --port/--no-open → webview.Listen。 | 仅loopback，打印实际URL；默认用 `browser.Open` 打开浏览器，失败stderr提示但继续服务；--no-open禁自动打开。显式env token或空交后端，gh在Web边界Resolve而非CLI永久缓存。取消后先Host.Close等待handler，再由Run关闭根Service。 |
| `browser.Open` | ctx、URL或目录 → 交给系统默认处理程序。 | 实现在 [`internal/browser`](../internal/browser/browser.go)，serve 与桌面托盘菜单共用；按平台选 xdg-open/open/rundll32，失败返回错误、不伪装已打开。 |

## 11. `cmd/touko`：进程边界

### [`main.go`](../cmd/touko/main.go)

| 精确符号 | 作用、输入与输出 | 副作用、错误与设计理由 |
| --- | --- | --- |
| `main` | 将真实 os.Args/stdin/stdout/stderr 接到 cli.Run。 | 只在进程入口建立 Background 根 context，通过 NotifyContext 处理 SIGINT/SIGTERM；Run 返回后 stop 通知并 os.Exit(code)。深层长 IO 继续使用传入 ctx，不自行另造 Background 截断取消链。 |

### [`internal/buildinfo`](../internal/buildinfo/buildinfo.go)

`Version`、`Commit` 由 `-ldflags -X` 注入，未注入时为 `dev`、`unknown`。`String` 为两个入口提供一致的版本文字，不读取 Git、网络或本地工作区。`version --json` 直接编码两个字段。


## 12. 按问题定位，而不是随意跨层改代码

| 你观察到的问题 | 应先看哪些符号 | 应避免的误修复 |
| --- | --- | --- |
| 同图上传两次为何两个 ID、一个对象？ | NewAssetID → Service.Add → PayloadPath → placements/PlanCommit | 不把 UUID 改成 hash，否则独立 metadata 记录合并。 |
| 删除第一张同图却还保留 binary？ | Service.Remove → PlanCommit 的 before/after placement | 不按单条 rm 直接删文件；末引用才应删除。 |
| GitHub timeout 后到底发出去了没有？ | SavePublication → publishJournal → publicationRelation → recoverPublication | 不仅凭 HTTP error 清 stage，不创建第二个候选冒充重试。 |
| 远端发布了，本机仍旧列表？ | finalizeCandidate → Store.Save → SyncWith/recovery | 不删workspace/journal；cache坏可reindex保留证据，再API sync，不能以浅clone否定发布。 |
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

`examples/images/` 三张 64×48 RGB PNG 为自生成红/绿/蓝图案，hash 不同；重复 add red 证明同内容去重，不额外复制 fixture。用户 `logo/touko-logo.png` 不属于脚本生成物，只有用户执行打印的 add 命令时才被读取。

## 14. `internal/webview`：本地图库边界与浏览器控制器

### `app/view.go`：同一 Load 的页面视图

| 精确符号 | 作用、输入与输出 | 副作用与约束 |
| --- | --- | --- |
| `(*Service).ReadView` | 一次Store.Load → State与overlay。 | 无网络、不另开分类库、不自行持锁；cache SQL与durable workspace由Store mutex组合。调用方按需持Service.Lock直到staged文件已打开，不把两资源称为同一SQLite事务。 |

### [`server.go`](../internal/webview/server.go)：请求与错误边界

| 精确符号 | 作用、输入与输出 | 副作用与约束 |
| --- | --- | --- |
| `New`、`newServer` | Service、服务端token、精确loopback origin → Server。 | newServer验证并生成CSRF，New还建立workspaceSet/default。借用根Service；额外Service由Host收尾关闭。token不进JSON/HTML。 |
| `(*Server).ServeHTTP` | http.Handler 入口。 | 设置 CSP、no-store、nosniff、frame/referrer/CORP 等 header；校验精确 Host/Origin 和 Fetch Metadata；写入必须带 Origin 与恒时比较的 X-Touko-CSRF。API 路由与静态资源分流，无 CORS 放行。 |
| `(*Server).json` | 任意结果 → JSON 响应。 | 设置 UTF-8 Content-Type；连接输出错误不能回滚已经完成的业务。 |
| `(*Server).fail` | HTTP status、code、message → error envelope。 | 统一 `{\"error\":{\"code\",\"message\"}}`；message 中若出现当前 token，先替换为 redacted，不输出请求授权 header。 |
| `(*requestError).Error`、`usage`、`tooLarge` | 生成/呈现请求错误。 | usage 为400，尺寸/数量上限为413；不把 domain conflict 伪装成参数错误。 |
| `(*Server).respondError` | error 链 → 状态码和错误 JSON。 | missing404、conflict/busy409、登录失效unauthorized401、GitHub权限forbidden403、corrupt/hash422、其余GitHub API502、其他IO500；限流不映射为需要登录。 |
| `decodeJSON` | 有界 request body → 指定 input struct。 | 限1MiB、要求 application/json 与恰好一个对象，拒绝 null/数组/未知字段/尾随第二个值。第二次解码直接读 raw bytes，不复制为 string。 |
| `bodyError` | 解码/读取错误 → request error。 | MaxBytesError 原样保留以返回413；其他 malformed input 返回400。 |

### [`api.go`](../internal/webview/api.go)：装配现有业务，而非另写业务

| 精确符号 | 作用、输入与输出 | 副作用与约束 |
| --- | --- | --- |
| `(*Server).api` | 白名单 path、method → endpoint handler。 | image/url/edit 要求规范完整 UUID；其他 method 返回405。workMu 串行状态操作，已取消请求不进入 mutation；原图及浏览器登录会话另走各自短锁，等待浏览器授权不占图库业务锁。成功后才写结果，无自动 reset/retry。 |
| `(*Server).location` | ctx → *Location 或 nil。 | 仅检查已知 state-dir/config.json 是否缺失；未选择仓库可 onboarding，损坏 config 必须报错，不能当空工作区。 |
| `(*Server).backend` | 当前 Location → localgit/GitHub adapter。 | GitHub token 只保留在实例内部；未选择仓库为 usage。没有可由浏览器指定的 API host。 |
| `(*Server).state` | ctx → 完整页面状态。 | 持Service.Lock、一次ReadView读head/base/assets/changes/journal；附workspace和local_read{available,path}与缓存认证状态，不隐式fetch或暴露token。 |
| `(*Server).selectRepository` | 严格JSON → {url,location}。 | local可显式create；GitHub Discover默认branch/private。workspaceSet按Location选择独立Service，不覆盖旧仓库stage；仅local create自动Initialize，其他库需显式init/sync；前端收到url后导航。 |
| `(*Server).assetURL` | UUID、provider query → url、committed。 | 持本地锁读取可见 Asset，调用现有 BuildURL；added 为预期链接，modified 的 object 仍 committed。local/private 不产生假链接。 |
| `(contextReader).Read` | 调用者 ctx 与 reader → bytes。 | 每次读之前检查取消；不承诺能强制打断任意自定义阻塞 reader，HTTP 连接关闭与 adapter 负责实际 IO 解除。 |
| `(*Server).preview` | Asset selector → 原图字节。 | 先按当前 workspace 解析 Asset（删除后这里返回404），再以内容 hash 设置 ETag 与 `private, immutable`；`If-None-Match` 命中直接304，跳过原图读取。缓存不能绕过身份/删除检查。 |
| `(*Server).upload` | multipart files + directory/metadata → staged AssetViews。 | request≤500MB、单图≤100MB、≤100文件；私有临时目录先spool原始part，metadata（含 `convert_webp`/`webp_quality`）有界且不重复，tags/collections为JSON数组，然后在已知选项下统一Ingest；支持PNG/JPEG/GIF/WebP。整批预检后一次Add，directory/转换参数传AddOptions；defer清spool，失败不留半批stage。 |
| `(*lazyGit).resolve` | ctx → 按需打开并复用的 Git adapter。 | Reset 没有 journal 时可离线，不因提前打开不可用 backend 而失败；有 journal 时才访问远端，不能绕过恢复。 |
| `(*lazyGit).Head`、`Commit`、`Tree`、`OpenBlob`、`IsAncestor` | repository.Git 读接口的延迟装配。 | 每个方法先 resolve，再调用同一 adapter；只解决普通 reset 的离线边界，不改变返回值/错误。 |
| `(*lazyGit).CreateBlob`、`CreateTree`、`CreateCommit`、`UpdateRef` | repository.Git 写接口的延迟装配。 | 与上行相同；完整实现接口，不伪造未使用方法的成功结果。 |

### [`auth.go`](../internal/webview/auth.go)、[`read.go`](../internal/webview/read.go)

| 精确符号 | 作用、输入与输出 | 副作用与约束 |
| --- | --- | --- |
| `(*Server).credentials` | ctx、refresh → 内存token/source。 | 所有workspace委托root的authMu与缓存；显式传入token优先，token改变清旧login。GET auth主动refresh，浏览器登录成功后成员不保留旧身份。 |
| `(*Server).authenticationState` | 当前共享缓存身份 → configured/source/login。 | 只读内存，不请求GitHub、不暴露token；gallery state使用此快照。 |
| `(*Server).authentication` | Resolve刷新 + GET /user → 真实身份。 | anonymous返回configured=false，无token不发User请求；401登录失效、403权限限制分开反馈，不把旧login当新成功。 |
| `(*Server).authOwner`、`refreshAuth` | 定位root认证所有者；gh完成后重新读取凭据。 | workspace不另存登录会话或身份；只有实际读取到凭据才可报告授权成功。 |
| `(*Server).repositories` | 凭据 → repositories envelope。 | 调ListRepositories分页列当前用户可访问仓库，普通gallery/图片命中不调用此API。 |
| `(*Server).branches` | `repository=OWNER/REPO` → branches envelope。 | 校验恰好一段owner/name，拒绝空段与多余斜杠；复用同一凭据缓存调ListBranches分页。这是分支下拉的唯一来源，不接受任意URL或分支名。 |
| `(*Server).reader` | 当前Location → 本地Git reader。 | GitHub先ensureClone（缺失时可联网创建，但不fetch已有clone），再localgit.Open；localgit回backend。verify/import以此读取固定快照。 |
| `(*Server).syncRepository` | Service.SyncWith → SyncResult。 | 同锁prepare中普通路径先updateClone再reader；journal走backend权威API，恢复成功后同锁刷新clone/再sync。 |

### [`login.go`](../internal/webview/login.go)：浏览器设备授权

| 精确符号 | 作用、输入与输出 | 副作用与约束 |
| --- | --- | --- |
| `newLoginManager`、`loginStateResponse` | 一份root进程级会话 → status、gh可用性、env覆盖与root CSRF。 | GET不启动子进程或打开浏览器；仅等待中暴露设备验证码与固定GitHub地址，不保留原始gh输出。 |
| `startLogin`、`(*loginManager).start`、`session` | `{}` → 开始或复用活动会话；后台运行真实gh并刷新凭据。 | 单活动会话、约15分钟超时，不占workMu；失败、取消、未读到凭据都不能报告成功。 |
| `(*loginOutput).parse`、`runGHLogin`、`loginEnvironment` | 非TTY gh输出 → 合法一次性code/固定设备URL和存储警告。 | TOUKO负责开浏览器，输出不进网页；只固定执行gh登录，不接受命令参数或任意URL。Windows登录子进程隐藏控制台。 |
| `cancelLogin`、`(*loginManager).cancel`、`shutdownLogin` | 取消并join后台授权。 | 清除验证码，迟到的旧会话回调不能覆盖新状态；退出服务先停止登录，避免遗留gh。 |
| `openLoginDevice`、`openGHInstall` | 重新打开等待中的授权页，或打开官方组件安装页。 | browser.Open只接收固定GitHub/gh地址；失败明确保留手动访问办法，不宣称已经打开。 |


### [`workspaces.go`](../internal/webview/workspaces.go)、[`host.go`](../internal/webview/host.go)

| 精确符号 | 作用、输入与输出 | 副作用与约束 |
| --- | --- | --- |
| `workspaceKey` | Location JSON → SHA256小写稳定key。 | 仅用于本地工作区标识，不是RepositoryID，也不含凭据。 |
| `validWorkspaceID`、`workspaceURL`、`(*workspaceSet).directory` | 校验default或64位小写hex；派生/w/id/和本地目录。 | default保留根state；其他在根workspaces/key/，不接受任意路径穿越。 |
| `(*workspaceSet).open` | id/create → 对应独立Server/Service。 | mutex下复用同id实例，检查目录/config后app.Open与newServer；从不替换已有Server.service，旧请求/打开图片不会串仓库。 |
| `(*workspaceSet).selectLocation` | Location → 已选择的独立Server。 | 首个/根同Location用default，其余稳定key；Service.Select保持各自config/cache/clone/stage/journal隔离。 |
| `(*workspaceSet).remember`、`activeURL` | 保存/读取active-workspace → 导航URL。 | 临时文件Sync/Close/Rename保存选择；读取验证id及config存在，坏状态报错，不是对业务stage的替代存储。 |
| `(*workspaceSet).list` | current → 可导航workspace列表。 | 只读根与workspaces子目录config，核对Location/key一致并排序；空未选目录跳过，坏配置显式失败，不静默切库。 |
| `(*workspaceSet).serve` | /w/id/请求 → 对应Server.ServeHTTP。 | 检查id，规范尾斜杠，克隆request后去前缀并保留安全边界；无效/不存在工作区404，不改共享Service指针。 |
| `(*workspaceSet).close` | 关闭额外工作区Services。 | Host完成登录会话与请求收尾后执行；default根Service仍由调用方拥有。 |
| `Listen` | ctx、Service、token、port → Host。 | 仅tcp4 127.0.0.1，实际端口建立精确Origin，New建立默认/多工作区路由；跟踪连接用于有序收尾。 |
| `(*Host).Wait`、`Close` | 等取消/Serve错误；幂等停止服务。 | Close先取消请求并停止/join gh会话，再做10秒Shutdown并等待全部handler，最后关闭额外workspace；取消不等于远端回滚。 |

页面 API 契约：

| 方法与路径 | 输入/输出重点 |
| --- | --- |
| GET `/api/state` | csrf、location、initialized、head/base/assets/changes/publication、workspace、local_read、auth；无隐式同步。 |
| POST `/api/repo` | localgit: backend/path/branch/create；github: backend/repository/branch → {url,location}，导航到独立workspace。 |
| POST `/api/init`、`/api/sync`、`/api/reindex` | `{}` → head、asset_count；sync更新clone，reindex保留durable stage/journal。 |
| POST `/api/commit` | 非空 message → head、change_count、asset_count。 |
| POST `/api/reset` | `{\"confirm\":true}`；普通丢弃保持离线，有 journal 先恢复/核验。 |
| POST `/api/remove` | 非空 selectors 数组，完整UUID，原子暂存移除。 |
| POST `/api/assets/<uuid>/edit` | 可选 title、tags、collections；省略保留，空数组清空。 |
| POST `/api/upload` | files、directory、namespace、可选title、tags/collections JSON数组 → assets。 |
| GET `/api/assets/<uuid>/image` | 原始图片流；不接受文件系统路径，已移除/不存在为404。 |
| GET `/api/assets/<uuid>/url?provider=github或jsdelivr` | url、committed，public GitHub only。 |
| GET `/api/auth`、`/api/repos` | 实际login/configured/source；可访问repositories列表（含private/can_push），不输出凭据。 |
| GET `/api/auth/login` | root共享登录status、available、blocked_source、csrf；等待时给user_code与固定verification_url，不返回访问token或gh原始输出。GET不启动登录。 |
| POST `/api/auth/login`、`/api/auth/login/cancel` | `{}` → 开始/复用活动会话，或取消并join。检查精确Origin及对应root CSRF；显式环境token覆盖时阻止gh授权。 |
| POST `/api/auth/login/open`、`/api/auth/install` | `{}` → 系统浏览器打开固定GitHub设备授权页或gh官方安装页，不接受任意URL或命令；重新打开授权只允许等待中的会话。 |
| GET `/api/branches?repository=OWNER/REPO` | 该仓库全部分支name/protected；缺参、多参或非OWNER/REPO为400，未登录/无权限/网络错误显式返回，不返回空列表冒充成功。 |
| GET `/api/workspaces` | {current,workspaces:[{id,url,location}]}；/w/id/下同样可用。 |
| POST `/api/import` | directory/namespace/tags/collections → imported/skipped；原路径仅暂存。 |
| POST `/api/verify` | `{}` → 当前reader（GitHub为clone、不fetch）的head/assets/objects。 |

### [`preview.go`](../internal/webview/preview.go)、[`static.go`](../internal/webview/static.go)

| 精确符号 | 作用与不变量 |
| --- | --- |
| `(*Server).preview` | openImage后设置真实MIME/长度，CopyN流出原图；断流则ErrAbortHandler中止，不把JSON拼进图像或假装图片完整。 |
| `(*Server).openImage` | 先staged payload，其次本地图/immutable cached-tree blob，必要时API fallback；真实ObjectPath，不追branch外链。 |
| `(*Server).imageSnapshot` | workMu+Service.Lock下一次ReadView，按完整UUID定位，新增payload先Open/Stat再解锁；导入显式path无payload时改读原Git blob，delete不可见。 |
| `(*Server).imageBlob` | cached tree与真实path → GitEntry；缓存该tree的引用映射、验mode/type/SHA，不缓存binary。 |
| `serveStatic`、`matchesETag` | embed.FS白名单GET/HEAD，按内容ETag与If-None-Match（含weak/*）返回304；HTML/JS/CSS字体等仍私有且每次重验证，不暴露文件系统。 |

### [`localgit/clone.go`](../internal/localgit/clone.go)、[`webview/clone.go`](../internal/webview/clone.go)：本地工作副本

| 精确符号 | 作用与不变量 |
| --- | --- |
| `localgit.Clone` | 建立选定分支浅副本（--depth 1 --no-tags --single-branch）；非空目录仅在localgit.Open失败时拒绝。当前复用路径不核对origin URL，因此不能宣称已证明这是所选远端；只用于TOUKO拥有的clone目录，勿指向用户已有仓库。 |
| `localgit.Update` | `fetch` 分支后 `reset --hard` + `clean -fd`，使工作树与远端**严格一致**（远端删除的文件会消失）。只用于 TOUKO 管理的目录。 |
| `localgit.WorkingCopy` | 缺失则 Clone，然后 Open；读命令用它。 |
| `localgit.runCredentialed` | Git命令经credential protocol与继承fd3传凭据，不进URL/argv/env/文件，等待子进程结束；错误经redact。 |
| `localgit.validateRemote`、`validateBranch`、`redact` | 拒绝嵌入凭据/危险选项及非法branch，错误脱敏；不修改用户全局Git配置。 |
| `webview.(*Server).ensureClone` | cloneOnce下复用已有副本；缺失时用运行时凭据首次Clone。绝不fetch已有clone，不能把“缺失时创建可能联网”写成所有预览都离线。 |
| `webview.localImage` | canonical从checkout读取并验regular/size；显式Path不读可变同名文件，而由openImage按immutable Git blob读取。未命中可回退其他固定快照读取。 |
| `webview.(*Server).updateClone`、`CloneStatus` | 显式sync创建/更新clone；state调用CloneStatus报告实际可用性/路径。并非未使用helper，不在gallery/preview偷偷fetch。 |
| `cli.(*runtime).reader` | GitHub经Resolve与WorkingCopy取得本地clone（缺失可创建），不刷新已有clone；localgit直接backend。verify不fetch，sync由syncClone显式先更新。 |

### [`static/app.js`](../internal/webview/static/app.js)：按用户操作查函数

浏览器不实现planner；真实mutation经当前workspace API/Service。workspace是服务器快照，filters/selected是临时展示状态；workspacePrefix/apiURL使导航、API、upload与image路径隔离。CSS在[`style.css`](../internal/webview/static/style.css)，dialog/表单/SVG在[`index.html`](../internal/webview/static/index.html)。无动态HTML注入、外部脚本或localStorage token。

| 精确函数/符号 | 职责与关键边界 |
| --- | --- |
| `$`、`element`、`icon`、`button` | ID查询、DOM/textContent、安全固定SVG use、带type=button的事件按钮。untrusted metadata只作文本或属性。 |
| `bytes`、`date`、`list` | 十进制体积、本地日期、逗号/中文逗号/换行拆列表并trim/去重。持久化排序与规范化仍由app控制。 |
| `showError`、`clearError`、`toast`、`responseError` | 可见错误/状态；保留实际服务端message，冲突/忙/鉴权另给操作提示。toast不是业务事实来源，不隐藏失败。 |
| `api`、`apiURL`、`apiRoot`、`apiMember` | pathname决定当前workspace/library前缀；根POST先取得并使用root CSRF，各member POST使用自己的CSRF。失败保留code/status，不自动重试mutation；401登录提示与403权限不足分开。 |
| `ensureRootCSRF`、`fetchLoginStatus` | 读取共享登录状态并缓存root CSRF，同一在途读取合并。 |
| `renderAuthLogin`、`loginDiagnostic` | 显示申请中、设备code、成功/失败、env覆盖与缺组件；不显示原始gh输出，终态清掉旧code。 |
| `startBrowserLogin`、`performStartBrowserLogin`、`maybeAutoBrowserLogin` | 用户主动连接时至多自动开始一次；手动重试仍可用，403不循环触发。generation丢弃过期结果。 |
| `scheduleLoginPoll`、`stopLoginPolling`、`cancelAuthLogin` | 串行状态轮询、停止与取消；关弹窗/切本地连接会取消未完成流程，不自动重发业务写入。 |
| `handleLoginSucceeded`、`refreshRepositoryLogin`、`refreshLoginStatus` | 授权完成后刷新真实身份、仓库与分支；状态查询不等于授权成功。 |
| `installAuthComponent`、`recheckLoginComponent`、`copyAuthCode` | 官方安装页入口、组件重检与设备code复制；剪贴板不可用时只选中供手动复制，不声称已复制。 |
| `setBusy`、`updateActions` | 禁重复mutation和处理中dialog交互；根据是否选仓、初始化、changes/journal/selected启用动作。 |
| `mutate` | 单次action→重新GET state→恢复按钮；失败保留表单。已成功但刷新失败时明确要求确认状态，不重复提交冒充重试。 |
| `refreshState` | 读取完整state、清理失效selection/卡片缓存并渲染；sequence拒绝旧刷新响应覆盖新结果。 |
| `showDialog`、`hideDialog`、`closeNavigation` | 原生modal焦点约束、处理期间禁止关闭、移动导航状态；close事件恢复可用原焦点。详情默认聚焦关闭按钮，不跳过原图滚到编辑栏。 |
| `resetFilters`、`ready` | 清查询/namespace/分类/标签/真实目录；上传要求已选仓并初始化，不自动写远端。 |
| `renderWorkspace`、`counts`、`renderFilters`、`renderTrail` | 用真实Asset/changes展示仓库、stage、auth缓存、本地clone可用性、namespace与目录；分类/标签计数及query/namespace/目录/分类/标签可独立撤销凭条。不是假seed数据或云端图床服务。 |
| `assetPath`、`visibleAssets` | 显式asset.path优先，否则canonical派生；query包含真实路径，目录筛选支持子目录与根目录，叠加namespace/分类/标签AND；稳定排序，不写metadata。 |
| `imageURL`、`card`、`renderGallery` | UUID原图端点、lazy img、可访问选择/详情按钮、state/chips/尺寸/体积；过滤期间复用卡片DOM，显示空/错误/选择状态，无seed图库。 |
| `openPreview` | 填真实原图与完整metadata，编辑控件可清空列表；只public GitHub显示URL控件，新增提示prospective。 |
| `renderPending`、`openPending` | 展示规范化add/update/delete、base与publication警告。删除无Asset payload时显示UUID，不补造metadata。 |
| `confirm`、`removeAssets` | 共用明确确认dialog，保留实际失败；remove只暂存UUID列表，成功刷新后关闭对应详情，不立即宣称远端删除。 |
| `releaseUploadFiles`、`setUploadFiles`、`renderUploadFiles`、`openUpload` | 维护File/ObjectURL、客户端数量/体积提示和可移除预览；移除/关闭revoke，失败保留本批输入。 |
| `upload` | FormData+XHR，把传输百分比和服务端校验阶段分开；完整batch一次API请求，不把每个文件变成独立commit。 |
| `openRepository`、`updateRepositoryFields` | local create/use与GitHub字段切换，回填Location并GET workspaces生成导航链接；换仓返回url后window.location.assign，不先refresh旧workspace，不丢旧stage。 |
| `loadGitHubRepositories`、`loadRepositoryBranches` | 打开GitHub连接即检查身份并读完整仓库/分支列表；缺少登录时每次连接弹窗最多自动开始一次浏览器授权，成功后重读，取消/失败/403不循环重启。预选真实preferred/当前/default/首项，显示只读与保护规则，不让用户猜仓库或分支名。 |
| `invalidateGitHubRequests`、`validGitHubSelection`、`updateRepositoryControls`、`resetBranchChoices` | 仓库/分支请求各有递增sequence，切换仓库立即清旧分支并让晚到响应失效；只有下拉里真实存在的仓库+分支才启用连接，`aria-busy`只反映加载态，不用自由文本冒充有效选择。 |
| `result` | 验证结果等数据用dl/textContent展示，长HEAD可换行。 |
| `openImport` | 显示原路径导入表单与未初始化提示；提交directory/namespace/tags/collections后显示imported/skipped与必须commit的提示，不自动初始化/发布。 |
| `copyDelivery` | 读取真实URL；Markdown转义alt/换行，生成安全目标链接；尝试clipboard.writeText，失败显示并选中可手动复制框。生成文本不等于权限受限浏览器已自动复制。 |
| `bindEvents` | 注册上传directory、导入、reindex、同锁sync、verify、commit/reset、编辑、原图URL/Markdown、工作区导航及仓库/分支两级下拉（读取auth/repos/branches，显示private/can_push，切仓库即清旧分支）；匿名回调复用真实API，不另造业务层，不接收token。 |

## 15. `internal/desktop` 与 `internal/browser`：MyGo 窗口与系统打开

### [`cmd/touko-desktop/main.go`](../cmd/touko-desktop/main.go)、[`desktop/app.go`](../internal/desktop/app.go)

| 精确符号 | 作用、输入与输出 | 副作用、错误与设计理由 |
| --- | --- | --- |
| `main` | `--state-dir`、`--port` → `desktop.Run`；`--version` 仅打印 buildinfo。 | 与 CLI 相同的 state 选择规则；参数非法退出码 2，Run 出错退出码 1。版本查询不启动窗口或 Service。窗口壳不另写业务状态。 |
| `Run` | stateDir、port、输出 → 直到退出的错误。 | 必须在主 goroutine 上运行：MyGo 在该线程驱动系统 WebView。复用 `app.Open` + `webview.Listen`（同一内嵌 UI 与 API），窗口关闭时隐藏而不退出，真正退出时先取消并 join 请求再关 SQLite。 |
| `session.onClose` | window close 事件。 | 未在退出流程时 `PreventDefault` 并隐藏窗口；MyGo 只要有一个窗口拒绝关闭就整体放弃退出，因此退出标志必须放行，否则托盘“退出”会静默挂住。 |
| `session.addTray` | 托盘图标与菜单。 | 菜单项：显示主窗口（`Show`+`Focus`）、在浏览器中打开（`browser.Open` 到 loopback URL）、打开工作区目录、分隔线、退出 TOUKO。AppIndicator 不提供点击事件，所以“重新打开窗口”必须是一个菜单项；打开失败写 stdout，不静默。 |
| `openWindow` | origin → `*mygo.Window`。 | 只允许导航回同一 loopback origin，`SetWindowOpenHandler` 返回 nil 拒绝弹窗，权限处理器一律拒绝；DevTools 关闭。窗口图标用嵌入的 256px PNG。 |

**两个 MyGo 0.5.5 陷阱（改这里前必须知道）：**

1. 托盘与被点击的菜单项必须一直被 Go 侧引用。`mygo` 用弱引用表按 uid 找回 `*MenuItem`，若 `*Tray`/`*Menu` 被 GC 回收，菜单仍然显示、点击却完全没有反应（症状与“菜单坏了”一样）。因此包级变量持有 `trayHandle`/`trayMenuRef`。
2. `Window.OnClose` 里无条件 `PreventDefault` 会同时阻止 `App.Quit()`：MyGo 的退出流程逐个关闭窗口，任何一个窗口拒绝就让整次退取消。隐藏窗口的逻辑必须先判断是否已在退出。

### [`internal/browser/browser.go`](../internal/browser/browser.go)

| 精确符号 | 作用、输入与输出 | 副作用、错误与设计理由 |
| --- | --- | --- |
| `Open` | ctx、target（URL 或本地路径）→ 启动结果。 | `touko serve` 打开页面与桌面托盘的两个“打开”项共用；只负责把目标交给系统默认处理程序，不猜测应用、不等待处理程序加载完。 |
| `launcher` | target → 平台命令与参数。 | darwin `open`、windows `rundll32 url.dll,FileProtocolHandler`、其余 `xdg-open`。 |
