<div align="center">

<img src="https://socialify.dev/Eurekaimer/TOUKO/image?custom_language=Go&font=Raleway&forks=1&language=1&logo=https%3A%2F%2Fraw.githubusercontent.com%2FEurekaimer%2FTOUKO-IMGBED-03%2Fmain%2Fobjects%2Fdefault%2Fsha256%2Fa6%2Fe0%2Fa6e002cde06d25d106a9e80bafcc7762003491f4a2bf4f5bc2a8332d5411c6b0.png&name=1&owner=1&pattern=Circuit+Board&pulls=1&stargazers=1&theme=Light" alt="TOUKO" width="640" />

# TOUKO

**把图片存进你自己的 Git 仓库。**

内容寻址 · Git 暂存语义 · 本地优先读取 · 多仓库统一图库

[![Go](https://img.shields.io/badge/Go-1.27%2B-00ADD8?logo=go&logoColor=white)](go.mod)
[![License](https://img.shields.io/badge/license-MIT-9c5238)](LICENSE)
[![Platform](https://img.shields.io/badge/platform-Linux%20%C2%B7%20macOS%20%C2%B7%20Windows-1b2a3d)](#安装)
[![Docs](https://img.shields.io/badge/docs-7%20guides-7f3f59)](docs/webview.md)

[简体中文](README.md) · [English](README_en.md)

</div>

---

TOUKO 是一个以 Git 仓库为数据库的图片库。它不像网盘那样把图片交给第三方托管，而是把每一张图、每一份元数据都写进**你拥有的仓库**——GitHub 是唯一数据源，jsDelivr 只是可选的分发层。

## 名字

TOUKO 取自《终将成为你》（やがて君になる）的**七海灯子**（Nanami Touko）。

> 好きって、なんだろう。
> 好きって、なに？
>
> —— 七海灯子

她用整个故事追问「喜欢」是什么，也追问自己是谁。图片库也在追问同类问题：一张图属于谁、存在哪里、谁能访问、删掉后还算不算存在。TOUKO 把答案交给 Git——每次改动都有记录，每个文件都以内容命名。

## 为什么不是另一个 PicGo

PicGo 和 PicList 解决的是「上传完就拿到链接」。TOUKO 解决的是「一年后我还能不能找回这张图，并确定它就是我当初上传的那一张」。

| | 传统图床客户端 | TOUKO |
| --- | --- | --- |
| 状态存放 | 远端目录 + 本地配置 | Git 仓库本身，可完整重建 |
| 去重 | 通常无 | 内容寻址，相同字节共用对象 |
| 发布 | 逐张即时上传 | 暂存后一次提交，失败保留现场 |
| 换机 | 重新配置 | `repo use` + `sync` 完整恢复 |
| 删除 | 删文件 | 有定义的删除，历史仍可追溯 |
| 规模 | 单仓库 | 多仓库统一图库，按容量分配 |

## 特性

- **仓库即数据库**——远端保存 Asset、对象、标题、标签、分类、MIME、尺寸与内容哈希。本地 SQLite 只是可重建的缓存。
- **内容寻址**——文件名是内容的 SHA-256。同样的字节天然去重，链接永远不会指向被覆盖的旧图。
- **暂存 → 提交**——上传、编辑、删除先进入暂存区；`commit` 才写成一个 Git 提交，中途失败不会留下半个图库。
- **本地优先读取**——预览读本地浅克隆，不联网、不打扰远端。需要最新状态时才 `sync`。
- **统一图库**——把多个仓库合并成一个浏览与上传入口，每个仓库仍保有自己的暂存、提交与恢复语义，上传按剩余容量自动分配。
- **真的删除**——从最新树移除，并可校验结果；Git 历史仍然保留，这是特性而非遗漏。
- **一个二进制**——Go 编写，无 Node 构建、无运行时 CDN；CLI、Web 界面与 MyGo 原生窗口共用同一套 HTTP 与业务逻辑。

## 界面

<div align="center">
  <img src="docs/images/webview-gallery.png" alt="TOUKO 图库界面" width="820" />
</div>

奶油米色与深藏蓝、霞鹜文楷与 Monaspace Radon；单击卡片即复制 Markdown 并在指针旁提示结果。支持拖放、剪贴板粘贴、按分类/标签/目录/内容哈希筛选，以及多仓库来源过滤。

## 安装

需要 Go 1.27.1 或更高版本，以及支持的 Git。

```sh
git clone https://github.com/Eurekaimer/TOUKO.git
cd TOUKO
go build -o touko ./cmd/touko
go build -o touko-desktop ./cmd/touko-desktop   # 可选：原生窗口
```

Arch 用户可以构建本地包（从当前 checkout 编译，同时安装 CLI、桌面条目与图标）：

```sh
cd packaging && makepkg -si
```

若桌面条目启动失败，通常是 `Exec=` 指向了不在会话 `PATH` 里的裸命令名——改成二进制绝对路径即可。

## 快速开始

```sh
# 连接一个已有仓库，或先 repo init 初始化
./touko --state-dir ~/.local/share/touko repo use Eurekaimer/TOUKO-IMGBED-01
./touko --state-dir ~/.local/share/touko sync

# 加一张图，整理元数据，发布
./touko --state-dir ~/.local/share/touko add photo.png --tag 旅行 --collection 示例
./touko --state-dir ~/.local/share/touko commit -m "images: add a photo"

# 找回来：按标题、文件名、标签、分类或内容哈希
./touko --state-dir ~/.local/share/touko find 旅行
./touko --state-dir ~/.local/share/touko find 4aff401b
```

常用命令：`status`、`ls`、`find`、`show`、`edit`、`rm`、`verify`、`reindex`、`import`。全部支持 `--json`。

## 图形界面

```sh
./touko --state-dir ~/.local/share/touko serve --port 7788
```

默认打开浏览器；`--no-open` 只启动服务。也可以用 `touko-desktop` 开一个原生窗口——无地址栏，但仍使用系统 WebView 渲染。

只监听本机回环地址，校验 Host、Origin 与 CSRF；GitHub 凭据留在服务端进程内，绝不进入网页。

## 图床与 CDN

GitHub 仓库是源，jsDelivr 是可选分发层：

```
https://cdn.jsdelivr.net/gh/OWNER/REPO@BRANCH/objects/<ns>/sha256/<aa>/<bb>/<hash>.<ext>
https://raw.githubusercontent.com/OWNER/REPO/BRANCH/objects/<ns>/sha256/<aa>/<bb>/<hash>.<ext>
```

jsDelivr 对 GitHub 包有体积上限，超过后整包返回 403；此时改用 raw 直链即可，内容完全相同。TOUKO 按容量把图片分配到多个仓库，并为每个 Asset 记录真实所在仓库。

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

## 边界

预览不是缩略图服务；本地与私有仓库不生成公网链接。`rm` 从最新树移除文件，Git 历史保留——要真正清除需要重写历史，TOUKO 不提供。当前只验证 Linux/Wayland；Windows 与 macOS 的剪贴板路径按 MyGo 契约实现，尚未实机验收。

## 致谢

字体 Monaspace Radon、霞鹜文楷、Quicksand、Playfair Display（均为 SIL OFL）；原生窗口由 [MyGo](https://github.com/egoist/mygo) 提供；视觉审查参考 Anthropic 的 `frontend-design` 指引。

## License

[MIT](LICENSE)
