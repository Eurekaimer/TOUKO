const $ = (id) => document.getElementById(id);
// A page is either one workspace (/w/<id>/) or one library (/l/<id>/). Both use
// the same document; only the prefix and the shape of the state differ.
const viewPrefix = window.location.pathname.match(/^\/(?:w|l)\/[^/]+(?=\/|$)/)?.[0] || "";
const isLibraryURL = viewPrefix.startsWith("/l/");
const apiURL = (path, prefix = viewPrefix) => `${prefix}/api/${path}`;
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
  source: "",
  sort: "newest",
};
// Asset IDs are only unique inside one repository, so every identity in the UI
// is the owning workspace plus the asset ID.
const selected = new Set();
const cards = new Map();
const focusOrigins = new WeakMap();
let state = null;
let view = null;
let busy = false;
let currentKey = null;
let uploadFiles = [];
let confirmOperation = null;
let toastTimer;
let refreshSequence = 0;
let uploadSequence = 0;
let repositories = [];
let branches = [];
let repositoriesLoading = false;
let branchesLoading = false;
let repositoryDialogSequence = 0;
let githubRequestSequence = 0;
let branchRequestSequence = 0;
let clipboardBusy = false;
let operationRetry = null;

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
  const target = $(id);
  target.hidden = true;
  target.textContent = "";
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
  if (code === "capacity_full")
    message += "\n请手动连接新的仓库，或先提交/清理已有图片。";
  if (code === "unauthorized")
    message += "\n请在终端运行 touko auth login，或检查 GITHUB_TOKEN / GH_TOKEN 的仓库权限，再重新读取登录。";
  return new Error(message);
}
// A refused connection is not a server error: the window may still be starting,
// or the process may have exited. Say which, instead of Chromium's "failed to
// fetch", which tells the user nothing about what to do next.
function networkError(error) {
  if (error instanceof TypeError) {
    return new Error(
      "无法连接本地 TOUKO 服务。若刚启动，请稍候再刷新；若终端里的进程已退出，请重新打开 TOUKO。",
    );
  }
  return error;
}
async function api(path, body, prefix = viewPrefix) {
  const options = { headers: { Accept: "application/json" } };
  if (body !== undefined) {
    options.method = "POST";
    options.headers["Content-Type"] = "application/json";
    options.headers["X-Touko-CSRF"] = view.csrf;
    options.body = JSON.stringify(body);
  }
  let response;
  try {
    response = await fetch(apiURL(path, prefix), options);
  } catch (error) {
    throw networkError(error);
  }
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
// apiMember talks to one owner workspace using that member's own mutation token.
async function apiMember(member, path, body) {
  const prefix = `/w/${member.id}`;
  const options = { headers: { Accept: "application/json" } };
  if (body !== undefined) {
    options.method = "POST";
    options.headers["Content-Type"] = "application/json";
    options.headers["X-Touko-CSRF"] = member.csrf;
    options.body = JSON.stringify(body);
  }
  let response;
  try {
    response = await fetch(apiURL(path, prefix), options);
  } catch (error) {
    throw networkError(error);
  }
  let data;
  try {
    data = await response.json();
  } catch {
    throw new Error(
      `成员 ${member.id.slice(0, 8)} 返回了无效响应（HTTP ${response.status}）。`,
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
function keyOf(view) {
  return `${view.workspace}:${view.asset.id}`;
}
function keyOfID(workspaceID, assetID) {
  return `${workspaceID}:${assetID}`;
}
function splitKey(key) {
  const index = key.indexOf(":");
  return { workspace: key.slice(0, index), assetID: key.slice(index + 1) };
}
function memberByID(id) {
  return view?.members.find((member) => member.id === id) || null;
}
function memberName(id) {
  const member = memberByID(id);
  const location = member?.location;
  if (!location) return member?.error ? `不可用成员 ${id.slice(0, 8)}` : id.slice(0, 8);
  return location.backend === "github"
    ? `${location.owner}/${location.name}`
    : location.path.split("/").filter(Boolean).at(-1);
}
function memberForAsset(assetView) {
  return memberByID(assetView.workspace);
}
function viewForKey(key) {
  if (!view) return null;
  const { workspace, assetID } = splitKey(key);
  return view.assets.find(
    (entry) => entry.workspace === workspace && entry.asset.id === assetID,
  ) || null;
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
// normalize turns both server shapes into one view: a library returns members,
// a single workspace is simply its own only member. Nothing invents a shared
// head, location, or publication for a library.
function normalize(raw) {
  const members = raw.members || [
    {
      id: raw.workspace,
      url: viewPrefix + "/",
      csrf: raw.csrf,
      location: raw.location,
      initialized: raw.initialized,
      head: raw.head,
      base_head: raw.base_head,
      assets: raw.assets,
      changes: raw.changes,
      publication: raw.publication,
      local_read: raw.local_read,
      capacity: null,
      error: null,
    },
  ];
  const assets = [];
  for (const entry of raw.assets || []) {
    assets.push({ workspace: entry.workspace || raw.workspace, state: entry.state, asset: entry.asset });
  }
  const changes = (raw.changes || []).map((entry) => {
    if (entry && entry.change) return { workspace: entry.workspace, change: entry.change };
    return { workspace: raw.workspace, change: entry };
  });
  return {
    kind: raw.kind || "workspace",
    library: raw.library || null,
    csrf: raw.csrf,
    members,
    assets,
    changes,
    auth: raw.auth || { configured: false, source: "", login: "" },
  };
}
async function refreshState() {
  const sequence = ++refreshSequence;
  const next = normalize(await api("state"));
  if (sequence !== refreshSequence) return;
  state = next;
  view = next;
  const available = new Set(next.assets.map(keyOf));
  for (const key of selected) if (!available.has(key)) selected.delete(key);
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
  hideCursorHint();
  $("sidebar").classList.remove("is-open");
  $("toggle-nav").setAttribute("aria-expanded", "false");
}
function resetFilters() {
  filters.query = filters.namespace = filters.collection = filters.tag = filters.directory = filters.source = "";
  $("search").value = "";
  $("namespace-filter").value = "";
  $("directory-filter").value = "";
  if ($("source-filter")) $("source-filter").value = "";
  renderWorkspace();
}
function firstUsableMember() {
  return view?.members.find((member) => member.location && member.initialized && !member.error)
    || view?.members.find((member) => member.location)
    || null;
}
function ready() {
  if (!view) {
    toast("请等待图库读取完成。");
    return false;
  }
  if (!firstUsableMember()) {
    openRepository();
    return false;
  }
  return true;
}
function updateActions() {
  if (!view) return;
  const member = firstUsableMember();
  const initialized = view.members.some((entry) => entry.initialized);
  $("sync").disabled = busy || !member;
  $("verify").disabled = busy || !initialized;
  $("reindex").disabled = busy || !initialized;
  $("open-import").disabled = busy;
  $("import-submit").disabled = busy || !initialized;
  $("open-upload").disabled = busy;
  $("setup-action").disabled = busy;
  $("upload-submit").disabled = busy || !uploadFiles.length || !initialized;
  $("upload-paste").disabled = busy || clipboardBusy;
  $("commit-submit").disabled =
    busy || !(view.changes.length || view.members.some((entry) => entry.publication));
  $("reset-staged").disabled =
    busy || !(view.changes.length || view.members.some((entry) => entry.publication));
  $("retry-operation").hidden = !operationRetry || busy;
  $("remove-selected").disabled = busy || !selected.size;
}
function renderWorkspace() {
  const { assets, changes, members } = view;
  $("nav-total").textContent = assets.length;
  $("nav-pending").textContent = changes.length;
  const library = view.kind === "library";
  const primary = members.find((entry) => entry.location) || members[0];
  $("repo-name").textContent = library
    ? view.library?.name || "统一图库"
    : !primary?.location
      ? "尚未连接"
      : memberName(primary.id);
  $("repo-detail").textContent = library
    ? `${members.length} 个仓库成员 · 每个仓库独立暂存与发布`
    : !primary?.location
      ? "连接仓库后，图片就归档在这里。"
      : `${primary.location.backend === "github" ? (primary.location.private ? "GitHub 私有仓库" : "GitHub 公开仓库") : "本地 Git 仓库"}，分支 ${primary.location.branch}`;
  $("repo-detail").title = primary?.location?.path || "";
  $("repo-dot").classList.toggle("connected", members.some((entry) => entry.location));
  if (library) {
    const heads = members.filter((entry) => entry.head);
    $("head-indicator").textContent = heads.length
      ? `${heads.length} / ${members.length} 个成员已初始化，${changes.length ? "有修改待发布" : "没有待发布修改"}`
      : "成员尚未初始化";
    $("head-indicator").title = members
      .map((entry) => `${memberName(entry.id)}: ${entry.head || "未初始化"}${entry.error ? ` (${entry.error})` : ""}`)
      .join("\n");
  } else {
    $("head-indicator").textContent = primary?.head
      ? `HEAD ${primary.head.slice(0, 8)}，${changes.length ? "有修改待发布" : "没有待发布修改"}`
      : "尚未连接仓库";
    $("head-indicator").title = primary?.head || "";
  }
  $("auth-status").textContent = view.auth.configured
    ? "服务端已有 GitHub 凭据，可读取登录与仓库列表"
    : "尚未读取 GitHub 登录；可使用 gh 登录或环境变量凭据";
  const readable = members.find((entry) => entry.local_read?.available);
  $("local-read-status").textContent = readable
    ? `本地读图副本可用：${readable.local_read.path}`
    : "本地读图副本尚不可用。图片是否可读取决于仓库后端；同步后可再次检查。";
  $("local-read-status").title = readable?.local_read?.path || "";
  renderMemberStatus();
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
  // The source filter only exists when one page truly holds several repositories.
  const sources = [...new Set(assets.map((entry) => entry.workspace))];
  if ($("source-filter")) {
    $("source-filter-field").hidden = sources.length < 2;
    $("source-filter").replaceChildren(
      new Option("全部来源仓库", ""),
      ...sources.map((id) => new Option(memberName(id), id)),
    );
    if (!sources.includes(filters.source)) filters.source = "";
    $("source-filter").value = filters.source;
  }
  $("setup-banner").hidden = members.some((entry) => entry.location && entry.initialized);
  if (!members.some((entry) => entry.location)) {
    $("setup-title").textContent = "还没有连接仓库";
    $("setup-description").textContent =
      "连接本机已有的 Git 仓库，或连接一个专门存图片的 GitHub 仓库。图片与元数据始终由你掌握。";
    $("setup-action").textContent = "连接仓库";
  } else if (members.some((entry) => entry.location && !entry.initialized && !entry.error)) {
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
function renderMemberStatus() {
  const target = $("member-status");
  if (!target) return;
  target.replaceChildren();
  const problems = view.members.filter((member) => member.error);
  target.hidden = view.kind !== "library" || !problems.length;
  if (target.hidden) return;
  for (const member of problems) {
    const row = element("div", "member-status-row");
    row.append(
      element("strong", "", memberName(member.id)),
      element("span", "muted", member.error || "该成员当前不可读"),
    );
    target.append(row);
  }
}
function counts(field) {
  const result = new Map();
  for (const { asset } of view.assets)
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
  if (!view) return [];
  const query = filters.query.trim().toLocaleLowerCase();
  return view.assets
    .filter(
      ({ asset, workspace }) =>
        (!filters.source || workspace === filters.source) &&
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
            // Content hashes are searched explicitly, not only through the
            // canonical path that happens to contain them.
            asset.object.hash,
            ...asset.tags,
            ...asset.collections,
          ].some((text) => (text || "").toLocaleLowerCase().includes(query))),
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
      return order || keyOf(a).localeCompare(keyOf(b));
    });
}
function imageURL(entry) {
  const member = memberForAsset(entry);
  const head = member?.head || "";
  return apiURL(
    `assets/${entry.asset.id}/image?content=${entry.asset.object.hash}&head=${head}`,
    `/w/${entry.workspace}`,
  );
}
// canCopyReference reports whether this picture can have a public delivery URL at
// all: private or local repositories deliberately produce none.
function canCopyReference(entry) {
  const location = memberForAsset(entry)?.location;
  return !!location && location.backend === "github" && !location.private;
}
// A small label follows the pointer while a copy request runs and then reports
// the outcome in place, so the user never has to look away from the picture.
let cursorHint = null;
function showCursorHint(event, text, state = "") {
  if (!cursorHint) {
    cursorHint = element("div", "cursor-hint");
    cursorHint.setAttribute("role", "status");
    cursorHint.setAttribute("aria-live", "polite");
    document.body.append(cursorHint);
  }
  cursorHint.textContent = text;
  cursorHint.className = `cursor-hint ${state}`.trim();
  cursorHint.hidden = false;
  const x = Math.min(event?.clientX ?? window.innerWidth / 2, window.innerWidth - 220);
  const y = Math.min(event?.clientY ?? window.innerHeight / 2, window.innerHeight - 60);
  cursorHint.style.transform = `translate(${Math.max(8, x + 14)}px, ${Math.max(8, y + 16)}px)`;
}
function hideCursorHint(delay = 0) {
  if (!cursorHint) return;
  const node = cursorHint;
  setTimeout(() => {
    if (node === cursorHint) node.hidden = true;
  }, delay);
}
async function copyCardMarkdown(key, event) {
  if (busy) return;
  showCursorHint(event, "正在复制 Markdown…", "pending");
  try {
    const delivered = await copyReference(true, key);
    const published = delivered?.committed;
    showCursorHint(event, published ? "已复制 Markdown" : "已复制 Markdown（尚未发布，提交后才能访问）", "done");
    setTimeout(() => hideCursorHint(), published ? 1600 : 3200);
  } catch (error) {
    showCursorHint(event, `复制失败：${error.message}`, "error");
    setTimeout(() => hideCursorHint(), 4000);
  }
}
function card(entry) {
  const { asset } = entry;
  const key = keyOf(entry);
  const node = element("article", "asset-card");
  node.dataset.key = key;
  const media = element("div", "card-image-wrap");
  // The picture is a button of its own: one click copies the Markdown reference,
  // and the whole behaviour changes only where no public link can exist.
  const open = element("button", "card-open");
  open.type = "button";
  const copyable = canCopyReference(entry);
  open.setAttribute(
    "aria-label",
    copyable
      ? `单击复制 Markdown，双击查看 ${asset.title || asset.original_name}`
      : `查看 ${asset.title || asset.original_name}`,
  );
  open.addEventListener("click", (event) => {
    event.preventDefault();
    if (copyable) void copyCardMarkdown(key, event);
    else openPreview(key);
  });
  open.addEventListener("dblclick", (event) => {
    event.preventDefault();
    hideCursorHint();
    openPreview(key);
  });
  const img = element("img", "card-image");
  img.alt = asset.title || asset.original_name;
  img.loading = "lazy";
  img.decoding = "async";
  img.src = imageURL(entry);
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
    if (checkbox.checked) selected.add(key);
    else selected.delete(key);
    renderGallery();
  });
  select.append(checkbox);
  media.append(open, select);
  const content = element("div", "card-content");
  // The title always opens the preview, including on a copyable card.
  const title = button(asset.title || asset.original_name, "card-title", () =>
    openPreview(key),
  );
  title.title = asset.title;
  const heading = element("h3", "card-heading");
  heading.append(title);
  const badges = element("div", "card-tags");
  badges.append(element("span", "namespace-chip", asset.namespace));
  // A card says which repository it belongs to whenever the page can hold more
  // than one, so identical titles from two members never look like one picture.
  if (view.members.length > 1) {
    const source = element("span", "card-tag source");
    source.append(icon("branch"), document.createTextNode(memberName(entry.workspace)));
    badges.append(source);
  }
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
  if (entry.state !== "committed") {
    stateSlip = element(
      "span",
      `state-chip ${entry.state}`,
      entry.state === "added" ? "待发布" : "有修改",
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
  if (filters.source)
    parts.push({
      label: `来源 ${memberName(filters.source)}`,
      clear: () => { filters.source = ""; if ($("source-filter")) $("source-filter").value = ""; },
    });
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
  if (!view) return;
  const views = visibleAssets();
  const active = !!(
    filters.query ||
    filters.namespace ||
    filters.collection ||
    filters.tag ||
    filters.directory ||
    filters.source
  );
  $("clear-filters").hidden = !active;
  $("all-assets").classList.toggle("active", !active);
  $("result-count").textContent =
    `${views.length} / ${view.assets.length} 张`;
  renderTrail();
  $("gallery").setAttribute("aria-busy", "false");
  $("gallery").classList.toggle("has-selection", selected.size > 0);
  $("selection-bar").hidden = selected.size === 0;
  $("selected-count").textContent = `已选 ${selected.size} 张`;
  const checked = views.filter((entry) => selected.has(keyOf(entry))).length;
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
        active ? "清除筛选" : firstUsableMember() ? "上传图片" : "连接仓库",
        "button primary",
        active ? resetFilters : () => openUpload(),
      ),
    );
    $("gallery").replaceChildren(empty);
  } else {
    const nodes = views.map((entry) => {
      const key = keyOf(entry);
      let node = cards.get(key);
      if (!node) {
        node = card(entry);
        cards.set(key, node);
      }
      node.classList.toggle("is-selected", selected.has(key));
      node.classList.toggle("is-staged", entry.state !== "committed");
      node.querySelector("input[type=checkbox]").checked = selected.has(key);
      return node;
    });
    $("gallery").replaceChildren(...nodes);
  }
  $("gallery-footer-note").textContent = active
    ? `筛选出 ${views.length} 张图片。`
    : view.changes.length
      ? `有 ${view.changes.length} 项修改只在待发布区，提交后才会写入仓库。`
      : "没有待发布的修改。";
  updateActions();
}
function openPreview(key) {
  const entry = viewForKey(key);
  if (!entry) return;
  currentKey = key;
  const { asset } = entry;
  const member = memberForAsset(entry);
  $("preview-image-error").hidden = true;
  $("preview-image").hidden = false;
  $("preview-image").src = imageURL(entry);
  $("preview-image").alt = asset.title || asset.original_name;
  $("asset-path").textContent = assetPath(asset);
  $("edit-title").value = asset.title;
  $("edit-tags").value = asset.tags.join(", ");
  $("edit-collections").value = asset.collections.join(", ");
  $("preview-state").className = `asset-state ${entry.state}`;
  $("preview-state").textContent = stateNames[entry.state];
  $("asset-metadata").replaceChildren();
  for (const [label, value] of [
    ["来源仓库", view.members.length > 1 ? memberName(entry.workspace) : null],
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
    if (value === null) continue;
    $("asset-metadata").append(
      element("dt", "", label),
      element("dd", "", value),
    );
  }
  const publicGitHub =
    member?.location?.backend === "github" && !member.location.private;
  $("delivery-controls").hidden = !publicGitHub;
  $("delivery-unavailable").hidden = publicGitHub;
  $("delivery-url").hidden = true;
  $("delivery-url").value = "";
  $("delivery-note").textContent =
    entry.state === "added"
      ? "还没发布：这个地址要等提交之后才能访问。"
      : "链接跟随分支；jsDelivr 的缓存传播可能有延迟。";
  clearError("edit-error");
  showDialog("preview-dialog");
}
function pendingGroups() {
  const groups = new Map();
  for (const entry of view.changes) {
    if (!groups.has(entry.workspace)) groups.set(entry.workspace, []);
    groups.get(entry.workspace).push(entry.change);
  }
  return groups;
}
function renderPending() {
  const pending = view.changes.length;
  $("pending-count").textContent = pending;
  const bases = view.members.filter((member) => member.base_head);
  $("pending-base").textContent = bases.length
    ? `基于 ${bases.map((member) => `${memberName(member.id)} HEAD ${member.base_head.slice(0, 8)}`).join("、")}`
    : "还没有提交过";
  const publications = view.members.filter((member) => member.publication);
  $("publication-warning").hidden = !publications.length;
  if (publications.length)
    $("publication-warning").textContent = publications
      .map((member) => `${memberName(member.id)}：发布结果未确认（${member.publication.candidate_head}），请先同步。`)
      .join("\n");
  $("pending-list").replaceChildren();
  for (const { workspace, change } of view.changes) {
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
      element("small", "", `${memberName(workspace)} · ${change.asset_id}`),
    );
    row.append(
      element("span", `change-label ${change.kind}`, changeNames[change.kind]),
      detail,
    );
    $("pending-list").append(row);
  }
  if (!view.changes.length)
    $("pending-list").append(
      element(
        "p",
        "pending-empty",
        publications.length ? "发布日志还没恢复，请先同步。" : "没有待发布的修改。",
      ),
    );
  renderOperationResults();
}
function renderOperationResults() {
  const panel = $("operation-panel");
  if (!panel) return;
  const target = $("operation-results");
  target.replaceChildren();
  if (!operationRetry) {
    panel.hidden = true;
    $("pending-member-results")?.replaceChildren();
    return;
  }
  panel.hidden = false;
  for (const row of operationRetry.results) {
    const item = element("div", `member-result ${row.ok ? "ok" : "error"}`);
    item.append(
      element("strong", "", memberName(row.workspace)),
      element(
        "span",
        "muted",
        row.ok ? row.message || "完成" : row.error || "失败",
      ),
    );
    target.append(item);
    $("pending-member-results")?.append(item.cloneNode(true));
  }
  $("retry-operation").hidden = busy || !operationRetry.failed.length;
  $("retry-operation").textContent = `重试失败成员（${operationRetry.failed.length}）`;
}
function openPending() {
  if (!view) return;
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
function removeAssets(keys) {
  if (!keys.length) return;
  const groups = new Map();
  for (const key of keys) {
    const { workspace, assetID } = splitKey(key);
    if (!groups.has(workspace)) groups.set(workspace, []);
    groups.get(workspace).push(assetID);
  }
  confirm(
    "移除所选图片？",
    `将从 ${groups.size} 个仓库移除 ${keys.length} 张图片到待发布的移除列表；提交后它们才会从仓库的最新版本里消失。\n本机的原始文件不会被删除，Git 历史也不会被清除。`,
    async () => {
      const results = [];
      const failed = [];
      for (const [workspace, selectors] of groups) {
        const member = memberByID(workspace);
        try {
          if (!member) throw new Error("成员已不在当前图库中");
          await apiMember(member, "remove", { selectors });
          results.push({ workspace, ok: true, message: `移除 ${selectors.length} 张` });
        } catch (error) {
          failed.push(workspace);
          results.push({ workspace, ok: false, error: error.message });
        }
      }
      operationRetry = { results, failed, action: "remove", selection: groups };
      $("confirm-dialog").close();
      if (keys.includes(currentKey)) $("preview-dialog").close();
      try {
        await refreshState();
      } catch (error) {
        showError("page-error", error);
      }
      if (failed.length) toast(`${failed.length} 个仓库移除失败，请查看结果`);
      else toast("已加入待发布的移除列表");
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
  const limit = view?.kind === "library" ? 20_000_000 : 100_000_000;
  if (
    combined.length > 100 ||
    combined.some((file) => file.size > limit) ||
    combined.reduce((total, file) => total + file.size, 0) > 500_000_000
  ) {
    showError(
      "upload-error",
      view?.kind === "library"
        ? "统一图库一次最多上传 100 张、合计 500 MB，单张不超过 20 MB（jsDelivr 单文件限制）。"
        : "一次最多上传 100 张、合计 500 MB，单张不超过 100 MB。请去掉一些文件再试。",
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
  if ($("upload-limit"))
    $("upload-limit").textContent = view?.kind === "library"
      ? "统一图库：单张不超过 20 MB，一次合计不超过 500 MB。图片会按容量自动分配到成员仓库。"
      : "单张不超过 100 MB，一次合计不超过 500 MB。";
  updateActions();
}
function openUpload(files) {
  if (busy || !ready()) return;
  clearError("upload-error");
  $("upload-progress-area").hidden = true;
  if ($("upload-results")) {
    $("upload-results").hidden = true;
    $("upload-results").replaceChildren();
  }
  closeNavigation();
  // A non-text control is focused so Ctrl+V pastes an image instead of text.
  showDialog("upload-dialog", "upload-paste");
  if (files)
    setUploadFiles(files, $("upload-dialog").open && uploadFiles.length > 0);
}
function uploadFields() {
  const data = new FormData();
  data.append("namespace", $("upload-namespace").value.trim());
  data.append("directory", $("upload-directory").value.trim());
  if ($("upload-title").value.trim())
    data.append("title", $("upload-title").value);
  data.append("tags", JSON.stringify(list($("upload-tags").value)));
  data.append(
    "collections",
    JSON.stringify(list($("upload-collections").value)),
  );
  return data;
}
function uploadRequest(data, files, sequence) {
  $("upload-progress-area").hidden = false;
  $("upload-progress").value = 0;
  return new Promise((resolve, reject) => {
    const request = new XMLHttpRequest();
    request.open("POST", apiURL("upload", view.kind === "library" ? viewPrefix : `/w/${firstUsableMember().id}`));
    request.setRequestHeader("X-Touko-CSRF", view.csrf);
    request.upload.onprogress = ({ lengthComputable, loaded, total }) => {
      if (sequence !== uploadSequence || !lengthComputable) return;
      $("upload-progress").value = Math.round((loaded / total) * 100);
      $("upload-progress-text").textContent =
        loaded === total
          ? "正在校验并保存到本机…"
          : `上传至本机 ${Math.round((loaded / total) * 100)}%`;
      $("upload-progress-text").textContent += files.length ? "" : "";
    };
    request.onload = () => {
      let body;
      try {
        body = JSON.parse(request.responseText);
      } catch {
        reject(new Error("本地服务响应无效，请检查终端后刷新确认。"));
        return;
      }
      if ((request.status >= 200 && request.status < 300) || request.status === 207) resolve(body);
      else reject(responseError(body, request.status));
    };
    request.onerror = () =>
      reject(new Error("连接中断，请刷新确认暂存状态后再继续。请先刷新核对暂存，不要自动重发整批。"));
    request.onabort = () =>
      reject(new Error("上传已取消，请刷新确认当前状态。"));
    request.send(data);
  });
}
function upload() {
  const files = uploadFiles.map(({ file }) => file);
  const data = uploadFields();
  files.forEach((file) => data.append("files", file, file.name));
  const sequence = ++uploadSequence;
  return uploadRequest(data, files, sequence).then((body) => {
    if (sequence !== uploadSequence) return body;
    if (view.kind !== "library") return body;
    // A library answers per input index: keep only the inputs that failed, so a
    // retry never re-sends images that are already staged.
    const failedIndexes = new Set((body.failed || []).map((item) => item.index));
    const failed = body.failed || [];
    const results = body.results || [];
    if ($("upload-results")) {
      $("upload-results").hidden = !failed.length;
      $("upload-results").replaceChildren();
      for (const row of results) {
        const item = element("div", "member-result ok");
        item.append(
          element("strong", "", `${files[row.index]?.name || row.index}`),
          element("span", "muted", `→ ${memberName(row.workspace)}`),
        );
        $("upload-results").append(item);
      }
      for (const row of failed) {
        const item = element("div", "member-result error");
        item.append(
          element("strong", "", `${files[row.index]?.name || row.index}`),
          element("span", "muted", row.message || row.code),
        );
        $("upload-results").append(item);
      }
    }
    if (failedIndexes.size) {
      const kept = uploadFiles.filter((_, index) => failedIndexes.has(index));
      uploadFiles.forEach(({ url }) => URL.revokeObjectURL(url));
      uploadFiles = kept.map(({ file }) => ({ file, url: URL.createObjectURL(file) }));
      renderUploadFiles();
      $("upload-dialog").open;
      return body;
    }
    return body;
  });
}
function pasteErrorIsPermanent(error) {
  return /no image|no local file|not supported|denied|permission/i.test(
    error?.message || "",
  );
}
// MyGo injects one runtime as window.mygo; bound services are reached by name
// through mygo.call, not as globals. A browser simply has no runtime.
function desktopBridge() {
  const runtime = window.mygo;
  if (!runtime || typeof runtime.call !== "function") return null;
  return {
    Read: () => runtime.call("DesktopClipboard.Read"),
    Release: (token) => runtime.call("DesktopClipboard.Release", token ?? ""),
    WriteText: (text) => runtime.call("DesktopClipboard.WriteText", text),
  };
}
function supportedClipboardType(type) {
  return ["image/png", "image/jpeg", "image/gif"].includes(type);
}
// pasteUpload appends successfully read clipboard images to the existing queue
// and leaves every previously queued file untouched. The capability URLs are
// released in `finally`, so a failed fetch cannot leave snapshots behind.
async function pasteUpload() {
  if (busy || clipboardBusy || !ready()) return;
  const bridge = desktopBridge();
  clearError("upload-error");
  clipboardBusy = true;
  updateActions();
  const sequence = refreshSequence;
  try {
    if (bridge) {
      const batch = await bridge.Read();
      const files = [];
      for (const entry of batch.files || []) {
        const response = await fetch(entry.url);
        if (!response.ok)
          throw new Error(`剪贴板图片读取失败（HTTP ${response.status}）`);
        const blob = await response.blob();
        files.push(new File([blob], entry.name, { type: entry.type || blob.type }));
      }
      if (sequence !== refreshSequence) return;
      if (!files.length) throw new Error("剪贴板里没有可用的图片或文件。");
      setUploadFiles(files, true);
      return;
    }
    if (!navigator.clipboard?.read)
      throw new Error(
        "此浏览器不支持读取剪贴板图片。请使用「选择文件」、拖放，或在桌面版中粘贴。",
      );
    const items = await navigator.clipboard.read();
    const files = [];
    for (const item of items) {
      const type = item.types.find(supportedClipboardType);
      if (!type) continue;
      const blob = await item.getType(type);
      const extension = type === "image/png" ? "png" : type === "image/jpeg" ? "jpg" : "gif";
      files.push(new File([blob], `clipboard.${extension}`, { type }));
    }
    if (!files.length)
      throw new Error(
        "剪贴板里没有 PNG、JPEG 或 GIF 图片。浏览器无法读取本地文件路径，请改用「选择文件」或拖放。",
      );
    if (sequence !== refreshSequence) return;
    setUploadFiles(files, true);
  } catch (error) {
    showError("upload-error", error);
  } finally {
    clipboardBusy = false;
    updateActions();
    const bridgeAfter = desktopBridge();
    if (bridgeAfter)
      try {
        await bridgeAfter.Release("");
      } catch {
        // Releasing a batch that was already gone is not a user-facing failure.
      }
  }
}
async function importFiles(files) {
  for (const file of files) {
    if (!supportedClipboardType(file.type)) continue;
    setUploadFiles([file], true);
  }
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
  if (!$("repo-type")) return;
  const github = $("repo-type").value === "github";
  const mode = $("repo-mode")?.value || "single";
  const libraryMode = mode === "library" || mode === "add";
  $("repo-path").disabled = busy || github;
  $("repo-branch").disabled = busy || github;
  $("repo-choices").disabled = busy || !github || repositoriesLoading || !repositories.length;
  $("repo-branches").disabled = busy || !github || repositoriesLoading || branchesLoading || !branches.length;
  $("check-auth").disabled = busy || repositoriesLoading;
  if ($("library-fields")) $("library-fields").hidden = !libraryMode;
  if ($("single-repo-fields")) $("single-repo-fields").hidden = libraryMode;
  renderLibraryChoices();
  $("repo-connect").disabled =
    busy ||
    (github && (libraryMode
      ? ![...document.querySelectorAll("#library-repos input:checked")].length
      : !validGitHubSelection()));
  $("github-fields").setAttribute("aria-busy", String(repositoriesLoading || branchesLoading));
}
// renderLibraryChoices keeps one row per accessible repository with its own
// branch select. Rows are rebuilt only when the repository list changes, so a
// branch choice made by the user survives an unrelated re-render.
function renderLibraryChoices() {
  const host = $("library-repos");
  if (!host || !repositories.length) {
    if (host) host.replaceChildren(element("p", "field-help", repositoriesLoading ? "正在读取仓库…" : "读取仓库后可以勾选成员。"));
    return;
  }
  // Members already in this library are shown checked with their real branch, so
  // the dialog reflects the configuration instead of an empty list.
  const members = new Map();
  for (const member of view?.members || []) {
    const location = member.location;
    if (location?.backend === "github")
      members.set(`${location.owner}/${location.name}`, location.branch);
  }
  const signature = `${repositories.length}:${repositories[0]?.full_name}:${[...members].sort().join(",")}`;
  if (host.dataset.signature === signature) return;
  host.dataset.signature = signature;
  host.replaceChildren();
  repositories.forEach((repo, index) => {
    const row = element("div", "library-repo-row");
    const checkbox = element("input");
    checkbox.type = "checkbox";
    checkbox.id = `library-repo-${index}`;
    checkbox.value = repo.full_name;
    const joined = members.get(repo.full_name);
    checkbox.checked = joined !== undefined;
    const label = element("label", "", `${repo.full_name}${repo.private ? " · 私有" : ""}${repo.can_push ? "" : " · 只读"}`);
    label.setAttribute("for", checkbox.id);
    const select = element("select");
    select.dataset.repository = repo.full_name;
    select.disabled = !checkbox.checked;
    select.append(new Option("默认分支", ""));
    if (joined !== undefined) {
      select.replaceChildren(new Option(joined, joined));
      select.value = joined;
      select.dataset.loaded = "true";
    }
    // A member row states membership in words; a checked box alone is easy to
    // miss in a long repository list.
    if (joined !== undefined) row.classList.add("is-member");
    select.addEventListener("focus", () => loadLibraryBranches(repo.full_name, select));
    checkbox.addEventListener("change", () => {
      select.disabled = !checkbox.checked || !!select.dataset.loaded;
      if (checkbox.checked) void loadLibraryBranches(repo.full_name, select);
      updateRepositoryControls();
    });
    row.append(checkbox, label, select);
    host.append(row);
  });
}
async function loadLibraryBranches(repository, select) {
  if (select.dataset.loading === "true") return;
  select.dataset.loading = "true";
  try {
    const value = await api(`branches?repository=${encodeURIComponent(repository)}`);
    const repo = repositories.find((item) => item.full_name === repository);
    select.replaceChildren(
      ...value.branches.map((branch) => new Option(
        `${branch.name}${branch.name === repo?.default_branch ? " · 默认" : ""}`,
        branch.name,
      )),
    );
    select.value = repo?.default_branch && value.branches.some((branch) => branch.name === repo.default_branch)
      ? repo.default_branch
      : value.branches[0]?.name || "";
    if (!select.dataset.loaded || select.dataset.loaded === "true") {
      const previous = select.value;
      if (previous && value.branches.some((branch) => branch.name === previous)) select.value = previous;
    }
    select.dataset.loaded = "true";
    select.disabled = !select.closest(".library-repo-row").querySelector("input").checked;
  } catch (error) {
    select.replaceChildren(new Option("分支读取失败", ""));
    select.dataset.loaded = "error";
    showError("repo-error", error);
  } finally {
    select.dataset.loading = "false";
  }
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
    $("repo-branches").value = [preferredBranch, repo.default_branch, branches[0].name]
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
  $("library-repos").replaceChildren(element("p", "field-help", "正在读取仓库…"));
  $("library-repos").dataset.signature = "";
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
    $("library-repos").dataset.signature = "";
    updateRepositoryControls();
    const location = firstUsableMember()?.location;
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
  $("library-repos").replaceChildren(element("p", "field-help", "读取仓库后可以勾选成员。"));
  $("library-repos").dataset.signature = "";
  resetBranchChoices("请先选择仓库");
  if ($("repo-mode")) $("repo-mode").value = view?.kind === "library" ? "add" : "single";
  const location = firstUsableMember()?.location;
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
    $("library-nav").replaceChildren();
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
  try {
    const value = await api("libraries");
    if (session !== repositoryDialogSequence || !$("repo-dialog").open) return;
    $("library-nav").replaceChildren();
    for (const library of value.libraries) {
      const row = element("div", "library-entry");
      const current = view?.kind === "library" && view.library?.id === library.id;
      const link = element("a", "workspace-link", library.name);
      link.href = `/l/${library.id}/`;
      link.append(element("span", "muted", `${library.member_count} 个仓库`));
      if (current) {
        link.setAttribute("aria-current", "page");
        link.append(element("span", "muted", "当前图库"));
      }
      link.addEventListener("click", (event) => {
        if (busy) event.preventDefault();
      });
      row.append(link);
      // The per-member list is what makes a library manageable: each repository
      // can be opened on its own, with its own staging and publication.
      const detail = element("div", "library-members");
      detail.append(element("small", "muted", "正在读取成员…"));
      row.append(detail);
      $("library-nav").append(row);
      void loadLibraryMembers(library.id, detail, session);
    }
    if (!value.libraries.length)
      $("library-nav").append(element("p", "sidebar-hint", "连接多个仓库后，会在这里合并成一个图库。"));
  } catch (error) {
    if (session !== repositoryDialogSequence || !$("repo-dialog").open) return;
    $("library-nav").replaceChildren(element("p", "sidebar-hint", `图库列表读取失败：${error.message}`));
  }
}
// loadLibraryMembers fills one library's member list. It is read-only: opening a
// member only navigates, so a mis-click cannot change a repository's state.
async function loadLibraryMembers(id, host, session) {
  try {
    const detail = await api("detail", undefined, `/l/${id}`);
    if (session !== repositoryDialogSequence || !$("repo-dialog").open) return;
    host.replaceChildren();
    for (const member of detail.members || []) {
      const location = member.location;
      const name = location
        ? location.backend === "github"
          ? `${location.owner}/${location.name} · ${location.branch}`
          : `${location.path} · ${location.branch}`
        : "未连接";
      const item = element("a", "workspace-link member", name);
      item.href = member.url;
      if (member.error) item.append(element("span", "muted", member.error));
      host.append(item);
    }
    if (!(detail.members || []).length)
      host.append(element("small", "muted", "这个图库还没有成员。"));
  } catch (error) {
    if (session !== repositoryDialogSequence) return;
    host.replaceChildren(element("small", "muted", `成员读取失败：${error.message}`));
  }
}
function updateRepositoryFields() {
  const type = $("repo-type").value;
  const github = type === "github";
  const mode = $("repo-mode")?.value || "single";
  const libraryMode = github && (mode === "library" || mode === "add");
  $("local-fields").hidden = github;
  $("github-fields").hidden = !github;
  $("library-fields").hidden = !libraryMode;
  $("single-repo-fields").hidden = libraryMode;
  $("repo-path").required = !github;
  $("repo-choices").required = github && !libraryMode;
  $("repo-branches").required = github && !libraryMode;
  $("library-name").required = github && mode === "library";
  if (mode === "add" && view?.kind === "library")
    $("library-name").value = view.library?.name || "";
  $("local-path-help").textContent =
    type === "local-create"
      ? "填写一个尚不存在或为空的目录，将创建 bare Git 仓库。"
      : "填写已有 Git 仓库的绝对路径，不会覆盖原内容。";
  updateRepositoryControls();
}
function librarySelection() {
  return [...document.querySelectorAll("#library-repos input:checked")].map((checkbox) => {
    const select = checkbox.closest(".library-repo-row").querySelector("select");
    return { repository: checkbox.value, branch: select.value };
  });
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
  if (busy || !view) return;
  if (!firstUsableMember()) {
    void openRepository();
    return;
  }
  const initialized = view.members.filter((member) => member.location && member.initialized);
  $("import-initialization").hidden = initialized.length > 0;
  $("import-result").hidden = true;
  clearError("import-error");
  // Repository-level import always names its member: scanning never spreads one
  // repository's files across the library automatically.
  $("import-member-field").hidden = initialized.length < 2;
  $("import-member").replaceChildren(
    ...initialized.map((member) => new Option(memberName(member.id), member.id)),
  );
  closeNavigation();
  showDialog("import-dialog", "import-directory");
}
function selectedImportMember() {
  const id = $("import-member").value;
  return memberByID(id) || view.members.find((member) => member.initialized) || null;
}
// deliveryText resolves the delivery URL once and returns the exact text plus
// whether it is committed, so every copy path shares the same escaping rules.
async function deliveryText(markdown, key = currentKey) {
  const entry = viewForKey(key);
  if (!entry) throw new Error("图片已不在当前图库，请刷新后重试。");
  const member = memberForAsset(entry);
  if (!member) throw new Error("找不到图片所属的仓库成员。");
  const provider = $("url-provider").value;
  const value = await apiMember(member, `assets/${entry.asset.id}/url?provider=${provider}`);
  const alt = (entry.asset.title || entry.asset.original_name)
    .replace(/[\r\n\u2028\u2029]+/g, " ")
    .replace(/[\\[\]`*_<>]/g, "\\$&");
  const target = value.url.replace(/</g, "%3C").replace(/>/g, "%3E").replace(/[\r\n]/g, "");
  return {
    text: markdown ? `![${alt}](<${target}>)` : value.url,
    committed: value.committed,
  };
}
// copyReference copies one delivery reference. ClipboardItem is created
// synchronously with a promise payload, so the browser still sees the user's
// gesture while the link is requested; the desktop bridge has no such rule.
async function copyReference(markdown, key = currentKey) {
  const bridge = desktopBridge();
  if (bridge) {
    const delivered = await deliveryText(markdown, key);
    await bridge.WriteText(delivered.text);
    return delivered;
  }
  if (window.ClipboardItem && navigator.clipboard?.write) {
    let delivered = null;
    const pending = deliveryText(markdown, key).then((value) => {
      delivered = value;
      return value.text;
    });
    await navigator.clipboard.write([
      new ClipboardItem({
        "text/plain": pending.then((text) => new Blob([text], { type: "text/plain" })),
      }),
    ]);
    await pending;
    return delivered;
  }
  const delivered = await deliveryText(markdown, key);
  await navigator.clipboard.writeText(delivered.text);
  return delivered;
}
async function copyDelivery(markdown) {
  clearError("edit-error");
  try {
    const delivered = await copyReference(markdown);
    $("delivery-url").hidden = false;
    $("delivery-url").value = delivered.text;
    $("delivery-url").setAttribute("aria-label", markdown ? "图片 Markdown" : "图片公开链接");
    $("delivery-note").textContent = delivered.committed
      ? "图片已提交。链接跟随分支，CDN 传播可能有延迟。"
      : "还没发布：这个地址要等提交之后才能访问。";
    toast(delivered.committed
      ? `已复制${markdown ? " Markdown" : "图片链接"}`
      : "已复制，但图片还没发布，提交后才能访问。");
  } catch (error) {
    // A failed request or a rejected clipboard write must never claim success.
    try {
      const delivered = await deliveryText(markdown);
      $("delivery-url").hidden = false;
      $("delivery-url").value = delivered.text;
      $("delivery-url").focus();
      $("delivery-url").select();
    } catch {
      // Keep the original error: it explains why nothing could be copied.
    }
    showError("edit-error", error);
  }
}
// memberOperation runs one action per member in order and reports each result.
// A failing member keeps its own staging and error; successful members stay done.
async function memberOperation(action, members, options = {}) {
  const results = [];
  const failed = [];
  for (const member of members) {
    try {
      if (member.error) throw new Error(member.error);
      if (options.skipUninitialized && !member.initialized)
        throw new Error("尚未初始化");
      const value = await options.run(member);
      results.push({ workspace: member.id, ok: true, message: options.describe?.(value) || "完成", value });
    } catch (error) {
      failed.push(member.id);
      results.push({ workspace: member.id, ok: false, error: error.message });
    }
  }
  operationRetry = { results, failed, action: options.action || action };
  return { results, failed };
}
async function runMemberOperation(action, options = {}) {
  const members = options.members || view.members.filter((member) => member.location);
  if (!members.length) {
    toast("还没有可操作的仓库成员。");
    return null;
  }
  clearError("page-error");
  clearError("pending-error");
  setBusy(true);
  let value;
  try {
    value = await memberOperation(action, members, options);
  } finally {
    try {
      await refreshState();
    } catch (error) {
      showError("page-error", `操作已执行，但刷新失败。请刷新确认状态。\n${error.message}`);
    }
    setBusy(false);
  }
  if (!value) return null;
  if (value.failed.length) {
    showError(
      "page-error",
      new Error(
        `${value.failed.length} 个仓库成员未完成，已在结果中列出；它们的暂存内容仍保留。请修复后只重试失败成员。`,
      ),
    );
    return value;
  }
  if (options.success) toast(options.success);
  return value;
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
      if (dialog.id === "upload-dialog") {
        uploadSequence++;
        releaseUploadFiles();
      }
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
  $("upload-paste").addEventListener("click", () => void pasteUpload());
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
  $("source-filter")?.addEventListener("change", () => {
    filters.source = $("source-filter").value;
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
    for (const entry of visibleAssets()) {
      const key = keyOf(entry);
      if ($("select-visible").checked) selected.add(key);
      else selected.delete(key);
    }
    renderGallery();
  });
  $("remove-selected").addEventListener("click", () =>
    removeAssets([...selected]),
  );
  $("remove-current").addEventListener("click", () =>
    removeAssets([currentKey]),
  );
  $("refresh-state").addEventListener("click", async () => {
    if (busy) return;
    try {
      await refreshState();
      toast("图库已刷新");
    } catch (error) {
      showError("page-error", error);
    }
  });
  $("sync").addEventListener("click", () =>
    runMemberOperation("sync", {
      run: (member) => apiMember(member, "sync", {}),
      success: "已同步全部成员",
      describe: (value) => `HEAD ${String(value.head || "").slice(0, 8) || "已更新"}`,
    }),
  );
  $("reindex").addEventListener("click", () =>
    runMemberOperation("reindex", {
      run: (member) => apiMember(member, "reindex", {}),
      success: "已从各仓库固定快照重建索引，未发布修改保留",
      skipUninitialized: true,
    }),
  );
  $("import-form").addEventListener("submit", async (event) => {
    event.preventDefault();
    const member = selectedImportMember();
    if (!member) return;
    $("import-result").hidden = true;
    clearError("import-error");
    setBusy(true);
    try {
      const value = await apiMember(member, "import", {
        directory: $("import-directory").value.trim(),
        namespace: $("import-namespace").value.trim(),
        tags: list($("import-tags").value),
        collections: list($("import-collections").value),
      });
      $("import-result").textContent = `${memberName(member.id)}：${value.imported} 张已暂存，${value.skipped} 张已跳过。图片保留原仓库路径；尚未发布，必须在「待发布」中提交才会写入管理元数据。`;
      $("import-result").hidden = false;
      await refreshState();
    } catch (error) {
      showError("import-error", error);
    } finally {
      setBusy(false);
    }
  });
  $("setup-action").addEventListener("click", async () => {
    if (!view?.members.some((member) => member.location)) {
      openRepository();
      return;
    }
    await runMemberOperation("init", {
      members: view.members.filter((member) => member.location && !member.initialized),
      run: (member) => apiMember(member, "init", {}),
      success: "TOUKO 仓库已就绪",
    });
  });
  $("verify").addEventListener("click", async () => {
    const value = await runMemberOperation("verify", {
      run: (member) => apiMember(member, "verify", {}),
      skipUninitialized: true,
    });
    if (!value) return;
    const rows = value.results.map((row) => [
      memberName(row.workspace),
      row.ok ? `${row.value.assets} 张图 · ${row.value.objects} 个对象 · HEAD ${String(row.value.head || "").slice(0, 8)}` : row.error,
    ]);
    result(
      value.failed.length ? "部分成员校验失败" : "全部成员校验通过",
      rows.length ? rows : [["结果", "没有可校验的成员"]],
      "校验的是每个仓库后端固定 HEAD 上的完整元数据与图片文件，不包含还没发布的修改；不会自动获取远端更新，请先按需同步。",
    );
  });
  $("preview-image").addEventListener("error", () => {
    $("preview-image").hidden = true;
    $("preview-image-error").hidden = false;
  });
  $("edit-form").addEventListener("submit", async (event) => {
    event.preventDefault();
    const entry = viewForKey(currentKey);
    const member = entry ? memberForAsset(entry) : null;
    if (!member) return;
    const key = currentKey;
    const value = await mutate(
      () =>
        apiMember(member, `assets/${entry.asset.id}/edit`, {
          title: $("edit-title").value,
          tags: list($("edit-tags").value),
          collections: list($("edit-collections").value),
        }),
      "edit-error",
      "修改已保存到待发布区",
    );
    if (value) openPreview(key);
  });
  $("copy-url").addEventListener("click", () => copyDelivery(false));
  $("copy-markdown").addEventListener("click", () => copyDelivery(true));
  $("upload-files").addEventListener("change", () => {
    // Appending keeps files pasted earlier; clearing value allows picking the
    // same file again after it has been removed.
    setUploadFiles([...$("upload-files").files], true);
    $("upload-files").value = "";
  });
  $("upload-form").addEventListener("submit", async (event) => {
    event.preventDefault();
    if (!uploadFiles.length) return;
    const value = await mutate(
      upload,
      "upload-error",
      view.kind === "library" ? "图片已加入对应成员的待发布区" : "图片已加入待发布区，还没有写入仓库",
    );
    if (value) {
      if (value.failed?.length) {
        toast(`${value.failed.length} 张未能加入待发布区，已保留在队列中`);
      } else {
        $("upload-dialog").close();
      }
    }
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
  document.addEventListener("paste", (event) => {
    const files = [...(event.clipboardData?.files || [])];
    const fromItems = [...(event.clipboardData?.items || [])]
      .filter((item) => item.kind === "file")
      .map((item) => item.getAsFile())
      .filter(Boolean);
    const candidates = files.length ? files : fromItems;
    if (!candidates.length) {
      // A URI-only clipboard has no file bytes; never request an arbitrary path.
      if ($("upload-dialog").open && event.clipboardData?.getData("text/uri-list"))
        showError(
          "upload-error",
          "剪贴板里只有本地文件路径，浏览器无法读取。请使用「选择文件」或拖放，或改用桌面版粘贴。",
        );
      return;
    }
    if (!$("upload-dialog").open) {
      openUpload(candidates.filter((file) => supportedClipboardType(file.type)));
      return;
    }
    event.preventDefault();
    void importFiles(candidates);
  });
  document.addEventListener("keydown", (event) => {
    const pasteRequest =
      (event.ctrlKey || event.metaKey) && event.key.toLowerCase() === "v";
    if (pasteRequest && $("upload-dialog").open && !busy && !clipboardBusy) {
      const active = document.activeElement;
      const typing =
        ["INPUT", "TEXTAREA", "SELECT"].includes(active?.tagName) ||
        active?.isContentEditable;
      if (!typing) {
        event.preventDefault();
        void pasteUpload();
        return;
      }
    }
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
  $("commit-form").addEventListener("submit", async (event) => {
    event.preventDefault();
    const message = $("commit-message").value.trim();
    if (!message) return;
    const pending = view.members.filter((member) =>
      view.changes.some((entry) => entry.workspace === member.id) || member.publication,
    );
    const value = await runMemberOperation("commit", {
      members: pending,
      run: (member) => apiMember(member, "commit", { message }),
      success: "有待发布修改的成员已提交",
      describe: (result) => `HEAD ${String(result.head || "").slice(0, 8)}`,
    });
    if (!value) return;
    if (!value.failed.length) {
      $("commit-message").value = "";
      $("pending-dialog").close();
    }
  });
  $("retry-operation").addEventListener("click", async () => {
    if (!operationRetry || busy) return;
    const action = operationRetry.action;
    if (action === "remove") {
      const groups = operationRetry.selection;
      const failed = operationRetry.failed;
      const value = await runMemberOperation("remove", {
        members: failed.map(memberByID).filter(Boolean),
        run: async (member) => apiMember(member, "remove", { selectors: groups.get(member.id) }),
      });
      if (value && !value.failed.length) operationRetry = null;
      renderOperationResults();
      return;
    }
    const members = operationRetry.failed.map(memberByID).filter(Boolean);
    const message = $("commit-message").value.trim();
    const value = await runMemberOperation(action, {
      members,
      run: (member) =>
        action === "commit"
          ? apiMember(member, "commit", { message })
          : apiMember(member, action, {}),
    });
    if (value && !value.failed.length) operationRetry = null;
    renderOperationResults();
  });
  $("dismiss-operation")?.addEventListener("click", () => {
    operationRetry = null;
    renderOperationResults();
  });
  $("reset-staged").addEventListener("click", () =>
    confirm(
      "丢弃全部未发布修改？",
      `待发布区里的新增、编辑和移除都会从 ${view.members.length} 个成员中丢弃，而且不能撤销。仓库里已经发布过的历史不会改变。\n如果有还没确认的发布，服务会先核实远端结果。`,
      async () => {
        const value = await runMemberOperation("reset", {
          run: (member) => apiMember(member, "reset", { confirm: true }),
          success: "待发布修改已清空",
        });
        if (value && !value.failed.length) {
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
  $("repo-mode").addEventListener("change", () => {
    clearError("repo-error");
    updateRepositoryFields();
  });
  $("repo-form").addEventListener("submit", async (event) => {
    event.preventDefault();
    const type = $("repo-type").value;
    const mode = $("repo-mode")?.value || "single";
    if (busy) return;
    if (type === "github" && (mode === "library" || mode === "add")) {
      const repositoriesChosen = librarySelection();
      if (!repositoriesChosen.length) {
        showError("repo-error", new Error("请至少勾选一个仓库成员。"));
        return;
      }
      if (repositoriesChosen.some((choice) => !choice.branch)) {
        showError("repo-error", new Error("请为每个勾选的仓库选择分支。"));
        return;
      }
      const name = $("library-name").value.trim();
      if (mode === "library" && !name) {
        showError("repo-error", new Error("请填写图库名称。"));
        return;
      }
      clearError("repo-error");
      setBusy(true);
      try {
        const value =
          mode === "library"
            ? await api("libraries", { name, repositories: repositoriesChosen })
            : await apiMember(
                { id: view.library?.id || "", csrf: view.csrf },
                "members",
                { repositories: repositoriesChosen },
              );
        if (mode === "library") {
          window.location.assign(value.url);
          return;
        }
        await refreshState();
        $("repo-dialog").close();
        toast(`已连接 ${value.added?.length || 0} 个新成员`);
        setBusy(false);
      } catch (error) {
        showError("repo-error", error);
        setBusy(false);
      }
      return;
    }
    if (type === "github" && !validGitHubSelection()) return;
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
}

bindEvents();
document.querySelector(".brand").href = `${viewPrefix}/`;
try {
  await refreshState();
} catch (error) {
  showError("page-error", error);
  $("gallery").replaceChildren(
    element("div", "empty-state", "图库未能读取。请按上方提示处理后点击刷新。"),
  );
  $("gallery").setAttribute("aria-busy", "false");
}
