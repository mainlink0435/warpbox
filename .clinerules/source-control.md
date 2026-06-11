# Source Control & Git Rules

1. **Automatic Commits:** Automatically stage and commit changes to Git after successfully implementing and testing a requested feature, updating documentation, or resolving a bug.
2. **Conventional Commits:** Always use standard conventional commit messages for all commits (e.g., `feat:`, `fix:`, `refactor:`, `docs:`, `chore:`).
3. **Commit Pacing:** Do not bundle massive, unrelated changes into a single commit. Commit logically separated units of work immediately after they are verified.
4. **Branching Strategy:** Do not create new branches automatically. Commit all changes directly to the current working branch. However, you should suggest creating a new branch if a requested feature is complex, experimental, or risky. Always wait for explicit user approval before creating or switching branches.
5. **Version Tagging:** After completing a meaningful batch of feature work or a bug-fix release, tag the current commit with a semantic version (e.g. `git tag v0.1.0; git push origin v0.1.0`). Use `vMAJOR.MINOR.PATCH` — bump MAJOR for breaking changes, MINOR for new features, PATCH for bug fixes. The Gitea Actions pipeline automatically builds binaries and Docker images when a tag is pushed.
6. **Never guess a version number.** Before tagging, you MUST:
   - Run `git tag --list "v*" --sort=-version:refname` to list all existing tags sorted by version.
   - Filter out any `-test`, `-ci`, or other pre-release suffix tags (e.g. `v0.5.0-test` does NOT count as a release).
   - Take the highest version tag that has NO suffix (e.g. `v0.1.0`, not `v0.5.0-test`).
   - Increment from that tag: PATCH for bug fixes, MINOR for features, MAJOR for breaking changes.
   - Present the intended tag to the user for confirmation before pushing. Do not push version tags autonomously.
7. **Issue Lifecycle (Kanban Workflow):** All issues flow through the **"Warpbox Kanban"** project board:
   📥 Backlog → 🧠 Research/Spikes → 📋 Ready to Dev → 🚧 In Progress → 👀 Review/QA → ✅ Done

   - Gitea does **not** support auto-closing via commit message keywords. After pushing a commit that resolves an issue, you MUST:
     1. Move the issue to ✅ Done on the Kanban board (see system-patterns.md §8 for board operations).
     2. Use the `gitea-unified` `issue_write` tool (method: `update`, state: `closed`) to close it.
   - Include the issue number in the commit message for documentation purposes (e.g., `fix: handle CDN expiry, refs #28`), but do not rely on keywords to close it.
