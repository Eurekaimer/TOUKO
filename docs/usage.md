# 从运行 demo 到管理自己的图片

本文讲实际操作；想理解代码，接着读 [架构学习路线](code-guide.md) 和 [逐函数导航](function-reference.md)。协议细节分别在 [format](repository-format.md)、[local state](local-state.md)、[publication/recovery](publication.md)、[GitHub](github.md)。

需要图形界面时，直接读 [Webview 启动与真实仓库验收](webview.md)。`touko serve` 和本文的 CLI 共用工作区格式；不需要导出/导入第二套图库，同一工作区的并发写入由锁保护。Web 在同一服务中为不同仓库/分支保存独立 URL 工作区；CLI 操作其中一个时，用该工作区实际的 state 路径，而非一律使用 Web 的根 state-dir。

## 1. 环境与构建

CLI 构建需要 Go 1.27.1 或更高版本（以 `go.mod` 为准）、Git（须支持 SHA-1 repository 和 `show-ref --exists`，建议 2.46+）、可写的本地 state 目录。SQLite 是纯 Go driver，不需要 CGO 或另外启动数据库服务。GitHub 浏览器登录需要安装 GitHub CLI（`gh`）；已有 `gh` 登录可直接复用，也可改用 `GITHUB_TOKEN` / `GH_TOKEN`。离线 demo 另外需要 Bash、Python 3（仅标准库断言，不是 CLI 的运行依赖）。

在项目根目录（Linux/macOS）：

```bash
go test ./...
go vet ./...
CGO_ENABLED=0 go build -trimpath -o /tmp/touko ./cmd/touko
/tmp/touko --help
```

Windows PowerShell：

```powershell
go test ./...
go vet ./...
$env:CGO_ENABLED = "0"
go build -trimpath -o touko.exe .\cmd\touko
.\touko.exe --help
go build -trimpath -ldflags "-H=windowsgui" -o touko-desktop.exe .\cmd\touko-desktop
```

发布构建固定使用 `CGO_ENABLED=0`、`-trimpath -s -w`，并注入 `buildinfo`；Windows 桌面加 `-H=windowsgui` 只是不弹控制台，不改变 WebView2 依赖。Windows 的 SQLite 路径编码、目录同步与原子替换已按平台实现，并由 release matrix 的原生回归覆盖。不要直接修改 `.touko` metadata 或 SQLite 表来操作图片；使用 CLI 保持内容描述、staging base 和 publication journal 一致。

### 桌面窗口与本地包（可选）

`touko-desktop` 用系统 WebView（Linux 需要 GTK3 + WebKitGTK 4.1，Windows WebView2，macOS WKWebView）显示与 `touko serve` 完全相同的界面，不启动完整浏览器、不使用 Electron，但仍有渲染内核，内存要按完整进程树量。MyGo 不打包 Chromium 或 Node：界面 HTML/CSS/JS/字体以 `go:embed` 编入二进制，Go 后端用本机回环 HTTP 提供页面并桥接剪贴板。二进制仍内嵌约 12 MiB 字体、SQLite 与图像处理代码，所以包小不等于运行内存小。窗口关闭默认隐藏到系统托盘；托盘菜单提供显示主窗口、在浏览器中打开、打开工作区目录和退出，只有“退出”才会释放 HTTP 端口与 SQLite，SIGINT/SIGTERM 也一样。没有托盘支持时它会明确提示“关闭窗口即停止 TOUKO”，不会留下无法找回的隐藏窗口。Linux 桌面已在 Wayland 上实机验收；Windows 与 macOS 桌面由发布流程原生构建并通过 CLI smoke，但窗口、托盘和剪贴板尚未 GUI 验收。

```bash
go build -trimpath -o /tmp/touko-desktop ./cmd/touko-desktop
/tmp/touko-desktop --state-dir "$HOME/.local/share/touko-gallery"   # 省略则用默认 state 目录
```

Windows 本地构建使用 `go build -trimpath -ldflags "-H=windowsgui" -o touko-desktop.exe .\cmd\touko-desktop`，避免启动时附带控制台窗口。

Arch 系可以直接装成本地包：`packaging/PKGBUILD` 从当前 checkout 构建（含未提交改动），安装 `touko`、`touko-desktop`、桌面条目和 16–256px 图标：

```bash
cd packaging && makepkg -si     # 需要 fakeroot；会要求 sudo 安装
touko-desktop
```

任务栏/应用菜单的图标靠 freedesktop 规则：窗口的 app_id 是**可执行文件基名**，所以包里装的桌面条目叫 `touko-desktop.desktop`，图标放在 `hicolor/<尺寸>/apps/touko.png`。把二进制改名会让图标查不到。图标由 `packaging/make-icons.py`（需要 Pillow）从本机 `logo/touko-logo.png` 裁出方形后生成；原 logo 不进入仓库与包，重新生成只影响 `internal/desktop/assets/` 下已提交的派生 PNG。

## 2. 先运行自动验收 demo

```bash
bash examples/demo.sh
# 或明确指定一个不存在/已有但完全为空的目录：
bash examples/demo.sh /tmp/my-touko-demo
```

脚本不是只打印命令：它实际构建 CLI、创建 bare Git、通过独立进程执行 add/commit/sync/edit/rm/verify，检查四个 Asset 共用三个对象、过期编辑冲突、共享/末引用删除、第三设备恢复、SQLite 重建和 Unicode stdin。使用脚本输出的 ROOT 路径；目录保留，不自动删除。相同非空目标再次运行会拒绝，避免覆盖你的数据。

```bash
# 把下面路径换成脚本实际输出的 ROOT，不要原样使用占位路径。
ROOT=/tmp/脚本实际输出的目录
"$ROOT/touko" --state-dir "$ROOT/device-a" ls
"$ROOT/touko" --state-dir "$ROOT/device-a" status --json
"$ROOT/touko" --state-dir "$ROOT/device-c" verify
```

脚本结束后最新 gallery 只包含 blue/green 两个 Asset；red 历史仍在 Git 中，不等于删除失败。

## 3. 亲自导入你的 logo

用户提供的 `logo/touko-logo.png` 保持原文件不动。可继续使用 demo 的 binary/state，也可按下一节另建 gallery。

```bash
ID=$("$ROOT/touko" --state-dir "$ROOT/device-a" add logo/touko-logo.png \
  --namespace blog --title '我的 TOUKO logo' --tag demo --tag logo \
  --collection '项目素材' --output id)
printf 'Asset ID: %s\n' "$ID"
"$ROOT/touko" --state-dir "$ROOT/device-a" status --json
"$ROOT/touko" --state-dir "$ROOT/device-a" show "$ID"
"$ROOT/touko" --state-dir "$ROOT/device-a" commit -m 'images: add my logo'
"$ROOT/touko" --state-dir "$ROOT/device-a" verify
```

`add` 不发布远端。UUID 是图片记录身份，不是文件路径或 hash。再次上传相同 bytes 得到新 UUID；默认路径下同 namespace 共用一份 binary。`--output id` 只输出 UUID，适合 shell 捕获，不能和 `--json` 合用；在旧 Git 图库原地纳管已有图片用后文的 `import`。

## 4. 从零建一个自己的离线 gallery

```bash
# 以下目录应由你选择；gallery 目标不能是已有非空目录。
export TOUKO_HOME="$HOME/.local/share/touko-my-gallery"
/tmp/touko repo init --local "$HOME/Pictures/my-gallery.git"
ID=$(/tmp/touko add logo/touko-logo.png --namespace blog --title '我的 logo' --tag demo --output id)
/tmp/touko status
/tmp/touko commit -m 'images: initial import'
/tmp/touko ls --json
/tmp/touko verify
```

Gallery 是 **bare Git repository**，没有普通 checkout 工作区。可以用 `git --git-dir "$HOME/Pictures/my-gallery.git" ls-tree -r --name-only main` 查看当前 committed paths；不要用文件浏览器找工作区图片。

State 目录优先级为 `--state-dir` → `TOUKO_HOME` → `$XDG_DATA_HOME/touko` → `$HOME/.local/share/touko`。CLI 每台设备/每个 gallery 使用独立 state-dir；`repo use` 不会把另一仓库的 cache/stage 混进来。Web 切换仓库则自动使用独立工作区，旧根 state 作为默认工作区保留，新工作区在 `<state-root>/workspaces/<key>/`。从网页显示的本地副本路径去掉末尾 `/clone` 可得到该 GitHub 工作区的 CLI state 路径；不要凭空猜 key。

### GitHub 登录与选择仓库

```bash
# 安装 gh 后在终端运行；无需把 token 粘贴到网页。
/tmp/touko auth login
/tmp/touko auth status
/tmp/touko repo list
/tmp/touko --state-dir "$HOME/.local/share/touko-github-gallery" repo use OWNER/gallery
/tmp/touko --state-dir "$HOME/.local/share/touko-github-gallery" sync
```

`auth login` 调用真实 `gh auth login --hostname github.com --web --git-protocol https`；`auth status` 仅输出登录名和凭据来源，不输出 token。凭据优先级为 `GITHUB_TOKEN` → `GH_TOKEN` → `gh` 登录；没有凭据可匿名读取公开仓库，但列出当前账户仓库和写入需要认证。显式 env 凭据无效不会偷偷回退到 gh。`repo list --json` 可查看 `full_name/default_branch/private/can_push`，只读仓库无法发布。

`repo use` 只选择仓库，不初始化远端；已有 TOUKO 仓库接着 `sync`，尚无 TOUKO manifest 的旧图库则按下节显式 `repo init`。普通 `sync` 先联网更新 GitHub clone，再索引其固定 HEAD，**一次调用即更新图库**；`verify` 和已有 clone 的预览不 fetch，只校验/读取当前本地副本，不能用来确认远端是否有新提交。首次读取需联网建立浅克隆；发布仍用 GitHub API，不执行 `git push`。

## 5. 查找与修改

```bash
/tmp/touko ls --namespace blog
/tmp/touko find demo
/tmp/touko show "$ID" --json
/tmp/touko edit "$ID" --title '新的标题' --tag logo --tag public
/tmp/touko edit "$ID" --collection '首页素材'
/tmp/touko edit "$ID" --clear-tags
/tmp/touko status --json
/tmp/touko commit -m 'metadata: organize logo'
```

- 完整 UUID 或至少八字符且唯一的 UUID 前缀可作 selector；不要依赖文件名。多匹配报 ambiguous，零匹配报 not-found。
- `find` 对 title、original filename、tags、collections 作不区分大小写的子串搜索，query 不能为空。
- `edit` 只改 title/tags/collections；未提供的字段不变。重复 `--tag/--collection` 表示完整新列表，不是追加；trim/去空/去重/排序但保持大小写。显式清空用 clear flags，不能和同字段设置同时使用。
- ls/show/find 是 committed snapshot 上叠加 staging，状态为 committed/added/modified，staged-deleted 记录隐藏。
- 未实现 namespace move、换图、thumbnail 或自动合并；图形界面已有，见 [Webview 教程](webview.md)。Web 还可搜索真实仓库路径，并按目录递归筛选；CLI `find` 不包含路径搜索。

## 6. stdin 与批量导入

```bash
cat logo/touko-logo.png | /tmp/touko add - --name '剪贴板.png' --namespace notes --output id
/tmp/touko add examples/images/red.png examples/images/green.png examples/images/blue.png --tag demo
/tmp/touko status --json
/tmp/touko commit -m 'images: batch import'
```

一次 batch 的任一输入失败，该批不会留下部分 stage rows。stdin `-` 只能单独使用，不能混其他文件；`--name` 仅对 stdin 有效。未指定 stdin name 则按实际格式命名为 stdin.png/jpg/gif/webp。图片格式由 header 决定，不信文件扩展名。当前支持 PNG/JPEG/GIF/WebP，单个最大 100,000,000 bytes，不整张图片加载到内存。

### WebP 上传与可选转换

```bash
# 原样上传 WebP
/tmp/touko add photo.webp --namespace blog

# 允许把本批 PNG/JPEG 转成 WebP；默认质量 85
/tmp/touko add photo.png --webp --webp-quality 85
```

转换是逐文件的：只有编码后的 WebP 比原字节更小时才替换，否则保留原文件。GIF 与已有 WebP 始终原样保存（不丢动画、不做二次有损压缩）。转换在进入 staging 前完成，因此对象 hash、大小、扩展名和 manifest 都描述实际保存的 WebP 字节；`original_name` 仍记录用户传入的文件名。转换需要解码完整图片，超过 50,000,000 像素的图片会明确拒绝转换，可去掉 `--webp` 原样上传。`--webp-quality` 必须配合 `--webp`，范围 1–100。

### 指定仓库上传目录

```bash
/tmp/touko add examples/images/red.png --namespace blog --directory images/blog --tag demo
/tmp/touko status
/tmp/touko commit -m 'images: upload into images/blog'
```

`--directory` 是**仓库内目标目录**，不是本地输入目录；文件使用确定性内容 hash 命名，metadata 记录实际路径。留空保持默认 `objects/<namespace>/sha256/...`。目录须是规范相对 POSIX 路径（`/` 分隔），不能是绝对路径、含 `..`、反斜杠、控制字符或 `.git/.touko` 路径段。相同 bytes 不同最终路径是独立 placement；删除按实际完整路径计数。

### 原地导入旧图库 / PicGo

```bash
# 独立 state 与自己的专用仓库；分支可按需追加 --branch。
LEGACY_STATE="$HOME/.local/share/touko-legacy"
/tmp/touko --state-dir "$LEGACY_STATE" repo use OWNER/old-gallery
# 仅在还没有 TOUKO manifest 时执行：这一步会写入 manifest。
/tmp/touko --state-dir "$LEGACY_STATE" repo init
/tmp/touko --state-dir "$LEGACY_STATE" sync
/tmp/touko --state-dir "$LEGACY_STATE" import --directory img --namespace blog --tag legacy --collection '旧图'
/tmp/touko --state-dir "$LEGACY_STATE" status --json
/tmp/touko --state-dir "$LEGACY_STATE" commit -m 'images: manage legacy paths'
/tmp/touko --state-dir "$LEGACY_STATE" verify
```

`import` 不接受本地文件参数：扫描选定仓库固定 Git HEAD 中已提交的图片，`--directory img` 包括该目录下子目录；省略时扫描全仓库。已有 clone 想读取最新远端时先 `sync`。未初始化时明确拒绝导入，必须先由用户显式 `repo init`，不会自动接管原仓库。

支持按真实 header 识别 PNG/JPEG/GIF，包括 Unicode 名和**无后缀**图片。导入只新增待发布 metadata，保留原路径与原 Git blob，不移动、不复制到 canonical objects、不重新上传；原路径旧链接不因导入而改变。已有 metadata 管理的路径、非普通文件/非图片会计入 skipped，`.touko` 不扫描；重复导入不创建重复记录。无法读取、损坏或不安全路径等失败不会留下部分导入。结果给出 imported/skipped 数量，检查后仍需显式 `commit`。

**版本警告：导入和非空 `add --directory` 会产生显式 `path`；首次发布这样的 metadata 将 manifest 从 v1 升级到 v2，升级与 metadata 在同一个 commit 中完成。旧客户端会 fail-closed 拒绝 v2，发布前先升级其他设备。** 现有 v1 仍可读取，默认 canonical 上传不强制升级。导入后执行 `rm` 并提交会删除其原路径（仅当最后一个路径引用消失）；不是“只取消纳管”，且不会清理未知文件。

## 7. 删除、撤销与多设备

```bash
/tmp/touko rm "$ID"
/tmp/touko status --json
/tmp/touko commit -m 'images: remove logo'
/tmp/touko verify
# 尚未发布的修改可全部丢弃；不是逐条撤销：
/tmp/touko reset --staged
```

最后一个**实际完整路径**引用删除才删当前 tree 的 binary；默认 canonical 路径包含 namespace，自定义路径和旧图路径则按各自完整路径计数。`rm` 不擦 Git 历史，不承诺 CDN 全球缓存立刻失效。对 staged add 执行 rm 只是取消新增；已 staged-delete 再 rm 幂等，但 delete→edit 是冲突，不隐式恢复。

另一个设备或 state-dir：

```bash
/tmp/touko --state-dir /tmp/device-b repo use --local "$HOME/Pictures/my-gallery.git"
/tmp/touko --state-dir /tmp/device-b sync
/tmp/touko --state-dir /tmp/device-b ls --json
```

如果 B 已 stage edit，而 A 删除并发布该 Asset，B commit 返回 conflict（退出码3，delete_modify），不会让图片复活。决定丢弃 B 全部未发布工作后，执行 B 的 `reset --staged; sync`。没有自动 merge/rebase。

## 8. 恢复、cache 与故障排查

| 现象 | 处理方式 | 不要做什么 |
| --- | --- | --- |
| busy | 等待同一 state-dir 的写命令退出 | 不删除 `.lock`，不自造 stale-lock 清理 |
| head_changed/delete_modify | 查看 status，决定保留还是 reset 全部 stage，再 sync | 不 force publish，不隐式恢复远端已删除记录 |
| publication outcome unknown | 保留完整工作区与 stage，运行 sync；索引先损坏时先 reindex 再 sync | 不盲目重新上传，不删除 journal |
| published head … local index failed | 远端可能已提交；修复本地存储后 sync | 不把它解释成远端没提交 |
| repository_corrupt/hash_mismatch | 保留旧 cache，检查远端固定 HEAD 与 payload | 不跳过坏 metadata 生成部分 gallery |
| unauthorized/api_error | 运行 auth status，检查 gh 登录或 env 凭据权限/branch protection，或等待限流再重试 | 不把 token 写进命令参数或聊天，不绕过保护 |

当前 SQLite schema2 的 `index.db` 只存 committed cache；未发布 changes、base HEAD 和 publication journal 保存在独立耐久 `workspace.json`，staging payload 也需保留。推荐恢复命令，不手动删表：

```bash
# 若 Web/桌面仍使用这个工作区，先正常退出，再从 CLI 修复。
/tmp/touko --state-dir "$HOME/.local/share/touko-my-gallery" reindex
/tmp/touko --state-dir "$HOME/.local/share/touko-my-gallery" status --json
# 有未决 publication 时由 sync 经 API 判定发布结果，reindex 不会清 journal。
/tmp/touko --state-dir "$HOME/.local/share/touko-my-gallery" sync
```

`reindex` 完整验证固定源快照及图片 hash 后重建 committed cache，可恢复缺失、物理损坏或 metadata 逻辑损坏的 index，并保留耐久暂存。物理损坏旧文件保留在工作区的 `index-corrupt-*` 目录，不静默丢弃。CLI 的 GitHub `reindex` 会先更新 clone；若有未发布 stage 而 HEAD 已变，返回冲突，不覆盖用户修改。pending journal 保留，之后必须 `sync` 恢复，不能把重建成功当作发布成功。

**旧 schema1 例外：** 旧版本把 stage/journal 存在 `index.db` 中，必须先用当前版本打开健康旧索引完成自动迁移，才有独立耐久工作区。迁移失败保留旧源；若迁移前旧 DB 已删除/损坏且无备份，远端不能恢复唯一未发布数据，`reindex` 会拒绝猜空。即使迁移后，也不要删除 `workspace.json`、暂存文件或整个 state-dir，不要在服务运行中手动删除 DB/WAL/SHM；备份应保留整个 state。

GitHub 接入与历史公开仓库验收记录见 [专用指南](github.md)。local/private repo 的 URL/Markdown 不可用是正确行为，不输出假公网链接；公开仓库验收不代表私有仓库等尚未实测分支也通过。

## 9. JSON 与退出码

stdout 只放命令结果；错误在 stderr。`--json` 错误形状为 `{"error":{"code":"conflict","message":"..."}}`。成功0、usage2、conflict3、其他错误1。

| 命令 | JSON 外层 |
| --- | --- |
| add/ls/find | `{"assets":[AssetView...]}` |
| show | `{"asset":AssetView}` |
| status | `{"head":"...","base_head":"...","changes":[...]}` |
| sync/reindex | `{"head":"...","asset_count":N}` |
| import | `{"imported":N,"skipped":N}` |
| repo list | `{"repositories":[{"full_name":"OWNER/REPO","default_branch":"main","private":false,"can_push":true}]}` |
| auth status | `{"login":"LOGIN","authenticated":true,"source":"gh"}`（source 也可能为 `github_token/gh_token`） |
| commit | `{"head":"...","change_count":N,"asset_count":N}` |
| verify | `{"head":"...","assets":N,"objects":N}` |
| url | `{"url":"...","committed":true/false}` |

AssetView 为 `{"asset":Asset,"state":"committed|added|modified"}`。空数组是 `[]`，不是 null。flags 可放在参数后。

## 10. 开发时如何验证自己的修改

```bash
go test ./...
go vet ./...
go test -race ./...
go test ./internal/repository -run '^$' -fuzz '^FuzzDecodeAsset$' -fuzztime=10s
bash examples/demo.sh
```

先根据 [逐函数导航](function-reference.md) 找调用链，再阅读相关行为测试。优先修改已有模块，不另建通用 Storage/Plugin 层。新增用户可见行为要验证真实 CLI/Git/SQLite 状态，不写只验证调用转发或函数不 panic 的测试。稳定的设计决策与使用说明维护在 `docs/`；Agent 上下文、临时图库、性能输出与打包产物不进入源码提交。测试使用新建的独立图库和 state，不操作日常使用的图库。

`.github/workflows/ci.yml` 在 `main` push 与所有 pull request 上执行 `go test ./...`、`go test -race ./...`、`go vet ./...` 和隔离的 CLI demo；使用 `go.mod` 指定的工具链及 Go module cache。Linux runner 启用 CGO 供 race detector 使用，任何一步失败都会使 job 失败。该 workflow 也可由 release workflow 调用。

## 11. 版本与发布

`touko version`、`touko --version` 和 `touko-desktop --version` 显示版本与完整源码 commit；`touko version --json` 输出 `{"version":"…","commit":"…"}`。这些路径不打开工作区或数据库，桌面版本查询也不需要显示会话。普通 `go build` 默认为 `dev` / `unknown`，不从目录名猜版本。

两个程序共用 `internal/buildinfo`；可直接注入：

```bash
COMMIT=$(git rev-parse HEAD)
go build -trimpath \
  -ldflags "-X github.com/Eurekaimer/TOUKO/internal/buildinfo.Version=v0.1.0 -X github.com/Eurekaimer/TOUKO/internal/buildinfo.Commit=$COMMIT" \
  -o /tmp/touko ./cmd/touko
/tmp/touko version --json
```

### 本地构建 release bundle

脚本签名固定为：

```bash
bash scripts/release.sh vMAJOR.MINOR.PATCH FULL_COMMIT [OUTPUT_DIR]
```

第三个参数可指定输出目录，默认 `dist/`；同名归档或校验文件已存在时拒绝覆盖。脚本用 `GOOS` / `GOARCH` 选择目标：未设置时构建宿主平台，显式支持 linux/{amd64,arm64}、windows/amd64、darwin/{amd64,arm64}，其他组合直接失败。例如：

```bash
# 从已审核的 checkout 构建；脚本只打包，不创建 tag 或发布。
bash scripts/release.sh v0.1.0 "$(git rev-parse HEAD)"
GOOS=linux GOARCH=arm64 bash scripts/release.sh v0.1.0 "$(git rev-parse HEAD)"
GOOS=windows GOARCH=amd64 bash scripts/release.sh v0.1.0 "$(git rev-parse HEAD)"
GOOS=darwin GOARCH=arm64 bash scripts/release.sh v0.1.0 "$(git rev-parse HEAD)"
cd dist && sha256sum -c touko_0.1.0_linux_amd64.sha256
```

脚本要求 Bash、Go（以 `go.mod` 为准）、Git、Python 3.12+，以及 `cp`、`mktemp`、`chmod` 等基础工具；归档与 SHA-256 由脚本内置的 Python helper 生成，不依赖 GNU tar。Windows 原生 runner 通过 Git Bash 与 setup-python 执行，本地 Windows 重跑也需要同样的 Bash 与 Python 3.12+ 环境。构建统一使用 `CGO_ENABLED=0`、`-trimpath -s -w` 并注入 `buildinfo.Version` / `buildinfo.Commit`；Windows 桌面额外加 `-H=windowsgui`。所有目标使用归档根名 `touko_0.1.0_${GOOS}_${GOARCH}`：Windows 为 `.zip`，其余为 `.tar.gz`，各自附同名 `.sha256`。

布局固定：CLI 在 `bin/touko[.exe]`；Linux/Windows 桌面在 `bin/touko-desktop[.exe]`；macOS 桌面在 `TOUKO.app/Contents/MacOS/touko-desktop`，不再复制到 `bin/`。macOS `.app` 含 Info.plist 和 `.icns` 图标；Linux 包保留桌面条目和 16–256px 图标。每个归档都带 `share/doc/touko/` 下的 README 与 `docs/`，以及 `share/licenses/touko/LICENSE`。桌面仍依赖系统 WebView（Linux GTK3/WebKitGTK 4.1 与 Ayatana 托盘，Windows WebView2，macOS WKWebView），禁用 CGO 不等于不需要这些运行库。安装步骤见 [README](../README.md#安装)。

本地 Arch 包继续从当前 checkout 构建，注入 `dev-r…` 版本及源码 commit；`Git` 是运行依赖。它不是 AUR 正式发布包。

### GitHub Release

`.github/workflows/release.yml` 由 `v*` tag push 触发，也支持手动 `workflow_dispatch` 指定一个已存在的稳定 tag。发布顺序：

1. resolve job 用完整历史解析 tag 指向的 commit；tag 不存在或不是 `vMAJOR.MINOR.PATCH` 就失败。
2. 调用 `ci.yml` 对同一 commit 执行 test、race、vet 与隔离 CLI demo。
3. 在五个原生 runner（ubuntu-24.04、ubuntu-24.04-arm、windows-2025、macos-15-intel、macos-15）上按 `go.mod` 安装 Go、复用 module cache，以 `GOOS`/`GOARCH` 调用 `release.sh` 构建对应归档和 SHA-256。这里不是交叉编译，每个产物都由目标 OS/架构的 runner 生成。
4. 每个目标运行原生 smoke：检查归档布局和版本/commit 注入，并在新建的本地图库执行 `init`、`add`、`commit`、`verify`、`find`；Windows 同时覆盖 SQLite 路径编码、目录同步和原子替换回归。任一目标失败都不会发布。
5. 汇总全部归档与 `.sha256`，用最小 `contents: write` 权限创建 **draft** GitHub Release；`--verify-tag` 禁止隐式创建 tag。

tag push 默认只生成 draft，便于先在本机和 Windows 实机测试。draft 中的 Windows 与 macOS 包已经过 CLI smoke，但不代表 GUI、托盘或剪贴板已验收。维护者测试通过后，在 GitHub 上手动把 draft 发布为正式 Release。本轮会尝试创建 `v0.1.0` draft；是否已经存在、状态是 draft 还是正式发布，以 GitHub 的实际状态为准，本文不承诺发布成功。push `main` 或提交 PR 只触发 CI，不发布 Release。
