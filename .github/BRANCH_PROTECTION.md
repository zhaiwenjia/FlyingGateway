# 分支保护配置指南

> 规则依据：根目录 [`AGENTS.md`](../AGENTS.md)。本文件是「**将来升 Pro 后怎么配**」的说明，
> **不是「现在生效的配置」**（见第 0 节）。

## 0. ⭐ 本仓现在**没有**分支保护（先看这条再往下）

- 本仓是**私有仓**且在 **GitHub 免费版** ⇒ 分支保护 API 不可用
  （`gh api` 返回 403 "Upgrade to GitHub Pro"）。
- ⇒ 在此之前，`main` 的保护靠**流程**而非 GitHub 强制：
  - `main` **禁止直接 push**（AGENTS.md 的提交条），**唯一例外**是纯文档改动
    （只改 `progress/` / `AGENTS.md` / `README.md` / `docs/` / `bump_version.py`）；
  - 改 `src/` `test/` `main.py` `pyproject.toml` `uv.lock` `.github/workflows/`
    `.gitignore` ⇒ **一律走 PR**，由 PR 评审兜住；
  - 助手只负责 `commit` 与 `push`，**不开 PR、不合并**（需老宅逐次明确授权）。
- ⚠️ **本仓 `ci.yml` 没有 `push` 触发器**（只有 `pull_request` 与 `workflow_dispatch`）
  ⇒ 推分支**不会**起 run、不烧 Actions 分钟。

## 1. `main` 分支保护（升 Pro 后按这张表配）

前往 GitHub → `Settings` → `Branches` → `Add rule` / `Edit` `main`:

| 选项 | 推荐值 | 备注 |
|------|--------|------|
| Branch name pattern | `main` | |
| **Protect matching branches** | ✅ | |
| Require a pull request before merging | ✅ | 禁止直接 push |
| Require approvals | ✅ | **至少 1 个** |
| Dismiss stale pull request approvals when new commits are pushed | ✅ | 新提交需重新 review |
| Require review from Code Owners | ✅ | 见 `.github/CODEOWNERS` |
| Require status checks to pass before merging | ✅ | 见下表 |
| Require branches to be up to date before merging | ✅ | |
| Require conversation resolution before merging | ✅ | 未解决的 review 讨论不能 merge |
| Require signed commits | ❌ | 可选，合规要求时开启 |
| Require linear history | ❌ | 本仓用 squash 合并，**不要**开 |
| Include administrators | ✅ | admin 也要走 PR |
| Allow force pushes | ❌ | 严禁 force push |
| Allow deletions | ❌ | |

### 必需的 status checks（**只有两个**，名字与 `ci.yml` 的 `name:` 逐字一致）

| Status check | ci.yml 位置 | 失败时阻止合并? |
|---|---|---|
| `Lint (ruff + semver)` | `ci.yml` 的 `lint` job | ✅ |
| `Unit tests (unittest)` | `ci.yml` 的 `unit-test` job | ✅ |

> ⚠️ **名字必须逐字照抄上面两行** —— GitHub 按 job 的 `name:` 匹配，不是按 job id。
> 写成 "Unit tests (pytest)" 之类会要求**永远不会上报的 check** ⇒ PR 永远卡住。

## 2. 本仓的分支形态（**没有** `develop`，**没有** `archive/*`）

- 唯一长期分支：`main`。
- 其余全是**特性分支**，命名规则见 AGENTS.md 的命名条：**`年月日-分支目的`**
  （如 `20261111-router-降级链排序`）。
- ⇒ 不设 `develop` 分支保护，也不设 `archive/*` 只读分支 —— 本仓没有这两个分支，
  配了也只是空规则。
- `archive/*` 若将来真的引入，再按「只读：禁止 push / 禁止 force push / 禁止删除」补一节。

## 3. 推荐的 PR 模板

`.github/pull_request_template.md` 已自动启用（内容以该文件为准）：

```markdown
## 变更说明
### 修改的脚本与版本变更（双轨制）
| 脚本 | 修改前版本 | 修改后版本 | 变更类型 | 原因 |
## 测试
- [ ] 已运行 `uv run python -m unittest discover -t . -s test -v` 且全部通过
## CI 检查
- [ ] `Lint (ruff + semver)` 通过
- [ ] `Unit tests (unittest)` 通过
```

> ⚠️ 测试命令是 **`unittest`**、目录是 **`test/`（不带 s）** ⇒
> **不要**写 `pytest tests/...`：本仓既没装 pytest，也没有 `tests/` 目录。

## 4. 检查清单

每次新仓库初始化时，按此清单逐项勾选：

- [ ] `main` 分支保护规则已应用（`Lint` + `Unit tests` 两个 required status checks）
- [ ] `.github/CODEOWNERS` 已创建（路径与本仓真实布局一致）
- [ ] `.github/pull_request_template.md` 已存在且测试命令是 unittest 口径
- [ ] `Settings → General → Pull Requests` 启用 "Allow squash merging"
- [ ] `Settings → Code security and analysis` 启用 Dependabot / Code scanning（可选）

## 5. 紧急修复流程（hotfix）

AGENTS.md 的分支命名是**强制**的 ⇒ hotfix 也走同一套命名
（`YYYYMMDD-hotfix-简述`），不另开 `hotfix/*` 命名空间：

```bash
# 从 main 拉特性分支（工作分支必须是 origin/main 的后代）
git fetch origin
git checkout -b 20261111-hotfix-简述 origin/main
# ... 修复 + 自测 ...
# 提交信息用 heredoc，别把多行说明塞进 -m
git commit -F -
```

- 仍然走 PR（`main` 禁直接 push，AGENTS.md 的提交条）。
- PR 标题前缀：`hotfix:` 便于 changelog 分类。
- PR 描述必须包含 incident ID / 影响范围 / 复现步骤。
