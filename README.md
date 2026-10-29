<div align="center">

<img src="https://img.eurekaimer.icu/objects/default/sha256/fa/93/fa93a8ca741cfa57253de4eed814435452ee5fce2863932f0112f7e94455041a.webp" alt="TOUKO" width="220" />

# TOUKO

把自己的图片存进自己的 Git 仓库。

[![Go](https://img.shields.io/badge/Go-1.27%2B-00ADD8?logo=go&logoColor=white)](go.mod)
[![License](https://img.shields.io/badge/license-MIT-9c5238)](LICENSE)
[![Platform](https://img.shields.io/badge/platform-Linux%20%7C%20Windows%20%7C%20macOS-1b2a3d)](#安装)

[简体中文](README.md) · [English](README_en.md)

</div>

TOUKO 是一个以 Git 仓库为数据源的图片库。图片字节和元数据都进仓库，GitHub 负责托管；默认用 jsDelivr 分发，也可以开启可选的 Cloudflare R2 镜像。本地 SQLite 只做读取缓存，删掉后能从仓库重建。

图片先进入暂存区，`commit` 才写成一次 Git 提交。文件名是 Asset UUID，路径形如 `objects/<namespace>/<asset-id>.<ext>`，同一张图上传两次是两个独立记录。库里记录标题、标签、分类、MIME、尺寸和 SHA-256，用来校验字节，不参与 URL。

## 功能

- 内容寻址的图片与元数据，`commit` 是一次原子发布
- 暂存、提交、同步分离；`rm` 只从最新树移除，Git 历史保留
- 多仓库图库：每个仓库仍有自己的暂存和发布流程
- 网页与桌面界面：拖放或粘贴上传、搜索、按标签/分类/目录筛选、一键复制 Markdown
- 可选 Cloudflare R2 镜像，发布成功后才上传，失败不影响 GitHub
- 设置面板：界面与字体缩放，Cloudflare 配置，R2 凭据保存在本机
- 分类和标签可以折叠，每页 12 条

## 安装

发布版覆盖 Linux（amd64/arm64）、Windows amd64 和 macOS（amd64/arm64）。从 [Releases](https://github.com/Eurekaimer/TOUKO/releases) 下载 `v0.2.0` 的归档和同名 `.sha256`，校验后解压。

Linux：

```sh
sha256sum -c touko_0.2.0_linux_amd64.sha256
tar -xzf touko_0.2.0_linux_amd64.tar.gz
sudo install -m755 touko_0.2.0_linux_amd64/bin/touko touko_0.2.0_linux_amd64/bin/touko-desktop /usr/local/bin/
sudo cp -R touko_0.2.0_linux_amd64/share/. /usr/local/share/
touko version
```

Windows 解压 `.zip` 后运行 `bin\touko-desktop.exe`，需要 WebView2 Runtime。macOS 把 `TOUKO.app` 拖进「应用程序」，首次打开若被 Gatekeeper 拦截，用右键「打开」放行。

Linux 桌面需要 GTK3、WebKitGTK 4.1 和托盘宿主；图形化 GitHub 登录需要安装一次 [GitHub CLI](https://cli.github.com/)。CLI 和桌面都依赖 Git 2.46 或更高版本。

从源码构建：

```sh
git clone https://github.com/Eurekaimer/TOUKO.git
cd TOUKO
go build -trimpath -o touko ./cmd/touko
go build -trimpath -o touko-desktop ./cmd/touko-desktop
```

## 快速开始

```sh
touko repo use Eurekaimer/TOUKO-WEBP-01
touko sync
touko add photo.png --tag 旅行 --collection 示例
touko commit -m "images: add a photo"
touko find 旅行
touko url <UUID> --provider jsdelivr
```

`touko --help` 列出全部命令：`add`、`edit`、`rm`、`ls`、`find`、`show`、`status`、`commit`、`sync`、`verify`、`reindex`、`import`、`repo`、`auth`、`serve`、`url`、`cloudflare`、`reset`、`version`。命令都支持 `--json`。

## 图形界面

```sh
touko serve --port 7788        # 浏览器，加 --no-open 不自动打开
touko-desktop                  # 原生窗口
```

服务只监听 `127.0.0.1`，校验 Host、Origin 和 CSRF。GitHub 凭据留在服务端进程内。

左下角齿轮打开设置：

- 显示与外观：界面缩放、字体缩放
- Cloudflare R2：启用开关、endpoint、bucket、公开访问地址、密钥
- 密钥写入工作区状态目录的 `cloudflare/credentials.json`（目录 0700，文件 0600），打开设置时按掩码回填，点「显示」才能看到明文

保存设置不会上传或同步图片。关闭开关会保留已填的地址，重新打开时不必再输。

## 可选 Cloudflare R2

默认分发仍是 jsDelivr。R2 是可选的镜像，需要在设置里填写，或直接写进工作区的 `config.json`：

```json
"cloudflare": {
  "enabled": true,
  "endpoint": "https://<ACCOUNT_ID>.r2.cloudflarestorage.com",
  "bucket": "touko-imgs",
  "public_base_url": "https://img.eurekaimer.icu"
}
```

`endpoint` 是不带路径的 HTTPS 地址，region 固定 `auto`。`public_base_url` 必须显式填写，TOUKO 不从 endpoint 推断域名。凭据只从设置面板或两个环境变量读取，不读 `.env`、AWS profile 或 gh 登录：

```sh
export TOUKO_R2_ACCESS_KEY_ID=...
export TOUKO_R2_SECRET_ACCESS_KEY=...
```

开启后，GitHub 发布确认完成才会上传本批新增图片。已有图库和失败任务用：

```sh
touko cloudflare sync --dry-run
touko cloudflare sync
touko url <UUID> --provider cloudflare
```

只有状态为已同步、且服务端实时 HEAD 校验通过的图片才返回 Cloudflare 链接。暂存中的图片不能复制。自定义域名的绑定、DNS 和缓存由 Cloudflare 管理，TOUKO 不代为配置。

## 性能

一次 `commit` 只发必要的写请求：单图 = 5 个 GitHub API 请求（图片 blob、metadata blob、tree、commit、更新 ref），删除 = 3 个（开启 R2 时另有 1 次删除前确认）。图库里有 200 张还是 2000 张都不增加请求数。基准数据来自 200 张图的公开仓库、同一台机器与网络（GitHub 每请求 0.3–0.9 s）：

| 操作 | GitHub 请求 | R2 调用 | 耗时 |
| --- | ---: | ---: | ---: |
| `add`（仅暂存，不联网） | 0 | 0 | 0.05 s |
| 提交一张新图，镜像开 | 5 | 5 | 7.0 s |
| 提交一张新图，镜像关 | 5 | 0 | 6.0 s |
| 删除一张图，镜像开 | 4 | 2 | 5.6 s |

剩余时间主要是网络往返：每个 GitHub 请求 0.3–0.9 s，R2 写入还要回读校验。完整的请求清单、方法、对比历史和限制见 [Benchmark](docs/benchmark.md#single-image-publication-against-a-real-gallery)。

## 文档

[CLI 用法](docs/usage.md) · [URL 迁移](docs/migration.md) · [Webview](docs/webview.md) · [架构](docs/code-guide.md) · [函数导航](docs/function-reference.md) · [仓库格式](docs/repository-format.md) · [本地状态](docs/local-state.md) · [发布协议](docs/publication.md) · [Benchmark](docs/benchmark.md)

## 边界

单张图片上限 100 MB。WebP 转换只在结果更小时替换原图，GIF 和已有 WebP 保持原样。预览不是缩略图服务。本地仓库和私有 GitHub 默认不生成公开链接，私有仓库可以显式开启 Cloudflare 公开分发。Git 历史不会因为 `rm` 被删除。

Linux/Wayland 已做界面验收。Windows 与 macOS 由发布流程构建并通过 CLI 冒烟测试，GUI、托盘和剪贴板仍待实机确认。

## 致谢

名字来自《终将成为你》的七海灯子。字体 Monaspace Radon、霞鹜文楷、Quicksand 和 Playfair Display 均为 SIL OFL。原生窗口由 [MyGo](https://github.com/egoist/mygo) 提供。

## License

[MIT](LICENSE)
