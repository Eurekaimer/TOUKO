# TOUKO 项目记忆与后续入口

**用户语言偏好：一律用中文回复。**

## 当前交付状态

CLI 核心、中文教程与本轮 Webview 已实现并分小步提交（最新 `407a5c1`）。整页重设计已按 Anthropic `frontend-design` skill 重建并经主代理独立复核（CDP 实测字体、契约 id、折行、对比度）。真实 GitHub live smoke 仍未执行，缺少用户授权的专用 gallery/token；`examples/github-acceptance.sh` 已就绪待用户执行。无人值守期间的迭代协议见 `local://touko-iteration-protocol.md`，日志见 `local://touko-iteration-log.md`。

用户曾要求暂停保存记忆，随后明确要求继续并完成全部内容，且**每个实现 commit 后必须更新本文件**。步骤5、6、7完成后均已立即更新；不再把上次暂停时的未验证状态当作当前状态。后续也要保持代码、行为检查、对应文档和记忆同步，不要一次做大量用户无法追踪的设计变化。

## 当前 Webview 要求与确定的设计

- 用户要求简洁、美观、设计感强、字体好看，参考 PicList 的图库布局；可在界面内整理 tags 和分类。分类直接对应现有平面 `Collections`，不引入未经要求的层级或 format 迁移。
- 最新视觉参考：《终将成为妳》的校服与两位主人公配色。使用奶油米色、深藏蓝作底，绿色主色，侑的珊瑚橙与灯子的暗樱红少量点缀。
- 用户点名要求：**所有**英文部分与中文部分都要换字体——英文用 Monaspace Radon，中文用 LXGW 文楷（霞鹜文楷）。当前字体：Monaspace Radon（拉丁/数字/UI，变量轴 wght 200-800、wdth 100-125、slnt 0…-11，无 CJK）、LXGW WenKai（中文，Regular 400 + Medium 500，SIL OFL 且其附加许可明确允许为 web 交付子集化为 WOFF2）、`TOUKO Wordmark`（Quicksand 圆几何字体，子集出 T/O/U/K 四字形，仅用于字标，1.3KB）与 `Playfair Display` 斜体（hero 标题专用；刻意不用带 opsz 光学尺寸轴的字面，Bodoni 因 opsz 在大字号变细被用户否决）。五者均 SIL OFL，许可证内嵌。此前临时使用的 Manrope / Source Sans 3 / Italianno / Inter / Newsreader / IBM Plex Sans SC 与 `static/brand/` 目录均已删除。
- 用户要求左上角 TOUKO 字标要更「规整圆润、有设计感」，按 logotype 处理而不是默认排字；用户已确认字标**保持 700** 字重。
- 用户选定：favicon 改为**方案感知 SVG**（深色标签栏用纸底版，浅色用原稿）；静态资源改为 **ETag + no-cache** 重新校验（内嵌文件无 mtime，max-age 会导致陈旧预览，故不采用）。
- 用户要求参照设计规范而不是即兴发挥：已取用 Anthropic 公开的 `frontend-design` skill 作为审查标准（含其「AI 生成页面」常见特征清单），用于修正流于模板的配色/排版特征。
- 用户提到“可以考虑使用mygo制作”，含义尚不确定且为可选建议；不据此擅自引入新框架。当前确定采用 Go `net/http` + `embed` + 原生 HTML/CSS/JS，无 Node 构建依赖、无运行时字体/脚本 CDN。
- `touko serve --port 7788` 仅绑定127.0.0.1；GitHub token只保留服务端环境变量，不进入浏览器、localStorage、config或SQLite。API校验Host/Origin/CSRF，图片仅按Asset ID读取。
- Webview复用 app 的 staging、commit/sync/recovery，不另写存储/发布逻辑；已在真实浏览器完成下面记录的验收。今后每次变更仍需实际运行相应路径，再小步提交、更新本记忆及教程。

## Webview 本轮实测与正在展示的预览

- 启动入口：`touko --state-dir PATH serve --port 7788`，0为动态端口。源码入口是 `internal/cli/serve.go`、`internal/webview/{server,api,preview,static}.go` 与 `static/{index.html,style.css,app.js}`；`app/view.go` 的 ReadView 从同一 SQLite snapshot 生成页面视图。
- 用户中途要求打开成果窗口：已用可见浏览器打开，并用 `xdg-open http://127.0.0.1:7799` 向现有桌面浏览器发送打开请求。**预览服务仍运行于 http://127.0.0.1:7799/**，当前进程名 `touko-7799-redesign`；binary `/tmp/touko-7799`（重设计构建），state `/tmp/touko-webview-acceptance/state-a`，真实 bare gallery `/tmp/touko-webview-acceptance/gallery.git`。此前 7799 曾长期挂在旧构建上并向用户展示了过期页面；每次前端改动后必须重建该 binary 并重启预览，不能只重建而不替换正在服务的进程。
- Webview 前端改动由子代理执行，主代理只做集成与验证；设计审查使用 Anthropic `frontend-design` skill，避免即兴取舍。
- 实测桌面1440×1024、移动390×844；移动无横向溢出，侧栏筛选可收起，详情从原图顶部打开，支持Esc/焦点恢复/reduced-motion。原图、favicon和内嵌字体请求均实际200。
- 界面导入用户TOUKO原图和3张自生成PNG验收图；4个add一次暂存。修改中文title/tags/collections，筛选得到指定单图，刷新后metadata仍完整；新增图多次编辑仍是同一add。
- 界面发布后verify为4 assets/4 objects；独立state-b sync完整还原。再次导入同一logo得到不同UUID与相同hash：5 assets/4 objects。界面移除第一份并提交后为4 assets/4 objects，另一引用仍可预览；最终HEAD `70a709d11a88eef755b09f9338225e0c3bfa8365`。
- 多选全部4图、确认暂存移除后画面0图/4 changes；明确确认丢弃后恢复4图/0 changes。上传bad.png真实400，错误可见且标题/文件仍保留。
- B在旧HEAD编辑已被A删除的UUID，界面commit真实返回`conflict: delete_modify`，保留1条update与旧HEAD，不自动reset。B经明确丢弃、sync后与A的完整AssetView metadata及HEAD一致，被删UUID不存在。
- 实际HTTP拒绝wrong Host和缺CSRF均403；原图SHA-256与Asset.Object.Hash相同。16个并发原图请求全200。原实现32个并发请求有30个409，已用短snapshot gate修复并保留`TestConcurrentGalleryPreviews`回归；网络/图片流不占工作区锁。
- `go test ./...`、`go vet ./...`、`go test -race ./...`已通过；真实服务曾Ctrl+C正常exit0。完整测试在实现提交前再统一执行，不能把这条记录替代后续变更检查。
- 原始`logo/TOUKO.png`未修改，sha256仍为`a6d1e0baae6395177c9795d35341b9c24bce709ab4df51648990ede0ead37068`；只提交用户允许的webp/favicon衍生资源与字体OFL，不提交原始logo。
- 完整使用及专用GitHub仓库验收在[Webview教程](docs/webview.md)；页面状态、HTTP接口与每个具名控制器函数已补入代码导读/函数导航。未引入Node依赖或外部运行时CDN。

## 阅读顺序

1. [实际操作教程](docs/usage.md)：构建、demo、用户 logo、标签/分类、stdin、批量、删除、恢复、JSON。
2. [架构与设计教程](docs/code-guide.md)：三种身份/四种状态、依赖方向、阅读顺序、逐步调用链、失败窗口、设计取舍、安全修改练习。
3. [逐函数导航](docs/function-reference.md)：各生产包的函数/方法（含非导出 helpers）的输入输出、副作用、错误及存在理由，另有 demo Python helpers。
4. [Webview 使用与真实 GitHub 验收](docs/webview.md)，以及 [Repository format](docs/repository-format.md)、[Local state](docs/local-state.md)、[Publication/recovery](docs/publication.md)、[GitHub 接入](docs/github.md)。

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
| 8 | `0e8c50c` | `docs: explain architecture functions and hands-on workflows` |
| 9 | `6526ae9` | `test(github): add one-command live acceptance script` |
| 10 | `1821343` | `feat(webview): add the local gallery interface` |
| 11 | `3fc5815` | `feat(desktop): serve the gallery and open it in a MyGo window` |
| 12 | `3fb95a9` | `feat(webview): set the display face to Monaspace Radon` |
| 13 | `188cef1` | `docs: document the gallery interface, desktop shell and acceptance flow` |
| 14 | `a10fe26` | `docs(memory): record the webview commits and current verification state` |
| 15 | `15b9356` | `docs(memory): record the desktop shell smoke run` |
| 16 | `7ee4f06` | `feat(webview): add the LXGW WenKai and wordmark fonts` |
| 17 | `3e958d9` | `feat(webview): add Bodoni Moda italic for the hero headline` |
| 18 | `407a5c1` | `feat(webview): rebuild the interface around a mounted-photograph system` |
| 19 | `0d506a5` | `feat(webview): cache assets safely and make the hero italic legible` |

每个 coherent implementation commit 前均运行并通过 `gofmt -l`、`go vet ./...` 与 `go test ./...`；Webview/Radon 相关提交另跑 `go test -race ./...`。Webview 提交前还在真实浏览器核验了契约钩子（117 id + 25 JS class 全部存在）、字体真实加载与交互路径。hash 均来自实际 `git commit` 输出，不猜测未生成的 hash。

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

- **真实GitHub live smoke未执行**：用户没有提供专用gallery repo/token。不能拿Eurekaimer/TOUKO代码仓库充当gallery。已提供 `examples/github-acceptance.sh`（只从 `GITHUB_TOKEN` 读取凭据、拒绝源码仓库作为gallery、非零退出保留现场），其断言已在真实本地 bare Git 仓库上完整跑通（init幂等、批量提交、两个provider URL一致性、verify、第二设备逐字段metadata一致、末引用删除在第三设备不可见）；GitHub专属步骤（token认证、public raw bytes校验）仍待用户提供专用repo/token后执行。执行方法见docs/github.md。不要让用户把token发聊天。
- 原生桌面壳已存在并在本机实测：`cmd/touko-desktop` 用 MyGo 打开同一 Webview（`internal/desktop/app.go`）。真实启动 `/tmp/touko-desktop --state-dir … --port 7807` 打印 `TOUKO MyGo: http://127.0.0.1:7807`、窗口进程存活、页面 HTTP 200（标题「图片库 · TOUKO」），终止后进程退出且端口释放。缺系统 webview 时明确报错而非静默回退。
- GitHub force:false仅fast-forward，不是任意外部force/reset下的严格CAS。分支应禁止force-push；TOUKO固定parent/HEADchecks保护正常non-force并发。不能宣称解决外部管理员rewrite race。
- localgit/private GitHub无公网URL/Markdown；public推导URL不等于commit完成或CDN传播。jsDelivr延迟不是远端commit失败。
- 普通rm只保证latest HEAD，不purge history/CDN。已有本地浏览器Webview；没有原生桌面壳、thumbnail、R2、OAuthlogin、自动merge、namespace move/objectreplace或假migration。
- 用户logo仍是用户未提交文件；不得为“清理工作区”将它删除或擅自stage。其他后续用户变更也不加入实现提交。

## 后续开发方式

先读操作教程和架构教程，再按函数导航定位，不要猜API。每次先明确用户可观察行为与修改层，沿已有模式做行为测试和真实CLI/浏览器smoke、更新docs与本记忆，然后小步commit。避免实现断言/源文本测试、mock echo、假Store、占位命令和掩盖失败的fallback。Webview视觉方向已获用户明确要求，不重复索取确认；始终复用现有核心。

## CLI 阶段的教程检查记录

CLI教程阶段执行过一次性文档smoke：70个本地Markdown链接全部可达；Go AST收集到的186个生产函数/方法名称均出现在函数参考中。它只证明当时的链接/名称覆盖，不是本轮Webview新增代码的验证，也不证明逐条语义；本轮的实际检查结果另行记录。
