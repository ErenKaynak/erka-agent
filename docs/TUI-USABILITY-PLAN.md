# TUI usability follow-up

This local-only checkpoint addresses the post-M2 interactive feedback.

## Delivery slices

1. Render help and confirmation overlays inside the current terminal viewport,
   including small terminal sizes, instead of appending them below the screen.
2. Add local slash commands for help, conversations, opening a task, starting a
   conversation, diff, permissions, cancellation, status, and exit.
3. Show matching slash-command hints while typing and support Tab completion.
4. Prove commands never become model prompts, preserve confirmation boundaries,
   and remain deterministic at narrow, medium, and wide terminal sizes.
5. Update the public README and push each verified checkpoint without tracking
   this file or any other document under `docs/`.

## Publishing checkpoints

- `fix(tui): keep overlays inside the terminal viewport`
- `feat(tui): add local slash commands`
- `docs: document TUI slash commands`

## Completion

Completed on 2026-09-03 in commits `2ac14e0`, `c0ec32a`, and `bf554bf`.
GitHub Actions run `33747527515` passed the full platform, Go-version, vet,
formatting, build, and race matrix. This document remained local and ignored.
