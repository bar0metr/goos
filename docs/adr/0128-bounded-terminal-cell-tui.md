# ADR 0128: Bounded terminal-cell TUI primitives

## Status

Accepted.

## Context

`userspace/terminal` provides allocation-free ANSI output, input parsing and
the GoOS one-cell UTF-8 width model. The `gfm` file manager built its screen,
dirty-row and dialog composition directly on that layer. The viewer and editor
need the same geometry, clipping and text-viewport rules, but copying the whole
file-manager renderer would produce incompatible edge cases.

## Decision

Add a thin `userspace/tui` package. It knows terminal cells only and writes
solely through `terminal.Output`. A `Screen` uses caller-owned cell and dirty-row
storage; construction rejects invalid dimensions and multiplication overflow.
Drawing clips against checked `Rect` geometry. Flush uses absolute cursor
positions, emits only dirty rows and omits the bottom-right cell because writing
that cell can scroll a terminal.

Semantic roles map through a caller-selected theme to the existing sixteen ANSI
colours. Text uses the same one-cell UTF-8 model as `userspace/terminal`, so
invalid sequences remain bounded replacement cells and no pixel knowledge
enters the package.

The reusable layer contains text, light-or-ASCII borders, status bars, centered
dialogs, list viewports and a text viewport. `TextViewport` scans caller-owned
text in place and supports vertical and horizontal scrolling plus selection and
cursor overlays. It does not retain a line index or allocate substrings.

Do not migrate `gfm` in this phase. Its renderer combines two-panel file-system
layout, entry-size formatting and application-specific dialog state; replacing
that tested code before another consumer exists would increase change risk
rather than remove generic duplication. New viewer/editor work will consume the
TUI package directly, after which shared `gfm` pieces can be evaluated with
concrete duplication evidence.

## Consequences

- A changed steady-state frame allocates no heap memory.
- Every primitive works at 80x24 and clips safely on larger bounded screens.
- Caller storage determines memory use; the package owns no global screen.
- There are no menus, mouse routing, layout engine or retained widget tree.
- Existing `gfm` behavior and rendering remain unchanged.
