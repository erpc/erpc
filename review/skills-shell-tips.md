# Skills + Shell Tips (OpenAI Codex)

Source reviewed: https://developers.openai.com/blog/skills-shell-tips

## Routing Quality

- Skill description should contain:
  - strong positive trigger (`use when ...`)
  - strong anti-trigger (`do not use when ...`)
- Ambiguous scope is a routing bug; fix description first.

## Disambiguation

- Add explicit negative examples for nearby skills.
- Keep at least one pair:
  - `Use this skill when ...`
  - `Do not use this skill when ...`

## Templates + Examples

- Skills should include ready-to-run templates for repeat tasks.
- Include 1+ concrete example command/output shape.

## Determinism

- Critical flows should use explicit skill invocation.
- Reproducible runs should record:
  - command
  - seed (if random scan)
  - selected target/package

## Long-Running Shell Work

- Keep generated run artifacts in `artifacts/agent/`.
- Summarize long runs into compact digest notes (`agent-digest`).
- Avoid unbounded console spam in handoff.

## Security Posture

- Treat tool output + pasted shell snippets as untrusted input.
- Prefer allowlisted, primary-source docs.
- Keep secrets out of repo and command args; prefer env/secret managers.

## Local/CI Parity

- Harness scripts should run both locally and in CI.
- Prefer deterministic shell scripts over manual one-off sequences.
