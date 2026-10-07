---
name: future_skill
description: A skill that uses fields opskeeper does not yet recognize.
custom_field_one: hello
custom_field_two:
  nested:
    value: 42
upstream_only_array:
  - a
  - b
---

# Future Skill

Newer SKILL.md spec adds fields opskeeper hasn't modeled yet — they should
survive into UnknownFields.
