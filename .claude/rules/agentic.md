---
paths:
  - ".claude/**"
---

# How the agent-side pieces fit

The engineer states a goal and watches; Claude leads. Each piece has one
job:

| piece | job | where |
|---|---|---|
| `CLAUDE.md`, `AGENTS.md` | what every session knows | repo root |
| skills | procedures the lead follows: `aoc` (the loop), `ab-test`, `experiment`, `investigate`, `status`, `stop` | `.claude/skills/<name>/SKILL.md` |
| agents | work the lead delegates to keep its own context for the loop: `investigator`, `code-reader`, `notebook-author` | `.claude/agents/<name>.md` |
| hooks | rules enforced, not just stated: the session briefing, one run at a time, launches through the UI, no whole-file reads, settings files guarded | `.claude/hooks/*.sh`, wired in `.claude/settings.json` |
| rules | context that loads when a path is opened | `.claude/rules/*.md` |

Writing them:

- A skill is a procedure with tables, not an essay: what they say → the
  flag or command. Its `description` is the trigger; name the phrases an
  engineer uses. Under 200 lines.
- An agent's body is what the lead cannot say in a prompt each time: what
  to read first, the budget, what to return and in which shape. Restrict
  tools (`tools:` or `disallowedTools:`) to what the job needs; agents that
  explain do not edit.
- A hook decides, it does not advise: `deny` with the exact substitution
  the agent should make, `ask` when the human owns the decision, silence
  otherwise. Keep each check a regex a reader can verify, test it with a
  sample tool call on stdin, run `shellcheck`.
- Every rule stated in a skill that a hook can enforce gets a hook; every
  hook has the matching sentence in the skill so the agent knows why it was
  refused.
- No slop: no narrating comments, no emoji, no restating the code.
