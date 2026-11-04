# Rules

Path-scoped rules for Claude. One file per area, each with a `paths:` frontmatter so it loads only when
working on matching files. Keep each short. Put procedures in skills, not here.
Anything that must never be broken belongs in a hook or permission rule, not in advice.
