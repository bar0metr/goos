# Terminal-cell TUI layer

`userspace/tui` is the bounded rendering layer between full-screen applications
and `userspace/terminal` (ADR 0128). It operates in terminal cells, never pixels,
so the same bytes work on COM1 and the framebuffer terminal.

## Storage and frames

Applications provide `[]tui.Cell` sized to columns times rows and one dirty flag
per row. `tui.NewScreen` validates positive dimensions, storage lengths and
integer multiplication. The screen retains desired cells in that storage.
Drawing changes only affected dirty rows; `Flush` positions the cursor
absolutely and writes those rows through `terminal.Output`.

The last cell of the last row is never emitted. Many terminals scroll when that
cell is written, so applications get a stable full-screen surface of
`columns*rows-1` writable cells.

## Geometry and styling

`Rect` uses checked half-open geometry and intersection for clipping. Every
primitive clips to both its rectangle and the screen. Semantic roles select a
pair from a `Theme`; the default theme uses only the existing sixteen ANSI
colours.

Borders use the light box-drawing glyphs supported by GoOS, with an explicit
ASCII fallback. Text width and invalid UTF-8 handling follow
`userspace/terminal`: each accepted or replacement glyph occupies one cell.

## Primitives

- `Screen`, `Rect` and `Text` provide the drawing base.
- `Border` and `StatusBar` provide bounded chrome.
- `Dialog` centers a title/message prompt with optional input and confirmation
  text.
- `ListViewport` keeps a selected item visible without owning the items.
- `TextViewport` maps caller-owned text lines to rows, clips horizontally,
  scrolls, and overlays a selection and cursor.

The package deliberately has no event bus, layout framework, menus, mouse
routing or retained widget graph.
