// CLink v1.3 前端交互（苹果风深色 UI · 悬浮球 · 模组仓库 · 工具箱 · 信令节点选择）
/* global window, document */
(() => {
  const $ = (id) => document.getElementById(id);

  // ---- Wails 桥 ----
  const app = () => (window.go && window.go.main && window.go.main.App) || null;
  const call = (method, ...args) => {
    const a = app();
    if (!a || typeof a[method] !== "function") {
      return Promise.reject(new Error("请通过 CLink 程序打开本窗口（不要用浏览器直接打开）"));
    }
    return a[method](...args);
  };
  const on = (name, cb) => {
    if (window.runtime && window.runtime.EventsOn) window.runtime.EventsOn(name, cb);
  };

  // ---- 通用 ----
  let busy = false;
  let ballMode = false;
  let currentView = "home";
  let currentRoomCode = "";

  const fmtBytes = (n) => {
    if (n == null || n < 0) return "?";
    if (n >= 1 << 30) return (n / (1 << 30)).toFixed(1) + " GB";
    if (n >= 1 << 20) return (n / (1 << 20)).toFixed(1) + " MB";
    if (n >= 1 << 10) return (n / (1 << 10)).toFixed(1) + " KB";
    return n + " B";
  };

  const toast = (msg, cls = "", ms = 3200) => {
    const el = document.createElement("div");
    el.className = "toast " + cls;
    el.textContent = msg;
    $("toasts").appendChild(el);
    setTimeout(() => el.remove(), ms);
  };

  const copyText = async (t, tip) => {
    try {
      if (window.runtime && window.runtime.ClipboardSetText) {
        await window.runtime.ClipboardSetText(t);
      } else {
        await navigator.clipboard.writeText(t);
      }
      toast(tip || ("已复制：" + t), "ok", 2200);
    } catch (e) {
      toast("复制失败", "err");
    }
  };

  const setStatus = (id, msg, cls = "") => {
    const el = $(id);
    el.textContent = msg;
    el.className = "status " + cls;
  };

  // ---- 视图切换 ----
  document.querySelectorAll(".nav-btn").forEach((btn) => {
    btn.addEventListener("click", () => {
      document.querySelectorAll(".nav-btn").forEach((b) => b.classList.remove("active"));
      btn.classList.add("active");
      document.querySelectorAll(".view").forEach((v) => v.classList.remove("active"));
      currentView = btn.dataset.view;
      $("view-" + currentView).classList.add("active");
      if (currentView === "mods") refreshQueue();
    });
  });

  // ---- 标题栏 ----
  $("tl-min").addEventListener("click", () => window.runtime && window.runtime.WindowMinimise());
  $("tl-ball").addEventListener("click", () => call("EnterBallMode").catch((e) => toast(e.message, "err")));
  $("tl-close").addEventListener("click", () => call("EnterBallMode").catch((e) => toast(e.message, "err")));

  // ================= 主页：联机 =================

  // 端口自动捕获
  on("clink:mcport", (port) => {
    if (port > 0 && !$("mc-port").value) {
      $("mc-port").value = port;
      setStatus("host-status", "检测到游戏端口 " + port + "，点「创建房间」就行", "ok");
    }
  });

  $("btn-create").addEventListener("click", async () => {
    if (busy) return;
    const port = parseInt($("mc-port").value || "25565", 10);
    if (!port || port <= 0 || port > 65535) return setStatus("host-status", "端口要在 1-65535 之间", "warn");
    busy = true;
    $("btn-create").disabled = true;
    setStatus("host-status", "正在创建房间…");
    try {
      const code = await call("CreateRoom", port);
      currentRoomCode = code;
      $("room-code").textContent = code;
      $("room-box").classList.remove("hidden");
      setStatus("host-status", "房间已开好！把房间码发给小伙伴，等他们加入（可缩小成悬浮球挂着）", "ok");
      await copyText(code, "房间码已复制，快发给小伙伴！");
      // 房间码二维码
      call("TbQRCode", code).then((url) => { $("room-qr").src = url; }).catch(() => {});
    } catch (e) {
      setStatus("host-status", "创建失败：" + e.message, "warn");
    } finally {
      busy = false;
      $("btn-create").disabled = false;
    }
  });

  $("btn-copy-code").addEventListener("click", () => copyText($("room-code").textContent, "房间码已复制"));

  // 一键复制邀请文案（playit.gg 式：带上房间码 + 下载链接，直接发给朋友）
  $("btn-copy-invite").addEventListener("click", () => {
    const code = $("room-code").textContent || currentRoomCode;
    if (!code || code.includes("·")) return toast("先创建房间", "err");
    copyText(
      "来一起玩 Minecraft！打开 CLink 输入房间码 " + code + " 就能进（复制这个码就行）。\n" +
      "没装 CLink？下载：https://clink.541887.xyz",
      "邀请文案已复制，发给朋友就行！"
    );
  });

  // ---- 最近房间（快速重连）----
  async function renderRecentRooms() {
    try {
      const list = await call("RecentRooms");
      const box = $("recent-rooms");
      const ul = $("recent-list");
      ul.innerHTML = "";
      if (!list || !list.length) {
        box.classList.add("hidden");
        return;
      }
      list.slice(0, 6).forEach((code) => {
        const b = document.createElement("button");
        b.className = "recent-chip";
        b.textContent = code;
        b.title = "点击填入房间码";
        b.onclick = () => {
          $("join-code").value = code;
          $("join-code").focus();
        };
        ul.appendChild(b);
      });
      box.classList.remove("hidden");
    } catch (e) { /* 浏览器调试静默 */ }
  }
  renderRecentRooms();

  let prewarmTimer = null;
  $("join-code").addEventListener("input", () => {
    // ROADMAP #60：敲房间码时后台预热（NAT 分类 + STUN 缓存）
    if ($("join-code").value.trim().length >= 4 && !prewarmTimer) {
      prewarmTimer = setTimeout(() => {
        prewarmTimer = null;
        call("Prewarm").catch(() => {});
      }, 500);
    }
  });

  $("btn-join").addEventListener("click", async () => {
    if (busy) return;
    const code = $("join-code").value.trim().toLowerCase();
    const name = ($("set-name").value || "").trim();
    if (code.length !== 6) return setStatus("guest-status", "房间码是 6 位，数一数是不是漏了", "warn");
    busy = true;
    $("btn-join").disabled = true;
    setStatus("guest-status", "正在加入房间…（最多等 45 秒）");
    try {
      await call("JoinRoom", code, name);
    } catch (e) {
      setStatus("guest-status", "加入失败：" + e.message, "warn");
      busy = false;
      $("btn-join").disabled = false;
    }
  });

  $("btn-leave-host").addEventListener("click", () => call("Leave").then(resetHome));
  $("btn-leave-guest").addEventListener("click", () => call("Leave").then(resetHome));
  $("btn-copy-addr").addEventListener("click", () => copyText($("local-addr").textContent, "地址已复制，去游戏里粘贴"));

  function resetHome() {
    $("room-box").classList.add("hidden");
    $("joined-box").classList.add("hidden");
    $("room-code").textContent = "· · · · · ·";
    currentRoomCode = "";
    setStatus("host-status", "先开单人世界 → Esc →「对局域网开放」→ 回来点「创建房间」");
    setStatus("guest-status", "加入成功后会自动复制服务器地址，进游戏直接连");
    busy = false;
    $("btn-join").disabled = false;
    renderMembers([]);
    updateBadge(0);
    renderRecentRooms();
  }

  // ---- 会话事件流 ----
  on("clink:event", (e) => {
    if (!e || !e.stage) return;
    if (typeof e.guests === "number" && e.guests > 0) updateBadge(e.guests);
    switch (e.stage) {
      case "guest-joined":
        toast(e.msg, "ok");
        if (currentView === "home") setStatus("host-status", e.msg + "（共 " + e.guests + " 人）", "ok");
        break;
      case "guest-left":
        toast("👋 " + e.msg, "");
        if (currentView === "home") setStatus("host-status", e.msg + "（当前 " + e.guests + " 人）", "");
        break;
      case "guest-kicked":
      case "guest-rejected":
        toast(e.msg, e.stage === "guest-kicked" ? "" : "err");
        break;
      case "connected": {
        $("joined-box").classList.remove("hidden");
        if (e.addr) $("local-addr").textContent = e.addr;
        // 连接质量徽标（借鉴主流 P2P 应用：方式 + 延迟一目了然）
        const tierName = e.tier === "ipv6" ? "IPv6 极速直连" : "P2P 打洞";
        const rtt = e.rtt && e.rtt > 0 ? " · 延迟 " + e.rtt + "ms" : "";
        $("conn-quality").textContent = "（" + tierName + rtt + "）";
        setStatus("guest-status", "连接成功！已自动复制服务器地址", "ok");
        busy = false;
        $("btn-join").disabled = false;
        renderRecentRooms();
        break;
      }
      case "reconnecting":
        if (ballMode) break;
        setStatus("guest-status", "⚠️ 连接抖了一下，正在自动重连…（游戏里可能卡几秒）", "warn");
        break;
      case "error":
        if (ballMode) break;
        setStatus("guest-status", e.msg, "warn");
        setStatus("host-status", e.msg, "warn");
        // 终态错误（房间不存在/加入失败）恢复按钮，避免卡死在"正在加入"
        busy = false;
        $("btn-join").disabled = false;
        break;
      case "info":
        if (!ballMode) setStatus("host-status", e.msg);
        break;
      case "waiting":
        if (!ballMode) setStatus("host-status", "房间已挂出，等待小伙伴加入…（悬浮球可挂后台）");
        break;
    }
  });

  // ---- 成员列表 ----
  on("clink:members", renderMembers);

  function renderMembers(members) {
    const ul = $("member-list");
    ul.innerHTML = "";
    const list = members || [];
    $("member-count").textContent = list.length;
    if (!list.length) {
      ul.innerHTML = '<li class="muted-li">等待小伙伴加入…</li>';
      return;
    }
    list.forEach((name) => {
      const li = document.createElement("li");
      const av = document.createElement("span");
      av.className = "avatar";
      av.textContent = name.slice(0, 1).toUpperCase();
      const nm = document.createElement("span");
      nm.textContent = name;
      const kick = document.createElement("button");
      kick.className = "kick";
      kick.textContent = "踢出";
      kick.onclick = () => call("KickMember", name, false).catch((e) => toast(e.message, "err"));
      const ban = document.createElement("button");
      ban.className = "ban";
      ban.textContent = "拉黑";
      ban.onclick = () => call("KickMember", name, true).then(() => toast(name + " 已踢出并拉黑", "ok")).catch((e) => toast(e.message, "err"));
      li.append(av, nm, kick, ban);
      ul.appendChild(li);
    });
  }

  // ================= 悬浮球 =================
  on("clink:ball", (onBall) => {
    ballMode = !!onBall;
    $("ball").classList.toggle("hidden", !ballMode);
    $("titlebar").style.display = ballMode ? "none" : "";
    $("sidenav").style.display = ballMode ? "none" : "";
    $("content").style.display = ballMode ? "none" : "";
    if (!ballMode) $("ball-menu").classList.add("hidden");
  });

  function updateBadge(n) {
    const b = $("orb-badge");
    if (n > 0) {
      b.textContent = n;
      b.classList.remove("hidden");
    } else {
      b.classList.add("hidden");
    }
  }

  let menuOpen = false;
  $("orb").addEventListener("click", () => {
    if (menuOpen) {
      menuOpen = false;
      $("ball-menu").classList.add("hidden");
      call("ExitBallMode").catch((e) => toast(e.message, "err"));
    } else {
      // 第一次点弹菜单，选择恢复或退出
      menuOpen = true;
      $("ball-menu").classList.remove("hidden");
      setTimeout(() => {
        document.addEventListener("click", function once() {
          menuOpen = false;
          $("ball-menu").classList.add("hidden");
          document.removeEventListener("click", once);
        });
      }, 50);
    }
  });
  $("ball-restore").addEventListener("click", (ev) => {
    ev.stopPropagation();
    call("ExitBallMode").catch((e) => toast(e.message, "err"));
  });
  $("ball-quit").addEventListener("click", (ev) => {
    ev.stopPropagation();
    call("QuitApp");
  });

  // ================= 模组仓库 =================
  const GVS = ["1.21.10","1.21.9","1.21.8","1.21.7","1.21.6","1.21.5","1.21.4","1.21.1","1.21","1.20.6","1.20.4","1.20.1","1.19.4","1.19.2","1.18.2","1.16.5"];
  const gvSel = $("mod-gv");
  GVS.forEach((v) => {
    const o = document.createElement("option");
    o.value = v; o.textContent = v;
    gvSel.appendChild(o);
  });

  let modsDir = "";
  function refreshModsDirLabel() {
    call("GetNet").then((net) => {
      modsDir = (net.mcDir || "").trim();
      $("mods-dir-label").textContent = modsDir ? modsDir + "\\mods" : "（在设置里配置 MC 目录后自动填入）";
    }).catch(() => {});
  }
  refreshModsDirLabel();

  $("btn-pick-dir").addEventListener("click", async () => {
    if (!window.runtime || !window.runtime.OpenDirectoryDialog) return toast("当前环境不支持选择文件夹", "err");
    const dir = await window.runtime.OpenDirectoryDialog({ Title: "选择 mods 文件夹（或 MC 目录）" });
    if (!dir) return;
    if (!dir.toLowerCase().endsWith("mods")) {
      $("set-mcdir").value = dir;
      toast("已填入 MC 目录，记得点设置里的「保存」", "", 4000);
    } else {
      modsDir = dir.slice(0, -5);
      $("set-mcdir").value = modsDir;
      toast("已选择 mods 文件夹", "ok");
    }
    $("mods-dir-label").textContent = dir;
  });

  $("btn-mod-search").addEventListener("click", doModSearch);
  $("mod-query").addEventListener("keydown", (e) => { if (e.key === "Enter") doModSearch(); });

  async function doModSearch() {
    const q = $("mod-query").value.trim();
    if (!q) return;
    $("mod-results").innerHTML = '<div class="empty">搜索中…</div>';
    try {
      const results = await call("ModSearch", q);
      renderModResults(results);
    } catch (e) {
      $("mod-results").innerHTML = '<div class="empty">搜索失败：' + e.message + "</div>";
    }
  }

  function renderModResults(list) {
    const box = $("mod-results");
    box.innerHTML = "";
    if (!list || !list.length) {
      box.innerHTML = '<div class="empty">没找到，换个词试试（试试英文）</div>';
      return;
    }
    list.forEach((m) => {
      const item = document.createElement("div");
      item.className = "mod-item";
      item.innerHTML =
        '<img src="' + (m.icon_url || "data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg'/%3E") + '" onerror="this.style.visibility=\'hidden\'">' +
        '<div class="mi-info"><div class="mi-title"></div><div class="mi-desc"></div>' +
        '<div class="mi-meta">⬇️ ' + fmtBytes(0) + '  ·  ' + (m.categories || []).slice(0, 3).join(" / ") + "</div></div>";
      item.querySelector(".mi-title").textContent = m.title;
      item.querySelector(".mi-desc").textContent = m.desc || "";
      item.querySelector(".mi-meta").textContent = "⬇️ " + m.downloads.toLocaleString() + " 次下载" +
        ((m.categories || []).length ? " · " + m.categories.slice(0, 3).join(" / ") : "");
      const btn = document.createElement("button");
      btn.className = "btn primary sm mi-dl-btn";
      btn.textContent = "选版本";
      btn.onclick = () => pickVersion(m, item);
      item.appendChild(btn);
      box.appendChild(item);
    });
  }

  async function pickVersion(m, item) {
    // 移除其他展开的版本面板
    document.querySelectorAll(".versions-pop").forEach((p) => p.remove());
    const pop = document.createElement("div");
    pop.className = "versions-pop";
    pop.innerHTML = '<div class="empty" style="padding:10px">加载版本中…</div>';
    item.parentNode.insertBefore(pop, item.nextSibling);
    try {
      const gv = gvSel.value, loader = $("mod-loader").value;
      const vers = await call("ModVersions", m.project_id, gv, loader);
      pop.innerHTML = "";
      if (!vers || !vers.length) {
        pop.innerHTML = '<div class="empty" style="padding:10px">' +
          (gv || loader ? "该版本/加载器下没有文件，试试清空筛选" : "没有可用版本") + "</div>";
        return;
      }
      vers.slice(0, 12).forEach((v) => {
        const li = document.createElement("div");
        li.className = "ver-item";
        const vn = document.createElement("span"); vn.className = "vn"; vn.textContent = v.version_number || v.name;
        const vm = document.createElement("span"); vm.className = "vm";
        vm.textContent = (v.loaders || []).join("/") + " · " + (v.game_versions || []).slice(-2).join(",") + " · " + fmtBytes(v.file_size);
        const dl = document.createElement("button");
        dl.className = "btn primary sm";
        dl.textContent = "下载";
        dl.onclick = () => startModDownload(v, pop);
        li.append(vn, vm, dl);
        pop.appendChild(li);
      });
    } catch (e) {
      pop.innerHTML = '<div class="empty" style="padding:10px">版本拉取失败：' + e.message + "</div>";
    }
  }

  function startModDownload(v, pop) {
    if (!modsDir) {
      toast("先在设置里配置 MC 目录（或点上方「选择文件夹」）", "err", 4200);
      return;
    }
    const dest = modsDir + "\\mods\\" + v.filename;
    call("ModDownload", v.file_url, dest, 8)
      .then(() => { toast("已加入下载队列（8 线程）", "ok"); pop.remove(); refreshQueue(); })
      .catch((e) => toast(e.message, "err"));
  }

  // ---- 下载队列 ----
  const jobs = new Map(); // dest → {p, el}

  on("clink:modprogress", (p) => {
    if (!p || !p.dest) return;
    if (p.finished) {
      toast("✅ " + p.name + " 下载完成，进游戏就能用！", "ok", 4200);
      jobs.delete(p.dest);
      refreshQueue();
      return;
    }
    if (p.failed) {
      toast("⚠️ " + p.name + "：" + (p.err || "下载失败") + "（重新点下载可续传）", "err", 5200);
      jobs.delete(p.dest);
      refreshQueue();
      return;
    }
    jobs.set(p.dest, p);
    renderQueueItem(p);
  });

  function renderQueueItem(p) {
    const ul = $("queue-list");
    let li = ul.querySelector('[data-dest="' + CSS.escape(p.dest) + '"]');
    if (!li) {
      li = document.createElement("li");
      li.className = "queue-item";
      li.dataset.dest = p.dest;
      li.innerHTML =
        '<div class="qi-top"><span class="qi-name"></span><span class="qi-speed"></span>' +
        '<button class="qi-cancel">取消</button></div>' +
        '<div class="qi-bar"><div class="qi-fill"></div></div>' +
        '<div class="qi-sub"><span class="qi-pct"></span><span class="qi-threads"></span></div>';
      li.querySelector(".qi-cancel").onclick = () => call("ModCancel", p.dest);
      ul.appendChild(li);
      $("queue-box").classList.remove("hidden");
    }
    li.querySelector(".qi-name").textContent = p.name;
    const pct = p.total > 0 ? Math.min(100, Math.round((p.done / p.total) * 100)) : 0;
    li.querySelector(".qi-fill").style.width = pct + "%";
    li.querySelector(".qi-pct").textContent = pct + "% · " + fmtBytes(p.done) + " / " + fmtBytes(p.total);
    li.querySelector(".qi-speed").textContent = p.speed > 0 ? fmtBytes(p.speed) + "/s" : "";
    li.querySelector(".qi-threads").textContent = p.threads > 0 ? p.threads + " 线程" : "";
  }

  function refreshQueue() {
    const ul = $("queue-list");
    ul.innerHTML = "";
    call("ModJobs").then((list) => {
      (list || []).forEach((p) => {
        if (!p.finished && !p.failed) { jobs.set(p.dest, p); renderQueueItem(p); }
      });
      $("queue-box").classList.toggle("hidden", ul.children.length === 0);
    }).catch(() => {});
  }

  // ================= 工具箱 =================
  $("btn-ip").addEventListener("click", async () => {
    $("ip-out").innerHTML = "查询中…";
    try {
      const r = await call("TbQueryIP", "");
      $("ip-out").innerHTML = '<span class="big">' + r.ip + "</span><br>" +
        '<span class="dim">' + (r.region || "未知地区") + " · " + (r.isp || "") + "</span>";
    } catch (e) {
      $("ip-out").innerHTML = "查询失败：" + e.message;
    }
  });

  $("btn-mcstatus").addEventListener("click", async () => {
    const addr = $("mc-addr").value.trim();
    if (!addr) return;
    $("mcstatus-out").innerHTML = "查询中…";
    try {
      const r = await call("TbMCStatus", addr);
      $("mcstatus-out").innerHTML = r.online
        ? '<span class="big">🟢 在线</span> <span class="dim">' + (r.players || "?") + " 人 · " + (r.version || "") +
          (r.latencyMS ? " · " + r.latencyMS + "ms" : "") + "</span><br>" + (r.motd || "")
        : '<span class="big" style="color:var(--danger)">🔴 不在线</span>';
    } catch (e) {
      $("mcstatus-out").innerHTML = "查询失败：" + e.message;
    }
  });

  $("btn-qr").addEventListener("click", async () => {
    const t = $("qr-text").value.trim();
    if (!t) return;
    $("qr-img").classList.add("hidden");
    try {
      const url = await call("TbQRCode", t);
      $("qr-img").src = url;
      $("qr-img").classList.remove("hidden");
    } catch (e) {
      toast(e.message, "err");
    }
  });

  // ================= 设置 =================
  $("btn-save-profile").addEventListener("click", () => {
    const name = $("set-name").value.trim();
    if (name.length > 16) return toast("昵称最多 16 个字", "err");
    call("SetProfile", name, $("set-notify").checked)
      .then(() => toast("已保存", "ok"))
      .catch((e) => toast(e.message, "err"));
  });

  $("btn-bl-add").addEventListener("click", () => {
    const name = $("bl-name").value.trim();
    if (!name) return;
    call("BlAdd", name).then(() => {
      $("bl-name").value = "";
      toast(name + " 已拉黑", "ok");
      renderBL();
    }).catch((e) => toast(e.message, "err"));
  });

  function renderBL() {
    call("BlList").then((list) => {
      const ul = $("bl-list");
      ul.innerHTML = "";
      (list || []).forEach((e) => {
        const li = document.createElement("li");
        li.textContent = "🚫 " + e.name + (e.reason ? "（" + e.reason + "）" : "");
        const btn = document.createElement("button");
        btn.className = "unban";
        btn.textContent = "解除";
        btn.onclick = () => call("BlRemove", e.name).then(renderBL).catch((er) => toast(er.message, "err"));
        li.appendChild(btn);
        ul.appendChild(li);
      });
    }).catch(() => {});
  }

  $("btn-probe").addEventListener("click", async () => {
    $("probe-out").innerHTML = "诊断中…（多节点探测，最多十几秒）";
    try {
      const r = await call("NetProbe");
      const stunLine = r.stunNode
        ? "STUN 节点：<code>" + r.stunNode.replace("stun:", "") + "</code>（" + (r.rttMs || "?") + "ms）<br>"
        : "";
      $("probe-out").innerHTML =
        "IPv6 直连：" + (r.hasV6 ? "✅ 支持" : "❌ 不支持（会自动走 P2P 打洞）") + "<br>" +
        stunLine +
        "NAT 类型：" + (r.natType || "检测中…") + "<br>" +
        '<span class="dim">双层网络自动选路，你不用管</span>';
    } catch (e) {
      $("probe-out").innerHTML = "诊断失败：" + e.message;
    }
  });

  // ================= 网络与信令设置 =================
  let presetTurn = [];

  function renderTurnPresets(enabled) {
    const box = $("turn-preset-list");
    box.innerHTML = "";
    (presetTurn || []).forEach((n) => {
      const row = document.createElement("label");
      row.className = "switch-row turn-node";
      const cb = document.createElement("input");
      cb.type = "checkbox";
      cb.value = n.name;
      cb.checked = (enabled || []).includes(n.name);
      const span = document.createElement("span");
      span.textContent = n.name + " · " + n.note;
      row.append(cb, span);
      box.appendChild(row);
    });
  }

  function syncCustomFields() {
    $("set-mailbox").classList.toggle("hidden", $("sig-node").value !== "custom");
    $("set-stun").classList.toggle("hidden", $("stun-mode").value !== "custom");
  }
  $("sig-node").addEventListener("change", syncCustomFields);
  $("stun-mode").addEventListener("change", syncCustomFields);

  $("btn-save-adv").addEventListener("click", () => {
    const turnNodes = Array.from(document.querySelectorAll("#turn-preset-list input:checked")).map((c) => c.value);
    const cfg = {
      node: $("sig-node").value,
      mailbox: $("set-mailbox").value.trim(),
      stunMode: $("stun-mode").value,
      stun: $("set-stun").value.trim(),
      turnNodes: turnNodes,
      turn: $("set-turn").value.trim(),
      localPort: parseInt($("set-localport").value || "25565", 10),
      mcDir: $("set-mcdir").value.trim(),
      relayNodes: $("set-relay").value.trim(),
      relayPSK: $("set-relaypsk").value.trim(),
    };
    call("SetNet", cfg)
      .then(() => { toast("已保存", "ok"); refreshModsDirLabel(); })
      .catch((e) => toast(e.message, "err"));
  });

  // ================= 短信盒子 =================
  function smsOut(msg, cls) {
    const el = $("sms-out");
    el.textContent = msg;
    el.style.color = cls === "err" ? "var(--danger)" : cls === "ok" ? "var(--green)" : "var(--text2)";
  }
  $("btn-sms-ping").addEventListener("click", () => {
    smsOut("正在连接盒子…");
    // 先临时保存再测（用户可能没点保存）
    call("SmsSave", $("sms-url").value, $("sms-pin").value, $("sms-phone").value,
      $("sms-join").checked, $("sms-reject").checked)
      .then(() => call("SmsPing"))
      .then(() => smsOut("✅ 盒子连接成功", "ok"))
      .catch((e) => smsOut("连接失败：" + e.message, "err"));
  });
  $("btn-sms-test").addEventListener("click", () => {
    smsOut("正在发送测试短信…");
    call("SmsSave", $("sms-url").value, $("sms-pin").value, $("sms-phone").value,
      $("sms-join").checked, $("sms-reject").checked)
      .then(() => call("SmsTest", $("sms-phone").value))
      .then(() => smsOut("✅ 测试短信已发出，注意查收手机", "ok"))
      .catch((e) => smsOut("发送失败：" + e.message, "err"));
  });
  $("btn-sms-save").addEventListener("click", () => {
    call("SmsSave", $("sms-url").value, $("sms-pin").value, $("sms-phone").value,
      $("sms-join").checked, $("sms-reject").checked)
      .then(() => smsOut("已保存 ✓", "ok"))
      .catch((e) => smsOut(e.message, "err"));
  });

  // 短信邀请（房主房间卡内）
  $("btn-sms-invite").addEventListener("click", async () => {
    const phone = $("invite-phone").value.trim();
    if (!phone) { toast("先填朋友的手机号", "err"); return; }
    try {
      await call("SmsInvite", phone);
      toast("📱 邀请短信已发出，等朋友来吧", "ok");
      $("invite-phone").value = "";
    } catch (e) {
      toast(e.message, "err");
    }
  });

  // ================= 启动恢复 =================
  // ---- 更新通知（发布编号去重在后端做，这里只展示横幅）----
  on("clink:update", (info) => {
    if (!info || !info.hasUpdate) return;
    $("upd-version").textContent = "v" + info.latestVersion;
    $("upd-notes").textContent = info.notes || "优化体验";
    $("upd-link").onclick = (ev) => {
      ev.preventDefault();
      if (window.runtime && window.runtime.BrowserOpenURL) window.runtime.BrowserOpenURL(info.download || info.page);
    };
    $("update-bar").classList.remove("hidden");
    toast("🚀 发现新版本 v" + info.latestVersion + "，去设置页可立即检查", "ok", 5200);
  });
  $("upd-close").addEventListener("click", () => $("update-bar").classList.add("hidden"));

  // ---- 手动检查更新（设置页）----
  $("btn-check-update").addEventListener("click", async () => {
    const out = $("check-out");
    out.textContent = "正在检查…";
    try {
      const r = await call("CheckUpdate");
      if (r.hasUpdate) {
        out.innerHTML = '🚀 有新版本 <b>v' + r.latestVersion + '</b>（发布编号 ' + r.latestBuild +
          '）<br>' + (r.notes || "") + '<br><a href="#" id="upd-dl-link">点这里下载新版本</a>';
        out.querySelector("#upd-dl-link").onclick = (ev) => {
          ev.preventDefault();
          if (window.runtime && window.runtime.BrowserOpenURL) window.runtime.BrowserOpenURL(r.download || r.page);
        };
      } else {
        out.innerHTML = "✅ 已是最新版本 v" + r.currentVersion + "（发布编号 " + r.currentBuild + "）";
      }
    } catch (e) {
      out.textContent = "检查失败：" + e.message;
    }
  });

  (async function init() {
    try {
      const st = await call("Status");
      if (st.ball) { $("ball").classList.remove("hidden"); }
      if (st.name) $("set-name").value = st.name;
      // 网络与信令设置回显
      const net = await call("GetNet");
      $("sig-node").value = net.node || "auto";
      $("set-mailbox").value = net.mailbox || "";
      $("stun-mode").value = net.stunMode || "auto";
      $("set-stun").value = net.stun || "";
      $("set-turn").value = net.turn || "";
      $("set-localport").value = net.localPort || 25565;
      $("set-relay").value = net.relayNodes || "";
      $("set-relaypsk").value = net.relayPSK || "";
      $("set-mcdir").value = net.mcDir || "";
      // 预设节点渲染（信令下拉/TURN 勾选列表）
      try {
        const pn = await call("PresetNodes");
        presetTurn = (pn && pn.turn) || [];
      } catch (e) { presetTurn = []; }
      renderTurnPresets(net.turnNodes);
      syncCustomFields();
      refreshModsDirLabel();
      // 当前版本信息
      const cv = await call("CurrentVersion");
      $("cur-version").textContent = "v" + cv.version;
      $("cur-build").textContent = cv.build;
      $("tb-ver").textContent = "v" + cv.version;
      // 短信盒子配置回显
      const sm = await call("SmsGet");
      $("sms-url").value = sm.url || "";
      $("sms-pin").value = sm.pin || "";
      $("sms-phone").value = sm.phone || "";
      $("sms-join").checked = !!sm.joinOn;
      $("sms-reject").checked = !!sm.rejectOn;
      // 恢复未完成下载
      if ((net.mcDir || "").trim()) {
        call("ModResumeAll", net.mcDir + "\\mods").catch(() => {});
      }
      renderBL();
    } catch (e) { /* 浏览器调试时静默 */ }
  })();
})();
