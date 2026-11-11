# -*- coding: utf-8 -*-
# 日期: 2026-11-11 19:58:12.407
# 作者: Zhai Wenjia
"""
bump_version.py —— 脚本版本 bump + 组件版本快照 CLI

职责（遵循 AGENTS.md 的版本管理条「双轨制」）：
- 仅对 CLI 参数指定的脚本递增 SemVer（patch / minor / major）；
- 未指定的脚本版本号保持不变（**独立版本原则**）；
- 生成新的 ``snapshots/YYYY-M-D_<n>.json`` 记录当前所有脚本版本组合；
- 支持 ``--dry-run``（只预览不写）与 ``rollback``（回滚到上一个快照）。

用法::

    # Patch bump 指定脚本，生成新快照
    python bump_version.py bump main.py src/flying_gateway/router.py --type patch

    # Minor bump 单个脚本
    python bump_version.py bump src/flying_gateway/registry.py --type minor

    # Dry run（只打印将发生的变化，不写文件）
    python bump_version.py bump main.py --type patch --dry-run

    # 回滚到上一个快照（还原 __version__）
    python bump_version.py rollback

    # 列出当前所有组件版本
    python bump_version.py list

    # 指定项目根（默认为本脚本所在目录）
    python bump_version.py --root /path/to/repo bump main.py --type patch

设计要点：
- **不引入 semver 第三方库**，自己实现解析与递增（十行代码足够）；
- 解析脚本的 ``__version__`` 用 ``ast``（而不是正则或 ``eval``），避免执行用户代码；
- 生成快照时遍历 ``src/`` 下所有 ``.py`` 收集 ``__version__``，**无需手工维护清单**；
- 快照创建用 ``O_CREAT | O_EXCL`` 原子独占，撞号不覆盖（详见 :func:`write_snapshot`）。
"""

from __future__ import annotations

import argparse
import ast
import datetime
import json
import os
import re
import sys
from pathlib import Path


__version__ = "0.1.0"


# ---------------------------------------------------------------------------
# SemVer 工具
# ---------------------------------------------------------------------------

_SEMVER_RE = re.compile(r"^(\d+)\.(\d+)\.(\d+)$")


def parse_semver(version: str) -> tuple[int, int, int]:
    """解析 SemVer 字符串为 ``(major, minor, patch)`` 三元组。

    不接受 pre-release / build metadata（本项目暂不需要）。
    """
    m = _SEMVER_RE.match(version.strip())
    if not m:
        raise ValueError(f"invalid SemVer: {version!r}")
    return int(m.group(1)), int(m.group(2)), int(m.group(3))


def bump_semver(version: str, bump_type: str) -> str:
    """对 SemVer 字符串做递增，返回新版本字符串。

    patch: ``0.0.N`` → ``0.0.N+1``；minor: ``0.(N+1).0``；major: ``(N+1).0.0``。
    """
    major, minor, patch = parse_semver(version)
    if bump_type == "patch":
        return f"{major}.{minor}.{patch + 1}"
    if bump_type == "minor":
        return f"{major}.{minor + 1}.0"
    if bump_type == "major":
        return f"{major + 1}.0.0"
    raise ValueError(f"unknown bump type: {bump_type!r}")


# ---------------------------------------------------------------------------
# __version__ 解析与写入
# ---------------------------------------------------------------------------

_VERSION_ASSIGN_RE = re.compile(
    r'^(__version__\s*=\s*["\'])([^"\']+)(["\'])',
    re.MULTILINE,
)


def read_version(path: Path) -> str:
    """从 ``.py`` 文件读取 ``__version__`` 字符串。

    用 ``ast`` 校验格式、正则匹配赋值 —— 只信任**模块级**字符串字面量的
    ``__version__`` 赋值，忽略函数内同名变量。
    """
    src = path.read_text(encoding="utf-8")
    tree = ast.parse(src)
    for node in tree.body:
        if not isinstance(node, ast.Assign):
            continue
        if len(node.targets) != 1:
            continue
        target = node.targets[0]
        if not isinstance(target, ast.Name) or target.id != "__version__":
            continue
        if isinstance(node.value, ast.Constant) and isinstance(node.value.value, str):
            return node.value.value
    raise ValueError(f"no __version__ found in {path}")


def write_version(path: Path, new_version: str) -> None:
    """把文件中 ``__version__`` 的字面量替换为 ``new_version``。"""
    src = path.read_text(encoding="utf-8")
    new_src, n = _VERSION_ASSIGN_RE.subn(
        lambda m: f"{m.group(1)}{new_version}{m.group(3)}",
        src,
        count=1,
    )
    if n != 1:
        raise ValueError(f"failed to rewrite __version__ in {path}")
    path.write_text(new_src, encoding="utf-8")


# ---------------------------------------------------------------------------
# 快照收集与生成
# ---------------------------------------------------------------------------


def collect_components(root: Path) -> dict[str, str]:
    """收集 ``{相对路径: __version__}`` 映射。

    只扫三类：
    - ``root/src/**/*.py``（跳过 ``__pycache__`` 与 ``__init__.py``）；
    - ``root/main.py``（项目根 CLI 入口）；
    - ``root/bump_version.py``（本脚本自身）。

    ``test/`` 不参与版本管理：测试不是版本化组件（与 AGENTS.md 的版本管理条一致）。
    """
    components: dict[str, str] = {}

    src_dir = root / "src"
    if src_dir.is_dir():
        for py in sorted(src_dir.rglob("*.py")):
            if "__pycache__" in py.parts:
                continue
            if py.name == "__init__.py":
                continue
            try:
                ver = read_version(py)
            except (ValueError, SyntaxError):
                continue
            rel = py.relative_to(root)
            key = ".".join(rel.with_suffix("").parts)
            components[key] = ver

    for name in ("main.py", "bump_version.py"):
        f = root / name
        if not f.exists():
            continue
        try:
            ver = read_version(f)
        except (ValueError, SyntaxError):
            continue
        components[name.removesuffix(".py")] = ver

    return components


def parse_snap_id(name: str) -> tuple[int, int, int, int]:
    """解析快照文件名 ``YYYY-M-D_<n>.json`` → ``(y, m, d, n)``。

    解析不出来就返回 ``(0, 0, 0, 0)`` —— 调用方**必须过滤掉**这个值，
    否则非快照命名的 ``.json`` 会把 max 拖成 0。
    """
    m = re.match(r"^(\d{4})-(\d{1,2})-(\d{1,2})_(\d+)\.json$", name)
    if not m:
        return (0, 0, 0, 0)
    return (int(m.group(1)), int(m.group(2)), int(m.group(3)), int(m.group(4)))


def next_snapshot_id(root: Path) -> str:
    """生成下一个快照 ID，格式 ``YYYY-M-D_<n>``，n 每日重置从 1 开始。

    扫描 ``snapshots/`` 下同日期的 ID 并**取最大值 +1**。
    ⚠️ 不用 ``len(existing) + 1``：那只在编号严格连续时正确，而 ``rollback``
    会删掉文件、多分支各建各的也会留跳号 ⇒ 跳号场景下旧实现会**重新生成一个
    已被占用的编号**，静默覆盖别的快照。
    """
    today = datetime.date.today()
    date_str = f"{today.year}-{today.month}-{today.day}"
    snapshots_dir = root / "snapshots"
    if not snapshots_dir.exists():
        return f"{date_str}_1"

    ns = [
        parse_snap_id(p.name)[3]
        for p in snapshots_dir.glob(f"{date_str}_*.json")
        if parse_snap_id(p.name)[:3] == (today.year, today.month, today.day)
    ]
    n = max(ns) + 1 if ns else 1
    return f"{date_str}_{n}"


#: 快照占号的原子创建重试上限。撞号时不覆盖、不报错，而是换一个候选重试。
#: ⚠️ **只防同一台机器上的并发** —— 跨分支撞号（分支 A 已 push、B 未 fetch）
#: 文件锁也防不住，只能在合并时重排编号。
_SNAPSHOT_CREATE_ATTEMPTS = 16


def write_snapshot(root: Path, snapshot: dict) -> Path:
    """原子地把快照写入 ``snapshots/<id>.json``，返回实际写入的路径。

    ⭐ 用 ``O_CREAT | O_EXCL`` 让内核**一步**完成「检查不存在 + 创建」——
    两步式的「先判断存不存在、再创建」在两个进程之间有窗口期，两边都会认为
    自己是第一个（⇒ 撞号 ⇒ 覆盖别人的快照）。

    ⚠️ 撞号时**不覆盖、不静默**，而是 ``n += 1`` 换候选重试；重试耗尽抛错
    —— 宁可停下告知（铁律 1：不可伪造），不可悄悄毁掉别人的版本快照。
    """
    snapshots_dir = root / "snapshots"
    snapshots_dir.mkdir(parents=True, exist_ok=True)

    base_id = snapshot["id"]
    for attempt in range(_SNAPSHOT_CREATE_ATTEMPTS):
        snap_id = base_id if attempt == 0 else f"{base_id}_{attempt + 1}"
        path = snapshots_dir / f"{snap_id}.json"
        payload = dict(snapshot)
        payload["id"] = snap_id
        body = json.dumps(payload, ensure_ascii=False, indent=2) + "\n"
        try:
            fd = os.open(path, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o644)
        except FileExistsError:
            continue
        with os.fdopen(fd, "w", encoding="utf-8") as fh:
            fh.write(body)
        return path

    raise RuntimeError(
        f"failed to create snapshot after {_SNAPSHOT_CREATE_ATTEMPTS} attempts: {base_id}"
    )


# ---------------------------------------------------------------------------
# 子命令
# ---------------------------------------------------------------------------


def cmd_bump(args: argparse.Namespace) -> int:
    root: Path = args.root
    targets = [Path(t) for t in args.scripts]
    if not targets:
        print("nothing to bump: no scripts given", file=sys.stderr)
        return 1

    changes: list[tuple[Path, str, str]] = []
    for target in targets:
        path = target if target.is_absolute() else root / target
        if not path.exists():
            print(f"skip(not found): {target}", file=sys.stderr)
            continue
        if path.name == "__init__.py":
            # 包声明文件一律不写 __version__（AGENTS.md 的版本管理条）⇒ 跳过并提示，
            # 而不是报错 —— 报错会让「批量 bump src/**/*.py」这种常见调用直接失败。
            print(f"skip(__init__.py, 无 __version__): {target}")
            continue
        old = read_version(path)
        new = bump_semver(old, args.type)
        changes.append((path, old, new))

    if not changes:
        print("nothing to bump: all targets missing", file=sys.stderr)
        return 1

    for path, old, new in changes:
        rel = path.relative_to(root) if path.is_relative_to(root) else path
        print(f"{rel}: {old} -> {new}")

    if args.dry_run:
        print("[dry-run] no files written")
        return 0

    for path, _old, new in changes:
        write_version(path, new)

    components = collect_components(root)
    snapshot = {
        "id": next_snapshot_id(root),
        "created_at": datetime.datetime.now().isoformat(timespec="seconds"),
        "bump_type": args.type,
        "changed": {
            str(p.relative_to(root) if p.is_relative_to(root) else p): new
            for p, _o, new in changes
        },
        "components": components,
    }
    path = write_snapshot(root, snapshot)
    print(f"snapshot: {path.relative_to(root)}")
    return 0


def cmd_snapshot(args: argparse.Namespace) -> int:
    """只生成一份组件版本快照，**不改任何版本号**。

    ⭐ 为什么要有这个子命令：bump 会改版本号，而「先记一份当前基线」这件事
    本身不涉及任何版本变化 —— 强行用 bump 达成，等于为了留档而改版本号。
    ⇒ 版本号会变成「记录动作的副作用」，而不是「代码状态的真实描述」。
    """
    root: Path = args.root
    components = collect_components(root)
    if not components:
        print("no versioned components found — nothing to snapshot", file=sys.stderr)
        return 1
    snapshot = {
        "id": next_snapshot_id(root),
        "created_at": datetime.datetime.now().isoformat(timespec="seconds"),
        "bump_type": "snapshot-only",
        "changed": {},
        "components": components,
    }
    path = write_snapshot(root, snapshot)
    print(f"snapshot: {path.relative_to(root)}")
    for key in sorted(components):
        print(f"  {key}: {components[key]}")
    return 0


def cmd_list(args: argparse.Namespace) -> int:
    components = collect_components(args.root)
    if not components:
        print("(no versioned components found)")
        return 0
    for key in sorted(components):
        print(f"{key}: {components[key]}")
    return 0


def cmd_rollback(args: argparse.Namespace) -> int:
    root: Path = args.root
    snapshots_dir = root / "snapshots"
    snaps = sorted(
        (
            p
            for p in snapshots_dir.glob("*.json")
            if parse_snap_id(p.name)[:3] != (0, 0, 0)
        ),
        key=lambda p: parse_snap_id(p.name),
    )
    if len(snaps) < 1:
        print("nothing to roll back: no snapshots found", file=sys.stderr)
        return 1

    current = snaps[-1]
    if len(snaps) < 2:
        print(
            "only one snapshot exists — rolling back to it would restore the same state",
            file=sys.stderr,
        )
        return 1

    target = snaps[-2]
    data = json.loads(target.read_text(encoding="utf-8"))
    components = data.get("components", {})

    for key, ver in components.items():
        path = root / (key.replace(".", "/") + ".py")
        if not path.exists():
            print(f"skip(missing): {path.relative_to(root)}")
            continue
        try:
            old = read_version(path)
        except (ValueError, SyntaxError):
            print(f"skip(no __version__): {path.relative_to(root)}")
            continue
        if old == ver:
            continue
        write_version(path, ver)
        print(f"{path.relative_to(root)}: {old} -> {ver}")

    current.unlink()
    print(f"rolled back to {target.name}; removed {current.name}")
    return 0


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="bump_version.py",
        description="脚本版本 bump + 组件版本快照 CLI（双轨制）",
    )
    parser.add_argument(
        "--root",
        type=Path,
        default=Path(__file__).resolve().parent,
        help="项目根目录（默认：本脚本所在目录）",
    )
    sub = parser.add_subparsers(dest="command", required=True)

    p_bump = sub.add_parser("bump", help="bump 指定脚本的版本号并生成快照")
    p_bump.add_argument("scripts", nargs="+", help="要 bump 的脚本路径（相对项目根）")
    p_bump.add_argument(
        "--type",
        choices=["patch", "minor", "major"],
        default="patch",
        help="递增类型（默认 patch）",
    )
    p_bump.add_argument("--dry-run", action="store_true", help="只预览不写文件")
    p_bump.set_defaults(func=cmd_bump)

    p_list = sub.add_parser("list", help="列出当前所有组件版本")
    p_list.set_defaults(func=cmd_list)

    p_snapshot = sub.add_parser("snapshot", help="只生成组件版本快照，不改任何版本号")
    p_snapshot.set_defaults(func=cmd_snapshot)

    p_rollback = sub.add_parser("rollback", help="回滚到上一个快照")
    p_rollback.set_defaults(func=cmd_rollback)

    return parser


def main() -> int:
    parser = build_parser()
    args = parser.parse_args()
    root: Path = args.root.resolve()
    args.root = root
    return args.func(args)


if __name__ == "__main__":
    raise SystemExit(main())
