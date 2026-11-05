# decimal128-text-codec-go

A thin codec between the two IEEE 754-2019 decimal128 interchange encodings —
BID and DPD — and decimal character sequences. Nothing else: no arithmetic, no
comparison, no value type, no context, no status flags.

One file, no dependencies outside the standard library. The point is that a
caller who holds interchange bits or a string, and wants the other one, can
depend on this without pulling in a full IEEE 754-2019 engine.

For the complete decimal128 implementation see `decimal128-go` at
<https://github.com/migueldecimal128/decimal128-go> and <https://decimal128.com>.

## API

```go
import codec "github.com/migueldecimal128/decimal128-text-codec-go"

// bits -> text, into the caller's buffer
var buf [codec.MaxLen]byte
n := codec.Bid128ToAscii(&buf, hi, lo)   // the text is buf[:n]
m := codec.Dpd128ToAscii(&buf, hi, lo)   // the text is buf[:m]

// text -> bits
hi, lo, err := codec.Bid128Parse("1.23E+45")
hi, lo, err  = codec.Dpd128Parse("1.23E+45")
```

An encoding is a pair of `uint64`: `hi` holds the high 64 bits, `lo` the low
64. The sign bit is the top bit of `hi`, and the rest is a combination field
and a trailing significand field laid out per IEEE 754-2019 §3.5.2. That is the
entire boundary; there is no value type, no context, no status flags.

Both parse functions **always return** an encoding — the canonical positive
quiet NaN on failure — so a caller who discards the error still holds a legal
IEEE 754-2019 datum rather than a zero value that means something else. That is
the family's contract, spelled the way Go spells things: multi-return rather
than the sibling ports' receiver-first out-parameter. The error message is
human-readable; its exact wording is not part of the contract.

The renderers write into a buffer the caller owns and return a length, so
nothing allocates. `MaxLen` is 42, the exact bound on a rendered decimal128,
and it sits in the parameter's type — an undersized buffer is a compile error
rather than a runtime check, which is what lets the renderers keep their
cannot-fail property. They return the length rather than `out[:n]` because a Go
slice header carries a capacity this API has no use for: `out[:n]` would hand
back `cap == MaxLen`, so appending to the result would silently write into the
rest of the caller's buffer. Returning the length is also what the standard
library does for this shape — `utf8.EncodeRune`, `binary.PutUvarint`,
`hex.Encode`.

## Behavior

**Rendering cannot fail.** Every 128-bit pattern maps to a valid decimal
character sequence. A non-canonical BID coefficient (≥ 10³⁴) renders as zero at
the encoded exponent, per IEEE 754-2019 §3.5.2, and a NaN payload ≥ 10³³
renders as no payload. Those patterns never arise from this codec's own encode
direction — they arise from network and file data, which is why the codec
handles them.

**Output is the GDAS `to-scientific-string` form, and only that.** Integer form
when the quantum exponent is zero, plain form when the quantum exponent is
negative and the adjusted exponent is at least `-6`, scientific otherwise. The
exponent sign is always written:
`6.02E+23`, never `6.02E23`. Cohort information is preserved — `1.00` and `1.0`
are distinct outputs from distinct encodings of the same numerical value, and
nothing is ever normalized or stripped.

**Accepted input** matches what the full `decimal128-{lang}` ports accept, with
one deliberate exception below, so a caller can move between this codec and the
full library without the grammar shifting under them: plain and scientific
notation with `E` or `e`, a leading `+` or `-`, `_` digit separators *between
two digits*, and the case-insensitive words `Infinity`, `Inf`, `NaN`, `qNaN`,
`sNaN` with an optional sign and an optional payload of decimal digits.

**The exception: a NaN payload may be parenthesised** — `NaN(123)` as well as
`NaN123`. C's own `strtod` reads `NAN(n-char-sequence)`, so the spelling
reaches a caller from the platform itself. The form is positional and balanced:
`(` immediately after the word, at least one digit, `)` last. So `NaN()`,
`NaN(1`, `NaN(1]`, `NaN(1)2(3)`, `NaN (1)`, `NaN[1]` and `NaN{1}` are all
rejected. The resemblance to `strtod` stops at the parentheses — its sequence
is letters, digits and underscores read as an implementation-defined integer,
so `NAN(abc)` and `NAN(0x1c8)` are legal there and rejected here. Rendering
never emits parentheses; `NaN(123)` parses and renders back as `NaN123`.

The full ports accept the same spelling — `decimal128-go` took it on
2026-08-23, closing what had been the codecs' one grammar divergence from them.

**Rounding is ties-to-even, hardcoded**, and reachable only from the text →
bits direction: more than 34 significant digits, overflow to Infinity, or
underflow to a subnormal or zero. There is no rounding parameter and no format
selector — the premise is a four-function surface with no configuration. IEEE
754-2019 §5.4.2 places `convertFromDecimalCharacter` under the applicable
rounding-direction attribute; this codec deliberately implements only the
ties-to-even case, and is therefore a conforming **subset** of that operation.
Callers needing another rounding direction want the full library.

## Building

```
go build ./...
go test ./...
go test -race ./...
go test -tags verify ./...   # the same corpus with every assertion armed
```

Go 1.26 or later, matching the full `decimal128-go` port. No cgo, no external
modules, nothing to `go.sum`.

Internal-consistency claims — the ones unreachable for any input — are spelled
`verify` and gated on the `verify` build tag, which is the same contract the
full `decimal128-go` port uses: with the tag off, the call inlines to nothing
and the thunk holding the predicate is never allocated, so nothing it would
have computed survives into the binary. The default is off, so a consumer
linking this package never takes a panic inside a library on data they do not
control. `demand` — the precision invariant that stands in for the removed
precision-truncation step — is live in every build.

## Tests

Seven tests in one file.

| file | what it covers |
|---|---|
| `corpus_vectors_test.go` | the four conformance tables over the shared corpus, 2,601 rows, both directions, both formats; then the full 84,783-row canonical table. Also holds the corpus reader and the hex helpers, its only consumer. |

That is the whole suite, and the shape is deliberate: **every test vector in
this repo comes from the shared corpus.** Nothing in the suite reaches past the
public surface.

The repo is standalone: it depends on no decimal128 port, at runtime or in
tests. The independent oracle is the shared `text_bid_dpd` corpus, generated by
the `decimal128-c` engine through its public API; that engine reaches its
answers by different algorithms, so agreement is a cross-check rather than a
tautology.

The vectors are read from `resources/text_bid_dpd`, where `resources` is the
`decimal128-resources` submodule — the same mount every decimal128 port uses.
Nothing is copied into this repo. `go test` runs the binary in its package's
source directory, so the relative path is all this port needs; the C port's
compiled-in `VECTOR_DIR`, Swift's `#filePath` and C#'s `[CallerFilePath]` all
exist to recover what Go hands over. An absolute `DECIMAL128_RESOURCES`
overrides it.

Two things worth knowing if you touch the tests:

- **The corpus quotes two of its five files.** `parse` and `error` double-quote
  the text column with `\\` and `\"` escapes; the other three leave it bare. A
  naive comma split silently corrupts four rows and still passes, because a
  malformed input fails to parse either way. The reader honours the quoting,
  the row counts are asserted, and `TestQuotedColumnRoundTrips` checks those
  four rows by hand.
- **The submodule must be checked out.** With `resources/text_bid_dpd` empty
  the loader fails with a message naming the `git submodule update --init
  --recursive` that fixes it, rather than a suite that quietly tests nothing.
  Clone with `--recurse-submodules`.

## Provenance

Ported from `decimal128-text-codec-c`, which is itself a port of
`decimal128-text-codec-kotlin`, the reference implementation for the family. C
is the closer base because it answered the same question first: what the file
looks like when the language has neither a 128-bit integer nor a string type
that can be handed back — so the encoding is a pair of unsigned 64-bit words,
the way it is here. The design spec is `TextCodecLayer.md` in the
`decimal128-workorder` repository.

Where this port differs from its siblings, in each case because Go already had
the answer:

- **The 128-bit primitives are `math/bits`** — `Mul64`, `Add64`,
  `LeadingZeros64`. Go guarantees them on every target it compiles for, so the
  reference's four-partial-product decomposition of a widening multiply, and
  the C port's three-way `__int128` / `__umulh` / synthesize-from-limbs split
  along with the whole extra build that exists to exercise the fallback,
  collapse to a call each.
- **Parse returns `(hi, lo uint64, err error)`.** Multi-return is Go's
  out-param, so the finalize and parse chains thread values back the same way
  the Swift port's tuples do rather than taking pointers. The failure values
  are unexported sentinels: the contract is that a caller can tell success from
  failure and read a message, not that any particular wording is stable, and
  exporting them would turn every one of those strings into API.
- **The leading-zero flush in the two digit counters is an `if`.** The
  reference coerces a comparison to an integer to keep it branchless; Go has no
  bool-to-integer conversion, and spelling it as arithmetic on a `bool` would
  be worse than the branch.
- **Integer overflow wraps**, as it does in C for unsigned words and in Kotlin,
  so the sites the Swift port has to spell `&+` / `&*` are plain operators here
  — the exponent accumulator and the over-limit NaN payload accumulator, each
  still carrying the note saying why the wrap is the intended arithmetic.

The finalizer's precision-truncation step is removed, in this port and in the
reference alike. It descends from a finalizer built for an arithmetic engine,
whose multiply and FMA hand it a double-width product; a codec's only caller is
a parser that accumulates at most 19 + 15 = 34 significant digits and turns
everything past them into a residue. Measured on the C port: zero executions
across the shared corpus, 400,000 random numerals, and 76,280 finalize calls
from inputs of up to 5,000 digits. A dead path is an unverified path, so it is
replaced by an unconditional `demand`. It stays live in every full port's
`Finalize`, where multiply and FMA do reach it; a caller here that is not a
parser needs it back.

The codec stands on the interchange formats alone. It holds no intermediate
value type: parsing yields loose scalars — sign, quantum exponent, coefficient,
residue — which are finalized and written *toward* an encoding. It does not
pivot through any engine's in-memory representation, and must not be changed
to.

## License

MIT.
