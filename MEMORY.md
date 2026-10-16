# TOUKO 项目记忆与后续入口

**用户语言偏好：一律用中文回复。**

## 当前交付状态

**最新交接（2026-10-17）。** 上一轮留在工作区的增量实现已按模块分阶段提交（见下表；本文件所在提交即记录这些 hash 的 memory 提交）；下表记录每个阶段的实际提交与实测结果。`logo/` 始终是用户文件，禁止擅自提交或清理。

本轮在隔离 state（`/tmp/touko-e2e.GzXGj8Ox`）与端口 `17888` 上用真实 `gh` 登录实测（binary `/tmp/touko-review`）：

- **仓库/分支两级联动下拉已实现并真实验收。** 打开“连接 / 切换仓库”选 GitHub 后自动读取全部 **63** 个可访问仓库（分页逻辑有单测），选仓库后读取全部 **4** 个分支，与 `gh api repos/Eurekaimer/touko-gallery-test/branches` 逐项一致；默认分支 `main` 自动预选；快速连切两个仓库后只保留最后所选仓库的分支（无旧请求串入）；重开已连接工作区会预选当前仓库与当前分支；`connect` 在无有效选择时禁用。
- **工作区隔离已实测。** `/w/default/`（GitHub smoke 分支，6 assets，clone 本地读图可用）与一个本地 bare 仓库工作区并存，stage/HEAD/assets 互不可见；在一个工作区暂存 2 张后往返导航，暂存计数与内容保持。
- **目标目录上传与 format v2 已实测。** UI 填 `images/blog` 上传 2 张后提交，真实路径为 `images/blog/<hash>.png`，首次显式 path 提交把 `repository.json` 原子升级为 format v2。CLI 同样：canonical 普通提交保持 v1 不升级，随后 `--directory` 提交升级 v2 且 `verify` 通过。
- **CLI 实测：** `auth status`（登录名 + 来源）、`repo list`（63 条与 `/user/repos` 一致）、`reindex`（物理损坏 `index.db` 时保留 `index-corrupt-*` 与已暂存变更，随后仍可提交）、`import`（含 `--directory`；无后缀 / 大写扩展名 / Unicode 路径按真实 header 识别，原 blob 与路径不动，第二设备 `sync` 逐字段还原）。
- **schema1 → schema2 迁移实测：** 旧二进制留下的未发布变更被 `workspace.json` 承接，新二进制读取后变更仍在，旧二进制随后 fail-closed 拒绝 `user_version 2`。

仍未完成的验收：跨工作区的完整冲突/恢复组合、Web 端“删除 + 重启 + 多筛选”组合、剪贴板自动复制、MyGo 内存量测与 AUR。`logo/` 未加入任何实现提交。

### 提交进度（按模块）

| 阶段 | 内容 | commit |
| --- | --- | --- |
| 1 | format v2 / 显式路径 / 原路径 import | `3a99579` |
| 2 | index 恢复（schema2 + workspace.json + reindex） | `860f7ca` |
| 3 | credential 与 GitHub 仓库/分支列表 | `3e15310` |
| 4 | CLI 接线（auth/repo list/reindex/import/serve） | `3544720` |
| 5 | Webview 工作区与仓库/分支联动下拉 | `1713f27` |
| 6 | 文档与记忆 | `76ff6ce` |

每个阶段提交前都在 detached HEAD worktree 里按累计文件集跑过 `go build ./...` 与 `go test ./...`，确认中间提交可编译、可测试；全部提交后再在最终树跑一次 `gofmt -l`、`go vet ./...`、`go test -race -count=1 ./...`（全部通过）。hash 均取自实际 `git log` 输出。

### 本轮已实现（分阶段提交）

| 范围 | 实现与关键入口 |
| --- | --- |
| 认证 | 新 `internal/credential`：`GITHUB_TOKEN` → `GH_TOKEN` → `gh auth token`；CLI `auth login/status`、`repo list`。不持久化 token；网页在后端复用 gh 登录。 |
| 仓库列表 | `internal/github/repositories.go` 完整分页列出可访问仓库，含 private/can_push；`branches.go` 分页列出分支（name/protected）。Web `GET /api/repos` 与 `GET /api/branches?repository=OWNER/REPO`；前端为自动加载的**两级联动下拉**，已真实验收（63 仓库 / 4 分支与 `gh api` 一致）。 |
| 独立工作区 | `internal/webview/workspaces.go`：每个仓库/分支独立 Service、DB、clone、stage、journal；新 URL `/w/<key>/`，根 state 保留为 default。跨工作区隔离与暂存保留已在真实浏览器验证。 |
| 索引恢复 | SQLite schema2 仅 committed cache；独立 `workspace.json` 耐久保存 changes/base/journal，健康 schema1 自动迁移。新 `index/{workspace,rebuild}.go`、`app/reindex.go`；CLI/Web `reindex`。 |
| PicGo 原路径 | `Asset.Path`、`repository.ObjectPath`；`app/import.go` 扫固定 Git tree，按真实 header 识别 PNG/JPEG/GIF，包括无后缀图片，原路径/原 blob 不动，只暂存 metadata。首次提交显式 path 原子升级 format v2；普通 canonical v1 不升级。 |
| 目录与链接 | `add --directory`、HTTP multipart directory、Web 目录输入/筛选、详情真实路径与 Markdown。`delivery.BuildURL` 已修复为接受安全实际路径，不再只接受 canonical 六段；危险路径拒绝，特殊字符逐段编码。 |
| 同步与恢复 | `app/refresh.go` 的 `SyncWith` 在同一工作区锁内恢复/更新 clone/索引；CLI/Web 普通 sync 先 fetch 再读，journal 关系用 API。`cli/root.go` 的 reset 已改用 backend，不能用浅 clone 判断已发布 candidate。 |
| 启动 | `serve` 默认打开浏览器，支持 `--no-open`；打开失败明确提示但服务仍运行。现有 MyGo 壳保留，本轮未优化桌面入口。 |

### 本轮实际验证

- 全包 `go test -race ./...`、`go vet ./...`、CLI build 已通过；随后修复 CLI reset 后又通过 `go test -race ./internal/cli ./internal/app`、`go vet ./...` 与 CLI build。最初 index 测试缺 `errors` import 已用 gopls Organize Imports 修正，不是仍在失败。
- 交接文档检查通过：10 个 Markdown、113 个本地文件链接可达，代码围栏成对，未发现真实令牌形态。该检查只验证文档结构/文件可达性，不替代功能验收。
- 真实 CLI：旧 schema1 打开后 3 张图片保留；PicGo Unicode/无后缀/大小写扩展名导入保持原 Git blob，重复导入幂等，format v2 与第二设备逐字段恢复通过；目录上传、编辑、删除与新进程持久化通过。
- 实际制造 `index.db` 物理损坏：`reindex` 保留损坏副本、保留已暂存上传，修复后能发布；另删除第二设备 DB 后完整重建，ID/path/tags/collections/HEAD 保持。第二设备一次 sync 即读到新 HEAD。
- `Eurekaimer/MyIMGs` **只读克隆到本地 bare 副本**，仅初始化这个本地副本；`img/` 导入得到 **204 张图片，其中 16 张无扩展名**。204 条仍是本地暂存，未向原仓库发布，未改其 main。
- 真实公开测试仓库 `Eurekaimer/touko-gallery-test` 使用独立分支 `touko-handoff-20261016-133916`，不向 main 写入。浏览器实际导入 `legacy/旧图 无后缀`、预览成功、发布 metadata，再编辑标题/标签并发布；最后由自动化观察的发布 HEAD 为 `a57319aecbbf337e6a9c84967a16bb49d02b10a9`，用户之后可能继续修改。
- 为隔离自动化另建分支 `touko-handoff-20261016-133916-smoke`。真实复现「远端已发布、clone/缓存仍旧、journal 未落账」：旧 CLI reset 错清 journal 并留下缺图旧缓存；修复后在**同一故障现场**通过 API 验证 candidate、恢复新 HEAD 和图片、清 stage/journal，远端 HEAD 不变。
- 真实浏览器确认原路径图 Gallery/Detail 均加载、原生 lazy loading、无横向溢出；发现并修复 BuildURL 拒绝原路径的 bug。已验证 Markdown 正确生成到文本框。**剪贴板权限受限，未证明自动复制或原生 Ctrl+C 成功；不得写成“复制全部通过”。**
- 本轮（2026-10-17）补齐的浏览器端到端：仓库/分支联动下拉（含与 `gh api` 逐项比对、快速切换防串、已连接工作区预选）、跨工作区暂存保留、目录上传 UI 全流程（`images/blog` → format v2）。仍**未**完成：Web 端一次 sync 的冲突现场、删除/多筛选/重启的组合验证、剪贴板自动复制；不要拿已有单元回归代替这些操作。

### 运行现场与用户数据边界

- 验收服务 `http://127.0.0.1:17884/`，进程服务名 `touko-incremental-review-v2`，state `/tmp/touko-handoff-audit.ZbLufUTk/incremental-live-a`，binary `/tmp/touko-incremental`。此服务在 BuildURL 修复后启动；之后 CLI reset 的改动不影响该 HTTP 页面。已设置 persistent 保留供用户操作，恢复会话时仍先确认进程是否在。
- 用户可能正在该可见窗口操作，已出现用户自行选择的待上传文件；主代理仅撤回自己添加的 blue 样例，没有替用户发布。**不得强制刷新、关闭窗口、清空表单或把整个 /tmp 根目录当成无人使用的脚手架清理。** 后续自动化另开隔离 state/端口/隐藏窗口；新版 UI 另开端口供用户切换。
- 用户原有 7799 服务仍属用户，**不要停止或替换**；用户 `logo/` 未加入实现提交。
- 保留的测试根目录 `/tmp/touko-handoff-audit.ZbLufUTk`：`incremental-local-a/b` 与 `incremental-picgo.git` 为本地恢复/原路径实测；`incremental-myimgs-copy-state` 与 `incremental-myimgs-copy.git` 为 204 图只读副本；`incremental-auto-a` 为已完成 reset 恢复现场。两个 GitHub 测试分支尚未删除，不自动清理用户正在查看的内容。
- 剪贴板安全事件：复制失败排查时，系统剪贴板原有访问令牌意外进入工具记录；已清空并告知用户建议撤销/轮换，**未宣称已撤销**。不在任何 md 重录令牌，后续只输出匹配布尔值，不输出剪贴板原文。

### 最新用户决定与立即下一步

1. ~~优先完成连接窗口的两级联动下拉~~ **已完成并实测（2026-10-17）**：打开连接窗口即自动加载当前登录账号可访问的全部仓库（分页），选仓库后加载全部分支并预选默认分支，重开已连接工作区保留当前分支；GitHub 已改为下拉，本地仓库路径/分支输入保留。
2. ~~建议接口 `GET /api/branches` + `ListBranches`~~ **已实现**：`internal/github/branches.go` 分页、`internal/webview/auth.go` 的 `branches()`、`api.go` 路由与前端 `loadGitHubRepositories`/`loadRepositoryBranches`；加载中禁用无效连接，未登录/空列表/失败均有显式文案，快速切仓库不会串入旧请求分支。
3. ~~补齐隔离浏览器验收~~ **已完成注册项**（见开头实测列表），并按阶段提交；**每个实现 commit 后同步更新本记忆**。仍未完成的组合验收见上一条“本轮实际验证”末句，不要写成全部通过。
4. **用户 Webview 确认后再优化 MyGo**：目标像 PicGo 一样直接出现独立窗口，无浏览器地址栏，不启动完整 Chrome/Firefox，不用 Electron；复用现有 Go HTTP + embedded Web UI，不另写业务状态。
5. MyGo 不是“完全不需要浏览器内核”：复用系统 WebView，Linux GTK3/WebKitGTK 4.1、Windows WebView2、macOS WKWebView。现有 `internal/desktop/app.go` / `cmd/touko-desktop` 已存在，先复用而非重写。内存占用需要量测**完整进程树**，区分空闲/图库加载/上传与 renderer，不宣称零内核或必定比 PicGo 省内存；同时检查剪贴板、文件选择、关闭后 HTTP/SQLite 释放。**本轮尚未做新的 MyGo 实机/内存测量。**
6. MyGo 通过后再考虑 AUR 打包与发布；不提前实现。私有仓库、分支保护拒绝写入、组织 token 审批仍未实测。clone 复用当前只检查可打开，没有核对已有 origin/branch；不能在文档中夸大保证。

此前已提交的视觉重设计与 13 项 GitHub live 脚本验收为历史成果，详情保留在下文；不代表本轮新增交互已经验完。早期内部迭代记录 `local://touko-iteration-protocol.md`、`local://touko-iteration-log.md`、`local://touko-session-summary.md` 只作历史参考，当前以本文件、源码和实际输出为准。

## 当前 Webview 要求与确定的设计

- 用户要求简洁、美观、设计感强、字体好看，参考 PicList 的图库布局；可在界面内整理 tags 和分类。分类复用平面 `Collections`，不引入层级或独立相册数据库；format v2 仅承载旧图/目标目录的显式路径。
- 最新视觉参考：《终将成为妳》的校服与两位主人公配色。使用奶油米色、深藏蓝作底，绿色主色，侑的珊瑚橙与灯子的暗樱红少量点缀。
- 用户点名要求：**所有**英文部分与中文部分都要换字体——英文用 Monaspace Radon，中文用 LXGW 文楷（霞鹜文楷）。当前字体：Monaspace Radon（拉丁/数字/UI，变量轴 wght 200-800、wdth 100-125、slnt 0…-11，无 CJK）、LXGW WenKai（中文，Regular 400 + Medium 500，SIL OFL 且其附加许可明确允许为 web 交付子集化为 WOFF2）、`TOUKO Wordmark`（Quicksand 圆几何字体，子集出 T/O/U/K 四字形，仅用于字标，1.3KB）与 `Playfair Display` 斜体（hero 标题专用；刻意不用带 opsz 光学尺寸轴的字面，Bodoni 因 opsz 在大字号变细被用户否决）。五者均 SIL OFL，许可证内嵌。此前临时使用的 Manrope / Source Sans 3 / Italianno / Inter / Newsreader / IBM Plex Sans SC 与 `static/brand/` 目录均已删除。
- 用户要求左上角 TOUKO 字标要更「规整圆润、有设计感」，按 logotype 处理而不是默认排字；用户已确认字标**保持 700** 字重。
- 用户选定：favicon 改为**方案感知 SVG**（深色标签栏用纸底版，浅色用原稿）；静态资源改为 **ETag + no-cache** 重新校验（内嵌文件无 mtime，max-age 会导致陈旧预览，故不采用）。
- 用户要求参照设计规范而不是即兴发挥：已取用 Anthropic 公开的 `frontend-design` skill 作为审查标准（含其「AI 生成页面」常见特征清单），用于修正流于模板的配色/排版特征。
- 用户已明确采用 **MyGo 独立窗口**，体验类似 PicGo；不是可有可无的建议。Webview 先验收，之后优化既有桌面壳；Go `net/http` + `embed` + 原生 HTML/CSS/JS 保留，无 Node 构建依赖、无运行时字体/脚本 CDN，也不使用 Electron。
- `touko serve --port 7788` 仅绑定127.0.0.1；GitHub 凭据由服务端环境或 gh 登录解析，不进入浏览器、localStorage、config或SQLite。API校验Host/Origin/CSRF，图片仅按Asset ID读取。
- Webview复用 app 的 staging、commit/sync/recovery，不另写存储/发布逻辑；已在真实浏览器完成下面记录的验收。今后每次变更仍需实际运行相应路径，再小步提交、更新本记忆及教程。

## 当前架构与 clone 读取路径（本轮新增，下一步接着做）

**目标**：从「GitHub API 驱动」改为 **local-first + Git-native + API-assisted**——Git 仓库是数据源、TOUKO 管本地 clone、GitHub API 退居辅助。

**用户已拍板的三个决定（不要重开）**：
1. 读路径 = **本地 clone**（已实现）。
2. 写路径 = **保持 GitHub API**（明确不改成 `git add/commit/push`）。
3. clone = **浅克隆 `--depth 1`**（已实现），不是全历史、也不是 partial clone。

**当前读取路径（含本轮尚未提交实现）**：
- `internal/localgit/clone.go` 的 `Clone`/`Update`/`WorkingCopy` 保留浅克隆 `--depth 1 --no-tags --branch B --single-branch`；Update 通过 fetch/reset/clean 刷新 TOUKO 自管副本。复用仅证明目录可打开，当前未校验 origin URL/branch，不得声称识别所有误用目录。
- clone 位于 **`<state-dir>/clone/`**（`repository.CloneDir`）。canonical 图片走 `objects/<ns>/sha256/...`；format v2 的旧图/目标目录图片走 `Asset.Path`。统一用 `repository.ObjectPath`。
- 预览绝不 fetch；canonical 文件可直接读，显式原路径使用固定 Git blob，避免可变文件名被远端更新后串到旧索引视图。缺副本时首次 clone 仍需联网，不能承诺首次使用离线成功。
- CLI/Web 普通 sync 先更新 clone，再索引固定 HEAD；verify 仅验证当前本地快照。commit/init 与 publication recovery 仍用 API；CLI reset 已改用 backend，Web reset 仅有 journal 才惰性访问后端。
- 早期空网络命名空间曾证明已存在本地图片可离线预览；该历史证据不意味着需要 fetch 的 sync 可离线，也不意味着浅克隆可以判断远端发布结果。

**凭据安全**：token 经 **Git credential protocol + 继承的 fd3 管道**传入，绝不进 URL / argv / 环境变量 / Git 写出的文件；`validateRemote` 拒绝已内嵌凭据的 URL；错误输出里 token 被 `[redact]`。`gitEnvironment()` 本就剥离所有 `GIT_*`、`GIT_TERMINAL_PROMPT=0`、全局配置指向 `/dev/null`。

**本轮已接通的 Web 入口与保留边界**：
- `Server.updateClone` 已接入 `SyncWith` 普通同步与恢复后刷新；与本地 stage mutation 共用工作区锁，不能交错破坏 snapshot。
- `Server.CloneStatus` 已通过 `/api/state` 的 `local_read` 提供状态，界面显示本地读图副本是否可用；已在实际页面观察。
- **历史膨胀已量化**：真实验收仓库里被删的 `red.png` 仍在历史；HEAD 只需 2 个对象而历史可达 13 个。浅克隆解决"下载量"，但**远端仓库本身的历史仍会增长**——若要真正清除需 `git filter-repo` + force push，属高危、TOUKO 未提供（`docs` 里已写明普通 rm 不 purge 历史）。
- 提案里提到的 `--filter=blob:none` 部分克隆**不适用于 TOUKO**：图片全在 HEAD，partial clone 只跳过"历史里才有"的 blob，当前图片仍会下载（已实测确认本地 clone 会硬链接、且浅克隆在 `file://` 下才生效）。
- 提案建议的 `git pull --rebase` **不要采纳**：TOUKO 远端写入靠唯一 parent + expected-old CAS，rebase 会重写本地未发布 commit，破坏"发布前远端不可见"不变量。

## Webview 本轮实测与正在展示的预览

- 启动入口：`touko --state-dir PATH serve --port 7788`，0为动态端口。源码入口是 `internal/cli/serve.go`、`internal/webview/{server,api,preview,static}.go` 与 `static/{index.html,style.css,app.js}`；`app/view.go` 的 ReadView 从同一 SQLite snapshot 生成页面视图。
- 用户中途要求打开成果窗口：曾用 `xdg-open http://127.0.0.1:7799` 打开。**7799 现在是用户自己启动的 `touko-live`（pid 240832），state `/tmp/touko-livegallery-state`，指向真实仓库；不要杀掉它。** 教训：7799 曾长期挂在旧构建上向用户展示过期页面——每次前端改动后必须重建 binary**并重启正在服务的进程**，不能只重建。
- 另有若干可用于验证的本地状态目录（都不含用户数据）：`/tmp/touko-webview-acceptance/state-a`（本地 bare gallery，4 assets）、`/tmp/touko-livegallery-state`（真实仓库副本）、`/tmp/touko-clonestate`（已含 clone）、`/tmp/touko-github.bVoTx3f6/device-{a,b,c}`（真实验收运行现场，含 token 无关的状态）。
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
| 20 | `fa54d43` | `docs: record the Playfair hero, ETag caching and favicon decisions` |
| 21 | `9923edd` | `docs(github): give paste-ready acceptance steps` |
| 22 | `e5884d9` | `test(github): stop publishing the user's artwork by default` |
| 23 | `aa6aff4` | `fix(github): parse the repeated repo use as JSON` |
| 24 | `132e196` | `docs: record the passing live GitHub acceptance` |
| 25 | `83c0469` | `feat(localgit): clone and update a working copy of a remote branch` |
| 26 | `fdbc330` | `feat(webview): preview committed pictures from a local clone` |
| 27 | `10d7744` | `perf(localgit): clone shallow so history cannot inflate the checkout` |
| 28 | `b89d1d1` | `feat(cli): read metadata and bytes from the local checkout` |

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

- Module `github.com/Eurekaimer/TOUKO`，Go 1.27.1（以 go.mod 为准）；uuid1.6.0、flock0.13.1、cobra1.10.2、modernc/sqlite1.60.1，既有 MyGo0.5.5。CLI 无 CGO/ORM/网络框架/插件框架；桌面壳另需系统 WebView。
- `cmd → cli → app → domain/repository/index/staging`；CLI装配localgit/github端口，app不导入前端。image/staging负责流式payload，delivery只派生URL。
- AssetID/RepositoryID独立UUID；ObjectHash严格小写SHA256；Git BlobID/TreeID/Head不能混用。Asset UUID与文件名、hash独立。
- Format v1/v2：`.touko/repository.json`、`.touko/assets/<id>.json`；无显式 path 时默认 canonical `objects/<namespace>/sha256/...`。首次发布显式 path 在同一 commit 升 v2；旧客户端 fail-closed，普通 v1 上传不强制升级，未知版本仍拒绝。
- namespace是物理路由：同namespace同bytes共用placement；跨namespace同hash但不同placement。删除按最终完整path引用，delete+samebytesadd保留对象，不清未知orphan/非TOUKO文件。
- PNG/JPEG/GIF，max100,000,000 bytes。source一次stream SHA256+temp，header从temp读，ext不信文件名。URL不入metadata。
- state优先级--state-dir→TOUKO_HOME→XDG_DATA_HOME/touko→~/.local/share/touko；0700目录/0600文件，稳定.lock非阻塞flock。不删stale锁。
- SQLite schema2/WAL/单连接仅保存 repository_state/assets committed cache；独立 `workspace.json` 耐久保存 base/changes/publication，staged bytes 在 stage/。健康 schema1 先自动迁移；迁移前 DB 是未发布状态唯一副本，不能先删。schema2 reindex 可恢复丢失/损坏 cache，保留 workspace/payload；config 不含 token。
- 多次stage归一化，first change绑定baseHEAD；overlay隐藏delete；edit/rm在base+stagedadds并集解UUID；无文件名猜测。tags/collections trim/去空/去重/排序，保持大小写。
- PlanCommit无IO；coordinator流式校验、固定parent/base tree、journal-before-ref、candidate/descendant recovery、transactional finalize、最后cleanup。CLI不能直接更新ref。
- localgit严格expected-old update-ref CAS，显式TOUKO identity，SHA1 repository only；tree保留mode/symlink/gitlink/非TOUKO文件。candidate比较leaf entries，不比较派生directory SHA。
- GitHub生产endpoint固定api.github.com，API header2026-03-10；rawblob读取，pipe+base64流式上传，tree删除显式sha:null，base_tree保留文件，truncated tree拒绝部分cache，无隐藏重试。阻塞型自定义Reader须可取消或io.Closer；CLI文件符合。

## 已知限制与未执行项

- **真实GitHub live 验收已通过**（2026-10-16，公开仓库 `Eurekaimer/touko-gallery-test`）：`examples/github-acceptance.sh` 13 项断言全绿——auth 只显示登录名、repo use 选中 main、init 幂等、批量 add 不动远端 HEAD、commit 发布 3 项、github/jsdelivr URL 与 canonical path 一致、verify assets=3 objects=3、raw 字节 SHA-256 等于记录 hash、第二设备逐字段恢复、末引用删除在第三设备不可见。独立复核：raw 直取 `.touko/repository.json`（format_version 1、repository_id dc117605-…），README.md 仍 200（init 只增不重写），伪造路径 404。
- **脚本凭据约束**：只从 `GITHUB_TOKEN` 读、拒绝 `eurekaimer/touko` 源码仓库作 gallery、失败非零退出保留现场、**默认不上传用户 `logo/TOUKO.png`**（需 `TOUKO_ACCEPT_INCLUDE_LOGO=1` 显式开启）。已用假 token 实测：失败报 GitHub 真实 401 且不回显 token。
- **仍未实测的分支**：私有仓库路径（走 `delivery_unavailable` 断言）、分支保护拒绝写入、组织仓库 token 审批。
- 教训：验收脚本曾被自己的离线 harness 用正则删掉 auth/`repo use` 段，导致“重复 repo use 缺 `--json`”这类解析 bug 漏到用户真实运行；现 harness 保留原样只换传输层，此类 bug 会被本地复现。
- 原生桌面壳已存在并在本机实测：`cmd/touko-desktop` 用 MyGo 打开同一 Webview（`internal/desktop/app.go`）。真实启动 `/tmp/touko-desktop --state-dir … --port 7807` 打印 `TOUKO MyGo: http://127.0.0.1:7807`、窗口进程存活、页面 HTTP 200（标题「图片库 · TOUKO」），终止后进程退出且端口释放。缺系统 webview 时明确报错而非静默回退。
- GitHub force:false仅fast-forward，不是任意外部force/reset下的严格CAS。分支应禁止force-push；TOUKO固定parent/HEADchecks保护正常non-force并发。不能宣称解决外部管理员rewrite race。
- localgit/private GitHub无公网URL/Markdown；public推导URL不等于commit完成或CDN传播。jsDelivr延迟不是远端commit失败。
- 普通rm只保证latest HEAD，不purge history/CDN（已在真实仓库量化）。没有 thumbnail、R2/S3 backend、自建 OAuth、自动 merge、namespace move/object replace；gh 登录委托已实现，MyGo 壳已有。
- 读路径本地化不覆盖恢复证明：普通 sync 更新 clone；verify/已有图预览只读本地；publication recovery 与 CLI reset 的 journal 判定仍用 GitHub API。私有 clone 需要服务端凭据，走 credential 管道。
- clone 是浅克隆，不提供完整历史，不依赖其做候选发布祖先证明、历史浏览或回滚。不要把可能暂留的旧 Git objects 当作完整历史契约。
- 用户logo仍是用户未提交文件；不得为“清理工作区”将它删除或擅自stage。其他后续用户变更也不加入实现提交。

## 后续开发方式

先读操作教程和架构教程，再按函数导航定位，不要猜API。每次先明确用户可观察行为与修改层，沿已有模式做行为测试和真实CLI/浏览器smoke、更新docs与本记忆，然后小步commit。避免实现断言/源文本测试、mock echo、假Store、占位命令和掩盖失败的fallback。Webview视觉方向已获用户明确要求，不重复索取确认；始终复用现有核心。

**方法学教训（本轮踩过，务必遵守）**：
1. **改完必须重测**，不能沿用改前的结论。本轮多次出现"改了一行 → 旧结论失效"。
2. **测试不能删掉被测逻辑**：本轮的离线 harness 曾用正则删掉 auth/`repo use` 段，导致缺 `--json` 的解析 bug 漏到用户真实运行。harness 只应替换传输层，保留被测代码原样。
3. **判定"字体/资源真的生效"要用权威手段**：CDP `CSS.getPlatformFontsForNode`（给出实际实例化字体名）。canvas `measureText` 只给度量、区分不了等宽字形，且内联样式刚改就测会拿到过期上下文——本轮因此误判过一次"变量轴失效"。
4. **`dispatchEvent(new KeyboardEvent(...))` 测不出原生 `<dialog>` 的 Esc 行为**，必须用真实按键（`tab.press`）。本轮因此两次误报"Esc 不关闭"。
5. **离线验证用 `unshare -rn`**（无外网命名空间）才能证明"不依赖网络"；`--proxy` 只影响 curl 与本地的连接，证明不了服务端。

## 下一步建议（按优先级）

1. 先做用户新要求的 GitHub 仓库/分支自动联动下拉；当前分支仍须手填，**未完成**。接口、状态与验收计划见本文件开头和 [Webview 文档](docs/webview.md)。
2. 补齐隔离浏览器端到端与 coherent 提交，保留用户正在操作的 17884 页面、7799 服务及上传表单。
3. 用户确认 Webview 后优化既有 MyGo 独立窗口、测完整进程树内存；再确认后做 AUR。不要把“复用系统 WebView”说成“不需要渲染内核”。
4. 写路径继续保持 GitHub API；不重开 git push 迁移，不实现自动 rebase、历史 purge 或未要求的云存储。

## CLI 阶段的教程检查记录

CLI教程阶段执行过一次性文档smoke：70个本地Markdown链接全部可达；Go AST收集到的186个生产函数/方法名称均出现在函数参考中。它只证明当时的链接/名称覆盖，不是本轮Webview新增代码的验证，也不证明逐条语义；本轮的实际检查结果另行记录。
