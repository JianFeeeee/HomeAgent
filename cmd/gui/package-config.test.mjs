// GUI 发行打包配置的一致性判据。
//
// ## 要判的是什么
//
// 两条打包路径的配置各自漏过东西，且都是**静默**失败：
//
//   1) electron-packager（make build-gui，Windows 发行 + 本机安装都走它）
//      · 曾漏 --icon=icon.ico ⇒ exe 一直是 Electron 默认图标
//        （已修：Makefile e79eff6）
//
//   2) package.json 的 build 段（electron-builder，Linux 发行走它）
//      · files 白名单漏了 icon.ico / icon-tray*.png ⇒ 图标不进包
//      · win.icon 路径写错（electron-builder 相对**项目目录**解析，
//        写 'build/icon.ico' 会找不到文件）
//
//   3) deploy/packaging/package-linux.sh 的手工组装曾逐文件 cp 七个文件，
//      漏掉 renderer/vendor/*（本地化的 four 个库）与全部图标文件
//      ⇒ Linux 发行包装完星图与托盘图标直接坏，且**没有任何构建报错**
//      （已修：改目录整体同步 + 必备文件校验）
//
// 打包配置类问题的特点是"改了没人知道、坏了要等用户装上才发现"，
// 所以在这里钉死。
//
// 运行：node package-config.test.mjs

import { readFileSync, existsSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const here = dirname(fileURLToPath(import.meta.url));
const repoRoot = join(here, "..", "..");
const guiDir = here;

let failures = 0;
const check = (name, ok, detail) => {
  if (ok) console.log(`  ✓ ${name}`);
  else {
    failures++;
    console.log(`  ✗ ${name}${detail ? " — " + detail : ""}`);
  }
};

// ── 1) Makefile: electron-packager 必须带 --icon ──────────────────────
const mk = readFileSync(join(repoRoot, "Makefile"), "utf8");
const buildGui = mk.slice(
  mk.indexOf("build-gui:"),
  mk.indexOf("\nbuild-static:"),
);

check("能定位 build-gui 目标", buildGui.length > 0);
check(
  "★ build-gui 传 --icon（否则 exe 一直是 Electron 默认图标）",
  /--icon[= ]\S*icon\.ico/.test(buildGui),
  "缺 --icon ⇒ exe 内嵌图标为 Electron 默认",
);
check(
  "build-gui 仍带 --no-sandbox",
  /--no-sandbox/.test(buildGui),
  "--no-sandbox 没了，本机可能起不来",
);

// ── 2) package.json build 段 ─────────────────────────────────────────
const pkg = JSON.parse(readFileSync(join(guiDir, "package.json"), "utf8"));
const files = pkg.build?.files || [];

check(
  "★ files 白名单含 icon.ico",
  files.includes("icon.ico"),
  "files = " + JSON.stringify(files),
);
check(
  "files 白名单含托盘图标",
  files.includes("icon-tray.png") && files.includes("icon-tray@2x.png"),
  "缺 icon-tray*.png ⇒ 托盘图标在发行包里丢失",
);
check(
  "files 含 renderer/**/*（含 vendor/）",
  files.some((f) => f.startsWith("renderer/")),
  "renderer 未列入 ⇒ vendor 库不会进包",
);

// ── 3) win.icon 路径语义 ─────────────────────────────────────────────
// electron-builder 的 icon 相对**项目目录**（package.json 所在）解析。
// 写成 'build/icon.ico'（相对输出目录）会找不到文件。
const winIcon = pkg.build?.win?.icon;
check("配置了 win.icon", !!winIcon, "未配置 win.icon");
if (winIcon) {
  check(
    "★ win.icon 路径不是误写的 build/ 前缀",
    !winIcon.startsWith("build/"),
    `win.icon='${winIcon}' —— electron-builder 相对项目目录解析，` +
      "带 build/ 前缀会找不到文件",
  );
  check(
    "win.icon 指向的文件真实存在",
    existsSync(join(guiDir, winIcon)),
    `${winIcon} 不存在于 cmd/gui/`,
  );
}

// ── 4) icon.ico 本身要含足够尺寸 ────────────────────────────────────
const ico = readFileSync(join(guiDir, "icon.ico"));
const isIco = ico.readUInt16LE(0) === 0 && ico.readUInt16LE(2) === 1;
check("icon.ico 是合法 ICO", isIco, "头部不是 00 00 01 00");
const icoCount = ico.readUInt16LE(4);
check(
  "icon.ico 含多尺寸（含 256x256，electron-builder 需要）",
  icoCount >= 2,
  `只含 ${icoCount} 个尺寸`,
);

// ── 5) package-linux.sh 不得再逐文件手列 ────────────────────────────
const pkgLinux = readFileSync(
  join(repoRoot, "deploy", "packaging", "package-linux.sh"),
  "utf8",
);
check(
  "★ package-linux.sh 按目录整体同步 renderer",
  /cp -r "\$gui_dir\/renderer\/\."/.test(pkgLinux),
  "仍在逐文件 cp ⇒ 新增资源（如 vendor/）不会被打进 Linux 包",
);
check(
  "package-linux.sh 同步根级图标",
  /cp "\$gui_dir"\/\*\.ico/.test(pkgLinux) &&
    /icon-tray/.test(pkgLinux),
  "未同步根级 ico / 托盘图标",
);
check(
  "package-linux.sh 有必备文件校验",
  /vendor\/three\.min\.js/.test(pkgLinux) && /rm -rf "\$gui_out"/.test(pkgLinux),
  "缺必备文件校验 ⇒ 装完才发现坏包",
);

console.log("");
if (failures > 0) {
  console.log(`全部失败：${failures} 条`);
  process.exit(1);
}
console.log("全部通过");