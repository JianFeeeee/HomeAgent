#!/usr/bin/env python3
"""生产切换：经 pluginmgr 正规通道安装 17 个 hmap（Part 6.5）。

与手工拷贝方案的区别 —— 这里复用内核自己的安装逻辑：

  validatePackage   校验 manifest + 平台二进制齐全
  StopAndUnload     停旧实例但**保留配置表**
  os.Rename 备份    解包失败自动回滚到旧版本
  platformBinary()  按 runtime 挑当前平台那份，重命名为 plugin.bin
  chmod 0755        补执行位

手工拷贝会重新实现这一套，且必然实现得更差（第一版就漏了 platforms 字段
与配置保留语义）。

用法：
    switch-production.py            演练
    switch-production.py --apply    实际安装
"""

import json
import os
import sys
import urllib.error
import urllib.request

PROD_PLUGINS = "/home/newqqagent/plugins"
SDK_EXAMPLE = "/home/program/TrueAgent/third_party/homeagent-sdk/example"
PLUGINMGR = "http://127.0.0.1:9876/plugins"


def find_hmap(name):
    """找插件的 hmap 包。

    bundle:true  -> <snake>_bundle.hmap（含多平台二进制）
    bundle:false -> <snake>_<goos>_<goarch>.hmap（qq 是这种）
    """
    dist = os.path.join(SDK_EXAMPLE, name, "dist")
    if not os.path.isdir(dist):
        return None
    cands = [f for f in os.listdir(dist) if f.endswith(".hmap")]
    if not cands:
        return None
    for c in cands:
        if c.endswith("_bundle.hmap"):
            return os.path.join(dist, c)
    return os.path.join(dist, sorted(cands)[0])


def install(path):
    """POST 到 pluginmgr。overwrite=true 走原地更新分支，保留配置表。"""
    body = json.dumps({"path": path, "overwrite": True}).encode()
    req = urllib.request.Request(
        PLUGINMGR, data=body,
        headers={"Content-Type": "application/json"},
        method="POST")
    try:
        with urllib.request.urlopen(req, timeout=180) as resp:
            return json.loads(resp.read().decode()), None
    except urllib.error.HTTPError as e:
        return None, "HTTP %d: %s" % (e.code, e.read().decode()[:300])
    except Exception as e:
        return None, str(e)


def main():
    apply = "--apply" in sys.argv

    targets = sorted(
        d for d in os.listdir(PROD_PLUGINS)
        if os.path.isfile(os.path.join(PROD_PLUGINS, d, "plugin.so"))
        or os.path.isfile(os.path.join(PROD_PLUGINS, d, "plugin.bin"))
    )
    print("生产外部插件: %d 个" % len(targets))

    # 先全部校验，任一缺包就整批中止。
    # 理由：新 homed 不认 .so，「一半装了一半没装」的中间态最难排查。
    plan = []
    missing = []
    for name in targets:
        h = find_hmap(name)
        if h is None:
            missing.append(name)
        else:
            plan.append((name, h))

    if missing:
        print("\n✗ 中止：以下插件缺 hmap 包：")
        for m in missing:
            print("    " + m)
        print("\n先跑 rebuild-plugins.sh 重编。")
        return 1

    print("✓ 全部 %d 个 hmap 就位\n" % len(plan))
    for name, h in plan:
        print("  %-16s %-44s %6d KB" % (
            name, os.path.basename(h), os.path.getsize(h) // 1024))

    if not apply:
        print("\n[演练] 加 --apply 才实际安装")
        return 0

    print("\n经 pluginmgr 安装（overwrite=true，保留配置）...")
    ok = 0
    failed = []
    for name, h in plan:
        result, err = install(h)
        if err:
            print("  ✗ %-16s %s" % (name, err))
            failed.append(name)
            continue
        if "error" in result:
            print("  ✗ %-16s %s: %s" % (
                name, result["error"], result.get("details", "")))
            failed.append(name)
            continue
        print("  ✓ %-16s %-12s v%s -> v%s config_kept=%s" % (
            name,
            result.get("action", "?"),
            result.get("previous_version", "?"),
            result.get("version", "?"),
            result.get("config_kept", False)))
        ok += 1

    print("\n成功 %d / 失败 %d" % (ok, len(failed)))
    if failed:
        print("失败: " + " ".join(failed))
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
