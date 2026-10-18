# GitHub 接入

浏览器图库的完整准备与验收步骤见 [Webview 教程](webview.md)。网页不接收 token；推荐先在终端用 `touko auth login` 登录并复用 GitHub CLI（`gh`）凭据。以下 CLI 命令也可独立核对网页发布结果。构建要求以 `go.mod` 为准，当前为 Go 1.27.1；Git 需支持 SHA-1 repository 与 `show-ref --exists`。

## 准备专用 gallery

在 GitHub **自行创建专用图片仓库并勾选 Initialize with README**，不要把 TOUKO 源代码仓库当 gallery。Git Data API 不能为完全空仓库创建第一个 ref。分支应禁止 force push；若 branch protection 拒绝 TOUKO 写入，CLI 保留 staging，不绕过保护。

认证优先级为 **`GITHUB_TOKEN` → `GH_TOKEN` → 已有 gh 登录**；无凭据可匿名读取公开仓库，账户信息/可访问仓库列表和写入需要认证。显式环境凭据无效时不静默回退。`auth login` 委托真实 `gh auth login --hostname github.com --web --git-protocol https`，需先安装 gh；`auth status` 只显示登录名与来源，不显示 token。凭据不写入 config/SQLite/metadata，也不传到网页、URL 或命令参数。自行配置 fine-grained token 时只授权目标仓库 Contents read/write，不发进聊天。

```bash
# 在项目根目录构建；下列 OWNER/touko-demo 必须换成你的专用仓库。
go build -o /tmp/touko ./cmd/touko
# 已有 gh 登录可跳过 login；没有手工粘贴 token 的应用入口。
/tmp/touko auth login
export TOUKO_HOME="$(mktemp -d)"
/tmp/touko auth status
/tmp/touko repo list
/tmp/touko repo use OWNER/touko-demo
/tmp/touko repo init
ID=$(/tmp/touko add logo/touko-logo.png --namespace blog --tag demo --output id)
/tmp/touko status --json
/tmp/touko commit -m "demo: publish logo"
/tmp/touko url "$ID" --provider github
/tmp/touko url "$ID" --provider jsdelivr
/tmp/touko verify
```

如果没有用户 logo，使用 `examples/images/red.png`。`repo list` 完整分页列出可访问仓库，含默认分支、private、can_push；只读仓库不能发布。`repo use` 从 GitHub 获取 default branch 与 private 状态，可显式 `--branch BRANCH`。选择仓库不写远端；`repo init` 只增加 TOUKO manifest，保留 README 等文件，合法已有 manifest 幂等。

### 网页选择、多仓库与本地读取

```bash
/tmp/touko --state-dir "$HOME/.local/share/touko-galleries" serve --port 7788
# 不自动打开浏览器：
# /tmp/touko --state-dir "$HOME/.local/share/touko-galleries" serve --port 7788 --no-open
```

`serve` 默认打开浏览器，打开失败会提示手动访问且继续服务。“连接 / 切换仓库”里选择 GitHub 后自动读取登录与仓库列表，先选仓库再从它的分支下拉里选分支（默认分支已预选）；没有 token 输入框，也不需要手填 `OWNER/REPO`。gh 在终端完成登录后可直接重新读取，无需重启。若改的是环境变量，需要重启进程才能获得新环境。

每个仓库/分支是独立 URL 工作区（新工作区为 `/w/<key>/`），各自保存 clone、index、stage/journal；切换不会清空旧暂存，不必停止服务换 state。根工作区保持兼容，新工作区存于 `<state-root>/workspaces/<key>/`。独立 CLI 仍需按实际工作区路径传 `--state-dir`，不要拿根 state-dir 操作所有仓库。

首次 GitHub 读操作会建立浅克隆（`--depth 1`）。已有 clone 时预览和 `verify` 只读本地当前快照，不 fetch，不是“远端最新”证明；普通 `sync` 先更新 clone，再在**同一次调用**中索引新的固定 HEAD。发布结果未知的 journal 通过 API 恢复，不用浅克隆判断远端提交；`commit`、`repo init` 仍用 GitHub API 写入，不执行 `git push`。认证/仓库发现/列表同样会访问 API。

### PicGo 旧路径纳管与上传目录

```bash
LEGACY_STATE="$HOME/.local/share/touko-picgo"
/tmp/touko --state-dir "$LEGACY_STATE" repo use OWNER/old-gallery
# 若尚无 TOUKO manifest，必须由用户显式初始化；已有合法 manifest 时幂等。
/tmp/touko --state-dir "$LEGACY_STATE" repo init
/tmp/touko --state-dir "$LEGACY_STATE" sync
/tmp/touko --state-dir "$LEGACY_STATE" import --directory img --namespace blog --tag legacy
/tmp/touko --state-dir "$LEGACY_STATE" status --json
/tmp/touko --state-dir "$LEGACY_STATE" commit -m 'images: manage PicGo paths'
/tmp/touko --state-dir "$LEGACY_STATE" add examples/images/red.png --directory images/blog
/tmp/touko --state-dir "$LEGACY_STATE" commit -m 'images: upload into images/blog'
```

`import` 只读取固定 Git HEAD 中已提交的普通 PNG/JPEG/GIF/WebP；按真实 header 识别，支持无后缀和 Unicode 文件。`--directory img` 递归限定仓库路径，留空扫描全仓库；已有管理路径/非图片跳过，重复导入幂等。导入只暂存 metadata，保留原路径、原 Git blob 和原链接，不重新上传、不复制到 `objects/`。未初始化会拒绝，错误不留下部分导入；检查 imported/skipped 及暂存后必须显式 commit。删除纳管图并提交会按完整路径末引用规则移除原文件，不是仅取消纳管。

`add --directory` 是仓库目标目录，不是本机输入目录，新图用内容 hash 命名。路径为规范相对 POSIX 路径，禁止穿越、反斜杠、控制字符、`.git/.touko` 路径段。**首次提交含显式 `path` 的导入/目录上传会原子升级 manifest 到 format v2；旧客户端 fail-closed 拒绝读取，请先升级每台设备。** 现有 v1 可读，默认 canonical 上传不强制升级。网页有同样的旧图库导入、目标目录、真实路径搜索与目录筛选。

缓存损坏/删除可用 `touko --state-dir "$LEGACY_STATE" reindex` 完整验证后重建 committed cache，保留独立耐久 `workspace.json` 和暂存；有 journal 之后仍需 `sync`，有 stage 且 HEAD 变化会冲突。旧 schema1 必须先从健康旧 DB 自动迁移，迁移前不可删除唯一未发布状态。具体物理损坏备份、工作区路径与恢复步骤见 [CLI 恢复指南](usage.md#8-恢复cache-与故障排查)。

## 一键真实验收脚本

**这是单独的示例 live 脚本，不是应用认证入口。** 应用 CLI/Web 可复用 gh，但既有 `examples/github-acceptance.sh` 显式检查 `GITHUB_TOKEN`，运行它仍须按下列步骤提供该环境变量；本轮没有修改脚本。

### 第 1 步：建专用仓库

打开 https://github.com/new，仓库名例如 `touko-gallery-test`，**必须勾选 Add a README file**（Git Data API 无法在完全空仓库创建第一个 ref），不要勾 .gitignore 或 license。公开还是私有：

| | 公开 | 私有 |
| --- | --- | --- |
| 存储、同步、编辑、界面预览、校验 | ✅ | ✅ |
| `url` 生成公网链接（raw / jsDelivr） | ✅ | ❌ 明确报 `delivery_unavailable` |

脚本对公开仓库会额外下载 raw 字节并比对 SHA-256；私有仓库会跳过该步并改为断言 URL 被拒绝。

### 第 2 步：生成最小权限 token

打开 https://github.com/settings/personal-access-tokens/new：

1. Token name：`touko-acceptance`；Expiration：7 天
2. Repository access → **Only select repositories** → 只勾上面那个专用仓库
3. Permissions → Repository permissions → **Contents** 设为 **Read and write**（其余保持 No access）
4. Generate token，复制 `github_pat_...`

仅在运行脚本的终端用隐藏输入读取 token：不要直接把值写入 export 命令、聊天、参数或任何文件/Git。不要使用输出凭据的命令来检查登录，使用 `touko auth status`。

### 第 3 步：把下面整段粘进终端（改掉仓库名）

```bash
# 在项目根目录运行，替换成自己的专用仓库。
read -rsp 'GitHub token: ' GITHUB_TOKEN; printf '\n'
export GITHUB_TOKEN
export TOUKO_ACCEPT_REPO='你的用户名/touko-gallery-test'

bash examples/github-acceptance.sh "$TOUKO_ACCEPT_REPO"

unset GITHUB_TOKEN TOUKO_ACCEPT_REPO
```

默认只上传 `examples/images/` 里三张自生成样例。若你**希望**把你本机的 `logo/touko-logo.png` 也一起发布到该仓库（公开仓库即对所有人可见），在运行前加上：

```bash
export TOUKO_ACCEPT_INCLUDE_LOGO=1
```

不设置该变量时脚本会打印一行说明并跳过你的原图。

失败时脚本非零退出并保留 `/tmp/touko-github.XXXXXXXX` 现场；若报 `expected exit 0, got 1` 且带 `unauthorized`/`404`，通常是 token 权限、仓库名或 README 未勾；若报 `AssertionError`，那是 TOUKO 自身行为不符预期，请把整段输出交给维护者。

### 脚本断言的内容

脚本只从 `GITHUB_TOKEN` 读取凭据，不接受 token 参数、不打印 token、不写入凭据到磁盘（已用假 token 实测：失败时报 GitHub 的真实 401，输出中不含 token 字符串）；拒绝 `eurekaimer/touko`（源码仓库）作为 gallery。它依次断言：`auth status` 已认证、`repo use` 选中期望仓库与分支、`repo init` 幂等、批量 `add` 不移动远端 HEAD、暂存态 `url` 报 `committed:false`、`commit` 发布全部变更并清空暂存、两个 provider 的 URL 与 canonical object path 一致（private 仓库改为断言 `delivery_unavailable`）、`verify` 的 assets/objects 与 HEAD 一致、公开仓库 raw bytes 的 SHA-256 等于记录的 object hash、第二设备 `sync` 后完整 metadata 逐字段相等、最后一条引用的删除在第三台全新设备上不可见。真实远端不可达或断言失败时脚本非零退出并保留工作目录现场。

脚本的公开仓库验收记录见下文“协议与限制”；不能扩大为私有仓库或所有新增场景都已验证。脚本如何工作与某次是否运行是两件事。

## 第二设备与删除验收

记录第一个 `TOUKO_HOME` 为 A，另开全新目录 B，在有权访问的 gh 登录或环境凭据下执行 `repo use OWNER/touko-demo; sync; ls --json`，比较完整 metadata（含显式 path）。A `rm "$ID"; commit -m "demo: delete logo"` 后，用第三个全新 state-dir use/sync，确认 ID 消失并 verify。若该实际完整路径还有其他 Asset 引用，binary 应保留；最后引用删除才从最新 tree 移除。

删除前可 GET raw GitHub URL 并校验 bytes 的 SHA-256。jsDelivr 链接推导成功不代表 CDN 已传播；CDN 延迟不等于 remote commit 失败。完成后：

```bash
unset GITHUB_TOKEN
```

## URL 与 Markdown

- `url ID --provider github|jsdelivr` 根据当前 location 的 **branch** 派生 URL，不把 URL 存入 Asset。
- `url ID --json` 输出 `{"url":"...","committed":false}` 表示新 Asset 尚未发布。metadata-only modified Asset 的 binary 已提交，因此 committed 为 true。
- `cat logo/touko-logo.png | /tmp/touko add - --name '剪贴板.png' --namespace blog --output markdown` 输出图片 Markdown，并在 stderr 提示 staged/not published。必须 commit 后才可作为已发布链接使用。
- `--output id|markdown` 均不能和 `--json` 同用；冲突在 mutation 前拒绝。
- localgit 与 private GitHub repository 不提供公网 delivery；Markdown 请求也在 staging mutation 前拒绝。private repo 仍支持存储、恢复和 verify。
- 显式路径（PicGo 原路径或目标目录）的 URL 使用实际 path，不假定 canonical `objects/...`，Unicode/空格等由 provider 正确编码。Web 可选择 provider 后“复制 Markdown”；权限失败时显示并选中文本框供手动复制。当前浏览器验收仅确认 Markdown 正确生成及手动复制框，不声称自动写入剪贴板成功。

## 协议与限制

固定生产 endpoint 为 `https://api.github.com`，API header `2026-03-10`。blob GET 使用 raw bytes，POST 使用流式 base64，不把图片/base64 整体装入内存。每次只有一个 blob worker。取消/timeout 独立关闭 pipe/source 并等待 encoder 退出，防止 HTTP transport 等待 request-body writer 造成死锁。阻塞型自定义 Reader 必须支持取消或 io.Closer；CLI 的本地文件符合此要求。

上传取消时会关闭源和管道并等待编码器退出；即使 HTTP transport 先观察到管道关闭，返回错误仍保留 `context.Canceled` / `context.DeadlineExceeded` 的分类，同时保留原 transport 错误。`TestUploadCancellationClosesBlockedSourceAndJoinsWriter` 覆盖取消后的源释放与调用返回。

Tree 写入包含 `base_tree`，删除显式 `sha:null`；recursive tree `truncated:true` 拒绝构建部分 cache。401/权限403 映射 unauthorized；限流403/429/5xx/timeout 不隐藏重试，保留 stage。journal 决定发布恢复行为，见 [publication](publication.md)。

**GitHub `force:false` 只保证 fast-forward，不提供 expected-old-SHA CAS。** TOUKO 固定唯一 parent 为 staging base，发布前复查 HEAD，正常 non-force 并发由 non-fast-forward 拒绝保护；无法保证外部管理员任意 force/reset 与 PATCH 之间的严格 CAS。Local Git 的 update-ref 则有 expected-old CAS。

此前真实 GitHub live 验收已执行（专用公开仓库，2026-10-16）。脚本 13 项断言通过，包括：`auth status` 只显示登录名、`repo use` 选中 main 分支、`repo init` 幂等、批量 `add` 不移动远端 HEAD、`commit` 发布 3 项变更、两个 provider 的 URL 与 canonical object path 一致、`verify` assets=3 objects=3、raw GitHub 字节的 SHA-256 等于记录的 object hash、第二设备 `sync` 后逐字段 metadata 相等、末引用删除在第三台设备不可见。这是历史记录，不是当前 checkout 已重新执行 live 验收的声明。

该次验收的独立复核（不是当前所有分支格式的声明）：以 raw CDN 直取远端，`.touko/repository.json` 返回 200 且 `format_version:1`、`repository_id: dc117605-…`、`hash_algorithm: sha256`；原 `README.md` 仍为 200（`repo init` 只增加 manifest，不重写既有文件）；伪造路径与不存在的文件返回 404。本轮包含显式 path 的测试分支则升级为 v2。

本轮增量使用该专用仓库独立分支 `touko-handoff-20261016-133916`，已实际经浏览器导入旧路径并显式发布，没有改 main。`Eurekaimer/MyIMGs` 仅只读克隆到本地 bare 副本后扫描 `img/`，得到 204 张（其中 16 张无后缀），没有写原仓库。已实际验证 gh keyring 认证、schema1 迁移保留数据、删除/物理损坏 index 后 reindex 保留暂存并继续发布、无后缀/Unicode 原 Git blob 保持、v2 升级、第二设备完整 metadata 恢复、单次 sync 更新 HEAD、目录上传/编辑/删除；这不代表后列限制已经覆盖。

仍未验证的部分：私有仓库路径（现有实测使用公开仓库，脚本私有分支会改为断言 `delivery_unavailable`）、分支保护下被拒的写入、组织仓库的 token 审批。不能由公开仓库成功推断这些权限与策略分支也已通过。

协议依据：[refs](https://docs.github.com/en/rest/git/refs)、[trees](https://docs.github.com/en/rest/git/trees)、[blobs](https://docs.github.com/en/rest/git/blobs)、[compare](https://docs.github.com/en/rest/commits/commits#compare-two-commits)。
