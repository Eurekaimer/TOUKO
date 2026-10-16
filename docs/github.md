# GitHub 接入

浏览器图库的完整准备与验收步骤见 [Webview 教程](webview.md)。网页不接收 token；先在启动 `serve` 的终端配置 `GITHUB_TOKEN`。以下 CLI 命令也可用于独立核对网页发布结果。

## 准备专用 gallery

在 GitHub **自行创建专用图片仓库并勾选 Initialize with README**，不要把 TOUKO 源代码仓库当 gallery。Git Data API 不能为完全空仓库创建第一个 ref。分支应禁止 force push；若 branch protection 拒绝 TOUKO 写入，CLI 保留 staging，不绕过保护。

用户在执行机配置仅授权该仓库 Contents read/write 的 fine-grained token；不要发进聊天或作为命令参数。TOUKO 只读取 `GITHUB_TOKEN`，不保存到 config/SQLite/metadata。公开仓库可匿名读取；写操作和 auth status 需要 token。

```bash
# 在项目根目录构建；下列 OWNER/touko-demo 必须换成你的专用仓库。
go build -o /tmp/touko ./cmd/touko
read -rsp 'GitHub token: ' GITHUB_TOKEN; printf '\n'
export GITHUB_TOKEN
export TOUKO_HOME="$(mktemp -d)"
/tmp/touko auth status
/tmp/touko repo use OWNER/touko-demo
/tmp/touko repo init
ID=$(/tmp/touko add logo/TOUKO.png --namespace blog --tag demo --output id)
/tmp/touko status --json
/tmp/touko commit -m "demo: publish logo"
/tmp/touko url "$ID" --provider github
/tmp/touko url "$ID" --provider jsdelivr
/tmp/touko verify
```

如果没有用户 logo，使用 `examples/images/red.png`。`repo use` 从 GitHub 获取 default branch 与 private 状态；可显式 `--branch BRANCH`。选择仓库不写远端；`repo init` 只增加 TOUKO manifest，保留 README 等文件；合法已有 manifest 幂等。

## 一键真实验收脚本

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

token 只在一个终端里 `export`：**不要**贴进聊天、不要作为命令参数、不要写进任何文件或 Git。

### 第 3 步：把下面整段粘进终端（改掉仓库名）

```bash
cd /home/eurekaimer/Documents/GitHub/TOUKO

export GITHUB_TOKEN='这里粘贴你的 github_pat_... 字符串'
export TOUKO_ACCEPT_REPO='你的用户名/touko-gallery-test'

bash examples/github-acceptance.sh "$TOUKO_ACCEPT_REPO"

unset GITHUB_TOKEN TOUKO_ACCEPT_REPO
```

失败时脚本非零退出并保留 `/tmp/touko-github.XXXXXXXX` 现场；若报 `expected exit 0, got 1` 且带 `unauthorized`/`404`，通常是 token 权限、仓库名或 README 未勾；若报 `AssertionError`，那是 TOUKO 自身行为不符预期，请把整段输出交给维护者。

### 脚本断言的内容

脚本只从 `GITHUB_TOKEN` 读取凭据，不接受 token 参数、不打印 token、不写入凭据到磁盘（已用假 token 实测：失败时报 GitHub 的真实 401，输出中不含 token 字符串）；拒绝 `eurekaimer/touko`（源码仓库）作为 gallery。它依次断言：`auth status` 已认证、`repo use` 选中期望仓库与分支、`repo init` 幂等、批量 `add` 不移动远端 HEAD、暂存态 `url` 报 `committed:false`、`commit` 发布全部变更并清空暂存、两个 provider 的 URL 与 canonical object path 一致（private 仓库改为断言 `delivery_unavailable`）、`verify` 的 assets/objects 与 HEAD 一致、公开仓库 raw bytes 的 SHA-256 等于记录的 object hash、第二设备 `sync` 后完整 metadata 逐字段相等、最后一条引用的删除在第三台全新设备上不可见。真实远端不可达或断言失败时脚本非零退出并保留工作目录现场。

脚本的断言逻辑已在离线真实 Git 仓库上完整跑通（init 幂等、批量提交、verify、第二/第三设备恢复与删除）；GitHub 专属步骤（token 认证、raw bytes 校验）只有在你提供专用仓库与 token 后才会真正执行，此前不得宣称线上验收已完成。

## 第二设备与删除验收

记录第一个 `TOUKO_HOME` 为 A，另开全新目录 B，执行 `repo use OWNER/touko-demo; sync; ls --json`，比较完整 metadata。A `rm "$ID"; commit -m "demo: delete logo"` 后，用第三个全新 state-dir use/sync，确认 ID 消失并 verify。若该 placement 还有其他 Asset 引用，binary 应保留；最后引用删除才从最新 tree 移除。

删除前可 GET raw GitHub URL 并校验 bytes 的 SHA-256。jsDelivr 链接推导成功不代表 CDN 已传播；CDN 延迟不等于 remote commit 失败。完成后：

```bash
unset GITHUB_TOKEN
```

## URL 与 Markdown

- `url ID --provider github|jsdelivr` 根据当前 location 的 **branch** 派生 URL，不把 URL 存入 Asset。
- `url ID --json` 输出 `{"url":"...","committed":false}` 表示新 Asset 尚未发布。metadata-only modified Asset 的 binary 已提交，因此 committed 为 true。
- `cat logo/TOUKO.png | /tmp/touko add - --name '剪贴板.png' --namespace blog --output markdown` 输出图片 Markdown，并在 stderr 提示 staged/not published。必须 commit 后才可作为已发布链接使用。
- `--output id|markdown` 均不能和 `--json` 同用；冲突在 mutation 前拒绝。
- localgit 与 private GitHub repository 不提供公网 delivery；Markdown 请求也在 staging mutation 前拒绝。private repo 仍支持存储、恢复和 verify。

## 协议与限制

固定生产 endpoint 为 `https://api.github.com`，API header `2026-03-10`。blob GET 使用 raw bytes，POST 使用流式 base64，不把图片/base64 整体装入内存。每次只有一个 blob worker。取消/timeout 独立关闭 pipe/source 并等待 encoder 退出，防止 HTTP transport 等待 request-body writer 造成死锁。阻塞型自定义 Reader 必须支持取消或 io.Closer；CLI 的本地文件符合此要求。

Tree 写入包含 `base_tree`，删除显式 `sha:null`；recursive tree `truncated:true` 拒绝构建部分 cache。401/权限403 映射 unauthorized；限流403/429/5xx/timeout 不隐藏重试，保留 stage。journal 决定发布恢复行为，见 [publication](publication.md)。

**GitHub `force:false` 只保证 fast-forward，不提供 expected-old-SHA CAS。** TOUKO 固定唯一 parent 为 staging base，发布前复查 HEAD，正常 non-force 并发由 non-fast-forward 拒绝保护；无法保证外部管理员任意 force/reset 与 PATCH 之间的严格 CAS。Local Git 的 update-ref 则有 expected-old CAS。

本轮未提供专用远端仓库/token，因此真实 GitHub live smoke **未执行**。离线真实 Git/SQLite 与 httptest-backed Git Data API 故障测试不等于线上成功；不要将它们混淆。

协议依据：[refs](https://docs.github.com/en/rest/git/refs)、[trees](https://docs.github.com/en/rest/git/trees)、[blobs](https://docs.github.com/en/rest/git/blobs)、[compare](https://docs.github.com/en/rest/commits/commits#compare-two-commits)。
