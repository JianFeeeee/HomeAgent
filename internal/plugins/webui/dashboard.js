      // ===== State =====
      const state = {
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
        pipelinePhase: "", // 当前阶段（SSE stage 事件驱动总览页）
        pipelineTimer: null,
        stageTrail: [], // 本轮已发生的事件轨迹（工具/输出调用，供阶段管道展示循环）
        toolFlash: false, // 本轮新到一条工具调用：本轮渲染后给「工具」格放一次滑入动画
        healthResult: null,
        starmapInit: false,
        starmapLoading: false,
        starmapData: null,
        // ===== 星图：跟随 agent 活动 =====
        // 三路活动信号（均已在页面上，无需新数据源）：
        //   1. toolPulseMs —— SSE tool_call/stage 触发的「脉冲」，实时
        //   2. activity   —— /runtime 调度器计数（排队/中断/抢占），3s 轮询
        //   3. grownIds   —— /memory/graph/pulse 检出「新长出来」的节点，10s 轮询
        starmapPulses: [], // { mesh, until, kind } 活动脉冲队列
        starmapGrown: {}, // nodeId -> 生长动画截止时间戳
        starmapActivity: null, // /runtime 的调度器快照
        starmapPulseTimer: null, // /memory/graph/pulse 轮询 id
        starmapActivityTimer: null, // /runtime 轮询 id
        starmapLastPulseAt: 0, // 最后一次活动时间（ms），驱动全局呼吸
        starmapPulseSince: 0, // 下次 pulse 请求的回看起点（unix 秒）
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
        chatLastSeq: 0, // 增量轮询游标：已合并到本地的最大消息 seq
        lang: localStorage.getItem("ha-lang") || "zh",
      };

      // ===== I18n =====
      window._i18n = {
        navOverview: ["概览", "Overview"],
        navChat: ["对话", "Chat"],
        navStarmap: ["星图", "Star Map"],
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
        document.querySelectorAll("[data-i18n]").forEach((el) => {
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
        document.querySelectorAll("[data-i18n]").forEach((el) => {
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
        // mono 放第一位：它现在是默认配色（黑白单调），也应当是选择器里最显眼那个。
        mono: "#c2c7d0",
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
        // 配色变了要强制重画运行态：拓扑的归属配色跟着 data-color 走，
        // 而数据签名没变，不置空 _rtSig 的话图会一直停在旧色。
        _rtSig = null;
        if (document.getElementById("rt-sec-topo")) {
          try {
            renderRuntime();
          } catch (e) {}
        }
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
          var cur = localStorage.getItem("ha-color") || "mono";
          var img = localStorage.getItem("ha-bg-img") || "";
          var blur = localStorage.getItem("ha-bg-blur") || "0";
          var dots = "";
          Object.keys(PALETTES).forEach((k) => {
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

      (() => {
        var saved = localStorage.getItem("ha-theme");
        setTheme(saved || "light");
        // 默认配色 = 黑白：必须**主动** setColor 一次。只设 data-theme 时
        // base 主题仍是樱粉（:root 的 --accent 就是粉色），不写 data-color 就
        // 会默认花。用户选过别的颜色时以 localStorage 为准。
        setColor(localStorage.getItem("ha-color") || "mono");
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
        t._hideTimer = setTimeout(() => {
          t.style.display = "none";
        }, 3000);
      }

      // ===== 8.6 Unified toast + confirm dialog =====

      // ===== 8.4 Card 3D tilt + cursor glow =====
      document.addEventListener("mousemove", (e) => {
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
      document.addEventListener("mouseleave", (e) => {
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
          ov.querySelectorAll("button[data-mode]").forEach((b) => {
            b.onclick = () => {
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
        document.querySelectorAll(".tab-content").forEach((e) => {
          e.classList.remove("active");
        });
        var el = document.getElementById("tab-" + n);
        if (el) el.classList.add("active");
        document.querySelectorAll("nav a").forEach((e) => {
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
        if (n === "starmap") {
          // 独立星图页签：把 canvas 搬过来并按新容器尺寸重算。
          // 必须在 renderAll 之前做，否则 renderChatStarmap 会按上一次的
          // 活跃容器（总览页）算尺寸。
          var sc = document.getElementById("sm-container-page");
          if (sc) {
            if (starmapRen) {
              if (starmapRen.domElement.parentElement !== sc) {
                sc.appendChild(starmapRen.domElement);
                starmapRen.domElement.style.display = "block";
              }
              onStarmapResize();
            } else if (!state.starmapInit && !state.starmapLoading) {
              loadChatStarmapData();
            }
          }
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
          await loadProxyServices();
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
          renderStarmapTab();
        } catch (e) {
          console.error("renderStarmapTab", e);
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
              .forEach((c) => {
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
        uptimeTick = setInterval(() => {
          var el = document.querySelector("#ov-uptime");
          if (el && state.startedAt) {
            var now = Date.now();
            el.textContent = fmtUptime(now - state.startedAt);
          } else if (!state.startedAt) {
            var el2 = document.querySelector("#ov-uptime");
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
      // 面板从上到下：四个数字块回答"现在忙不忙"；阶段管道滑块回答"这一轮走到哪"；
      // 五条队列条形 + 中断栈回答"堵在哪一级/压了几层"；最后的分带拓扑回答
      // "哪条输入喂给哪个 agent、哪个 agent 往哪条输出写、各自负载多高"。
      // 四级语义直接照抄内核（internal/agent/core/scheduler.go 的 Level 定义），
      // 别自己起名字——前端叫法一旦和内核不一致，看板就成了误导。
      var RT_LEVELS = [
        { lv: 4, name: "L4", desc: "内核独占", cls: "rt-lv-4" },
        { lv: 3, name: "L3", desc: "交互", cls: "rt-lv-3" },
        { lv: 2, name: "L2", desc: "消息", cls: "rt-lv-2" },
        { lv: 1, name: "L1", desc: "后台", cls: "rt-lv-1" },
      ];
      var RT_CAP_NAMES = { 1: "text", 2: "file", 4: "image", 8: "audio", 16: "structured" };

      // _rtSig 缓存上一次渲染的数据签名。
      //
      // 为什么必须缓存：运行态每 3s 轮询一次，数据绝大多数时候是**没变**的；
      // 无条件 `innerHTML =` 会把整块 DOM（含各级条的 transition）每 3s 重建一遍，
      // 视觉效果就是“首页一闪一闪”。签名相同就一个字节也不动。
      var _rtSig = null;

      // morph：把容器的 DOM 就地「形变」成 html 描述的样子。
      //
      // 为什么不用 innerHTML = html：整块重建会把**没变的**节点也换掉 ——
      // SVG 过渡从头播、图片重解码、展开态丢失、滚动锚点重置。运行态每几秒
      // 刷一次，未变节点占绝大多数，那些闪烁就是这么来的。
      //
      // 做法：按「子节点位置 + nodeName」递归对账。同名元素复用同一个 DOM 节点，
      // 只同步发生变化的属性与文本；只有标签真的不同才替换。元素身份不变 ⇒
      // CSS 过渡继续、不闪。
      function morph(container, html) {
        var tpl = document.createElement("div");
        tpl.innerHTML = html;
        morphChildren(container, tpl);
      }
      function morphChildren(oldParent, newParent) {
        var olds = Array.prototype.slice.call(oldParent.childNodes);
        var news = Array.prototype.slice.call(newParent.childNodes);
        var n = Math.max(olds.length, news.length);
        for (var i = 0; i < n; i++) {
          var o = olds[i], x = news[i];
          if (!o && x) {
            oldParent.appendChild(x);
            continue;
          }
          if (o && !x) {
            oldParent.removeChild(o);
            continue;
          }
          if (o.nodeType !== x.nodeType || o.nodeName !== x.nodeName) {
            oldParent.replaceChild(x, o);
            continue;
          }
          if (o.nodeType === 3 || o.nodeType === 8) {
            if (o.nodeValue !== x.nodeValue) o.nodeValue = x.nodeValue;
            continue;
          }
          morphAttrs(o, x);
          morphChildren(o, x);
        }
      }
      function morphAttrs(o, x) {
        var i, a;
        for (i = o.attributes.length - 1; i >= 0; i--) {
          a = o.attributes[i];
          if (!x.hasAttribute(a.name)) o.removeAttribute(a.name);
        }
        for (i = 0; i < x.attributes.length; i++) {
          a = x.attributes[i];
          if (o.getAttribute(a.name) !== a.value) o.setAttribute(a.name, a.value);
        }
      }

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
        Object.keys(RT_CAP_NAMES).forEach((bit) => {
          if (caps & Number(bit)) {
            out += '<span class="rt-cap">' + RT_CAP_NAMES[bit] + "</span>";
          }
        });
        return out ? '<span class="rt-caps">' + out + "</span>" : "";
      }

      // 归属配色：根用青色，驻留子轮转其余颜色（同一张图里能一眼分清谁是谁）。
      var RT_OWNER_COLORS = ["#88c0d0", "#b48ead", "#a3be8c", "#ebcb8b", "#d08770", "#8fbcbb"];
      // 黑白配色下，拓扑归属也用灰阶 —— 否则整页只剩这张图还是一片彩。
      var RT_OWNER_COLORS_MONO = ["#e6e9ef", "#c2c7d0", "#9aa0ad", "#7b818d", "#5f6570", "#454a55"];
      function rtOwnerColors() {
        return document.documentElement.getAttribute("data-color") === "mono"
          ? RT_OWNER_COLORS_MONO
          : RT_OWNER_COLORS;
      }

      // rtOwnerGroups 把 inputch 按**归属**分组（owner == 根 agent id 与 "" 归一为根）。
      function rtOwnerGroups(inputs, residents, rootID) {
        var order = [];
        var map = {};
        inputs.forEach((c) => {
          var o = c.owner || "";
          if (o === rootID) o = "";
          if (!map[o]) {
            map[o] = [];
            order.push(o);
          }
          map[o].push(c);
        });
        // 驻留子即使一条 inputch 都没划到也要出现——否则「子存在但看不见」
        // 与「子不存在」无法区分。
        residents.forEach((r) => {
          var o = r.id || "";
          if (o && o !== rootID && !map[o]) {
            map[o] = [];
            order.push(o);
          }
        });
        order.sort((a, b) => {
          if (a === "") return -1;
          if (b === "") return 1;
          return a < b ? -1 : 1;
        });
        var ci = 0;
        var OW = rtOwnerColors();
        return order.map((o) => {
          var res = null;
          residents.forEach((r) => {
            if (r.id === o) res = r;
          });
          var list = map[o] || [];
          return {
            owner: o,
            child: o !== "",
            label: o === "" ? __("根 agent / 内核默认", "root agent / kernel default") : __("驻留子 ", "resident ") + o,
            color: o === "" ? OW[0] : OW[1 + (ci++ % (OW.length - 1))],
            list: list,
            count: list.length,
            res: res,
          };
        });
      }

      // ---- 阶段管道滑块 ----
      //
      // 七阶段与内核 sdk.Stage 一一对应（顺序即执行顺序），由 SSE `stage` 事件驱动：
      // 哪个阶段在执行，滑块就滑到哪一格。空闲时整条管道降透明度，不做假动画。
      //
      // 为什么放进总览：阶段管道回答"这一轮走到哪一步"，总览其余图形回答"积压了多少"，
      // 两者合起来才是运行态。此前它只是对话页一个 10px 的角标，等于看不见。
      // 阶段管道：**循环流程 + 本轮轨迹**，不是单向滑块。
      //
      // 一轮里工具调用会反复回到「行动后」再接下一个工具（post_action →
      // before_toolcall → after_toolcall → post_action …），还可能中途多次
      // 输出，所以线性滑块本身就是错的表述。这里画成
      //   输入 → 行动 ⇄(工具) → 输出 → 结束
      // （工具那格带循环标记），下面再用一排 chip 记下**本轮真实发生过什么**：
      // 普通工具与 output_* 输出通道调用用不同配色区分开来。
      // 页面上**任何位置都不用 emoji/符号字符充当图标** —— 一律内联 SVG，
      // 24x24 viewBox + currentColor stroke，随主题与状态变色，不额外引资源。
      var RT_ICO = {
        in: '<svg class="rt-ico" viewBox="0 0 24 24"><path d="M21 12H8"/><path d="M13 6l-6 6 6 6"/></svg>',
        act: '<svg class="rt-ico" viewBox="0 0 24 24"><circle cx="12" cy="12" r="3.2"/><path d="M12 2v3M12 19v3M2 12h3M19 12h3M5.5 5.5l2.1 2.1M16.4 16.4l2.1 2.1M18.5 5.5l-2.1 2.1M7.6 16.4l-2.1 2.1"/></svg>',
        tool: '<svg class="rt-ico" viewBox="0 0 24 24"><path d="M14.5 6.5a3.8 3.8 0 0 1 5 5L10 21l-5-5z"/><path d="M14.5 6.5 17.5 9.5"/></svg>',
        out: '<svg class="rt-ico" viewBox="0 0 24 24"><path d="M4 12h13"/><path d="M13 6l6 6-6 6"/></svg>',
        done: '<svg class="rt-ico" viewBox="0 0 24 24"><path d="M20 6 9 17l-5-5"/></svg>',
        loop: '<svg class="rt-ico" viewBox="0 0 24 24"><path d="M20.5 12a8.5 8.5 0 1 1-2.5-6"/><path d="M21 3.5V9h-5.5"/></svg>',
        bolt: '<svg class="rt-ico" viewBox="0 0 24 24"><path d="M13 2 4.5 13.5H11l-1 8.5L18.5 10H12z"/></svg>',
        caret: '<svg class="rt-ico rt-ico-sm" viewBox="0 0 24 24"><path d="M6 9l6 6 6-6"/></svg>',
      };

      var RT_PIPE_GROUPS = [
        { zh: "输入", en: "in", ico: "in" },
        { zh: "行动", en: "act", ico: "act" },
        { zh: "工具", en: "tool", ico: "tool", loop: true },
        { zh: "输出", en: "out", ico: "out" },
        { zh: "结束", en: "done", ico: "done" },
      ];
      function rtPhaseGroup(phase) {
        switch (phase) {
          case "on_input":
            return 0;
          case "pre_action":
          case "post_action":
            return 1;
          case "before_toolcall":
          case "after_toolcall":
            return 2;
          case "before_output":
            return 3;
          case "after_output":
            return 4;
        }
        return -1;
      }
      // rtShortTool 把 `qq_get_message` / `output_send__qq` 压成尾段短名。
      function rtShortTool(name) {
        var n = String(name || "");
        var i = n.lastIndexOf("__");
        if (i >= 0) n = n.slice(i + 2);
        return n.length > 14 ? n.slice(0, 13) + "…" : n;
      }
      // rtSlots 画一组「车位」式格槽：槽位数量固定可见，被占用的点亮。
      //
      // 为什么不用进度条：队列为 0 时进度条宽度就是 0，整行只剩一串文字，
      // 看上去就是「这块空着」。格槽在 0 时仍有形状，占用多少一眼可数，
      // 也不用为了「看得见」而给 0 画一条假进度。
      function rtSlots(depth, slots, extraCls) {
        var n = Math.max(5, Math.min(16, slots || 5));
        var d = depth || 0;
        var out = '<span class="rt-slots ' + (extraCls || "") + '">';
        for (var i = 0; i < n; i++) out += '<i class="' + (i < d ? "on" : "") + '"></i>';
        // 溢出计数必须留在 .rt-slots 内：格槽是 flex 行，多一个兄弟节点会被挤出去，
        // 多一个兄弟节点会被挤到下一行，整块队列就串了。
        if (d > n) out += '<b class="rt-slots-more">+' + (d - n) + "</b>";
        return out + "</span>";
      }
      // rtChipEl 画一枚事件 chip。工具与输出通道调用各有配色；名称包进
      // .rt-chip-t，让窄框里由它做省略号截断（.rt-chip 本身是 flex 容器，
      // text-overflow 对直接子文本不生效，会被硬切掉半截）。
      function rtChipEl(t) {
        var kind = t.kind || "stage";
        var ico = kind === "output" ? RT_ICO.out : kind === "tool" ? RT_ICO.tool : "";
        return (
          '<span class="rt-chip rt-chip-' + kind + '" title="' + escHtml(t.label) + '">' + ico +
          '<b class="rt-chip-t">' + escHtml(t.short || t.label) + "</b>" +
          (t.n > 1 ? '<i class="rt-chip-n">x' + t.n + "</i>" : "") +
          "</span>"
        );
      }
      // rtPipelineHtml 画阶段管道：**五个等大的表框**，每框里是本阶段本轮发生的事。
      //
      // 为什么不是「小圆点 + 连接线 + 9px 小字」：那种画法在总览里几乎读不出
      // 「现在走到哪一步」，尺寸也远小于旁边的 KPI 框。现在与中断队列、KPI 用同
      // 一套视觉语言（等大框 + 大号加粗标题），并且事件直接落在它所属的框里，
      // 所以「这一步发生了什么」不需要靠颜色或图例去猜。
      function rtPipelineHtml(phase, trail) {
        var g = rtPhaseGroup(phase);
        var cells = RT_PIPE_GROUPS.map((s, i) => {
          var items = (trail || []).filter((t) => (t.g | 0) === i);
          // 「工具」格是**循环**格：一轮里可能调几十次工具/输出通道。把每一次都
          // 追加成 chip，这格会被撑成一长条，读者反而看不出「现在正在调什么」。
          // 所以它只保留**最新一条**，旧条在同一行视口里向上滚走
          // （rtFlashLatestTool 触发滑入），右侧另给本轮累计次数。
          var cls = "rt-pipe-events";
          var body;
          if (!items.length) {
            body = '<span class="rt-chip rt-chip-none">' + __("无", "none") + "</span>";
          } else if (s.loop) {
            cls += " rt-pipe-scroll";
            var total = 0;
            for (var k = 0; k < items.length; k++) total += items[k].n || 1;
            body =
              rtChipEl(items[items.length - 1]) +
              '<i class="rt-scroll-count" title="' +
              __("本轮工具调用累计次数", "tool calls this turn") + '">x' + total + "</i>";
          } else {
            body = items.map(rtChipEl).join("");
          }
          return (
            '<div class="rt-pipe-cell' + (i === g ? " active" : "") + '">' +
            '<div class="rt-pipe-head">' + RT_ICO[s.ico] +
            "<b>" + __(s.zh, s.en) + "</b>" +
            (s.loop
              ? '<em class="rt-loop" title="' +
                __("工具调用会回到行动后，可多次", "tool calls loop back; may repeat") +
                '">' + RT_ICO.loop + "</em>"
              : "") +
            "</div>" +
            '<div class="' + cls + '">' + body + "</div>" +
            "</div>"
          );
        }).join("");
        return (
          '<div class="rt-section-title">' +
          __("阶段管道", "Stage pipeline") +
          (g < 0 ? "　" + __("（空闲）", "(idle)") : "") +
          "</div>" +
          '<div class="rt-pipe-row' + (g < 0 ? " rt-pipe-idle" : "") + '">' + cells + "</div>"
        );
      }

      // rtTrailPush 把一条「本轮发生过的事」落到它实际发生的阶段列里。
      // 同一阶段重复出现同一条（如同一工具连调 3 次）只累加计数，不刷屏。
      function rtTrailPush(g, kind, label, short) {
        if (!state.stageTrail) state.stageTrail = [];
        var arr = state.stageTrail;
        var last = arr.length ? arr[arr.length - 1] : null;
        if (last && last.g === g && last.kind === kind && last.short === short) {
          last.n = (last.n || 1) + 1;
          return;
        }
        arr.push({ g: g, kind: kind, label: label, short: short, n: 1 });
        if (arr.length > 24) arr.shift();
      }

      // rtFlashLatestTool 让「工具」格里最新那条做一次「向上滚入」。
      //
      // morph 是就地改文本：新工具到来时 chip 节点不会被替换，纯 CSS 的
      // animation 因此不会自动重放。这里摘类 → 强制 reflow → 重加类，重置动画
      // 时间轴。prefers-reduced-motion 由样式表统一压到 0.01ms，无需在此判断。
      function rtFlashLatestTool() {
        var box = document.querySelector(".rt-pipe-events.rt-pipe-scroll");
        if (!box) return;
        var chip = box.querySelector(".rt-chip");
        if (!chip) return;
        chip.classList.remove("rt-chip-enter");
        void chip.offsetWidth; // 强制 reflow：少了这句，重加类不会重启动画
        chip.classList.add("rt-chip-enter");
      }

      // ---- per-agent 负载 ----
      //
      // 负载 = 该 agent **自己的**调度器积压折算成的百分比。
      //
      // 为什么按级别加权：四级中断里 L4 是内核独占、L3 是交互，堵在 L4 一条比堵在
      // L1 五条更严重；中断栈深度意味着有现场被压着。权重是启发式的（"满载"没有
      // 硬定义），因此函数做成饱和式 backlog/(backlog+K)：单调、上界 100、不越界。
      // context_full 单独加分：子上下文满了以后每轮都要压缩，本身就是高负载。
      function rtLoadPct(sc, ctxFull) {
        sc = sc || {};
        var q = sc.interrupt_queues || [0, 0, 0, 0, 0];
        var backlog =
          (sc.ready_queue_depth || 0) +
          (sc.pending_interrupts || 0) * 1.5 +
          (sc.suspend_stack || 0) * 2 +
          (q[1] || 0) * 1 +
          (q[2] || 0) * 1.2 +
          (q[3] || 0) * 1.5 +
          (q[4] || 0) * 2;
        var pct = 100 * (backlog / (backlog + 8));
        if (ctxFull) pct += 15;
        return Math.max(0, Math.min(100, Math.round(pct)));
      }

      // rtAgents 把「inputch 归属 + 驻留子 + 各自的调度器积压」整理成每个 agent 一行。
      // 根 agent 的积压来自 rt.scheduler；驻留子的来自它自己的 ResidentStatus
      // （见 internal/sdk/status.go 的 ReadyQueueDepth 等四项）。
      function rtAgents(rt) {
        var rootID = rt.agent_id || "";
        var groups = rtOwnerGroups(rt.input_channels || [], rt.residents || [], rootID);
        var outChans = (rt.channels || []).filter((c) => c.direction === "out" || c.direction === "io");
        return groups.map((g) => {
          var id = g.owner || rootID;
          var outputs = [];
          var seen = {};
          function add(name) {
            if (!name || seen[name]) return;
            seen[name] = 1;
            outputs.push(name);
          }
          if (g.child) {
            // 驻留子：优先用父显式授权的输出；没有授权登记时退回它自己 inputch 的
            // 默认回程（`output` 字段）。
            ((g.res && g.res.allowed_outputs) || []).forEach(add);
            if (!outputs.length) g.list.forEach((c) => { add(c.output); });
          } else {
            // 根 agent 可以写任何输出通道；上限交给渲染侧截断。
            outChans.forEach((c) => { add(c.name); });
          }
          var load = g.child
            ? rtLoadPct(
                {
                  ready_queue_depth: g.res && g.res.ready_queue_depth,
                  pending_interrupts: g.res && g.res.pending_interrupts,
                  suspend_stack: g.res && g.res.suspend_stack,
                  interrupt_queues: (g.res && g.res.interrupt_queues) || [],
                },
                g.res && g.res.context_full,
              )
            : rtLoadPct(rt.scheduler || {}, false);
          return {
            id: id, child: g.child, label: g.label, color: g.color,
            inputs: g.list, outputs: outputs, load: load, res: g.res || null,
          };
        });
      }

      // 连线路径表：通道名 → path d。光点动画靠它把「哪个通道」翻成一条曲线。
      var _rtEdgeIn = {};
      var _rtEdgeOut = {};
      var RT_SVGNS = "http://www.w3.org/2000/svg";

      // rtAgentTopology 画「通道 → agent → 通道」的分带拓扑。
      //
      // 每个 agent 一条横带：左边是归它的 inputch，中间是它自己（圆环 = 负载），
      // 右边是它能写的 outputch。连线即路由；光点沿连线跑表示消息正在流动。
      // 与旧版「归属框 → 单个内核盒」的差别：内核盒只有一个，看不出"这条输入到底
      // 喂给了哪个子"，而子 agent 才是运行态里最该看清的东西。
      // rtTopoWidth 用**实测容器宽**当 viewBox 宽：viewBox 宽与渲染宽一致时缩放才是
      // 1:1。此前固定 640 而容器 ~920，浏览器按 "meet" 把内容顶在左上、右侧空出一大块，
      // 观感就是"间距不对"。
      function rtTopoWidth() {
        var host = document.getElementById("rt-sec-topo");
        var w = host ? host.clientWidth : 0;
        if (!w) {
          var panel = document.getElementById("rt-panel");
          w = panel ? panel.clientWidth : 0;
        }
        if (!w || w < 360) w = 900;
        return Math.round(w) - 4;
      }

      // rtClip 按估算宽度截断长通道名（SVG 没有 text-overflow，只能自己量）。
      // 全角按 1em、其余按 0.56em 估宽：只求不越界，不求像素级精确。
      function rtClip(text, maxPx, fontPx) {
        var s = String(text == null ? "" : text);
        var w = 0;
        var i = 0;
        for (; i < s.length; i++) {
          var cw = s.charCodeAt(i) > 0x2e80 ? fontPx : fontPx * 0.56;
          if (w + cw > maxPx) break;
          w += cw;
        }
        if (i >= s.length) return s;
        return s.slice(0, Math.max(1, i - 1)) + "…";
      }

      function rtAgentTopology(agents) {
        var W = rtTopoWidth();
        var ROW = 30, BOX_H = 20, GAP = 14, NODE_R = 20, MAX_OUT = 8;
        // 三列按比例分：输入 0~32%，节点居中在 44%，输出 56%~100%，两侧留白对称。
        var IN_X = 0, IN_W = Math.round(W * 0.32);
        var NX = Math.round(W * 0.44), OUT_X = Math.round(W * 0.56), OUT_W = W - Math.round(W * 0.56) - 2;
        var out = [];
        _rtEdgeIn = {};
        _rtEdgeOut = {};
        function esc(s) {
          return String(s == null ? "" : s).replace(/[<>&]/g, (m) => m === "<" ? "&lt;" : m === ">" ? "&gt;" : "&amp;");
        }
        function pathD(x1, y1, x2, y2) {
          var mx = (x1 + x2) / 2;
          return "M" + x1 + " " + y1 + " C" + mx + " " + y1 + " " + mx + " " + y2 + " " + x2 + " " + y2;
        }
        var y = 8;
        if (!agents.length) {
          out.push('<text class="tp-hint" x="8" y="24">' + __("暂无通道 / agent", "no channels / agents") + "</text>");
          y = 44;
        }
        agents.forEach((a) => {
          var nin = a.inputs.length;
          var nout = Math.min(a.outputs.length, MAX_OUT);
          var rows = Math.max(nin, nout, 1);
          var truncated = a.outputs.length > MAX_OUT;
          // 内容高必须**容得下节点块**（圆环 + 下方标签一行），否则单行带里节点和它的
          // 名字会溢出到上/下一个带上（"间距不对"的一个来源）。
          var nodeBlock = NODE_R * 2 + (a.res && a.res.context_full ? 52 : 20);
          var contentH = Math.max(rows * ROW, nodeBlock, truncated ? nout * ROW + 18 : 0);
          var bandH = contentH + GAP;
          var cy = y + contentH / 2;
          // 短的一列在带内**居中**：连线短而对称，不再出现"左边挤在上半、右边铺满"的错位感。
          var inTop = y + (contentH - nin * ROW) / 2;
          var outTop = y + (contentH - nout * ROW) / 2;
          out.push('<rect x="0" y="' + (y - 3) + '" width="' + W + '" height="' + (bandH - 4) + '" rx="10" fill="' + a.color + '" fill-opacity="0.05"/>');
          out.push('<rect x="0" y="' + (y - 3) + '" width="3" height="' + (bandH - 4) + '" rx="1.5" fill="' + a.color + '" fill-opacity="0.7"/>');

          // 输入通道 → agent
          a.inputs.forEach((c, i) => {
            var ry = inTop + i * ROW + ROW / 2;
            out.push('<rect x="' + IN_X + '" y="' + (ry - BOX_H / 2) + '" width="' + IN_W + '" height="' + BOX_H + '" rx="7" fill="rgba(255,255,255,0.05)" stroke="' + a.color + '" stroke-opacity="0.35"/>');
            out.push('<text class="tp-name" x="' + (IN_X + 10) + '" y="' + (ry + 4) + '">' + esc(rtClip(c.name, IN_W - 44, 12)) + "<title>" + esc(c.name) + "</title></text>");
            if (c.capacity) {
              out.push('<text class="tp-sub" x="' + (IN_X + IN_W - 9) + '" y="' + (ry + 4) + '" text-anchor="end">' + esc(c.capacity) + "</text>");
            }
            var d = pathD(IN_X + IN_W, ry, NX - NODE_R - 2, cy);
            _rtEdgeIn[c.name] = d;
            out.push('<path class="tp-edge tp-edge-in" d="' + d + '" stroke="' + a.color + '"/>');
          });

          // agent 节点：两圈 + 一段负载弧（stroke-dasharray 画进度）
          var C = 2 * Math.PI * (NODE_R - 3);
          out.push('<circle cx="' + NX + '" cy="' + cy + '" r="' + NODE_R + '" fill="rgba(13,13,22,0.72)" stroke="' + a.color + '" stroke-opacity="0.55"/>');
          out.push('<circle cx="' + NX + '" cy="' + cy + '" r="' + (NODE_R - 3) + '" fill="none" stroke="rgba(255,255,255,0.10)" stroke-width="3.5"/>');
          out.push(
            '<circle cx="' + NX + '" cy="' + cy + '" r="' + (NODE_R - 3) + '" fill="none" stroke="' + a.color +
            '" stroke-width="3.5" stroke-linecap="round" stroke-dasharray="' + ((a.load / 100) * C).toFixed(1) + " " + C.toFixed(1) +
            '" transform="rotate(-90 ' + NX + " " + cy + ')"/>',
          );
          out.push('<text class="tp-val" x="' + NX + '" y="' + (cy + 4) + '" text-anchor="middle">' + a.load + "</text>");
          out.push('<text class="tp-lab" x="' + NX + '" y="' + (cy + NODE_R + 15) + '" text-anchor="middle" fill="' + a.color + '">' + esc(rtClip(a.child ? a.id : __("根 agent", "root"), 140, 11)) + "</text>");
          if (a.res && a.res.context_full) {
            out.push('<text class="tp-warn" x="' + NX + '" y="' + (cy + NODE_R + 30) + '" text-anchor="middle">' + __("上下文已满", "ctx full") + "</text>");
          }

          // agent → 输出通道
          a.outputs.slice(0, MAX_OUT).forEach((name, i) => {
            var ry = outTop + i * ROW + ROW / 2;
            out.push('<rect x="' + OUT_X + '" y="' + (ry - BOX_H / 2) + '" width="' + OUT_W + '" height="' + BOX_H + '" rx="7" fill="rgba(255,255,255,0.04)" stroke="rgba(255,255,255,0.12)"/>');
            out.push('<text class="tp-name" x="' + (OUT_X + 10) + '" y="' + (ry + 4) + '">' + esc(rtClip(name, OUT_W - 20, 12)) + "<title>" + esc(name) + "</title></text>");
            var d = pathD(NX + NODE_R + 2, cy, OUT_X, ry);
            _rtEdgeOut[a.id + "\u0000" + name] = d;
            out.push('<path class="tp-edge tp-edge-out" d="' + d + '" stroke="' + a.color + '"/>');
          });
          if (a.outputs.length > MAX_OUT) {
            out.push('<text class="tp-hint" x="' + OUT_X + '" y="' + (outTop + nout * ROW + 11) + '">+' + (a.outputs.length - MAX_OUT) + " " + __("更多", "more") + "</text>");
          }
          y += bandH;
        });
        var H = Math.max(60, y);
        return (
          '<div class="rt-section-title">' + __("拓扑（圆环 = 负载）", "Topology (ring = load)") + "</div>" +
          '<div class="rt-svg-wrap"><svg id="rt-topo-svg" class="rt-svg rt-topo-svg" viewBox="0 0 ' + W + " " + H + '" preserveAspectRatio="xMinYMin meet" width="' + W + '" height="' + H + '">' +
          out.join("") +
          "</svg></div>"
        );
      }

      // rtSpark 让一个光点沿一条连线跑一趟。
      //
      // 用 SMIL <animateMotion> 而不是 CSS offset-path / rAF：它由浏览器自己按
      // SVG 用户坐标跑，不需要前端维护动画循环，也没有 path() 在缩放 viewBox 下
      // 的坐标换算问题。跑完把节点摘掉，避免 DOM 累积。
      function rtSpark(pathD, color) {
        if (!pathD) return;
        var svg = document.getElementById("rt-topo-svg");
        if (!svg) return;
        var c = document.createElementNS(RT_SVGNS, "circle");
        c.setAttribute("r", "3.2");
        c.setAttribute("fill", color || "#ffffff");
        c.setAttribute("class", "rt-spark");
        var am = document.createElementNS(RT_SVGNS, "animateMotion");
        am.setAttribute("dur", "0.85s");
        am.setAttribute("path", pathD);
        am.setAttribute("fill", "freeze");
        am.setAttribute("begin", "0s");
        c.appendChild(am);
        svg.appendChild(c);
        setTimeout(() => { if (c.parentNode) c.parentNode.removeChild(c); }, 950);
      }
      // 输入：某条 inputch 来消息了（SSE channel_input）。
      function rtSparkInput(source) {
        rtSpark(_rtEdgeIn[source], "#88c0d0");
      }
      // 输出：某个 agent 往某条 outputch 写了东西（SSE agent_output）。
      function rtSparkOutput(agentID, channel) {
        if (!channel || !agentID) return;
        rtSpark(_rtEdgeOut[agentID + "\u0000" + channel], "#ff7fac");
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
        // 签名里必须带 pipelinePhase：否则 SSE 把阶段推到下一格时，
        // 数据没变 → 早退 → 滑块不动，只能等下一次 /runtime 轮询才追上。
        var sig = JSON.stringify([
          sc, residents, channels, inputs, state.pipelinePhase,
          (state.stageTrail || []).map((t) => t.g + ":" + t.kind + ":" + (t.short || t.label) + "x" + (t.n || 1)).join(","),
        ]);
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
            '<div id="rt-sec-pipe"></div>' +
            '<div id="rt-sec-levels"></div>' +
            '<div id="rt-sec-stack"></div>' +
            '<div id="rt-sec-topo"></div>' +
            "</div>";
        }
        function put(sec, html) {
          var n = document.getElementById("rt-sec-" + sec);
          if (!n || n.__sig === html) return; // 该段没变：一个字节都不动
          n.__sig = html;
          // 变了也只「形变」到新样子：复用未变的子节点，而不是整块重建。
          morph(n, html);
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
        h += rtTile(ready, __("排队", "Ready"), "", ready ? Math.min(100, ready * 20) : 0, ready > 0);
        h += rtTile(pending, __("中断", "Pending"), "", pending ? Math.min(100, pending * 25) : 0, pending > 0, pending > 0);
        h += rtTile(stack + "/" + maxStack, __("栈", "Stack"), "", (stack / (maxStack || 4)) * 100, stack > 0);
        var rFull = residents.filter((r) => r.context_full).length;
        h += rtTile(residents.length, __("子代理", "Subagents"), rFull ? rFull + __("满", " full") : "", residents.length ? Math.min(100, residents.length * 20) : 0, residents.length > 0, rFull > 0);
        h += "</div>";
        // 「累计：入队 … 执行 … 抢占 …」那一整行纯文字被去掉了：它对"现在忙不忙"
        // 没有帮助，却占掉一整行，是总览里最大的一坨文字。
        put("tiles", h);

        // ---- 段 2：阶段管道（滑块，SSE stage 事件驱动）----
        put("pipe", rtPipelineHtml(state.pipelinePhase, state.stageTrail));

        // ---- 段 3：队列（四级中断 + 一条排队）----
        //
        // 设计是「四条中断队列（L1–L4）+ 一条排队队列」共五个，所以必须画五行：
        // 只画四条会让「排队输入」这条线在运行态里凭空消失，而它正是
        // 「不需要及时处理」的那一半输入。排队队列**无级别**，故用不同配色 + 虚线。
        h = '<div class="rt-section-title">' + __("队列", "Queues") + "</div>";
        var maxQ = Math.max(1, ready, q[1] || 0, q[2] || 0, q[3] || 0, q[4] || 0);
        var maxReg = 1;
        var maxPre = 1;
        RT_LEVELS.forEach((L) => {
          maxReg = Math.max(maxReg, byLv[L.lv] || 0);
          maxPre = Math.max(maxPre, preLv[L.lv] || 0);
        });
        // 槽位数按全场最大深度缩放（且至少 5 格）：0 时也有可见形状，不空着。
        var qSlots = Math.max(5, Math.min(16, maxQ));
        // 五个**等大表框**（四级中断 + 一条排队），与阶段管道同一套视觉语言。
        // 此前是五行扁条，四级中断全为 0 时四行几乎全是空白，又占高度又难看。
        h += '<div class="rt-queues">';
        RT_LEVELS.forEach((L) => {
          var depth = q[L.lv] || 0;
          var reg = byLv[L.lv] || 0;
          var pre = preLv[L.lv] || 0;
          h +=
            '<div class="rt-qcell ' + L.cls + (depth ? " rt-active" : "") + '" title="' + escHtml(L.desc) + '">' +
            '<div class="rt-qhead"><b>' + L.name + "</b><span>" + escHtml(L.desc) + "</span></div>" +
            '<div class="rt-qnum">' + depth + "</div>" +
            rtSlots(depth, qSlots, L.cls) +
            '<div class="rt-qmeta">' +
            rtMini(reg, maxReg, __("登记", "registered")) +
            rtMini(pre, maxPre, __("抢占", "preempted")) +
            "</div></div>";
        });
        // 第五条：排队队列（无级别，纯 FIFO）。
        // 它不是优先级而是另一**类别**（排队 vs 中断），所以用虚线框区分。
        h +=
          '<div class="rt-qcell rt-qcell-queued rt-lv-q' + (ready ? " rt-active" : "") + '" title="' + __("排队（无级别，纯 FIFO）", "queued (no priority, FIFO)") + '">' +
          '<div class="rt-qhead"><b>' + __("排队", "queued") + "</b><span>FIFO</span></div>" +
          '<div class="rt-qnum">' + ready + "</div>" +
          rtSlots(ready, qSlots, "rt-lv-q") +
          '<div class="rt-qmeta">' + __("无级别", "no priority") + "</div></div>";
        h += "</div>";
        if (sc.immediate) {
          h += '<div class="rt-frame">' + RT_ICO.bolt + " " + __("立即运行", "immediate") + "：" +
            escHtml(sc.immediate.kind || "") + " #" + sc.immediate.id +
            '<span class="rt-frame-top">L' + (sc.immediate.level || 0) + "</span></div>";
        }
        put("levels", h);

        // ---- 段 4：中断栈 ----
        h = '<div class="rt-section-title">' + __("栈", "Stack") + "</div>";
        // 深度先给一条进度条（"压了几层 / 上限几层"一眼可读），下面再列具体帧。
        h += rtSlider(stack, maxStack, __("深度", "depth"), stack + " / " + maxStack);
        if (frames.length) {
          h += '<div class="rt-stack">';
          frames.forEach((f, i) => {
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

        // ---- 段 5：Agent 拓扑 ----
        //
        // 每个 agent 一条横带：左是归它的 inputch、中间是它自己（圆环 = 负载）、
        // 右是它能写的 outputch，连线即路由。归属、路由、容量、负载全部画进
        // 同一张 SVG，不再单开"通道分配"一节——那本来就是这个拓扑的一部分。
        put("topo", rtAgentTopology(rtAgents(rt)));
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
      // startChatTicker：聊天页可见时 3s 轮询一次增量（与运行态同一节奏）。
      // SSE 仍在（token 级流式靠它），轮询是「数据查询 api」那条腿：
      // 界面最终状态由轮询拉到的数据决定，SSE 只负责让流式看起来即时。
      function startChatTicker() {
        if (state._chatTicker) return;
        state._chatTicker = setInterval(() => {
          var tab = document.querySelector("#tab-chat");
          if (tab && tab.classList.contains("active")) {
            syncChatFromHistory().catch(() => {});
          }
        }, 3000);
      }

      function startRuntimeTicker() {
        if (state._runtimeTicker) return;
        state._runtimeTicker = setInterval(() => {
          var tab = document.querySelector("#tab-overview");
          if (tab && tab.classList.contains("active")) {
            loadRuntime();
          }
        }, 3000);
      }

      // ===== Overview =====
      //
      // 总览页是**静态骨架 + 数据绑定**：DOM 只在首帧建一次，之后所有刷新只改
      // 文本/类名，绝不重建节点。此前 renderAll 每 15s 把整个总览（含运行态面板）
      // innerHTML 重建一遍 —— 那是页面上最刺眼的周期性闪烁的来源。
      //
      // 文字也压到最少：状态用彩色圆点表达，其余只留数字与两字标签，不再堆
      // 「未初始化 / 不可用」这类整句描述。
      var OV_ICONS = {
        activity:
          '<svg viewBox="0 0 24 24"><path d="M22 12h-4l-3 9L9 3l-3 9H2"/></svg>',
        clock:
          '<svg viewBox="0 0 24 24"><circle cx="12" cy="12" r="9"/><path d="M12 7v5l3 2"/></svg>',
        puzzle:
          '<svg viewBox="0 0 24 24"><path d="M4 7h4a2 2 0 1 1 4 0h4v4a2 2 0 1 0 0 4v4h-4a2 2 0 1 0-4 0H4v-4a2 2 0 1 1 0-4z"/></svg>',
        tag:
          '<svg viewBox="0 0 24 24"><path d="M20 12l-8 8-9-9V3h8z"/><circle cx="7.5" cy="7.5" r="1.3"/></svg>',
        brain:
          '<svg viewBox="0 0 24 24"><path d="M9 3a3 3 0 0 0-3 3 3 3 0 0 0-1 5.8V15a3 3 0 0 0 4 2.8V21"/><path d="M15 3a3 3 0 0 1 3 3 3 3 0 0 1 1 5.8V15a3 3 0 0 1-4 2.8V21"/></svg>',
        db:
          '<svg viewBox="0 0 24 24"><ellipse cx="12" cy="6" rx="8" ry="3"/><path d="M4 6v12c0 1.7 3.6 3 8 3s8-1.3 8-3V6"/><path d="M4 12c0 1.7 3.6 3 8 3s8-1.3 8-3"/></svg>',
        file:
          '<svg viewBox="0 0 24 24"><path d="M14 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8z"/><path d="M14 3v5h5"/></svg>',
        cpu:
          '<svg viewBox="0 0 24 24"><rect x="6" y="6" width="12" height="12" rx="2"/><path d="M9 2v3M15 2v3M9 19v3M15 19v3M2 9h3M2 15h3M19 9h3M19 15h3"/></svg>',
      };

      function ovKpi(id, icon, label, dot) {
        return (
          '<div class="ov-kpi"><span class="ov-ico">' +
          icon +
          '</span><span class="ov-val">' +
          (dot ? '<i class="ov-dot" id="ov-' + id + '-dot"></i>' : "") +
          '<b id="ov-' + id + '">—</b></span>' +
          '<span class="ov-sub" id="ov-' + id + '-sub"></span>' +
          '<span class="ov-lab">' +
          label +
          "</span></div>"
        );
      }

      function renderOverview() {
        var host = document.getElementById("tab-overview");
        if (!host) return;
        // 只在首帧建骨架；之后（renderAll 每 15s 一次）只 updateOverview。
        // 一个节点都不重建，所以不会闪。
        if (!document.getElementById("ov-kpis")) {
          host.innerHTML =
            '<div id="rt-panel"></div>' +
            '<div class="card"><div class="ov-kpis" id="ov-kpis">' +
            ovKpi("status", OV_ICONS.activity, __("状态", "Status"), true) +
            ovKpi("uptime", OV_ICONS.clock, __("运行", "Uptime")) +
            ovKpi("plugins", OV_ICONS.puzzle, __("插件", "Plugins")) +
            ovKpi("version", OV_ICONS.tag, __("版本", "Version")) +
            ovKpi("llm", OV_ICONS.brain, "LLM", true) +
            ovKpi("memory", OV_ICONS.db, __("记忆", "Memory")) +
            ovKpi("docs", OV_ICONS.file, __("文档", "Docs")) +
            ovKpi("runtime", OV_ICONS.cpu, __("运行时", "Runtime")) +
            '</div></div>' +
            // 星图搬到主页：作为总览的门面，跟随 agent 活动脉动。
            // 开源许可单独成框：之前在 KPI 卡底部只是一行小链接（.ov-foot），
            // 几乎看不见，也看不出是许可证还是别的什么。
            '<div class="card"><h2>' +
            __("记忆星图", "Memory Star Map") +
            ' <span class="badge" id="sm-home-badge" style="font-size:10px;font-weight:400"></span></h2>' +
            '<div id="sm-container-home" style="height:360px"></div></div>' +
            '<div class="card" id="ov-legal"></div>';
        }
        updateOverview();
        // 星图在主页常驻：容器已建好，首次就拉数据 + 初始化 3D。
        if (document.getElementById("sm-container-home") && !state.starmapInit) {
          renderChatStarmap();
        }
      }

      function ovSet(id, text) {
        var el = document.getElementById(id);
        if (el && el.textContent !== String(text)) el.textContent = String(text);
      }
      function ovDot(id, cls) {
        var el = document.getElementById(id);
        if (el && el.className !== cls) el.className = cls;
      }

      // updateOverview 只写值与类名，从不改结构。
      function updateOverview() {
        var s = state.status || {};
        var k = state.kernel;
        var running = s.status === "running";
        ovSet("ov-status", running ? __("运行中", "running") : s.status || "unknown");
        ovDot("ov-status-dot", "ov-dot " + (running ? "ok" : "warn"));
        ovSet("ov-uptime", state.startedAt ? fmtUptime(Date.now() - state.startedAt) : "-");
        ovSet("ov-plugins", ((k && k.plugins) || []).length || 0);
        // 内核身份：光有版本号分不清是哪个内核、哪次构建 —— 补上内核名与 commit。
        // 之前 36b577b 改图标 KPI 时把 kernel_name 丢了，只剩余 "v1.4.0"。
        var kb = (k && k.build) || {};
        var kv = kb.version || s.version || "";
        ovSet("ov-version", kv ? "v" + kv : "—");
        ovSet(
          "ov-version-sub",
          kv
            ? (kb.kernel_name || "HomeAgent") +
                (kb.commit && kb.commit !== "unknown"
                  ? " · " + String(kb.commit).slice(0, 7)
                  : "")
            : "",
        );
        var llm = (k && k.llm) || {};
        ovDot("ov-llm-dot", "ov-dot " + (llm.available ? "ok" : "bad"));
        ovSet("ov-llm", llm.provider || "—");
        var mem = (k && k.memory) || {};
        ovSet("ov-memory", mem.available ? mem.entity_count + "/" + mem.relation_count : "—");
        var docs = (k && k.documents) || {};
        ovSet("ov-docs", docs.available ? docs.doc_count : "—");
        var rt = (k && k.runtime) || {};
        ovSet(
          "ov-runtime",
          rt.goroutines ? rt.goroutines + (rt.memory_mb ? " · " + rt.memory_mb + "M" : "") : "—",
        );
        // 开源许可卡：协议标识 + 协议全文 + 源码仓库。
        //
        // 为什么单独成框、且三者都给：AGPL-3.0 §13 的义务是「向网络使用者提供
        // 取得 Corresponding Source 的机会」——只给一个仓库链接、不写协议名，
        // 使用者看不出这受什么许可约束，也看不出网络服务场景下还有 §13 的义务。
        // 内容对一次构建是常量，所以填一次就够（__html 比对避免重复重建）。
        var legal = document.getElementById("ov-legal");
        if (legal) {
          var lb = (k && k.build) || {};
          var src = lb.source_url || "";
          var lic = lb.license || "";
          var licURL = lb.license_url || "";
          var rows = "";
          if (lic) {
            rows +=
              '<div class="kv-row"><span class="key">' +
              __("许可协议", "License") +
              '</span><span class="val">' +
              (licURL
                ? '<a href="' + escHtml(licURL) + '" target="_blank" rel="noopener noreferrer">' + escHtml(lic) + "</a>"
                : escHtml(lic)) +
              "</span></div>";
          }
          if (src) {
            rows +=
              '<div class="kv-row"><span class="key">' +
              __("源码仓库", "Source") +
              '</span><span class="val"><a href="' +
              escHtml(src) +
              '" target="_blank" rel="noopener noreferrer">' +
              escHtml(src) +
              "</a></span></div>";
          }
          // 网络条款只在 AGPL 系的许可下才成立，所以按标识判断，不硬写协议名。
          var note =
            lic && lic.toUpperCase().indexOf("AGPL") >= 0
              ? '<p class="ov-legal-note">' +
                __(
                  "网络服务条款（§13）：把修改后的版本作为网络服务对外提供时，必须向使用者提供取得对应源码的途径。",
                  "Network clause (section 13): offering a modified version as a network service requires giving users a way to obtain the Corresponding Source.",
                ) +
                "</p>"
              : "";
          var want = rows
            ? '<h2>' + __("开源许可", "License") + "</h2>" + rows + note
            : "";
          if (legal.__html !== want) {
            legal.__html = want;
            legal.innerHTML = want;
          }
        }
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
          '</span><span class="val" id="know-count">' +
          (k?.knowledge?.item_count ?? "-") +
          "</span></div>" +
          '<details id="know-tree-box" style="margin-top:8px">' +
          '<summary style="cursor:pointer;font-size:12px;opacity:.8">' +
          __("按分类浏览", "Browse by category") +
          '</summary>' +
          '<div id="know-tree" style="margin-top:6px;max-height:200px;overflow:auto;font-size:12px"></div>' +
          '</details>' +
          '<div style="margin-top:8px;display:flex;gap:4px;flex-wrap:wrap;align-items:center">' +
          '<input id="know-query" placeholder="' +
          __("搜索知识", "Search knowledge") +
          '" style="flex:1;min-width:120px">' +
          '<span id="know-cat-scope" style="font-size:11px;opacity:.7"></span>' +
          '<button class="btn btn-primary btn-sm" onclick="searchKnowledgeChat()">' +
          __("搜索", "Search") +
          "</button>" +
          '<button class="btn btn-sm" onclick="clearKnowledgeCategory()">' +
          __("全库", "All") +
          "</button>" +
          '</div><div id="know-result-chat" style="margin-top:8px;max-height:220px;overflow:auto"></div>' +
          '<div style="margin-top:12px;border-top:1px solid var(--border-color);padding-top:8px">' +
          '<input id="know-name" placeholder="' +
          __("知识名称", "Knowledge name") +
          '" style="margin-bottom:4px">' +
          '<textarea id="know-content" placeholder="' +
          __("内容", "Content") +
          '" style="min-height:50px;margin-bottom:4px"></textarea>' +
          '<input type="file" id="know-media" multiple accept="image/*,audio/*,video/*,.md,.txt" style="margin-bottom:6px;font-size:11px">' +
          '<div><button class="btn btn-primary btn-sm" onclick="createKnowledgeChat()">' +
          __("创建", "Create") +
          "</button>" +
          '<span id="know-media-hint" style="margin-left:6px;font-size:11px;opacity:.7"></span>' +
          "</div>" +
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

      // ===== 列表对账（keyed reconcile）=====
      //
      // 为什么必须对账而不是 innerHTML 整块重建：整块重建会把**没变的**那些消息
      // 节点也推倒重来 —— 图片重新解码闪一下、工具卡片的展开态丢失、CSS 过渡从
      // 头播、滚动锚点被重置。这些闪烁几乎无法用"重建后再还原"消除。
      //
      // 做法：新 HTML 先在游离容器里解析成节点，然后按 data-key 逐个对账：
      //   - key 命中且内容一致 → 复用原节点（一个字节都不动）；
      //   - key 命中但内容变了 → 只替换这一个节点；
      //   - key 未命中 → 作为新节点插入；
      // 最后把多出来的旧节点删掉。未变节点因此完全不受影响。
      var _localKeySeq = 0;

      // chatMsgKey 给一条消息一个**稳定**的 key。
      //   - 服务端消息用 seq（单调、落盘、重启不重编号）；
      //   - 本地乐观消息（用户刚发的 / 正在流式的）第一次渲染时分配一个进程内
      //     自增 key 并挂在对象上，后续渲染不变。
      function chatMsgKey(m) {
        if (m && m.seq) return "s" + m.seq;
        if (m && !m._k) m._k = "L" + ++_localKeySeq;
        return (m && m._k) || "L0";
      }

      function commitChatList(container, html) {
        var tpl = document.createElement("div");
        tpl.innerHTML = html;
        var next = Array.prototype.slice.call(tpl.children);
        var existing = {};
        Array.prototype.forEach.call(container.children, (n) => {
          var k = n.getAttribute && n.getAttribute("data-key");
          if (k) existing[k] = n;
        });
        for (var i = 0; i < next.length; i++) {
          var fresh = next[i];
          var key = fresh.getAttribute && fresh.getAttribute("data-key");
          var node = fresh;
          if (key && existing[key]) {
            var old = existing[key];
            // 内容逐字节相同 → 复用旧节点（不触碰它，保存动画/展开/图片状态）；
            // 否则只替换这一个。outerHTML 比较对消息节点足够（含 tool_calls 状态）。
            if (old.outerHTML === fresh.outerHTML) {
              node = old;
            } else {
              old.remove();
            }
          }
          var cur = container.children[i];
          if (cur !== node) container.insertBefore(node, cur || null);
        }
        // 多出来的旧节点（本轮新列表里没有的）从尾部清掉
        while (container.children.length > next.length) {
          container.removeChild(container.lastElementChild);
        }
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
            () => {
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
            () => {
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
            .map((m) => {
              var c = m.content || "";
              return (
                (m.role || "") +
                ":" +
                c.length +
                ":" +
                c.slice(-40) +
                ":" +
                (m.tool_calls || [])
                  .map((t) => (t.tool || t.name || "") + "/" + (t.status || ""))
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
            (lastM.tool_calls || []).forEach((tc) => {
              if (!tc.result && tc.status !== "denied") {
                var nm = tc.tool || tc.name || "";
                if (newPending.indexOf(nm) === -1) newPending.push(nm);
              }
            });
          }
        }
        var newlyDone = prevPending.filter((n) => newPending.indexOf(n) === -1);
        msgsEl._lastPending = newPending;
        var streamingLast = !!(
          state.chatLoading &&
          lastM &&
          lastM.role === "assistant" &&
          !lastM._final
        );
        function pillHtml() {
          var s = "";
          newPending.forEach((nm) => {
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
          msgs.forEach((m, i) => {
            var _k = chatMsgKey(m);
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
                '" data-key="' + _k + '"><div class="msg-bubble"><div class="att-wrap">' +
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
              m.tool_calls.forEach((tc) => {
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
                  '<span class="tc-caret">' + RT_ICO.caret + "</span></div>" +
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
                '<div class="msg msg-system" data-key="' + _k + '"><div class="msg-bubble">' +
                (c || "") +
                "</div></div>";
            } else if (isChan) {
              html +=
                '<div class="msg msg-channel" data-key="' + _k + '">' +
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
                '" data-key="' + _k + '">' +
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
            '<div class="msg msg-assistant" data-key="pending"><div class="msg-avatar">' +
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
        commitChatList(msgsEl, html);
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
            _rerenderTimer = setTimeout(() => {
              _rerenderTimer = null;
              renderChatStreamChunk();
            }, 90);
            return;
          }
        }
        // 非流式（完成/工具/历史变化）：全量重渲（含防抖合并）
        if (_rerenderTimer) clearTimeout(_rerenderTimer);
        _rerenderTimer = setTimeout(() => {
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
          '<span class="rc-chev">' + RT_ICO.caret + "</span></div>" +
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

      // renderChatStarmap：把星图挂到**当前活跃的容器**上。
      // 容器优先级：独立星图页签 > 总览页小图 > 聊天面板。
      // 同一套 three.js renderer 在容器间搬运 canvas，不重建场景。
      function renderChatStarmap() {
        var cont = starmapActiveContainer();
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
        // 容器可以是独立星图页签 / 总览页 / 聊天面板 —— 同一套 renderer
        // 会在多个容器间搬运 canvas（appendChild 即可）。
        var cont = starmapActiveContainer();
        if (!cont) return;
        var rect = cont.getBoundingClientRect();
        var w = Math.max(rect.width || 300, 100);
        var h = Math.max(rect.height || 250, 100);
        if (starmapRen) {
          starmapRen.setSize(w, h);
          // 换父节点前先把旧角标从旧容器里拨掉，否则它会残留在旧位置。
          if (starmapLabelEl && starmapLabelEl.parentElement === cont) {
            starmapLabelEl.style.display = "none";
          }
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
        // 共享几何（低规格）：所有节点/光晕球复用这两份。
        starmapGeo = new THREE.SphereGeometry(0.5, 8, 6);
        starmapGlowGeo = new THREE.SphereGeometry(1.0, 8, 6);
        buildChatStarmapGraph();
        starmapRen.domElement.addEventListener("mousemove", onStarmapMove);
        starmapRen.domElement.addEventListener("click", onStarmapClick);
        window.addEventListener("resize", onStarmapResize);
        if (starmapRaf) cancelAnimationFrame(starmapRaf);
        starmapAnimate();
        // 活动数据源：/runtime 3s + /memory/graph/pulse 10s。
        // 只在星图真正初始化后启动，避免在隐藏页签空跑。
        starmapStartActivity();
      }

      // starmapStartActivity 启动两路活动轮询（幂等）。
      function starmapStartActivity() {
        if (state.starmapPulseTimer) return;
        starmapPullActivity();
        starmapPullPulse();
        state.starmapPulseTimer = setInterval(() => {
          starmapPullActivity();
        }, 3000);
        var pulseTimer = setInterval(() => {
          starmapPullPulse();
        }, 10000);
        // 两个 id 合并到一个字段会导致 setInterval 被覆盖，这里分开记。
        state.starmapActivityTimer = pulseTimer;
      }

      function buildChatStarmapGraph() {
        starmapNodeMeshes.forEach((m) => {
          starmapScene.remove(m);
        });
        starmapEdgeLines.forEach((l) => {
          starmapScene.remove(l);
        });
        starmapNodeMeshes = [];
        starmapEdgeLines = [];
        if (starmapNodes.length === 0) return;
        // Calculate node degrees for leaf node detection
        var nodeDegs = {};
        starmapNodes.forEach((n) => {
          nodeDegs[n.id] = 0;
        });
        starmapEdges.forEach((e) => {
          nodeDegs[e.source_id] = (nodeDegs[e.source_id] || 0) + 1;
          nodeDegs[e.target_id] = (nodeDegs[e.target_id] || 0) + 1;
        });
        var nodeMap = {};
        starmapNodes.forEach((n) => {
          nodeMap[n.id] = n;
        });
        var sorted = starmapNodes.slice().sort((a, b) => (b.mention_count || 0) - (a.mention_count || 0));
        var mc = sorted.map((n) => n.mention_count || 0);
        var maxMc = Math.max(...mc, 1),
          minMc = Math.min(...mc, 0),
          rng = maxMc - minMc || 1;
        // Layout positions
        var pos = {};
        var baseR = 15,
          maxR = 80;
        var total = sorted.length;
        var acc = 0;
        sorted.forEach((n, i) => {
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
        sorted.forEach((n) => {
          var deg = nodeDegs[n.id] || 0;
          if (deg !== 1) return;
          var edge = starmapEdges.find((e) => e.source_id === n.id || e.target_id === n.id);
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
          starmapEdges.forEach((e) => {
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
          ids.forEach((id) => {
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
        //
        // ★ 低规格渲染（1151 节点实测后定的方案）：保留**全部**节点，
        // 但把每节点开销从「一个独立球 + 一个独立球光晕 + 一张 256x64
        // 文字贴图」降到「共享几何 + 共享材质 + 无常驻文字」。
        //
        // 改之前的实际开销（生产实例 1151 节点）：
        //   - SphereGeometry(16,12) 主球 + 同样规格的光晕球 = 2302 个独立
        //     BufferGeometry，共约 88 万三角形
        //   - 每节点一张 256x64 CanvasTexture = 1151 张 <canvas> +
        //     1151 个纹理，仅文字就吃约 72MB 显存
        // 这两样在「星图只是个展示」的前提下纯属浪费：星图全图远看根本
        // 读不清标签（256x64 贴在半径 0.5~2.5 的球上，本来就糊）。
        //
        // 现在：共享一份 SphereGeometry(8,6)（约 84 三角形/节点），
        // 颜色靠每 mesh 的 material.color（材质本身仍每节点一份，
        // 因为 MeshPhongMaterial 要独立发光强度才能做活动脉冲）。
        // 标签改为「hover / 选中时才在容器角上显示 HTML 文本」——
        // 文字清晰度反而比 3D 贴图好，且零显存。
        starmapNodes.forEach((n) => {
          var p = pos[n.id];
          if (!p) return;
          var mn = n.mention_count || 0,
            mnr = rng > 0 ? (mn - minMc) / rng : 0;
          var rad = 0.5 + mnr * 2.0;
          // 服务端 type 是首字母大写（"Concept" / "Person" …），
          // 原 smTypeColors 的键全是小写，永远匹配不上 ⇒ 全图单色 0xcccccc。
          // 这里统一小写归一化，并补上服务端实际会产出的类型。
          var col = smTypeColors[String(n.type || "").toLowerCase()] || 0xcccccc;
          var ei = 0.3 + mnr * 0.7;
          var mat = new THREE.MeshPhongMaterial({
            color: col,
            emissive: col,
            emissiveIntensity: ei,
            shininess: 30,
          });
          var mesh = new THREE.Mesh(starmapGeo, mat);
          mesh.position.set(p.x, p.y, p.z);
          mesh.scale.setScalar(rad / 0.5); // 共享几何半径 0.5，按需缩放
          mesh.userData.nodeData = n;
          mesh.userData.nodeId = n.id;
          mesh.userData.baseEmissive = ei;
          mesh.userData.baseScale = rad / 0.5;
          mesh.userData.baseGlow = 0.12 + mnr * 0.08;
          // Glow sphere：共享几何 + 各自材质（发光强度要独立才能做脉冲）
          var gm = new THREE.MeshBasicMaterial({
            color: col,
            transparent: true,
            opacity: mesh.userData.baseGlow,
            side: THREE.BackSide,
            blending: THREE.AdditiveBlending,
          });
          var gs = new THREE.Mesh(starmapGlowGeo, gm);
          gs.scale.setScalar(rad * 1.2 + mnr * 0.5);
          mesh.add(gs);
          mesh.userData.glowSphere = gs;
          // 名字不再烘成贴图；hover 时由 onStarmapMove 写进角标。
          starmapScene.add(mesh);
          starmapNodeMeshes.push(mesh);
        });
        // Create edges
        //
        // 866 条边原本每条一个 BufferGeometry + Line + LineBasicMaterial
        // （866 个 draw call）。改为**按关系类型分组**的少量 LineSegments：
        // 同类型边合并成一个几何体，draw call 从 866 降到「关系类型数」
        // （生产实例实测 6 种左右）。
        //
        // 副作用：单条边不再能单独点选。星图此前也没有点选边的交互
        // （onStarmapClick 只处理节点），所以这是纯粹的成本削减。
        var edgeByColor = {};
        starmapEdges.forEach((e) => {
          var a = pos[e.source_id],
            b = pos[e.target_id];
          if (!a || !b) return;
          var col =
            smEdgeColors[e.relation_type] || smEdgeColors[e.type] || 0x444466;
          var k = String(col);
          if (!edgeByColor[k]) edgeByColor[k] = { col: col, pts: [] };
          edgeByColor[k].pts.push(a.x, a.y, a.z, b.x, b.y, b.z);
        });
        Object.keys(edgeByColor).forEach((k) => {
          var g = edgeByColor[k];
          if (!g.pts.length) return;
          var geo = new THREE.BufferGeometry();
          geo.setAttribute(
            "position",
            new THREE.Float32BufferAttribute(g.pts, 3),
          );
          var mat = new THREE.LineBasicMaterial({
            color: g.col,
            transparent: true,
            opacity: 0.4,
          });
          var seg = new THREE.LineSegments(geo, mat);
          starmapScene.add(seg);
          starmapEdgeLines.push(seg);
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
        state._turnWatchdog = setTimeout(() => {
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
        panel.addEventListener("dragover", (e) => {
          e.preventDefault();
          panel.style.outline = "2px dashed var(--accent, #4a90d9)";
        });
        panel.addEventListener("dragleave", () => {
          panel.style.outline = "";
        });
        panel.addEventListener("drop", (e) => {
          e.preventDefault();
          panel.style.outline = "";
          if (e.dataTransfer.files && e.dataTransfer.files.length) {
            sendChatFile(e.dataTransfer.files[0]);
          }
        });
        // 粘贴截图/复制的文件直接发送
        document.addEventListener("paste", (e) => {
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
          var ackTimer = setTimeout(() => {
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
                .map((e) => escHtml(e.name || e.id || ""))
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

      // 知识库面板：搜索 / 创建 / 删除 / 刷新计数。
      //
      // 此前三处问题：搜索把裸 JSON 直接 stringify 丢进 <pre>（用户看到一坨
      // 机器码）；创建后不刷新计数（1644 行读的是 state.kernel 快照，创建
      // 完仍是旧值）；没有任何删除入口，也没有媒体上传。
      function knowEsc(v) {
        return String(v == null ? "" : v)
          .replace(/&/g, "&amp;")
          .replace(/</g, "&lt;")
          .replace(/>/g, "&gt;");
      }

      async function refreshKnowledgeCount() {
        try {
          var d = await api("/knowledge");
          var el = document.getElementById("know-count");
          if (el && d && d.names) el.textContent = d.names.length;
          // 稠密路（多模态）状态提示
          var hint = document.getElementById("know-media-hint");
          if (hint && d && d.dense) {
            if (d.dense.enabled) {
              hint.textContent =
                __("多模态已就绪 ", "Multimodal ready ") +
                (d.dense.ready || 0) +
                "/" +
                ((d.dense.ready || 0) + (d.dense.stale || 0));
            } else {
              hint.textContent = __(
                "未接入多模态：图片只记录不参与召回",
                "Multimodal off: images recorded but not searchable",
              );
            }
          }
        } catch (e) {
          /* 计数刷新失败不该打断用户操作 */
        }
      }

      function renderKnowledgeResults(views, q) {
        if (!views || !views.length) {
          return (
            '<p style="opacity:.7;font-size:12px">' +
            __("未找到相关知识", "No matching knowledge") +
            "</p>"
          );
        }
        var html = "";
        views.forEach((v) => {
          html += '<div class="know-item" style="padding:6px 0;border-bottom:1px solid var(--border-color)">';
          html += '<div style="display:flex;gap:6px;align-items:baseline">';
          html += '<strong style="font-size:12px;flex:1;word-break:break-all">' + knowEsc(v.name) + "</strong>";
          if (v.size) {
            html += '<span style="font-size:10px;opacity:.6">' + v.size + " B</span>";
          }
          html +=
            '<button class="btn btn-sm" style="font-size:10px" onclick="deleteKnowledge(' +
            JSON.stringify(v.name).replace(/"/g, "&quot;") +
            ')">' +
            __("删除", "Delete") +
            "</button>";
          html += "</div>";
          if (v.media && v.media.length) {
            html += '<div style="font-size:10px;opacity:.7;margin-top:2px">';
            v.media.forEach((m) => {
              var kind = m.kind || "file";
              html +=
                '<span style="margin-right:6px">[' + knowEsc(kind) + "] " + knowEsc(m.digest.slice(0, 8)) + "…</span>";
            });
            html += "</div>";
          }
          if (v.preview) {
            html +=
              '<div style="font-size:11px;opacity:.8;margin-top:3px;white-space:pre-wrap;max-height:80px;overflow:auto">' +
              knowEsc(v.preview) +
              "</div>";
          }
          html += "</div>";
        });
        return html;
      }

      // 知识库分类树浏览。
      //
      // 数据来自 /knowledge/tree（只读树接口）。当前分类存 state，
      // 搜索时自动带上 —— 这样"先定位分类再检索"这个更准的用法在 UI 上
      // 是一步的事，而不是要求用户手打分类名。
      var _knowCat = ""; // 当前分类的完整 path；"" = 全库

      function renderKnowTree(node, depth) {
        var html = "";
        // 本节点的条目
        (node.items || []).forEach((it) => {
          html +=
            '<div style="padding-left:' +
            (depth * 12 + 4) +
            'px;padding-top:1px;padding-bottom:1px">' +
            '<span style="opacity:.7">▸</span> ' +
            '<span style="word-break:break-all">' +
            knowEsc(it.name) +
            "</span>" +
            (it.size ? ' <span style="opacity:.5;font-size:10px">' + it.size + "B</span>" : "") +
            (it.media && it.media.length
              ? ' <span style="opacity:.6;font-size:10px">[' + it.media.length + __(" 媒体", " media") + "]</span>"
              : "") +
            "</div>";
        });
        // 子分类
        (node.children || []).forEach((c) => {
          var isCur = _knowCat === c.path;
          var mark = isCur ? "● " : "";
          html +=
            '<div style="padding-left:' +
            (depth * 12) +
            'px"><span onclick="selectKnowledgeCategory(\'' +
            knowEsc(c.path).replace(/'/g, "&#39;") +
            '\')" style="cursor:pointer;user-select:none">' +
            mark +
            knowEsc(c.name) +
            ' <span style="opacity:.55;font-size:10px">' +
            c.item_count +
            "/" +
            c.total_count +
            "</span></span></div>";
          html += renderKnowTree(c, depth + 1);
        });
        return html;
      }

      async function loadKnowledgeTree() {
        var box = document.getElementById("know-tree");
        if (!box) return;
        box.innerHTML = '<div class="loading"></div>';
        try {
          var d = await api("/knowledge/tree?items=1");
          if (!d || !d.tree) {
            box.innerHTML =
              '<p style="opacity:.6;font-size:11px">' + __("暂无分类", "No categories yet") + "</p>";
            return;
          }
          box.innerHTML = renderKnowTree(d.tree, 0);
        } catch (e) {
          box.innerHTML =
            '<p style="color:#fca5a5;font-size:11px">' + __("加载分类失败: ", "Load categories failed: ") + knowEsc(e.message) + "</p>";
        }
      }

      function selectKnowledgeCategory(path) {
        _knowCat = path || "";
        var lbl = document.getElementById("know-cat-scope");
        if (lbl) {
          lbl.textContent = _knowCat
            ? __("范围：", "scope: ") + _knowCat
            : __("范围：全库", "scope: all");
        }
        loadKnowledgeTree();
        // 立刻按新范围搜一次，省一次点击
        var q = document.getElementById("know-query")?.value;
        if (q) searchKnowledgeChat();
      }

      function clearKnowledgeCategory() {
        selectKnowledgeCategory("");
      }

      async function searchKnowledgeChat() {
        var q = document.getElementById("know-query")?.value;
        var cat = _knowCat || "";
        var r = document.getElementById("know-result-chat");
        if (!r) return;
        if (!q) {
          r.innerHTML =
            '<p style="opacity:.7;font-size:12px">' +
            __("请输入查询关键词", "Enter a keyword") +
            "</p>";
          return;
        }
        r.innerHTML = '<div class="loading"></div>';
        try {
          var url = "/knowledge?q=" + encodeURIComponent(q);
          if (cat) url += "&category=" + encodeURIComponent(cat);
          var data = await api(url);
          r.innerHTML = renderKnowledgeResults(data && data.results, q);
        } catch (e) {
          r.innerHTML =
            '<p style="color:#fca5a5">' + __("搜索失败: ", "Search failed: ") + knowEsc(e.message) + "</p>";
        }
      }

      async function deleteKnowledge(name) {
        if (
          !confirm(
            __("确定删除知识「", "Delete knowledge \"") + name + __("」？此操作不可撤销。", "\"? This cannot be undone."),
          )
        ) {
          return;
        }
        try {
          await api("/knowledge?name=" + encodeURIComponent(name), { method: "DELETE" });
          toast(__("知识「", 'Knowledge "') + name + __("」已删除", '" deleted'));
          refreshKnowledgeCount();
          searchKnowledgeChat();
        } catch (e) {
          toast(__("删除失败: ", "Delete failed: ") + e.message, true);
        }
      }

      async function createKnowledgeChat() {
        var name = document.getElementById("know-name")?.value;
        var content = document.getElementById("know-content")?.value;
        var fileInput = document.getElementById("know-media");
        var files = fileInput && fileInput.files ? fileInput.files : null;
        if (!name) {
          toast(__("名称不能为空", "Name is required"), true);
          return;
        }
        if ((!content || !content.trim()) && (!files || !files.length)) {
          toast(
            __("内容与媒体至少要有一项", "Content or media is required"),
            true,
          );
          return;
        }
        try {
          var r;
          if (files && files.length) {
            // 有文件走 multipart：服务端按**探测到的真实类型**分流，
            // 图片/音视频入媒体库并按 digest 挂到条目上，文本存正文。
            var fd = new FormData();
            fd.append("name", name);
            if (content) fd.append("content", content);
            for (var i = 0; i < files.length; i++) fd.append("file", files[i]);
            r = await api("/knowledge", { method: "POST", body: fd });
          } else {
            r = await api("/knowledge", {
              method: "POST",
              body: JSON.stringify({ name: name, content: content }),
            });
          }
          var msg = __("知识「", 'Knowledge "') + name + __("」已创建", '" created');
          if (r && r.media) msg += __("，含 ", " with ") + r.media + __(" 个媒体", " media item(s)");
          if (r && r.rejected && r.rejected.length) {
            msg += __("；", "; ") + r.rejected.length + __(" 项被跳过", " skipped");
          }
          toast(msg);
          document.getElementById("know-name").value = "";
          document.getElementById("know-content").value = "";
          if (fileInput) fileInput.value = "";
          refreshKnowledgeCount();
        } catch (e) {
          toast(__("创建失败: ", "Create failed: ") + e.message, true);
        }
      }

      function switchChatPanel(tab, el) {
        // 切到知识面板时拉实时计数：面板里的数字来自 state.kernel 快照，
        // 而知识条目会经工具/上传增删，快照不会自己变（实测创建后仍显示 "-"）。
        if (tab === "knowledge") {
          refreshKnowledgeCount();
          var lbl = document.getElementById("know-cat-scope");
          if (lbl && !lbl.textContent) {
            lbl.textContent = _knowCat ? __("范围：", "scope: ") + _knowCat : __("范围：全库", "scope: all");
          }
          loadKnowledgeTree();
        }
        var panels = {
          chat: document.getElementById("chat-panel-chat"),
          starmap: document.getElementById("chat-panel-starmap"),
          terminal: document.getElementById("chat-panel-terminal"),
          cmd: document.getElementById("chat-panel-cmd"),
          memory: document.getElementById("chat-panel-memory"),
          context: document.getElementById("chat-panel-context"),
          knowledge: document.getElementById("chat-panel-knowledge"),
        };
        Object.keys(panels).forEach((k) => {
          var p = panels[k];
          if (p) p.classList.toggle("active", k === tab);
        });
        if (el) {
          var parent = el.parentElement;
          if (parent) {
            Array.from(parent.children).forEach((ch) => {
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
            state.chatLastSeq = historyLastSeq(data);
          }
        } catch (e) {}
      }

      // historyLastSeq 从一次 /chat/history 响应里取出「已见到的最大 seq」：
      // 优先用服务端给的 last_seq，缺了就取消息里的最大值。
      function historyLastSeq(data) {
        if (!data) return 0;
        if (typeof data.last_seq === "number") return data.last_seq;
        var mx = 0;
        (data.messages || []).forEach((m) => {
          if (m && m.seq > mx) mx = m.seq;
        });
        return mx;
      }

      // applyServerMessages 把服务端消息并进 state.messages，按 seq 对账：
      //   - 同 seq 已存在 → 原地替换（工具调用/最终文本是原地更新）；
      //   - 不存在 → 追加（若末尾是无 seq 的乐观消息且 role+content 一致，则替换它，
      //     避免"自己刚发的那条"重复成两条）。
      // tailOnly=true 时只做原地更新与"比本地新才追加"，不把尾探测当成新消息。
      // @returns {boolean} 是否真的改动了 state.messages
      function applyServerMessages(list, tailOnly) {
        if (!list || !list.length) return false;
        var msgs = state.messages;
        var changed = false;
        function carry(prev, sm) {
          if (prev && prev._final) sm._final = true;
          if (prev && prev._grow) sm._grow = true;
          return sm;
        }
        list.forEach((sm) => {
          if (!sm) return;
          var seq = sm.seq;
          var found = -1;
          for (var j = msgs.length - 1; j >= 0 && j >= msgs.length - 12; j--) {
            if (seq && msgs[j] && msgs[j].seq === seq) {
              found = j;
              break;
            }
          }
          if (found >= 0) {
            if (JSON.stringify(msgs[found]) !== JSON.stringify(sm)) {
              msgs[found] = carry(msgs[found], sm);
              changed = true;
            }
            return;
          }
          if (tailOnly) {
            var lastS = msgs.length ? msgs[msgs.length - 1].seq : 0;
            if (seq && (!lastS || seq > lastS)) {
              msgs.push(sm);
              changed = true;
            }
            return;
          }
          if (msgs.length) {
            var last = msgs[msgs.length - 1];
            if (
              !last.seq &&
              (last.role || "") === (sm.role || "") &&
              (last.content || "") === (sm.content || "")
            ) {
              msgs[msgs.length - 1] = carry(last, sm);
              changed = true;
              return;
            }
          }
          msgs.push(sm);
          changed = true;
        });
        list.forEach((sm) => {
          if (sm && sm.seq > (state.chatLastSeq || 0)) state.chatLastSeq = sm.seq;
        });
        if (changed) rerenderChat();
        return changed;
      }

      // pollChatIncremental 轮询「自上次以来新增了什么」。
      //
      // 这就是「暴露数据查询 api，前端轮询后 patch 视图」那条路：after=游标
      // 只拿增量，再单独探一次尾部做原地更新（工具调用/最终文本是原地改的，
      // 不会产生新 seq，只靠 after 拿不到）。视图更新走 commitChatList 的
      // keyed 对账，未变消息节点一个字节都不动 —— 闪烁由此消失。
      function pollChatIncremental() {
        var after = state.chatLastSeq || 0;
        if (!after) {
          // 还没建立游标（首次 / 本地为空）：退回一次性全量，交给已有一致性逻辑
          return api("/chat/history?limit=" + CHAT_PAGE_SIZE).then((data) => {
            state.chatLastSeq = historyLastSeq(data);
            return mergeChatFromHistory(data);
          });
        }
        return api("/chat/history?after=" + after)
          .then((data) => {
            if (!data) return;
            state.chatLastSeq = historyLastSeq(data) || after;
            applyServerMessages(data.messages || [], false);
            return api("/chat/history?limit=1");
          })
          .then((tail) => {
            if (tail && tail.messages && tail.messages.length) {
              applyServerMessages(tail.messages.slice(-1), true);
            }
          });
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
      // syncChatFromHistory 保留旧名（多处调用点）：内部改走游标增量轮询。
      function syncChatFromHistory() {
        if (_syncingChat) return Promise.resolve();
        _syncingChat = true;
        return pollChatIncremental()
          .catch(() => {})
          .then(() => {
            _syncingChat = false;
          });
      }

      // mergeChatFromHistory 把服务端的一页历史并进本地：只追加新消息，不重建已有节点。
      // 关键约束：**绝不**用更短的服务端页替换更长的本地列表（那会让用户翻上来的旧页
      // 凭空消失、视口跳回顶部）。
      function mergeChatFromHistory(data) {
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
            newMsgs.forEach((m) => {
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
          (state.pendingTools || []).forEach((nm) => {
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
        list.forEach((t, i) => {
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
        var running = (state.terminals || []).filter((t) => t.running);
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
        running.forEach((t) => {
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
        es.addEventListener("agent_output", (e) => {
          try {
            var ev = JSON.parse(e.data);
            var p = ev.payload || {};
            // 光点：agent → 输出通道。总览页拓扑靠它做"消息出去了"的动画；
            // 其它页没有该 SVG，rtSparkOutput 查不到元素就是 no-op。
            if (p.channel) rtSparkOutput(ev.source || "", p.channel);
            // 星图跟随：agent 输出是一次完整活动的收尾，给一记强脉冲。
            starmapPulse("output", p.channel || null);
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
        es.addEventListener("content_delta", (e) => {
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
        es.addEventListener("terminal_output", (e) => {
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
        es.addEventListener("reasoning", (e) => {
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
        es.addEventListener("reasoning_delta", (e) => {
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
        es.addEventListener("tool_call", (e) => {
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
            // 星图跟随：工具调用是最可靠的活动信号。工具名（如
            // knowledge_list / qq_send）会拿去匹配相关实体节点并点亮。
            starmapPulse("tool", p.tool);
            rerenderChat();
          } catch (ex) {
            console.error("[SSE] tool_call error", ex);
          }
        });
        // 注：后端不发布 tool_result 类型事件（工具结果随 EventToolCall 一次发出），无此监听器。
        es.addEventListener("stage", (e) => {
          try {
            var ev = JSON.parse(e.data);
            var p = ev.payload || {};
            var phase = p.phase || "";
            var tool = p.tool || "";
            if (p.channel === "_consolidation_") return;
            console.log("[SSE] stage event", phase, tool);
            // 驱动总览页的"阶段管道"滑块：滑到当前阶段；一轮跑完（或 2.5s 无新
            // 事件）自动回到空闲，避免留下一个永远停在 after_output 的假状态。
            // 阶段轨迹：本轮真实发生过什么，交给阶段管道画成 chip 序列。
            // 工具调用会反复出现（多轮 toolcall），output_* 单独配色。
            if (phase === "on_input") {
              state.stageTrail = [];
              rtTrailPush(0, "stage", __("输入", "input"), __("输入", "input"));
            } else if (phase === "pre_action") {
              rtTrailPush(1, "stage", __("组装上下文并思考", "assemble context and think"), __("思考", "think"));
            } else if (phase === "before_toolcall" && tool) {
              var isOut = tool.indexOf("output_") === 0;
              rtTrailPush(2, isOut ? "output" : "tool", tool, rtShortTool(tool));
              state.toolFlash = true;
            } else if (phase === "before_output") {
              rtTrailPush(3, "stage", __("生成回复", "generate reply"), __("生成", "gen"));
            } else if (phase === "after_output") {
              rtTrailPush(4, "stage", __("本轮完成", "turn complete"), __("完成", "done"));
            }
            // 星图跟随：阶段推进也作为活动信号（工具名优先匹配节点）。
            starmapPulse("stage", tool || phase);
            state.pipelinePhase = phase;
            if (state.pipelineTimer) clearTimeout(state.pipelineTimer);
            state.pipelineTimer = setTimeout(() => {
              state.pipelinePhase = "";
              if (document.getElementById("rt-sec-pipe")) renderRuntime();
            }, 2500);
            if (document.getElementById("rt-sec-pipe")) renderRuntime();
            // 渲染后再放动画：morph 已经把 chip 文本更新成最新那条。
            if (state.toolFlash) {
              state.toolFlash = false;
              rtFlashLatestTool();
            }
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
        // channel_input：内核在输入进来时另发的一条轻量事件（只带通道名与 agent
        // id，不带正文）。总览页拓扑靠它画"光点进入 agent"。
        es.addEventListener("channel_input", (e) => {
          try {
            var d = JSON.parse(e.data);
            var src = d.source || "";
            if (src) rtSparkInput(src);
          } catch (ex) {
            console.error("[SSE] channel_input error", ex);
          }
        });
        es.onopen = () => {
          console.log("[SSE] connection opened");
        };
        es.onerror = (e) => {
          console.error("[SSE] error", e);
          // 1) 立即 close 阻止浏览器原生自动重连与手动 setTimeout(connectSSE) 双连接竞态
          try { state.eventSource && state.eventSource.close(); state.eventSource = null; } catch(ex){}
          // 2) 连接错误期间可能丢失事件，增量补拉历史（无闪烁）
          syncChatFromHistory().catch(()=> {});
          // 3) 2s 后手动重连（比原 5s 更快恢复）
          setTimeout(connectSSE, 2000);
        };
        // sync_required：Server 因 Last-Event-ID 不在 ring（delta ID / 已到 tip）无法重放，
        // 通知前端增量补拉历史——避免前端空等后续聚合事件导致「消息同步不及时」。
        es.addEventListener("sync_required", (e) => {
          console.log("[SSE] sync_required received, incremental sync");
          syncChatFromHistory().catch(()=> {});
        });
        // Periodically refresh sidebar data
        if (state._sidebarRefresh) clearInterval(state._sidebarRefresh);
        state._sidebarRefresh = setInterval(async () => {
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
      // 「服务入口」：被反代的插件服务，点一下直接打开。
      //
      // 为什么单独一张卡而不是塞进每个插件的详情：入口是**跨插件**的（同一个
      // webui 端口、不同子域），用户的心智是「我要打开某个服务」，不是
      // 「我要进某个插件的管理页」。
      //
      // 链接带 ?__token=<api_key>：子域与门户不同源，浏览器不会带上会话 cookie；
      // 不带 token 会 401（这是刻意的受保护默认）。页面已登录，此处复用同一把 key。
      function renderProxyServicesCard() {
        var svcs = state.proxyServices || [];
        var base = state.proxyBaseDomain || "localhost";
        var html =
          '<div class="card"><h2>' +
          __("服务入口", "Service Entry Points") +
          "</h2>";
        if (!svcs.length) {
          return (
            html +
            '<p style="color:var(--text-muted);font-size:13px">' +
            __(
              "暂无被反代的插件服务。插件在 plugin.json 的 proxies 字段里声明后会自动出现在这里。",
              "No proxied plugin services yet. Declare them in plugin.json's proxies field.",
            ) +
            "</p></div>"
          );
        }
        var token = state.settings?.["plugin.webui.api_key"] || "";
        html +=
          '<div style="font-size:12px;color:var(--text-muted);margin-bottom:10px">' +
          __(
            "插件服务经 HomeAgent 同一端口反代。子域形态需要 DNS 能解析 ",
            "Proxied through the same port. The subdomain form needs DNS for ",
          ) +
          "<b>" +
          escHtml("*." + base) +
          "</b>" +
          __(
            "；路径形态无 DNS 依赖，穿透场景下更可靠（点「打开」优先用它）。",
            "; the path form has no DNS dependency and is more reliable behind a tunnel (Open prefers it).",
          ) +
          "</div>";
        html += '<div class="svc-list">';
        svcs.forEach((s) => {
          var url = s.url || "";
          if (url && token) url += "?__token=" + encodeURIComponent(token);
          var dot = s.ok ? "var(--ok, #22c55e)" : "var(--danger, #d1383d)";
          html +=
            '<div class="svc-row">' +
            '<span class="svc-dot" style="background:' + dot + '"></span>' +
            '<span class="svc-name">' + escHtml(s.plugin_name || s.plugin) + "</span>" +
            '<span class="svc-sub">' + escHtml(s.name) + "</span>" +
            (s.websocket
              ? '<span class="svc-tag">WS</span>'
              : "") +
            (s.auth === "none"
              ? '<span class="svc-tag svc-tag-warn">' +
                __("未保护", "unprotected") +
                "</span>"
              : "") +
            '<span class="svc-path">' + escHtml(s.host + "." + base) + "</span>";
          // 「打开」优先用**路径形态**（url_portal）：
          //   - 它没有 DNS 依赖，单端口穿透 / 子域无证书时都能用；
          //   - 子域形态要求 DNS 能解析 <host>.<基域名>，而这些域名
          //     在外部常常不可达（实测：外层只放行一个 Host，三级子域
          //     因通配证书不匹配而握手失败）。
          // 保留子域链接作为次选（局域网内直连时它更直观）。
          var primary = s.url_portal || url;
          if (s.ok && primary) {
            html +=
              '<a class="btn btn-primary btn-sm" style="margin-left:auto" target="_blank" rel="noopener" href="' +
              escHtml(primary) +
              '">' +
              __("打开", "Open") +
              "</a>";
            if (url && s.url_portal && url !== s.url_portal) {
              html +=
                '<a class="btn btn-ghost btn-sm" target="_blank" rel="noopener" title="' +
                escHtml(url) +
                '" href="' +
                escHtml(url) +
                '">' +
                __("子域", "subdomain") +
                "</a>";
            }
          } else {
            html +=
              '<span class="svc-err" title="' +
              escHtml(s.error || "") +
              '">' +
              escHtml(s.error || __("不可用", "unavailable")) +
              "</span>";
          }
          html += "</div>";
        });
        html += "</div></div>";
        return html;
      }

      async function loadProxyServices() {
        try {
          var d = await api("/proxy/services");
          state.proxyServices = (d && d.services) || [];
          state.proxyBaseDomain = (d && d.base_domain) || "localhost";
        } catch (e) {
          state.proxyServices = [];
        }
      }

      function renderPlugins() {
        var k = state.kernel;
        var plugins = k?.plugins || [];
        var tools = k?.tools || [];
        var installed = state.installedPlugins || [];
        var html = renderProxyServicesCard();
        html +=
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
        (state.disabledPlugins || []).forEach((d) => {
          disabledNames[d.name] = d;
        });
        var installedNames = (state.installedPlugins || []).map((p) => p.name);
        var allPluginNames = {};
        plugins.forEach((p) => {
          allPluginNames[p.name] = true;
        });
        state.disabledPlugins.forEach((d) => {
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
          names.forEach((name) => {
            var isExternal = installedNames.indexOf(name) >= 0;
            var isDisabled = disabledNames[name];
            var loaded = plugins.some((p) => p.name === name);
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
          installed.forEach((p) => {
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
          tools.forEach((t) => {
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
          var healthTool = tools.find((t) => t.name === "healthcheck");
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
        var passed = checks.filter((c) => c.pass).length;
        var failed = checks.filter((c) => !c.pass).length;
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
        checks.forEach((c) => {
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
        var b = k.build || {};
        var html =
          '<div class="card"><h2>' +
          __("构建", "Build") +
          "</h2>" +
          '<div class="kv-row"><span class="key">' +
          __("内核", "Kernel") +
          '</span><span class="val">' +
          escHtml(b.kernel_name || "HomeAgent") +
          "</span></div>" +
          '<div class="kv-row"><span class="key">' +
          __("内核版本", "Kernel version") +
          '</span><span class="val">' +
          escHtml(b.version ? "v" + b.version : "-") +
          "</span></div>" +
          '<div class="kv-row"><span class="key">Commit</span><span class="val">' +
          escHtml(b.commit || "-") +
          "</span></div>" +
          '<div class="kv-row"><span class="key">' +
          __("构建时间", "Build time") +
          '</span><span class="val">' +
          escHtml(b.build_time || "-") +
          "</span></div>" +
          '<div class="kv-row"><span class="key">' +
          __("SDK 兼容", "SDK compat") +
          '</span><span class="val">' +
          escHtml(b.sdk_compatible || "-") +
          "</span></div>" +
          (b.source_url
            ? '<div class="kv-row"><span class="key">' +
              __("源码", "Source") +
              '</span><span class="val"><a href="' +
              escHtml(b.source_url) +
              '" target="_blank" rel="noopener noreferrer">' +
              escHtml(b.source_url) +
              "</a></span></div>"
            : "") +
          "</div>";
        html +=
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
          k.plugins.forEach((p) => {
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
      // 共享几何：1151 节点各自 new SphereGeometry 会产生 1151 个
      // BufferGeometry（另加同样数量的光晕球）。几何形状对所有节点相同，
      // 差别只在外层 mesh.scale，故共享一份即可。半径固定 0.5，
      // 真实半径由 scale 给出（见 buildChatStarmapGraph）。
      var starmapGeo = null,
        starmapGlowGeo = null;
      // 标签角标：hover / 选中时显示的 HTML 元素（零显存，文字清晰）。
      var starmapLabelEl = null;
      // smTypeColors 的键必须与**服务端实际产出的 type 字符串小写后**一致。
      // 服务端默认类型是 "Concept"（首字母大写，见 internal/memory/graph.go），
      // 原键全为小写 ⇒ 永远匹配不上 ⇒ 1150 个节点全渲染成同一个灰色 0xcccccc，
      // 分类配色实际上从未生效过。
      var smTypeColors = {
        person: 0x4488ff,
        task: 0xff8844,
        ai: 0xaa44ff,
        concept: 0x44ff88,
        object: 0xff4444,
        // 服务端还会产出这些（indexer_test 里可见 "Person"/"Location"）
        location: 0xffaa44,
        source: 0x8899ff,
        document: 0xaabbcc,
        event: 0xff88cc,
        entity: 0x44ddcc,
        scene: 0x88ff44,
      };
      var smEdgeColors = {
        喜欢: 0xff6b6b,
        学习: 0x4ecdc4,
        属于: 0x45b7d1,
        相关: 0x96ceb4,
        使用: 0xfeca57,
        创建: 0xff9ff3,
      };

      // ===== 星图活动：让图跟上 agent 的动作 =====
      //
      // 改之前：starmapAnimate() 只转星空，节点完全静止。现在三路信号：
      //
      //  1. starmapPulse(kind)      —— 实时。SSE 的 tool_call / stage /
      //     agent_output 触发。命中的节点（按工具名匹配已知实体，否则随机
      //     取一批）做一次扩散涟漪 + 发光冲高。
      //  2. starmapPullActivity()   —— /runtime 调度器快照，3s。
      //     ready_queue_depth > 0 ⇒ 图整体「绷紧」（轻微缩放脉冲）；
      //     interrupts/preempts 上升 ⇒ 高优先级别的红色电弧感闪烁。
      //  3. starmapPullPulse()      —— /memory/graph/pulse，10s。
      //     最近变动的实体（新增概念 / mention_count 变化）做一次
      //     「生长」：从 0 缩放到正常大小，并留下余晖。
      //
      // 全部在渲染循环里推进，不额外起定时器。
      var SM_PULSE_MS = 1400; // 单个脉冲的生命期
      var SM_RIPPLE_R = 26; // 涟漪最大半径

      // starmapPulse 发出一次活动脉冲。
      // kind: "tool" | "stage" | "output" | "grow"
      function starmapPulse(kind, hint) {
        if (!starmapScene || !starmapNodeMeshes.length) return;
        var now = Date.now();
        state.starmapLastPulseAt = now;
        // 有 hint（工具名/阶段名）时优先点亮名字与提示相关的节点，
        // 这是「图在跟 agent 动」最直接的体现：调了 knowledge_* 就亮知识节点。
        var targets = starmapPickPulseTargets(hint);
        if (!targets.length) return;
        var life = kind === "grow" ? SM_PULSE_MS * 1.6 : SM_PULSE_MS;
        targets.forEach((m) => {
          state.starmapPulses.push({ mesh: m, until: now + life, kind: kind });
        });
        // 队列上限：防止密集工具调用时脉冲无限堆积占内存。
        if (state.starmapPulses.length > 260)
          state.starmapPulses = state.starmapPulses.slice(-260);
      }

      // starmapPickPulseTargets 选出该被点亮的节点。
      // 优先名字/类型命中 hint 的；不足时按 mention_count 补齐（高权重节点
      // 本身就是最常被 agent 触碰的，用它们代表「整体活动」合理）。
      //
      // ★ 匹配必须用「词」而不是子串包含：
      //   hint="knowledge" 与实体名 "k" / "e" 互为子串，会让半个图谱
      //   （含 "时"、"会" 这类单字实体）全部命中 ⇒ 脉冲退化成「全图齐亮」，
      //   既看不出关联，又把队列瞬间打满。浏览器实测：旧写法一次 pulse
      //   就选中 250 个节点、队列顶到 260 上限。
      function starmapPickPulseTargets(hint) {
        var out = [];
        if (!starmapNodeMeshes.length) return out;
        if (hint) {
          var keys = starmapHintTokens(hint);
          if (keys.length) {
            for (var i = 0; i < starmapNodeMeshes.length && out.length < 26; i++) {
              var nd = starmapNodeMeshes[i].userData.nodeData || {};
              var nm = String(nd.name || "").toLowerCase();
              var ty = String(nd.type || "").toLowerCase();
              if (!nm) continue;
              for (var k = 0; k < keys.length; k++) {
                var key = keys[k];
                // 词边界命中：实体名恰好等于该词，或以该词为词首
                // （如 "knowledge_base" 命中词 "knowledge"）。
                if (nm === key || nm.indexOf(key + "_") === 0 || ty === key) {
                  out.push(starmapNodeMeshes[i]);
                  break;
                }
              }
            }
          }
        }
        // ★ 只有「一个都没匹配上」时才用全局权重节点代表「整体活动」。
        //   实测（浏览器里跑真数据）：词匹配对 knowledge_list 只命中 1 个，
        //   但旧的补齐逻辑会把它补到 20 个 —— 于是脉冲看起来仍然是「一大片
        //   无关节点在亮」，与要修的子串 bug 效果一样，只是换了个成因。
        //   补齐只保留在真正无匹配的场景（hint 为空，如调度器脉冲）。
        if (out.length > 0) return out;
        // 补齐：按 mention_count 降序取靠前且尚未入列的。
        var sorted = starmapNodeMeshes.slice().sort((a, b) => (
            (b.userData.nodeData || {}).mention_count -
            (a.userData.nodeData || {}).mention_count
          ));
        for (var j = 0; j < sorted.length && out.length < 8; j++) {
          if (out.indexOf(sorted[j]) === -1) out.push(sorted[j]);
        }
        return out;
      }

      // starmapHintTokens 把提示词（工具名/阶段名）拆成可匹配的词元。
      // 例："knowledge_list" → ["knowledge","list"]。
      // 只保留长度 >= 3 的词：单/双字母词（"a"/"ls"）几乎必然撞上无关
      // 实体名，匹配它们只会制造噪声。
      function starmapHintTokens(hint) {
        var raw = String(hint).toLowerCase().split(/[^a-z0-9\u4e00-\u9fa5]+/);
        var out = [];
        for (var i = 0; i < raw.length; i++) {
          var t = raw[i];
          if (t.length >= 3 && out.indexOf(t) === -1) out.push(t);
        }
        return out;
      }

      // starmapPullActivity 拉 /runtime，把调度器状态映射成图的整体节奏。
      function starmapPullActivity() {
        if (!starmapScene) return;
        api("/runtime")
          .then((rt) => {
            if (!rt || !rt.scheduler) return;
            var s = rt.scheduler;
            var prev = state.starmapActivity;
            state.starmapActivity = s;
            var pend =
              (s.ready_queue_depth || 0) +
              (s.pending_interrupts || 0) +
              (s.suspend_stack || 0);
            // 有任务在排队/中断 ⇒ 脉冲，让图「绷紧」。
            if (pend > 0) starmapPulse("stage", null);
            // 中断或抢占计数上升 ⇒ 一次强脉冲（高优先级插入）。
            if (prev) {
              var dInt =
                (s.interrupts_by_level || []).reduce((a, b) => a + b, 0) -
                (prev.interrupts_by_level || []).reduce((a, b) => a + b, 0);
              var dPre =
                (s.preempts_by_level || []).reduce((a, b) => a + b, 0) -
                (prev.preempts_by_level || []).reduce((a, b) => a + b, 0);
              if (dInt > 0 || dPre > 0) starmapPulse("output", null);
            }
          })
          .catch(() => {});
      }

      // starmapPullPulse 拉轻量活动端点，把新长出来的节点标记为「生长」。
      function starmapPullPulse() {
        if (!starmapScene) return;
        var since = state.starmapPulseSince || 0;
        api("/memory/graph/pulse?since=" + since)
          .then((r) => {
            if (!r || !r.success || !r.data) return;
            var now = Math.floor(Date.now() / 1000);
            state.starmapPulseSince = now - 30; // 30s 重叠，防跨轮漏节点
            var nodes = r.data.nodes || [];
            if (!nodes.length) return;
            var known = 0;
            nodes.forEach((n) => {
              var m = starmapNodeMeshes.find((x) => x.userData.nodeId === n.id);
              if (!m) return; // 全量图里没有（可能刚创建）⇒ 忽略，等下次全量
              known++;
              state.starmapGrown[m.userData.nodeId] = Date.now() + SM_PULSE_MS * 2;
              starmapPulse("grow", n.name);
            });
            // 若有新实体但一个都没匹配上，说明全量图过期了，
            // 下一拍重拉全量（新节点才能出现）。
            if (known === 0 && nodes.length > 2) starmapDirty = true;
          })
          .catch(() => {});
      }

      // starmapTickActivity 在渲染循环里推进所有脉冲与余晖。
      function starmapTickActivity(t) {
        var now = Date.now();
        var breathe = 0;
        // 1) 活动脉冲：发光冲高 + 尺寸微扩 + 涟漪环
        if (state.starmapPulses.length) {
          var keep = [];
          for (var i = 0; i < state.starmapPulses.length; i++) {
            var p = state.starmapPulses[i];
            if (p.until <= now) {
              p.mesh.material.emissiveIntensity = p.mesh.userData.baseEmissive;
              p.mesh.scale.setScalar(p.mesh.userData.baseScale);
              continue;
            }
            keep.push(p);
            var left = (p.until - now) / SM_PULSE_MS; // 1→0
            var k = 1 - left; // 0→1
            var wave = Math.sin(Math.min(1, k) * Math.PI);
            var amp = p.kind === "output" ? 1.8 : 1.2;
            p.mesh.material.emissiveIntensity =
              p.mesh.userData.baseEmissive + wave * amp;
            p.mesh.scale.setScalar(
              p.mesh.userData.baseScale * (1 + wave * 0.28),
            );
            // 生长：新节点从 0 弹到正常大小
            if (p.kind === "grow") {
              var g = Math.min(1, k * 1.4);
              p.mesh.scale.setScalar(
                p.mesh.userData.baseScale * (0.15 + 0.85 * g),
              );
            }
          }
          state.starmapPulses = keep;
        }
        // 2) 「生长」余晖：脉冲结束后短暂保留一点亮
        if (state.starmapGrown) {
          for (var gid in state.starmapGrown) {
            if (state.starmapGrown[gid] <= now) {
              delete state.starmapGrown[gid];
              continue;
            }
            var gm = starmapNodeMeshes.find((x) => x.userData.nodeId == gid);
            if (gm)
              gm.material.emissiveIntensity = Math.max(
                gm.material.emissiveIntensity,
                gm.userData.baseEmissive + 0.6,
              );
          }
        }
        // 3) 全局呼吸：距上次活动越近越亮，实现「agent 一忙图就活」
        var idle = (now - (state.starmapLastPulseAt || 0)) / 4000;
        breathe = Math.max(0, 1 - idle);
        if (breathe > 0.01 && starmapNodeMeshes.length) {
          // 只抽样一部分节点做呼吸，避免每帧改 1151 个材质。
          var stride = 24;
          for (var b = 0; b < starmapNodeMeshes.length; b += stride) {
            var m2 = starmapNodeMeshes[b];
            if (m2.userData.baseEmissive === undefined) continue;
            m2.material.emissiveIntensity = Math.max(
              m2.userData.baseEmissive,
              m2.userData.baseEmissive + breathe * 0.25,
            );
          }
        }
        return breathe;
      }

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
            // 复位用 baseScale，不能用 set(1,1,1)：节点现在是按 mention_count
            // 缩放过的（userData.baseScale），置 1 会把大节点缩成最小尺寸。
            // （这是低规格改造后必须跟着改的一处，旧代码能“跑”是因为那时
            //  几何体本身就带半径、不靠 scale。）
            if (starmapHovered)
              starmapHovered.scale.setScalar(
                starmapHovered.userData.baseScale || 1,
              );
            starmapHovered = n;
            starmapLabelShow(n);
          }
        } else {
          if (starmapHovered) {
            starmapHovered.scale.setScalar(
              starmapHovered.userData.baseScale || 1,
            );
            starmapHovered = null;
            starmapLabelHide();
          }
        }
      }

      // starmapLabelShow/Hide：hover 时在容器角上显示一个 HTML 角标。
      // 取代原先每节点一张 256x64 CanvasTexture（1151 张贴图 ≈ 72MB 显存），
      // 文字用 DOM 渲染反而更清楚，且零 GPU 开销。
      function starmapLabelShow(mesh) {
        var cont = starmapActiveContainer();
        if (!cont) return;
        if (!starmapLabelEl) {
          starmapLabelEl = document.createElement("div");
          starmapLabelEl.id = "sm-label";
        }
        // 角标必须跟 canvas 在同一个容器里（它是绝对定位在容器上的）。
        // canvas 换页签搬运时，角标也要跟着搬。
        if (starmapLabelEl.parentElement !== cont) cont.appendChild(starmapLabelEl);
        var nd = (mesh && mesh.userData && mesh.userData.nodeData) || {};
        var nm = nd.name || nd.id || "";
        starmapLabelEl.textContent =
          nm + (nd.mention_count ? "  ×" + nd.mention_count : "");
        starmapLabelEl.style.display = "block";
      }
      function starmapLabelHide() {
        if (starmapLabelEl) starmapLabelEl.style.display = "none";
      }
      // starmapContainer 返回当前星图所在的容器（主页或聊天面板任一）。
      function starmapContainer() {
        return (
          document.getElementById("sm-container-home") ||
          document.getElementById("sm-container-chat")
        );
      }

      // starmapActiveContainer 选出「该把星图画在哪」的容器。
      //
      // 规则：先看独立星图页签，不存在（该页签还没打开过）就退回总览页，
      // 再退回聊天面板。为什么要这个优先级：three.js 的 canvas 只能有一个
      // 父节点，同时往两处渲染就会一边黑屏。每次切页签都把 canvas 搬到
      // 当前该显示的地方，是单一 renderer 前提下最干净的做法。
      function starmapActiveContainer() {
        var tab = document.getElementById("tab-starmap");
        if (tab && tab.classList.contains("active")) {
          var c = document.getElementById("sm-container-page");
          if (c) return c;
        }
        var ov = document.getElementById("tab-overview");
        if (ov && ov.classList.contains("active")) {
          var h = document.getElementById("sm-container-home");
          if (h) return h;
        }
        return (
          document.getElementById("sm-container-chat") ||
          document.getElementById("sm-container-home") ||
          null
        );
      }

      // renderStarmapTab 渲染独立星图页签（只建一次骨架，不重复重建）。
      function renderStarmapTab() {
        var host = document.getElementById("tab-starmap");
        if (!host) return;
        if (!document.getElementById("sm-container-page")) {
          var rt = state.runtime || {};
          var sc = rt.scheduler || {};
          var pend =
            (sc.ready_queue_depth || 0) + (sc.pending_interrupts || 0);
          host.innerHTML =
            '<div class="card"><h2>' +
            __("记忆星图", "Memory Star Map") +
            ' <span class="badge" id="sm-page-stat" style="font-size:10px;font-weight:400"></span></h2>' +
            '<div id="sm-container-page" style="height:calc(100vh - 260px);min-height:420px"></div>' +
            '<div style="display:flex;gap:14px;flex-wrap:wrap;margin-top:10px;font-size:11px;color:var(--text-secondary)">' +
            smLegend("person", __("人物", "Person")) +
            smLegend("concept", __("概念", "Concept")) +
            smLegend("object", __("对象", "Object")) +
            smLegend("location", __("地点", "Location")) +
            smLegend("source", __("来源", "Source")) +
            "</div>" +
            '<div style="margin-top:8px;font-size:11px;color:var(--text-muted)">' +
            __(
              "星图跟随 agent 活动脉动：工具调用 / 阶段推进 / 输出 / 调度器繁忙 / 新记忆生长。",
              "The map pulses with agent activity: tool calls, stage progress, output, scheduler load, new memory.",
            ) +
            "</div></div>";
        }
        // 页签每次激活都把 canvas 搬过来 + 重新按容器尺寸 resize。
        if (document.getElementById("sm-container-page")) {
          if (!state.starmapInit && !state.starmapLoading)
            loadChatStarmapData();
          else if (starmapRen) {
            var cont = document.getElementById("sm-container-page");
            if (starmapRen.domElement.parentElement !== cont) {
              cont.appendChild(starmapRen.domElement);
              starmapRen.domElement.style.display = "block";
            }
            onStarmapResize();
          }
        }
        smUpdateStat();
      }

      // smLegend 生成图例小项。
      function smLegend(key, label) {
        var col = smTypeColors[key] || 0xcccccc;
        var hex = "#" + ("0000" + col.toString(16)).slice(-6);
        return (
          '<span style="display:inline-flex;align-items:center;gap:5px">' +
          '<i style="width:9px;height:9px;border-radius:50%;background:' +
          hex +
          ';display:inline-block"></i>' +
          escHtml(label) +
          "</span>"
        );
      }

      // smUpdateStat 在星图页签头部显示节点/边/活动状态。
      function smUpdateStat() {
        var el = document.getElementById("sm-page-stat");
        if (!el) return;
        var act = state.starmapActivity;
        var busy = act && (act.ready_queue_depth || act.pending_interrupts);
        var txt = starmapNodes.length + " " + __("节点", "nodes");
        if (starmapEdges.length) txt += " / " + starmapEdges.length + " " + __("关系", "edges");
        if (busy) txt += " · " + __("调度中", "busy");
        el.textContent = txt;
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
        var m = starmapNodeMeshes.find((x) => x.userData.nodeId === nodeId);
        if (!m) return;
        var tp = m.position.clone(),
          sp = starmapCam.position.clone(),
          st = starmapCtrl.target.clone();
        var dist = tp.length() + 25,
          ep = new THREE.Vector3(tp.x, tp.y + dist * 0.4, tp.z + dist * 0.8);
        var t0 = Date.now();
        (function lerp() {
          var t = Math.min((Date.now() - t0) / dur, 1),
            e = 1 - (1 - t) ** 3;
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
        // 推进活动脉冲 / 生长 / 呼吸（无活动时开销≈0）。
        if (starmapNodeMeshes.length) starmapTickActivity();
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
        // 「外观」不是插件设置，但它是 webui 自己的配置，放在设置页最前面。
        var ap = document.createElement("span");
        ap.textContent = __("外观", "Appearance");
        if (state.selectedSection === "appearance") ap.className = "active";
        ap.onclick = () => {
          state.selectedSection = "appearance";
          renderOneSettings();
        };
        el.appendChild(ap);
        state.settingsPlugins.forEach((p) => {
          var a = document.createElement("span");
          a.textContent = pluginDisplayName(p);
          if (p === state.selectedSection) a.className = "active";
          a.onclick = () => {
            state.selectedSection = p;
            renderOneSettings();
          };
          el.appendChild(a);
        });
      }

      // 外观设置区：主题（浅/深）+ 配色 + 背景图。与侧栏那个 palette-pop 同一套
      // setColor/applyBgImg/applyBgBlur，只是给它一个正式的、可发现的落点
      // （原来只有侧栏底部一个调色盘图标，找不到）。
      function appearanceHtml() {
        var theme = document.documentElement.getAttribute("data-theme") || "light";
        var color = localStorage.getItem("ha-color") || "mono";
        var dots = "";
        Object.keys(PALETTES).forEach((k) => {
          dots +=
            '<button class="ap-dot' + (k === color ? " on" : "") + '" data-c="' + k +
            '" title="' + k + '" style="background:' + PALETTES[k] + '" onclick="pickColor(\'' + k + '\')"></button>';
        });
        var img = localStorage.getItem("ha-bg-img") || "";
        var blur = localStorage.getItem("ha-bg-blur") || "0";
        return (
          '<div class="card"><h2>' + __("外观", "Appearance") + "</h2>" +
          '<div class="kv-row"><span class="key">' + __("主题", "Theme") + '</span><span class="val">' +
          '<button class="btn btn-sm' + (theme === "light" ? "" : " btn-ghost") + '" onclick="pickTheme(\'light\')">' + __("浅色", "Light") + "</button> " +
          '<button class="btn btn-sm' + (theme === "dark" ? "" : " btn-ghost") + '" onclick="pickTheme(\'dark\')">' + __("深色", "Dark") + "</button>" +
          "</span></div>" +
          '<div class="kv-row"><span class="key">' + __("配色", "Color") + '</span><span class="val"><span class="ap-dots">' + dots + "</span></span></div>" +
          "<label>" + __("背景图片 URL", "Background image URL") + "</label>" +
          '<input type="text" id="ap-bg-img" value="' + escHtml(img) + '" placeholder="https://...jpg / png">' +
          '<div style="display:flex;gap:8px;margin-top:8px">' +
          '<button class="btn btn-sm" onclick="applyBgImg(document.getElementById(\'ap-bg-img\').value)">' + __("应用", "Apply") + "</button>" +
          '<button class="btn btn-ghost btn-sm" onclick="applyBgImg(\'\');document.getElementById(\'ap-bg-img\').value=\'\'">' + __("清除", "Clear") + "</button>" +
          "</div>" +
          '<label style="margin-top:12px">' + __("背景模糊", "Background blur") + '：<span id="ap-blur-val">' + blur + "px</span></label>" +
          '<input type="range" id="ap-blur-range" min="0" max="30" value="' + blur + '" oninput="applyBgBlur(this.value);var v=document.getElementById(\'ap-blur-val\');if(v)v.textContent=this.value+\'px\'">' +
          '<p style="color:var(--text-muted);font-size:12px;margin-top:10px">' +
          __("配色只影响本站点界面的强调色，不改动 Agent 的人格或数据。", "Colors only affect this dashboard's accent; agent behavior and data are unchanged.") +
          "</p></div>"
        );
      }
      function pickTheme(name) {
        saveTheme(name);
        renderOneSettings();
      }
      function pickColor(name) {
        setColor(name);
        renderOneSettings();
      }

      function renderOneSettings() {
        if (state.selectedSection === "appearance") {
          document.getElementById("tab-settings").innerHTML =
            '<div class="settings-layout"><div class="settings-tabs"></div><div class="settings-content">' +
            appearanceHtml() +
            "</div></div>";
          renderSettingsSidebar();
          return;
        }
        var prefix = state.selectedSection + ".";
        var allKeys = Object.keys(state.settings || {});
        var filtered = allKeys.filter((k) => k === prefix.slice(0, -1) || k.startsWith(prefix));
        filtered.sort();
        var hideTopLlms = [
          "core.llm.base_url",
          "core.llm.model",
          "core.llm.api_key",
          "core.llm.adapter",
          "core.llm.adapter_path",
          "core.llm.thinking_enabled",
        ];
        var sourceKeys = filtered.filter((k) => k.startsWith("core.llm.sources."));
        var sourceMap = {};
        sourceKeys.forEach((k) => {
          var parts = k.split(".");
          var srcName = parts[3];
          if (!sourceMap[srcName]) sourceMap[srcName] = {};
          sourceMap[srcName][k] = true;
        });
        var mcpServerKeys = filtered.filter((k) => (
            k.startsWith("plugin.mcp.servers.") && k.split(".").length >= 5
          ));
        var mcpServerMap = {};
        mcpServerKeys.forEach((k) => {
          var parts = k.split(".");
          var srvName = parts[3];
          if (!mcpServerMap[srvName]) mcpServerMap[srvName] = {};
          mcpServerMap[srvName][k] = true;
        });
        var regularKeys = filtered.filter((k) => (
            !k.startsWith("core.llm.sources.") &&
            hideTopLlms.indexOf(k) === -1 &&
            !k.startsWith("plugin.mcp.servers.") &&
            k !== "plugin.mcp.servers"
          ));
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
          regularKeys.forEach((k) => {
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
              opts.forEach((o) => {
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
              m.extra.forEach((f) => {
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
                    f.options.forEach((o) => {
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
            .forEach((src) => {
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
              fields.forEach((f) => {
                var fk = baseKey + "." + f.key;
                var fv = state.settings?.[fk] || "";
                var flabel = f.label;
                var fieldId = "inp-" + fk.replace(/\./g, "_");
                if (f.type === "select") {
                  var fopts = "";
                  f.options.forEach((o) => {
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
              .forEach((srv) => {
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
                fields.forEach((f) => {
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
        var promises = keys.map((f) => api("/settings", {
            method: "PUT",
            body: JSON.stringify({
              key: "core.llm.sources." + name + "." + f,
              value: values[f],
            }),
          }));
        Promise.all(promises)
          .then(() => {
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
          .catch((e) => {
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
        var promises = fields.map((f) => api("/settings", {
            method: "PUT",
            body: JSON.stringify({
              key: "plugin.mcp.servers." + name + "." + f,
              value: values[f],
            }),
          }));
        Promise.all(promises)
          .then(() => {
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
          .catch((e) => {
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
            adapters.forEach((a) => {
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
            '\nreturn {\n  name = &quot;openai&quot;,\n  version = &quot;1.0&quot;,\n  transform_request = function(raw) ... end,\n  transform_response = function(raw) ... end,\n}"></textarea>' +
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
      (async () => {
        await loadChatHistory();
        renderAll();
        connectSSE();
        startUptimeTicker();
        startRuntimeTicker();
        startChatTicker();
        maybeShowPersonaWizard();
      })();
      setInterval(renderAll, 15000);
      // 消息同步轮询兜底：每30秒增量同步 chatHistory，补偿 SSE 断连窗口期
      // 丢失的事件（尤其是非 WebUI 触发的跨渠道消息，如 CLI/QQ/设备桥输出）。
      // syncChatFromHistory 仅追加新消息 DOM 节点，不重建已有消息，无闪烁。
      setInterval(() => {
        syncChatFromHistory().catch(()=> {});
      }, 30000);
