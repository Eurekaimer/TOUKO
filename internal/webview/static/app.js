const $ = (id) => document.getElementById(id);
const workspacePrefix = window.location.pathname.match(/^\/w\/[^/]+(?=\/|$)/)?.[0] || "";
const apiURL = (path) => `${workspacePrefix}/api/${path}`;
// Vocabulary: 待发布 = still only on this machine, 新增/修改/移除 = what happened to one picture.
const stateNames = {
  committed: "已提交",
  added: "待发布、新增",
  modified: "待发布、修改",
};
const changeNames = { add: "新增", update: "修改", delete: "移除" };
const filters = {
  query: "",
  namespace: "",
  collection: "",
  tag: "",
  directory: "",
  sort: "newest",
};
const selected = new Set();
const cards = new Map();
const focusOrigins = new WeakMap();
let workspace = null;
let busy = false;
let currentAsset = null;
let uploadFiles = [];
let confirmOperation = null;
let toastTimer;
let refreshSequence = 0;
let repositories = [];
let branches = [];
let repositoriesLoading = false;
let branchesLoading = false;
let repositoryDialogSequence = 0;
let githubRequestSequence = 0;
let branchRequestSequence = 0;

// Metadata is always text, never interpolated HTML. The only SVG references are
// our own fixed symbols in index.html; uploaded bytes cannot introduce markup.
function element(tag, className = "", text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
}
function icon(name) {
  const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  svg.setAttribute("class", "icon");
  svg.setAttribute("aria-hidden", "true");
  const use = document.createElementNS(svg.namespaceURI, "use");
  use.setAttribute("href", `#i-${name}`);
  svg.append(use);
  return svg;
}
function button(text, className, action) {
  const node = element("button", className, text);
  node.type = "button";
  node.addEventListener("click", action);
  return node;
}
function bytes(size) {
  if (size < 1000) return `${size} B`;
  if (size < 1_000_000) return `${(size / 1000).toFixed(1)} KB`;
  return `${(size / 1_000_000).toFixed(1)} MB`;
}
function date(value) {
  return new Intl.DateTimeFormat("zh-CN", {
    year: "numeric",
    month: "2-digit",
    day: "2-digit",
  }).format(new Date(value));
}
function list(value) {
  return [
    ...new Set(
      value
        .split(/[,，\n]/)
        .map((item) => item.trim())
        .filter(Boolean),
    ),
  ];
}
function showError(id, error) {
  const target = $(id);
  target.textContent = error instanceof Error ? error.message : String(error);
  target.hidden = false;
}
function clearError(id) {
  $(id).hidden = true;
  $(id).textContent = "";
}
function toast(message) {
  clearTimeout(toastTimer);
  $("toast").textContent = message;
  $("toast").hidden = false;
  toastTimer = setTimeout(() => {
    $("toast").hidden = true;
  }, 4500);
}
function responseError(data, status) {
  const code = data?.error?.code;
  let message = data?.error?.message || `请求失败（HTTP ${status}）`;
  if (code === "conflict")
    message +=
      "\n请查看待发布列表：保留修改，或明确丢弃后再同步。同步不会自动覆盖远端。";
  if (code === "busy") message += "\n当前工作区正在处理其他操作，请稍后刷新。";
  if (code === "unauthorized")
    message += "\n请在终端运行 touko auth login，或检查 GITHUB_TOKEN / GH_TOKEN 的仓库权限，再重新读取登录。";
  return new Error(message);
}
async function api(path, body) {
  const options = { headers: { Accept: "application/json" } };
  if (body !== undefined) {
    options.method = "POST";
    options.headers["Content-Type"] = "application/json";
    options.headers["X-Touko-CSRF"] = workspace.csrf;
    options.body = JSON.stringify(body);
  }
  const response = await fetch(apiURL(path), options);
  let data;
  try {
    data = await response.json();
  } catch {
    throw new Error(
      `本地服务返回了无效响应（HTTP ${response.status}）。请检查终端。`,
    );
  }
  if (!response.ok) throw responseError(data, response.status);
  return data;
}
function setBusy(value) {
  busy = value;
  document
    .querySelectorAll(
      "[data-mutation], dialog button, dialog input, dialog textarea, dialog select",
    )
    .forEach((node) => {
      node.disabled = value;
    });
  updateActions();
  updateRepositoryControls();
}
async function mutate(action, errorId, success) {
  if (busy) return null;
  clearError(errorId);
  setBusy(true);
  let result;
  try {
    result = await action();
  } catch (error) {
    showError(errorId, error);
    setBusy(false);
    return null;
  }
  try {
    await refreshState();
  } catch (error) {
    showError(
      "page-error",
      `操作已完成，但刷新失败。请刷新确认当前状态，不要重复提交。\n${error.message}`,
    );
  }
  setBusy(false);
  if (success) toast(success);
  return result;
}
async function refreshState() {
  const sequence = ++refreshSequence;
  const next = await api("state");
  if (sequence !== refreshSequence) return;
  workspace = next;
  const available = new Set(next.assets.map((view) => view.asset.id));
  for (const id of selected) if (!available.has(id)) selected.delete(id);
  cards.clear();
  clearError("page-error");
  renderWorkspace();
}
function showDialog(id, focus) {
  const dialog = $(id);
  if (dialog.open) return;
  focusOrigins.set(dialog, document.activeElement);
  dialog.showModal();
  if (focus) $(focus).focus();
}
function hideDialog(dialog) {
  if (!busy) dialog.close();
}
function closeNavigation() {
  $("sidebar").classList.remove("is-open");
  $("toggle-nav").setAttribute("aria-expanded", "false");
}
function resetFilters() {
  filters.query = filters.namespace = filters.collection = filters.tag = filters.directory = "";
  $("search").value = "";
  $("namespace-filter").value = "";
  $("directory-filter").value = "";
  renderWorkspace();
}
function ready() {
  if (!workspace) {
    toast("请等待工作空间读取完成。");
    return false;
  }
  if (!workspace.location) {
    openRepository();
    return false;
  }
  if (!workspace.initialized) {
    toast("请先同步已有图库，或显式初始化新的 TOUKO 仓库。");
    return false;
  }
  return true;
}
function updateActions() {
  if (!workspace) return;
  $("sync").disabled = busy || !workspace.location;
  $("verify").disabled = busy || !workspace.initialized;
  $("reindex").disabled = busy || !workspace.initialized;
  $("open-import").disabled = busy;
  $("import-submit").disabled = busy || !workspace.initialized;
  $("open-upload").disabled = busy;
  $("setup-action").disabled = busy;
  $("upload-submit").disabled =
    busy || !uploadFiles.length || !workspace.initialized;
  $("commit-submit").disabled =
    busy || !(workspace.changes.length || workspace.publication);
  $("reset-staged").disabled =
    busy || !(workspace.changes.length || workspace.publication);
  $("remove-selected").disabled = busy || !selected.size;
}
function renderWorkspace() {
  const { assets, changes, location, head } = workspace;
  $("nav-total").textContent = assets.length;
  $("nav-pending").textContent = changes.length;
  $("repo-name").textContent = !location
    ? "尚未连接"
    : location.backend === "github"
      ? `${location.owner}/${location.name}`
      : location.path.split("/").filter(Boolean).at(-1);
  $("repo-detail").textContent = !location
    ? "连接仓库后，图片就归档在这里。"
    : `${location.backend === "github" ? (location.private ? "GitHub 私有仓库" : "GitHub 公开仓库") : "本地 Git 仓库"}，分支 ${location.branch}`;
  $("repo-detail").title = location?.path || "";
  $("repo-dot").classList.toggle("connected", !!location);
  $("head-indicator").textContent = head
    ? `HEAD ${head.slice(0, 8)}，${changes.length ? "有修改待发布" : "没有待发布修改"}`
    : "尚未连接仓库";
  $("head-indicator").title = head;
  $("auth-status").textContent = workspace.auth.configured
    ? "服务端已有 GitHub 凭据，可读取登录与仓库列表"
    : "尚未读取 GitHub 登录；可使用 gh 登录或环境变量凭据";
  const localRead = workspace.local_read;
  $("local-read-status").textContent = localRead?.available
    ? `本地读图副本可用：${localRead.path}`
    : "本地读图副本尚不可用。图片是否可读取决于仓库后端；同步后可再次检查。";
  $("local-read-status").title = localRead?.path || "";
  const namespaces = [
    ...new Set(assets.map(({ asset }) => asset.namespace)),
  ].sort();
  $("namespace-filter").replaceChildren(
    new Option("全部命名空间", ""),
    ...namespaces.map((name) => new Option(name, name)),
  );
  if (!namespaces.includes(filters.namespace)) filters.namespace = "";
  $("namespace-filter").value = filters.namespace;
  const directories = new Set();
  for (const { asset } of assets) {
    const parts = assetPath(asset).split("/");
    parts.pop();
    if (!parts.length) directories.add(".");
    for (let i = 1; i <= parts.length; i++)
      directories.add(parts.slice(0, i).join("/"));
  }
  const orderedDirectories = [...directories].sort((a, b) => a.localeCompare(b, "zh-CN"));
  $("directory-filter").replaceChildren(
    new Option("全部路径目录", ""),
    ...orderedDirectories.map((path) => new Option(path === "." ? "仓库根目录" : `${path}/`, path)),
  );
  if (!directories.has(filters.directory)) filters.directory = "";
  $("directory-filter").value = filters.directory;
  $("setup-banner").hidden = !!location && workspace.initialized;
  if (!location) {
    $("setup-title").textContent = "还没有连接仓库";
    $("setup-description").textContent =
      "连接本机已有的 Git 仓库，或连接一个专门存图片的 GitHub 仓库。图片与元数据始终由你掌握。";
    $("setup-action").textContent = "连接仓库";
  } else if (!workspace.initialized) {
    $("setup-title").textContent = "仓库已连接，还没有初始化";
    $("setup-description").textContent =
      "已有 TOUKO 图库请先「同步」；PicGo 等旧图库请先显式初始化，再使用「导入旧图库 / PicGo」暂存原路径图片，提交后才写入管理元数据。初始化不会自动导入或搬动图片。GitHub 空仓库须先创建 README。";
    $("setup-action").textContent = "初始化 TOUKO";
  }
  renderFilters();
  renderGallery();
  renderPending();
  updateActions();
}
function counts(field) {
  const result = new Map();
  for (const { asset } of workspace.assets)
    for (const name of asset[field])
      result.set(name, (result.get(name) || 0) + 1);
  return [...result].sort(([a], [b]) => a.localeCompare(b, "zh-CN"));
}
function renderFilters() {
  const collections = counts("collections");
  const tags = counts("tags");
  $("collection-filters").replaceChildren();
  for (const [name, count] of collections) {
    const item = button(
      "",
      `collection-button${filters.collection === name ? " active" : ""}`,
      () => {
        filters.collection = filters.collection === name ? "" : name;
        closeNavigation();
        renderFilters();
        renderGallery();
      },
    );
    item.setAttribute("aria-pressed", String(filters.collection === name));
    const leader = element("span", "ledger-leader");
    leader.setAttribute("aria-hidden", "true");
    item.append(
      icon("folder"),
      element("span", "", name),
      leader,
      element("span", "count", count),
    );
    $("collection-filters").append(item);
  }
  if (!collections.length)
    $("collection-filters").append(
      element("p", "sidebar-hint", "归档图片后，分类会列在这里。"),
    );
  $("tag-filters").replaceChildren();
  for (const [name, count] of tags) {
    const item = button(
      name,
      `tag-filter${filters.tag === name ? " active" : ""}`,
      () => {
        filters.tag = filters.tag === name ? "" : name;
        closeNavigation();
        renderFilters();
        renderGallery();
      },
    );
    item.setAttribute("aria-pressed", String(filters.tag === name));
    item.append(element("span", "", count));
    $("tag-filters").append(item);
  }
  if (!tags.length)
    $("tag-filters").append(
      element("p", "sidebar-hint", "归档图片后，标签会列在这里。"),
    );
}
function assetPath(asset) {
  if (asset.path) return asset.path;
  const hash = asset.object.hash;
  return `objects/${asset.namespace}/sha256/${hash.slice(0, 2)}/${hash.slice(2, 4)}/${hash}.${asset.object.ext}`;
}
function visibleAssets() {
  if (!workspace) return [];
  const query = filters.query.trim().toLocaleLowerCase();
  return workspace.assets
    .filter(
      ({ asset }) =>
        (!filters.namespace || asset.namespace === filters.namespace) &&
        (!filters.directory ||
          (filters.directory === "."
            ? !assetPath(asset).includes("/")
            : assetPath(asset).startsWith(`${filters.directory}/`))) &&
        (!filters.collection ||
          asset.collections.includes(filters.collection)) &&
        (!filters.tag || asset.tags.includes(filters.tag)) &&
        (!query ||
          [
            asset.title,
            asset.original_name,
            assetPath(asset),
            ...asset.tags,
            ...asset.collections,
          ].some((text) => text.toLocaleLowerCase().includes(query))),
    )
    .sort((a, b) => {
      let order;
      if (filters.sort === "title")
        order = a.asset.title.localeCompare(b.asset.title, "zh-CN");
      else if (filters.sort === "oldest")
        order = a.asset.created_at.localeCompare(b.asset.created_at);
      else if (filters.sort === "updated")
        order = b.asset.updated_at.localeCompare(a.asset.updated_at);
      else order = b.asset.created_at.localeCompare(a.asset.created_at);
      return order || a.asset.id.localeCompare(b.asset.id);
    });
}
function imageURL(asset) {
  return apiURL(`assets/${asset.id}/image?content=${asset.object.hash}&head=${workspace.head}`);
}
function card(view) {
  const { asset } = view;
  const node = element("article", "asset-card");
  node.dataset.id = asset.id;
  const media = element("div", "card-image-wrap");
  const open = button("", "card-open", () => openPreview(asset.id));
  open.setAttribute("aria-label", `查看 ${asset.title || asset.original_name}`);
  const img = element("img", "card-image");
  img.alt = asset.title || asset.original_name;
  img.loading = "lazy";
  img.decoding = "async";
  img.src = imageURL(asset);
  img.addEventListener(
    "error",
    () => {
      const error = element("span", "card-image-error");
      error.append(
        icon("image"),
        element("span", "", "原图读取失败，点开可以查看详情"),
      );
      open.replaceChildren(error);
    },
    { once: true },
  );
  open.append(img);
  const select = element("label", "card-select");
  const checkbox = element("input");
  checkbox.type = "checkbox";
  checkbox.setAttribute(
    "aria-label",
    `选择 ${asset.title || asset.original_name}`,
  );
  checkbox.addEventListener("change", () => {
    if (checkbox.checked) selected.add(asset.id);
    else selected.delete(asset.id);
    renderGallery();
  });
  select.append(checkbox);
  // Only the selection control floats over the picture; the state slip is
  // pinned to the mount itself, so it reads as a kraft label on the mat rather
  // than as one more caption detail.
  media.append(open, select);
  const content = element("div", "card-content");
  const title = button(asset.title || asset.original_name, "card-title", () =>
    openPreview(asset.id),
  );
  title.title = asset.title;
  const heading = element("h3", "card-heading");
  heading.append(title);
  // Caption row: where the picture is filed, and whether it is staged. Tags stay
  // in the rail, which already lists every one with a count. The namespace is
  // only shown when the workspace holds more than one.
  const badges = element("div", "card-tags");
  badges.append(element("span", "namespace-chip", asset.namespace));
  asset.collections
    .slice(0, 2)
    .forEach((name) =>
      badges.append(element("span", "card-tag collection", name)),
    );
  if (asset.collections.length > 2)
    badges.append(
      element(
        "span",
        "card-tag more",
        `还有 ${asset.collections.length - 2} 个分类`,
      ),
    );
  if (!asset.collections.length)
    badges.append(element("span", "card-tag none", "未分类"));
  // A staged picture gets a kraft slip pinned to the top of its mount: the mat
  // is already kraft paper, and the slip is what says in words which state it
  // is in. Colour alone never carries this.
  let stateSlip;
  if (view.state !== "committed") {
    stateSlip = element(
      "span",
      `state-chip ${view.state}`,
      view.state === "added" ? "待发布" : "有修改",
    );
  }
  const footer = element("div", "card-footer");
  const dimensions = element("span");
  dimensions.append(
    icon("image"),
    document.createTextNode(`${asset.object.width} × ${asset.object.height}`),
  );
  const added = element("time", "", date(asset.created_at));
  added.dateTime = asset.created_at;
  footer.append(
    dimensions,
    element("span", "", bytes(asset.object.size)),
    added,
  );
  content.append(heading, badges, footer);
  node.append(media, content);
  if (stateSlip) node.append(stateSlip);
  return node;
}
function renderTrail() {
  const trail = $("filter-trail");
  trail.replaceChildren();
  const parts = [];
  if (filters.query) parts.push({ label: `搜索“${filters.query}”`, clear: () => { filters.query = ""; $("search").value = ""; } });
  if (filters.namespace)
    parts.push({
      label: `命名空间 ${filters.namespace}`,
      clear: () => { filters.namespace = ""; $("namespace-filter").value = ""; },
    });
  if (filters.directory)
    parts.push({
      label: `路径 ${filters.directory === "." ? "仓库根目录" : filters.directory + "/"}`,
      clear: () => { filters.directory = ""; $("directory-filter").value = ""; },
    });
  if (filters.collection)
    parts.push({ label: `分类 ${filters.collection}`, clear: () => { filters.collection = ""; } });
  if (filters.tag)
    parts.push({ label: `标签 ${filters.tag}`, clear: () => { filters.tag = ""; } });
  trail.hidden = !parts.length;
  if (!parts.length) return;
  trail.append(element("span", "trail-label", "当前筛选"));
  parts.forEach((part, index) => {
    if (index) trail.append(element("span", "trail-sep", "›"));
    const chip = button("", "trail-chip", () => {
      part.clear();
      renderFilters();
      renderGallery();
    });
    chip.append(element("span", "", part.label), icon("close"));
    chip.setAttribute("aria-label", `清除筛选：${part.label}`);
    trail.append(chip);
  });
}
function renderGallery() {
  if (!workspace) return;
  const views = visibleAssets();
  const active = !!(
    filters.query ||
    filters.namespace ||
    filters.collection ||
    filters.tag ||
    filters.directory
  );
  $("clear-filters").hidden = !active;
  $("all-assets").classList.toggle("active", !active);
  $("result-count").textContent =
    `${views.length} / ${workspace.assets.length} 张`;
  renderTrail();
  $("gallery").setAttribute("aria-busy", "false");
  $("gallery").classList.toggle("has-selection", selected.size > 0);
  $("selection-bar").hidden = selected.size === 0;
  $("selected-count").textContent = `已选 ${selected.size} 张`;
  const checked = views.filter(({ asset }) => selected.has(asset.id)).length;
  $("select-visible").checked = !!views.length && checked === views.length;
  $("select-visible").indeterminate = checked > 0 && checked < views.length;
  if (!views.length) {
    const empty = element("div", "empty-state");
    empty.append(
      icon("image"),
      element("h3", "", active ? "没有匹配的图片" : "这里还没有图片"),
      element(
        "p",
        "",
        active
          ? "换个关键词试试，或者清除筛选，看全部图片。"
          : "把 PNG、JPEG 或 GIF 拖进来，再用标签和分类归档。",
      ),
    );
    empty.append(
      button(
        active ? "清除筛选" : workspace.location ? "上传图片" : "连接仓库",
        "button primary",
        active ? resetFilters : () => openUpload(),
      ),
    );
    $("gallery").replaceChildren(empty);
  } else {
    const nodes = views.map((view) => {
      let node = cards.get(view.asset.id);
      if (!node) {
        node = card(view);
        cards.set(view.asset.id, node);
      }
      node.classList.toggle("is-selected", selected.has(view.asset.id));
      node.classList.toggle("is-staged", view.state !== "committed");
      node.querySelector("input[type=checkbox]").checked = selected.has(
        view.asset.id,
      );
      return node;
    });
    $("gallery").replaceChildren(...nodes);
  }
  $("gallery-footer-note").textContent = active
    ? `筛选出 ${views.length} 张图片。`
    : workspace.changes.length
      ? `有 ${workspace.changes.length} 项修改只在待发布区，提交后才会写入仓库。`
      : "没有待发布的修改。";
  updateActions();
}
function openPreview(id) {
  const view = workspace.assets.find(({ asset }) => asset.id === id);
  if (!view) return;
  currentAsset = id;
  const { asset } = view;
  $("preview-image-error").hidden = true;
  $("preview-image").hidden = false;
  $("preview-image").src = imageURL(asset);
  $("preview-image").alt = asset.title || asset.original_name;
  $("asset-path").textContent = assetPath(asset);
  $("edit-title").value = asset.title;
  $("edit-tags").value = asset.tags.join(", ");
  $("edit-collections").value = asset.collections.join(", ");
  $("preview-state").className = `asset-state ${view.state}`;
  $("preview-state").textContent = stateNames[view.state];
  $("asset-metadata").replaceChildren();
  for (const [label, value] of [
    ["文件名", asset.original_name],
    ["命名空间", asset.namespace],
    ["格式", asset.object.mime],
    ["尺寸", `${asset.object.width} × ${asset.object.height}`],
    ["大小", bytes(asset.object.size)],
    ["Asset ID", asset.id],
    ["内容哈希", asset.object.hash],
    ["添加时间", new Date(asset.created_at).toLocaleString("zh-CN")],
    ["修改时间", new Date(asset.updated_at).toLocaleString("zh-CN")],
  ]) {
    $("asset-metadata").append(
      element("dt", "", label),
      element("dd", "", value),
    );
  }
  const publicGitHub =
    workspace.location?.backend === "github" && !workspace.location.private;
  $("delivery-controls").hidden = !publicGitHub;
  $("delivery-unavailable").hidden = publicGitHub;
  $("delivery-url").hidden = true;
  $("delivery-url").value = "";
  $("delivery-note").textContent =
    view.state === "added"
      ? "还没发布：这个地址要等提交之后才能访问。"
      : "链接跟随分支；jsDelivr 的缓存传播可能有延迟。";
  clearError("edit-error");
  showDialog("preview-dialog");
}
function renderPending() {
  $("pending-count").textContent = workspace.changes.length;
  $("pending-base").textContent = workspace.base_head
    ? `基于 HEAD ${workspace.base_head}`
    : "还没有提交过";
  $("publication-warning").hidden = !workspace.publication;
  if (workspace.publication)
    $("publication-warning").textContent =
      `上一次发布还没确认结果：${workspace.publication.candidate_head}\n请先同步确认远端状态。不要删除本地数据库，也不要重复上传。`;
  $("pending-list").replaceChildren();
  for (const change of workspace.changes) {
    const row = element("div", "pending-row");
    const detail = element("div");
    detail.append(
      element(
        "strong",
        "",
        change.asset?.title ||
          change.asset?.original_name ||
          `移除图片 ${change.asset_id.slice(0, 8)}`,
      ),
      element("small", "", change.asset_id),
    );
    row.append(
      element("span", `change-label ${change.kind}`, changeNames[change.kind]),
      detail,
    );
    $("pending-list").append(row);
  }
  if (!workspace.changes.length)
    $("pending-list").append(
      element(
        "p",
        "pending-empty",
        workspace.publication
          ? "发布日志还没恢复，请先同步。"
          : "没有待发布的修改。",
      ),
    );
}
function openPending() {
  if (!workspace) return;
  renderPending();
  clearError("pending-error");
  closeNavigation();
  showDialog("pending-dialog", "commit-message");
}
function confirm(title, description, action) {
  $("confirm-heading").textContent = title;
  $("confirm-description").textContent = description;
  clearError("confirm-error");
  confirmOperation = action;
  showDialog("confirm-dialog", "confirm-action");
}
function removeAssets(ids) {
  if (!ids.length) return;
  confirm(
    "移除所选图片？",
    `将 ${ids.length} 张图片移入待发布的移除列表；提交后它们才会从仓库的最新版本里消失。\n本机的原始文件不会被删除，Git 历史也不会被清除。`,
    async () => {
      const result = await mutate(
        () => api("remove", { selectors: ids }),
        "confirm-error",
        "已加入待发布的移除列表",
      );
      if (result) {
        $("confirm-dialog").close();
        if (ids.includes(currentAsset)) $("preview-dialog").close();
      }
    },
  );
}
function releaseUploadFiles() {
  uploadFiles.forEach(({ url }) => URL.revokeObjectURL(url));
  uploadFiles = [];
  $("upload-files").value = "";
  renderUploadFiles();
}
function setUploadFiles(files, append = false) {
  if (busy) return;
  clearError("upload-error");
  const combined = [
    ...(append ? uploadFiles.map(({ file }) => file) : []),
    ...files,
  ];
  if (
    combined.length > 100 ||
    combined.some((file) => file.size > 100_000_000) ||
    combined.reduce((total, file) => total + file.size, 0) > 500_000_000
  ) {
    showError(
      "upload-error",
      "一次最多上传 100 张、合计 500 MB，单张不超过 100 MB。请去掉一些文件再试。",
    );
    return;
  }
  uploadFiles.forEach(({ url }) => URL.revokeObjectURL(url));
  uploadFiles = combined.map((file) => ({
    file,
    url: URL.createObjectURL(file),
  }));
  renderUploadFiles();
}
function renderUploadFiles() {
  $("upload-list").replaceChildren();
  uploadFiles.forEach(({ file, url }, index) => {
    const row = element("div", "upload-row");
    const image = element("img");
    image.src = url;
    image.alt = "";
    image.addEventListener(
      "error",
      () => {
        image.hidden = true;
      },
      { once: true },
    );
    const info = element("div");
    info.append(
      element("strong", "", file.name),
      element("small", "", bytes(file.size)),
    );
    const remove = button("", "icon-button", () =>
      setUploadFiles(
        uploadFiles.filter((_, i) => i !== index).map(({ file }) => file),
      ),
    );
    remove.append(icon("close"));
    remove.setAttribute("aria-label", `移除上传文件 ${file.name}`);
    row.append(image, info, remove);
    $("upload-list").append(row);
  });
  $("upload-summary").textContent = uploadFiles.length
    ? `${uploadFiles.length} 张图片，共 ${bytes(uploadFiles.reduce((total, { file }) => total + file.size, 0))}`
    : "尚未选择图片";
  updateActions();
}
function openUpload(files) {
  if (busy || !ready()) return;
  clearError("upload-error");
  $("upload-progress-area").hidden = true;
  closeNavigation();
  showDialog("upload-dialog", "upload-namespace");
  if (files)
    setUploadFiles(files, $("upload-dialog").open && uploadFiles.length > 0);
}
function upload() {
  const data = new FormData();
  uploadFiles.forEach(({ file }) => data.append("files", file, file.name));
  data.append("namespace", $("upload-namespace").value.trim());
  data.append("directory", $("upload-directory").value.trim());
  if ($("upload-title").value.trim())
    data.append("title", $("upload-title").value);
  data.append("tags", JSON.stringify(list($("upload-tags").value)));
  data.append(
    "collections",
    JSON.stringify(list($("upload-collections").value)),
  );
  $("upload-progress-area").hidden = false;
  $("upload-progress").value = 0;
  return new Promise((resolve, reject) => {
    const request = new XMLHttpRequest();
    request.open("POST", apiURL("upload"));
    request.setRequestHeader("X-Touko-CSRF", workspace.csrf);
    request.upload.onprogress = ({ lengthComputable, loaded, total }) => {
      if (lengthComputable) {
        $("upload-progress").value = Math.round((loaded / total) * 100);
        $("upload-progress-text").textContent =
          loaded === total
            ? "正在校验并保存到本机…"
            : `上传至本机 ${Math.round((loaded / total) * 100)}%`;
      }
    };
    request.onload = () => {
      let data;
      try {
        data = JSON.parse(request.responseText);
      } catch {
        reject(new Error("本地服务响应无效，请检查终端后刷新确认。"));
        return;
      }
      if (request.status >= 200 && request.status < 300) resolve(data);
      else reject(responseError(data, request.status));
    };
    request.onerror = () =>
      reject(new Error("连接中断，请刷新确认暂存状态后再继续。"));
    request.onabort = () =>
      reject(new Error("上传已取消，请刷新确认当前状态。"));
    request.send(data);
  });
}
function invalidateGitHubRequests() {
  githubRequestSequence++;
  branchRequestSequence++;
  repositoriesLoading = false;
  branchesLoading = false;
}
function validGitHubSelection() {
  return !repositoriesLoading && !branchesLoading &&
    repositories.some((repo) => repo.full_name === $("repo-choices").value) &&
    branches.some((branch) => branch.name === $("repo-branches").value);
}
function updateRepositoryControls() {
  const github = $("repo-type").value === "github";
  $("repo-path").disabled = busy || github;
  $("repo-branch").disabled = busy || github;
  $("repo-choices").disabled = busy || !github || repositoriesLoading || !repositories.length;
  $("repo-branches").disabled = busy || !github || repositoriesLoading || branchesLoading || !branches.length;
  $("check-auth").disabled = busy || repositoriesLoading;
  $("repo-connect").disabled = busy || (github && !validGitHubSelection());
  $("github-fields").setAttribute("aria-busy", String(repositoriesLoading || branchesLoading));
}
function resetBranchChoices(message) {
  branchRequestSequence++;
  branches = [];
  branchesLoading = false;
  $("repo-branches").replaceChildren(new Option(message, ""));
  $("repo-branch-help").textContent = message;
}
async function loadRepositoryBranches(preferredBranch = "") {
  resetBranchChoices("正在读取全部分支…");
  const repository = $("repo-choices").value;
  const repo = repositories.find((item) => item.full_name === repository);
  if (!repo) {
    resetBranchChoices("请先选择仓库");
    updateRepositoryControls();
    return;
  }
  const request = branchRequestSequence;
  const githubRequest = githubRequestSequence;
  const current = () => request === branchRequestSequence &&
    githubRequest === githubRequestSequence && $("repo-dialog").open &&
    $("repo-type").value === "github" && $("repo-choices").value === repository;
  branchesLoading = true;
  clearError("repo-error");
  updateRepositoryControls();
  try {
    const value = await api(`branches?repository=${encodeURIComponent(repository)}`);
    if (!current()) return;
    branches = value.branches;
    if (!branches.length) {
      $("repo-branches").replaceChildren(new Option("此仓库没有可用分支", ""));
      $("repo-branch-help").textContent = "此仓库尚无分支。请先在 GitHub 添加 README 或提交内容，再重新读取。";
      return;
    }
    $("repo-branches").replaceChildren(...branches.map((branch) => new Option(
      `${branch.name}${branch.name === repo.default_branch ? " · 默认" : ""}${branch.protected ? " · 受保护" : ""}`,
      branch.name,
    )));
    const location = workspace?.location;
    const connectedBranch = location?.backend === "github" &&
      `${location.owner}/${location.name}` === repository ? location.branch : "";
    $("repo-branches").value = [preferredBranch, connectedBranch, repo.default_branch, branches[0].name]
      .find((name) => branches.some((branch) => branch.name === name));
    $("repo-branch-help").textContent = `已读取全部 ${branches.length} 个分支。受保护分支仍可选择，发布须遵守仓库规则。${repo.can_push ? "" : " 当前仓库只读，连接后不能发布。"}`;
  } catch (error) {
    if (!current()) return;
    branches = [];
    $("repo-branches").replaceChildren(new Option("分支读取失败", ""));
    $("repo-branch-help").textContent = "分支读取失败，请检查登录与仓库权限后重新读取，或选择其他仓库。";
    showError("repo-error", error);
  } finally {
    if (current()) {
      branchesLoading = false;
      updateRepositoryControls();
    }
  }
}
async function loadGitHubRepositories() {
  if (busy || !$("repo-dialog").open || $("repo-type").value !== "github") return;
  const preferredRepository = $("repo-choices").value;
  const preferredBranch = $("repo-branches").value;
  invalidateGitHubRequests();
  const request = githubRequestSequence;
  const current = () => request === githubRequestSequence &&
    $("repo-dialog").open && $("repo-type").value === "github";
  repositories = [];
  repositoriesLoading = true;
  $("repo-choices").replaceChildren(new Option("正在读取全部仓库…", ""));
  $("repo-choice-help").textContent = "正在检查 GitHub 登录并读取全部可访问仓库…";
  $("auth-status").textContent = "正在读取 GitHub 登录…";
  resetBranchChoices("请等待仓库列表加载");
  clearError("repo-error");
  updateRepositoryControls();
  try {
    const auth = await api("auth");
    if (!current()) return;
    $("auth-status").textContent = auth.configured
      ? `GitHub 身份：${auth.login}（来源：${auth.source}）`
      : "未找到登录。请在终端运行 touko auth login（需安装 GitHub CLI），或配置 GITHUB_TOKEN / GH_TOKEN，然后重新读取；无需在网页输入 token。";
    if (!auth.configured) {
      $("repo-choices").replaceChildren(new Option("请先登录 GitHub", ""));
      $("repo-choice-help").textContent = "尚未登录，无法读取可访问仓库。完成终端登录后点击「重新读取」。";
      resetBranchChoices("请先登录并选择仓库");
      return;
    }
    $("auth-status").textContent += " · 正在加载仓库…";
    const value = await api("repos");
    if (!current()) return;
    repositories = value.repositories;
    $("auth-status").textContent = `GitHub 身份：${auth.login}（来源：${auth.source}）`;
    $("repo-choices").replaceChildren(
      new Option(repositories.length ? `请选择仓库（共 ${repositories.length} 个）` : "没有可访问仓库", ""),
      ...repositories.map((repo) => new Option(
        `${repo.full_name} · ${repo.private ? "私有" : "公开"} · ${repo.can_push ? "可写" : "只读"}`,
        repo.full_name,
      )),
    );
    $("repo-choice-help").textContent = repositories.length
      ? `已读取全部 ${repositories.length} 个仓库，包含公开 / 私有与写入权限。只读仓库可以浏览，但不能发布。`
      : "当前账号没有可访问仓库。请在 GitHub 创建仓库或授予仓库 / 组织访问权限，然后重新读取。";
    const location = workspace?.location;
    const connectedRepository = location?.backend === "github" ? `${location.owner}/${location.name}` : "";
    const selectedRepository = [preferredRepository, connectedRepository]
      .find((name) => repositories.some((repo) => repo.full_name === name));
    if (selectedRepository) {
      $("repo-choices").value = selectedRepository;
      repositoriesLoading = false;
      void loadRepositoryBranches(selectedRepository === preferredRepository ? preferredBranch : "");
    } else {
      resetBranchChoices(repositories.length ? "请先选择仓库" : "没有可用仓库，无法读取分支");
    }
  } catch (error) {
    if (!current()) return;
    repositories = [];
    $("repo-choices").replaceChildren(new Option("GitHub 仓库读取失败", ""));
    $("repo-choice-help").textContent = "读取失败，请检查网络、GitHub 登录与仓库 / 组织权限，然后重新读取。";
    $("auth-status").textContent = "GitHub 读取失败，请重试。";
    resetBranchChoices("请先重新读取仓库");
    showError("repo-error", error);
  } finally {
    if (current()) {
      repositoriesLoading = false;
      updateRepositoryControls();
    }
  }
}
async function openRepository() {
  if (busy || $("repo-dialog").open) return;
  const session = ++repositoryDialogSequence;
  invalidateGitHubRequests();
  repositories = [];
  $("repo-choices").replaceChildren(new Option("尚未读取仓库", ""));
  resetBranchChoices("请先选择仓库");
  const location = workspace?.location;
  if (location) {
    $("repo-type").value = location.backend === "github" ? "github" : "local-use";
    if (location.backend !== "github") {
      $("repo-path").value = location.path || "";
      $("repo-branch").value = location.branch;
    }
  }
  updateRepositoryFields();
  clearError("repo-error");
  closeNavigation();
  showDialog("repo-dialog");
  if ($("repo-type").value === "github") void loadGitHubRepositories();
  $("workspace-links").replaceChildren(element("p", "field-help", "正在读取已连接工作区…"));
  try {
    const value = await api("workspaces");
    if (session !== repositoryDialogSequence || !$("repo-dialog").open) return;
    $("workspace-links").replaceChildren();
    for (const item of value.workspaces) {
      const location = item.location;
      const name = location
        ? location.backend === "github"
          ? `${location.owner}/${location.name}`
          : location.path
        : "默认连接入口";
      const link = element("a", "workspace-link", `${name}${location?.branch ? ` · ${location.branch}` : ""}`);
      link.href = item.url;
      link.addEventListener("click", (event) => {
        if (busy) event.preventDefault();
      });
      if (item.id === value.current) {
        link.setAttribute("aria-current", "page");
        link.append(element("span", "muted", "当前工作区"));
      }
      $("workspace-links").append(link);
    }
    if (!value.workspaces.length)
      $("workspace-links").append(element("p", "field-help", "还没有已连接的工作区。"));
  } catch (error) {
    if (session !== repositoryDialogSequence || !$("repo-dialog").open) return;
    $("workspace-links").replaceChildren(element("p", "field-help", `工作区列表读取失败，可重新打开重试；下方仍可连接仓库。\n${error.message}`));
  }
}
function updateRepositoryFields() {
  const type = $("repo-type").value;
  const github = type === "github";
  $("local-fields").hidden = github;
  $("github-fields").hidden = !github;
  $("repo-path").required = !github;
  $("repo-choices").required = github;
  $("repo-branches").required = github;
  $("local-path-help").textContent =
    type === "local-create"
      ? "填写一个尚不存在或为空的目录，将创建 bare Git 仓库。"
      : "填写已有 Git 仓库的绝对路径，不会覆盖原内容。";
  updateRepositoryControls();
}
function result(title, fields, explanation) {
  $("result-heading").textContent = title;
  const body = $("result-body");
  body.replaceChildren();
  if (explanation) body.append(element("p", "", explanation));
  const values = element("dl");
  for (const [key, value] of fields)
    values.append(element("dt", "", key), element("dd", "", value));
  body.append(values);
  showDialog("result-dialog");
}
function openImport() {
  if (busy || !workspace) return;
  if (!workspace.location) {
    void openRepository();
    return;
  }
  $("import-initialization").hidden = workspace.initialized;
  $("import-result").hidden = true;
  clearError("import-error");
  closeNavigation();
  showDialog("import-dialog", "import-directory");
}
async function copyDelivery(markdown) {
  clearError("edit-error");
  try {
    const value = await api(`assets/${currentAsset}/url?provider=${$("url-provider").value}`);
    const asset = workspace.assets.find(({ asset }) => asset.id === currentAsset)?.asset;
    if (!asset) throw new Error("图片已不在当前工作区，请刷新后重试。");
    const alt = (asset.title || asset.original_name)
      .replace(/[\r\n\u2028\u2029]+/g, " ")
      .replace(/[\\[\]`*_<>]/g, "\\$&");
    const target = value.url.replace(/</g, "%3C").replace(/>/g, "%3E").replace(/[\r\n]/g, "");
    const text = markdown ? `![${alt}](<${target}>)` : value.url;
    $("delivery-url").hidden = false;
    $("delivery-url").value = text;
    $("delivery-url").setAttribute("aria-label", markdown ? "图片 Markdown" : "图片公开链接");
    $("delivery-note").textContent = value.committed
      ? "图片已提交。链接跟随分支，CDN 传播可能有延迟。"
      : "还没发布：这个地址要等提交之后才能访问。";
    try {
      await navigator.clipboard.writeText(text);
      toast(value.committed
        ? `已复制${markdown ? " Markdown" : "图片链接"}`
        : "已复制，但图片还没发布，提交后才能访问。");
    } catch {
      $("delivery-url").focus();
      $("delivery-url").select();
      toast("请从链接框手动复制");
    }
  } catch (error) {
    showError("edit-error", error);
  }
}

function bindEvents() {
  document
    .querySelectorAll("[data-close]")
    .forEach((node) =>
      node.addEventListener("click", () => hideDialog(node.closest("dialog"))),
    );
  document.querySelectorAll("dialog").forEach((dialog) => {
    dialog.addEventListener("cancel", (event) => {
      if (busy) event.preventDefault();
    });
    dialog.addEventListener("close", () => {
      if (dialog.id === "upload-dialog") releaseUploadFiles();
      if (dialog.id === "repo-dialog" && !dialog.open) {
        repositoryDialogSequence++;
        invalidateGitHubRequests();
      }
      const origin = focusOrigins.get(dialog);
      if (origin?.isConnected) origin.focus();
    });
  });
  $("toggle-nav").addEventListener("click", () => {
    const open = $("sidebar").classList.toggle("is-open");
    $("toggle-nav").setAttribute("aria-expanded", String(open));
  });
  $("all-assets").addEventListener("click", () => {
    resetFilters();
    closeNavigation();
  });
  $("clear-filters").addEventListener("click", resetFilters);
  $("open-pending").addEventListener("click", openPending);
  $("open-repo").addEventListener("click", openRepository);
  $("open-upload").addEventListener("click", () => openUpload());
  $("open-import").addEventListener("click", openImport);
  $("search").addEventListener("input", () => {
    filters.query = $("search").value;
    renderGallery();
  });
  $("namespace-filter").addEventListener("change", () => {
    filters.namespace = $("namespace-filter").value;
    renderGallery();
  });
  $("directory-filter").addEventListener("change", () => {
    filters.directory = $("directory-filter").value;
    renderGallery();
  });
  $("sort").addEventListener("change", () => {
    filters.sort = $("sort").value;
    renderGallery();
  });
  $("clear-selection").addEventListener("click", () => {
    selected.clear();
    renderGallery();
  });
  $("select-visible").addEventListener("change", () => {
    for (const { asset } of visibleAssets())
      if ($("select-visible").checked) selected.add(asset.id);
      else selected.delete(asset.id);
    renderGallery();
  });
  $("remove-selected").addEventListener("click", () =>
    removeAssets([...selected]),
  );
  $("remove-current").addEventListener("click", () =>
    removeAssets([currentAsset]),
  );
  $("refresh-state").addEventListener("click", async () => {
    if (busy) return;
    try {
      await refreshState();
      toast("工作空间已刷新");
    } catch (error) {
      showError("page-error", error);
    }
  });
  $("sync").addEventListener("click", async () => {
    await mutate(() => api("sync", {}), "page-error", "已同步远端图库");
  });
  $("reindex").addEventListener("click", async () => {
    await mutate(() => api("reindex", {}), "page-error", "已从仓库固定快照重建索引，未发布修改保留");
  });
  $("import-form").addEventListener("submit", async (event) => {
    event.preventDefault();
    if (!workspace.initialized) return;
    $("import-result").hidden = true;
    const value = await mutate(
      () => api("import", {
        directory: $("import-directory").value.trim(),
        namespace: $("import-namespace").value.trim(),
        tags: list($("import-tags").value),
        collections: list($("import-collections").value),
      }),
      "import-error",
    );
    if (value) {
      $("import-result").textContent = `${value.imported} 张已暂存，${value.skipped} 张已跳过。图片保留原仓库路径；尚未发布，必须在「待发布」中提交才会写入管理元数据。`;
      $("import-result").hidden = false;
    }
  });
  $("setup-action").addEventListener("click", async () => {
    if (!workspace?.location) openRepository();
    else await mutate(() => api("init", {}), "page-error", "TOUKO 仓库已就绪");
  });
  $("verify").addEventListener("click", async () => {
    const value = await mutate(() => api("verify", {}), "page-error");
    if (value)
      result(
        "仓库校验通过",
        [
          ["图片", `${value.assets} 张`],
          ["内容对象", `${value.objects} 个`],
          ["HEAD", value.head],
        ],
        "校验的是仓库后端固定 HEAD 上的完整元数据与图片文件，不包含还没发布的修改；不会自动获取远端更新，请先按需同步。",
      );
  });
  $("preview-image").addEventListener("error", () => {
    $("preview-image").hidden = true;
    $("preview-image-error").hidden = false;
  });
  $("edit-form").addEventListener("submit", async (event) => {
    event.preventDefault();
    const value = await mutate(
      () =>
        api(`assets/${currentAsset}/edit`, {
          title: $("edit-title").value,
          tags: list($("edit-tags").value),
          collections: list($("edit-collections").value),
        }),
      "edit-error",
      "修改已保存到待发布区",
    );
    if (value) openPreview(currentAsset);
  });
  $("copy-url").addEventListener("click", () => copyDelivery(false));
  $("copy-markdown").addEventListener("click", () => copyDelivery(true));
  $("upload-files").addEventListener("change", () =>
    setUploadFiles([...$("upload-files").files]),
  );
  $("upload-form").addEventListener("submit", async (event) => {
    event.preventDefault();
    if (!uploadFiles.length) return;
    const value = await mutate(
      upload,
      "upload-error",
      "图片已加入待发布区，还没有写入仓库",
    );
    if (value) $("upload-dialog").close();
  });
  document.addEventListener("dragover", (event) => {
    if ([...event.dataTransfer.types].includes("Files")) {
      event.preventDefault();
      $("drop-zone").classList.add("drag-over");
    }
  });
  document.addEventListener("dragleave", (event) => {
    if (!event.relatedTarget) $("drop-zone").classList.remove("drag-over");
  });
  document.addEventListener("drop", (event) => {
    if (![...event.dataTransfer.types].includes("Files")) return;
    event.preventDefault();
    $("drop-zone").classList.remove("drag-over");
    const dialog = document.querySelector("dialog[open]");
    if (dialog && dialog.id !== "upload-dialog") {
      toast("请先关闭当前对话框，再拖入图片。");
      return;
    }
    openUpload([...event.dataTransfer.files]);
  });
  $("commit-form").addEventListener("submit", async (event) => {
    event.preventDefault();
    const value = await mutate(
      () => api("commit", { message: $("commit-message").value.trim() }),
      "pending-error",
      "整批修改已提交到仓库",
    );
    if (value) {
      $("commit-message").value = "";
      $("pending-dialog").close();
    }
  });
  $("reset-staged").addEventListener("click", () =>
    confirm(
      "丢弃全部未发布修改？",
      "待发布区里的新增、编辑和移除都会丢弃，而且不能撤销。仓库里已经发布过的历史不会改变。\n如果有还没确认的发布，服务会先核实远端结果。",
      async () => {
        const value = await mutate(
          () => api("reset", { confirm: true }),
          "confirm-error",
          "待发布修改已清空",
        );
        if (value) {
          $("confirm-dialog").close();
          $("pending-dialog").close();
        }
      },
    ),
  );
  $("confirm-action").addEventListener("click", () => {
    if (!busy && confirmOperation) void confirmOperation();
  });
  $("repo-type").addEventListener("change", () => {
    invalidateGitHubRequests();
    updateRepositoryFields();
    clearError("repo-error");
    if ($("repo-type").value === "github") void loadGitHubRepositories();
  });
  $("repo-form").addEventListener("submit", async (event) => {
    event.preventDefault();
    const type = $("repo-type").value;
    if (busy || (type === "github" && !validGitHubSelection())) return;
    const input =
      type === "github"
        ? {
            backend: "github",
            repository: $("repo-choices").value,
            branch: $("repo-branches").value,
          }
        : {
            backend: "localgit",
            path: $("repo-path").value.trim(),
            branch: $("repo-branch").value.trim() || "main",
            create: type === "local-create",
          };
    clearError("repo-error");
    setBusy(true);
    try {
      const value = await api("repo", input);
      window.location.assign(value.url);
    } catch (error) {
      showError("repo-error", error);
      setBusy(false);
    }
  });
  $("check-auth").addEventListener("click", () => {
    if (busy) return;
    $("repo-type").value = "github";
    updateRepositoryFields();
    void loadGitHubRepositories();
  });
  $("repo-choices").addEventListener("change", () => {
    if (!busy) void loadRepositoryBranches();
  });
  $("repo-branches").addEventListener("change", updateRepositoryControls);
  document.addEventListener("keydown", (event) => {
    if (
      event.key === "/" &&
      !document.querySelector("dialog[open]") &&
      !["INPUT", "TEXTAREA", "SELECT"].includes(document.activeElement.tagName)
    ) {
      event.preventDefault();
      $("search").focus();
    }
    if (event.key === "Escape" && !document.querySelector("dialog[open]"))
      closeNavigation();
  });
}

bindEvents();
document.querySelector(".brand").href = `${workspacePrefix}/`;
try {
  await refreshState();
} catch (error) {
  showError("page-error", error);
  $("gallery").replaceChildren(
    element("div", "empty-state", "工作空间未能读取，请检查终端并点击刷新。"),
  );
  $("gallery").setAttribute("aria-busy", "false");
}
