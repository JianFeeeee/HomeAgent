      // ===== State =====
      let state = {
        status: {},
        kernel: null,
        settings: {},
        meta: {},
        runtime: null, // /api/v1/runtime 的运行态小快照（调度器/驻留子/通道）
        pluginMeta: {},
        disabledPlugins: [],
        settingsPlugins: ["core"],
        selectedSection: "core",
        messages: [],
        chatLoading: false,
        chatStage: "",
        healthResult: null,
        starmapInit: false,
        starmapLoading: false,
        starmapData: null,
        chatHistory: [],
        terminals: [],
        cmdHistory: [],
        termScreens: {},
        chatStick: true,
        pendingTools: [],
        eventSource: null,
        chatFinalIdx: -1,
        chatOffset: 0, // 分段历史：当前已加载消息在服务端全量中的起始下标
        chatTotal: 0, // 服务端历史总条数
        chatHasMore: false, // 是否还有更早历史可向上加载
        lang: localStorage.getItem("ha-lang") || "zh",
      };

      // ===== I18n =====
      window._i18n = {
        navOverview: ["概览", "Overview"],
        navChat: ["对话", "Chat"],
        navPlugins: ["插件", "Plugins"],
        navSettings: ["设置", "Settings"],
        navAdapters: ["适配器", "Adapters"],
        navKernel: ["内核", "Kernel"],
        navLogout: ["退出登录", "Logout"],
        themeToggle: ["切换亮色/暗色模式", "Toggle theme"],
      };

      function __(zh, en) {
        return state.lang === "en" ? en : zh;
      }
      function L() {
        return state.lang;
      }

      function toggleLang() {
        state.lang = state.lang === "zh" ? "en" : "zh";
        localStorage.setItem("ha-lang", state.lang);
        document.querySelectorAll("[data-i18n]").forEach(function (el) {
          var k = el.getAttribute("data-i18n");
          var m = window._i18n && window._i18n[k];
          if (m) el.textContent = __(m[0], m[1]);
        });
        renderAll();
      }

      function applyI18n() {
        var lang = state.lang;
        var btn = document.getElementById("lang-btn");
        if (btn) btn.textContent = lang === "zh" ? "EN" : "中";
        document.querySelectorAll("[data-i18n]").forEach(function (el) {
          var k = el.getAttribute("data-i18n");
          var m = window._i18n && window._i18n[k];
          if (m) el.textContent = lang === "en" ? m[1] : m[0];
        });
      }

      // ===== Theme =====
      var ICON_SUN =
        '<svg viewBox="0 0 24 24" width="15" height="15" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><circle cx="12" cy="12" r="4"/><path d="M12 2v2m0 16v2M4.9 4.9l1.4 1.4m11.4 11.4 1.4 1.4M2 12h2m16 0h2M4.9 19.1l1.4-1.4m11.4-11.4 1.4-1.4"/></svg>';
      var ICON_MOON =
        '<svg viewBox="0 0 24 24" width="15" height="15" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8z"/></svg>';

      function setTheme(name) {
        document.documentElement.setAttribute("data-theme", name);
        var btn = document.getElementById("theme-btn");
        if (btn) btn.innerHTML = name === "light" ? ICON_SUN : ICON_MOON;
      }

      function saveTheme(name) {
        localStorage.setItem("ha-theme", name);
        setTheme(name);
      }

      function toggleTheme() {
        var cur = document.documentElement.getAttribute("data-theme");
        saveTheme(cur === "light" ? "dark" : "light");
      }

      var PALETTES = {
        sakura: "#ff7fac",
        cyan: "#2dd4bf",
        violet: "#a78bfa",
        emerald: "#34d399",
        amber: "#fbbf24",
        blue: "#60a5fa",
      };

      function setColor(name) {
        document.documentElement.setAttribute("data-color", name);
        localStorage.setItem("ha-color", name);
        var pop = document.getElementById("palette-pop");
        if (!pop) return;
        var btns = pop.querySelectorAll("button.cdot");
        for (var i = 0; i < btns.length; i++) {
          btns[i].className =
            btns[i].getAttribute("data-c") === name ? "cdot on" : "cdot";
        }
      }

      function applyBgImg(url) {
        url = (url || "").trim();
        if (!url) {
          document.documentElement.style.setProperty("--bg-img", "none");
          localStorage.removeItem("ha-bg-img");
          return;
        }
        document.documentElement.style.setProperty(
          "--bg-img",
          'url("' + url.replace(/"/g, '\\"') + '")',
        );
        localStorage.setItem("ha-bg-img", url);
      }

      function applyBgBlur(n) {
        n = Math.max(0, Math.min(30, Number(n) || 0));
        document.documentElement.style.setProperty("--bg-blur", String(n));
        localStorage.setItem("ha-bg-blur", String(n));
        var v = document.getElementById("bg-blur-val");
        if (v) v.textContent = n + "px";
        var r = document.getElementById("bg-blur-range");
        if (r) r.value = String(n);
      }

      function toggleAppearance() {
        var pop = document.getElementById("palette-pop");
        if (!pop) return;
        var on = pop.classList.contains("on");
        if (!pop.querySelector("button.cdot")) {
          var cur = localStorage.getItem("ha-color") || "sakura";
          var img = localStorage.getItem("ha-bg-img") || "";
          var blur = localStorage.getItem("ha-bg-blur") || "0";
          var dots = "";
          Object.keys(PALETTES).forEach(function (k) {
            dots +=
              '<button class="cdot" data-c="' +
              k +
              '" title="' +
              k +
              '" style="background:' +
              PALETTES[k] +
              '"' +
              (k === cur ? "" : "") +
              " onclick=\"setColor('" +
              k +
              "')\"></button>";
          });
          pop.innerHTML =
            "<h4>主题色</h4><div>" +
            dots +
            "</div>" +
            '<h4 style="margin-top:8px">背景图片 URL</h4>' +
            '<input type="text" id="bg-img-input" placeholder="https://...jpg / png" value="' +
            escHtml(img) +
            '">' +
            '<div class="pp-row"><button class="btn btn-ghost btn-sm" onclick="applyBgImg(document.getElementById(\'bg-img-input\').value)">应用</button>' +
            '<button class="btn btn-ghost btn-sm" onclick="applyBgImg(\'\')">清除</button>' +
            '<span class="pp-val" style="margin-left:auto;min-width:90px;font-size:10px">模糊 ' +
            '<input type="range" id="bg-blur-range" min="0" max="30" value="' +
            blur +
            '" oninput="applyBgBlur(this.value)" style="width:80px;display:inline-block;vertical-align:middle">' +
            '<span id="bg-blur-val">' +
            blur +
            "px</span></span></div>";
          setColor(cur);
          var rr = document.getElementById("bg-blur-range");
          if (rr) rr.value = blur;
          var vv = document.getElementById("bg-blur-val");
          if (vv) vv.textContent = blur + "px";
        }
        pop.classList.toggle("on", !on);
      }

      (function initAppearance() {
        var img = localStorage.getItem("ha-bg-img");
        var blur = localStorage.getItem("ha-bg-blur");
        if (img) applyBgImg(img);
        if (blur) applyBgBlur(blur);
      })();

      (function () {
        var saved = localStorage.getItem("ha-theme");
        setTheme(saved || "light");
        var savedColor = localStorage.getItem("ha-color");
        if (savedColor) setColor(savedColor);
        var sb = localStorage.getItem("ha-sidebar");
        if (sb === "1" || (!sb && window.innerWidth <= 768)) {
          document.body.classList.add("sidebar-collapsed");
        }
      })();

      // 字节数人性化显示（附件卡片用）
      function formatBytes(n) {
        if (!n || n <= 0) return "";
        var units = ["B", "KB", "MB", "GB"];
        var i = 0;
        while (n >= 1024 && i < units.length - 1) {
          n /= 1024;
          i++;
        }
        return (i === 0 ? n : n.toFixed(1)) + " " + units[i];
      }

      // ===== Utility =====
      function renderMd(text) {
        if (typeof text !== "string") text = String(text || "");
        var html;
        if (typeof marked !== "undefined") {
          try {
            html = marked.parse(text);
          } catch (e) {
            html = escHtml(text);
          }
        } else {
          html = "<pre>" + escHtml(text) + "</pre>";
        }
        if (
          typeof DOMPurify !== "undefined" &&
          typeof DOMPurify.sanitize === "function"
        ) {
          try {
            return DOMPurify.sanitize(html, { USE_PROFILES: { html: true } });
          } catch (e) {}
        }
        return html
          .replace(/<script[\s\S]*?<\/script>/gi, "")
          .replace(/\son\w+\s*=\s*"[^"]*"/gi, "")
          .replace(/\son\w+\s*=\s*'[^']*'/gi, "")
          .replace(/javascript:/gi, "");
      }
      function escHtml(s) {
        return String(s)
          .replace(/&/g, "&amp;")
          .replace(/</g, "&lt;")
          .replace(/>/g, "&gt;")
          .replace(/"/g, "&quot;");
      }

      function toast(m, isError) {
        var t = document.getElementById("toast");
        t.textContent = m;
        t.className = "toast" + (isError ? " error" : "");
        t.style.display = "block";
        clearTimeout(t._hideTimer);
        t._hideTimer = setTimeout(function () {
          t.style.display = "none";
        }, 3000);
      }

      // ===== 8.6 Unified toast + confirm dialog =====

      // ===== 8.4 Card 3D tilt + cursor glow =====
      document.addEventListener("mousemove", function (e) {
        var card = e.target.closest ? e.target.closest(".card.tilt") : null;
        if (card) {
          var r = card.getBoundingClientRect();
          card.style.setProperty("--mx", e.clientX - r.left + "px");
          card.style.setProperty("--my", e.clientY - r.top + "px");
          var rx = ((e.clientY - r.top) / r.height - 0.5) * -4;
          var ry = ((e.clientX - r.left) / r.width - 0.5) * 4;
          card.style.transform =
            "perspective(1000px) rotateX(" +
            rx.toFixed(2) +
            "deg) rotateY(" +
            ry.toFixed(2) +
            "deg) translateY(-1px)";
        }
      });
      document.addEventListener("mouseleave", function (e) {
        var card = e.target.closest ? e.target.closest(".card.tilt") : null;
        if (card) card.style.transform = "";
      });

        // ===== 首启人格向导 =====
        // 人格是配置项（core.agent.personal_prompt，默认模板不含任何版本号）。
        // 首次启动问一次「默认 / 自定义 / 稍后」，之后不再打扰；
        // 不回答 = 稍后 = 保留默认人格，绝不阻塞启动。
        async function maybeShowPersonaWizard() {
          var st;
          try {
            st = await api("/persona");
          } catch (e) {
            return; // 拿不到状态就不打扰用户
          }
          if (!st || st.initialized) return;
          var ov = document.createElement("div");
          ov.className = "confirm-overlay";
          ov.style.display = "flex";
          ov.innerHTML =
            '<div class="confirm-box">' +
            "<h3>" + escHtml(__("人格设定", "Persona")) + "</h3>" +
            "<p>" + escHtml(__(
              "首次启动：选一下助手的人格。选「使用默认」即可（之后可在设置里修改）；自定义内容在下次重启后生效。",
              "First run: pick your assistant's persona. \"Use default\" is fine (change it later in Settings); custom content takes effect after the next restart."
            )) + "</p>" +
            '<div class="persona-warn"></div>' +
            '<textarea class="persona-ta" rows="6"></textarea>' +
            '<div class="persona-actions">' +
            '<button class="btn btn-ghost btn-sm" data-mode="later">' + escHtml(__("稍后再说", "Later")) + "</button>" +
            '<button class="btn btn-ghost btn-sm" data-mode="custom">' + escHtml(__("自定义…", "Custom…")) + "</button>" +
            '<button class="btn btn-sm" data-mode="default">' + escHtml(__("使用默认", "Use default")) + "</button>" +
            "</div></div>";
          document.body.appendChild(ov);
          var ta = ov.querySelector(".persona-ta");
          var warn = ov.querySelector(".persona-warn");
          var customOpen = false;
          if (st.file_override) {
            warn.style.display = "block";
            warn.textContent = __(
              "注意：检测到 personal/personal.md，它优先于这里的设置。",
              "Note: personal/personal.md exists and takes precedence over this choice."
            );
          }
          function close() {
            ov.remove();
          }
          async function submit(mode, content) {
            try {
              var r = await api("/persona", {
                method: "POST",
                body: JSON.stringify({ mode: mode, content: content || "" }),
              });
              if (r && r.restart_required) toast(__("已保存，重启后生效", "Saved; takes effect after restart"));
              else toast(__("已保存", "Saved"));
            } catch (e) {
              toast(__("保存失败：", "Save failed: ") + e, true);
            }
            close();
          }
          ov.querySelectorAll("button[data-mode]").forEach(function (b) {
            b.onclick = function () {
              var mode = b.getAttribute("data-mode");
              if (mode !== "custom") {
                submit(mode);
                return;
              }
              if (!customOpen) { // 第一次点：展开文本域并预填当前人格
                customOpen = true;
                ta.style.display = "block";
                ta.value = st.current_prompt || "";
                ta.focus();
                return;
              }
              if (!ta.value.trim()) {
                toast(__("内容不能为空", "Content cannot be empty"), true);
                return;
              }
              submit("custom", ta.value);
            };
          });
        }

        // ===== API =====
        async function api(p, o) {
        var opts = {
          credentials: "include",
          headers: { "Content-Type": "application/json", ...o?.headers },
          ...o,
        };
        // FormData 时不能手动设 Content-Type（浏览器需自动生成 multipart boundary）
        if (o?.body instanceof FormData) {
          delete opts.headers["Content-Type"];
        }
        var r = await fetch("/api/v1" + p, opts);
        if (r.status === 401) {
          location.href = "/login";
          throw new Error("unauthorized");
        }
        if (opts.raw) return r;
        var ct = r.headers.get("content-type") || "";
        if (ct.includes("json")) return r.json();
        return r.text();
      }

      // ===== Navigation =====
      function switchTab(n) {
        document.querySelectorAll(".tab-content").forEach(function (e) {
          e.classList.remove("active");
        });
        var el = document.getElementById("tab-" + n);
        if (el) el.classList.add("active");
        document.querySelectorAll("nav a").forEach(function (e) {
          e.classList.remove("active");
        });
        var match = document.querySelector('nav a[onclick*="' + n + '"]');
        if (match) match.classList.add("active");
        var crumb = document.querySelector(".crumb-current");
        if (crumb) {
          var crumbKey = "nav" + n.charAt(0).toUpperCase() + n.slice(1);
          var m = window._i18n && window._i18n[crumbKey];
          crumb.setAttribute("data-i18n", crumbKey);
          if (m) crumb.textContent = __(m[0], m[1]);
          else crumb.textContent = n;
        }
        if (window.innerWidth <= 768) {
          document.body.classList.add("sidebar-collapsed");
        }
        if (n === "chat") {
          state.chatStick = true;
          var msgsEl = document.getElementById("chat-msgs");
          if (msgsEl) {
            msgsEl.scrollTop = msgsEl.scrollHeight;
          }
          initChatDragDrop();
        }
        renderAll();
      }

      function toggleSidebar() {
        document.body.classList.toggle("sidebar-collapsed");
        var saved = localStorage.getItem("ha-sidebar");
        localStorage.setItem(
          "ha-sidebar",
          document.body.classList.contains("sidebar-collapsed") ? "1" : "0",
        );
      }

      // ===== Tab Render Dispatch =====
      async function renderAll() {
        try {
          var s = await api("/status");
          state.status = s;
          state.startedAt = s.startedAt
            ? new Date(s.startedAt).getTime()
            : null;
        } catch (e) {}
        try {
          state.kernel = await api("/kernel");
        } catch (e) {}
        try {
          var s = await api("/settings");
          state.settings = s.settings || {};
          state.meta = s.meta || {};
          state.settingsPlugins = s.plugins || ["core"];
          state.pluginMeta = s.plugin_meta || {};
          state.disabledPlugins = s.disabled_plugins || [];
        } catch (e) {}
        try {
          state.installedPlugins = await api("/plugins");
        } catch (e) {}
        try {
          await loadTerminals();
        } catch (e) {}
        try {
          await loadCmdHistory();
        } catch (e) {}
        try {
          renderOverview();
        } catch (e) {
          console.error("renderOverview", e);
        }
        try {
          await loadRuntime();
        } catch (e) {
          console.error("loadRuntime", e);
        }
        try {
          renderChat();
        } catch (e) {
          console.error("renderChat", e);
        }
        try {
          renderChatStarmap();
        } catch (e) {
          console.error("renderChatStarmap", e);
        }
        try {
          renderPlugins();
        } catch (e) {
          console.error("renderPlugins", e);
        }
        try {
          renderKernel();
        } catch (e) {
          console.error("renderKernel", e);
        }
        try {
          renderOneSettings();
        } catch (e) {
          console.error("renderOneSettings", e);
        }
        try {
          renderAdapters();
        } catch (e) {
          console.error("renderAdapters", e);
        }
        try {
          if (!window.matchMedia("(hover: none)").matches) {
            document
              .querySelectorAll(
                "#tab-overview .card, #tab-plugins .card, #tab-kernel .card",
              )
              .forEach(function (c) {
                if (!c.querySelector(".tilt-glow")) {
                  var g = document.createElement("span");
                  g.className = "tilt-glow";
                  c.appendChild(g);
                  c.classList.add("tilt");
                }
              });
          }
        } catch (e) {}
        applyI18n();
      }

      // fmtBuild 把构建身份压成一行：commit + 构建时间（日期部分）。
      //
      // 未注入 ldflags 的本地构建会得到 "unknown"，此时只显示 SDK 版本，
      // 避免把 "unknown / unknown" 这种噪音摆在概览卡上。
      function fmtBuild(b, s) {
        var parts = [];
        var commit = (b && b.commit) || (s && s.commit) || "";
        if (commit && commit !== "unknown") parts.push(commit);
        var bt = (b && b.build_time) || (s && s.build_time) || "";
        if (bt && bt !== "unknown") {
          // 2026-09-03T05:55:35Z → 2026-09-03
          parts.push(String(bt).split("T")[0]);
        }
        var sdkv = (b && b.sdk_compatible) || (s && s.sdk_version) || "";
        if (sdkv) parts.push("SDK " + sdkv);
        return parts.length ? parts.join(" · ") : "-";
      }

      function fmtUptime(ms) {
        var s = Math.floor(ms / 1000);
        if (s < 60) return s + "s";
        var m = Math.floor(s / 60);
        s = s % 60;
        if (m < 60) return m + "m " + s + "s";
        var h = Math.floor(m / 60);
        m = m % 60;
        return h + "h " + m + "m " + s + "s";
      }

      var uptimeTick = null;
      function startUptimeTicker() {
        if (uptimeTick) clearInterval(uptimeTick);
        uptimeTick = setInterval(function () {
          var el = document.querySelector("#uptime-val");
          if (el && state.startedAt) {
            var now = Date.now();
            el.textContent = fmtUptime(now - state.startedAt);
          } else if (!state.startedAt) {
            var el2 = document.querySelector("#uptime-val");
            if (el2) el2.textContent = "-";
          }
        }, 1000);
      }

      // ===== Overview =====

      // ===== 运行态面板：把内核的调度器/驻留子/通道画出来 =====
      //
      // 数据来自 /api/v1/runtime（内核 internal/sdk 暴露的 KernelStatus 子集），
      // 每 3 秒刷一次——运行态要"实时"，而 /kernel 是 30KB 级的全量状态，不适合秒级轮询。
      //
      // 四个数字块回答"现在忙不忙"：排队任务、待处理中断、中断栈深度、驻留子数量；
      // 下面的四级条形回答"堵在哪一级"；中断栈图回答"谁打断了谁"；
      // 通道拓扑回答"消息从哪儿进、能往哪儿出"。
      // 四级语义直接照抄内核（internal/agent/core/scheduler.go 的 Level 定义），
      // 别自己起名字——前端叫法一旦和内核不一致，看板就成了误导。
      var RT_LEVELS = [
        { lv: 4, name: "L4 内核独占", cls: "rt-lv-4" },
        { lv: 3, name: "L3 交互", cls: "rt-lv-3" },
        { lv: 2, name: "L2 消息", cls: "rt-lv-2" },
        { lv: 1, name: "L1 后台", cls: "rt-lv-1" },
      ];
      var RT_CAP_NAMES = { 1: "text", 2: "file", 4: "image", 8: "audio", 16: "structured" };

      // _rtSig 缓存上一次渲染的数据签名。
      //
      // 为什么必须缓存：运行态每 3s 轮询一次，数据绝大多数时候是**没变**的；
      // 无条件 `innerHTML =` 会把整块 DOM（含各级条的 transition）每 3s 重建一遍，
      // 视觉效果就是“首页一闪一闪”。签名相同就一个字节也不动。
      var _rtSig = null;

      // rtSlider 把“值 / 上限”画成轨道 + 滑块 + 读数（比纯文本数字直观）。
      function rtSlider(value, max, label, display) {
        var pct = max > 0 ? Math.max(0, Math.min(100, (value / max) * 100)) : 0;
        return (
          '<span class="rt-slider"><span class="rt-slider-label">' +
          escHtml(label) +
          '</span><span class="rt-slider-track"><i style="width:' + pct + '%"></i><b style="left:' + pct + '%"></b></span>' +
          '<span class="rt-slider-val">' +
          escHtml(String(display === undefined ? value : display)) +
          "</span></span>"
        );
      }

      // rtMini 是行内迷你条（登记/抢占这类要并排两个的量）。
      function rtMini(value, max, label) {
        var pct = max > 0 ? Math.max(0, Math.min(100, (value / max) * 100)) : 0;
        return (
          '<span class="rt-mini" title="' +
          escHtml(label) + " " + value +
          '"><span class="rt-mini-track"><i style="width:' + pct + '%"></i></span>' +
          value +
          "</span>"
        );
      }

      function rtTile(num, label, sub, pct, active, warn) {
        return (
          '<div class="rt-tile' +
          (active ? " rt-active" : "") +
          (warn ? " rt-warn" : "") +
          '"><div class="rt-num">' +
          num +
          '</div><div class="rt-label">' +
          label +
          "</div>" +
          (sub ? '<div class="rt-sub">' + sub + "</div>" : "") +
          '<div class="rt-bar"><i style="width:' +
          Math.max(0, Math.min(100, pct || 0)) +
          '%"></i></div></div>'
        );
      }

      function rtCapsHtml(caps) {
        var out = "";
        Object.keys(RT_CAP_NAMES).forEach(function (bit) {
          if (caps & Number(bit)) {
            out += '<span class="rt-cap">' + RT_CAP_NAMES[bit] + "</span>";
          }
        });
        return out ? '<span class="rt-caps">' + out + "</span>" : "";
      }

      function renderRuntime() {
        var el = document.getElementById("rt-panel");
        if (!el) return;
        var rt = state.runtime;
        if (!rt) {
          el.innerHTML = '<div class="rt-empty">' + __("运行态数据不可用", "runtime unavailable") + "</div>";
          _rtSig = null;
          return;
        }
        var sc = rt.scheduler || {};
        var residents = rt.residents || [];
        var channels = rt.channels || [];
        var inputs = rt.input_channels || [];

        // 数据签名（**不含 uptime**——它每秒都变，带上就等于没缓存）：整体没变就整块跳过。
        var sig = JSON.stringify([sc, residents, channels, inputs]);
        if (sig === _rtSig) return;
        _rtSig = sig;

        // 外壳只建一次；随后**逐段**更新。
        //
        // 为什么不是整块 innerHTML：设备通道列表这类数据本来就会来回变（实测远程设备
        // 通道 11→9 条），整块重建会把没变的段落（含条形 transition）也推倒重来，
        // 视觉上就是「一闪一闪」。逐段比较后只替换真正变了的那一段。
        if (!document.getElementById("rt-sec-tiles")) {
          el.innerHTML =
            '<div class="card"><h2>' + __("运行态", "Runtime") + "</h2>" +
            '<div id="rt-sec-tiles"></div>' +
            '<div id="rt-sec-levels"></div>' +
            '<div id="rt-sec-stack"></div>' +
            '<div id="rt-sec-owners"></div>' +
            '<div id="rt-sec-topo"></div>' +
            "</div>";
        }
        function put(sec, html) {
          var n = document.getElementById("rt-sec-" + sec);
          if (!n || n.__sig === html) return; // 该段没变：一个字节都不动
          n.__sig = html;
          n.innerHTML = html;
        }

        var q = sc.interrupt_queues || [0, 0, 0, 0, 0];
        var pending = sc.pending_interrupts || 0;
        var ready = sc.ready_queue_depth || 0;
        var stack = sc.suspend_stack || 0;
        var maxStack = sc.max_suspend_depth || 4;
        var frames = sc.suspend_frames || [];
        var byLv = sc.interrupts_by_level || [];
        var preLv = sc.preempts_by_level || [];

        // ---- 段 1：四个数字块 ----
        var h = '<div class="rt-grid">';
        h += rtTile(ready, __("排队任务", "Ready queue"), __("等待执行的输入", "inputs waiting"), ready ? Math.min(100, ready * 20) : 0, ready > 0);
        h += rtTile(pending, __("待处理中断", "Pending interrupts"), __("四级队列 + 立即抢占", "queued + immediate"), pending ? Math.min(100, pending * 25) : 0, pending > 0, pending > 0);
        h += rtTile(stack + "/" + maxStack, __("中断栈", "Interrupt stack"), __("嵌套抢占的现场", "nested frames"), (stack / (maxStack || 4)) * 100, stack > 0);
        var rFull = residents.filter(function (r) { return r.context_full; }).length;
        h += rtTile(residents.length, __("驻留子 Agent", "Resident agents"), rFull ? rFull + __(" 个上下文已满", " context-full") : __("常驻子任务", "long-lived children"), residents.length ? Math.min(100, residents.length * 20) : 0, residents.length > 0, rFull > 0);
        h += "</div>";
        // 抢占/背压计数（设计 §11.6 E4 的按级别口径）
        h += '<div class="rt-chan-sub">' + __("累计", "totals") + "： " +
          __("入队", "enqueued") + " " + (sc.enqueued || 0) + " · " +
          __("执行", "executed") + " " + (sc.executed || 0) + " · " +
          __("抢占", "preempted") + " " + (sc.preempted || 0) + " · " +
          __("挂起/恢复", "susp/res") + " " + (sc.suspended || 0) + "/" + (sc.resumed || 0) + " · " +
          __("拒绝", "rejected") + " " + (sc.rejected || 0) + " · " +
          __("背压", "backpressure") + " " + (sc.backpressure || 0) + "</div>";
        put("tiles", h);

        // ---- 段 2：队列（四级中断 + 一条排队）----
        //
        // 设计是「四条中断队列（L1–L4）+ 一条排队队列」共五个，所以必须画五行：
        // 只画四条会让「排队输入」这条线在运行态里凭空消失，而它正是
        // 「不需要及时处理」的那一半输入。排队队列**无级别**，故用不同配色 + 虚线。
        h = '<div class="rt-section-title">' + __("队列（四级中断 + 排队）", "Queues (4 interrupt levels + queued)") + "</div>";
        var maxQ = Math.max(1, ready, q[1] || 0, q[2] || 0, q[3] || 0, q[4] || 0);
        var maxReg = 1;
        var maxPre = 1;
        RT_LEVELS.forEach(function (L) {
          maxReg = Math.max(maxReg, byLv[L.lv] || 0);
          maxPre = Math.max(maxPre, preLv[L.lv] || 0);
        });
        h += '<div class="rt-levels">';
        RT_LEVELS.forEach(function (L) {
          var depth = q[L.lv] || 0;
          var reg = byLv[L.lv] || 0;
          var pre = preLv[L.lv] || 0;
          h +=
            '<div class="rt-level"><span class="rt-lv-name">' + L.name + "</span>" +
            '<span class="rt-lv-track ' + L.cls + '"><i style="width:' +
            (depth ? Math.max(4, (depth / maxQ) * 100) : 0) +
            '%"></i></span>' +
            '<span class="rt-lv-meta">' + depth + " · " +
            rtMini(reg, maxReg, __("登记", "registered")) +
            rtMini(pre, maxPre, __("抢占", "preempted")) +
            "</span></div>";
        });
        h += "</div>";
        // 第五条：排队队列（无级别，纯 FIFO）
        h += '<div class="rt-level rt-level-queued"><span class="rt-lv-name">' +
          __("排队（无级别）", "queued (no level)") + "</span>" +
          '<span class="rt-lv-track rt-lv-q"><i style="width:' +
          (ready ? Math.max(4, (ready / maxQ) * 100) : 0) + '%"></i></span>' +
          '<span class="rt-lv-meta">' + ready + " · " +
          __("FIFO，可被任何中断打断", "FIFO, preempted by any interrupt") + "</span></div>";
        h += "</div>";
        if (sc.immediate) {
          h += '<div class="rt-frame">⚡ ' + __("立即运行", "immediate") + "：" +
            escHtml(sc.immediate.kind || "") + " #" + sc.immediate.id +
            '<span class="rt-frame-top">L' + (sc.immediate.level || 0) + "</span></div>";
        }
        put("levels", h);

        // ---- 段 3：中断栈 ----
        h = '<div class="rt-section-title">' + __("中断栈（栈顶在上）", "Interrupt stack (top first)") + "</div>";
        if (frames.length) {
          h += '<div class="rt-stack">';
          frames.forEach(function (f, i) {
            var t = (f && f.task) || {};
            h += '<div class="rt-frame">' + escHtml(t.kind || "") + " #" + (t.id || "?") +
              '<span class="rt-frame-top">L' + (t.level || 0) +
              (i === frames.length - 1 ? " · " + __("栈顶", "top") : "") + "</span></div>";
          });
          h += "</div>";
        } else {
          h += '<div class="rt-empty">' + __("中断栈为空（当前无被抢占的现场）", "stack empty (nothing preempted)") + "</div>";
        }
        if (sc.running) {
          h += '<div class="rt-empty">' + __("正在运行", "running") + "：" +
            escHtml(sc.running.kind || "") + " #" + sc.running.id + " (L" + (sc.running.level || 0) + ")</div>";
        }
        put("stack", h);

        // ---- 段 4：通道分配（按归属）----
        //
        // inputch 是输入路由单位，登记表由根 agent 与驻留子**共用同一份**——
        // 「这条输入归谁」必须画出来。只画设备能力（段 5）等于把「路由发生在
        // 进内核之前」这条设计事实藏起来，驻留子的通道分配就完全不可见。
        h = '<div class="rt-section-title">' + __("通道分配（按归属）", "Input channels by owner") + "</div>";
        var maxCap = 1;
        inputs.forEach(function (c) {
          if ((c.capacity || 0) > maxCap) maxCap = c.capacity;
        });
        var owners = [];
        var byOwner = {};
        inputs.forEach(function (c) {
          var o = c.owner || "";
          if (!byOwner[o]) {
            byOwner[o] = [];
            owners.push(o);
          }
          byOwner[o].push(c);
        });
        // 驻留子即使一条 inputch 都没划到也要出现在图里——否则「子存在但看不见」
        // 与「子不存在」无法区分。
        residents.forEach(function (r) {
          var o = r.id || "";
          if (o && !byOwner[o]) {
            byOwner[o] = [];
            owners.push(o);
          }
        });
        owners.sort(function (a, b) {
          if (a === "") return -1;
          if (b === "") return 1;
          return a < b ? -1 : 1;
        });
        if (!owners.length) {
          h += '<div class="rt-empty">' + __("暂无通道登记", "no channel registered") + "</div>";
        }
        owners.forEach(function (o) {
          var list = byOwner[o] || [];
          var res = null;
          if (o) {
            residents.forEach(function (r) {
              if (r.id === o) res = r;
            });
          }
          h += '<div class="rt-owner' + (o ? " rt-owner-child" : "") + '">';
          h += '<div class="rt-owner-head"><span class="rt-owner-name">' +
            (o ? "▸ " : "◆ ") +
            (o ? __("驻留子 ", "resident ") + escHtml(o) : __("根 agent / 内核默认", "root agent / kernel default")) +
            '</span><span class="rt-owner-meta">' + list.length + " " + __("条通道", "channels") +
            (res ? " · " + __("轮次", "rounds") + " " + (res.rounds || 0) : "") +
            (res && res.context_full ? ' <span class="rt-badge-warn">' + __("上下文已满", "ctx full") + "</span>" : "") +
            "</span></div>";
          if (list.length) {
            list.forEach(function (c) {
              var cap = c.capacity || 0;
              h += '<div class="rt-assign">' +
                '<span class="rt-chan-name">' + escHtml(c.name) + "</span>" +
                (c.plugin ? '<span class="rt-chip">' + escHtml(c.plugin) + "</span>" : "") +
                rtSlider(cap, maxCap, __("容量", "cap"), cap ? String(cap) : __("默认", "default")) +
                '<span class="rt-chan-sub">' +
                (c.output ? __("回程 ", "out ") + escHtml(c.output) : __("回程由来源决定", "out by source")) +
                "</span></div>";
            });
          } else {
            var allowed = (res && res.allowed_outputs) || [];
            h += '<div class="rt-chan-sub">' +
              (allowed.length
                ? __("可发往输出通道：", "allowed outputs: ") +
                  allowed.map(function (x) { return '<span class="rt-chip">' + escHtml(x) + "</span>"; }).join("")
                : __("未划入任何 inputch", "no input channel assigned")) +
              "</div>";
          }
          h += "</div>";
        });
        put("owners", h);

        // ---- 段 5：通道拓扑（设备能力面）----
        h = '<div class="rt-section-title">' + __("通道拓扑", "Channel topology") + "</div>";
        var ins = channels.filter(function (c) { return c.direction === "in" || c.direction === "io"; });
        var outs = channels.filter(function (c) { return c.direction === "out" || c.direction === "io"; });
        function chanHtml(c) {
          return '<div class="rt-chan" title="' + escHtml(c.description || "") + '">' +
            '<span class="rt-chan-name">' + escHtml(c.name) + "</span>" +
            rtCapsHtml(c.output_caps || 0) +
            '<div class="rt-chan-sub">' +
            escHtml((c.tools || []).length ? (c.tools || []).length + " " + __("个工具", "tools") : (c.description || "").slice(0, 26)) +
            "</div></div>";
        }
        h += '<div class="rt-topo">';
        h += '<div class="rt-topo-col">' + (ins.length ? ins.map(chanHtml).join("") : '<div class="rt-empty">' + __("无输入通道", "no input channel") + "</div>") + "</div>";
        h += '<div class="rt-core">' + __("内核", "Kernel") + "</div>";
        h += '<div class="rt-topo-col rt-right">' + (outs.length ? outs.map(chanHtml).join("") : '<div class="rt-empty">' + __("无输出通道", "no output channel") + "</div>") + "</div>";
        h += "</div>";
        put("topo", h);
      }

      // loadRuntime 拉运行态小快照并就地重绘面板（约 2KB，可秒级轮询）。
      async function loadRuntime() {
        try {
          var rt = await api("/runtime");
          state.runtime = rt;
          renderRuntime();
        } catch (e) {
          state.runtime = null;
        }
      }

      // startRuntimeTicker 起 3s 轮询：运行态只在总览页可见时才有意义，
      // 其它页签上不浪费请求。切回总览时 renderAll 会立刻再拉一次，不必等下一拍。
      function startRuntimeTicker() {
        if (state._runtimeTicker) return;
        state._runtimeTicker = setInterval(function () {
          var tab = document.querySelector("#tab-overview");
          if (tab && tab.classList.contains("active")) {
            loadRuntime();
          }
        }, 3000);
      }

      function renderOverview() {
        // 总览页会整块重建 #rt-panel（面板本身是空的），所以必须让运行态的
        // 数据签名失效，否则 renderRuntime 会以为“没变化”而不去填这块空面板。
        _rtSig = null;
        var s = state.status || {};
        var k = state.kernel;
        var html =
          '<div id="rt-panel"></div>' +
          '<div class="card"><h2>' +
          __("系统概览", "System Overview") +
          "</h2>" +
          '<div class="kv-row"><span class="key">' +
          __("运行状态", "Status") +
          '</span><span class="val"><span class="status-dot ' +
          (s.status === "running" ? "dot-green" : "dot-yellow") +
          '"></span>' +
          escHtml(s.status || "unknown") +
          "</span></div>" +
          '<div class="kv-row"><span class="key">' +
          __("运行时间", "Uptime") +
          '</span><span class="val"><span id="uptime-val">' +
          (state.startedAt ? fmtUptime(Date.now() - state.startedAt) : "-") +
          "</span></span></div>" +
          '<div class="kv-row"><span class="key">' +
          __("插件", "Plugins") +
          '</span><span class="val">' +
          ((k?.plugins || []).length || 0) +
          "</span></div>" +
          '<div class="kv-row"><span class="key">' +
          __("版本", "Version") +
          '</span><span class="val">' +
          escHtml(
            k?.build?.version
              ? (k.build.kernel_name || "HomeAgent") + " v" + k.build.version
              : s.version
                ? "v" + s.version
                : "-",
          ) +
          "</span></div>" +
          // AGPL-3.0 §13：向网络使用者提供取得对应源码的入口。
          // 地址来自内核 meta.SourceURL（构建时可 -ldflags 覆盖），
          // 修改后对外部署的分支必须把它指向自己的源码仓库。
          (k?.build?.source_url
            ? '<div class="kv-row"><span class="key">' +
              __("源码", "Source") +
              '</span><span class="val"><a href="' +
              escHtml(k.build.source_url) +
              '" target="_blank" rel="noopener noreferrer">' +
              escHtml(k.build.source_url) +
              "</a></span></div>"
            : "") +
          '<div class="kv-row"><span class="key">' +
          __("构建", "Build") +
          '</span><span class="val">' +
          escHtml(fmtBuild(k?.build, s)) +
          "</span></div></div>";
        if (k) {
          html +=
            '<div class="card"><h2>' +
            __("LLM 状态", "LLM Status") +
            "</h2>" +
            '<div class="kv-row"><span class="key">Provider</span><span class="val">' +
            (k.llm?.provider || __("未配置", "Not configured")) +
            "</span></div>" +
            '<div class="kv-row"><span class="key">' +
            __("可用源", "Sources") +
            '</span><span class="val">' +
            (k.llm?.sources || 0) +
            "</span></div>" +
            '<div class="kv-row"><span class="key">' +
            __("状态", "Status") +
            '</span><span class="val"><span class="status-dot ' +
            (k.llm?.available ? "dot-green" : "dot-red") +
            '"></span>' +
            (k.llm?.available
              ? __("运行中", "Running")
              : __("不可用", "Unavailable")) +
            "</span></div></div>";
          html +=
            '<div class="card"><h2>' +
            __("记忆状态", "Memory Status") +
            "</h2>" +
            '<div class="kv-row"><span class="key">' +
            __("图记忆", "Graph Memory") +
            '</span><span class="val"><span class="status-dot ' +
            (k.memory?.available ? "dot-green" : "dot-gray") +
            '"></span>' +
            (k.memory?.available
              ? k.memory.entity_count +
                __(" 实体, ", " entities, ") +
                k.memory.relation_count +
                __(" 关系", " relations")
              : __("未初始化", "Uninitialized")) +
            "</span></div>" +
            '<div class="kv-row"><span class="key">' +
            __("文档记忆", "Document Memory") +
            '</span><span class="val">' +
            (k.documents?.available
              ? k.documents.doc_count + __(" 文档", " docs")
              : __("未初始化", "Uninitialized")) +
            "</span></div>" +
            '<div class="kv-row"><span class="key">' +
            __("文本记忆", "Text Memory") +
            '</span><span class="val">' +
            (k.text_memory?.available
              ? k.text_memory.file_count + __(" 文件", " files")
              : __("未初始化", "Uninitialized")) +
            "</span></div>" +
            '<div class="kv-row"><span class="key">' +
            __("知识库", "Knowledge") +
            '</span><span class="val">' +
            (k.knowledge?.available
              ? k.knowledge.item_count + __(" 项", " items")
              : __("未初始化", "Uninitialized")) +
            "</span></div></div>";
          html +=
            '<div class="card"><h2>' +
            __("运行时", "Runtime") +
            "</h2>" +
            '<div class="kv-row"><span class="key">Goroutines</span><span class="val">' +
            (k.runtime?.goroutines || "-") +
            "</span></div>" +
            '<div class="kv-row"><span class="key">' +
            __("内存", "Memory") +
            '</span><span class="val">' +
            (k.runtime?.memory_mb ? k.runtime.memory_mb + " MB" : "-") +
            "</span></div>" +
            '<div class="kv-row"><span class="key">Go ' +
            __("版本", "Version") +
            '</span><span class="val">' +
            escHtml(k.runtime?.go_version || "-") +
            "</span></div></div>";
        }
        document.getElementById("tab-overview").innerHTML = html;
      }

      // ===== Chat =====
      var _chatLayoutBuilt = false;

      function buildChatLayout() {
        var cont = document.getElementById("tab-chat");
        var k = state.kernel || {};
        var html = '<div class="chat-layout">';
        html +=
          '<div class="chat-tabs">' +
          '<span class="active" onclick="switchChatPanel(\'chat\',this)">' +
          __("对话", "Chat") +
          "</span>" +
          "<span onclick=\"switchChatPanel('starmap',this)\">" +
          __("星图", "Star Map") +
          "</span>" +
          "<span onclick=\"switchChatPanel('terminal',this)\">" +
          __("终端", "Terminal") +
          "</span>" +
          "<span onclick=\"switchChatPanel('cmd',this)\">" +
          __("运行中命令", "Running Commands") +
          "</span>" +
          "<span onclick=\"switchChatPanel('memory',this)\">" +
          __("记忆", "Memory") +
          "</span>" +
          "<span onclick=\"switchChatPanel('context',this)\">" +
          __("上下文", "Context") +
          "</span>" +
          "<span onclick=\"switchChatPanel('knowledge',this)\">" +
          __("知识", "Knowledge") +
          "</span></div>";
        html +=
          '<div class="chat-panel active" id="chat-panel-chat"><div class="chat-main">';
        html +=
          '<div class="card"><h2>' +
          __("对话", "Chat") +
          ' <span id="chat-stage" class="badge" style="font-size:10px;font-weight:400;display:none">' +
          escHtml(state.chatStage || "") +
          '</span></h2><div class="chat-messages" id="chat-msgs">';
        if (state.messages.length === 0) {
          html +=
            '<div class="empty-state" style="flex:1;display:flex;align-items:center;justify-content:center"><p>' +
            __(
              "开始对话以测试 Agent 回复",
              "Start a conversation to test Agent replies",
            ) +
            "</p></div>";
        }
        html +=
          "</div>" +
          '<div class="chat-input-row">' +
          '<input type="file" id="chat-file" style="display:none" onchange="sendChatFile(this.files[0])">' +
          '<button class="btn" onclick=\'document.getElementById("chat-file").click()\' id="chat-file-btn" title="' +
          __("发送文件", "Send file") +
          '" style="padding:0 12px;display:flex;align-items:center">' +
          '<svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M21.44 11.05l-9.19 9.19a6 6 0 01-8.49-8.49l9.19-9.19a4 4 0 015.66 5.66l-9.2 9.19a2 2 0 01-2.83-2.83l8.49-8.48"/></svg>' +
          "</button>" +
          '<input id="chat-input" placeholder="' +
          __("输入消息...", "Type a message...") +
          '" onkeydown="if(event.key==\'Enter\')sendChat()">' +
          '<button class="btn" onclick="interruptChat()" id="chat-stop-btn" style="display:none;background:var(--danger, #d1383d);color:#fff">' +
          __("停止", "Stop") +
          "</button>" +
          '<button class="btn btn-primary" onclick="sendChat()" id="chat-send-btn">' +
          __("发送", "Send") +
          "</button>" +
          "</div></div></div></div>";
        html +=
          '<div class="chat-panel" id="chat-panel-starmap"><div class="card"><h2>' +
          __("星图", "Star Map") +
          "</h2>" +
          '<div id="sm-container-chat" style="display:flex;align-items:center;justify-content:center;min-height:480px"><div class="loading-spinner"></div></div></div></div>';
        html +=
          '<div class="chat-panel" id="chat-panel-terminal"><div class="card"><h2>' +
          __("终端", "Terminal") +
          ' <span id="term-count-badge" class="badge badge-blue">0</span></h2>' +
          '<div id="term-list" style="max-height:60vh;overflow-y:auto;font-size:12px"></div></div></div>';
        html +=
          '<div class="chat-panel" id="chat-panel-cmd"><div class="card"><h2>' +
          __("命令历史", "Command History") +
          ' <span id="cmd-count-badge" class="badge badge-blue">0</span></h2>' +
          '<div id="cmd-list" style="max-height:60vh;overflow-y:auto;font-size:12px"></div></div></div>';
        html +=
          '<div class="chat-panel" id="chat-panel-memory"><div class="card"><h2>' +
          __("记忆", "Memory") +
          "</h2>" +
          '<div class="kv-row"><span class="key">' +
          __("实体", "Entities") +
          '</span><span class="val">' +
          (k?.memory?.entity_count || "-") +
          "</span></div>" +
          '<div class="kv-row"><span class="key">' +
          __("关系", "Relations") +
          '</span><span class="val">' +
          (k?.memory?.relation_count || "-") +
          "</span></div>" +
          '<div style="margin-top:8px">' +
          '<input id="mem-query" placeholder="' +
          __("关键词查询", "Keyword query") +
          '">' +
          '<button class="btn btn-primary btn-sm" onclick="queryMemoryChat()">' +
          __("查询", "Query") +
          "</button>" +
          '</div><div id="mem-result-chat" style="margin-top:8px;max-height:180px;overflow:auto"></div>' +
          "</div></div>";
        html +=
          '<div class="chat-panel" id="chat-panel-context"><div class="card"><h2>' +
          __("上下文", "Context") +
          "</h2>" +
          '<div style="margin-top:8px">' +
          '<input id="ctx-query" placeholder="' +
          __("输入当前话题", "Enter current topic") +
          '">' +
          '<button class="btn btn-primary btn-sm" onclick="queryMemoryContext()">' +
          __("获取上下文", "Get Context") +
          "</button>" +
          '</div><div id="ctx-result" style="margin-top:8px;max-height:200px;overflow:auto"></div>' +
          "</div></div>";
        html +=
          '<div class="chat-panel" id="chat-panel-knowledge"><div class="card"><h2>' +
          __("知识", "Knowledge") +
          "</h2>" +
          '<div class="kv-row"><span class="key">' +
          __("项目", "Items") +
          '</span><span class="val">' +
          (k?.knowledge?.item_count || "-") +
          "</span></div>" +
          '<div style="margin-top:8px">' +
          '<input id="know-query" placeholder="' +
          __("搜索知识", "Search knowledge") +
          '">' +
          '<button class="btn btn-primary btn-sm" onclick="searchKnowledgeChat()">' +
          __("搜索", "Search") +
          "</button>" +
          '</div><div id="know-result-chat" style="margin-top:8px;max-height:180px;overflow:auto"></div>' +
          '<div style="margin-top:12px;border-top:1px solid var(--border-color);padding-top:8px">' +
          '<input id="know-name" placeholder="' +
          __("知识名称", "Knowledge name") +
          '" style="margin-bottom:4px">' +
          '<textarea id="know-content" placeholder="' +
          __("内容", "Content") +
          '" style="min-height:50px;margin-bottom:4px"></textarea>' +
          '<button class="btn btn-primary btn-sm" onclick="createKnowledgeChat()">' +
          __("创建", "Create") +
          "</button>" +
          "</div></div></div>";
        html += "</div>";
        cont.innerHTML = html;
        _chatLayoutBuilt = true;
      }

      var CHAN_COLORS = [
        "#e08a5f",
        "#5f9fe0",
        "#6bbf8f",
        "#c06bbf",
        "#d9a13b",
        "#5fb3bf",
        "#b06b6b",
        "#7f8ce0",
      ];

      function chanColor(src) {
        var h = 0;
        for (var i = 0; i < src.length; i++) {
          h = (h * 31 + src.charCodeAt(i)) >>> 0;
        }
        return CHAN_COLORS[h % CHAN_COLORS.length];
      }

      function chanLetter(src) {
        var s = (src || "").trim();
        if (!s) return "C";
        var ch = s.charAt(0).toUpperCase();
        return /[A-Za-z0-9]/.test(ch) ? ch : "C";
      }

      function renderChat() {
        if (!_chatLayoutBuilt) {
          buildChatLayout();
          renderChatStarmap();
          renderTerminals();
          renderCmdHistory();
        }
        var msgsEl = document.getElementById("chat-msgs");
        if (!msgsEl) return;
        if (!msgsEl._stickBound) {
          msgsEl._stickBound = true;
          msgsEl.addEventListener(
            "scroll",
            function () {
              state.chatStick =
                msgsEl.scrollHeight - msgsEl.scrollTop - msgsEl.clientHeight <
                80;
              // 触顶（近顶部 60px）且服务端还有更早历史 → 向上懒加载下一页
              if (msgsEl.scrollTop < 60 && state.chatHasMore) {
                loadOlderChat();
              }
            },
            { passive: true },
          );
          msgsEl.addEventListener(
            "load",
            function () {
              if (state.chatStick === true) {
                msgsEl.scrollTop = msgsEl.scrollHeight;
              }
            },
            true,
          );
        }
        var msgs = state.messages;
        var sig =
          msgs
            .map(function (m) {
              var c = m.content || "";
              return (
                (m.role || "") +
                ":" +
                c.length +
                ":" +
                c.slice(-40) +
                ":" +
                (m.tool_calls || [])
                  .map(function (t) {
                    return (t.tool || t.name || "") + "/" + (t.status || "");
                  })
                  .join(",")
              );
            })
            .join("|") +
          "|L" +
          (state.chatLoading ? "1" : "0") +
          "|P" +
          (state.pendingTools || []).join(",");
        if (msgsEl._chatSig === sig && msgsEl.childElementCount > 0) {
          if (state.chatStick === true) {
            msgsEl.scrollTop = msgsEl.scrollHeight;
          }
          return;
        }
        msgsEl._chatSig = sig;
        var prevPending = msgsEl._lastPending || [];
        var newPending = (state.pendingTools || []).slice();
        if (state.chatLoading) {
          var lastM = msgs.length ? msgs[msgs.length - 1] : null;
          if (lastM && lastM.role === "assistant") {
            (lastM.tool_calls || []).forEach(function (tc) {
              if (!tc.result && tc.status !== "denied") {
                var nm = tc.tool || tc.name || "";
                if (newPending.indexOf(nm) === -1) newPending.push(nm);
              }
            });
          }
        }
        var newlyDone = prevPending.filter(function (n) {
          return newPending.indexOf(n) === -1;
        });
        msgsEl._lastPending = newPending;
        var streamingLast = !!(
          state.chatLoading &&
          lastM &&
          lastM.role === "assistant" &&
          !lastM._final
        );
        function pillHtml() {
          var s = "";
          newPending.forEach(function (nm) {
            var anim = prevPending.indexOf(nm) !== -1 ? "" : " pill-in";
            s +=
              '<span class="thinking-tool' +
              anim +
              '" data-tool="' +
              escHtml(nm) +
              '">' +
              '<svg viewBox="0 0 24 24" width="11" height="11" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M14.7 6.3a4 4 0 0 0-5.4 5.4L3 18l3 3 6.3-6.3a4 4 0 0 0 5.4-5.4l-2.9 2.9-2.5-.6-.6-2.5z"/></svg>' +
              escHtml(nm) +
              "</span>";
          });
          return s;
        }
        var html = "";
        if (msgs.length === 0) {
          html =
            '<div class="empty-state" style="flex:1;display:flex;align-items:center;justify-content:center"><p>' +
            __(
              "开始对话以测试 Agent 回复",
              "Start a conversation to test Agent replies",
            ) +
            "</p></div>";
        } else {
          msgs.forEach(function (m, i) {
            var role = m.role || "user";
            var c = m.content || "";
            // 附件消息：agent 经 output_send__webui 发送，或用户上传。
            // 用 role 区分方向（user=右侧你发的 / assistant=左侧小宅发的），
            // 卡片加来源标签避免混淆。
            if (m.attachment) {
              var att = m.attachment;
              var isUserAtt = role === "user";
              var srcLabel = isUserAtt
                ? __("你发送的", "You sent")
                : __("小宅发送的", "Sent by agent");
              var attHtml = "";
              if (att.type === "image") {
                attHtml =
                  '<a href="' +
                  escHtml(att.url) +
                  '" target="_blank" rel="noopener">' +
                  '<img class="chat-attachment-img" src="' +
                  escHtml(att.url) +
                  '" ' +
                  'alt="image" loading="lazy" style="max-width:320px;max-height:240px;border-radius:10px;display:block;cursor:zoom-in" ' +
                  'onerror="this.parentElement.innerHTML=\'<span class=\\"att-err\\">图片加载失败</span>\'"/></a>';
              } else if (att.type === "audio") {
                // 音频用原生播放器：与图片同理，附件能在聊天里直接消费才算可见。
                // preload="metadata" 只拉时长不拉全部字节，避免历史消息满屏时并发下载。
                attHtml =
                  '<audio class="chat-attachment-audio" controls preload="metadata" src="' +
                  escHtml(att.url) +
                  '" style="max-width:320px;display:block"></audio>' +
                  '<a href="' +
                  escHtml(att.url) +
                  '" download style="font-size:12px;color:var(--text-muted,#888);text-decoration:none">' +
                  escHtml(att.name || "audio") +
                  (att.size ? " (" + formatBytes(att.size) + ")" : "") +
                  "</a>";
              } else {
                var sizeStr = att.size ? formatBytes(att.size) : "";
                attHtml =
                  '<a class="chat-attachment-file" href="' +
                  escHtml(att.url) +
                  '" download ' +
                  'style="display:inline-flex;align-items:center;gap:8px;padding:8px 14px;border-radius:10px;background:' +
                  (isUserAtt
                    ? "var(--accent-weak, rgba(74,144,217,0.12))"
                    : "var(--bg-sec,#f0f2f5)") +
                  ';text-decoration:none;color:inherit">' +
                  '<svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="2"><path d="M21 15v4a2 2 0 01-2 2H5a2 2 0 01-2-2v-4M7 10l5 5 5-5M12 15V3"/></svg>' +
                  "<span>" +
                  escHtml(att.name || "附件") +
                  (sizeStr ? " <small>(" + sizeStr + ")</small>" : "") +
                  "</span></a>";
              }
              html +=
                '<div class="msg ' +
                (isUserAtt ? "user" : "assistant") +
                '"><div class="msg-bubble"><div class="att-wrap">' +
                '<div style="font-size:11px;opacity:0.65;margin-bottom:4px">' +
                srcLabel +
                "</div>" +
                attHtml +
                (c ? '<div class="text">' + renderMd(c) + "</div>" : "") +
                "</div></div></div>";
              return;
            }
            if (role === "assistant") {
              c = renderMd(c);
            } else if (role === "system") {
              c = escHtml(c);
            } else {
              c = escHtml(c);
            }
            var isChan = !!(m.source && m.source !== "webui");
            var rc = "";
            if (m.reasoning_content) {
              rc = renderReasoningCard(m.reasoning_content, isStreamingLast);
            }
            var tcs = "";
            if (m.tool_calls && m.tool_calls.length > 0) {
              m.tool_calls.forEach(function (tc) {
                var argsStr =
                  typeof tc.args === "object"
                    ? JSON.stringify(tc.args, null, 1)
                    : tc.args || "";
                var resultStr = tc.result
                  ? typeof tc.result === "object"
                    ? JSON.stringify(tc.result, null, 1)
                    : String(tc.result)
                  : "";
                var running = !resultStr && tc.status !== "denied";
                var error =
                  tc.status === "error" || tc.status === "denied" || !!tc.error;
                var drip =
                  newlyDone.indexOf(tc.tool || tc.name || "") !== -1
                    ? " tool-drip-in"
                    : "";
                var iconSvg = error
                  ? '<svg viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><circle cx="12" cy="12" r="9"/><path d="M5.6 5.6l12.8 12.8"/></svg>'
                  : running
                    ? '<span class="tc-spinner"></span>'
                    : '<svg viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M14.7 6.3a4 4 0 0 0-5.4 5.4L3 18l3 3 6.3-6.3a4 4 0 0 0 5.4-5.4l-2.9 2.9-2.5-.6-.6-2.5z"/></svg>';
                var statusHtml =
                  tc.status === "denied"
                    ? '<span class="tc-state tc-deny">' +
                      __("已拒绝", "Denied") +
                      "</span>"
                    : running
                      ? '<span class="tc-state tc-run">' +
                        __("调用中", "Running") +
                        "</span>"
                      : '<span class="tc-state tc-done">' +
                        __("完成", "Done") +
                        "</span>";
                var pluginHtml = tc.plugin
                  ? '<span class="tc-plugin">' + escHtml(tc.plugin) + "</span>"
                  : "";
                tcs +=
                  '<div class="tool-card' +
                  (error ? " tc-error" : running ? " tc-running" : " tc-done") +
                  drip +
                  '" data-tool="' +
                  escHtml(tc.tool || tc.name || "") +
                  '" onclick="toggleToolCall(this)">' +
                  '<div class="tc-line"><span class="tc-ico">' +
                  iconSvg +
                  '</span><span class="tc-name">' +
                  escHtml(tc.tool || tc.name || "") +
                  "</span>" +
                  pluginHtml +
                  statusHtml +
                  '<span class="tc-caret">▾</span></div>' +
                  '<div class="tc-detail" style="display:none">' +
                  (argsStr && argsStr !== "{}"
                    ? '<div class="tc-args"><div class="tc-detail-label">' +
                      __("参数", "Args") +
                      "</div>" +
                      escHtml(argsStr) +
                      "</div>"
                    : "") +
                  (resultStr
                    ? '<div class="tc-result"><div class="tc-detail-label">' +
                      __("结果", "Result") +
                      "</div>" +
                      escHtml(resultStr) +
                      "</div>"
                    : "") +
                  "</div></div>";
              });
            }
            var body = rc + tcs;
            var growCls = m._grow ? " grow-in" : "";
            if (m._grow) m._grow = false;
            var isStreamingLast = i === msgs.length - 1 && streamingLast;
            if (isStreamingLast) {
              var liveRow =
                '<span class="live-spinner"></span>' +
                (newPending.length
                  ? '<span class="thinking-tools">' + pillHtml() + "</span>"
                  : "");
              if (c) {
                body +=
                  '<div class="msg-bubble' +
                  growCls +
                  '">' +
                  liveRow +
                  '<div class="text">' +
                  c +
                  "</div></div>";
                c = "";
              } else {
                body += '<div class="msg-bubble">' + liveRow + "</div>";
              }
            } else if (c) {
              body +=
                '<div class="msg-bubble' +
                growCls +
                '"><div class="text">' +
                c +
                "</div></div>";
            }
            if (role === "system") {
              html +=
                '<div class="msg msg-system"><div class="msg-bubble">' +
                (c || "") +
                "</div></div>";
            } else if (isChan) {
              html +=
                '<div class="msg msg-channel">' +
                '<div class="msg-avatar chan-avatar" style="background:' +
                chanColor(m.source) +
                '">' +
                chanLetter(m.source) +
                "</div>" +
                '<div class="msg-content"><div class="msg-chan-name">' +
                escHtml(m.source) +
                "</div>" +
                body +
                "</div></div>";
            } else {
              var userAvatar =
                '<svg viewBox="0 0 24 24" style="width:16px;height:16px" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><circle cx="12" cy="8" r="4"/><path d="M4 20c0-4 4-6 8-6s8 2 8 6"/></svg>';
              var aiAvatar = '<img src="/mascot.webp" alt="小宅">';
              html +=
                '<div class="msg msg-' +
                role +
                '">' +
                '<div class="msg-avatar">' +
                (role === "user" ? userAvatar : aiAvatar) +
                "</div>" +
                '<div class="msg-content">' +
                body +
                "</div>" +
                "</div>";
            }
          });
        }
        if (state.chatLoading && !streamingLast) {
          var aiAvatarL = '<img src="/mascot.webp" alt="小宅">';
          html +=
            '<div class="msg msg-assistant"><div class="msg-avatar">' +
            aiAvatarL +
            '</div><div class="msg-content"><div class="msg-bubble">' +
            '<span class="live-spinner"></span>' +
            (newPending.length
              ? '<span class="thinking-tools">' + pillHtml() + "</span>"
              : "") +
            "</div></div></div>";
        }
        // 重建前记住阅读位置：非粘底（用户正向上翻）时，innerHTML 重建后必须把位置还回去，
        // 否则视口会被重置——这就是"聊天记录跳到顶部"的直接来源。
        var prevTop = msgsEl.scrollTop;
        msgsEl.innerHTML = html;
        if (state.chatStick !== false) {
          msgsEl.scrollTop = msgsEl.scrollHeight;
        } else {
          msgsEl.scrollTop = prevTop;
        }
        updateChatBadge();
      }

      function updateChatBadge() {
        var badge = document.getElementById("chat-stage");
        if (!badge) return;
        badge.textContent = state.chatStage || "";
        badge.style.display = "none";
      }

      // 流式渲染分发：流式增量（只更新最后一条正文）vs 全量重建
      var _rerenderTimer = null;
      function rerenderChat(full) {
        // 流式中且非强制全量：走增量路径（防抖合并 chunk，只更新最后一条消息节点）
        if (!full && state.chatLoading) {
          var msgs = state.messages;
          var last = msgs.length ? msgs[msgs.length - 1] : null;
          if (last && last.role === "assistant" && !last._final) {
            if (_rerenderTimer) return; // 已有排程的增量更新
            _rerenderTimer = setTimeout(function () {
              _rerenderTimer = null;
              renderChatStreamChunk();
            }, 90);
            return;
          }
        }
        // 非流式（完成/工具/历史变化）：全量重渲（含防抖合并）
        if (_rerenderTimer) clearTimeout(_rerenderTimer);
        _rerenderTimer = setTimeout(function () {
          _rerenderTimer = null;
          renderChat();
          if (full) {
            renderChatStarmap();
            renderTerminals();
            renderCmdHistory();
          }
        }, 90);
      }

      // 流式增量渲染（移植自 GUI）：仅更新最后一条 assistant 消息的正文与思考预览，
      // 不重建 DOM。正文节流 parse（>200 字符或 >300ms 才 renderMd），小增量纯文本追加。
      function renderChatStreamChunk() {
        var msgsEl = document.getElementById("chat-msgs");
        var last = state.messages.length
          ? state.messages[state.messages.length - 1]
          : null;
        if (!msgsEl || !last) return;
        var el = msgsEl.lastElementChild;
        if (!el) {
          renderChat();
          return;
        }
        var textEl = el.querySelector(".msg-bubble .text");
        var c = last.content || "";
        if (textEl && c) {
          var now = Date.now();
          var lastParse = el.__lastParse || 0;
          var lastLen = el.__lastLen || 0;
          if (c.length - lastLen > 200 || now - lastParse > 300) {
            textEl.innerHTML = renderMd(c);
            el.__lastParse = now;
            el.__lastLen = c.length;
          } else {
            var tail = c.slice(lastLen);
            if (tail) textEl.appendChild(document.createTextNode(tail));
            el.__lastLen = c.length;
          }
          if (state.chatStick !== false) {
            try {
              msgsEl.scrollTop = msgsEl.scrollHeight;
            } catch (e) {}
          }
          return;
        }
        // 思考预览更新（流式中折叠，只刷 preview 文本）
        var rcPrev = el.querySelector(".reasoning-preview");
        if (rcPrev && last.reasoning_content) {
          rcPrev.textContent = last.reasoning_content
            .replace(/[\s\n]+/g, " ")
            .slice(0, 60);
          if (state.chatStick !== false) {
            try {
              msgsEl.scrollTop = msgsEl.scrollHeight;
            } catch (e) {}
          }
          return;
        }
        // 结构变化兜底：全量
        renderChat();
      }
      function toggleToolCall(el) {
        var d = el.querySelector(".tc-detail");
        if (!d) return;
        var open = d.style.display !== "none";
        d.style.display = open ? "none" : "block";
        if (open) {
          el.classList.remove("open");
        } else {
          el.classList.add("open");
        }
      }

      function renderReasoningCard(text, isStreaming) {
        var preview =
          typeof marked !== "undefined"
            ? text.replace(/[\s\n]+/g, " ").slice(0, 60)
            : escHtml(text)
                .replace(/<[^>]+>/g, " ")
                .slice(0, 60);
        return (
          '<div class="reasoning-card' +
          (isStreaming ? " rc-streaming" : "") +
          '">' +
          '<div class="reasoning-head" onclick="toggleReasoning(this)">' +
          '<svg viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="rc-ico"><path d="M9 3a2 2 0 0 0-2 2v2a2 2 0 0 1-2 2H3a2 2 0 0 0-2 2v6a2 2 0 0 0 2 2h2a2 2 0 0 1 2 2v2a2 2 0 0 0 2 2h1a2 2 0 0 0 2-2v-2a2 2 0 0 1 2-2h2a2 2 0 0 0 2-2V7a2 2 0 0 0-2-2h-2a2 2 0 0 1-2-2V3a2 2 0 0 0-2-2H9zM12 8v4m0 4h.01"/></svg>' +
          '<span class="rc-title">' +
          (isStreaming
            ? __("思考中...", "Thinking...")
            : __("思考", "Thinking")) +
          "</span>" +
          '<span class="rc-chev">▾</span></div>' +
          '<div class="reasoning-body" style="display:' +
          (isStreaming ? "block" : "none") +
          '">' +
          (isStreaming
            ? '<div class="reasoning-preview">' +
              escHtml(preview) +
              '</div><div class="reasoning-sweep"></div>'
            : '<div class="reasoning-content">' + renderMd(text) + "</div>") +
          "</div></div>"
        );
      }
      function toggleReasoning(el) {
        var card = el.closest(".reasoning-card");
        if (!card) return;
        var body = card.querySelector(".reasoning-body");
        if (!body) return;
        var open = body.style.display !== "none";
        body.style.display = open ? "none" : "block";
        if (open) {
          card.classList.remove("open");
        } else {
          card.classList.add("open");
        }
      }

      function renderChatStarmap() {
        var cont = document.getElementById("sm-container-chat");
        if (!cont) return;
        if (
          window._THREE_FAILED ||
          (!window.THREE && window._THREE_FAILED !== undefined)
        ) {
          cont.innerHTML =
            '<p style="color:var(--text-muted);padding:20px;text-align:center;font-size:11px">' +
            __(
              "3D 星图不可用（CDN 加载失败）",
              "Star map unavailable (CDN load failed)",
            ) +
            "</p>";
          state.starmapInit = true;
          state.starmapLoading = false;
          return;
        }
        if (!window.THREE) {
          cont.innerHTML =
            '<div style="display:flex;align-items:center;justify-content:center;height:100%;padding:20px"><div class="loading-spinner"></div></div>';
          state.starmapInit = false;
          state.starmapLoading = false;
          return;
        }
        if (cont.querySelector("canvas")) {
          var rect = cont.getBoundingClientRect();
          if (starmapRen && rect.width > 0)
            starmapRen.setSize(rect.width, Math.max(rect.height, 250));
          return;
        }
        if (state.starmapInit) {
          if (starmapRen) {
            var rect = cont.getBoundingClientRect();
            if (rect.width > 0)
              starmapRen.setSize(rect.width, Math.max(rect.height, 250));
            cont.appendChild(starmapRen.domElement);
            starmapRen.domElement.style.display = "block";
          } else {
            // starmapRen was destroyed (e.g. re-render cycle), restart
            state.starmapInit = false;
            state.starmapLoading = false;
          }
          return;
        }
        if (state.starmapLoading) return;
        state.starmapLoading = true;
        loadChatStarmapData();
      }

      async function loadChatStarmapData() {
        try {
          var resp = await api("/memory/graph");
          if (
            !resp ||
            !resp.success ||
            !resp.data ||
            !resp.data.nodes ||
            resp.data.nodes.length === 0
          ) {
            document.getElementById("sm-container-chat").innerHTML =
              '<p style="color:var(--text-muted);padding:20px;text-align:center">' +
              __("暂无记忆数据", "No memory data") +
              "</p>";
            state.starmapInit = true;
            state.starmapLoading = false;
            return;
          }
          var d = resp.data;
          starmapNodes = d.nodes || [];
          starmapEdges = d.edges || [];
          state.starmapInit = true;
          state.starmapLoading = false;
          initChatStarmap();
        } catch (e) {
          document.getElementById("sm-container-chat").innerHTML =
            '<p style="color:var(--text-muted);padding:20px;text-align:center">' +
            __("加载失败", "Load failed") +
            "</p>";
          state.starmapInit = true;
          state.starmapLoading = false;
        }
      }

      function initChatStarmap() {
        var cont = document.getElementById("sm-container-chat");
        if (!cont) return;
        var rect = cont.getBoundingClientRect();
        var w = Math.max(rect.width || 300, 100);
        var h = Math.max(rect.height || 250, 100);
        if (starmapRen) {
          starmapRen.setSize(w, h);
          cont.appendChild(starmapRen.domElement);
          starmapRen.domElement.style.display = "block";
          return;
        }
        starmapScene = new THREE.Scene();
        starmapScene.fog = new THREE.FogExp2(0x0a0a1a, 0.015);
        starmapCam = new THREE.PerspectiveCamera(60, w / h, 0.1, 2000);
        starmapCam.position.set(0, 20, 40);
        starmapRen = new THREE.WebGLRenderer({ antialias: true, alpha: true });
        starmapRen.setSize(w, h);
        starmapRen.setPixelRatio(Math.min(window.devicePixelRatio, 2));
        starmapRen.setClearColor(0x0a0a1a, 1);
        cont.innerHTML = "";
        cont.appendChild(starmapRen.domElement);
        starmapCtrl = new THREE.OrbitControls(
          starmapCam,
          starmapRen.domElement,
        );
        starmapCtrl.enableDamping = true;
        starmapCtrl.dampingFactor = 0.05;
        starmapCtrl.rotateSpeed = 0.5;
        starmapCtrl.zoomSpeed = 0.8;
        var al = new THREE.AmbientLight(0x444466, 0.6);
        starmapScene.add(al);
        var dl = new THREE.DirectionalLight(0xffffff, 0.8);
        dl.position.set(50, 100, 50);
        starmapScene.add(dl);
        createStarField();
        createNebula();
        buildChatStarmapGraph();
        starmapRen.domElement.addEventListener("mousemove", onStarmapMove);
        starmapRen.domElement.addEventListener("click", onStarmapClick);
        window.addEventListener("resize", onStarmapResize);
        if (starmapRaf) cancelAnimationFrame(starmapRaf);
        starmapAnimate();
      }

      function buildChatStarmapGraph() {
        starmapNodeMeshes.forEach(function (m) {
          starmapScene.remove(m);
        });
        starmapEdgeLines.forEach(function (l) {
          starmapScene.remove(l);
        });
        starmapNodeMeshes = [];
        starmapEdgeLines = [];
        if (starmapNodes.length === 0) return;
        // Calculate node degrees for leaf node detection
        var nodeDegs = {};
        starmapNodes.forEach(function (n) {
          nodeDegs[n.id] = 0;
        });
        starmapEdges.forEach(function (e) {
          nodeDegs[e.source_id] = (nodeDegs[e.source_id] || 0) + 1;
          nodeDegs[e.target_id] = (nodeDegs[e.target_id] || 0) + 1;
        });
        var nodeMap = {};
        starmapNodes.forEach(function (n) {
          nodeMap[n.id] = n;
        });
        var sorted = starmapNodes.slice().sort(function (a, b) {
          return (b.mention_count || 0) - (a.mention_count || 0);
        });
        var mc = sorted.map(function (n) {
          return n.mention_count || 0;
        });
        var maxMc = Math.max(...mc, 1),
          minMc = Math.min(...mc, 0),
          rng = maxMc - minMc || 1;
        // Layout positions
        var pos = {};
        var baseR = 15,
          maxR = 80;
        var total = sorted.length;
        var acc = 0;
        sorted.forEach(function (n, i) {
          var m = n.mention_count || 0,
            mn = rng > 0 ? (m - minMc) / rng : 0;
          var radius = baseR + mn * (maxR - baseR);
          var baseStep = (Math.PI * 2) / total;
          var extra = mn * baseStep * 2;
          var angle = acc + extra / 2;
          acc += baseStep + extra;
          pos[n.id] = {
            x: radius * Math.cos(angle),
            y: (Math.random() - 0.5) * (10 + mn * 20),
            z: radius * Math.sin(angle),
            mn: mn,
            rad: radius,
          };
        });
        // Leaf nodes (degree 1) reposition near parent
        sorted.forEach(function (n) {
          var deg = nodeDegs[n.id] || 0;
          if (deg !== 1) return;
          var edge = starmapEdges.find(function (e) {
            return e.source_id === n.id || e.target_id === n.id;
          });
          if (!edge) return;
          var parentId =
            edge.source_id === n.id ? edge.target_id : edge.source_id;
          if (!pos[parentId]) return;
          var pp = pos[parentId];
          var m = n.mention_count || 0,
            mn = rng > 0 ? (m - minMc) / rng : 0;
          var off = 6 + mn * 8 + Math.random() * 4;
          var a2 = Math.random() * Math.PI * 2;
          pos[n.id] = {
            x: pp.x + off * Math.cos(a2),
            y: pp.y + (Math.random() - 0.5) * (4 + mn * 6),
            z: pp.z + off * Math.sin(a2),
            mn: mn,
            rad: off,
          };
        });
        // Force-directed simulation
        for (var it = 0; it < 50; it++) {
          var ids = Object.keys(pos);
          // Repulsion
          for (var i = 0; i < ids.length; i++) {
            for (var j = i + 1; j < ids.length; j++) {
              var a = pos[ids[i]],
                b = pos[ids[j]];
              var dx = a.x - b.x,
                dy = a.y - b.y,
                dz = a.z - b.z,
                d = Math.sqrt(dx * dx + dy * dy + dz * dz) + 0.1;
              var rf = 0.5 + (a.mn + b.mn) * 0.5;
              if (d < 25) {
                var force = (0.06 * rf) / Math.max(d, 0.5);
                a.x += (dx / d) * force;
                a.y += (dy / d) * force;
                a.z += (dz / d) * force;
                b.x -= (dx / d) * force;
                b.y -= (dy / d) * force;
                b.z -= (dz / d) * force;
              }
            }
          }
          // Attraction along edges
          starmapEdges.forEach(function (e) {
            var a = pos[e.source_id],
              b = pos[e.target_id];
            if (!a || !b) return;
            var dx = b.x - a.x,
              dy = b.y - a.y,
              dz = b.z - a.z,
              d = Math.sqrt(dx * dx + dy * dy + dz * dz) + 0.1;
            var af = Math.max(0.3, 1.0 - (a.mn + b.mn) * 0.3);
            if (d > 20) {
              var force = 0.04 * af;
              a.x += (dx / d) * force;
              a.y += (dy / d) * force;
              a.z += (dz / d) * force;
              b.x -= (dx / d) * force;
              b.y -= (dy / d) * force;
              b.z -= (dz / d) * force;
            }
          });
          // Centering constraint
          ids.forEach(function (id) {
            var p = pos[id];
            var dist = Math.sqrt(p.x * p.x + p.y * p.y + p.z * p.z);
            var maxA = maxR * 1.5;
            if (dist > maxA) {
              var s = maxA / dist;
              p.x *= s;
              p.y *= s;
              p.z *= s;
            }
          });
        }
        // Create nodes
        starmapNodes.forEach(function (n) {
          var p = pos[n.id];
          if (!p) return;
          var mn = n.mention_count || 0,
            mnr = rng > 0 ? (mn - minMc) / rng : 0;
          var rad = 0.5 + mnr * 2.0;
          var col = smTypeColors[n.type] || 0xcccccc;
          var ei = 0.3 + mnr * 0.7;
          var g = new THREE.SphereGeometry(rad, 16, 12);
          var mat = new THREE.MeshPhongMaterial({
            color: col,
            emissive: col,
            emissiveIntensity: ei,
            shininess: 30,
          });
          var mesh = new THREE.Mesh(g, mat);
          mesh.position.set(p.x, p.y, p.z);
          mesh.userData.nodeData = n;
          mesh.userData.nodeId = n.id;
          mesh.userData.baseEmissive = ei;
          // Glow sphere
          var gr = rad * 1.2 + mnr * 0.5;
          var gg = new THREE.SphereGeometry(gr, 16, 12);
          var gm = new THREE.MeshBasicMaterial({
            color: col,
            transparent: true,
            opacity: 0.12 + mnr * 0.08,
            side: THREE.BackSide,
            blending: THREE.AdditiveBlending,
          });
          var gs = new THREE.Mesh(gg, gm);
          mesh.add(gs);
          mesh.userData.glowSphere = gs;
          // Label sprite
          var canvas = document.createElement("canvas");
          canvas.width = 256;
          canvas.height = 64;
          var ctx = canvas.getContext("2d");
          ctx.clearRect(0, 0, 256, 64);
          ctx.font = "Bold 24px Courier New";
          ctx.textAlign = "center";
          ctx.textBaseline = "middle";
          ctx.shadowColor = "#aaccff";
          ctx.shadowBlur = 8;
          ctx.fillStyle = "#ffffff";
          ctx.fillText((n.name || n.id).substring(0, 12), 128, 32);
          var tex = new THREE.CanvasTexture(canvas);
          tex.needsUpdate = true;
          var spMat = new THREE.SpriteMaterial({
            map: tex,
            transparent: true,
            opacity: 0.9,
            depthTest: false,
            depthWrite: false,
            blending: THREE.AdditiveBlending,
          });
          var sprite = new THREE.Sprite(spMat);
          sprite.scale.set(8, 2, 1);
          sprite.position.y = rad + 2;
          mesh.add(sprite);
          starmapScene.add(mesh);
          starmapNodeMeshes.push(mesh);
        });
        // Create edges
        starmapEdges.forEach(function (e) {
          var a = pos[e.source_id],
            b = pos[e.target_id];
          if (!a || !b) return;
          var col =
            smEdgeColors[e.relation_type] || smEdgeColors[e.type] || 0x444466;
          var pts = [
            new THREE.Vector3(a.x, a.y, a.z),
            new THREE.Vector3(b.x, b.y, b.z),
          ];
          var geo = new THREE.BufferGeometry().setFromPoints(pts);
          var mat = new THREE.LineBasicMaterial({
            color: col,
            transparent: true,
            opacity: 0.4,
          });
          var line = new THREE.Line(geo, mat);
          line.userData = { edgeId: e.id, edgeData: e };
          starmapScene.add(line);
          starmapEdgeLines.push(line);
        });
      }

      // 回合收尾：由 SSE 事件（agent_output final / reset 帧）或 watchdog 驱动。
      // POST 结束 ≠ 回合结束：agent 可能还在生成（排队+长生成），提前复位
      // chatLoading 会让后续 delta 走全量重建、停止按钮消失、用户误发重复消息。
      function endChatTurn() {
        if (!state.chatLoading) return;
        state.chatLoading = false;
        state.chatStage = "";
        if (state._turnWatchdog) {
          clearTimeout(state._turnWatchdog);
          state._turnWatchdog = null;
        }
        var btn = document.getElementById("chat-send-btn");
        if (btn) {
          btn.disabled = false;
          btn.textContent = __("发送", "Send");
        }
        var sb = document.getElementById("chat-stop-btn");
        if (sb) sb.style.display = "none";
        rerenderChat(true);
      }

      // 回合看门狗：POST 已 abort 且 SSE 迟迟无终帧时兕底收尾（连接不稳/事件丢失），
      // 提示用户回复可能已生成、可刷新查看历史。避免回合永久卡在 loading。
      function armTurnWatchdog() {
        if (state._turnWatchdog) clearTimeout(state._turnWatchdog);
        state._turnWatchdog = setTimeout(function () {
          state._turnWatchdog = null;
          if (state.chatLoading) {
            endChatTurn();
            toast(
              __(
                "长时间未收到回复，连接可能不稳定；回复可能已生成，可刷新页面查看",
                "No reply received for a long time; the reply may have been generated, refresh to check",
              ),
              true,
            );
          }
        }, 120000);
      }

      // 上传文件并注入 agent：multipart POST /api/v1/chat/file。
      // 服务端落盘 uploads/ 后注入「[用户发送了文件: 名字 (大小)] 已保存到 <路径>」；
      // 回复与普通消息一样走 SSE 流式渲染。可选附言从输入框读取。
      // 拖拽文件到聊天区即发送（可选：先在输入框写附言）
      function initChatDragDrop() {
        var panel = document.getElementById("chat-panel-chat");
        if (!panel || panel.__dnd) return;
        panel.__dnd = true;
        panel.addEventListener("dragover", function (e) {
          e.preventDefault();
          panel.style.outline = "2px dashed var(--accent, #4a90d9)";
        });
        panel.addEventListener("dragleave", function () {
          panel.style.outline = "";
        });
        panel.addEventListener("drop", function (e) {
          e.preventDefault();
          panel.style.outline = "";
          if (e.dataTransfer.files && e.dataTransfer.files.length) {
            sendChatFile(e.dataTransfer.files[0]);
          }
        });
        // 粘贴截图/复制的文件直接发送
        document.addEventListener("paste", function (e) {
          var chatVisible =
            document.getElementById("chat-panel-chat") &&
            document.getElementById("chat-input");
          if (!chatVisible) return;
          var items = e.clipboardData && e.clipboardData.files;
          if (items && items.length && document.activeElement !== null) {
            sendChatFile(items[0]);
          }
        });
      }

      async function sendChatFile(fileObj, extraText) {
        if (!fileObj || state.chatLoading) return;
        var inp = document.getElementById("chat-input");
        var message = (extraText || inp.value || "").trim();
        if (inp) inp.value = "";
        var fd = new FormData();
        fd.append("file", fileObj);
        if (message) fd.append("message", message);
        state.messages.push({
          role: "user",
          content: message,
          attachment: {
            // 与服务端的 attType 判定保持一致（image/audio/file），
            // 否则乐观渲染的卡片会在 SSE 回流后变成另一种样式。
            type: /^image\//.test(fileObj.type)
              ? "image"
              : /^audio\//.test(fileObj.type)
                ? "audio"
                : "file",
            url: URL.createObjectURL(fileObj),
            name: fileObj.name,
            size: fileObj.size,
          },
        });
        rerenderChat(true);
        state.chatLoading = true;
        state.chatStage = __("等待AI回复...", "Waiting for AI...");
        rerenderChat(true);
        try {
          await api("/chat/file", { method: "POST", body: fd, rawBody: true });
          // 回复经 SSE 流式到达，这里无需处理响应体
        } catch (e) {
          toast(
            __("文件发送失败：" + e.message, "File send failed: " + e.message),
            true,
          );
          state.chatLoading = false;
          endChatTurn();
        }
      }

      async function sendChat() {
        var inp = document.getElementById("chat-input");
        var btn = document.getElementById("chat-send-btn");
        var stopBtn = document.getElementById("chat-stop-btn");
        var text = inp.value.trim();
        if (!text || state.chatLoading) return;
        state.chatStick = true;
        state.chatFinalIdx = -1;
        state.messages.push({ role: "user", content: text });
        inp.value = "";
        rerenderChat(true);
        state.chatLoading = true;
        state.chatStage = __("等待AI回复...", "Waiting for AI...");
        btn.disabled = true;
        btn.textContent = "";
        if (stopBtn) stopBtn.style.display = ""; // 生成期间可停止
        rerenderChat(true);
        // 触发式 POST：短超时仅确认受理；回复靠 SSE 流式渲染（对齐 GUI 行为）。
        try {
          var ctrl = new AbortController();
          var ackTimer = setTimeout(function () {
            ctrl.abort();
          }, 15000);
          var r = null;
          try {
            r = await api("/chat", {
              method: "POST",
              body: JSON.stringify({ message: text }),
              signal: ctrl.signal,
            });
          } catch (ackErr) {
            // 同步超时/失败：不阻塞 UI，等 SSE 兑底；明确提示"可能已发送"
            console.warn("[sendChat] trigger failed: " + ackErr.message);
            toast(
              __(
                "请求超时（可能已发送，请稍候或在收到回复前勿重复发送）",
                "Request timeout (may have been sent; wait for reply before resending)",
              ),
              true,
            );
            r = null;
          } finally {
            clearTimeout(ackTimer);
          }
          var last = state.messages[state.messages.length - 1];
          if (r && r.response) {
            if (last && last.role === "assistant" && last._streaming) {
              last.content = r.response || __("(无响应)", "(no response)");
              last._grow = true;
              if (!last.reasoning_content) {
                last.reasoning_content = r.reasoning_content || "";
              }
              last._final = true;
              delete last._streaming;
            } else {
              state.messages.push({
                role: "assistant",
                content: r.response || __("(无响应)", "(no response)"),
                reasoning_content: r.reasoning_content,
                tool_calls:
                  last && last.role === "assistant" && last.tool_calls
                    ? last.tool_calls
                    : [],
                _final: true,
                _grow: true,
              });
            }
            state.chatFinalIdx = state.messages.length - 1;
          }
          rerenderChat(true);
        } catch (e) {
          // 真实错误（非受理超时）：展示错误信息
          if (!String(e.message || "").includes("aborted")) {
            state.messages.push({
              role: "assistant",
              content: __("错误: ", "Error: ") + e.message,
              _final: true,
            });
            rerenderChat(true);
            toast(__("请求失败: ", "Request failed: ") + e.message, true);
          }
        } finally {
          if (r && r.response) {
            // 同步兜底已拿到完整回复：回合结束
            endChatTurn();
          } else {
            // 触发式受理（POST 已 abort/失败）：回合仍打开，等 SSE 流式渲染；
            // 由 agent_output final / reset 帧 / watchdog 收尾
            armTurnWatchdog();
          }
        }
      }

      // 停止生成 / 发送中断消息。核心拦截语义：有 LLM 在跑则取消当前
      // 请求并以 [中断消息] 重启轮次；无则在跑则作为普通消息处理。
      async function interruptChat() {
        try {
          await api("/chat/interrupt", {
            method: "POST",
            body: JSON.stringify({}),
          });
          toast(__("已发送中断信号", "Interrupt signal sent"));
        } catch (e) {
          toast(__("中断失败: ", "Interrupt failed: ") + e.message, true);
        }
      }

      async function queryMemoryChat() {
        var q = document.getElementById("mem-query")?.value;
        var r = document.getElementById("mem-result-chat");
        if (!r || !q) return;
        r.innerHTML = '<div class="loading"></div>';
        try {
          var data = await api(
            "/memory?q=" + encodeURIComponent(q) + "&depth=2",
          );
          r.innerHTML =
            '<pre style="font-size:11px">' +
            escHtml(JSON.stringify(data, null, 2)) +
            "</pre>";
        } catch (e) {
          r.innerHTML =
            '<p style="color:#fca5a5">' +
            __("查询失败: ", "Query failed: ") +
            escHtml(e.message) +
            "</p>";
        }
      }

      async function queryMemoryContext() {
        var q = document.getElementById("ctx-query")?.value;
        var r = document.getElementById("ctx-result");
        if (!r) return;
        r.innerHTML = '<div class="loading"></div>';
        try {
          var data = await api(
            "/memory/context?q=" + encodeURIComponent(q || ""),
          );
          var ctx = data?.context || __("无上下文", "No context");
          var summary = data?.summary || "";
          var entities = data?.entities || [];
          var tk = data?.token_estimate || 0;
          var html = '<div style="font-size:11px">';
          if (summary)
            html +=
              '<div class="kv-row"><span class="key">' +
              __("摘要", "Summary") +
              '</span><span class="val">' +
              escHtml(summary) +
              "</span></div>";
          html +=
            '<div class="kv-row"><span class="key">Token ' +
            __("预估", "Estimate") +
            '</span><span class="val">' +
            tk +
            "</span></div>";
          if (entities.length) {
            html +=
              '<div class="kv-row"><span class="key">' +
              __("实体", "Entities") +
              '</span><span class="val">' +
              entities
                .map(function (e) {
                  return escHtml(e.name || e.id || "");
                })
                .join(", ") +
              "</span></div>";
          }
          html +=
            '<pre style="font-size:11px;margin-top:8px">' +
            escHtml(ctx) +
            "</pre></div>";
          r.innerHTML = html;
        } catch (e) {
          r.innerHTML =
            '<p style="color:#fca5a5">' +
            __("获取失败: ", "Get failed: ") +
            escHtml(e.message) +
            "</p>";
        }
      }

      async function searchKnowledgeChat() {
        var q = document.getElementById("know-query")?.value;
        var r = document.getElementById("know-result-chat");
        if (!r || !q) return;
        r.innerHTML = '<div class="loading"></div>';
        try {
          var data = await api("/knowledge?q=" + encodeURIComponent(q));
          r.innerHTML =
            '<pre style="font-size:11px">' +
            escHtml(JSON.stringify(data, null, 2)) +
            "</pre>";
        } catch (e) {
          r.innerHTML =
            '<p style="color:#fca5a5">' +
            __("搜索失败: ", "Search failed: ") +
            escHtml(e.message) +
            "</p>";
        }
      }

      async function createKnowledgeChat() {
        var name = document.getElementById("know-name")?.value;
        var content = document.getElementById("know-content")?.value;
        if (!name || !content) {
          toast(
            __("名称和内容不能为空", "Name and content cannot be empty"),
            true,
          );
          return;
        }
        try {
          var r = await api("/knowledge", {
            method: "POST",
            body: JSON.stringify({ name: name, content: content }),
          });
          if (r.status || r.id) {
            toast(
              __("知识「", 'Knowledge "') + name + __("」已创建", '" created'),
            );
            document.getElementById("know-name").value = "";
            document.getElementById("know-content").value = "";
          } else {
            toast(__("创建失败", "Create failed"), true);
          }
        } catch (e) {
          toast(__("创建失败: ", "Create failed: ") + e.message, true);
        }
      }

      function switchChatPanel(tab, el) {
        var panels = {
          chat: document.getElementById("chat-panel-chat"),
          starmap: document.getElementById("chat-panel-starmap"),
          terminal: document.getElementById("chat-panel-terminal"),
          cmd: document.getElementById("chat-panel-cmd"),
          memory: document.getElementById("chat-panel-memory"),
          context: document.getElementById("chat-panel-context"),
          knowledge: document.getElementById("chat-panel-knowledge"),
        };
        Object.keys(panels).forEach(function (k) {
          var p = panels[k];
          if (p) p.classList.toggle("active", k === tab);
        });
        if (el) {
          var parent = el.parentElement;
          if (parent) {
            Array.from(parent.children).forEach(function (ch) {
              ch.classList.remove("active");
            });
            el.classList.add("active");
          }
        }
        if (tab === "starmap") {
          if (!state.starmapInit) {
            state.starmapLoading = false;
            renderChatStarmap();
          } else if (starmapRen) {
            var cont = document.getElementById("sm-container-chat");
            if (cont) {
              var rect = cont.getBoundingClientRect();
              if (rect.width > 0)
                starmapRen.setSize(rect.width, Math.max(rect.height, 250));
              if (!cont.contains(starmapRen.domElement))
                cont.appendChild(starmapRen.domElement);
            }
          }
        }
        if (tab === "chat") {
          state.chatStick = true;
          var msgsEl = document.getElementById("chat-msgs");
          if (msgsEl) {
            try {
              msgsEl.scrollTo({ top: msgsEl.scrollHeight, behavior: "smooth" });
            } catch (e) {
              msgsEl.scrollTop = msgsEl.scrollHeight;
            }
          }
        }
        if (tab === "context") queryMemoryContext();
      }

      // 首屏分段加载条数：只拉最新 N 条，向上滚动触顶时再拉更早的。
      var CHAT_PAGE_SIZE = 40;
      async function loadChatHistory() {
        try {
          var data = await api("/chat/history?limit=" + CHAT_PAGE_SIZE);
          if (data && data.messages) {
            state.messages = data.messages;
            state.chatOffset = typeof data.offset === "number" ? data.offset : 0;
            state.chatTotal = typeof data.total === "number" ? data.total : data.messages.length;
            state.chatHasMore = !!data.has_more;
          }
        } catch (e) {}
      }

      // loadOlderChat 向上翻页：拉 offset 之前的一页，前置到 messages 头部。
      // 保持滚动位置（插入前后 scrollHeight 差值补偿），避免视口跳动。
      var _loadingOlder = false;
      async function loadOlderChat() {
        if (_loadingOlder || !state.chatHasMore) return;
        _loadingOlder = true;
        var msgsEl = document.getElementById("chat-msgs");
        var prevH = msgsEl ? msgsEl.scrollHeight : 0;
        var prevTop = msgsEl ? msgsEl.scrollTop : 0;
        try {
          var before = state.chatOffset || 0;
          if (before <= 0) {
            state.chatHasMore = false;
            return;
          }
          var data = await api(
            "/chat/history?limit=" + CHAT_PAGE_SIZE + "&before=" + before,
          );
          if (data && data.messages && data.messages.length) {
            state.messages = data.messages.concat(state.messages);
            state.chatOffset = typeof data.offset === "number" ? data.offset : 0;
            state.chatHasMore = !!data.has_more;
            state.chatStick = false; // 向上翻页时不自动粘底
            renderChat();
            if (msgsEl) {
              // 补偿新增内容高度，保持用户视觉位置不变
              msgsEl.scrollTop = prevTop + (msgsEl.scrollHeight - prevH);
            }
          } else {
            state.chatHasMore = false;
          }
        } catch (e) {
        } finally {
          _loadingOlder = false;
        }
      }

      // syncChatFromHistory 增量同步：先做一次极轻的"尾巴探测"，只有尾巴变了才拉整页。
      //
      // 原先每 30s（以及每次 SSE 报错）都直接拉一页 40 条：本地实测 180KB、
      // 生产消息更大时可达 ~1MB —— 这是"每次都在发完整聊天记录"的观感来源。
      // 探测只需 1 条（约几 KB），尾巴一致就直接跳过。
      var _syncingChat = false;
      function syncChatFromHistory() {
        if (_syncingChat) return Promise.resolve();
        _syncingChat = true;
        return api("/chat/history?limit=1")
          .then(function (tail) {
            var t = tail && tail.messages && tail.messages[0];
            var local = state.messages.length
              ? state.messages[state.messages.length - 1]
              : null;
            var same =
              t &&
              local &&
              t.role === (local.role || local.Role) &&
              (t.content || "") === (local.content || local.Content || "");
            if (same) return null; // 尾巴一致：无需拉整页
            return api("/chat/history?limit=" + CHAT_PAGE_SIZE);
          })
          .then(function (data) {
            if (!data) return;
            return mergeChatFromHistory(data);
          })
          .catch(function () {})
          .then(function () {
            _syncingChat = false;
          });
      }

      // mergeChatFromHistory 把服务端的一页历史并进本地：只追加新消息，不重建已有节点。
      // 关键约束：**绝不**用更短的服务端页替换更长的本地列表（那会让用户翻上来的旧页
      // 凭空消失、视口跳回顶部）。
      function mergeChatFromHistory(data) {
        {
          if (!data || !data.messages || data.messages.length === 0) return;
          var serverMsgs = data.messages;
          var localMsgs = state.messages;
          // 首次加载（空列表）→ 全量赋值
          if (localMsgs.length === 0) {
            state.messages = serverMsgs;
            state.chatOffset = typeof data.offset === "number" ? data.offset : 0;
            state.chatHasMore = !!data.has_more;
            rerenderChat(true);
            return;
          }
          // 以「本地末尾 vs 服务端末尾」做依据。分段拉取只会回传最新页，
          // 本地已向上翻页加载了更早内容，长度上本地可能 ≥ 服务端。
          var lastLocal = localMsgs[localMsgs.length - 1];
          var lastServer = serverMsgs[serverMsgs.length - 1];
          var localContent = lastLocal.content || lastLocal.Content || "";
          var serverContent = lastServer.content || lastServer.Content || "";
          // 服务端末尾和本地末尾内容相同 → 无新增（可能裁剪过的轻量项因不含 tool_calls args/result，
          // 依赖 content+role+time 足以识别）
          if (localContent === serverContent) {
            // 检查是否本地末尾被服务端改写（如 stage 插件改写最终文本）
            if (lastServer.role === "assistant" && localContent !== serverContent && serverContent) {
              lastLocal.content = serverContent;
              if (lastServer.ReasoningContent) lastLocal.reasoning_content = lastServer.ReasoningContent;
              var msgsEl0 = document.getElementById("chat-msgs");
              if (msgsEl0 && msgsEl0.lastElementChild) {
                var textEl0 = msgsEl0.lastElementChild.querySelector(".msg-bubble .text");
                if (textEl0) textEl0.innerHTML = renderMd(serverContent);
              }
            }
            return;
          }
          // 有新增消息：服务端末尾与本地末尾不同 → 找出本地新增的差异，追加到 DOM。
          // 服务端只回传最新页，我们需在 localMsgs 末尾取交集起点（本地末尾在服务端页的位置）
          // 简化：取服务端末尾3条与本地末尾3条比对，找到重含点的下标；若找不到全量替换最新页。
          var overlap = -1;
          for (var k = Math.min(3, serverMsgs.length - 1, localMsgs.length - 1); k >= 1; k--) {
            var sMsg = serverMsgs[serverMsgs.length - 1 - k];
            var lMsg = localMsgs[localMsgs.length - 1 - k];
            if (lMsg && (lMsg.content || "") === (sMsg.content || "") && lMsg.role === sMsg.role) {
              overlap = k;
              break;
            }
          }
          var newMsgs;
          if (overlap >= 0) {
            // 本地末尾 overlap 条在服务端页里，之后的部分是新增
            newMsgs = serverMsgs.slice(serverMsgs.length - overlap);
            if (newMsgs.length === 0) return; // 无新增（内容改写走上面的分支）
          } else {
            // 找不到重合点：**不能**直接拿服务端页覆盖本地。
            // 服务端只回一页，本地翻上来的旧页更长；覆盖会同时造成两个后果：
            // 用户翻过的旧消息凭空消失、容器变矮后视口被夹回顶部。
            // 只有在服务端页不短于本地时才整体替换（那种情况下不丢内容）。
            if (serverMsgs.length >= localMsgs.length) {
              state.messages = serverMsgs;
              rerenderChat(true);
            }
            return;
          }
          var msgsEl = document.getElementById("chat-msgs");
          if (msgsEl) {
            var aiAvatar = '<img src="/mascot.webp" alt="小宅">';
            var userAvatar = '<svg viewBox="0 0 24 24" style="width:16px;height:16px" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><circle cx="12" cy="8" r="4"/><path d="M4 20c0-4 4-6 8-6s8 2 8 6"/></svg>';
            newMsgs.forEach(function (m) {
              var role = m.role || m.Role || "user";
              var c = m.content || m.Content || "";
              if (role === "assistant") c = renderMd(c); else c = escHtml(c);
              var isChan = !!(m.source && m.source !== "webui");
              var bubble = c ? '<div class="msg-bubble"><div class="text">' + c + '</div></div>' : '<div class="msg-bubble"></div>';
              if (role === "system") {
                msgsEl.insertAdjacentHTML("beforeend", '<div class="msg msg-system"><div class="msg-bubble">' + c + '</div></div>');
              } else if (isChan) {
                msgsEl.insertAdjacentHTML("beforeend", '<div class="msg msg-channel"><div class="msg-avatar chan-avatar" style="background:#888">' + (m.source||"?")[0].toUpperCase() + '</div><div class="msg-content"><div class="msg-chan-name">' + escHtml(m.source) + '</div>' + bubble + '</div></div>');
              } else {
                msgsEl.insertAdjacentHTML("beforeend", '<div class="msg msg-' + role + '"><div class="msg-avatar">' + (role === "user" ? userAvatar : aiAvatar) + '</div><div class="msg-content">' + bubble + '</div></div>');
              }
            });
            // 删除流式占位符（同步完成，下一次 SSE 会重建）
            var streamingPh = msgsEl.querySelector(".msg-streaming-ph");
            if (streamingPh) streamingPh.remove();
          }
          // 追加新消息对象到 state.messages
          Array.prototype.push.apply(state.messages, newMsgs);
          // 同步聊天占位符（如果有新消息但最后一条非 assistant → 显示流式占位）
          syncStreamingPlaceholder();
        }
      }
      // syncStreamingPlaceholder：同步聊天占位符的可见性
      function syncStreamingPlaceholder() {
        var msgsEl = document.getElementById("chat-msgs");
        if (!msgsEl) return;
        var existing = msgsEl.querySelector(".msg-streaming-ph");
        var lastMsg = state.messages.length ? state.messages[state.messages.length - 1] : null;
        var showPh = state.chatLoading && (!lastMsg || lastMsg.role !== "assistant" || lastMsg._final);
        if (showPh && !existing) {
          var aiAvatar = '<img src="/mascot.webp" alt="小宅">';
          var pillHtml = "";
          (state.pendingTools || []).forEach(function (nm) {
            pillHtml += '<span class="thinking-tool"><svg viewBox="0 0 24 24" width="11" height="11" fill="none" stroke="currentColor" stroke-width="2"><path d="M14.7 6.3a4 4 0 0 0-5.4 5.4L3 18l3 3 6.3-6.3a4 4 0 0 0 5.4-5.4l-2.9 2.9-2.5-.6-.6-2.5z"/></svg>' + escHtml(nm) + '</span>';
          });
          msgsEl.insertAdjacentHTML("beforeend", '<div class="msg msg-assistant msg-streaming-ph"><div class="msg-avatar">' + aiAvatar + '</div><div class="msg-content"><div class="msg-bubble"><span class="live-spinner"></span>' + (pillHtml ? '<span class="thinking-tools">' + pillHtml + '</span>' : '') + '</div></div></div>');
        } else if (!showPh && existing) {
          existing.remove();
        }
      }

      async function loadTerminals() {
        try {
          var data = await api("/terminals");
          if (data && data.terminals) state.terminals = data.terminals;
        } catch (e) {}
      }

      async function loadCmdHistory() {
        try {
          var data = await api("/cmd/history");
          if (data && data.history) state.cmdHistory = data.history;
        } catch (e) {}
      }

      function appendTermBuf(el, text) {
        if (!text) return;
        el.textContent += text;
        if (el.textContent.length > 262144) {
          el.textContent = el.textContent.slice(el.textContent.length - 262144);
        }
        el.scrollTop = el.scrollHeight;
      }

      function renderTerminals() {
        var r = document.getElementById("term-list");
        var cnt = document.getElementById("term-count-badge");
        if (!r) return;
        var list = state.terminals || [];
        if (cnt) cnt.textContent = list.length;
        if (list.length === 0) {
          r.innerHTML =
            '<p style="color:var(--text-muted);padding:8px;text-align:center;font-size:11px">' +
            __("暂无终端会话", "No terminal sessions") +
            "</p>";
          return;
        }
        var html = "";
        list.forEach(function (t, i) {
          var detailId = "term-detail-" + i;
          var scr = (state.termScreens && state.termScreens[t.id]) || null;
          var running = scr ? scr.running : !!t.running;
          var fullOut = scr ? scr.output : t.output || "";
          if (!fullOut) {
            fullOut =
              '<span style="color:#5c6672">' +
              __("[终端暂无输出]", "[No terminal output]") +
              "</span>";
          } else {
            fullOut = escHtml(fullOut);
          }
          html +=
            '<div style="border:1px solid var(--border-color);border-radius:6px;margin-bottom:4px;font-size:11px">';
          html +=
            '<div style="display:flex;align-items:center;gap:6px;padding:6px 8px;cursor:pointer;background:var(--bg-hover)" onclick="var d=document.getElementById(\'' +
            detailId +
            "');d.style.display=d.style.display==='none'?'block':'none'\">";
          html +=
            '<span style="font-family:monospace;font-size:10px;flex:1">' +
            escHtml(t.id || "-") +
            "</span>";
          html +=
            '<span style="flex:1;color:var(--text-muted);overflow:hidden;text-overflow:ellipsis;white-space:nowrap">' +
            escHtml(t.command || "") +
            "</span>";
          html +=
            '<span class="badge ' +
            (running ? "badge-green" : "badge-red") +
            '">' +
            (running ? __("运行中", "Running") : __("已关闭", "Closed")) +
            "</span>";
          html +=
            '<span style="color:var(--text-muted);font-size:10px">' +
            escHtml(t.created_at || "") +
            "</span>";
          html += "</div>";
          html +=
            '<div id="' +
            detailId +
            '" style="display:none;padding:8px;border-top:1px solid var(--border-color);background:var(--bg-input)">';
          html += '<div class="term-screen">';
          html +=
            '<div class="term-head"><span class="term-dot' +
            (running ? "" : " stopped") +
            '" id="term-dot-' +
            escHtml(t.id) +
            '"></span><span style="font-weight:600">' +
            escHtml(t.id) +
            "</span><span>" +
            escHtml(t.command || "") +
            '</span><span style="flex:1"></span><span>' +
            escHtml(t.uptime || "") +
            "</span></div>";
          html +=
            '<pre class="term-buf" id="term-buf-' +
            escHtml(t.id) +
            '">' +
            fullOut +
            "</pre>";
          html += "</div></div></div>";
        });
        r.innerHTML = html;
      }

      function renderCmdHistory() {
        var r = document.getElementById("cmd-list");
        var cnt = document.getElementById("cmd-count-badge");
        if (!r) return;
        var running = (state.terminals || []).filter(function (t) {
          return t.running;
        });
        if (cnt) cnt.textContent = running.length;
        if (running.length === 0) {
          r.innerHTML =
            '<p style="color:var(--text-muted);padding:8px;text-align:center;font-size:11px">' +
            __("暂无运行中的命令", "No running commands") +
            "</p>";
          return;
        }
        var html =
          '<table style="font-size:10px"><tr><th>' +
          __("命令", "Command") +
          "</th><th>" +
          __("状态", "Status") +
          "</th><th>" +
          __("运行时长", "Uptime") +
          "</th></tr>";
        running.forEach(function (t) {
          var scr = (state.termScreens && state.termScreens[t.id]) || null;
          var out = scr ? scr.output : t.output || "";
          html +=
            "<tr>" +
            '<td style="font-family:monospace;max-width:200px;overflow:hidden;text-overflow:ellipsis">' +
            escHtml(t.command || t.id || "") +
            "</td>" +
            '<td><span class="badge badge-green">' +
            __("运行中", "Running") +
            "</span></td>" +
            '<td style="color:var(--text-muted);white-space:nowrap">' +
            escHtml(t.uptime || "-") +
            "</td>" +
            "</tr>";
          if (out) {
            html +=
              "<tr>" +
              '<td colspan="3" style="padding:0"><pre style="margin:0;padding:4px 8px;max-height:120px;overflow:auto;background:var(--bg-input);border-radius:4px;font-size:10px;color:var(--text-secondary)">' +
              escHtml(out.substring(0, 2000)) +
              "</pre></td></tr>";
          }
        });
        html += "</table>";
        r.innerHTML = html;
      }

      function connectSSE() {
        if (state.eventSource) {
          console.log("[SSE] closing old connection");
          state.eventSource.close();
        }
        var es;
        try {
          es = new EventSource("/api/v1/chat/events", {
            withCredentials: true,
          });
          state.eventSource = es;
        } catch (ex) {
          console.error("[SSE] create failed", ex);
          return;
        }
        if (!es) {
          console.error("[SSE] es is null");
          return;
        }
        console.log("[SSE] connected");
        es.addEventListener("agent_output", function (e) {
          try {
            var ev = JSON.parse(e.data);
            var p = ev.payload || {};
            console.log(
              "[SSE] agent_output received",
              p.content ? p.content.substring(0, 50) : "(empty)",
            );
            if (!p.content) return;
            state.chatStage = __("AI 回复中...", "AI replying...");
            if (p.kind === "channel_output") {
              // 附件输出（output_type=image/file）：渲染为图片预览/下载卡片
              var att = null;
              if (
                p.output_type === "image" ||
                p.output_type === "audio" ||
                p.output_type === "file"
              ) {
                att = {
                  type: p.output_type,
                  url: p.url || p.content,
                  size: p.size || 0,
                  name: (p.url || "").split("/").pop() || "附件",
                };
              }
              var cm = {
                role: "assistant",
                content: att ? "" : p.content,
                source: p.channel || "",
                _final: true,
                _grow: true,
                attachment: att,
              };
              if (
                state.chatFinalIdx >= 0 &&
                state.chatFinalIdx < state.messages.length
              ) {
                state.messages.splice(state.chatFinalIdx, 0, cm);
                state.chatFinalIdx++;
              } else {
                state.messages.push(cm);
              }
              rerenderChat();
              return;
            }
            var lastM2 = state.messages.length
              ? state.messages[state.messages.length - 1]
              : null;
            if (lastM2 && lastM2.role === "assistant" && !lastM2._final) {
              // 聚合最终响应：覆盖 delta 累积的中间内容（以聚合为准，
              // 含 stage 插件改写后的最终文本），并置 final 结束本轮流式。
              lastM2._grow = true;
              lastM2.content = p.content;
              lastM2._final = true;
              rerenderChat();
              endChatTurn();
              return;
            }
            if (
              lastM2 &&
              lastM2.role === "assistant" &&
              lastM2._final &&
              !lastM2.source
            ) {
              // 去重：同一轮的重复帧（如 SSE 重连回放）内容相同则忽略；
              // 内容不同视为新一轮输出（上一轮已 final 且无 source），开新消息。
              // 旧逻辑无条件 return 会丢弃多轮连发时新一轮的最终回复。
              if (lastM2.content === p.content) {
                endChatTurn();
                return;
              }
            }
            state.messages.push({
              role: "assistant",
              content: p.content,
              _streaming: true,
              _grow: true,
              _final: true,
            });
            rerenderChat();
            endChatTurn();
          } catch (ex) {
            console.error("[SSE] agent_output error", ex);
          }
        });
        // token 级流式增量：逐块追加到当前回复内容（流式生成中）；
        // reset 帧表示轮次作废（用户中断）：定格已显示的部分内容，置 final。
        es.addEventListener("content_delta", function (e) {
          try {
            var ev = JSON.parse(e.data);
            var p = ev.payload || {};
            if (p.channel === "_consolidation_") return;
            if (p.reset) {
              var lm = state.messages.length
                ? state.messages[state.messages.length - 1]
                : null;
              if (lm && lm.role === "assistant" && !lm._final) {
                lm._final = true;
                rerenderChat();
              }
              // 轮次作废（用户中断）：定格已显示内容；核心会以 [中断消息]
              // 重启轮次，保持回合打开让确认回复继续流式渲染，
              // 由其 agent_output final / watchdog 收尾。
              armTurnWatchdog();
              return;
            }
            if (!p.content) return;
            state.chatStage = __("AI 回复中...", "AI replying...");
            var last =
              state.messages.length > 0
                ? state.messages[state.messages.length - 1]
                : null;
            if (!last || last.role !== "assistant" || last._final) {
              state.messages.push({
                role: "assistant",
                content: "",
                tool_calls: [],
                _streaming: true,
                _grow: true,
              });
              last = state.messages[state.messages.length - 1];
            }
            last.content += p.content;
            rerenderChat();
          } catch (ex) {}
        });
        es.addEventListener("terminal_output", function (e) {
          try {
            var ev = JSON.parse(e.data);
            var p = ev.payload || {};
            if (!p.terminal_id) return;
            var id = p.terminal_id;
            if (!state.termScreens) state.termScreens = {};
            var scr =
              state.termScreens[id] ||
              (state.termScreens[id] = { output: "", running: true });
            if (p.output) scr.output += p.output;
            if (typeof p.running === "boolean") scr.running = p.running;
            var bufel = document.getElementById("term-buf-" + id);
            if (bufel) {
              appendTermBuf(bufel, p.output || "");
              var dot = document.getElementById("term-dot-" + id);
              if (dot)
                dot.className = "term-dot" + (scr.running ? "" : " stopped");
            }
          } catch (ex) {
            console.error("[SSE] terminal_output error", ex);
          }
        });
        es.addEventListener("reasoning", function (e) {
          try {
            var ev = JSON.parse(e.data);
            var p = ev.payload || {};
            if (p.channel === "_consolidation_") return;
            if (p.content) {
              state.chatStage = __("AI 思考中...", "AI thinking...");
              var last =
                state.messages.length > 0
                  ? state.messages[state.messages.length - 1]
                  : null;
              if (!last || last.role !== "assistant" || last._final) {
                state.messages.push({
                  role: "assistant",
                  content: "",
                  reasoning_content: "",
                  tool_calls: [],
                  _streaming: true,
                });
                last = state.messages[state.messages.length - 1];
              }
              // 聚合 reasoning 帧携带全文：直接覆盖（若已有 delta 累积则等价）
              last.reasoning_content = p.content;
              rerenderChat();
            }
          } catch (ex) {}
        });
        // token 级流式增量：逐块追加到当前思考内容；reset 帧表示轮次作废
        es.addEventListener("reasoning_delta", function (e) {
          try {
            var ev = JSON.parse(e.data);
            var p = ev.payload || {};
            if (p.channel === "_consolidation_") return;
            if (p.reset) return; // 轮次作废（用户中断）：清空累积中的思考
            if (!p.content) return;
            state.chatStage = __("AI 思考中...", "AI thinking...");
            var last =
              state.messages.length > 0
                ? state.messages[state.messages.length - 1]
                : null;
            if (!last || last.role !== "assistant" || last._final) {
              state.messages.push({
                role: "assistant",
                content: "",
                reasoning_content: "",
                tool_calls: [],
                _streaming: true,
              });
              last = state.messages[state.messages.length - 1];
            }
            last.reasoning_content = (last.reasoning_content || "") + p.content;
            rerenderChat();
          } catch (ex) {}
        });
        es.addEventListener("tool_call", function (e) {
          try {
            var ev = JSON.parse(e.data);
            var p = ev.payload || {};
            console.log("[SSE] tool_call", p);
            if (!p.tool) return;
            var last =
              state.messages.length > 0
                ? state.messages[state.messages.length - 1]
                : null;
            if (!last || last.role !== "assistant" || last._final) {
              state.messages.push({
                role: "assistant",
                content: "",
                tool_calls: [],
                _streaming: true,
              });
              last = state.messages[state.messages.length - 1];
            }
            if (!last.tool_calls) last.tool_calls = [];
            last.tool_calls.push({
              tool: p.tool,
              name: p.tool,
              args: p.args || {},
              result: p.result || "",
              status: p.status || "ok",
              plugin: p.plugin || "",
            });
            var pidx = (state.pendingTools || []).indexOf(p.tool);
            if (pidx !== -1) state.pendingTools.splice(pidx, 1);
            state.chatStage = __("工具调用: ", "Tool: ") + (p.tool || "");
            rerenderChat();
          } catch (ex) {
            console.error("[SSE] tool_call error", ex);
          }
        });
        // 注：后端不发布 tool_result 类型事件（工具结果随 EventToolCall 一次发出），无此监听器。
        es.addEventListener("stage", function (e) {
          try {
            var ev = JSON.parse(e.data);
            var p = ev.payload || {};
            var phase = p.phase || "";
            var tool = p.tool || "";
            if (p.channel === "_consolidation_") return;
            console.log("[SSE] stage event", phase, tool);
            if (phase === "pre_action") {
              state.chatStage = __("AI 思考中...", "AI thinking...");
            } else if (phase === "before_toolcall") {
              state.chatStage = __("工具调用: ", "Tool: ") + (tool || "");
              if (tool && (state.pendingTools || []).indexOf(tool) === -1) {
                if (!state.pendingTools) state.pendingTools = [];
                state.pendingTools.push(tool);
                rerenderChat();
              }
            } else if (phase === "before_output") {
              state.chatStage = __("生成回复中...", "Generating response...");
            }
            var badge = document.getElementById("chat-stage");
            if (badge) {
              badge.textContent = state.chatStage || "";
              badge.style.display = "none";
            }
          } catch (ex) {
            console.error("[SSE] stage error", ex);
          }
        });
        es.onopen = function () {
          console.log("[SSE] connection opened");
        };
        es.onerror = function (e) {
          console.error("[SSE] error", e);
          // 1) 立即 close 阻止浏览器原生自动重连与手动 setTimeout(connectSSE) 双连接竞态
          try { state.eventSource && state.eventSource.close(); state.eventSource = null; } catch(ex){}
          // 2) 连接错误期间可能丢失事件，增量补拉历史（无闪烁）
          syncChatFromHistory().catch(function(){});
          // 3) 2s 后手动重连（比原 5s 更快恢复）
          setTimeout(connectSSE, 2000);
        };
        // sync_required：Server 因 Last-Event-ID 不在 ring（delta ID / 已到 tip）无法重放，
        // 通知前端增量补拉历史——避免前端空等后续聚合事件导致「消息同步不及时」。
        es.addEventListener("sync_required", function(e) {
          console.log("[SSE] sync_required received, incremental sync");
          syncChatFromHistory().catch(function(){});
        });
        // Periodically refresh sidebar data
        if (state._sidebarRefresh) clearInterval(state._sidebarRefresh);
        state._sidebarRefresh = setInterval(async function () {
          try {
            var td = await api("/terminals");
            if (td && td.terminals) state.terminals = td.terminals;
          } catch (e) {}
          try {
            var ch = await api("/cmd/history");
            if (ch && ch.history) state.cmdHistory = ch.history;
          } catch (e) {}
          renderTerminals();
          renderCmdHistory();
        }, 5000);
      }

      // ===== Plugins =====
      function renderPlugins() {
        var k = state.kernel;
        var plugins = k?.plugins || [];
        var tools = k?.tools || [];
        var installed = state.installedPlugins || [];
        var html =
          '<div class="card"><h2>' +
          __("安装插件", "Install Plugin") +
          "</h2>" +
          '<div style="display:flex;gap:8px;margin-bottom:8px">' +
          '<input id="plugin-url" placeholder=".hmap ' +
          __("包下载 URL", "Package URL") +
          '" style="flex:1" onkeydown="if(event.key==\'Enter\')installPlugin()">' +
          '<button class="btn btn-primary" onclick="installPlugin()">' +
          __("安装", "Install") +
          "</button></div>" +
          '<div><input type="file" id="plugin-file" accept=".hmap" style="display:inline;width:auto" onchange="installPluginFile(this.files[0])">' +
          '<label for="plugin-file" class="btn btn-ghost" style="cursor:pointer">' +
          __("选择 .hmap 文件上传", "Upload .hmap file") +
          "</label></div></div>";
        var disabledNames = {};
        (state.disabledPlugins || []).forEach(function (d) {
          disabledNames[d.name] = d;
        });
        var installedNames = (state.installedPlugins || []).map(function (p) {
          return p.name;
        });
        var allPluginNames = {};
        plugins.forEach(function (p) {
          allPluginNames[p.name] = true;
        });
        state.disabledPlugins.forEach(function (d) {
          allPluginNames[d.name] = true;
        });
        html +=
          '<div class="card"><h2>' +
          __("所有插件", "All Plugins") +
          " (" +
          Object.keys(allPluginNames).length +
          ")</h2>";
        var names = Object.keys(allPluginNames).sort();
        if (names.length === 0) {
          html +=
            '<div class="empty-state"><p>' +
            __("暂无插件", "No plugins") +
            "</p></div>";
        } else {
          html +=
            "<table><tr><th>" +
            __("名称", "Name") +
            "</th><th>" +
            __("状态", "Status") +
            "</th><th>" +
            __("操作", "Actions") +
            "</th></tr>";
          names.forEach(function (name) {
            var isExternal = installedNames.indexOf(name) >= 0;
            var isDisabled = disabledNames[name];
            var loaded = plugins.some(function (p) {
              return p.name === name;
            });
            var statusHtml =
              loaded && !isDisabled
                ? '<span class="badge badge-green">' +
                  __("已加载", "Loaded") +
                  "</span>"
                : loaded && isDisabled
                  ? '<span class="badge badge-yellow">' +
                    __("运行中(禁用待生效)", "Running (disable pending)") +
                    "</span>"
                  : isDisabled
                    ? '<span class="badge badge-red">' +
                      __("已禁用", "Disabled") +
                      "</span>"
                    : '<span class="badge badge-gray">' +
                      __("未加载", "Not Loaded") +
                      "</span>";
            var actionsHtml = "";
            if (loaded) {
              actionsHtml +=
                '<button class="btn btn-sm btn-warning" onclick="disablePlugin(\'' +
                escHtml(name) +
                '\')" style="margin-right:4px">' +
                __("禁用", "Disable") +
                "</button>";
            } else {
              actionsHtml +=
                '<button class="btn btn-sm btn-success" onclick="enablePlugin(\'' +
                escHtml(name) +
                '\')" style="margin-right:4px">' +
                __("启用", "Enable") +
                "</button>";
            }
            if (isExternal) {
              actionsHtml +=
                '<button class="btn btn-sm btn-danger" onclick="removePlugin(\'' +
                escHtml(name) +
                "')\">" +
                __("卸载", "Unload") +
                "</button>";
            }
            html +=
              "<tr><td>" +
              escHtml(name) +
              "</td>" +
              "<td>" +
              statusHtml +
              "</td>" +
              "<td>" +
              actionsHtml +
              "</td></tr>";
          });
          html += "</table>";
        }
        html += "</div>";
        if (installed.length > 0) {
          html +=
            '<div class="card"><h2>' +
            __("已安装外部插件", "Installed Plugins") +
            " (" +
            installed.length +
            ")</h2>" +
            "<table><tr><th>" +
            __("名称", "Name") +
            "</th><th>" +
            __("版本", "Version") +
            "</th><th>" +
            __("描述", "Description") +
            "</th><th>" +
            __("操作", "Actions") +
            "</th></tr>";
          installed.forEach(function (p) {
            html +=
              "<tr><td>" +
              escHtml(p.name) +
              "</td>" +
              "<td>" +
              escHtml(p.version || "-") +
              "</td>" +
              "<td>" +
              escHtml((p.description || "").substring(0, 50)) +
              "</td>" +
              '<td><button class="btn btn-sm btn-ghost" onclick="showPluginInfo(\'' +
              escHtml(p.name) +
              "')\">" +
              __("详情", "Details") +
              "</button> " +
              '<button class="btn btn-sm btn-danger" onclick="removePlugin(\'' +
              escHtml(p.name) +
              "')\">" +
              __("卸载", "Unload") +
              "</button></td></tr>";
          });
          html += "</table></div>";
        }
        if (state.pluginInfo) {
          html +=
            '<div class="card"><h2>' +
            __("插件详情", "Plugin Details") +
            ": " +
            escHtml(state.pluginInfo.name) +
            "</h2>" +
            "<pre>" +
            escHtml(JSON.stringify(state.pluginInfo, null, 2)) +
            "</pre>" +
            '<button class="btn btn-ghost" onclick="closePluginInfo()">' +
            __("关闭", "Close") +
            "</button></div>";
        }
        if (tools.length > 0) {
          html +=
            '<div class="card"><h2>' +
            __("已注册工具", "Registered Tools") +
            " (" +
            tools.length +
            ")</h2>" +
            '<div style="display:flex;flex-wrap:wrap;gap:4px">';
          tools.forEach(function (t) {
            html +=
              '<span class="tool-badge" title="' +
              escHtml(t.description || "") +
              '">' +
              escHtml(t.name) +
              "</span>";
          });
          html += "</div></div>";
        }
        html +=
          '<div class="card"><h2>' +
          __("系统操作", "System Operations") +
          "</h2>" +
          '<button class="btn btn-primary" onclick="reloadPlugins()">' +
          __("重载插件", "Reload Plugins") +
          "</button></div>";
        html +=
          '<div class="card health-card"><div class="health-head"><h2>' +
          __("健康检查", "Health Check") +
          '</h2><button class="btn btn-ghost btn-sm" onclick="runHealthcheck()">' +
          __("运行", "Run") +
          '</button></div><div id="health-panel">';
        if (state.healthResult) {
          html += renderHealthResult(state.healthResult);
        } else {
          html +=
            '<p style="color:var(--text-muted);font-size:13px">' +
            __(
              "尚未运行,点击右上角「运行」开始",
              "Not run yet, click Run to start",
            ) +
            "</p>";
        }
        html += "</div></div>";
        document.getElementById("tab-plugins").innerHTML = html;
      }

      async function loadInstalledPlugins() {
        try {
          state.installedPlugins = await api("/plugins");
        } catch (e) {
          state.installedPlugins = [];
        }
      }

      async function installPlugin() {
        var inp = document.getElementById("plugin-url");
        var url = inp?.value.trim();
        if (!url) {
          toast(__("请输入插件包 URL", "Please enter plugin URL"), true);
          return;
        }
        try {
          var raw = await api("/plugins", {
            method: "POST",
            body: JSON.stringify({ url: url }),
            raw: true,
          });
          var ct = raw.headers.get("content-type") || "";
          var r = ct.includes("json") ? await raw.json() : await raw.text();
          if (!raw.ok || (r && r.error)) {
            toast(
              __("安装失败: ", "Install failed: ") +
                ((r && (r.error || r.details)) || "HTTP " + raw.status),
              true,
            );
            return;
          }
          toast(
            __("安装结果: ", "Install result: ") +
              (r.status || JSON.stringify(r)),
          );
          if (r.action === "reload_required")
            toast(
              __(
                "已安装，请点击「重载插件」加载",
                'Installed, click "Reload Plugins" to load',
              ),
              false,
            );
          await loadInstalledPlugins();
          renderPlugins();
        } catch (e) {
          toast(__("安装失败: ", "Install failed: ") + e.message, true);
        }
      }

      async function installPluginFile(file) {
        if (!file) return;
        try {
          var r = await fetch("/api/v1/plugins", {
            method: "POST",
            body: file,
            headers: { "Content-Type": "application/octet-stream" },
          });
          var data = await r.json();
          toast(
            __("上传安装: ", "Upload install: ") +
              (data.status || JSON.stringify(data)),
          );
          if (data.action === "reload_required")
            toast(
              __(
                "已安装，请点击「重载插件」加载",
                'Installed, click "Reload Plugins" to load',
              ),
              false,
            );
          loadInstalledPlugins();
          renderPlugins();
        } catch (e) {
          toast(__("上传失败: ", "Upload failed: ") + e.message, true);
        }
      }

      async function showPluginInfo(name) {
        try {
          state.pluginInfo = await api("/plugins/" + encodeURIComponent(name));
          renderPlugins();
        } catch (e) {
          toast(__("获取详情失败: ", "Get details failed: ") + e.message, true);
        }
      }

      function closePluginInfo() {
        state.pluginInfo = null;
        renderPlugins();
      }

      var removingPlugins = {};

      async function removePlugin(name) {
        if (removingPlugins[name]) return; // 防重复点击
        if (
          !confirm(
            __("确定卸载插件", "Are you sure to unload plugin") +
              "「" +
              name +
              "」？",
          )
        )
          return;
        removingPlugins[name] = true;
        try {
          var raw = await api("/plugins/" + encodeURIComponent(name), {
            method: "DELETE",
            raw: true,
          });
          var ct = raw.headers.get("content-type") || "";
          var body = ct.includes("json") ? await raw.json() : await raw.text();
          if (!raw.ok) {
            var em =
              (body && (body.error || body.details)) || "HTTP " + raw.status;
            toast(__("卸载失败: ", "Unload failed: ") + em, true);
            // 内置插件或路径错误时刷新一次列表保持状态一致
            loadInstalledPlugins();
            renderPlugins();
            return;
          }
          toast(
            __("已卸载: ", "Unloaded: ") + (body.name || body.status || name),
          );
          await loadInstalledPlugins();
          // 同步内核插件/禁用列表，确保列表与工具立即消失
          try {
            state.kernel = await api("/kernel");
            var s = await api("/settings");
            state.disabledPlugins = s.disabled_plugins || [];
          } catch (e2) {}
          renderPlugins();
        } catch (e) {
          toast(__("卸载失败: ", "Unload failed: ") + e.message, true);
        } finally {
          delete removingPlugins[name];
        }
      }

      async function disablePlugin(name) {
        if (name === "webui") {
          var r = confirm(
            __(
              "禁用 WebUI 后将无法通过 URL:端口访问此管理面板，若要重新启用需要通过 CLI 命令 /plugin enable webui 恢复。\n\n确定要禁用吗？",
              "Disabling WebUI will make this management panel inaccessible via URL:port. To re-enable, use CLI command /plugin enable webui.\n\nAre you sure?",
            ),
          );
          if (!r) return;
        }
        try {
          var r = await api(
            "/plugins/" + encodeURIComponent(name) + "/disable",
            { method: "POST" },
          );
          toast(__("已禁用: ", "Disabled: ") + name);
          state.kernel = await api("/kernel");
          var s = await api("/settings");
          state.disabledPlugins = s.disabled_plugins || [];
          renderPlugins();
        } catch (e) {
          toast(__("禁用失败: ", "Disable failed: ") + e.message, true);
        }
      }

      async function enablePlugin(name) {
        try {
          var r = await api(
            "/plugins/" + encodeURIComponent(name) + "/enable",
            { method: "POST" },
          );
          toast(__("已启用: ", "Enabled: ") + name);
          state.kernel = await api("/kernel");
          var s = await api("/settings");
          state.disabledPlugins = s.disabled_plugins || [];
          renderPlugins();
        } catch (e) {
          toast(__("启用失败: ", "Enable failed: ") + e.message, true);
        }
      }

      async function reloadPlugins() {
        try {
          var r = await api("/plugins/reload", { method: "POST" });
          toast(__("插件已重载", "Plugins reloaded"));
          state.kernel = await api("/kernel");
          renderPlugins();
        } catch (e) {
          toast(__("重载失败: ", "Reload failed: ") + e.message, true);
        }
      }

      async function runHealthcheck() {
        var panel = document.getElementById("health-panel");
        if (!panel) return;
        panel.innerHTML =
          '<div class="loading" style="margin:12px auto"></div><p style="text-align:center;color:var(--text-muted)">' +
          __("运行中...", "Running...") +
          "</p>";
        try {
          var r = await api("/kernel");
          var tools = r?.tools || [];
          var healthTool = tools.find(function (t) {
            return t.name === "healthcheck";
          });
          if (!healthTool) {
            panel.innerHTML =
              '<p style="color:var(--text-secondary)">' +
              __("healthcheck 工具未注册", "healthcheck tool not registered") +
              "</p>";
            return;
          }
          panel.innerHTML =
            '<p style="color:var(--text-secondary)">' +
            __(
              "通过 Agent 对话触发 healthcheck...",
              "Triggering healthcheck via Agent...",
            ) +
            "</p>";
          var chatR = await api("/chat", {
            method: "POST",
            body: JSON.stringify({
              message: __(
                "请运行 healthcheck 工具进行全面健康检查并报告结果",
                "Please run the healthcheck tool for a full system check and report the results",
              ),
            }),
          });
          panel.innerHTML =
            "<pre>" + escHtml(JSON.stringify(chatR, null, 2)) + "</pre>";
        } catch (e) {
          panel.innerHTML =
            '<p style="color:#fca5a5">' +
            __("错误: ", "Error: ") +
            escHtml(e.message) +
            "</p>";
          toast(
            __("健康检查失败: ", "Health check failed: ") + e.message,
            true,
          );
        }
      }

      function renderHealthResult(r) {
        if (!r || !r.checks)
          return (
            '<p style="color:var(--text-secondary)">' +
            __("暂无健康检查数据", "No health check data") +
            "</p>"
          );
        var checks = r.checks || [];
        var passed = checks.filter(function (c) {
          return c.pass;
        }).length;
        var failed = checks.filter(function (c) {
          return !c.pass;
        }).length;
        var html =
          '<div style="margin-bottom:12px;display:flex;gap:16px;align-items:center">' +
          '<span class="badge badge-green">' +
          __("通过: ", "Pass: ") +
          passed +
          "</span>" +
          '<span class="badge ' +
          (failed > 0 ? "badge-red" : "badge-green") +
          '">' +
          __("失败: ", "Fail: ") +
          failed +
          "</span>" +
          '<span class="badge badge-blue">' +
          __("总计: ", "Total: ") +
          checks.length +
          "</span></div>";
        checks.forEach(function (c) {
          var passClass = c.pass ? "check-pass" : "check-fail";
          if (c.status === "skip") passClass = "check-skip";
          html +=
            '<div class="health-item">' +
            '<span class="check-name">' +
            escHtml(c.name) +
            "</span>" +
            '<span class="check-status ' +
            passClass +
            '">' +
            (c.status || "unknown") +
            "</span>" +
            '<span style="color:var(--text-muted);font-size:11px">' +
            escHtml(c.detail || "") +
            "</span></div>";
        });
        return html;
      }

      // ===== Kernel =====
      function renderKernel() {
        var k = state.kernel;
        if (!k) {
          document.getElementById("tab-kernel").innerHTML =
            '<div class="card"><p style="color:var(--text-muted)">' +
            __("内核未响应", "Kernel not responding") +
            "</p></div>";
          return;
        }
        var html =
          '<div class="card"><h2>' +
          __("运行时", "Runtime") +
          "</h2>" +
          '<div class="kv-row"><span class="key">Goroutines</span><span class="val">' +
          (k?.runtime?.goroutines || "-") +
          "</span></div>" +
          '<div class="kv-row"><span class="key">' +
          __("内存", "Memory") +
          '</span><span class="val">' +
          (k?.runtime?.memory_mb ? k.runtime.memory_mb + " MB" : "-") +
          "</span></div>" +
          '<div class="kv-row"><span class="key">Go ' +
          __("版本", "Version") +
          '</span><span class="val">' +
          (k?.runtime?.go_version || "-") +
          "</span></div></div>";
        html +=
          '<div class="card"><h2>LLM</h2>' +
          '<div class="kv-row"><span class="key">Provider</span><span class="val">' +
          (k.llm?.provider || __("未配置", "Not configured")) +
          "</span></div>" +
          '<div class="kv-row"><span class="key">' +
          __("可用源", "Sources") +
          '</span><span class="val">' +
          (k.llm?.sources || 0) +
          "</span></div>" +
          '<div class="kv-row"><span class="key">' +
          __("状态", "Status") +
          '</span><span class="val"><span class="status-dot ' +
          (k.llm?.available ? "dot-green" : "dot-red") +
          '"></span>' +
          (k.llm?.available
            ? __("运行中", "Running")
            : __("不可用", "Unavailable")) +
          "</span></div></div>";
        html +=
          '<div class="card"><h2>' +
          __("记忆", "Memory") +
          "</h2>" +
          '<div class="kv-row"><span class="key">' +
          __("图记忆", "Graph Memory") +
          '</span><span class="val">' +
          (k.memory?.available
            ? k.memory.entity_count +
              __(" 实体, ", " entities, ") +
              k.memory.relation_count +
              __(" 关系", " relations")
            : __("未初始化", "Uninitialized")) +
          "</span></div>" +
          '<div class="kv-row"><span class="key">' +
          __("文档记忆", "Document Memory") +
          '</span><span class="val">' +
          (k.documents?.available
            ? k.documents.doc_count + __(" 文档", " docs")
            : __("未初始化", "Uninitialized")) +
          "</span></div>" +
          '<div class="kv-row"><span class="key">' +
          __("文本记忆", "Text Memory") +
          '</span><span class="val">' +
          (k.text_memory?.available
            ? k.text_memory.file_count + __(" 文件", " files")
            : __("未初始化", "Uninitialized")) +
          "</span></div>" +
          '<div class="kv-row"><span class="key">' +
          __("知识库", "Knowledge") +
          '</span><span class="val">' +
          (k.knowledge?.available
            ? k.knowledge.item_count + __(" 项", " items")
            : __("未初始化", "Uninitialized")) +
          "</span></div></div>";
        html +=
          '<div class="card"><h2>' +
          __("插件", "Plugins") +
          " (" +
          (k.plugins?.length || 0) +
          ")</h2>";
        if (k.plugins?.length) {
          html += '<div style="display:flex;flex-wrap:wrap;gap:4px">';
          k.plugins.forEach(function (p) {
            html +=
              '<span class="badge badge-blue">' + escHtml(p.name) + "</span>";
          });
          html += "</div>";
        } else {
          html +=
            '<p style="color:var(--text-muted)">' + __("无", "None") + "</p>";
        }
        html += "</div>";
        document.getElementById("tab-kernel").innerHTML = html;
      }

      // ===== Star Map =====
      var starmapScene = null,
        starmapCam = null,
        starmapRen = null,
        starmapCtrl = null;
      var starmapNodes = [],
        starmapEdges = [];
      var starmapNodeMeshes = [],
        starmapEdgeLines = [],
        starmapStarField = null;
      var starmapHovered = null,
        starmapSelected = null,
        starmapAutoView = true;
      var starmapRaf = null;
      var smTypeColors = {
        person: 0x4488ff,
        task: 0xff8844,
        ai: 0xaa44ff,
        concept: 0x44ff88,
        object: 0xff4444,
      };
      var smEdgeColors = {
        喜欢: 0xff6b6b,
        学习: 0x4ecdc4,
        属于: 0x45b7d1,
        相关: 0x96ceb4,
        使用: 0xfeca57,
        创建: 0xff9ff3,
      };

      function createStarField() {
        var c = 3000;
        var p = new Float32Array(c * 3),
          cl = new Float32Array(c * 3),
          s = new Float32Array(c);
        for (var i = 0; i < c; i++) {
          var i3 = i * 3;
          var r = 400 + Math.random() * 600,
            th = Math.random() * Math.PI * 2,
            ph = Math.acos(2 * Math.random() - 1);
          p[i3] = r * Math.sin(ph) * Math.cos(th);
          p[i3 + 1] = r * Math.sin(ph) * Math.sin(th);
          p[i3 + 2] = r * Math.cos(ph);
          if (Math.random() < 0.7) {
            cl[i3] = 0.8 + Math.random() * 0.2;
            cl[i3 + 1] = 0.8 + Math.random() * 0.2;
            cl[i3 + 2] = 1;
          } else {
            cl[i3] = 1;
            cl[i3 + 1] = 0.9 + Math.random() * 0.1;
            cl[i3 + 2] = 0.8 + Math.random() * 0.2;
          }
          s[i] = 0.5 + Math.random() * 2;
        }
        var g = new THREE.BufferGeometry();
        g.setAttribute("position", new THREE.BufferAttribute(p, 3));
        g.setAttribute("color", new THREE.BufferAttribute(cl, 3));
        g.setAttribute("size", new THREE.BufferAttribute(s, 1));
        var m = new THREE.PointsMaterial({
          size: 1.5,
          vertexColors: true,
          transparent: true,
          opacity: 0.8,
          sizeAttenuation: true,
        });
        starmapStarField = new THREE.Points(g, m);
        starmapScene.add(starmapStarField);
      }

      function onStarmapMove(e) {
        if (!starmapRen || !starmapCam) return;
        var rect = starmapRen.domElement.getBoundingClientRect();
        var mouse = new THREE.Vector2(
          ((e.clientX - rect.left) / rect.width) * 2 - 1,
          -((e.clientY - rect.top) / rect.height) * 2 + 1,
        );
        var rc = new THREE.Raycaster();
        rc.setFromCamera(mouse, starmapCam);
        var hits = rc.intersectObjects(starmapNodeMeshes);
        if (hits.length > 0) {
          var n = hits[0].object;
          if (starmapHovered !== n) {
            if (starmapHovered) starmapHovered.scale.set(1, 1, 1);
            starmapHovered = n;
            n.scale.set(1.2, 1.2, 1.2);
          }
        } else {
          if (starmapHovered) {
            starmapHovered.scale.set(1, 1, 1);
            starmapHovered = null;
          }
        }
      }

      function onStarmapClick(e) {
        if (!starmapRen || !starmapCam) return;
        var rect = starmapRen.domElement.getBoundingClientRect();
        var mouse = new THREE.Vector2(
          ((e.clientX - rect.left) / rect.width) * 2 - 1,
          -((e.clientY - rect.top) / rect.height) * 2 + 1,
        );
        var rc = new THREE.Raycaster();
        rc.setFromCamera(mouse, starmapCam);
        var hits = rc.intersectObjects(starmapNodeMeshes);
        if (hits.length > 0) {
          var n = hits[0].object;
          starmapSelected = starmapSelected === n ? null : n;
          if (starmapAutoView && starmapSelected)
            flyStarmapTo(starmapSelected.userData.nodeId, 500);
          onStarmapMove(e);
        } else {
          starmapSelected = null;
        }
      }

      function flyStarmapTo(nodeId, dur) {
        if (!starmapAutoView) return;
        var m = starmapNodeMeshes.find(function (x) {
          return x.userData.nodeId === nodeId;
        });
        if (!m) return;
        var tp = m.position.clone(),
          sp = starmapCam.position.clone(),
          st = starmapCtrl.target.clone();
        var dist = tp.length() + 25,
          ep = new THREE.Vector3(tp.x, tp.y + dist * 0.4, tp.z + dist * 0.8);
        var t0 = Date.now();
        (function lerp() {
          var t = Math.min((Date.now() - t0) / dur, 1),
            e = 1 - Math.pow(1 - t, 3);
          starmapCam.position.lerpVectors(sp, ep, e);
          starmapCtrl.target.lerpVectors(st, tp, e);
          if (t < 1) requestAnimationFrame(lerp);
        })();
      }

      function onStarmapResize() {
        if (!starmapRen || !starmapCam) return;
        var cont = starmapRen.domElement.parentElement;
        if (!cont) return;
        var rect = cont.getBoundingClientRect();
        var w = rect.width || 800,
          h = Math.max(rect.height || 250, 100);
        if (w > 0 && h > 0) {
          starmapCam.aspect = w / h;
          starmapCam.updateProjectionMatrix();
          starmapRen.setSize(w, h);
        }
      }

      function starmapAnimate() {
        starmapRaf = requestAnimationFrame(starmapAnimate);
        if (starmapCtrl) starmapCtrl.update();
        if (starmapStarField) starmapStarField.rotation.y += 0.0001;
        if (starmapRen && starmapScene && starmapCam)
          starmapRen.render(starmapScene, starmapCam);
      }

      function createNebula() {
        var nc = 500;
        var p = new Float32Array(nc * 3),
          cl = new Float32Array(nc * 3);
        for (var i = 0; i < nc; i++) {
          var i3 = i * 3;
          p[i3] = (Math.random() - 0.5) * 800;
          p[i3 + 1] = (Math.random() - 0.5) * 800;
          p[i3 + 2] = (Math.random() - 0.5) * 800;
          var ch = Math.random();
          if (ch < 0.33) {
            cl[i3] = 0.5 + Math.random() * 0.3;
            cl[i3 + 1] = 0.2 + Math.random() * 0.2;
            cl[i3 + 2] = 0.7 + Math.random() * 0.3;
          } else if (ch < 0.66) {
            cl[i3] = 0.2 + Math.random() * 0.2;
            cl[i3 + 1] = 0.3 + Math.random() * 0.3;
            cl[i3 + 2] = 0.8 + Math.random() * 0.2;
          } else {
            cl[i3] = 0.7 + Math.random() * 0.3;
            cl[i3 + 1] = 0.2 + Math.random() * 0.2;
            cl[i3 + 2] = 0.5 + Math.random() * 0.3;
          }
        }
        var g = new THREE.BufferGeometry();
        g.setAttribute("position", new THREE.BufferAttribute(p, 3));
        g.setAttribute("color", new THREE.BufferAttribute(cl, 3));
        var m = new THREE.PointsMaterial({
          size: 8,
          vertexColors: true,
          transparent: true,
          opacity: 0.15,
          sizeAttenuation: true,
          blending: THREE.AdditiveBlending,
        });
        var np = new THREE.Points(g, m);
        starmapScene.add(np);
      }

      // ===== Settings =====
      function pluginDisplayName(p) {
        if (p === "core") return __("核心", "Core");
        var name = p.replace("plugin.", "");
        var meta = state.pluginMeta && state.pluginMeta[name];
        if (meta)
          return state.lang === "en"
            ? meta.name_en || name
            : meta.name_zh || name;
        return name;
      }

      function renderSettingsSidebar() {
        var el = document.querySelector(".settings-tabs");
        if (!el) return;
        el.innerHTML = "";
        state.settingsPlugins.forEach(function (p) {
          var a = document.createElement("span");
          a.textContent = pluginDisplayName(p);
          if (p === state.selectedSection) a.className = "active";
          a.onclick = function () {
            state.selectedSection = p;
            renderOneSettings();
          };
          el.appendChild(a);
        });
      }

      function renderOneSettings() {
        var prefix = state.selectedSection + ".";
        var allKeys = Object.keys(state.settings || {});
        var filtered = allKeys.filter(function (k) {
          return k === prefix.slice(0, -1) || k.startsWith(prefix);
        });
        filtered.sort();
        var hideTopLlms = [
          "core.llm.base_url",
          "core.llm.model",
          "core.llm.api_key",
          "core.llm.adapter",
          "core.llm.adapter_path",
          "core.llm.thinking_enabled",
        ];
        var sourceKeys = filtered.filter(function (k) {
          return k.startsWith("core.llm.sources.");
        });
        var sourceMap = {};
        sourceKeys.forEach(function (k) {
          var parts = k.split(".");
          var srcName = parts[3];
          if (!sourceMap[srcName]) sourceMap[srcName] = {};
          sourceMap[srcName][k] = true;
        });
        var mcpServerKeys = filtered.filter(function (k) {
          return (
            k.startsWith("plugin.mcp.servers.") && k.split(".").length >= 5
          );
        });
        var mcpServerMap = {};
        mcpServerKeys.forEach(function (k) {
          var parts = k.split(".");
          var srvName = parts[3];
          if (!mcpServerMap[srvName]) mcpServerMap[srvName] = {};
          mcpServerMap[srvName][k] = true;
        });
        var regularKeys = filtered.filter(function (k) {
          return (
            !k.startsWith("core.llm.sources.") &&
            hideTopLlms.indexOf(k) === -1 &&
            !k.startsWith("plugin.mcp.servers.") &&
            k !== "plugin.mcp.servers"
          );
        });
        var html =
          '<div class="settings-layout"><div class="settings-tabs"></div><div class="settings-content">';
        if (
          regularKeys.length === 0 &&
          Object.keys(sourceMap).length === 0 &&
          Object.keys(mcpServerMap).length === 0 &&
          state.selectedSection !== "plugin.mcp"
        ) {
          html +=
            '<div class="card"><h2>' +
            escHtml(state.selectedSection) +
            '</h2><p style="color:var(--text-muted)">' +
            __("暂无设置项", "No settings") +
            "</p></div>";
        } else {
          regularKeys.forEach(function (k) {
            var v = state.settings[k];
            var sv = typeof v === "object" ? JSON.stringify(v) : String(v);
            var m = state.meta?.[k];
            var shortName = k.split(".").pop().replace(/_/g, " ");
            var label = m?.display_name || shortName;
            var desc = m?.description || "";
            var typ = m?.type || "string";
            var ph = m?.placeholder || "";
            var opts = m?.options || [];
            var inpId = "inp-" + k.replace(/\./g, "_");
            var inp = "";
            if (typ === "bool") {
              var chk = sv === "true" ? "checked" : "";
              inp =
                '<label style="display:flex;align-items:center;gap:8px;cursor:pointer"><input type="checkbox" id="' +
                inpId +
                '" ' +
                chk +
                " onchange=\"markDirty('" +
                k +
                '\')" style="width:auto;margin:0"> ' +
                label +
                "</label>";
            } else if (typ === "select") {
              var selOpts = "";
              opts.forEach(function (o) {
                selOpts +=
                  '<option value="' +
                  o +
                  '"' +
                  (sv === o ? " selected" : "") +
                  ">" +
                  o +
                  "</option>";
              });
              inp =
                "<label>" +
                label +
                '</label><select id="' +
                inpId +
                '" onchange="markDirty(\'' +
                k +
                "')\">" +
                selOpts +
                "</select>";
            } else if (typ === "text") {
              inp =
                "<label>" +
                label +
                '</label><textarea id="' +
                inpId +
                '" placeholder="' +
                escHtml(ph) +
                '" onchange="markDirty(\'' +
                k +
                "')\">" +
                escHtml(sv) +
                "</textarea>";
            } else {
              inp =
                "<label>" +
                label +
                '</label><input type="text" id="' +
                inpId +
                '" value="' +
                escHtml(sv) +
                '" placeholder="' +
                escHtml(ph) +
                '" onchange="markDirty(\'' +
                k +
                "')\">";
            }
            var extra = "";
            if (m?.extra) {
              m.extra.forEach(function (f) {
                var fk = (k ? k + "." : "") + f.key;
                var fv = state.settings?.[fk];
                var fph = f.placeholder || __("输入", "Enter ") + f.label;
                extra +=
                  '<div class="form-row" style="margin-left:16px;margin-top:4px"><label>' +
                  f.label +
                  "</label>";
                if (f.type === "select") {
                  var fopts = "";
                  if (f.options)
                    f.options.forEach(function (o) {
                      fopts +=
                        '<option value="' +
                        o +
                        '"' +
                        (fv === o ? "selected" : "") +
                        ">" +
                        o +
                        "</option>";
                    });
                  extra +=
                    "<select onchange=\"saveSetting('" +
                    fk +
                    "',this.value)\">" +
                    fopts +
                    "</select>";
                } else {
                  extra +=
                    '<input type="text" value="' +
                    escHtml(fv || "") +
                    '" placeholder="' +
                    escHtml(fph) +
                    '" onchange="markDirty(\'' +
                    fk +
                    '\')" style="margin-bottom:0">';
                }
                extra += "</div>";
              });
            }
            var descHtml = desc
              ? '<p style="font-size:11px;color:var(--text-muted);margin:-6px 0 10px">' +
                escHtml(desc) +
                "</p>"
              : "";
            html +=
              '<div class="card"><div class="settings-key">' +
              escHtml(k) +
              "</div>" +
              inp +
              descHtml +
              extra +
              '<button class="btn btn-sm btn-ghost" style="border-color:var(--save-btn-border);margin-top:4px" onclick="saveSetting(\'' +
              k +
              "')\">" +
              __("保存", "Save") +
              "</button></div>";
          });
          // LLM Sources
          Object.keys(sourceMap)
            .sort()
            .forEach(function (src) {
              var baseKey = "core.llm.sources." + src;
              var srcData =
                state.settings?.[baseKey + ".adapter"] ||
                state.settings?.[baseKey + ".base_url"] ||
                "";
              var fields = [
                {
                  key: "adapter",
                  label: __("适配器", "Adapter"),
                  type: "text",
                },
                { key: "base_url", label: "Base URL", type: "text" },
                { key: "model", label: __("模型", "Model"), type: "text" },
                { key: "api_key", label: "API Key", type: "text" },
                {
                  key: "thinking_enabled",
                  label: __("思考模式", "Thinking Mode"),
                  type: "select",
                  options: ["true", "false"],
                },
                {
                  key: "adapter_path",
                  label: __("适配器路径", "Adapter Path"),
                  type: "text",
                },
              ];
              var headerLabel = mL10n(src, "LLM Source: " + src);
              html += '<div class="card"><h2>' + escHtml(headerLabel) + "</h2>";
              fields.forEach(function (f) {
                var fk = baseKey + "." + f.key;
                var fv = state.settings?.[fk] || "";
                var flabel = f.label;
                var fieldId = "inp-" + fk.replace(/\./g, "_");
                if (f.type === "select") {
                  var fopts = "";
                  f.options.forEach(function (o) {
                    fopts +=
                      '<option value="' +
                      o +
                      '"' +
                      (fv === o ? "selected" : "") +
                      ">" +
                      o +
                      "</option>";
                  });
                  html +=
                    "<label>" +
                    flabel +
                    '</label><select id="' +
                    fieldId +
                    '" onchange="markDirty(\'' +
                    fk +
                    '\')" style="margin-bottom:4px">' +
                    fopts +
                    "</select>";
                } else {
                  html +=
                    "<label>" +
                    flabel +
                    '</label><input type="text" id="' +
                    fieldId +
                    '" value="' +
                    escHtml(fv) +
                    '" placeholder="' +
                    (f.key === "api_key"
                      ? __("输入 API Key", "Enter API Key")
                      : __("输入", "Enter ") + flabel) +
                    '" onchange="markDirty(\'' +
                    fk +
                    '\')" style="margin-bottom:4px">';
                }
              });
              html +=
                '<div style="display:flex;gap:8px;margin-top:8px">' +
                '<button class="btn btn-sm btn-ghost" style="border-color:var(--save-btn-border)" onclick="saveSetting(\'' +
                baseKey +
                ".adapter');saveSetting('" +
                baseKey +
                ".base_url');saveSetting('" +
                baseKey +
                ".model');saveSetting('" +
                baseKey +
                ".api_key');var inp=document.getElementById('" +
                ("inp-" + baseKey + ".thinking_enabled").replace(/\./g, "_") +
                "');if(inp)saveSetting('" +
                baseKey +
                ".thinking_enabled');toast('" +
                __("源", "Source") +
                " \\'" +
                src +
                "\\' " +
                __("已保存", "saved") +
                "')\">" +
                __("保存", "Save") +
                "</button>" +
                '<button class="btn btn-sm btn-danger" onclick="deleteSource(\'' +
                src +
                "')\">" +
                __("删除", "Delete") +
                "</button></div></div>";
            });
          if (
            Object.keys(sourceMap).length > 0 ||
            state.selectedSection === "core.llm"
          ) {
            html +=
              '<button class="btn btn-ghost btn-sm" onclick="showAddSourceDialog()" style="margin-bottom:16px">+ ' +
              __("添加 LLM 源", "Add LLM Source") +
              "</button>";
          }
          // MCP Servers
          if (
            state.selectedSection === "plugin.mcp" ||
            Object.keys(mcpServerMap).length > 0
          ) {
            html +=
              '<div class="card"><h2>' +
              __("MCP 服务器", "MCP Servers") +
              '</h2><p style="font-size:11px;color:var(--text-muted);margin-bottom:8px">' +
              __(
                "配置 Model Context Protocol 服务端连接",
                "Configure Model Context Protocol server connections",
              ) +
              "</p></div>";
            Object.keys(mcpServerMap)
              .sort()
              .forEach(function (srv) {
                var baseKey = "plugin.mcp.servers." + srv;
                var fields = [
                  {
                    key: "command",
                    label: __("启动命令", "Command"),
                    type: "text",
                  },
                  { key: "url", label: "SSE URL", type: "text" },
                  {
                    key: "args",
                    label: __("参数(JSON数组)", "Args (JSON array)"),
                    type: "text",
                  },
                  {
                    key: "env",
                    label: __("环境变量(JSON数组)", "Env (JSON array)"),
                    type: "text",
                  },
                ];
                html += '<div class="card"><h2>' + escHtml(srv) + "</h2>";
                fields.forEach(function (f) {
                  var fk = baseKey + "." + f.key;
                  var fv = state.settings?.[fk] || "";
                  var fieldId = "inp-" + fk.replace(/\./g, "_");
                  html +=
                    "<label>" +
                    f.label +
                    '</label><input type="text" id="' +
                    fieldId +
                    '" value="' +
                    escHtml(fv) +
                    '" placeholder="' +
                    __("输入", "Enter ") +
                    f.label +
                    '" onchange="markDirty(\'' +
                    fk +
                    '\')" style="margin-bottom:4px">';
                });
                html +=
                  '<div style="display:flex;gap:8px;margin-top:8px">' +
                  '<button class="btn btn-sm btn-ghost" style="border-color:var(--save-btn-border)" onclick="saveSetting(\'' +
                  baseKey +
                  ".command');saveSetting('" +
                  baseKey +
                  ".url');saveSetting('" +
                  baseKey +
                  ".args');saveSetting('" +
                  baseKey +
                  ".env');toast('MCP \\'" +
                  srv +
                  "\\' " +
                  __("已保存", "saved") +
                  "')\">" +
                  __("保存", "Save") +
                  "</button>" +
                  '<button class="btn btn-sm btn-danger" onclick="deleteMCPServer(\'' +
                  srv +
                  "')\">" +
                  __("删除", "Delete") +
                  "</button></div></div>";
              });
            html +=
              '<button class="btn btn-ghost btn-sm" onclick="addMCPSource()" style="margin-bottom:16px">+ ' +
              __("添加 MCP 服务器", "Add MCP Server") +
              "</button>";
          }
        }
        html += "</div></div>";
        document.getElementById("tab-settings").innerHTML = html;
        renderSettingsSidebar();
      }

      function markDirty(k) {
        var inp = document.getElementById("inp-" + k.replace(/\./g, "_"));
        if (inp) inp.style.borderColor = "var(--save-btn-border)";
      }

      async function saveSetting(k) {
        var inp = document.getElementById("inp-" + k.replace(/\./g, "_"));
        if (!inp) return;
        var val;
        var m = state.meta?.[k];
        if (m?.type === "bool") {
          val = inp.checked ? "true" : "false";
        } else if (m?.type === "select") {
          val = inp.value;
        } else {
          var raw = inp.value;
          try {
            val = JSON.parse(raw);
          } catch (e) {
            val = raw;
          }
        }
        try {
          var r = await api("/settings", {
            method: "PUT",
            body: JSON.stringify({ key: k, value: val }),
          });
          if (r.status === "ok") {
            inp.style.borderColor = "";
            state.settings[k] = val;
            toast(__("已保存: ", "Saved: ") + k);
          } else {
            toast(
              __("保存失败: ", "Save failed: ") + (r.error || "unknown"),
              true,
            );
          }
        } catch (e) {
          toast(__("保存失败: ", "Save failed: ") + e.message, true);
        }
      }

      function renderConfigDisabled() {
        document.getElementById("tab-settings").innerHTML =
          '<div class="card"><h2>' +
          __("设置", "Settings") +
          '</h2><p style="color:var(--text-muted)">' +
          __("设置面板已加载", "Settings panel loaded") +
          "</p></div>";
        renderOneSettings();
      }

      function mL10n(key, fallback) {
        var meta = state.meta?.[key];
        if (meta?.display_name) return meta.display_name;
        return fallback || key;
      }

      function showAddSourceDialog() {
        var name = prompt(
          __(
            "输入新 LLM 源名称（如 openai、anthropic）:",
            "Enter new LLM source name (e.g. openai, anthropic):",
          ),
        );
        if (!name || !name.trim()) return;
        name = name
          .trim()
          .toLowerCase()
          .replace(/[^a-z0-9_]/g, "_");
        if (!name) {
          toast(__("名称无效", "Invalid name"), true);
          return;
        }
        var keys = [
          "base_url",
          "model",
          "api_key",
          "adapter",
          "adapter_path",
          "thinking_enabled",
        ];
        var values = {
          base_url: "https://api." + name + ".com",
          model: "",
          api_key: "",
          adapter: name,
          adapter_path: "",
          thinking_enabled: "false",
        };
        var promises = keys.map(function (f) {
          return api("/settings", {
            method: "PUT",
            body: JSON.stringify({
              key: "core.llm.sources." + name + "." + f,
              value: values[f],
            }),
          });
        });
        Promise.all(promises)
          .then(function () {
            toast(
              __("源", "Source") +
                ' "' +
                name +
                '" ' +
                __(
                  "已创建，请配置各项参数",
                  "created, please configure parameters",
                ),
            );
            renderAll();
          })
          .catch(function (e) {
            toast(__("创建失败: ", "Create failed: ") + e.message, true);
          });
      }

      async function deleteSource(name) {
        if (
          !confirm(
            __("确认删除源", "Are you sure to delete source") +
              ' "' +
              name +
              '"?',
          )
        )
          return;
        var base = "core.llm.sources." + name;
        var fields = [
          "adapter",
          "base_url",
          "model",
          "api_key",
          "thinking_enabled",
          "adapter_path",
        ];
        try {
          for (var f of fields) {
            await api("/settings", {
              method: "PUT",
              body: JSON.stringify({ key: base + "." + f, value: null }),
            });
          }
          toast(
            __("源", "Source") + ' "' + name + '" ' + __("已删除", "deleted"),
          );
          renderAll();
        } catch (e) {
          toast(__("删除失败: ", "Delete failed: ") + e.message, true);
        }
      }

      function addMCPSource() {
        var name = prompt(
          __("输入新 MCP 服务器名称:", "Enter new MCP server name:"),
        );
        if (!name || !name.trim()) return;
        name = name
          .trim()
          .toLowerCase()
          .replace(/[^a-z0-9_]/g, "_");
        if (!name) {
          toast(__("名称无效", "Invalid name"), true);
          return;
        }
        var fields = ["command", "url", "args", "env"];
        var values = { command: "", url: "", args: "[]", env: "[]" };
        var promises = fields.map(function (f) {
          return api("/settings", {
            method: "PUT",
            body: JSON.stringify({
              key: "plugin.mcp.servers." + name + "." + f,
              value: values[f],
            }),
          });
        });
        Promise.all(promises)
          .then(function () {
            toast(
              "MCP " +
                __("服务器", "server") +
                ' "' +
                name +
                '" ' +
                __("已创建", "created"),
            );
            renderAll();
          })
          .catch(function (e) {
            toast(__("创建失败: ", "Create failed: ") + e.message, true);
          });
      }

      async function deleteMCPServer(name) {
        if (
          !confirm(
            __("确认删除 MCP 服务器", "Are you sure to delete MCP server") +
              ' "' +
              name +
              '"?',
          )
        )
          return;
        var fields = ["command", "url", "args", "env"];
        try {
          for (var f of fields) {
            await api("/settings", {
              method: "PUT",
              body: JSON.stringify({
                key: "plugin.mcp.servers." + name + "." + f,
                value: null,
              }),
            });
          }
          toast(
            "MCP " +
              __("服务器", "server") +
              ' "' +
              name +
              '" ' +
              __("已删除", "deleted"),
          );
          renderAll();
        } catch (e) {
          toast(__("删除失败: ", "Delete failed: ") + e.message, true);
        }
      }

      // ===== Adapters =====
      async function renderAdapters() {
        var html = "";
        try {
          var r = await api("/adapters");
          var adapters = r.adapters || [];
          window._adapters = adapters;
          html +=
            '<div class="card"><h2>' +
            __("已加载的适配器", "Loaded Adapters") +
            " (" +
            adapters.length +
            ")</h2>";
          if (adapters.length === 0) {
            html +=
              '<p style="color:var(--text-muted)">' +
              __("暂无适配器", "No adapters") +
              "</p>";
          } else {
            html +=
              "<table><tr><th>" +
              __("名称", "Name") +
              "</th><th>" +
              __("版本", "Version") +
              "</th><th>" +
              __("操作", "Actions") +
              "</th></tr>";
            adapters.forEach(function (a) {
              html +=
                "<tr><td>" +
                escHtml(a.name) +
                "</td><td>" +
                escHtml(a.version || "-") +
                "</td>" +
                '<td><button class="btn btn-danger btn-sm" onclick="deleteAdapter(\'' +
                escHtml(a.name) +
                "')\">" +
                __("删除", "Delete") +
                "</button></td></tr>";
            });
            html += "</table>";
          }
          html += "</div>";
          html +=
            '<div class="card"><h2>' +
            __("上传新适配器", "Upload New Adapter") +
            "</h2>" +
            "<label>" +
            __("适配器名称（不带 .lua）", "Adapter name (without .lua)") +
            "</label>" +
            '<input id="adapter-name" placeholder="' +
            __("如 openai", "e.g. openai") +
            '">' +
            "<label>" +
            __("Lua 脚本代码", "Lua Script Code") +
            "</label>" +
            '<textarea id="adapter-code" rows="12" placeholder="-- ' +
            __("返回一个适配器表", "return an adapter table") +
            '\nreturn {\n  name = \&quot;openai\&quot;,\n  version = \&quot;1.0\&quot;,\n  transform_request = function(raw) ... end,\n  transform_response = function(raw) ... end,\n}"></textarea>' +
            '<button class="btn btn-primary" onclick="uploadAdapter()">' +
            __("上传", "Upload") +
            "</button></div>";
        } catch (e) {
          html +=
            '<div class="card"><p style="color:var(--text-muted)">' +
            __("加载适配器失败: ", "Failed to load adapters: ") +
            escHtml(e.message) +
            "</p></div>";
        }
        document.getElementById("tab-adapters").innerHTML = html;
      }

      async function uploadAdapter() {
        var name = document.getElementById("adapter-name")?.value;
        var code = document.getElementById("adapter-code")?.value;
        if (!name || !code) {
          toast(
            __("名称和代码不能为空", "Name and code cannot be empty"),
            true,
          );
          return;
        }
        try {
          var r = await api("/adapters", {
            method: "POST",
            body: JSON.stringify({ name: name, code: code }),
          });
          if (r.status === "loaded") {
            toast(
              __("适配器", "Adapter") +
                ' "' +
                name +
                '" ' +
                __("已加载", "loaded"),
            );
            renderAdapters();
          } else {
            toast(
              __("上传失败: ", "Upload failed: ") + (r.error || "unknown"),
              true,
            );
          }
        } catch (e) {
          toast(__("上传失败: ", "Upload failed: ") + e.message, true);
        }
      }

      async function deleteAdapter(name) {
        if (
          !confirm(
            __("确定删除适配器", "Are you sure to delete adapter") +
              ' "' +
              name +
              '"？',
          )
        )
          return;
        try {
          var r = await api("/adapters/" + encodeURIComponent(name), {
            method: "DELETE",
          });
          if (r.status === "deleted") {
            toast(
              __("适配器", "Adapter") +
                ' "' +
                name +
                '" ' +
                __("已删除", "deleted"),
            );
            renderAdapters();
          } else {
            toast(__("删除失败", "Delete failed"), true);
          }
        } catch (e) {
          toast(__("删除失败: ", "Delete failed: ") + e.message, true);
        }
      }

      // ===== Init =====
      async function logout() {
        try {
          await fetch("/api/v1/logout", {
            method: "POST",
            credentials: "include",
          });
        } catch (e) {}
        location.href = "/login";
      }

      renderConfigDisabled();
      // 先加载历史再连 SSE：避免 SSE 事件先到与历史加载顺序不确定导致消息重复/丢失
      (async function () {
        await loadChatHistory();
        renderAll();
        connectSSE();
        startUptimeTicker();
        startRuntimeTicker();
        maybeShowPersonaWizard();
      })();
      setInterval(renderAll, 15000);
      // 消息同步轮询兜底：每30秒增量同步 chatHistory，补偿 SSE 断连窗口期
      // 丢失的事件（尤其是非 WebUI 触发的跨渠道消息，如 CLI/QQ/设备桥输出）。
      // syncChatFromHistory 仅追加新消息 DOM 节点，不重建已有消息，无闪烁。
      setInterval(function () {
        syncChatFromHistory().catch(function(){});
      }, 30000);
