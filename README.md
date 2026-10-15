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

## 可操作 demo

```sh
# 首次需要先下载依赖；demo 本身禁用网络构建下载。
go mod download all
bash examples/demo.sh
```

需要 Bash 和 Python 3.8+ 标准库。脚本实际运行四图 batch、三设备恢复、过期编辑冲突、共享/末引用删除、SQLite 重建和 stdin 验收；保留 demo 目录并输出继续操作命令。仓库内 `examples/images/` 有三张自生成样例。你提供的图片可用 `add logo/TOUKO.png` 导入，不会修改原图。

## 阅读与学习

1. [操作教程](docs/usage.md)：从 demo 到自己的图片、标签/分类、多设备、恢复与 JSON。
2. [架构与设计教程](docs/code-guide.md)：阅读顺序、模块边界、完整调用链、状态转换、设计取舍与修改练习。
3. [逐函数导航](docs/function-reference.md)：每个生产函数/方法的职责、输入输出、副作用与错误边界。
4. [GitHub 接入](docs/github.md)：专用 gallery、token、URL/Markdown、API 原子性限制。
5. [发布与恢复协议](docs/publication.md)；[开发进度记忆](MEMORY.md)。

当前交付为 CLI，不包含 Webview。用户已确认下一阶段再做简洁美观、参考 PicList 的图库，并在界面内管理图片标签与分类；现有 Tags/Collections 模型与 app 层可复用。未提供专用 GitHub repo/token，因此没有宣称线上 live smoke 成功。
