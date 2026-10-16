# TOUKO Webview：启动、整理图片与真实 GitHub 测试

Webview 是 CLI 同一个核心的本地浏览器前端，不是独立云服务。图片操作仍遵守 `暂存 → 提交 → 同步`；点击上传不会立即写入 GitHub。标签对应 Asset.Tags，分类对应平面的 Asset.Collections，不另建分类数据库。

## 1. 构建与启动

在项目根目录运行：

```bash
go build -o /tmp/touko-web ./cmd/touko
/tmp/touko-web --state-dir "$HOME/.local/share/touko-web-demo" serve --port 7788
```

在浏览器打开终端打印的 `http://127.0.0.1:7788`。端口占用时换 `--port 7789`；`--port 0` 自动选择可用端口，以打印结果为准。使用打印的127.0.0.1地址，不改成localhost/局域网IP。Ctrl+C 停止服务；state-dir 保留，下次相同命令继续。

不需要 npm、Node、外部字体服务或数据库进程。HTML/CSS/JS、字体和 favicon 都嵌入 Go binary。拉丁字母与数字使用自托管的 Monaspace Radon，中文使用 LXGW 文楷，左上角字标使用子集化的圆几何字体。

### 先用离线 demo 体验

```bash
bash examples/demo.sh
# 将 ROOT 换成脚本实际输出的目录。
ROOT=/tmp/脚本实际输出的目录
/tmp/touko-web --state-dir "$ROOT/device-a" serve --port 7788
```

该 state 已有可用 local Git gallery；可直接在网页查看图片、继续导入 `logo/TOUKO.png`。本地 backend 不生成公网 URL，这是正常限制。

也可使用空 state 在界面连接本地仓库：新建 bare gallery 必须指定不存在或完全为空的目录；选择已有仓库不会覆盖它。一个 state-dir 只绑定一个仓库，换 gallery 请停止服务并用另一个 state-dir 启动，不强行复用旧 stage。

## 2. 真实 GitHub 测试前的准备

1. 在 GitHub 自行新建**专用测试仓库**，例如 `你的用户名/touko-webview-test`。不要使用 TOUKO 源代码仓库，也不要选含重要资料的仓库。
2. **勾选 Add a README file**。Git Data API 无法在完全空仓库创建第一个 ref。默认分支通常为 main，界面会读取实际 default branch。
3. 需要测试公开 URL 时选 public；private 仍能存储、网页预览、编辑、同步和 verify，但不提供 raw/jsDelivr 公网链接。
4. GitHub Settings → Developer settings → Personal access tokens → Fine-grained tokens：仅选择这个测试仓库，Repository permissions 中设置 **Contents: Read and write**。设置短有效期；组织仓库可能还需组织批准。
5. 分支禁止 force push。若分支保护要求 Pull Request 或签名而拒绝直接发布，使用你为测试允许直接提交的专用分支；TOUKO 不绕过保护。

Token 只能在执行机终端输入，**不要贴到聊天、网页输入框、URL、命令参数或 Git 仓库**。当前网页没有 token 表单或 OAuth login。

## 3. 推荐的真实仓库启动方式

在 Bash 中执行；把示例仓库名换成你的真实专用 repo：

```bash
go build -o /tmp/touko-web ./cmd/touko
read -rsp 'GitHub token: ' GITHUB_TOKEN; printf '\n'
export GITHUB_TOKEN
export TOUKO_GALLERY_STATE="$(mktemp -d -t touko-github.XXXXXXXX)"
printf '请保存这个 state 路径：%s\n' "$TOUKO_GALLERY_STATE"

/tmp/touko-web auth status
/tmp/touko-web --state-dir "$TOUKO_GALLERY_STATE" repo use YOUR_USER/touko-webview-test
/tmp/touko-web --state-dir "$TOUKO_GALLERY_STATE" repo init
/tmp/touko-web --state-dir "$TOUKO_GALLERY_STATE" serve --port 7788
```

`auth status` 应显示你的 GitHub 登录名，不显示 token。`repo init` 在现有 README commit 上增加 TOUKO manifest，不重写 README；重复执行合法 init 不创建额外提交。

如果希望全程在界面连接：只设置上述环境变量并启动空 state 的 `serve`，然后在连接仓库对话框选择 GitHub、输入 `YOUR_USER/touko-webview-test`，branch 留空使用默认分支。连接后按界面提示显式初始化格式或同步。浏览器不会得到 `GITHUB_TOKEN`；所有 GitHub API 请求由 Go 服务发出。

如果你改变终端中的 token，必须停止并重新启动 serve，运行中的进程不会自动读取另一个终端的新环境变量。

## 4. 按顺序验证每一个界面能力

### A. 上传与预览

1. 使用上传按钮或拖入 `logo/TOUKO.png`、`examples/images/red.png` 和 `blue.png`。
2. Namespace 设为 `blog`，标签填 `demo, 灯子`，分类填 `测试图库`；可设置标题。
3. 确认导入。应看到真实图片预览、尺寸/体积及“待发布”的新增状态。
4. 打开详情，检查原文件名、UUID、hash、尺寸与 MIME。
5. 此时去 GitHub 看分支 HEAD，**不应因为上传而产生 commit**。

每张最高100,000,000 bytes，单个网页请求最多500,000,000 bytes、100个文件；不支持的图片或失败输入不应留下半个批次。网页预览读取原图，不是假缩略图服务。

### B. 标签、分类和搜索

1. 在图片详情编辑标题、标签和分类并保存。
2. 标签/分类是完整列表；删除一个条目后保存表示从该图片移除，而非删除其他图片的标签。
3. 用左侧分类、标签和搜索缩小范围；清除筛选后完整图库应恢复。
4. 刷新浏览器，staging 修改应仍在，不能只保存在浏览器内存。
5. 多次编辑同一个新增 Asset 仍是一条 add，不应变成多个重复记录。

### C. 发布与真实远端检查

1. 打开待发布列表，检查新增/修改/删除内容。
2. 输入有意义的 commit message，例如 `webview: add Touko sample images`，确认提交。
3. 成功后待发布列表清空，卡片变成已提交，HEAD 更新。
4. 在 GitHub 文件页确认存在 `.touko/repository.json`、`.touko/assets/<uuid>.json`、`objects/blog/sha256/...`；README 仍在。
5. 执行界面的完整校验；其 asset/object 数量应与真实引用一致。
6. public repo 可复制 GitHub 或 jsDelivr 链接。先打开 raw GitHub 链接检查图像；jsDelivr 可能尚未传播，不能因此重复提交。

GitHub 写入失败必须显示实际错误并保留 staging，不应该显示“成功”。发生发布结果未知时先同步恢复，不反复点提交制造第二个候选。

### D. 同图去重与删除

1. 同一 namespace 再次上传 logo，提交。两个 UUID 不同，但 canonical object path 相同。
2. 删除第一条并提交：只少一个 metadata 文件，另一张图片仍可预览，binary 应保留。
3. 删除最后引用并提交：最新 GitHub tree 中对应 metadata 与 binary 都消失。
4. Git history 中仍能看到旧图片是正常的；TOUKO 不做历史 purge 或 CDN 缓存清除。
5. 删除只暂存时可在待发布列表检查；“丢弃全部暂存”需要明确确认，不能当作已经发布删除的撤销操作。

## 5. 用第二台设备证明不是本机假成功

保持相同 token 环境（private 仓库必需），另建全新 state-dir：

```bash
FRESH_STATE="$(mktemp -d -t touko-fresh.XXXXXXXX)"
/tmp/touko-web --state-dir "$FRESH_STATE" repo use YOUR_USER/touko-webview-test
/tmp/touko-web --state-dir "$FRESH_STATE" sync
/tmp/touko-web --state-dir "$FRESH_STATE" ls --json
/tmp/touko-web --state-dir "$FRESH_STATE" verify
/tmp/touko-web --state-dir "$FRESH_STATE" serve --port 7789
```

可以在另一个终端执行，需在该终端自行设置 `GITHUB_TOKEN`；不要把 token 写进 shell history。比较完整 metadata：ID、namespace、title、original_name、tags、collections、hash、size、MIME、尺寸、时间都应一致。第三个全新 state 在删除后同步，不应看到被删除 UUID。

并发冲突练习：A/B 同一 HEAD 时，B 编辑某图但不提交；A 删除并提交；B 提交必须 conflict/delete_modify，不能复活该图。B 决定放弃所有未发布修改后再显式 reset/sync。不要为了让测试通过关闭冲突检查。

测试结束时 Ctrl+C 停止 serve，再执行：

```bash
unset GITHUB_TOKEN
```

仓库和 state 保留供检查；确认无用后由你自行决定删除。不要在还有 stage/publication journal 时删除 index.db。

## 6. 安全与已知边界

- 只监听127.0.0.1，校验精确Host/Origin和会话CSRF；不提供局域网共享或公网部署开关。不要用反向代理绕过边界后当作多人云服务。
- API/图片不允许跨域，页面有 CSP、nosniff、no-referrer、frame保护；详情只按 UUID 查图，没有任意文件路径下载接口。
- token只在 Go server 内部；private图片通过服务端GitHub adapter流式代理，不把签名token/raw授权链接发给浏览器。
- GitHub `force:false` 是 fast-forward保护，不是针对任意外部管理员force/reset的严格expected-old-SHA CAS。禁止force-push是测试前提之一。
- GitHub header版本固定2026-03-10；错误、限流、branch protection和truncated tree不会被伪装成部分成功。
- 当前还没有由用户授权的专用repo/token，因此不能宣称已完成你的线上验收。项目的HTTP故障测试与本地浏览器验收不能替代本节真实远端检查。

## 7. 视觉资源与设计

色板来自用户指定方向：奶油米色与深藏蓝呼应校服，珊瑚橙作界面强调色，暗樱红只用于破坏性操作与「已修改」，卡纸色调用于侧栏与图片区。设计方向为克制的编辑式排版——两个明确区分的字体族、细线只用于分隔层级、中文正文使用自托管字体并保留充足行距。修正过程参照 Anthropic 公开的 `frontend-design` skill（含其列出的「AI 生成页面」常见特征清单），避免把默认模板特征误当设计。

英数与拉丁文本使用自托管的 Monaspace Radon（oblique 走 slnt 轴），中文使用 LXGW 文楷（霞鹜文楷，楷体正文面），左上角 TOUKO 字标另有子集化的圆几何字体。三者均为 SIL OFL，许可证随字体内嵌；LXGW 的附加许可明确允许为 web 交付子集化为 WOFF2。原图 `logo/TOUKO.png` 保持不变，页面内只使用从它派生的 favicon（`favicon.png`），不再放角色图条带；不主张角色或原图版权，分发衍生图片前应自行确认授权。界面无需外部 CDN 即可显示字体、图标与 favicon。
