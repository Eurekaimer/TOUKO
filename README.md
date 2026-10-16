# TOUKO
TOUKO treats image management as tree-oriented repository operations instead of individual file uploads.

## CLI

Requires Go 1.27.1 or later (see `go.mod`) and Git supporting SHA-1 repositories and `show-ref --exists`. No CGO required.

```sh
go build -o /tmp/touko ./cmd/touko
/tmp/touko --state-dir /tmp/my-gallery-state repo init --local /tmp/my-gallery.git
/tmp/touko --state-dir /tmp/my-gallery-state add picture.png --namespace blog --tag demo
/tmp/touko --state-dir /tmp/my-gallery-state status --json
/tmp/touko --state-dir /tmp/my-gallery-state commit -m "images: import"
/tmp/touko --state-dir /tmp/my-gallery-state verify
/tmp/touko --state-dir /tmp/my-gallery-state ls
```

State flags may follow command arguments. `add - --name 剪贴板.png` reads one image from stdin. `edit ID --title TITLE --tag TAG` replaces supplied fields only; `--clear-tags` and `--clear-collections` explicitly clear lists. `rm ID...` stages atomic removal; `reset --staged` discards unpublished changes.

Another device selects the existing repository with `repo use --local /absolute/gallery.git`, then `sync`. Each device needs its own state directory. Sync validates a fixed Git tree before atomically replacing the SQLite cache; corrupt metadata never produces a partial gallery.

See [repository format](docs/repository-format.md) and [local state](docs/local-state.md).

TOUKO 使用本地工作副本读取图片：GitHub 仓库首次读操作会浅克隆（`--depth 1`）到对应工作区的 `<state-dir>/clone`。已有 clone 时预览、`verify` 只读当前本地快照，不自动 fetch；离线可读已同步图片，但不能据此判断远端最新状态。**`sync` 会先联网更新 clone，再在同一次调用中索引更新后的固定 HEAD**。发布结果未知时通过 GitHub API 恢复 journal，不用浅克隆猜测结果；`commit`、`repo init` 仍通过 GitHub API 写入，不改为 `git push`。认证、仓库发现和列表也需要 GitHub API。

## Webview 图片手帖

```sh
go build -o /tmp/touko ./cmd/touko
/tmp/touko --state-dir "$HOME/.local/share/touko-gallery" serve --port 7788
```

`serve` 默认打开浏览器；若打开失败，服务仍运行，可手动访问终端打印的 `http://127.0.0.1:7788`。不自动打开用 `serve --port 7788 --no-open`。空工作区可在界面创建本地 Git 图库或连接专用 GitHub 仓库。支持拖入多图、原图预览、标题/标签/分类编辑、路径筛选搜索、多选移除、旧图库导入、待发布批次、提交、同步、重建索引及完整校验。上传和旧图库导入都只暂存到本机，**显式提交后才发布**。

推荐先安装 GitHub CLI，再执行：

```sh
/tmp/touko auth login
/tmp/touko auth status
/tmp/touko repo list
```

`auth login` 委托 `gh` 浏览器登录；已有 `gh` 登录无需重新输入 token。认证优先级为 `GITHUB_TOKEN` → `GH_TOKEN` → `gh` 登录，无效的显式环境凭据不会静默回退。网页“连接 / 切换仓库”自动列出全部可访问仓库（含 private/can_push），选中仓库后再列出它的全部分支并预选默认分支；没有手动 token 输入框，也不需要手填 `OWNER/REPO`（本地 Git 仓库仍填路径与分支）。每个仓库/分支拥有独立 URL 工作区、clone、索引和暂存，切换后原工作区的待发布修改保留，不必停止服务。

奶油米色与深藏蓝配色，七海灯子的 favicon，内嵌 Monaspace Radon（英数）与 LXGW 文楷（中文）字体（均 SIL OFL）；无需 Node 构建或运行时 CDN。只监听本机，凭据只由服务端读取，不进入网页。

完整启动、安全边界和真实 GitHub 验收步骤见 [Webview 教程](docs/webview.md)。原图预览不是缩略图服务；本地和 private GitHub 不生成公网分发链接。

### 旧图库 / PicGo 与指定上传目录

```sh
# 换成自己的独立 CLI state 与专用仓库；已有 TOUKO 仓库不必重复初始化。
/tmp/touko --state-dir /tmp/picgo-state repo use OWNER/gallery
/tmp/touko --state-dir /tmp/picgo-state repo init
/tmp/touko --state-dir /tmp/picgo-state sync
/tmp/touko --state-dir /tmp/picgo-state import --directory img --namespace blog --tag legacy
/tmp/touko --state-dir /tmp/picgo-state status
/tmp/touko --state-dir /tmp/picgo-state commit -m "images: manage existing PicGo paths"
/tmp/touko --state-dir /tmp/picgo-state add examples/images/red.png --directory images/blog
/tmp/touko --state-dir /tmp/picgo-state commit -m "images: upload into directory"
```

`import` 读取 Git 已提交图片，按内容识别 PNG/JPEG/GIF（包括无后缀文件），只暂存 metadata，保留原路径和原 Git blob，不复制到 `objects/`、不重新上传。省略 `--directory` 扫描全仓库，已管理路径与非图片会跳过；它不是从本机目录上传文件（本机文件用 `add`）。**首次提交含显式 `path` 的导入或目录上传会把仓库升级到 format v2；旧客户端将 fail-closed 拒绝读取，请先升级所有设备。** 默认 canonical 上传不会无故升级 v1。缓存损坏/丢失可用 `reindex` 保留耐久暂存重建；尚未迁移的 schema1 数据库是例外，必须先从健康旧库迁移，不能先删除唯一未发布状态。详见 [CLI 教程](docs/usage.md)。

若想用原生窗口而不是浏览器打开同一界面，可使用已有 MyGo 桌面壳。它不启动完整浏览器、不使用 Electron，但仍依赖系统 WebView 渲染内核；内存占用需计入 renderer 等子进程实测，不能承诺“没有浏览器内核”：

```sh
go build -o /tmp/touko-desktop ./cmd/touko-desktop
/tmp/touko-desktop --state-dir "$HOME/.local/share/touko-gallery"
```

用户已要求连接窗口改为**仓库 / 分支两级联动下拉**；当前分支仍为文本框，新交互尚未完成。后续顺序为：完成并确认 Webview → 优化既有 MyGo 独立窗口并实测内存 → 确认后再做 AUR。当前状态见 [MEMORY](MEMORY.md)，具体计划见 [Webview 文档](docs/webview.md)。

## 可操作 demo

```sh
# 首次需要先下载依赖；demo 本身禁用网络构建下载。
go mod download all
bash examples/demo.sh
```

需要 Bash 和 Python 3.8+ 标准库。脚本实际运行四图 batch、三设备恢复、过期编辑冲突、共享/末引用删除、SQLite 重建和 stdin 验收；保留 demo 目录并输出继续操作命令。仓库内 `examples/images/` 有三张自生成样例。你提供的图片可用 `add logo/TOUKO.png` 导入，不会修改原图。

需要用既有 `examples/github-acceptance.sh` 针对**真实 GitHub 仓库**做验收时，使用专用 gallery（必须先有 README）。**该示例脚本仍显式要求 `GITHUB_TOKEN`，不同于应用本身可复用 `gh` 登录**：

```sh
read -rsp 'GitHub token: ' GITHUB_TOKEN; printf '\n'; export GITHUB_TOKEN
bash examples/github-acceptance.sh OWNER/专用仓库
unset GITHUB_TOKEN
```

脚本不接受 token 参数、不打印 token，并拒绝把 TOUKO 源码仓库当 gallery。

## 阅读与学习

1. [Webview 与真实 GitHub 测试](docs/webview.md)：启动、图库整理、专用仓库与第二设备验收。
2. [CLI 操作教程](docs/usage.md)：从 demo 到自己的图片、标签/分类、多设备、恢复与 JSON。
3. [架构与设计教程](docs/code-guide.md)：阅读顺序、模块边界、完整调用链、状态转换、设计取舍与修改练习。
4. [逐函数导航](docs/function-reference.md)：每个生产函数/方法的职责、输入输出、副作用与错误边界。
5. [GitHub 接入](docs/github.md)：专用 gallery、`gh` 登录与凭据优先级、URL/Markdown、API 原子性限制，以及 `examples/github-acceptance.sh` 一键真实验收。
6. [发布与恢复协议](docs/publication.md)；[开发进度记忆](MEMORY.md)。

CLI 与 Webview 共用同一 staging/publication 核心。真实 GitHub 专用公开仓库 `Eurekaimer/touko-gallery-test` 的 live 验收已有实测记录（含 raw 字节 hash、第二设备 metadata 恢复和删除），见 [MEMORY.md](MEMORY.md#已知限制与未执行项)。本轮增量验收使用独立测试分支，未改该仓库 main；原 `Eurekaimer/MyIMGs` 仅只读克隆到本地副本用于导入验证，未写原仓库。私有仓库、分支保护拒绝写入及组织审批不可据此宣称已实测。浏览器已验证 Markdown 生成与手动复制框；当前剪贴板权限受限，不宣称自动复制成功。
