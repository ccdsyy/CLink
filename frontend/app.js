// CLink 前端交互（保姆级：零术语，状态人话化）
/* global window, document */
(() => {
  const $ = (id) => document.getElementById(id);

  // ---- Wails 桥 ----
  const app = () => (window.go && window.go.main && window.go.main.App) || null;
  const call = (method, ...args) => {
    const a = app();
    if (!a || !a[method]) {
      return Promise.reject(new Error("请通过 CLink 程序打开本窗口（不要用浏览器直接打开）"));
    }
    return a[method](...args);
  };
  const on = (name, cb) => {
    if (window.runtime && window.runtime.EventsOn) {
      window.runtime.EventsOn(name, cb);
    }
  };

  // ---- 状态 ----
  let busy = false;
  let guests = 0;

  const toast = (msg, cls = "", ms = 3200) => {
    const el = document.createElement("div");
    el.className = "toast " + cls;
    el.textContent = msg;
    document.body.appendChild(el);
    setTimeout(() => el.remove(), ms);
  };

  const copyText = async (t) => {
    try {
      if (window.runtime && window.runtime.ClipboardSetText) {
        await window.runtime.ClipboardSetText(t);
      } else {
        await navigator.clipboard.writeText(t);
      }
      toast("已复制：" + t, "ok", 2000);
    } catch (e) {
      toast("复制失败，请手动选中复制", "err");
    }
  };

  // ---- 房主 ----
  async function createRoom() {
    if (busy) return;
    const portStr = ($("mc-port").value || "").trim();
    const port = parseInt(portStr, 10);
    if (!port || port < 1 || port > 65535) {
      toast("先填游戏端口（或先在游戏里“对局域网开放”，会自动填）", "err");
      return;
    }
    busy = true;
    $("btn-create").disabled = true;
    $("host-status").textContent = "正在创建房间…（首次会请求一次管理员权限，用于放行防火墙，点“是”即可）";
    try {
      const code = await call("CreateRoom", port);
      $("room-box").classList.remove("hidden");
      $("room-code").textContent = code;
      $("host-phase").textContent = "房间准备中…";
      $("host-status").textContent = "把房间码发给小伙伴（已自动复制）";
      $("btn-leave").classList.remove("hidden");
      call("StartLogWatch").catch(() => {});
      toast("房间码已复制：" + code, "ok", 2400);
    } catch (e) {
      $("host-status").textContent = "创建失败：" + (e && e.message ? e.message : e);
      toast("创建房间失败", "err");
    } finally {
      busy = false;
      $("btn-create").disabled = false;
    }
  }

  // ---- 客机 ----
  async function joinRoom() {
    if (busy) return;
    const code = ($("room-input").value || "").trim().toLowerCase();
    if (!/^[abcdefghjkmnpqrstuvwxyz23456789]{6}$/.test(code)) {
      toast("房间码是 6 位字母数字（不含 0/1/i/l/o，防看错）", "err");
      return;
    }
    busy = true;
    $("btn-join").disabled = true;
    $("guest-status").textContent = "正在连接…（可能需要 10~30 秒打洞）";
    $("addr-box").classList.add("hidden");
    try {
      await call("JoinRoom", code);
      $("btn-leave").classList.remove("hidden");
    } catch (e) {
      $("guest-status").textContent = "加入失败：" + (e && e.message ? e.message : e);
      toast("加入失败", "err");
    } finally {
      busy = false;
      $("btn-join").disabled = false;
    }
  }

  // ---- 退出 ----
  async function leave() {
    try { await call("Leave"); } catch (e) { /* ignore */ }
    resetUI();
    toast("已退出联机", "ok", 2000);
  }

  function resetUI() {
    $("room-box").classList.add("hidden");
    $("addr-box").classList.add("hidden");
    $("btn-leave").classList.add("hidden");
    $("host-phase").textContent = "房间准备中…";
    $("host-guests").innerHTML = "";
    $("guest-status").textContent = "拿到房间码就可以加入啦";
    $("host-status").textContent = "先开单人世界 → Esc → 「对局域网开放」，再回来点「创建房间」";
    guests = 0;
  }

  // ---- 事件流 ----
  on("clink:mcport", (port) => {
    if (port && !$("room-box").classList.contains("hidden")) {
      toast("检测到游戏端口：" + port + "（已更新）", "ok", 2200);
    }
    if (port) $("mc-port").value = String(port);
  });

  on("clink:event", (e) => {
    if (!e) return;
    switch (e.stage) {
      case "created":
      case "waiting":
        $("host-phase").textContent = "等待小伙伴加入…（把房间码发给TA）";
        break;
      case "guest-joined":
        guests = e.guests || guests + 1;
        $("host-phase").textContent = "小伙伴在线，还可以继续把房间码发给其他朋友";
        const line = document.createElement("div");
        line.textContent = "🟢 " + (e.msg || "有小伙伴加入了");
        $("host-guests").appendChild(line);
        toast(e.msg || "有小伙伴加入了！", "ok", 2600);
        break;
      case "connected":
        $("addr-box").classList.remove("hidden");
        $("addr").textContent = e.addr || "127.0.0.1:25565";
        $("guest-status").textContent = "已连接！（" + (e.tier === "ipv6" ? "极速直连" : "P2P 打洞") + "）";
        $("host-phase").textContent = "小伙伴在线，还可以继续把房间码发给其他朋友";
        toast("连接成功！地址已复制，去游戏里粘贴", "ok", 3400);
        break;
      case "reconnecting":
        $("guest-status").textContent = e.msg || "网络波动，正在自动重连…";
        $("host-phase").textContent = e.msg || "小伙伴网络波动，正在自动重连…";
        toast("网络波动，正在自动重连…", "", 2600);
        break;
      case "error":
        $("guest-status").textContent = e.msg || "出错了";
        $("host-phase").textContent = e.msg || "出错了";
        toast(e.msg || "出错了", "err", 4200);
        break;
      case "info":
        toast(e.msg || "", "", 3600);
        break;
    }
  });

  // ---- 高级设置 ----
  function toggleAdvanced() {
    $("advanced").classList.toggle("hidden");
    if (!$("advanced").classList.contains("hidden")) {
      call("GetAdvanced").then((c) => {
        if (c) {
          $("adv-turn").value = c.turn || "";
          $("adv-mcdir").value = c.mcDir || "";
          $("adv-mailbox").value = c.mailbox || "";
        }
      }).catch(() => {});
    }
  }

  async function saveAdvanced() {
    try {
      await call("SetAdvanced",
        $("adv-turn").value.trim(),
        $("adv-mcdir").value.trim(),
        $("adv-mailbox").value.trim());
      toast("已保存（重新开房/加入后生效）", "ok", 2200);
    } catch (e) {
      toast("保存失败", "err");
    }
  }

  async function netCheck() {
    const el = $("netcheck-result");
    el.textContent = "体检中…";
    try {
      const r = await call("NetProbe");
      const v6 = r.hasV6 ? "✅ IPv6 可用（将走极速直连）" : "➖ 无 IPv6（走 P2P 打洞）";
      const nat = r.natType || "未知";
      el.textContent = v6 + " · " + nat;
    } catch (e) {
      el.textContent = "体检失败（不影响正常使用）";
    }
  }

  // ---- 初始化 ----
  $("btn-create").addEventListener("click", createRoom);
  $("btn-join").addEventListener("click", joinRoom);
  $("btn-leave").addEventListener("click", leave);
  $("btn-copy-code").addEventListener("click", () => copyText($("room-code").textContent));
  $("btn-copy-addr").addEventListener("click", () => copyText($("addr").textContent));
  $("room-code").addEventListener("click", () => copyText($("room-code").textContent));
  $("addr").addEventListener("click", () => copyText($("addr").textContent));
  $("btn-advanced").addEventListener("click", toggleAdvanced);
  $("btn-save-adv").addEventListener("click", saveAdvanced);
  $("btn-netcheck").addEventListener("click", netCheck);
  $("room-input").addEventListener("input", (ev) => {
    ev.target.value = ev.target.value.toLowerCase().replace(/[^a-z0-9]/g, "");
  });
  $("mc-port").addEventListener("input", (ev) => {
    ev.target.value = ev.target.value.replace(/\D/g, "").slice(0, 5);
  });

  // 启动时自动探测端口 + 开始日志监听
  call("DetectMCPort").then((p) => {
    if (p) {
      $("mc-port").value = String(p);
      toast("检测到你的游戏端口：" + p, "ok", 2400);
    }
    return call("StartLogWatch");
  }).catch(() => {});
})();
