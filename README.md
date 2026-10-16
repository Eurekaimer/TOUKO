# TOUKO
TOUKO treats image management as tree-oriented repository operations instead of individual file uploads.

## CLI

Requires Go 1.26+ and Git supporting SHA-1 repositories and `show-ref --exists`. No CGO required.

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

## Webview 图片手帖

```sh
go build -o /tmp/touko ./cmd/touko
/tmp/touko --state-dir "$HOME/.local/share/touko-gallery" serve --port 7788
```

浏览器打开终端打印的 `http://127.0.0.1:7788`。空工作区可在界面创建本地 Git 图库或连接专用 GitHub 仓库。支持拖入多图、原图预览、标题/标签/分类编辑、筛选搜索、多选移除、待发布批次、提交、同步及完整校验。上传只暂存到本机，**提交后才发布**。

奶油米色与深藏蓝配色，七海灯子的 favicon，内嵌 Monaspace Radon 与 IBM Plex Sans SC 字体（均 SIL OFL，字体名保留）；无需 Node 构建或运行时 CDN。只监听本机，GitHub token 仅从服务进程的 `GITHUB_TOKEN` 读取，不进入网页。

完整启动、安全边界和真实 GitHub 验收步骤见 [Webview 教程](docs/webview.md)。原图预览不是缩略图服务；本地和 private GitHub 不生成公网分发链接。

若想用原生窗口而不是浏览器打开同一界面（MyGo 桌面壳，需要系统 WebView）：

```sh
go build -o /tmp/touko-desktop ./cmd/touko-desktop
/tmp/touko-desktop --state-dir "$HOME/.local/share/touko-gallery"
```

## 可操作 demo

```sh
# 首次需要先下载依赖；demo 本身禁用网络构建下载。
go mod download all
bash examples/demo.sh
```

需要 Bash 和 Python 3.8+ 标准库。脚本实际运行四图 batch、三设备恢复、过期编辑冲突、共享/末引用删除、SQLite 重建和 stdin 验收；保留 demo 目录并输出继续操作命令。仓库内 `examples/images/` 有三张自生成样例。你提供的图片可用 `add logo/TOUKO.png` 导入，不会修改原图。

需要针对**真实 GitHub 仓库**做验收时，用专用 gallery（必须先有 README）和终端里的 `GITHUB_TOKEN`：

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
5. [GitHub 接入](docs/github.md):专用 gallery、token、URL/Markdown、API 原子性限制，以及 `examples/github-acceptance.sh` 一键真实验收。
6. [发布与恢复协议](docs/publication.md)；[开发进度记忆](MEMORY.md)。

CLI 与 Webview 共用同一 staging/publication 核心。未提供专用 GitHub repo/token，因此没有宣称用户线上仓库的 live smoke 成功；本地真实 Git、HTTP 安全边界与浏览器验收的记录见 MEMORY.md。
