# Pull Request 模板

> 规则依据：根目录 [`AGENTS.md`](../AGENTS.md)（本仓协作规则的**唯一来源**）。
>
> ⚠️ **本仓测试框架是 `unittest`，不是 pytest；测试目录是 `test/`（不带 `s`）。**
> 模板里若出现 `pytest` 或 `tests/`，都是别的项目搬来没改干净的残留 ⇒ **别照抄**。

## 变更说明

### 修改的脚本与版本变更（双轨制，AGENTS.md 版本管理条）

| 脚本 | 修改前版本 | 修改后版本 | 变更类型 | 原因 |
|------|----------|----------|---------|------|
| src/flying_gateway/router.py | 0.1.0 | 0.1.1 | patch | ... |
| ... | ... | ... | ... | ... |

> ⚠️ **必填**：本次 PR 改动的每个 `src/` 脚本必须独立 bump 自己的 SemVer，
> **未修改的脚本版本号保持不变**。命令：
> `python bump_version.py bump <脚本...> --type <patch|minor|major>`。
> `__init__.py` 例外（包声明文件不写 `__version__`）。

### 缺陷/任务追踪

- Issue 链接:
- 关联实验留档（`result/{实验名}/`）:

## 测试

- [ ] 已运行 `uv run python -m unittest discover -t . -s test -v` 且全部通过
      （单文件先跑：`uv run python -m unittest test.test_xxx -v`）
- [ ] 已添加/更新单元测试覆盖本次变更
- [ ] 已更新文档（如 API/行为变化）

## Checklist

- [ ] 改动仅限声明的脚本，未触动无关模块
- [ ] `__version__` 已 bump（对所有修改的 `src/` 脚本；`__init__.py` 除外）
- [ ] `.env` / `.venv` 未入库（`.gitignore` 同时含 `.venv` 与 `.venv/` 两种形式）
- [ ] 未提交 `__pycache__` / `*.pyc`
- [ ] 如有破坏性变更，已在 description 标注 BREAKING CHANGE
- [ ] 已更新 `snapshots/`（若本次变更涉及组件版本组合快照）

## CI 检查

`ci.yml` 里**只有这两个 job**，名字逐字对应（AGENTS.md 的门禁条：门禁命令与 `ci.yml` 逐字一致）：

- [ ] `Lint (ruff + semver)` 通过
- [ ] `Unit tests (unittest)` 通过

> ⚠️ **合并前请先在本地跑一遍**（本地跑门禁不限、不要问老宅；它不烧 Actions 分钟、不改远端状态）：
> `ruff check src/ test/ main.py` ·
> `ruff format --check src/ test/ main.py` ·
> `uv run python -m unittest discover -t . -s test -v`

---

> Reviewer 请关注：修改脚本的 SemVer bump 是否符合变更范围（AGENTS.md 版本管理条）。
