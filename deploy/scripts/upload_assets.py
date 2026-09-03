#!/usr/bin/env python3
"""上传 release 资产到 gitcode（两步：取签名 URL → PUT 到 OBS）。

用法: upload_assets.py <tag> <token> [file...]
不传 file 时上传 dist/release/ 下全部发布产物。

环境变量：
  GITCODE_REPO  目标仓库，默认 JianFeeeee/HomeAgent（SDK 仓传 JianFeeeee/homeagent-sdk）
  ASSET_DIR     资产目录，默认 <repo>/dist/release

为何两步：gitcode 的 release 附件不走 API 直传，而是先向
`releases/<tag>/upload_url` 要一个 OBS 预签名 URL（带 x-obs-* 回调头），
再把文件 PUT 到那个 URL。回调头必须原样透传，否则 OBS 收下了文件但
gitcode 侧不会登记为 release 附件。
"""
import json
import os
import sys
import urllib.error
import urllib.parse
import urllib.request

REPO = os.environ.get("GITCODE_REPO", "JianFeeeee/HomeAgent")
API = "https://gitcode.com/api/v5/repos"

# 发布产物后缀。注意 Windows 安装器是 HomeAgent_v*_win64.exe，
# 与 bin/ 里的裸 .exe 靠 _win64.exe 后缀区分。
ARTIFACT_SUFFIXES = (
    ".tar.gz",
    ".zip",
    ".deb",
    ".rpm",
    ".pkg",
    "_win64.exe",
)


def is_artifact(name: str) -> bool:
    return name == "SHA256SUMS" or name.endswith(ARTIFACT_SUFFIXES)


def get_upload_url(tag: str, token: str, filename: str) -> tuple[str, dict]:
    q = urllib.parse.urlencode({"file_name": filename})
    url = f"{API}/{REPO}/releases/{tag}/upload_url?{q}"
    req = urllib.request.Request(url, headers={"private-token": token})
    with urllib.request.urlopen(req, timeout=30) as r:
        data = json.loads(r.read())
    return data["url"], data.get("headers", {})


def put_file(url: str, headers: dict, path: str) -> tuple[int, str]:
    size = os.path.getsize(path)
    with open(path, "rb") as f:
        body = f.read()
    req = urllib.request.Request(url, data=body, method="PUT")
    for k, v in headers.items():
        req.add_header(k, v)
    req.add_header("Content-Length", str(size))
    try:
        # 大文件（Full 变体安装包近 100MB）给足超时。
        with urllib.request.urlopen(req, timeout=900) as r:
            return r.status, r.read().decode("utf-8", "replace")[:300]
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode("utf-8", "replace")[:300]
    except Exception as e:  # noqa: BLE001
        return 0, f"{type(e).__name__}: {e}"


def main() -> int:
    if len(sys.argv) < 3:
        print(__doc__)
        return 2
    tag, token = sys.argv[1], sys.argv[2]
    outdir = os.environ.get("ASSET_DIR") or os.path.join(
        os.path.dirname(os.path.dirname(os.path.abspath(__file__))),
        "dist",
        "release",
    )
    files = sys.argv[3:] or sorted(
        f for f in os.listdir(outdir) if is_artifact(f)
    )
    print(f"repo={REPO} tag={tag} dir={outdir}", flush=True)
    failed = []
    for name in files:
        path = os.path.join(outdir, name)
        if not os.path.isfile(path):
            print(f"skip (missing): {name}", flush=True)
            continue
        mib = os.path.getsize(path) / 1048576
        print(f"==> {name} ({mib:.1f} MiB)", flush=True)
        try:
            url, headers = get_upload_url(tag, token, name)
        except Exception as e:  # noqa: BLE001
            print(f"    upload_url FAILED: {e}", flush=True)
            failed.append(name)
            continue
        status, body = put_file(url, headers, path)
        ok = 200 <= status < 300
        print(f"    PUT -> {status} {'OK' if ok else body}", flush=True)
        if not ok:
            failed.append(name)
    print(f"\n{'ALL OK' if not failed else f'{len(failed)} FAILED: ' + ', '.join(failed)}")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
