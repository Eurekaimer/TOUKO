# TOUKO 项目记忆与后续入口

## 当前交付状态

批准的 CLI 核心 7 个步骤已全部完成并提交；离线真实 Git/SQLite、GitHub adapter、CLI、样例图片和可重复 demo 已交付。用户要求的详细中文操作/架构/逐函数教程已完成，README 提供阅读入口。本文件记录实际实现 commit、验证与下一阶段要求；保存本文的文档提交本身以 Git 历史为准，避免自引用 hash。

用户曾要求暂停保存记忆，随后明确要求继续并完成全部内容，且**每个实现 commit 后必须更新本文件**。步骤5、6、7完成后均已立即更新；不再把上次暂停时的未验证状态当作当前状态。后续也要保持代码、行为检查、对应文档和记忆同步，不要一次做大量用户无法追踪的设计变化。

## 用户确认：Webview 放到下一阶段

用户提出界面要求后，明确选择了「下一阶段实现 Webview」，本轮不增加 GUI，也没有占位界面。

- **视觉**：简洁、美观，参考 PicList 的图库布局与操作体验；不要做成只展示 CLI 输出的网页壳。
- **整理**：可在界面内部管理图片 tags（标签）和分类。现有领域字段 `Tags` / `Collections` 可复用；分类是否需要层级、是否直接对应 Collections，应在下一阶段与用户确认，不能擅自改 format v1。
- **架构**：复用 app 的 UUID、staged overlay、edit/rm、commit/sync/recovery；不要在前端另建第二套状态/存储来源。清楚区分 added/modified/committed 和未发布状态。
- 开始前先与用户确认页面布局和关键交互，以小步提交、同步记忆、实际浏览器视觉和功能验证推进。当前没有确定 Webview 技术栈，不要把猜测写成已选方案。

## 阅读顺序

1. [实际操作教程](docs/usage.md)：构建、demo、用户 logo、标签/分类、stdin、批量、删除、恢复、JSON。
2. [架构与设计教程](docs/code-guide.md)：三种身份/四种状态、依赖方向、阅读顺序、逐步调用链、失败窗口、设计取舍、安全修改练习。
3. [逐函数导航](docs/function-reference.md)：各生产包的函数/方法（含非导出 helpers）的输入输出、副作用、错误及存在理由，另有 demo Python helpers。
4. [Repository format](docs/repository-format.md)、[Local state](docs/local-state.md)、[Publication/recovery](docs/publication.md)、[GitHub 接入](docs/github.md)。

完整原批准计划的内部副本为 `local://touko-demo-core-plan.md`。若下一会话无法读取内部路径，以仓库实际代码、上述文档和本文件恢复；不要猜测不可读取的原文。

## 真实实现提交

初始历史为 `041bd15 Initial commit`。下列 hash 来自实际 git commit 输出；未 push，未修改 Git identity，未把用户 logo 加入提交。

| 步骤 | commit | message |
| --- | --- | --- |
| 1 | `b079269` | `feat(domain): define versioned content-addressed repository model` |
| 2 | `ee73fc6` | `feat(stage): persist streamed ingest and normalized local changes` |
| 3 | `c9d97e3` | `feat(repo): add local Git initialization and reconstructible sync` |
| 4 | `36cab59` | `feat(repo): plan atomic batches and reference-aware deletion` |
| 5 | `7a82b24` | `feat(app): publish recoverable commits and verify current-state deletion` |
| 6 | `7cad22f` | `feat(github): support safe repository publication and derived delivery URLs` |
| 7 | `270c0ac` | `test(demo): exercise recovery deletion and publication failure scenarios` |

每个 coherent implementation commit 前均运行并通过 go test ./... 与 go vet ./...。最终另有中文教程/README/本记忆的文档提交，不将其冒充第8个实现步骤。

## 可直接继续操作的真实 demo

实际运行 `bash examples/demo.sh` 后保留：

- ROOT：`/tmp/touko-demo.UNaCdFFM`
- Binary：`/tmp/touko-demo.UNaCdFFM/touko`
- Bare gallery：`/tmp/touko-demo.UNaCdFFM/gallery.git`
- 三个独立 state-dir：ROOT/device-a、ROOT/device-b、ROOT/device-c
- 最新 HEAD 仅有 blue/green 两个 Asset、两个 placement。red 的旧 bytes 仍在 Git history，符合删除边界。

```bash
ROOT=/tmp/touko-demo.UNaCdFFM
"$ROOT/touko" --state-dir "$ROOT/device-a" ls
ID=$("$ROOT/touko" --state-dir "$ROOT/device-a" add logo/TOUKO.png \
  --namespace blog --title '我的 TOUKO logo' --tag demo --collection '项目素材' --output id)
"$ROOT/touko" --state-dir "$ROOT/device-a" status --json
"$ROOT/touko" --state-dir "$ROOT/device-a" commit -m 'images: add my logo'
"$ROOT/touko" --state-dir "$ROOT/device-a" verify
```

以上是可供用户后续执行的命令，不是已在该保留目录内额外发布 logo 的声明。`/tmp` 可能被系统清理；消失后可重新 `bash examples/demo.sh`，记录新输出的 ROOT。脚本不自动删除结果，拒绝覆盖非空目录，不依赖 jq、不读取 token、不联网，构建时禁用模块/工具链网络下载；首次需先缓存 Go modules。Demo 依赖 Bash/Git/Go1.26+/Python3.8+标准库，CLI 本身不依赖 Python。

## 用户图片

`logo/TOUKO.png` 是用户提供的 PNG，实际检查为1254×1254 RGBA、1,322,142 bytes。SHA-256：`a6d1e0baae6395177c9795d35341b9c24bce709ab4df51648990ede0ead37068`。

已在独立 scratch 中通过真实 CLI 导入两次（不同 UUID/共用一个 object）、commit、verify、第二设备 fresh sync 完整 metadata 比较、删除第一引用保留 binary、删除末引用后 assets=0/objects=0。该 scratch 已清理；**用户原文件未修改、未删除、未加入实现提交**。

## 实际执行的最终 checks

以下均有真实成功输出，不是待运行清单：

```sh
go test ./...
go vet ./...
go test -race ./...
CGO_ENABLED=0 go build -o /tmp/touko-smoke ./cmd/touko
/tmp/touko-smoke --help
go test ./internal/repository -run '^$' -fuzz '^FuzzDecodeAsset$' -fuzztime=10s
bash examples/demo.sh
```

- Fuzz 实际 PASS，约11秒，1,422,200 次执行，无 crash。
- 三张自生成 fixture 经 `file` 确认为64×48、8-bit RGB PNG，无外部下载/版权依赖。
- Demo 实际断言：四 Asset add、仅一个 batch commit/三个 objects、fresh metadata、stale edit退出3且不复活删除、共享/末引用删除、第三设备、scratch SQLite 重建、Unicode piped stdin 重启保留/取消、local delivery拒绝、output/json mutation前拒绝、非空目标重跑拒绝且不改文件。
- `TestAtomicBatchRetryAfterBlobFailure`：100不同PNG、第63个blob失败，HEAD/cache不变且100 staged payloads保留，重开Store后retry只有一个visible child commit，fresh sync全metadata相等，verify100/100。
- 真实Git/SQLite故障回归包括 delete_modify、发布前blob/tree/commit/ref失败、丢UpdateRef响应及candidate后代恢复、真实SQLite RAISE(ABORT) HEAD UPDATE trigger后恢复、并发sibling发布不覆盖、损坏stage/candidate/object拒绝、journal base retry/sync、unknown outcome保留、offline reset。
- GitHub httptest 后端委托真实localgit，已通过 add/commit/freshsync/delete、401/403/404/429/5xx、tree/commit失败、ref拒绝、timeout/丢响应、truncated tree、token脱敏与stream取消检查。
- 新 CLI Markdown 转义/staged stderr、url JSON committed=false、缺token unauthorized 已通过独立可执行 smoke；public位置为隔离的 prospective view fixture，不是线上发布或CDN可用证明。

暂停后首次复跑定位到真实取消阻塞：HTTP Do 等待 request-body writer，source.Read却等Do错误分支才关闭。已改为独立context timeout/AfterFunc关闭source与pipe，用OnceFunc防止重复Close，并join goroutine；对应失败-before/passing-after测试保留。HTTP timeout测试服务还需消费请求body，才能观察连接取消，已修复其handler而非削弱行为断言。完整tests/vet/race随后通过。

## 当前结构和不可破坏的不变量

- Module `github.com/Eurekaimer/TOUKO`，go1.26.0；uuid1.6.0、flock0.13.1、cobra1.10.2、modernc/sqlite1.60.1。无CGO/ORM/网络框架/插件框架。
- `cmd → cli → app → domain/repository/index/staging`；CLI装配localgit/github端口，app不导入前端。image/staging负责流式payload，delivery只派生URL。
- AssetID/RepositoryID独立UUID；ObjectHash严格小写SHA256；Git BlobID/TreeID/Head不能混用。Asset UUID与文件名、hash独立。
- Format v1：`.touko/repository.json`；`.touko/assets/<id>.json`；`objects/<namespace>/sha256/<h[:2]>/<h[2:4]>/<hash>.<ext>`。未知版本失败，不自动迁移。
- namespace是物理路由：同namespace同bytes共用placement；跨namespace同hash但不同placement。删除按最终完整path引用，delete+samebytesadd保留对象，不清未知orphan/非TOUKO文件。
- PNG/JPEG/GIF，max100,000,000 bytes。source一次stream SHA256+temp，header从temp读，ext不信文件名。URL不入metadata。
- state优先级--state-dir→TOUKO_HOME→XDG_DATA_HOME/touko→~/.local/share/touko；0700目录/0600文件，稳定.lock非阻塞flock。不删stale锁。
- SQLite schema1/WAL/单连接：repository_state/assets/changes/publication。只有committed表是可重建cache；有stage/journal不能删DB。config不含token。
- 多次stage归一化，first change绑定baseHEAD；overlay隐藏delete；edit/rm在base+stagedadds并集解UUID；无文件名猜测。tags/collections trim/去空/去重/排序，保持大小写。
- PlanCommit无IO；coordinator流式校验、固定parent/base tree、journal-before-ref、candidate/descendant recovery、transactional finalize、最后cleanup。CLI不能直接更新ref。
- localgit严格expected-old update-ref CAS，显式TOUKO identity，SHA1 repository only；tree保留mode/symlink/gitlink/非TOUKO文件。candidate比较leaf entries，不比较派生directory SHA。
- GitHub生产endpoint固定api.github.com，API header2026-03-10；rawblob读取，pipe+base64流式上传，tree删除显式sha:null，base_tree保留文件，truncated tree拒绝部分cache，无隐藏重试。阻塞型自定义Reader须可取消或io.Closer；CLI文件符合。

## 已知限制与未执行项

- **真实GitHub live smoke未执行**：用户没有提供专用gallery repo/token。不能拿Eurekaimer/TOUKO代码仓库充当gallery。下一步按docs/github.md：用户自行建专用repo并初始化README，在执行机交互式设置repository-scoped GITHUB_TOKEN，auth/use/init/add/commit/url/verify，换全新state-dir做恢复/删除验收。不要让用户把token发聊天。
- GitHub force:false仅fast-forward，不是任意外部force/reset下的严格CAS。分支应禁止force-push；TOUKO固定parent/HEADchecks保护正常non-force并发。不能宣称解决外部管理员rewrite race。
- localgit/private GitHub无公网URL/Markdown；public推导URL不等于commit完成或CDN传播。jsDelivr延迟不是远端commit失败。
- 普通rm只保证latest HEAD，不purge history/CDN。没有GUI、thumbnail、R2、OAuthlogin、自动merge、namespace move/objectreplace或假migration。
- 用户logo仍是用户未提交文件；不得为“清理工作区”将它删除或擅自stage。其他后续用户变更也不加入实现提交。

## 后续开发方式

先读操作教程和架构教程，再按函数导航定位，不要猜API。每次先明确用户可观察行为与修改层，沿已有模式写行为测试、做真实CLI/浏览器smoke、更新docs与本记忆，然后小步commit。避免实现断言/源文本测试、mock echo、假Store、占位命令和掩盖失败的fallback。新增Webview前让用户理解并选择视觉与交互，始终复用已有核心。

## 最终教程检查

已执行一次性文档 smoke：70 个本地 Markdown 链接全部指向存在的目标；Go AST 收集到的186个生产函数/方法名称均出现在逐函数参考中。该检查只证明链接可达与名称覆盖，不冒充逐条语义自动证明；教程内容另按模块/调用链与实现核对。检查脚本已删除。README、code-guide、function-reference 与本记忆由最终文档提交统一保存；不再有待实现的本轮功能。
