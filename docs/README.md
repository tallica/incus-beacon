# Docs

Each doc owns a topic and starts with a one-paragraph statement of its scope.
Which file owns which kind of fact is mapped in [AGENTS.md](../AGENTS.md).

## Writing

- Prose wraps at 80 columns; `make fmt` rewraps whole paragraphs with Prettier.
  Tables, headings, code blocks, frontmatter and long URLs are exempt.
- Relative Markdown links (`[text](path.md#anchor)`), so they work on GitHub and
  in any editor.
- Current state, not history: history is in git and CHANGELOG.md.
- Runbooks follow Symptom, Cause, Fix, Verify, in that order.

## Index

| Doc                                          | Covers                                        |
| -------------------------------------------- | --------------------------------------------- |
| [ARCHITECTURE.md](ARCHITECTURE.md)           | Principles, components, key flows, invariants |
| [DEVELOPMENT.md](DEVELOPMENT.md)             | Prerequisites, layout, make targets, testing  |
| [RELEASING.md](RELEASING.md)                 | Release procedure and downstream steps        |
| [runbooks/](runbooks/)                       | One file per failure mode                     |
| [templates/handoff.md](templates/handoff.md) | Session handoff template                      |
