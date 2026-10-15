# 从运行 demo 到管理自己的图片

本文讲实际操作；想理解代码，接着读 [架构学习路线](code-guide.md) 和 [逐函数导航](function-reference.md)。协议细节分别在 [format](repository-format.md)、[local state](local-state.md)、[publication/recovery](publication.md)、[GitHub](github.md)。

## 1. 环境与构建

CLI 需要 Go 1.26+（构建时）、Git（localgit backend；须支持 `show-ref --exists`）、可写的本地 state 目录。SQLite 是纯 Go driver，不需要 CGO 或另外启动数据库服务。离线 demo 另外需要 Bash、Python 3（仅标准库断言，不是 CLI 的运行依赖）。

在项目根目录：

```bash
go test ./...
go vet ./...
CGO_ENABLED=0 go build -o /tmp/touko ./cmd/touko
/tmp/touko --help
```

不要直接修改 `.touko` metadata 或 SQLite 表来操作图片；使用 CLI 保持内容描述、staging base 和 publication journal 一致。

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

用户提供的 `logo/TOUKO.png` 保持原文件不动。可继续使用 demo 的 binary/state，也可按下一节另建 gallery。

```bash
ID=$("$ROOT/touko" --state-dir "$ROOT/device-a" add logo/TOUKO.png \
  --namespace blog --title '我的 TOUKO logo' --tag demo --tag logo \
  --collection '项目素材' --output id)
printf 'Asset ID: %s\n' "$ID"
"$ROOT/touko" --state-dir "$ROOT/device-a" status --json
"$ROOT/touko" --state-dir "$ROOT/device-a" show "$ID"
"$ROOT/touko" --state-dir "$ROOT/device-a" commit -m 'images: add my logo'
"$ROOT/touko" --state-dir "$ROOT/device-a" verify
```

`add` 不发布远端。UUID 是图片记录身份，不是文件路径或 hash。再次导入相同 bytes 得到新 UUID；同 namespace 共用一份 binary。`--output id` 只输出 UUID，适合 shell 捕获，不能和 `--json` 合用。

## 4. 从零建一个自己的离线 gallery

```bash
# 以下目录应由你选择；gallery 目标不能是已有非空目录。
export TOUKO_HOME="$HOME/.local/share/touko-my-gallery"
/tmp/touko repo init --local "$HOME/Pictures/my-gallery.git"
ID=$(/tmp/touko add logo/TOUKO.png --namespace blog --title '我的 logo' --tag demo --output id)
/tmp/touko status
/tmp/touko commit -m 'images: initial import'
/tmp/touko ls --json
/tmp/touko verify
```

Gallery 是 **bare Git repository**，没有普通 checkout 工作区。可以用 `git --git-dir "$HOME/Pictures/my-gallery.git" ls-tree -r --name-only main` 查看当前 committed paths；不要用文件浏览器找工作区图片。

State 目录优先级为 `--state-dir` → `TOUKO_HOME` → `$XDG_DATA_HOME/touko` → `$HOME/.local/share/touko`。每台设备/每个 gallery 用独立 state-dir；`repo use` 不会把已有另一仓库的 cache/stage 混进来。

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
- 未实现 namespace move、换图、thumbnail、GUI 或自动合并；CLI 不提供假命令。

## 6. stdin 与批量导入

```bash
cat logo/TOUKO.png | /tmp/touko add - --name '剪贴板.png' --namespace notes --output id
/tmp/touko add examples/images/red.png examples/images/green.png examples/images/blue.png --tag demo
/tmp/touko status --json
/tmp/touko commit -m 'images: batch import'
```

一次 batch 的任一输入失败，该批不会留下部分 stage rows。stdin `-` 只能单独使用，不能混其他文件；`--name` 仅对 stdin 有效。未指定 stdin name 则按实际格式命名为 stdin.png/jpg/gif。图片格式由 header 决定，不信文件扩展名。当前支持 PNG/JPEG/GIF，单个最大 100,000,000 bytes，不整张图片加载到内存。

## 7. 删除、撤销与多设备

```bash
/tmp/touko rm "$ID"
/tmp/touko status --json
/tmp/touko commit -m 'images: remove logo'
/tmp/touko verify
# 尚未发布的修改可全部丢弃；不是逐条撤销：
/tmp/touko reset --staged
```

最后一个引用删除才删当前 tree 的 binary，跨 namespace 独立计数。`rm` 不擦 Git 历史，不承诺 CDN 全球缓存立刻失效。对 staged add 执行 rm 只是取消新增；已 staged-delete 再 rm 幂等，但 delete→edit 是冲突，不隐式恢复。

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
| publication outcome unknown | 保留数据库与 stage，运行 sync | 不盲目重新上传，不删除 journal |
| published head … local index failed | 远端可能已提交；修复本地存储后 sync | 不把它解释成远端没提交 |
| repository_corrupt/hash_mismatch | 保留旧 cache，检查远端固定 HEAD 与 payload | 不跳过坏 metadata 生成部分 gallery |
| unauthorized/api_error | 修复 token/权限/branch protection 或等待限流，再重试 | 不把 token 写进命令参数或聊天，不绕过保护 |

有 staging/publication 时不要删除 `index.db`：它们是未发布状态，远端恢复不了。仅在 CLI 已退出且 stage clean 时，可保留 config、删除自己 scratch state 的 index.db/WAL/SHM，然后 sync 重建 committed gallery。demo 会实际演示，不需要你删自己的生产数据。

GitHub 接入按 [专用指南](github.md) 执行。local/private repo 的 url/Markdown 不可用是正确行为，不会输出假公网链接。token 和专用 repo 未提供时，HTTP 测试不等于 GitHub live smoke。

## 9. JSON 与退出码

stdout 只放命令结果；错误在 stderr。`--json` 错误形状为 `{"error":{"code":"conflict","message":"..."}}`。成功0、usage2、conflict3、其他错误1。

| 命令 | JSON 外层 |
| --- | --- |
| add/ls/find | `{"assets":[AssetView...]}` |
| show | `{"asset":AssetView}` |
| status | `{"head":"...","base_head":"...","changes":[...]}` |
| sync | `{"head":"...","asset_count":N}` |
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

先根据 [逐函数导航](function-reference.md) 找调用链，再阅读相关行为测试。优先修改已有模块，不另建通用 Storage/Plugin 层。新增用户可见行为要验证真实 CLI/Git/SQLite 状态，不写只验证调用转发或函数不 panic 的测试。提交后同步 MEMORY.md；当前实现提交和验证证据在那里记录。
