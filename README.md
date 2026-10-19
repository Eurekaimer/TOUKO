<div align="center">

<img src="https://socialify.dev/Eurekaimer/TOUKO/image?custom_language=Go&font=Raleway&forks=1&language=1&logo=https%3A%2F%2Fcdn.jsdelivr.net%2Fgh%2FEurekaimer%2FTOUKO-WEBP-01%40main%2Fobjects%2Fdefault%2Fsha256%2Ffa%2F93%2Ffa93a8ca741cfa57253de4eed814435452ee5fce2863932f0112f7e94455041a.webp&name=1&owner=1&pattern=Circuit+Board&pulls=1&stargazers=1&theme=Light" alt="TOUKO" width="640" />

# TOUKO

把图片存进自己的 Git 仓库。

[![Go](https://img.shields.io/badge/Go-1.27%2B-00ADD8?logo=go&logoColor=white)](go.mod)
[![License](https://img.shields.io/badge/license-MIT-9c5238)](LICENSE)
[![Platform](https://img.shields.io/badge/platform-Linux%20%7C%20Windows%20%7C%20macOS-1b2a3d)](#安装)
[![Docs](https://img.shields.io/badge/docs-8%20guides-7f3f59)](docs/webview.md)

[简体中文](README.md) · [English](README_en.md)

</div>

TOUKO 是一个以 Git 仓库为数据源的图片库。图片和元数据都存在仓库里，GitHub 是数据源，jsDelivr 只负责可选的公开分发。本地 SQLite 用来加速读取，删掉后可以从仓库重建。

## 名字

名字来自《终将成为你》（やがて君になる）的七海灯子（Nanami Touko）。

> 好きって、なんだろう。
> 好きって、なに？
>
> 七海灯子

一张图片也面对类似的问题：它属于谁，存在哪里，删除后还能不能找回。TOUKO 用 Git 记录这些变化。每次提交都有历史，每个文件名来自内容。

## 和 PicGo 的区别

PicGo 和 PicList 主要解决上传后拿到链接。TOUKO 还要回答一年后能不能找回原图，以及找回来的字节是否和当初一致。

为此，Asset 的标题、标签、分类、MIME、尺寸和 SHA-256 都保存在仓库里。相同字节共用一个对象，换机器时执行 `repo use` 和 `sync` 就能恢复完整图库。上传、编辑和删除先进入暂存区，`commit` 才写成一次 Git 提交。多个仓库可以放进同一个图库，每个仓库仍有独立的暂存、提交和恢复流程。

`rm` 只从最新树移除图片，Git 历史仍然保留。要彻底清除需要重写历史，TOUKO 不提供这个功能。

## 格式与空间

TOUKO 接受 PNG、JPEG、GIF 和 WebP。上传 PNG 或 JPEG 时可以选择转换为 WebP。只有编码后的字节更少时才替换原文件，因此不会为了统一格式反而把文件变大。GIF 和已有 WebP 保持原样，避免丢失动画或再次有损压缩。

转换在进入暂存区之前完成。对象哈希、大小、扩展名和仓库元数据描述的都是最终保存的字节。单张图片最大 100 MB，转换上限为 5000 万像素。

博客和笔记中实际引用的 194 张图片迁移到新图库时，质量为 85 的转换结果如下：

| 项目 | 数据 |
| --- | ---: |
| 原始文件 | 78.24 MiB |
| 新图库对象 | 16.87 MiB |
| 节省 | 61.37 MiB（78.4%） |
| 转为 WebP | 171 张 |
| 保持原格式 | 23 张 |

这是这批图片的实际结果，不代表所有图库都能达到同样比例。23 张保持原格式，是因为 WebP 编码后反而更大。转换只在更小时替换，更小的对象也会减少 CDN 传输和浏览器缓存占用。

## 界面

<div align="center">
  <img src="docs/images/webview-gallery.png" alt="TOUKO 图库界面" width="820" />
</div>

界面使用奶油米色和深藏蓝，中文为霞鹜文楷，拉丁文本为 Monaspace Radon。单击卡片会复制 Markdown，并在指针旁显示结果。可以拖放、粘贴剪贴板图片，也可以按分类、标签、目录或内容哈希筛选。

图库不会一次挂载所有卡片。首屏显示 60 张，滚动接近底部时继续加载，离屏卡片跳过布局和绘制。1000 张图库的筛选、滚动和勾选仍然可以正常操作，数据见[性能基线](#性能基线)。

## 安装

| 目标 | v0.1.0 状态 |
| --- | --- |
| Linux amd64 | 桌面在 Wayland + GTK3 + WebKitGTK 4.1 上验证；发布流程构建 CLI 与桌面 |
| Linux arm64 | 发布流程构建 CLI 与桌面，尚未做 GUI 实机验收 |
| Windows amd64 | 发布流程构建 CLI 与桌面，尚未做 GUI 实机验收 |
| macOS amd64 | 发布流程构建 CLI 与 `.app`，尚未做 GUI 实机验收 |
| macOS arm64 | 发布流程构建 CLI 与 `.app`，尚未做 GUI 实机验收 |

Linux 的验证只覆盖一套 Wayland 桌面，不代表所有发行版、桌面环境或架构。Windows 与 macOS 的产物由原生 runner 构建，并通过 CLI 冒烟测试，但本轮没有在真实图形会话里验收，界面、托盘和剪贴板行为仍需实机确认。

CLI 和桌面都需要 Git 支持 SHA-1 仓库与 `show-ref --exists`，建议使用 Git 2.46 或更高版本。桌面使用系统 WebView：Linux 需要 GTK3 与 WebKitGTK 4.1，托盘需要 Ayatana AppIndicator 和托盘宿主；Windows 需要 WebView2 Runtime；macOS 使用系统 WKWebView。GitHub 登录推荐安装 `gh`，也可改用 `GITHUB_TOKEN` 或 `GH_TOKEN`。

发布矩阵固定为 linux/{amd64,arm64}、windows/amd64、darwin/{amd64,arm64}。每个目标生成统一根名 `touko_0.1.0_${GOOS}_${GOARCH}` 的归档：Windows 为 `.zip`，其余为 `.tar.gz`，并各附同名 `.sha256`。归档都含 README、`docs/` 与 LICENSE；CLI 在 `bin/touko`（Windows 为 `bin\touko.exe`），Linux/Windows 桌面在 `bin/touko-desktop[.exe]`；macOS 桌面是带 Info.plist 与 `.icns` 图标的真实 `TOUKO.app`，入口为 `TOUKO.app/Contents/MacOS/touko-desktop`，不再另外复制一份桌面二进制。Linux 包还保留桌面条目和 hicolor 图标。

### 安装 release

发布由 GitHub Actions 在原生 runner 上按上述五个目标构建，CI 通过后附加全部归档与校验文件，并创建 **draft** Release。draft 用于本机检查和 Windows 用户自行实机测试，测试通过后才正式发布。本轮会尝试创建 `v0.1.0` draft，是否已公开以 [GitHub Releases](https://github.com/Eurekaimer/TOUKO/releases) 的实际状态为准。

下载归档和同名 `.sha256` 后先校验，再解压。Linux：

```sh
sha256sum -c touko_0.1.0_linux_amd64.sha256
tar -xzf touko_0.1.0_linux_amd64.tar.gz
sudo install -m755 touko_0.1.0_linux_amd64/bin/touko touko_0.1.0_linux_amd64/bin/touko-desktop /usr/local/bin/
sudo cp -R touko_0.1.0_linux_amd64/share/. /usr/local/share/
touko version
touko-desktop --version
```

Linux arm64 把文件名中的 `amd64` 换成 `arm64`。预编译包不需要 Go。桌面包直接运行解压后的 `bin/touko-desktop` 也可以，桌面条目和图标需要安装到 XDG 路径后才会出现在系统菜单。缺少系统 WebView 时桌面版会报错退出，CLI 仍可单独使用。

Windows（PowerShell）：

```powershell
Expand-Archive .\touko_0.1.0_windows_amd64.zip -DestinationPath .
cd .\touko_0.1.0_windows_amd64
Get-FileHash ..\touko_0.1.0_windows_amd64.zip -Algorithm SHA256
Get-Content ..\touko_0.1.0_windows_amd64.sha256
.\bin\touko.exe version --json
```

`Get-FileHash` 输出的十六进制应与 `.sha256` 中记录的值一致。要打开桌面版，双击包根下的 `bin\touko-desktop.exe`。关闭窗口只会隐藏到托盘，真正退出请使用托盘菜单里的“退出”。Windows 桌面需要 WebView2 Runtime；GitHub 登录推荐先执行 `gh auth login`。

首次测试请使用独立的 state 目录和新建的本地图仓，不要直接操作日常使用的仓库：

```powershell
$state = Join-Path $env:TEMP "touko-test-state"
$repo = Join-Path $env:TEMP "touko-test.git"
.\bin\touko.exe --state-dir $state repo init --local $repo
.\bin\touko.exe --state-dir $state add .\some-photo.png
.\bin\touko.exe --state-dir $state commit -m "test: add a photo"
.\bin\touko.exe --state-dir $state verify
```

macOS：

```sh
shasum -a 256 -c touko_0.1.0_darwin_arm64.sha256   # Intel 用 darwin_amd64
tar -xzf touko_0.1.0_darwin_arm64.tar.gz
cd touko_0.1.0_darwin_arm64
./bin/touko version --json
cp -R TOUKO.app /Applications/
open /Applications/TOUKO.app
```

首次打开未使用开发者 ID 签名、也未公证的 `.app` 可能被 Gatekeeper 拦截。请在 Finder 中右键选择“打开”，或到“系统设置 → 隐私与安全性”中确认放行；只在确认来源后操作，不推荐全局关闭 Gatekeeper 或其他安全机制。

### 包体积与运行内存

归档不包含 Chromium 或 Node：MyGo 复用系统 WebView，界面 HTML/CSS/JS/字体通过 `go:embed` 编进二进制，Go 后端通过本机回环 HTTP 提供页面并桥接剪贴板。因此包小不等于运行时内存小：二进制仍内嵌约 12 MiB 字体，以及 SQLite 与图像处理代码，桌面运行时还要加上系统 WebView 的进程树。最终体积以实际下载文件为准，可用 `ls -lh touko_0.1.0_*`（Windows：`Get-Item .\touko_0.1.0_windows_amd64.zip | Select-Object Length`）测量，本文不写死数字。

### 从源码构建

使用 `go.mod` 指定的 Go 版本（当前为 1.27.1）和上面的运行依赖：

```sh
git clone https://github.com/Eurekaimer/TOUKO.git
cd TOUKO
go build -trimpath -o touko ./cmd/touko
go build -trimpath -o touko-desktop ./cmd/touko-desktop
./touko version --json
```

发布构建统一使用 `CGO_ENABLED=0`、`-trimpath -s -w`，并注入 `buildinfo.Version` / `buildinfo.Commit`；Windows 桌面额外加 `-H=windowsgui`，避免附带控制台窗口。开发构建显示 `dev / unknown`，不会被误认为正式版本。版本注入和发布步骤见[版本与发布](docs/usage.md#11-版本与发布)，测量方法见 [benchmark](docs/benchmark.md)。

Arch 用户可以从当前 checkout 构建本地包。这不是已经发布的 AUR 包，会安装两个程序、桌面条目和图标：

```sh
cd packaging && makepkg -si
```

## 快速开始

```sh
./touko --state-dir ~/.local/share/touko repo use Eurekaimer/TOUKO-WEBP-01
./touko --state-dir ~/.local/share/touko sync

./touko --state-dir ~/.local/share/touko add photo.png --tag 旅行 --collection 示例
./touko --state-dir ~/.local/share/touko commit -m "images: add a photo"

./touko --state-dir ~/.local/share/touko find 旅行
./touko --state-dir ~/.local/share/touko find 4aff401b
```

常用命令有 `status`、`ls`、`find`、`show`、`edit`、`rm`、`verify`、`reindex` 和 `import`，全部支持 `--json`。

## 图形界面

```sh
./touko --state-dir ~/.local/share/touko serve --port 7788
```

默认会打开浏览器，加 `--no-open` 只启动服务。也可以运行 `touko-desktop` 打开原生窗口。窗口没有地址栏，但页面仍由系统 WebView 渲染。MyGo 不携带 Chromium 或 Node，界面资源通过 `go:embed` 编入 Go 二进制，服务端用本机回环 HTTP 提供页面并桥接剪贴板。

服务只监听本机回环地址，并校验 Host、Origin 和 CSRF。GitHub 凭据留在服务端进程内，不会进入网页。

## 图床与 CDN

GitHub 仓库保存图片，jsDelivr 提供可选分发：

```text
https://cdn.jsdelivr.net/gh/OWNER/REPO@BRANCH/objects/<ns>/sha256/<aa>/<bb>/<hash>.<ext>
https://raw.githubusercontent.com/OWNER/REPO/BRANCH/objects/<ns>/sha256/<aa>/<bb>/<hash>.<ext>
```

jsDelivr 对 GitHub 包有体积上限，超过后整个包返回 403。此时可以改用 raw 直链，内容相同。TOUKO 按容量把图片分配到多个仓库，并记录每个 Asset 实际所在的仓库。

预览会根据内容哈希返回 ETag 和 immutable 缓存。再次查看同一张图片时，浏览器可以复用缓存或收到 304，不必重新读取 Git 对象。

## 性能基线

以下数据来自本机一次可重复测量：Linux x86_64、20 逻辑 CPU、32 GB 内存、Go 1.27.1、本地 Git 后端，3 次重复取中位数。实际时间会随机器和缓存状态变化。

![CLI 与 HTTP 基线：0、100、1000 张图](docs/images/benchmark-cli.svg)

1000 张图时，读取路径仍在毫秒级。`reindex` 和 `commit` 需要遍历或重写仓库元数据，所以明显更慢。图表纵轴使用对数刻度，避免启动耗时被提交耗时压成一条平线。

![桌面 RSS：空图库与 1000 张图的四个阶段](docs/images/benchmark-memory.svg)

RSS 是 Linux 进程树的常驻集，这里取五次快照中的最高值。共享页会在多个进程中重复计算，整树数字不能当作新增的物理内存。空图库启动约 717 MiB，1000 张图浏览后约 918 MiB。主要占用来自 WebKit 渲染进程，属于系统 WebView 的成本。

复现命令：

```sh
ROOT=$(mktemp -d /tmp/touko-bench.XXXXXXXX)
python3 scripts/benchmark.py --root "$ROOT"
python3 scripts/plot-benchmark.py \
  --benchmark "$ROOT/benchmark.json" \
  --rss "0=$ROOT/profiles/0.jsonl" \
  --rss "1000=$ROOT/profiles/1000.jsonl" \
  --out-dir docs/images
```

## 文档

| 文档 | 内容 |
| --- | --- |
| [Webview](docs/webview.md) | 启动、安全边界、真实 GitHub 验收 |
| [CLI](docs/usage.md) | 从 demo 到多设备、恢复与 JSON 输出 |
| [架构](docs/code-guide.md) | 模块边界、调用链与设计取舍 |
| [函数导航](docs/function-reference.md) | 每个生产函数的输入输出与存在理由 |
| [仓库格式](docs/repository-format.md) | 远端目录结构与版本演进 |
| [本地状态](docs/local-state.md) | SQLite 缓存、耐久暂存与重建 |
| [发布协议](docs/publication.md) | 提交、恢复与删除语义 |
| [Benchmark](docs/benchmark.md) | 可重复的 CLI/HTTP 基线与桌面 RSS 方法 |

## 边界

预览不是缩略图服务，本地和私有仓库不会生成公网链接。`rm` 只从最新树移除文件，Git 历史仍然保留。Linux/Wayland 已验证；Windows 与 macOS 已由发布流程构建并通过 CLI 冒烟测试，但 GUI、托盘和剪贴板路径尚未实机验收。

## 致谢

字体 Monaspace Radon、霞鹜文楷、Quicksand 和 Playfair Display 均为 SIL OFL。原生窗口由 [MyGo](https://github.com/egoist/mygo) 提供。视觉审查参考 Anthropic 的 `frontend-design` 指引。

## License

[MIT](LICENSE)
