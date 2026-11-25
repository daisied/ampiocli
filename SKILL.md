---
name: ampiocli
description: Change an Ampio smart-home installation (M-SERV app configuration, CAN-module logic, Node-RED flows) by editing JSON files with the ampiocli CLI. Use when asked to rename or add objects, change what a switch or sensor does, edit module rules, or change Node-RED flows on an Ampio M-SERV.
---

# ampiocli

`ampiocli` keeps an Ampio installation as a folder of JSON files: `pull`, edit, `diff`, `push`. Read the README for how it works. This file is how to use it safely.

## Setup (once)

```
go install github.com/daisied/ampiocli@latest
```

The working copy is a folder with a `.env` holding `AMPIO_HOST` and `AMPIO_PASSWORD`. If there is none, ask the person for the M-SERV address and admin password, and write them into `.env` yourself. Never print the password or commit `.env`.

## Rules

1. **Never run `push --yes` without the person's explicit go for that exact diff.** This drives real lights, blinds, locks and ventilation in someone's home. Show them the `ampiocli push` output (the dry run), explain it in plain words, and wait.
2. Always `ampiocli pull` before editing, and edit only after it succeeds.
3. Change only what was asked. Keep every other field of a row or rule exactly as it is.
4. Never hand-edit `.ampiocli/`, `blobs/` or `devices.json`. `devices.json` is reference only, and edits to it are ignored.
5. If a check refuses, fix the edit. Don't work around a check, and don't write to the M-SERV by any other means (MQTT, HTTP, Designer).

## Workflow

```
ampiocli pull                 # live state → files (refuses if there are unpushed edits)
# edit files
ampiocli diff                 # what you changed, offline
ampiocli push                 # dry run: conflict check, validation, codec checks; writes nothing
# person says go
ampiocli push --yes           # backup, write, read back, verify
```

`--only config,logic,nodered` limits pull, diff and push. `ampiocli pull --force` throws local edits away and starts over.

## Finding things

- **Objects** (anything the app shows: lights, blinds, sensors, buttons): `config/objects.json`. `opis_menu` is the label. `lokalizacja` is the location id (`config/locations.json`). `leafId` ties the object to a module output or input.
- **Groups** and their members: `config/groups.json`, `config/group_objects.json` (`id_grupy` → group, `id_obiektu` → object).
- **Module rules:** `logic/<mac>.json`, where `<mac>` is the module's hex address. Rules refer to modules by `srcMac` (decimal) or `srcUniqMac` / `leafId` (`0_<mac hex>_<sf>_<subsf>_<n>`). To find which rules involve a module, grep for its hex mac across `logic/`.
- **Node-RED:** one file per tab in `nodered/`. Each node has a unique `id`, and `z` is its tab.
- `grep -rl "Kitchen" config logic nodered` is usually the fastest way in.

## Editing

### Config rows

- To change a row, edit fields in place.
- To add a row, copy a similar row and give it a placeholder id of its own, `"id": "@kitchen_spot"`. Use the same placeholder in the id column of any other new or changed row that must point at it (`id_obiektu`, `id_grupy`, `id_rodzica`, `lokalizacja`, ...; for example `"id_obiektu": "@kitchen_spot"` in `group_objects.json`). Push turns it into the next free id.
- Rows in `objects`, `groups`, `resources`, `flag_conditions` and `data` cannot be deleted. To hide an object or group, set bit 16 in `params` (`params | 16`). Rows in `group_objects`, `connected_objects` and `locations` can be removed by deleting them from the file.
- `conditions` and `scenes` are read-only.

### Module rules

- To write a new rule, copy an existing rule of the same kind (same trigger and action type) from any module and change the values. Don't invent fields.
- To turn a rule off, use `"disabled": true`.
- Don't change `name` or `protocol` in a logic file. Only `rules` is written.
- Push checks that the module's codec reproduces its current bytes, and that your rules survive encoding. If it says a field "does not survive encoding", that field is unknown or out of range. Compare with a working rule.
- Rule descriptions live in `config/data.json`, keyed by the rule's hash. Changing a rule's trigger or action detaches its description. Mention this to the person.

### Node-RED

- Edit node fields in place. A new node needs a unique `id` (16 hex characters) and a `z` pointing to its tab, and the wires of the nodes around it must be updated.
- Credentials are never in the files. Nodes keep theirs. A new node that needs credentials must get them in the Node-RED editor.

## When push fails

| Message | Meaning, and what to do |
|---|---|
| `changed on the M-SERV since your pull: …` | Someone changed it in Designer or the app. Nothing was written. Note your edit, run `pull --force`, redo the edit, and check again. |
| `… nothing written` | A check refused before any write. Fix the edit. |
| `the M-SERV refused the … save`, `no answer on …` | The push stopped part-way. Tell the person. Once they say go, run `push --yes` again: it skips what was already written. |
| `written, but … reads back differently` / `… is missing` | The M-SERV took the write but stored something else. Stop, and show the person the message and the backup folder. |
| `module … refused …` | The module refused the write. Stop, and tell the person. |

To undo a push, run `ampiocli backups`, then `ampiocli restore <name>`. Show the `ampiocli push` dry run to the person, then run `push --yes` once they say go. A restore can't delete rows the push created in non-removable tables. Hide those with the DELETED bit instead.
