---
name: tjuclaw-library
description: Reads, edits, adds and removes notes and files in the user's TJUClaw knowledge base through a local working copy, the way one works with a git checkout (tjuclaw clone, status, diff, pull, push). Use when the user asks you to organise, write, import or update their TJUClaw notes, folders or files, including spreadsheets (.xlsx).
compatibility: Requires tjuclaw on PATH (curl -fsSL https://tjuclaw-release.zaixi.dev/cli/install.sh | sh) and a signed-in account (tjuclaw login, or TJUCLAW_TOKEN for non-interactive use).
---

# TJUClaw knowledge base with tjuclaw

`tjuclaw` keeps a local working copy of one TJUClaw library. Edit the files
with your normal tools, then push. Every command prints a JSON envelope on
stdout (`{"ok":true,"data":…}` or `{"ok":false,"error":{"id":…}}`) and a
human summary on stderr.

## Sign in

```bash
tjuclaw whoami            # who is signed in; error "signed_out" if nobody
tjuclaw login             # prints a link and a code; the user confirms it in the browser
```

Ask the user to open the link and confirm the code; never try to approve it
yourself. For unattended use the user can provide `TJUCLAW_TOKEN`; never
print it, write it into files, or commit it.

## Work on a library

```bash
tjuclaw libraries                     # id and name of each library
tjuclaw clone "课程笔记" notes          # by name or id, into ./notes
cd notes
# … edit, add, move or delete files …
tjuclaw status                        # what changed since the last sync
tjuclaw diff [path]                   # unified diff of note edits
tjuclaw push --dry-run                # what would be sent, and what cannot be
tjuclaw push                          # send the changes
tjuclaw pull                          # bring in changes made elsewhere
```

Layout of a working copy:

- A folder is a directory. A note is `<title>.md` (Markdown); a note with child
  notes also has a directory of the same name holding them.
- A file keeps its name and bytes (PDF, images, `.xlsx`, …), up to 8 MB.
- Rich-text notes appear as Markdown for reading but are read-only: edits to
  them are reported as skipped, never sent.
- `.tjuclaw/` holds the sync state; never edit or delete it. Hidden files and
  directories are not synced.

Changes:

- A new `.md` file becomes a note; any other new file is uploaded as a file.
  A new directory becomes a folder.
- Moving or renaming a file keeps its identity (detected by identical content);
  do not change content and move in the same step if you want it kept as a move.
- Deleting a file deletes the entry. A folder is deleted only once it is empty
  in the library too, so remote-only content is never removed by accident.
- Spreadsheets: TJUClaw extracts `.xlsx` text for search and for its own Agent;
  to change a sheet, edit the file with a spreadsheet tool and push it.

## Conflicts

Every change carries the version it was based on. If someone changed the same
item in TJUClaw since your last sync, `push` reports a conflict and leaves that
item unsent. Then:

1. `tjuclaw pull` — the remote version is saved under
   `.tjuclaw/conflicts/<path>`; your file is untouched.
2. Merge the two versions into your file.
3. `tjuclaw resolve <path>`, then `tjuclaw push`.

Never resolve by deleting `.tjuclaw` or re-cloning over the user's edits.

## Errors

- `signed_out`, `cli_token_invalid`: ask the user to run `tjuclaw login`.
- `not_a_working_copy`: run inside a cloned directory or pass `-C <dir>`.
- `library_not_found`, `library_name_ambiguous`: list with `tjuclaw libraries`
  and use the id.
- `cli_token_scope_denied`: the token only covers libraries; model keys, MCP
  settings and conversations are not reachable from the CLI.
