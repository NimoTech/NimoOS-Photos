#!/usr/bin/env bash
# sync-to-public.sh — 把 DEV-NimoOS-Photos(私有开发库) 的内容同步到开源库 NimoTech/NimoOS-Photos 并开 PR
#
# 用法:
#   ./sync-to-public.sh merge [--branch <name>]             # 全量同步: 把 origin/main 整个合并进开源库
#   ./sync-to-public.sh pick <commit>... [--branch <name>]  # 部分同步: 只挑指定的提交 (cherry-pick)
#
# 分支命名: 默认按内容取名 —— pick 模式从首个 commit 的 conventional-commit 标题推导
# (如 fix/trash-cross-device...), merge 模式用 chore/public-sync-<日期>; 可用 --branch 显式指定。
#
# 流程: 基于 public/main 建分支 -> 合并/挑拣 -> 消毒+泄露检查 -> 展示改动供人工确认 -> 推到开源库 -> gh 开 PR
# 注意: PR 是开在开源库内部的 (分支 -> main), 因为 DEV 库不是 fork, 无法跨库开 PR。
#
# ⚠️ 开源库公开内容规范 (commit message / PR 标题 / PR 正文一律适用):
#   1. 全英文 —— 不得出现任何非 ASCII 字符(中文注释只留在本脚本/私库里)。
#   2. 不暴露私有开发库的存在 —— 禁止出现 "DEV"、"Sync from"、私库 PR 号 "(#N)"、
#      "sync-to-public" 等字样; 提交要看起来就像直接在开源库上开发的一样。
#   3. PR 标题/正文不用 "Sync ..." 模板, 而是从 commit 的 subject/body 自然生成。
#   脚本会自动: 去掉 pick 进来的 subject 末尾私库 PR 号 "(#N)", 并对整个待公开
#   范围做泄露检查(非 ASCII / DEV 字样 / sync 字样), 检查不过直接终止。
#   merge 模式会把 DEV 提交历史(含本脚本及其中文提交)原样公开, 泄露检查大概率
#   直接拦下 —— 基本只应使用 pick 模式。

set -euo pipefail

PUBLIC_REPO="NimoTech/NimoOS-Photos"
PUBLIC_REMOTE="public"
PUBLIC_URL="git@github.com:NimoTech/NimoOS-Photos.git"

cd "$(dirname "$(readlink -f "$0")")"

# Derive a content-based branch name from a commit's conventional-commit subject:
# "<type>(scope): Some subject" -> "<type>/some-subject" (type falls back to "chore").
derive_branch() {
    local subject type slug
    subject="$(git log -1 --format=%s "$1" | sed -E 's/ \(#[0-9]+\)$//')"
    type="${subject%%[(:]*}"
    [[ "$type" =~ ^(feat|fix|chore|docs|refactor|perf|test|build|ci)$ ]] || type="chore"
    slug="$(printf '%s' "${subject#*:}" | tr '[:upper:]' '[:lower:]' | sed -E 's/[^a-z0-9]+/-/g; s/^-+//; s/-+$//' | cut -c1-40 | sed -E 's/-+$//')"
    printf '%s/%s' "$type" "${slug:-public-sync-$(date +%Y%m%d)}"
}

SYNC_BRANCH=""
ARGS=()
while [[ $# -gt 0 ]]; do
    case "$1" in
        --branch|-b) SYNC_BRANCH="${2:?--branch 需要一个分支名}"; shift 2 ;;
        *) ARGS+=("$1"); shift ;;
    esac
done
MODE="${ARGS[0]:-}"
COMMITS=("${ARGS[@]:1}")

if [[ "$MODE" != "merge" && "$MODE" != "pick" ]]; then
    echo "用法: $0 merge [--branch <name>] | pick <commit>... [--branch <name>]" >&2
    exit 1
fi
if [[ "$MODE" == "pick" && ${#COMMITS[@]} -lt 1 ]]; then
    echo "pick 模式需要至少一个 commit: $0 pick <commit>... [--branch <name>]" >&2
    exit 1
fi
if [[ -z "$SYNC_BRANCH" ]]; then
    if [[ "$MODE" == "pick" ]]; then
        SYNC_BRANCH="$(derive_branch "${COMMITS[0]}")"
    else
        SYNC_BRANCH="chore/public-sync-$(date +%Y%m%d)"
    fi
fi

if [[ -n "$(git status --porcelain)" ]]; then
    echo "工作区不干净, 先提交或 stash 再同步。" >&2
    exit 1
fi

git remote get-url "$PUBLIC_REMOTE" >/dev/null 2>&1 || git remote add "$PUBLIC_REMOTE" "$PUBLIC_URL"
git fetch origin
git fetch "$PUBLIC_REMOTE"

PREV_BRANCH="$(git rev-parse --abbrev-ref HEAD)"
git switch -c "$SYNC_BRANCH" "$PUBLIC_REMOTE/main"

if [[ "$MODE" == "merge" ]]; then
    git merge --no-edit origin/main || {
        echo "合并冲突: 手动解决后 git push $PUBLIC_REMOTE $SYNC_BRANCH 并开 PR, 或 git merge --abort 放弃。" >&2
        exit 1
    }
else
    git cherry-pick "${COMMITS[@]}" || {
        echo "cherry-pick 冲突: 手动解决后 git push $PUBLIC_REMOTE $SYNC_BRANCH 并开 PR, 或 git cherry-pick --abort 放弃。" >&2
        exit 1
    }
    # 消毒: 去掉 subject 末尾的私库 PR 号 "(#N)" (squash 合并自动追加的那个),
    # 让提交看起来像直接在开源库上开发的。author/date/sign-off 原样保留。
    FILTER_BRANCH_SQUELCH_WARNING=1 git filter-branch -f \
        --msg-filter 'sed -E "1s/ \(#[0-9]+\)\$//"' "$PUBLIC_REMOTE/main..HEAD" >/dev/null
fi

# 泄露检查: 待公开范围内所有 commit message 必须全英文(纯 ASCII), 且不得出现
# 暴露私有开发库的字样。检查不过直接终止, 手动 reword 后重跑。
LEAKS="$(git log "$PUBLIC_REMOTE/main..HEAD" --format='%h %B' \
    | LC_ALL=C grep -nE '[^ -~[:space:]]|\bDEV\b|[Ss]ync[- ](from|to)|sync-to-public' || true)"
if [[ -n "$LEAKS" ]]; then
    echo "❌ 泄露检查未通过, 以下 commit message 内容不符合公开规范(非英文或含 DEV/sync 字样):" >&2
    echo "$LEAKS" >&2
    echo "手动 reword 后重跑; 放弃: git switch $PREV_BRANCH && git branch -D $SYNC_BRANCH" >&2
    exit 1
fi

echo
echo "===== 即将公开的提交 ====="
git log "$PUBLIC_REMOTE/main..HEAD" --oneline
echo
echo "===== 文件改动 ====="
git diff "$PUBLIC_REMOTE/main" --stat
echo
read -r -p "确认没有私有内容, 推送到开源库并开 PR? [y/N] " ANSWER
if [[ "$ANSWER" != "y" && "$ANSWER" != "Y" ]]; then
    echo "已取消。分支 $SYNC_BRANCH 保留在本地, 可自行检查后手动推送, 或删除: git switch $PREV_BRANCH && git branch -D $SYNC_BRANCH"
    exit 0
fi

# PR 标题/正文从 commit 自然生成(全英文、无同步痕迹):
# 单 commit -> 标题 = subject, 正文 = commit body(去掉 Signed-off-by);
# 多 commit -> 标题 = 最早那个 commit 的 subject, 正文 = subject 列表。
COMMIT_COUNT="$(git rev-list --count "$PUBLIC_REMOTE/main..HEAD")"
if [[ "$COMMIT_COUNT" -eq 1 ]]; then
    PR_TITLE="$(git log -1 --format=%s HEAD)"
    PR_BODY="$(git log -1 --format=%b HEAD | sed '/^Signed-off-by:/d')"
else
    PR_TITLE="$(git log --reverse --format=%s "$PUBLIC_REMOTE/main..HEAD" | head -1)"
    PR_BODY="$(git log --reverse --format='- %s' "$PUBLIC_REMOTE/main..HEAD")"
fi

git push "$PUBLIC_REMOTE" "$SYNC_BRANCH"
gh pr create --repo "$PUBLIC_REPO" --base main --head "$SYNC_BRANCH" \
    --title "$PR_TITLE" --body "$PR_BODY"

git switch "$PREV_BRANCH"
echo "完成。本地已切回 $PREV_BRANCH。"
