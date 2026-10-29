# URL 迁移：从内容哈希到 Asset ID

新上传的图片路径是 `objects/<namespace>/<asset-id>.<ext>`，URL 由实际路径推导，所以 GitHub Raw、jsDelivr 和 Cloudflare R2 三种链接自动一致。旧仓库里没有显式 `path` 的 Asset 仍按 `objects/<namespace>/sha256/aa/bb/<sha256>.<ext>` 读取，不需要先迁移才能打开。

迁移分两次提交完成，任何一次失败都不会让旧 metadata 指向不存在的新对象，也不会删除旧对象：

1. **对象阶段**：把新路径加入 Git tree，metadata 仍指向旧路径。旧 URL 继续可用。
2. **校验**：从真实发布位置读取每个新 Git 对象；GitHub 后端必须是公开仓库，并读取实际 Raw URL 校验字节；启用 R2 时先上传新 key，再读取实际公开 URL 校验字节。
3. **metadata 阶段**：一次提交把每个 Asset 的 `path` 改为新路径。旧对象仍在 tree 里。

GitHub 与 R2 之间没有事务。迁移只能保证：metadata 切换之前，新 Git 对象和新 R2 key 已经实际可读；任何校验失败都会停在旧 metadata。它不能保证跨服务同时可见，也不能抵御另一个客户端并发强推分支。

所有 Asset 的 `object.hash` 都是必需的 SHA-256。迁移用它与 Git blob 字节、公开 URL 字节逐字节比对；不存在无 hash 的迁移旁路。

## 0. 先处理 staged 改动

```sh
touko list
```

迁移不是普通提交批次。如果本地还有未提交的 Add/Edit/Delete，`migrate paths` 会以 `conflict` 拒绝执行，并保留这些 staged 改动。先 `touko commit` 或丢弃它们，再迁移。

## 1. 预演（默认行为）

```sh
touko migrate paths
touko migrate paths --json
```

`--apply` 不出现时命令只读：列出每个 Asset 的 `old_path → new_path`，不写远端、不写映射、不写 `plan.json`。`--dry-run` 仍可用，用于显式声明预览（与 `--apply` 互斥）。

预演仍会拒绝 staged 改动和未解决的 publication journal。如果本地索引落后于远端，先 `touko sync`。

## 2. 正式迁移

```sh
touko migrate paths --apply
```

成功时返回新的 head 与 `migrated/unchanged/failed` 计数。迁移会先写 durably 的精确映射：

```text
<state-dir>/migration/plan.json
```

它记录每个 Asset 的 `asset_id`、旧路径、新路径、对象描述、源 Git blob、源 SHA-256。恢复、校验、映射重建和后续清理都只认这份映射，绝不从当前 hash 猜测旧路径。`--output` 不会移动 `plan.json`。

迁移过程：

1. 校验旧对象类型、大小、内容摘要；同仓库内新路径直接复用旧 Git blob，不重复上传图片。
2. 对象提交发布后，读取新 Git 对象并校验；GitHub 公开 URL 与 R2 公开 URL 逐字节校验（大小 + SHA-256）。GitHub 公开 URL 失败会让迁移停在旧 metadata；重跑即可。
3. 只有全部目标校验通过后才发布 metadata 提交，并把 v1/v2 原子升级为 v3。旧客户端会拒绝 v3，避免它们把旧 R2 key 误判为待删除；先升级并重启所有设备。
4. 写出映射文件。

映射文件默认在 `<state-dir>/migration/`：

```text
url-mapping.csv    asset_id, old_path, new_path, old_github_url, new_github_url,
                   old_r2_url, new_r2_url, size_bytes, status, error, sha256
url-mapping.json   同一内容的数组，便于脚本处理
```

两个文件用原子替换写入；失败或崩溃不会留下半份映射。只有本次存在至少一个 `old_path != new_path` 的条目时才写映射；全新仓库的空操作不会生成没有可用变更的映射。`--output DIR` 可改写映射输出目录。

### 私有仓库 / localgit

GitHub 私有仓库没有可公开校验的 Raw URL。启用 GitHub 后端且仓库为私有、或无法构造/读取公开 URL 时，迁移在校验阶段 fail closed，不会切换 metadata。`localgit` 后端没有公开 URL，只做 Git 对象校验。

R2 公开 URL 也会被实际读取；CDN 延迟或暂时 5xx 会让迁移失败在旧 metadata，重跑即可。迁移不会因为“HTTP 200 但字节不同”而通过。

## 3. 失败与恢复

失败时旧 metadata 和旧 URL 仍然有效；对象阶段可能已经发布，这是安全的中间状态。

- `touko sync` 会读取 durable publication journal。若 candidate 已发布，它先校验完整 tree delta 再落本地状态；若分支仍在 base，它丢弃 journal。两种情况下都不会清空 staged 改动。
- 迁移中断后先 `touko sync`，再直接重跑 `touko migrate paths --apply`：已存在的目标对象会被校验并复用，然后只执行 metadata 阶段。
- 即使 metadata 已切换而映射文件尚未写出，`plan.json` 仍能让下一次运行重建 `old_path → new_path` 与旧公网 URL；映射条目的 `status` 为 `migrated`。
- 如果远端有并发提交，迁移返回 `conflict`，不会把别人的提交混进迁移。
- GitHub 的 ref 更新是 fast-forward 加一次前置读取，不是跨服务 CAS；并发 force reset 无法被完全排除。

## 4. 更新引用

```sh
python3 scripts/rewrite-object-urls.py \
  --mapping "$STATE/migration/url-mapping.json" \
  --root ~/Documents/GitHub/blog \
  --root ~/Documents/GitHub/Eurekaimer.github.io \
  --root ~/Documents/GitHub/CS61A
```

默认是 dry run，只列出会改哪些文件；加 `--apply` 才写入。脚本只替换映射文件中同一仓库、分支或 R2 域名下的 `objects/...` URL；不会改写其他仓库中同名的路径。它跳过 `dist/`、`node_modules/` 和图片等二进制文件。

同一个旧对象路径可能被多个 Asset ID 共享；映射里会为每个 ID 各写一行，旧路径相同、新路径不同。

重写脚本（`--mapping/--root/--apply`）对这种情况的规则是 fail closed：只有所有冲突行都是 `migrated`、大小相同且带有相同的非空 SHA-256 时，才会确定性地选择字典序最小的新路径并打印警告；任何一行失败、缺失 proof 或 proof 不一致，整份映射都会拒绝应用。

被选中的那个 UUID 是引用现在实际指向的对象。之后删除该 Asset 会让这些已重写的链接失效；映射文件不是永久重定向。需要保留链接时，不要删除被选中的 Asset。

## 5. 清理旧 Git object（不会清理 R2）

确认新 URL 可用、引用都更新之后，清理必须单独执行：

```sh
touko migrate paths --prune-old            # 只预览会删哪些
touko migrate paths --prune-old --apply    # 真正删除
```

清理默认也是预演；没有 `--apply` 不会写任何东西。它拒绝和 metadata 迁移在同一次调用里执行：如果当前仍有 Asset 未使用 asset-id 路径，命令返回 `conflict`。

`--prune-old --apply` 只删除 `plan.json` 里记录、且仍存在于当前 tree 的旧对象，并要求：

- 每个 Asset 都已使用 asset-id 路径，且新对象通过内容校验；
- 旧路径没有被任何当前 Asset 引用；
- 旧路径的 Git blob、大小、SHA-256 与映射完全一致；
- 删除前重新读取每个新对象的公开 GitHub/R2 URL 并逐字节校验；任何一个不可读都会中止清理。

它不会删除其他 `objects/` 文件、历史遗留对象、metadata 或映射之外的路径。自定义旧路径也由映射精确记录，不靠 hash 推导。

Git 无法证明仓库之外是否还有第三方复制了旧 URL。命令能保证的是“当前仓库没有任何 Asset 引用旧路径，且新链接已经真实可读”；执行前仍应先完成第 4 步的引用重写。

清理提交同时写入 `.touko/migration-retained.json`，列出所有需要继续保留的 R2 key。旧 R2 key 不会被自动删除：

- 删除前仍有旧 Git blob 在 tree 里时，支持 v3 的客户端普通 mirror 同步会跳过这些 key；旧客户端拒绝读取 v3；
- prune 之后由仓库内的 retention marker 继续保护，其他设备/state dir 也能读到；
- R2 的 `DeleteObject` 没有可靠的 `If-Match` 语义（实测不匹配的 `If-Match` 仍会删除），所以迁移不会假装 R2 删除是原子的，也不会自动执行。

要在未来真正清空旧 R2 key，需要一次明确的人工清理，并先移除 retention marker 中对应项；在那之前它们会继续占用存储。这是刻意的 fail-closed 取舍。

## 6. 回滚

metadata 切换是普通提交，旧 Git object 仍在同一棵树里。确认未执行 prune 时，直接在新提交之上 `git revert` 迁移提交即可恢复旧路径，不需要重新上传图片。对象阶段提交可以保留，重跑迁移会复用。

已经执行 `--prune-old --apply` 时，旧 Git object 只留在历史里，恢复需要从历史取回；R2 旧 key 仍保留，但由于 retention marker，普通同步不会删除它们。

## 7. 格式

支持 v1/v2/v3：迁移在 metadata 提交中原子升级到 v3，作为旧客户端的安全屏障；正常提交不会降级。所有版本都要求 Asset 有 SHA-256，迁移、发布、verify 和 R2 校验依赖它。迁移不改变 object 描述。
