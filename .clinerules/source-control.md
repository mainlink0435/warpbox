# Source Control & Git Rules

1. **Automatic Commits:** Automatically stage and commit changes to Git after successfully implementing and testing a requested feature, updating documentation, or resolving a bug.
2. **Conventional Commits:** Always use standard conventional commit messages for all commits (e.g., `feat:`, `fix:`, `refactor:`, `docs:`, `chore:`).
3. **Commit Pacing:** Do not bundle massive, unrelated changes into a single commit. Commit logically separated units of work immediately after they are verified.
4. **Branching Strategy:** Do not create new branches automatically. Commit all changes directly to the current working branch. However, you should suggest creating a new branch if a requested feature is complex, experimental, or risky. Always wait for explicit user approval before creating or switching branches.
5. **Version Tagging:** After completing a meaningful batch of feature work or a bug-fix release, tag the current commit with a semantic version (e.g. `git tag v0.1.0 && git push origin v0.1.0`). Use `vMAJOR.MINOR.PATCH` — bump MAJOR for breaking changes, MINOR for new features, PATCH for bug fixes. The CI pipeline automatically builds binaries and Docker images when a tag is pushed.
6. **Never guess a version number.** Before tagging, you MUST:
   - Run `git tag --list "v*" --sort=-version:refname` to list all existing tags sorted by version.
   - Filter out any `-test`, `-ci`, or other pre-release suffix tags (e.g. `v0.5.0-test` does NOT count as a release).
   - Take the highest version tag that has NO suffix (e.g. `v0.1.0`, not `v0.5.0-test`).
   - Increment from that tag: PATCH for bug fixes, MINOR for features, MAJOR for breaking changes.
   - Present the intended tag to the user for confirmation before pushing. Do not push version tags autonomously.
7. **Issue Autoclose:** Gitea (v1.24.x) does **not** support auto-closing issues via commit message keywords like `Closes #N`, `Fixes #N`, or `Resolves #N`. After pushing a commit that resolves an issue, you MUST use the `gitea-mcp` `issue_write` tool (method: `update`, state: `closed`) to close the issue manually. Include the issue number in the commit message for documentation purposes, but do not rely on it closing automatically.
