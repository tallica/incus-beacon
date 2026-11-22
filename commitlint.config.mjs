// Conventional Commits; types and scopes are documented in CONTRIBUTING.md.
export default {
  extends: ["@commitlint/config-conventional"],
  // Dependabot writes "build(deps): Bump ..." with a capital B, which
  // subject-case refuses; its commits and PR titles are not ours to fix.
  ignores: [(message) => /^build\(deps(-dev)?\): Bump /.test(message)],
  rules: {
    "body-max-line-length": [0],
    // Once the scopes settle, enforce CONTRIBUTING.md's scope table with
    // "scope-enum"; doc-named scopes such as readme are in use too.
  },
};
