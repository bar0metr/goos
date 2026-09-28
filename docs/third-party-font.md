# Built-in framebuffer font provenance

GoOS embeds selected glyphs from Spleen 12x24 version 2.2.0 at upstream
commit `57f9219328c9f5873085320fe8bc8f7dd34b8791`:

<https://github.com/fcambus/spleen>

Spleen is Copyright (c) 2018-2026, Frederic Cambus and is distributed under
the BSD 2-Clause license. GoOS stores the generated bitmap table in
`kernel/framebuffer/font_data.go`. The embedded subset contains printable
ASCII, Russian Cyrillic letters including `Ё` and `ё`, the upstream white
square glyph used as the visible replacement character, and the light
box-drawing glyphs `─ │ ┌ ┐ └ ┘ ├ ┤ ┬ ┴ ┼` (U+2500, U+2502, U+250C, U+2510,
U+2514, U+2518, U+251C, U+2524, U+252C, U+2534, U+253C) that full-screen
programs draw frames with.

`tools/fontgen` generates the table from the upstream `spleen-12x24.bdf` at the
commit above:

```sh
go run ./tools/fontgen -input spleen-12x24.bdf \
    -output kernel/framebuffer/font_data.go \
    -commit 57f9219328c9f5873085320fe8bc8f7dd34b8791
```

No glyph is drawn by hand.

## BSD 2-Clause License

Copyright (c) 2018-2026, Frederic Cambus

All rights reserved.

Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are met:

1. Redistributions of source code must retain the above copyright notice,
   this list of conditions and the following disclaimer.
2. Redistributions in binary form must reproduce the above copyright notice,
   this list of conditions and the following disclaimer in the documentation
   and/or other materials provided with the distribution.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS"
AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE
IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE
ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT HOLDER OR CONTRIBUTORS BE
LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR
CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF
SUBSTITUTE GOODS OR SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS
INTERRUPTION) HOWEVER CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN
CONTRACT, STRICT LIABILITY, OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE)
ARISING IN ANY WAY OUT OF THE USE OF THIS SOFTWARE, EVEN IF ADVISED OF THE
POSSIBILITY OF SUCH DAMAGE.
