# TOUKO 开发交接记忆

## 当前恢复状态

用户已要求继续，并新增要求：每个实现 commit 后立即更新本文件；最终提供按模块、函数和设计理念组织的详细中文教程。步骤5完整 go test/vet/race 与实际用户 logo CLI 发布/恢复/共享删除/末引用删除 smoke 已通过，准备提交；步骤6 adapter 的取消阻塞已修复且 HTTP 测试通过，但 CLI 尚待接线。下文保留上次暂停时的历史快照，**最新进度以本节及后续进度记录为准**，不再表示当前暂停。

批准的完整 7 步计划保存在 `local://touko-demo-core-plan.md`。后续会话若读不到该内部路径，以本文件和仓库现有代码恢复；不要猜测不可读取的原文。目标是同时交付真实离线 Git demo 和真实 GitHub adapter，不是 scaffold。验收闭环：repo init/use → add → status → commit → sync → ls/find/show/url → edit/rm → commit → verify。用户暂停并未批准缩减最终交付范围。

## 已完成并提交：步骤 1–4

基于初始 `041bd15 Initial commit`，已创建以下真实提交；没有 push：

| 步骤 | commit | message |
| --- | --- | --- |
| 1 | `b079269` | `feat(domain): define versioned content-addressed repository model` |
| 2 | `ee73fc6` | `feat(stage): persist streamed ingest and normalized local changes` |
| 3 | `c9d97e3` | `feat(repo): add local Git initialization and reconstructible sync` |
| 4 | `36cab59` | `feat(repo): plan atomic batches and reference-aware deletion` |

每个上述 commit 前均实际通过 `go test ./...` 与 `go vet ./...`。步骤 2 引入文件锁后还实际通过 `go test -race ./...`。步骤 3 的真实 bare Git 测试包括 executable/symlink/gitlink 和非 TOUKO 文件保留、fresh sync 完整 metadata、损坏远端不替换旧 cache。步骤 4 已覆盖共享对象/末引用删除、delete+same-bytes-add、跨 namespace 隔离、stale base、delete+metadata edit。

已实际执行的独立 smoke（不是仅测试）：

- 步骤 1：Go 可执行程序生成 UUID，编码/解码 Unicode Asset，打印 canonical placement 与稳定 JSON。
- 步骤 2：真实 SQLite + PNG ingest，关闭再打开 Service，staged UUID/原文件名保留，add→rm 恢复 clean。
- 步骤 3：构建 `/tmp/touko-step3`，运行实际 CLI `--help`、local repo init、add Unicode PNG、status/show、edit、find、rm；每次独立进程，JSON/state 与预期一致。
- 步骤 4：Go 可执行程序证明删一条共享引用仅删 metadata，删完两条才删除 canonical binary。

所有上述 throwaway Go smoke 文件及其 scratch 数据已删除。`/tmp/touko-step3` 是步骤 3 的旧 binary，不包含后续 publication；恢复后应重新构建，不要把旧 binary 当最新实现。

## 已落实的架构/协议

- Module `github.com/Eurekaimer/TOUKO`，`go 1.26.0`。直接依赖固定 uuid v1.6.0、flock v0.13.1、cobra v1.10.2、modernc/sqlite v1.60.1。纯 Go SQLite，无 ORM/CGO 依赖。
- 依赖方向：cmd → cli → app → domain/repository/index/staging；localgit/github 实现窄 Git port。application 不导入前端。
- AssetID/RepositoryID 为独立 UUID；ObjectHash 为严格 64 字符小写 SHA-256；Git BlobID/TreeID/RepositoryHead 不混用。
- Manifest `.touko/repository.json` 为 format v1；metadata `.touko/assets/<id>.json`；objects 路径为 `objects/<namespace>/sha256/<h[:2]>/<h[2:4]>/<hash>.<ext>`。
- Namespace 是物理路由。同 bytes 在同 namespace 共用 placement；跨 namespace 同 hash 但不同物理路径。引用计数用完整 placement path。
- PNG/JPEG/GIF，单图最大 100,000,000 bytes；source 一次流式读取、SHA256 + temp，再从 temp 解 header。stage payload 原子落盘；已有对象校验，不覆盖坏 bytes。
- State 优先级：--state-dir → TOUKO_HOME → XDG_DATA_HOME/touko → ~/.local/share/touko。0700 目录/0600 文件，稳定 `.lock` 非阻塞 flock。
- SQLite schema1：repository_state/assets/changes/publication。committed snapshot 可重建；changes/journal 是未发布状态，删除 DB 会丢弃它们。config 不含 token。
- Staging 归一化、overlay、至少8字符 UUID prefix、原子批量 add/rm、delete→edit conflict 已有测试。
- Git 使用真实 plumbing 和本地 update-ref expected-old CAS，显式 TOUKO author/committer，SHA1 repo only；绝不 shell 拼接。
- `PlanCommit` 无 IO，先算最终 Asset map，再算引用删除；不清理未知 orphan/非 TOUKO 文件，不覆盖已有 canonical placement。
- 现有文档：README.md、docs/repository-format.md、docs/local-state.md。步骤 5/6 的 docs 尚未补全。

## 未提交工作：步骤 5 为主，步骤 6 独立开发已落盘

暂停时 `git status` 显示 main 领先 origin/main 4 个提交；下列修改未提交，**不要 reset/clean 或当作可丢弃临时文件**：

已修改 tracked：
- `internal/app/stage.go`（删除旧 ResetLocal）
- `internal/app/sync.go`（接入 journal recovery）
- `internal/cli/root.go`（接 commit/verify 和新 Reset；仍只有 localgit backend）

新文件/目录：
- `internal/app/commit.go`, `commit_test.go`, `repo.go`, `verify.go`
- `internal/cli/publication.go`, `publication_test.go`
- `internal/repository/verify.go`, `verify_test.go`
- `internal/github/client.go`, `repository.go`, `repository_test.go`
- `internal/delivery/url.go`, `url_test.go`
- 用户文件 `logo/TOUKO.png`
- 本文件 `MEMORY.md`

### 步骤 5 当前代码

API：

- `(*app.Service).Commit(ctx, repository.Git, message) (CommitResult,error)`；JSON head/change_count/asset_count。
- `Initialize(ctx, Git)` 与 `Sync(ctx, Git)` 返回 SyncResult head/asset_count。
- `Reset(ctx, Git)` 替代 ResetLocal；CLI 已迁移。普通无 journal 的 reset 可离线；有 journal 必须确认远端关系。
- `repository.VerifyObject(ctx, Reader, BlobID, ContentObject) error` 流式 full hash/size/header 校验。
- `repository.VerifySnapshot(ctx, Reader, Snapshot) (VerifyResult,error)`；head/assets/objects，按 unique placement 计数，同 Git blob 校验可复用。
- `(*Service).Verify(ctx, Git)` 固定远端 HEAD 读取 snapshot 后深度校验；CLI commit/verify 已接线。

Coordinator 已实现 journal-before-ref、固定 base parent/base tree、staged hash/size 重校验、复用 remote object 校验、candidate/descendant 恢复、base retry 复用原 candidate、sync 清未发布 journal 保留 stage、未知发布结果保留 journal、SQLite 失败后恢复、published-head cleanup 错误报告。CLI 不直接更新 ref。

本轮集成时已修正两个问题：

1. `localgit.Tree` 包含 directory entries（-r -t）。candidate validation 现在仅比较 leaf entries，不能拿 base 的 directory SHA 与 changed tree 的 directory SHA 比较；保留 blob/gitlink 的精确 mode/type/SHA/size。
2. 无 journal 的 `Reset` 不应额外访问 remote；已去掉该额外验证，并加离线 reset 行为测试。

新增 app 故障测试包括：DeleteModifyConflictDoesNotResurrect、各 publication 前失败/重试、lost ref response（含 descendant）、真实 SQLite BEFORE UPDATE OF head RAISE(ABORT) trigger、并发 sibling commits、不完整/损坏 candidate 拒绝、stage 损坏、unknown outcome、journal base retry/sync、init recovery、offline reset。不是 fake Store。

### 步骤 6 当前代码与缺口

GitHub adapter 与 delivery 已独立开发，但**尚未完成整步验证和 CLI 接入**：

- `github.New(location, token)` 固定生产 api.github.com；`NewWithClient(location, token, apiURL, client)` 仅测试注入。
- `Discover(ctx,owner,name,branch)` 读取 default_branch/private 并检查 ref；`User(ctx)` GET /user。
- APIError 可 errors.As，Status/Endpoint/Message，APIStatus()；权限错误 unwrap unauthorized；token 字符串被脱敏。
- 固定 API header 2026-03-10，raw blob streaming；base64 io.Pipe upload、显式 null tree deletion、base_tree、唯一 parent、force:false、compare ancestry、truncated tree 拒绝。
- HTTP tests 使用 httptest + 真实 localgit 后端，并通过 app coordinator 测试发布/恢复/删除和故障。
- delivery.BuildURL 支持 public GitHub jsdelivr/raw，branch/path 安全转义；local/private 返回 ErrDeliveryUnavailable。

仍缺：CLI github backend 装配、`auth status`、`repo use OWNER/REPO`、`url`、`add --output markdown`（mutation 前拒绝 local/private、--json/output 冲突，staged link 提示与 Markdown escaping）、APIError→api_error CLI 分类、对应真实行为检查和 docs。不能把 adapter 文件存在当作步骤6完成。

GitHub live smoke 未执行：没有用户提供的专用 gallery repo/token。不能用 Eurekaimer/TOUKO 代码仓库作为 gallery。token 仅由用户在执行机设置 GITHUB_TOKEN，不发进聊天，不落 config/SQLite/argv。GitHub 空 repo 必须先初始化 README；force:false 不是对任意外部 force/reset 的严格 CAS；验收分支应禁止 force push。

## 暂停时最近一轮检查：仅部分结果，不得宣称全过

运行的流水线为：

```sh
gofmt -w internal/app internal/repository/verify.go internal/repository/verify_test.go internal/cli internal/github internal/delivery
go test ./...
go vet ./...
go test -race ./...
```

这是一个 `&&` 流水线。gofmt 已执行；`go test ./...` 已实际输出：

- cmd/touko：no test files
- internal/app：PASS（1.621s）
- internal/cli：PASS（0.269s）
- internal/delivery：PASS（0.002s）
- internal/domain：PASS（cached）

**随后用户要求停止，后台 job `bg_1` 已取消。没有收到本轮 github/image/index/localgit/repository/staging 的完成输出，也没有本轮 vet/race 成功证据。不可写成完整测试通过。** GitHub package 为什么尚未输出未调查；恢复后优先有 timeout 地运行其测试以判断是否有 streaming/cancellation 阻塞，不能凭取消推断测试失败。

步骤5新增代码还没有实际 CLI smoke，也尚未补 docs/创建第5个commit。第5个 commit 应只 stage step5 文件，不把 step6、用户 logo 或无关修改混进去。

## 恢复顺序

1. 先读本记忆，确认用户希望恢复。保留未提交代码与用户 logo。
2. 解决完整测试流水线：建议先 `go test -v -timeout 60s ./internal/github` 定位潜在等待，再完整 go test/vet/race。必要时修真实问题，禁止假成功/削弱测试。
3. 完成步骤5真实 CLI init/add/commit/fresh sync/delete/verify/recovery smoke；补 docs，通过 go test ./... 和 go vet ./... 后提交 `feat(app): publish recoverable commits and verify current-state deletion`。
4. 完成步骤6上述 CLI 接线、HTTP fault coverage/URL/Markdown 行为验证和 docs，通过检查后提交 `feat(github): support safe repository publication and derived delivery URLs`。
5. 步骤7尚未开始：生成自己拥有版权的64×48 red/green/blue PNG；examples/demo.sh 使用实际 CLI+三个 state-dir，四Asset/三object一次batch、stale edit退出3、共享与末引用删除、fresh sync、删scratch DB重建、Unicode stdin add/rm；保留可继续操作的demo目录。不依赖jq/网络/token，拒绝非空目标。
6. 补100张不同PNG、第63个blob失败、retry仅一个可见commit的真实Git/SQLite回归；保留 FuzzDecodeAsset。
7. 最终运行 go test ./...、go vet ./...、go test -race ./...、CGO_ENABLED=0 go build、CLI --help、repository fuzz10s、bash examples/demo.sh。用户 logo 应额外用于实际CLI操作，不取代计划的三张样例。仅 stage 本任务文件，不推送、不改Git identity。
8. 第7个提交：`test(demo): exercise recovery deletion and publication failure scenarios`。中文最终报告列已实现、实际checks、7个真实hash、known limits、GitHub安全连接方式；没有凭据就明确live smoke未执行。

## 工具/代理恢复提示

- 已结束代理成果：`agent://PublicationCoordinator`、`agent://GitHubPort`（内部链接若不可用，以代码为准）。没有仍需继续开发的代理任务。
- 主会话 xd://lsp 可用；某些子会话未挂载。变更 exported symbol 前先查询 references。旧 ResetLocal references 已查，仅原 app 声明和 CLI 调用，现已迁移。
- 已报告工具 stale LSP diagnostics：创建新root smoke文件时误报已删除旧smoke的main redeclared；真实Go检查无此问题。不要为这种缓存误报改生产结构。
