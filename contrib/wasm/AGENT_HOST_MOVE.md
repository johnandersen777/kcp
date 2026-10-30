# Moving a running Claude Code agent to another host

How the `wasm` agent moved itself from the Mac to hat-8 on 2026-10-30
(owner's request at atirc seq 2257) without losing its conversation, its
worktrees, its atirc position or the public dev URL. The same steps work for
any agent whose state lives in a repo tree plus a Claude Code session.

What has to move, and where it lives:

| State | Source (Mac) | Target (hat-8) |
| --- | --- | --- |
| Repo + worktrees + local commits | `~/src/kcp/wasm` | `~/src/kcp/kcp-wasm-mac` (new dir; the lead's `~/src/kcp/kcp-wasm` is never touched) |
| Session transcript, subagent transcripts, memory | `~/.claude/projects/-Users-johnandersen777-src-kcp-wasm/` | `~/.claude/projects/-home-johnandersen777-src-kcp-kcp-wasm-mac/` |
| atirc token + cursor | session scratchpad | `~/.tmp/wasm-scratch/` (0600) |
| Background jobs (dev server :8090, fedproxy tunnel, atirc listener) | Mac | restarted on the target by the resumed session |

The project directory under `~/.claude/projects/` is the working directory
with every `/` turned into `-`. A different path on the target means a
different project directory: copy the files there, or `claude --resume` will
not find the session.

## 1. Check the target and agree on paths

```sh
ssh linux 'hostname; df -h ~ | tail -1; ls ~/src/kcp; which claude atirc deno go tmux;
  claude --version; ss -ltn | grep -E ":(8090|8092) "; ls ~/.tmp/claude-kcpwasm/*.sh;
  ssh -p 1997 root@socialweb.computer true && echo prod-ssh-ok'
```

- Enough disk, the same tools, a free :8090 and working prod ssh.
- Ask the lead on the channel which directory to use. Answer: a new
  `~/src/kcp/kcp-wasm-mac`, because the lead's tree has untracked work.
- Post a status line before starting: "moving now, ~2 min, then the Mac
  process exits".

## 2. Stream the tree over ssh (no temporary tarball)

```sh
cd ~/src/kcp/wasm
export COPYFILE_DISABLE=1          # macOS: no ._ AppleDouble files
ssh linux 'test ! -e ~/src/kcp/kcp-wasm-mac && mkdir -p ~/src/kcp/kcp-wasm-mac'   # refuse to overwrite
tar --no-xattrs -czf - \
    --exclude='.claude/worktrees/*/.work' --exclude='.claude/worktrees/*/bin' . \
  | ssh linux 'tar xzf - -C ~/src/kcp/kcp-wasm-mac'
# the worktree in active use keeps its build output (saves a rebuild)
tar --no-xattrs -czf - .claude/worktrees/a8-page/.work .claude/worktrees/a8-page/bin \
  | ssh linux 'tar xzf - -C ~/src/kcp/kcp-wasm-mac'
```

- Leaving out every other worktree's `.work`/`bin` (18 GB) brought the copy down to 4.3 GB.
- `--no-xattrs` plus `COPYFILE_DISABLE` stops GNU tar on Linux from warning
  "Ignoring unknown extended header" for every file.
- `.git` comes along, so local unpushed commits move with it.

Repair the worktrees, whose `.git` files still hold Mac paths:

```sh
ssh linux 'cd ~/src/kcp/kcp-wasm-mac && git worktree repair .claude/worktrees/* && git worktree list | head'
```

## 3. Copy the session, memory and side state

```sh
P=-home-johnandersen777-src-kcp-kcp-wasm-mac
SRC=~/.claude/projects/-Users-johnandersen777-src-kcp-wasm
ssh linux "test ! -e ~/.claude/projects/$P && mkdir -p ~/.claude/projects/$P ~/.tmp/wasm-scratch && chmod 700 ~/.tmp/wasm-scratch"
COPYFILE_DISABLE=1 tar --no-xattrs -czf - -C "$SRC" . | ssh linux "tar xzf - -C ~/.claude/projects/$P"
tar --no-xattrs -czf - -C "$SCRATCH" wasm-bob.tok wasm.seq \
  | ssh linux 'tar xzf - -C ~/.tmp/wasm-scratch; chmod 600 ~/.tmp/wasm-scratch/*'
```

This copies every session transcript (`<id>.jsonl`), its subagent
directories and `memory/` together. Credentials go into files with mode 0600
and never into a command line or a channel message.

## 4. Write a handoff prompt

The resumed session has its whole transcript, but some facts changed with
the move, and the first prompt is where to state them. Write them to a file
on the target (`~/.tmp/wasm-scratch/handoff.txt`):

- the new paths: repo, worktree in use and its commit, token and cursor files;
- that the Mac dev server and tunnel are stopped, and the exact commands to
  restart them here (check that :8090 is free first);
- the first action: post "moved to hat-8, Mac process stopped, listening";
- the pending work and its gates (here: rc127 on hold for review fixes,
  rebuild, test, deploy only with the lead's OK);
- that subagents from the old host are gone, plus the standing rules (no
  GitHub pushes, `~/.tmp` not `/tmp`, never print credentials).

## 5. Stop the old host's side effects, then start the new session

Order matters: stop everything that consumes or serves before the new copy
starts, so there is never a double atirc consumer or two tunnels for one
hostname.

1. On the Mac, stop the background jobs: tunnel loop, :8090 dev server and
   atirc listener (`TaskStop` on each).
2. Copy the final atirc cursor and the final session transcript once more:
   both changed while steps 2-4 ran.
3. Start the resumed session in a detached tmux session on the target:

```sh
ssh linux 'tmux has-session -t wasm 2>/dev/null && echo EXISTS || {
  cd ~/src/kcp/kcp-wasm-mac && tmux new-session -d -s wasm -c ~/src/kcp/kcp-wasm-mac \
    "export TMPDIR=~/.tmp; claude --resume <session-id> --dangerously-skip-permissions \"\$(cat ~/.tmp/wasm-scratch/handoff.txt)\"; exec bash"; }'
```

- `-c` sets the working directory, which selects the project directory from
  step 3.
- `exec bash` keeps the tmux window open if claude exits.
- `tmux has-session` stops a second copy from starting.
- Attach later with `ssh linux -t tmux attach -t wasm`.

Starting the new session ends the old one. Its last tool call (the ssh
above) shows as interrupted in the transcript, which is expected.

## 6. First minutes on the target (done by the resumed session)

1. Post on the channel: moved, Mac process stopped, listening.
2. Re-arm the atirc listener with the new token and cursor paths. It resumes
   from the copied cursor, so it misses no message.
3. Check that nobody serves :8090, then start the dev server from the new
   tree, with debug files in their own directory (`~/.tmp/claude-kcpwasm-mac`).
4. Start the tunnel takeover loop and the watchdog. The takeover waits until
   the public URL has failed twice, then dials; it served 200 again about
   2 minutes later.
5. Save a memory note with the new paths, because the repo's AGENTS.md still
   names the lead's paths.
6. Spawn new subagents for dev work as needed.

## What went right, and what to watch

- No state was rebuilt: worktrees, local commits, build output, the
  conversation and the atirc position all carried over.
- Stopping the Mac jobs before starting hat-8 avoided duplicate listeners
  and fighting tunnels.
- The copy goes into a new directory. `test ! -e` before `mkdir` refuses to
  overwrite anything.
- The dev server and the production build differ (the old host's dev server
  served the raw `kcp.wasm`, production only `.gz`). Re-check such
  assumptions on the new host rather than carrying them over.
- Paths in the old transcript (for example `/private/tmp/...` scratchpads)
  are stale after the move. The handoff prompt is where to correct them.
