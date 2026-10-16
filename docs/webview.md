# TOUKO Webview：启动、整理图片与真实 GitHub 测试

Webview 是 CLI 同一个核心的本地浏览器前端，不是独立云服务。图片操作仍遵守 `暂存 → 提交 → 同步`；点击上传不会立即写入 GitHub。标签对应 Asset.Tags，分类对应平面的 Asset.Collections，不另建分类数据库。

## 1. 构建与启动

在项目根目录运行（构建需要 Go 1.27.1 或更高版本，以 `go.mod` 为准，以及支持 `show-ref --exists` 的 Git）：

```bash
go build -o /tmp/touko-web ./cmd/touko
/tmp/touko-web --state-dir "$HOME/.local/share/touko-web-demo" serve --port 7788
```

`serve` 默认自动打开浏览器；不想自动打开时使用 `/tmp/touko-web --state-dir "$HOME/.local/share/touko-web-demo" serve --port 7788 --no-open`。若自动打开失败，终端会提示手动访问，服务仍然可用。访问终端打印的 `http://127.0.0.1:7788`；端口占用时换 `--port 7789`，`--port 0` 自动选择可用端口，以打印结果为准。使用打印的127.0.0.1地址，不改成localhost/局域网IP。Ctrl+C 停止服务；state-dir 保留，下次相同命令继续。

不需要 npm、Node、外部字体服务或数据库进程。HTML/CSS/JS、字体和 favicon 都嵌入 Go binary。拉丁字母与数字使用自托管的 Monaspace Radon，中文使用 LXGW 文楷，左上角字标使用子集化的圆几何字体。

### 先用离线 demo 体验

```bash
bash examples/demo.sh
# 将 ROOT 换成脚本实际输出的目录。
ROOT=/tmp/脚本实际输出的目录
/tmp/touko-web --state-dir "$ROOT/device-a" serve --port 7788
```

该 state 已有可用 local Git gallery；可直接在网页查看图片、继续导入 `logo/TOUKO.png`。本地 backend 不生成公网 URL，这是正常限制。

也可使用空 state 在界面连接本地仓库：新建 bare gallery 必须指定不存在或完全为空的目录；选择已有仓库不会覆盖它。**同一服务可管理多个仓库/分支**：“连接 / 切换仓库”中可打开已连接工作区，也可连接新仓库。每个工作区拥有独立 URL（新工作区为 `/w/<key>/`）、clone、index、暂存和发布日志，切换只是导航，原工作区未发布修改保留，不必停止服务或重启新 state。旧根 state 仍作为默认工作区保留，新工作区在 `<state-root>/workspaces/<key>/`。不要手动修改 key 或把某个工作区的 index/stage 搬到另一个仓库。

## 2. 真实 GitHub 测试前的准备

1. 在 GitHub 自行新建**专用测试仓库**，例如 `你的用户名/touko-webview-test`。不要使用 TOUKO 源代码仓库，也不要选含重要资料的仓库。
2. **勾选 Add a README file**。Git Data API 无法在完全空仓库创建第一个 ref。默认分支通常为 main，界面会读取实际 default branch。
3. 需要测试公开 URL 时选 public；private 仍能存储、网页预览、编辑、同步和 verify，但不提供 raw/jsDelivr 公网链接。
4. 推荐安装 GitHub CLI（`gh`），通过下面的 `touko auth login` 登录；已有 gh 登录可直接复用。若自行使用 fine-grained token，应仅选择目标仓库并授予 **Contents: Read and write**，设置短有效期；组织仓库可能还需组织批准。
5. 分支禁止 force push。若分支保护要求 Pull Request 或签名而拒绝直接发布，使用你为测试允许直接提交的专用分支；TOUKO 不绕过保护。

不要把 token 贴到聊天、网页、URL、命令参数或 Git 仓库。网页没有手动 token 输入框，也不接收凭据；`auth login` 在终端委托 gh 打开 GitHub 登录流程，不是网页自建 OAuth 服务。运行时按 `GITHUB_TOKEN` → `GH_TOKEN` → gh 登录读取凭据；显式环境凭据无效不会静默回退。

## 3. 推荐的真实仓库启动方式

在 Bash 中执行；把示例仓库名换成你的真实专用 repo。已有 gh 登录时可跳过 login：

```bash
go build -o /tmp/touko-web ./cmd/touko
/tmp/touko-web auth login
export TOUKO_GALLERY_STATE="$(mktemp -d -t touko-github.XXXXXXXX)"
printf '请保存这个 state 路径：%s\n' "$TOUKO_GALLERY_STATE"

/tmp/touko-web auth status
/tmp/touko-web repo list
/tmp/touko-web --state-dir "$TOUKO_GALLERY_STATE" repo use YOUR_USER/touko-webview-test
/tmp/touko-web --state-dir "$TOUKO_GALLERY_STATE" repo init
/tmp/touko-web --state-dir "$TOUKO_GALLERY_STATE" serve --port 7788
```

`auth status` 应显示 GitHub 登录名和凭据来源，不显示 token。`repo list` 列出当前账户可访问仓库（含 private/can_push）；能读取不等于有发布权限。`repo init` 在现有 README commit 上增加 TOUKO manifest，不重写 README；重复执行合法 init 不创建额外提交。

当前实现中，可直接启动空 state 的 `serve`，在“连接 / 切换仓库”里选择 GitHub，点“读取 GitHub 登录 / 加载仓库”，再从可访问仓库列表选择；目前也保留仓库手填和分支文本框。**这不是最终交互：用户已要求仓库与分支均改为自动加载的联动下拉，尚未实现，具体计划见第 8 节。** 连接后导航到独立工作区，已有 TOUKO 仓库先同步，旧图库按提示显式初始化再导入；不会自动纳管全部图片。

没有安装 gh 或未登录时，先在终端运行 `touko auth login`，再回网页重新读取，**gh 登录不需要重启 serve**。若依靠环境变量，运行中进程不会获得另一个终端后来 export 的新值，修改环境凭据需重启服务。凭据只在 Go 服务内部，既不返回网页，也不写入 config/SQLite/metadata。

## 4. 按顺序验证每一个界面能力

### A. 上传与预览

1. 使用上传按钮或拖入 `logo/TOUKO.png`、`examples/images/red.png` 和 `blue.png`。
2. Namespace 设为 `blog`，标签填 `demo, 灯子`，分类填 `测试图库`；可设置标题。目标目录可留空使用 canonical `objects/...`，或填 `images/blog`，文件以内容 hash 命名，不是照搬本机文件名。
3. 确认上传。应看到真实图片预览、尺寸/体积及“待发布”的新增状态。
4. 打开详情，检查原文件名、实际仓库路径、UUID、hash、尺寸与 MIME。
5. 此时去 GitHub 看分支 HEAD，**不应因为上传而产生 commit**。

每张最高100,000,000 bytes，单个网页请求最多500,000,000 bytes、100个文件；不支持的图片或失败输入不应留下半个批次。网页预览读取原图，不是假缩略图服务。

### A2. 旧图库 / PicGo 原地纳管

1. 先连接旧仓库正确分支。无 TOUKO manifest 时，必须由你点击“初始化”明确写入 manifest；导入不会自动初始化。
2. 点击“导入旧图库 / PicGo”，填仓库目录（例如 `img`；留空扫描全仓库）、namespace、标签和分类。
3. 确认导入，检查 imported/skipped 统计与待发布记录。按真实图片 header 识别 PNG/JPEG/GIF，支持 Unicode 和无后缀图片；已有管理路径、非图片/非普通文件跳过，`.touko` 不扫描，重复导入幂等。
4. 此时只暂存 metadata，原路径与原 Git blob 不变，不移动、不复制到 `objects/`，也不重新上传。检查后仍需打开待发布列表，显式 `commit` 才发布 metadata；原有图片链接保持原路径。

**先升级所有设备：** PicGo 导入或指定非空上传目录产生显式 `path`。第一次提交此类 metadata 会在同一原子 commit 中升级 manifest 到 format v2；旧客户端会 fail-closed 拒绝读取 v2。已有 v1 与默认 canonical 上传不强制升级。已纳管旧图的删除也是暂存后提交，并按实际路径移除最后引用的原文件，不是单纯“取消管理”。

导入扫描的是已提交 Git tree，而非你本机未提交文件；远端有新图时先同步。路径必须为安全相对 POSIX 路径，不能穿越、使用反斜杠/控制字符或 `.git/.touko` 路径段。本机新图上传使用上传按钮，不用旧图库导入。

### B. 标签、分类和搜索

1. 在图片详情编辑标题、标签和分类并保存。
2. 标签/分类是完整列表；删除一个条目后保存表示从该图片移除，而非删除其他图片的标签。
3. 用左侧分类、标签、搜索和“全部路径目录”缩小范围；搜索也匹配真实仓库路径，目录筛选包含子目录，详情显示实际路径。清除筛选后完整图库应恢复。标签/分类可设置多个，分类为平级关系，不创建独立空相册实体。
4. 刷新浏览器，staging 修改应仍在，不能只保存在浏览器内存。
5. 多次编辑同一个新增 Asset 仍是一条 add，不应变成多个重复记录。

### C. 发布与真实远端检查

1. 打开待发布列表，检查新增/修改/删除内容。
2. 输入有意义的 commit message，例如 `webview: add Touko sample images`，确认提交。
3. 成功后待发布列表清空，卡片变成已提交，HEAD 更新。
4. 在 GitHub 文件页确认 `.touko/repository.json`、`.touko/assets/<uuid>.json` 及实际图片路径；默认上传位于 `objects/blog/sha256/...`，指定目录和旧图则使用详情所示路径。README 仍在。
5. 执行界面的完整校验；其 asset/object 数量应与当前本地引用一致。**校验不 fetch**，不是读取远端最新 HEAD；想更新远端状态用“同步”，它先更新 clone 再在同一次操作中刷新图库。普通“刷新”也不替代同步。
6. public repo 可选择 GitHub 或 jsDelivr provider，并复制链接或 Markdown。Markdown 对 alt 中的特殊字符和换行做转义处理；浏览器剪贴板权限被拒时，生成结果仍显示在可选中文本框中，按提示手动复制，不会谎报复制成功。先打开 raw GitHub 链接检查图像；jsDelivr 可能尚未传播，不能因此重复提交。未发布新图的链接带有明确提示，不能当作已发布 URL。

GitHub 写入失败必须显示实际错误并保留 staging，不应该显示“成功”。发生发布结果未知时先同步恢复，不反复点提交制造第二个候选。

### D. 同图去重与删除

1. 同一 namespace 再次上传 logo，提交。两个 UUID 不同，但 canonical object path 相同。
2. 删除第一条并提交：只少一个 metadata 文件，另一张图片仍可预览，binary 应保留。
3. 删除最后引用并提交：最新 GitHub tree 中对应 metadata 与 binary 都消失。
4. Git history 中仍能看到旧图片是正常的；TOUKO 不做历史 purge 或 CDN 缓存清除。
5. 删除只暂存时可在待发布列表检查；“丢弃全部暂存”需要明确确认，不能当作已经发布删除的撤销操作。

## 5. 用第二台设备证明不是本机假成功

在第二台设备安装 gh 并登录同一有权访问的账户（或显式配置环境凭据），另建全新 state-dir：

```bash
FRESH_STATE="$(mktemp -d -t touko-fresh.XXXXXXXX)"
/tmp/touko-web --state-dir "$FRESH_STATE" repo use YOUR_USER/touko-webview-test
/tmp/touko-web --state-dir "$FRESH_STATE" sync
/tmp/touko-web --state-dir "$FRESH_STATE" ls --json
/tmp/touko-web --state-dir "$FRESH_STATE" verify
/tmp/touko-web --state-dir "$FRESH_STATE" serve --port 7789
```

可在另一终端执行并复用本机 gh 登录；第二台设备需自行登录。若使用环境 token，仅在相应终端安全读取，不写入 shell history。比较完整 metadata：ID、namespace、实际 path（显式路径图片）、title、original_name、tags、collections、hash、size、MIME、尺寸、时间都应一致。第三个全新 state 在删除后同步，不应看到被删除 UUID。

并发冲突练习：A/B 同一 HEAD 时，B 编辑某图但不提交；A 删除并提交；B 提交必须 conflict/delete_modify，不能复活该图。B 决定放弃所有未发布修改后再显式 reset/sync。不要为了让测试通过关闭冲突检查。

测试结束时 Ctrl+C 停止 serve。若临时设置过环境凭据，清理相应变量（`unset GITHUB_TOKEN GH_TOKEN`）；仓库和 state 保留供检查，确认无用后再自行决定删除。既有示例 `examples/github-acceptance.sh` 仍要求 `GITHUB_TOKEN`，不会自动按应用的 gh 认证逻辑运行，详见 [GitHub 指南](github.md)。

### 索引恢复与未发布状态

界面“重建索引”在运行中的当前工作区内重建 committed cache，保留 `workspace.json` 的 changes/base HEAD/publication。它不是丢弃暂存，也不是远端发布恢复；有 journal 时之后必须“同步”，由 GitHub API 确认结果，不以浅克隆推测发布是否成功。若有 stage 且源 HEAD 已变化，显示冲突，不隐式覆盖。

物理损坏/删除 index 导致服务无法启动时，先正常退出占用该工作区的进程，用其**实际 state 目录**运行 CLI（不是所有工作区都使用根 state）：

```bash
WORKSPACE_STATE="$HOME/.local/share/touko-my-gallery" # 换成当前工作区实际路径
/tmp/touko-web --state-dir "$WORKSPACE_STATE" reindex
/tmp/touko-web --state-dir "$WORKSPACE_STATE" status --json
/tmp/touko-web --state-dir "$WORKSPACE_STATE" sync
```

CLI `reindex` 对 GitHub 先更新 clone，完整验证源快照与 hash 后恢复 committed cache，物理损坏旧文件保留在 `index-corrupt-*`。网页重建读取现有 clone，不自动 fetch；需要远端新状态先同步。schema2 已把未发布状态迁至耐久 `workspace.json`，但 staging payload 仍需保留，备份整个工作区。

**尚未迁移的 schema1 是例外**：stage/journal 唯一副本仍在旧 `index.db`，先用当前版本打开健康旧索引完成自动迁移，不能先删 DB；迁移前源已坏且无备份时无法从远端重建未发布数据，程序明确拒绝猜空。不要删除 `workspace.json` 或暂存目录，也不要在服务仍连接数据库时手动删除 DB/WAL/SHM。

## 6. 安全与已知边界

- 只监听127.0.0.1，校验精确Host/Origin和会话CSRF；不提供局域网共享或公网部署开关。不要用反向代理绕过边界后当作多人云服务。
- API/图片不允许跨域，页面有 CSP、nosniff、no-referrer、frame保护；详情只按 UUID 查图，没有任意文件路径下载接口。
- 凭据只在 Go server 内部；已有 GitHub clone 时从本地读图（包括 private），不会把 token/授权链接发给浏览器。界面会显示本地副本是否可用，不应把网络 fallback 当作本地读图。首次 clone、同步和认证需要网络，已有副本的预览/校验不自动 fetch。
- GitHub `force:false` 是 fast-forward保护，不是针对任意外部管理员force/reset的严格expected-old-SHA CAS。禁止force-push是测试前提之一。
- GitHub header版本固定2026-03-10；错误、限流、branch protection和truncated tree不会被伪装成部分成功。
- 专用公开仓库 `Eurekaimer/touko-gallery-test` 的真实 live 验收已有 [MEMORY 记录](../MEMORY.md#已知限制与未执行项)。本轮另在独立分支验证旧路径导入并发布，未改 main；`Eurekaimer/MyIMGs` 仅只读克隆到本地副本验证，未写原仓库。私有仓库、分支保护拒绝和组织审批不因此算已实测；浏览器剪贴板权限受限，目前仅确认 Markdown 生成与手动复制框，不声称自动复制成功。

## 7. 视觉资源与设计

色板来自用户指定方向：奶油米色（纸 #f7f3ea）与深藏蓝（墨 #1b2a3d）呼应校服，珊瑚橙作「已暂存/当前」强调色，暗樱红只用于破坏性操作与「已修改」，卡纸色（#e5dcc9）作侧栏与照片衬底。整套设计由 Anthropic 公开的 `frontend-design` skill 逐条审查后重建：页面只有一种材质（纸），状态靠**材质**而非阴影区分——已提交的照片坐在卡纸衬底上，暂存中的坐在牛皮纸色衬底上并把压边线变成珊瑚色；圆角分四级承担层级；全页 `text-transform` 为 0、无渐变、无 `rgba(0,0,0,…)` 页面阴影。中文用 LXGW 文楷（楷体笔画更细，故正文 16px/行高 1.85，对比度 5.64:1，相对旧设定实测墨量 +7.6%），拉丁与数字用 Monaspace Radon，hero 标题用 Playfair Display 斜体（`--font-hero`，仅此一处使用）；刻意避开带光学尺寸轴的字面，否则字号越大笔画越细、观感发虚。

英数与拉丁文本使用自托管的 Monaspace Radon（oblique 走 slnt 轴），中文使用 LXGW 文楷（霞鹜文楷，楷体正文面），左上角 TOUKO 字标另有子集化的圆几何字体（Quicksand，仅 T/O/U/K 四字形，1.3KB，并按字对做光学字距）。五者均为 SIL OFL，许可证随字体内嵌；LXGW 的附加许可明确允许为 web 交付子集化为 WOFF2。原图 `logo/TOUKO.png` 保持不变，页面内只使用从它派生的 favicon（`favicon.png`），不再放角色图条带；不主张角色或原图版权，分发衍生图片前应自行确认授权。界面无需外部 CDN 即可显示字体、图标与 favicon。

## 8. 下一步界面与桌面计划（尚未实现）

本节是用户最新确认的要求与实施顺序，不是现有功能已通过验收的声明。实际运行证据、未提交状态与测试现场见 [MEMORY](../MEMORY.md)。

### A. 仓库与分支两级联动下拉

目标：像常见的仓库选择器一样，选择仓库，再选择分支；正常连接 GitHub 不要求用户手填 `OWNER/REPO` 或分支名。

1. 打开 GitHub 连接窗口时自动读取当前账号，加载其凭据可访问的**全部仓库**，包括授权可见的私有仓库、协作及组织仓库；不得只显示第一页。公开/私有、可写/只读状态保留，能浏览不等于能发布。
2. 选择仓库后完整加载该仓库分支；新连接默认选仓库默认分支，重开当前连接时保留仍存在的当前分支。分支不是自由输入框；本地 Git 仓库的路径与分支输入不受此项改动影响。
3. 候选接口为 `GET /api/branches?repository=OWNER/REPO`，返回分支 `name` 与 `protected`；GitHub adapter 增加完整分页的 `ListBranches`。默认分支可复用已有仓库列表字段。**这些分支接口尚未实现。**
4. 读取期间禁用尚无有效仓库/分支的连接操作；未登录、无权限、无仓库、空仓库/无分支、网络错误均显示明确状态，不回退成让用户猜名字。
5. 切换仓库必须立即清除旧分支候选；忽略晚到的旧请求，避免给仓库 B 提交仓库 A 的分支。连接后的 URL/Service 仍按仓库与分支隔离，切换不丢暂存。
6. 浏览器验收须真实选择默认与非默认分支，比较界面列表和 GitHub 返回的完整列表；覆盖分页、快速切换、空/失败状态与工作区保留暂存。不要把“请求返回非空”当成完整验收。

目前可复用 `internal/github/repositories.go`、`internal/webview/auth.go`、`api.go`、`static/{app.js,index.html,style.css}`，优先沿已有模式修改，不增加 Provider/Manager 或新前端框架。

### B. MyGo 独立窗口与内存验证

用户目标是像 PicGo 一样启动后出现独立应用窗口：没有浏览器地址栏，不额外打开完整 Chrome/Firefox，不采用 Electron。已有 `cmd/touko-desktop` 与 `internal/desktop/app.go`，应复用同一个 HTTP 后端和 embedded Web UI，而不是重写图片管理逻辑。

**系统 WebView 仍有渲染内核。** MyGo 使用操作系统 WebView；当前代码要求 Linux GTK3 + WebKitGTK 4.1、Windows WebView2 或 macOS WKWebView。没有完整浏览器窗口不等于没有 renderer，也不能据此承诺内存必定低于 PicGo。

实施顺序：

1. 先完成仓库/分支下拉及当前 Webview 的上传、编辑、链接、删除、同步、工作区隔离和恢复验收，供用户确认。
2. 用户确认 Webview 后再优化既有 MyGo 入口与窗口体验，检查本地图片、字体、文件选择、拖放、剪贴板和关闭窗口后的 HTTP/SQLite 释放。现有桌面权限处理一律拒绝，剪贴板能力需要实际检查，不直接放开所有权限。
3. 实测完整进程树的内存（包含 renderer 等子进程），分别记录空闲、固定数量图库加载、上传时的状态；明确 RSS/PSS 口径和环境，不只报 Go 主进程，也不先承诺“无内核/零开销”。
4. MyGo 也经用户确认后，再做 AUR 打包与发布准备；本轮尚未开始新的桌面优化、内存测量或 AUR 实现。

### C. 当前交接边界

本轮增量代码和对应文档仍未提交，不能把计划或旧验收记录当作新功能全部完成。用户正在操作的页面、待上传表单和原 7799 服务应保留；自动化使用隔离 state、端口和窗口，更新 UI 时不要强制刷新用户的未提交表单。提交按 coherent 阶段拆分，每个实现 commit 后同步 MEMORY。
