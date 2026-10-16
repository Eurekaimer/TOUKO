const $ = (id) => document.getElementById(id);
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
    message += "\n请在启动服务的终端设置 GITHUB_TOKEN，然后重启服务。";
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
  const response = await fetch(`/api/${path}`, options);
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
  filters.query = filters.namespace = filters.collection = filters.tag = "";
  $("search").value = "";
  $("namespace-filter").value = "";
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
    ? "连接你自己的 Git 仓库后开始收藏"
    : `${location.backend === "github" ? (location.private ? "GitHub 私有仓库" : "GitHub 公开仓库") : "本地 Git 仓库"}，分支 ${location.branch}`;
  $("repo-detail").title = location?.path || "";
  $("repo-dot").classList.toggle("connected", !!location);
  $("head-indicator").textContent = head
    ? `HEAD ${head.slice(0, 8)}，${changes.length ? "有修改待发布" : "没有待发布修改"}`
    : "尚未连接仓库";
  $("head-indicator").title = head;
  $("auth-status").textContent = workspace.auth.configured
    ? "服务已配置 token，可以检查身份"
    : "服务没有配置 GITHUB_TOKEN";
  const namespaces = [
    ...new Set(assets.map(({ asset }) => asset.namespace)),
  ].sort();
  $("namespace-filter").replaceChildren(
    new Option("全部命名空间", ""),
    ...namespaces.map((name) => new Option(name, name)),
  );
  if (!namespaces.includes(filters.namespace)) filters.namespace = "";
  $("namespace-filter").value = filters.namespace;
  $("setup-banner").hidden = !!location && workspace.initialized;
  if (!location) {
    $("setup-title").textContent = "还没有连接仓库";
    $("setup-description").textContent =
      "连接本机已有的 Git 仓库，或连接一个专门存图片的 GitHub 仓库。图片与元数据始终由你掌握。";
    $("setup-action").textContent = "连接仓库";
  } else if (!workspace.initialized) {
    $("setup-title").textContent = "仓库已连接，还没有初始化";
    $("setup-description").textContent =
      "已经有 TOUKO 图库，请点上面的「同步」读入；新仓库可以在这里初始化格式。GitHub 仓库要先创建 README。";
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
    item.append(
      icon("folder"),
      element("span", "", name),
      element("span", "count", count),
    );
    $("collection-filters").append(item);
  }
  if (!collections.length)
    $("collection-filters").append(
      element("p", "sidebar-hint", "图片的分类会出现在这里"),
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
    $("tag-filters").append(element("p", "sidebar-hint", "图片的标签会出现在这里"));
}
function visibleAssets() {
  if (!workspace) return [];
  const query = filters.query.trim().toLocaleLowerCase();
  return workspace.assets
    .filter(
      ({ asset }) =>
        (!filters.namespace || asset.namespace === filters.namespace) &&
        (!filters.collection ||
          asset.collections.includes(filters.collection)) &&
        (!filters.tag || asset.tags.includes(filters.tag)) &&
        (!query ||
          [
            asset.title,
            asset.original_name,
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
  return `/api/assets/${asset.id}/image?content=${asset.object.hash}&head=${workspace.head}`;
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
  // Only the selection control floats over the picture; state is stated in the caption.
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
  if (view.state !== "committed")
    badges.append(
      element(
        "span",
        `state-chip ${view.state}`,
        view.state === "added" ? "待发布" : "有修改",
      ),
    );
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
    filters.tag
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
          ? "换个关键词试试，或者清除筛选看全部图片。"
          : "上传 PNG、JPEG 或 GIF，再用标签与分类整理。",
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
      ? "尚未发布 — 这是提交后才会生效的地址。"
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
    request.open("POST", "/api/upload");
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
function openRepository() {
  if (busy) return;
  const location = workspace?.location;
  if (location) {
    $("repo-type").value =
      location.backend === "github" ? "github" : "local-use";
    $("repo-path").value = location.path || "";
    $("repo-github").value = location.owner
      ? `${location.owner}/${location.name}`
      : "";
    $("repo-branch").value = location.branch;
  }
  updateRepositoryFields();
  clearError("repo-error");
  closeNavigation();
  showDialog("repo-dialog");
}
function updateRepositoryFields() {
  const type = $("repo-type").value;
  const github = type === "github";
  $("local-fields").hidden = github;
  $("github-fields").hidden = !github;
  $("repo-path").required = !github;
  $("repo-github").required = github;
  $("local-path-help").textContent =
    type === "local-create"
      ? "填写一个尚不存在或为空的目录，将创建 bare Git 仓库。"
      : "填写已有 Git 仓库的绝对路径，不会覆盖原内容。";
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
  $("search").addEventListener("input", () => {
    filters.query = $("search").value;
    renderGallery();
  });
  $("namespace-filter").addEventListener("change", () => {
    filters.namespace = $("namespace-filter").value;
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
        "校验的是远端固定 HEAD 上的完整元数据与图片文件，不包含还没发布的修改。",
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
  $("copy-url").addEventListener("click", async () => {
    clearError("edit-error");
    try {
      const value = await api(
        `assets/${currentAsset}/url?provider=${$("url-provider").value}`,
      );
      $("delivery-url").hidden = false;
      $("delivery-url").value = value.url;
      $("delivery-note").textContent = value.committed
        ? "图片已提交。链接跟随分支，CDN 传播可能有延迟。"
        : "尚未发布 — 这是提交后才会生效的地址。";
      try {
        await navigator.clipboard.writeText(value.url);
        toast(
          value.committed
            ? "已复制图片链接"
            : "已复制预期链接。图片还没发布，提交后才能访问。",
        );
      } catch {
        $("delivery-url").focus();
        $("delivery-url").select();
        toast("请从链接框手动复制");
      }
    } catch (error) {
      showError("edit-error", error);
    }
  });
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
  $("repo-type").addEventListener("change", updateRepositoryFields);
  $("repo-form").addEventListener("submit", async (event) => {
    event.preventDefault();
    const type = $("repo-type").value;
    const input =
      type === "github"
        ? {
            backend: "github",
            repository: $("repo-github")
              .value.trim()
              .replace(/\s*\/\s*/g, "/"),
            branch: $("repo-branch").value.trim(),
          }
        : {
            backend: "localgit",
            path: $("repo-path").value.trim(),
            branch: $("repo-branch").value.trim() || "main",
            create: type === "local-create",
          };
    const value = await mutate(
      () => api("repo", input),
      "repo-error",
      type === "local-create"
        ? "已创建本地图库"
        : "仓库已连接，请同步或初始化",
    );
    if (value) $("repo-dialog").close();
  });
  $("check-auth").addEventListener("click", async () => {
    $("check-auth").disabled = true;
    try {
      const value = await api("auth");
      $("auth-status").textContent = value.configured
        ? `GitHub 身份：${value.login}`
        : "请在终端设置 GITHUB_TOKEN 后重启服务";
    } catch (error) {
      showError("repo-error", error);
    } finally {
      $("check-auth").disabled = false;
    }
  });
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
try {
  await refreshState();
} catch (error) {
  showError("page-error", error);
  $("gallery").replaceChildren(
    element("div", "empty-state", "工作空间未能读取，请检查终端并点击刷新。"),
  );
  $("gallery").setAttribute("aria-busy", "false");
}
