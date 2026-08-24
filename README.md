# decimal128-text-codec-go

Thin codec between the two IEEE 754-2019 decimal128 interchange encodings — BID
and DPD — and decimal character sequences, for Go.

Ported from `decimal128-text-codec-c`, which is itself a port of
`decimal128-text-codec-kotlin`, the reference implementation for the family. C
is the closer base because it answered the same question first: what the file
looks like when the language has neither a 128-bit integer nor a string type
that can be handed back — so the encoding is a pair of unsigned 64-bit words,
the way it is here. One file, no dependencies outside the standard library.

## API

```go
import codec "github.com/migueldecimal128/decimal128-text-codec-go"

// bits -> text, into the caller's buffer
var buf [codec.Max]byte
n := codec.Bid128ToString(&buf, hi, lo)   // the text is buf[:n]
m := codec.Dpd128ToString(&buf, hi, lo)   // the text is buf[:m]

// text -> bits
hi, lo, err := codec.Bid128Parse("1.23E+45")
hi, lo, err  = codec.Dpd128Parse("1.23E+45")
```

An encoding is a pair of `uint64`: `hi` holds the high 64 bits, `lo` the low 64.
The sign bit is the top bit of `hi`, and the rest is a combination field and a
trailing significand field laid out per IEEE 754-2019 §3.5.2. That is the entire
boundary; there is no value type, no context, no status flags.

Both parse functions always return an encoding — the canonical positive quiet
NaN on failure — so a caller who discards the error still holds a legal IEEE
754-2019 datum rather than a zero value that means something else. That is the
family's contract, spelled the way Go spells things: multi-return rather than
the sibling ports' receiver-first out-parameter.

## Scope

Rounding, where required, is `roundTiesToEven` and is not configurable. Output
is the GDAS `to-scientific-string` form and is not configurable — the exponent
sign is always written (`6.02E+23`), and cohort information is preserved. No
arithmetic, no field extraction, no value type.

Accepted input matches what the full `decimal128-{lang}` ports accept. That
includes the parenthesised NaN payload: **`NaN(123)` as well as `NaN123`**,
matching the spelling C's `strtod` reads. The form is positional and balanced —
`(` immediately after the word, at least one digit, `)` last — so `NaN()`,
`NaN(1`, `NaN(1)2(3)`, `NaN[1]` and `NaN{1}` are all rejected. Rendering never
emits parentheses. The codecs had this before the ports did; the ports took it
on 2026-08-23, closing the family's one grammar divergence.

For the full decimal128 implementation see `decimal128-go` at
<https://decimal128.com>.

## Requirements

Go 1.26 or later, matching the full `decimal128-go` port. No cgo, no external
modules, nothing to `go.sum`. Where this port differs from its siblings, in each
case because Go already had the answer:

- The 128-bit primitives are `math/bits` — `Mul64`, `Add64`, `LeadingZeros64`.
  Go guarantees them on every target it compiles for, so the reference's
  four-partial-product decomposition of a widening multiply, and the C port's
  three-way `__int128` / `__umulh` / synthesize-from-limbs split along with the
  whole extra build that exists to exercise the fallback, collapse to a call
  each.
- The renderers write into the caller's `*[Max]byte` and return a length, as the
  C port does, so nothing is allocated. `Max` is 42, the exact bound on a
  rendered decimal128, and it sits in the parameter's type — an undersized
  buffer is a compile error rather than a runtime check, which is what lets the
  renderers keep their cannot-fail property. They return the length rather than
  `out[:n]` because a Go slice header carries a capacity this API has no use
  for: `out[:n]` would hand back `cap == Max`, so appending to the result would
  silently write into the rest of the caller's buffer. Returning the length is
  also what the standard library does for this shape — `utf8.EncodeRune`,
  `binary.PutUvarint`, `hex.Encode`.
- Parse returns `(hi, lo uint64, err error)`. Multi-return is Go's out-param, so
  the finalize and parse chains thread values back the same way the Swift port's
  tuples do rather than taking pointers. The failure values are unexported
  sentinels: the contract is that a caller can tell success from failure and
  read a message, not that any particular wording is stable, and exporting them
  would turn every one of those strings into API.
- The leading-zero flush in the two digit counters is an `if`. The reference
  coerces a comparison to an integer to keep it branchless; Go has no
  bool-to-integer conversion, and spelling it as arithmetic on a `bool` would be
  worse than the branch.

Integer overflow wraps, as it does in C for unsigned words and in Kotlin, so the
sites the Swift port has to spell `&+` / `&*` are plain operators here — the
exponent accumulator and the over-limit NaN payload accumulator, each still
carrying the note saying why the wrap is the intended arithmetic.

Internal-consistency claims — the ones unreachable for any input — are spelled
`verify` and gated on the `verify` build tag, which is the same contract the
full `decimal128-go` port uses: with the tag off, the call inlines to nothing
and the thunk holding the predicate is never allocated, so nothing it would have
computed survives into the binary. The default is off, so a consumer linking
this package never takes a panic inside a library on data they do not control.
`demand` — the precision invariant that stands in for the removed
precision-truncation step — is live in every build.

## Tests

```
git submodule update --init --recursive
go test ./...
go test -tags verify ./...   # the same corpus with every assertion armed
```

| file | what it covers |
|---|---|
| `corpus_vectors_test.go` | the four conformance tables over the shared corpus, 2,601 rows, both directions, both formats; then the full 84,783-row canonical table. Also holds the corpus reader and the hex helpers, its only consumer. |

That is the whole suite, and the shape is deliberate: **every test vector in this
repo comes from the shared corpus.** There is no hand-written table. The file is
an external test package (`decimal128textcodec_test`), so it can reach only the
four exported functions — nothing in it can touch a package-private helper even
by accident.

The repo is standalone: it depends on no decimal128 port, at runtime or in
tests. The independent oracle is the shared `text_bid_dpd` corpus, generated by
the decimal128-c engine, which reaches its answers by different algorithms than
this codec does.

The vectors are read from `resources/text_bid_dpd`, where `resources` is the
`decimal128-resources` submodule — the same mount every decimal128 port uses.
Nothing is copied into this repo. `go test` runs the binary in its package's
source directory, so the relative path is all this port needs; the C port's
compiled-in `VECTOR_DIR`, Swift's `#filePath` and C#'s `[CallerFilePath]` all
exist to recover what Go hands over. An absolute `DECIMAL128_RESOURCES`
overrides it.

## License

MIT.
