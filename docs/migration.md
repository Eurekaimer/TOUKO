# URL 迁移：从内容哈希到 Asset ID

新上传的图片路径是 `objects/<namespace>/<asset-id>.<ext>`，URL 由实际路径推导，所以 GitHub Raw、jsDelivr 和 Cloudflare R2 三种链接自动一致。旧仓库里没有显式 `path` 的 Asset 仍按 `objects/<namespace>/sha256/aa/bb/<sha256>.<ext>` 读取，不需要先迁移才能打开。

迁移会把已有 Asset 一次性改写成新路径，同时保留旧 object，因此旧链接在迁移后仍然可用，直到你确认并执行清理。

## 1. 预演

```sh
touko migrate paths --dry-run --json
```

只读：列出每个 Asset 的 `old_path → new_path`，不写远端、不写文件。如果本地索引落后于远端，先 `touko sync`。

## 2. 正式迁移

```sh
touko migrate paths
```

一次操作完成：

1. 把每个 Asset 的图片字节复制到新路径（GitHub blob、tree、commit 一次提交）。
2. 改写 `.touko/assets/<uuid>.json` 的 `path`。
3. 发布并逐个校验新 object（Git 内容校验，不是只看 HTTP 200）。
4. 启用了 R2 时，把新 key 同步到镜像。
5. 写出映射文件。

旧 object 仍留在 tree 里，所以旧 URL 不会立刻失效。提交是原子的：失败时分支不变，journal 仍可由 `touko sync` 恢复。

映射文件默认在 `<state-dir>/migration/`：

```text
url-mapping.csv    asset_id, old_path, new_path, old_github_url, new_github_url,
                   old_r2_url, new_r2_url, size_bytes, status, error
url-mapping.json   同一内容的数组，便于脚本处理
```

`--output DIR` 可改写输出目录。

## 3. 更新引用

```sh
python3 scripts/rewrite-object-urls.py \
  --mapping "$STATE/migration/url-mapping.json" \
  --root ~/Documents/GitHub/blog \
  --root ~/Documents/GitHub/Eurekaimer.github.io \
  --root ~/Documents/GitHub/CS61A
```

默认是 dry run，只列出会改哪些文件；加 `--apply` 才写入。脚本只替换仓库内路径 `objects/...`，因此 jsDelivr、raw.githubusercontent 和自定义 R2 域名三种形式同时生效，并且跳过 `dist/`、`node_modules/` 和图片等二进制文件。

没有出现在映射里的路径说明对应 Asset 已不存在（历史删除），脚本不会改动它们；这些链接本来就是失效的。

## 4. 清理旧 object

确认新 URL 可用、引用都更新之后：

```sh
touko migrate paths --prune-old --dry-run   # 先看会删哪些
touko migrate paths --prune-old
```

只删除 tree 中没有任何 Asset 引用的 `objects/` 条目，不改 metadata；R2 会随下一次镜像同步删除对应 key。不要在新对象验证之前执行。

## 5. 回滚

迁移是一个普通提交，旧 object 仍在同一棵树里，所以直接在新提交之上还原旧 metadata（例如 `git revert` 迁移提交）即可恢复旧路径；不需要重新上传图片。已经执行 `--prune-old` 时，旧 object 只会留在 Git 历史里，还原需要从历史取回。

## 6. 格式

迁移不新增 format 版本：Asset 带显式 `path`，沿用 format v2。v1/v2 旧仓库照常读取；新上传的第一次提交通过既有的原子升级写入 v2 manifest。
