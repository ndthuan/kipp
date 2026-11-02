# Third-party work in kipp

kipp's binary embeds two files it did not write. They are listed here
with where they come from and the licence they are under. The Go modules kipp is
built from are listed in `go.mod`, each under its own licence. A module
that only tests and the tools under `internal/devtools` use (kin-openapi,
which checks the API description) is in `go.mod` too and never reaches the
binary; a test holds that.

## htmx 2.0.11

`internal/web/static/htmx.min.js` is `dist/htmx.min.js` of the npm package
`htmx.org@2.0.11`, unchanged (a test holds its SHA-256). Home:
https://htmx.org. Licence: Zero-Clause BSD (`0BSD`), as the package's own
`LICENSE` file gives it (its quotation marks are written in ASCII here):

    Zero-Clause BSD
    =============

    Permission to use, copy, modify, and/or distribute this software for
    any purpose with or without fee is hereby granted.

    THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL
    WARRANTIES WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES
    OF MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE
    FOR ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY
    DAMAGES WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN
    AN ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT
    OF OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.

## SecLists password lists

`internal/auth/blocklist.txt` is made from five files of SecLists
(https://github.com/danielmiessler/SecLists) at commit
`21537beae7465102a3ebbedea6a7852a0cbcb3c4`, all under
`Passwords/Common-Credentials/`:

- `100k-most-used-passwords-NCSC.txt`, whole
- `darkweb2017_top-10000.txt`, whole
- `10k-most-common.txt`, whole
- `Pwdb_top-10000000.txt`, its first 1,700,000 lines
- `xato-net-10-million-passwords.txt`, its first 1,700,000 lines

The last two are ordered most common first; the cut-off keeps the file
under 1 MB (861,831 bytes). Their `-1000000` files in the same folder are
the first million lines of each, so they are covered.

Of these lines, with carriage returns removed, it keeps those of
printable ASCII only and of 15 to 256 characters, lower-cased,
deduplicated and sorted bytewise (`LC_ALL=C sort -u`): 48,597 entries. A
line with any other byte is left out because most of them are
mis-encoded text, which no password typed today would match. Licence:
MIT, as the repository's own `LICENSE` file gives it:

    MIT License

    Copyright (c) 2018 Daniel Miessler

    Permission is hereby granted, free of charge, to any person obtaining a copy
    of this software and associated documentation files (the "Software"), to deal
    in the Software without restriction, including without limitation the rights
    to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
    copies of the Software, and to permit persons to whom the Software is
    furnished to do so, subject to the following conditions:

    The above copyright notice and this permission notice shall be included in all
    copies or substantial portions of the Software.

    THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
    IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
    FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
    AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
    LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
    OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
    SOFTWARE.
