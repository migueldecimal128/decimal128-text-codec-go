// SPDX-License-Identifier: MIT

/*
Package decimal128textcodec is a thin codec between the two IEEE 754-2019
decimal128 interchange encodings -- BID (Binary Integer Decimal) and DPD
(Densely Packed Decimal) -- and decimal character sequences.

A decimal128 encoding is 128 bits: a sign bit, a combination field and a
trailing significand field, laid out per IEEE 754-2019 section 3.5.2. It
crosses this API as a pair of 64-bit words, hi and lo. That is the entire
boundary; there is no value type, no context, no status flags.

This codec provides no arithmetic, no comparison and no field extraction. For
the full IEEE 754-2019 decimal128 implementation -- arithmetic, comparison,
every rounding-direction attribute, IEEE status flags -- see
https://decimal128.com.

# Rounding

Rounding, where the text -> bits direction requires it, is the IEEE 754-2019
roundTiesToEven attribute and is not configurable. IEEE 754-2019 section 5.4.2
places convertFromDecimalCharacter under the applicable rounding-direction
attribute; this codec deliberately implements only the ties-to-even case and is
therefore a conforming subset of that operation. Callers needing another
rounding direction want the full library.

Output form is the GDAS to-scientific-string form and is not configurable.
Cohort information is preserved: "1.00" and "1.0" are distinct outputs from
distinct encodings of the same numerical value.

# Round-trip behavior

The renderer's output round-trips exactly: parsing rendered text and rendering
it again reproduces that text, and rendering a canonical encoding and parsing
the result reproduces the original bits. Three classes of input do not
round-trip:

  - More than 34 significant digits are accepted and rounded to 34 with
    roundTiesToEven. The rounded value is encoded; the input text is not
    preserved.
  - Magnitudes outside the representable range overflow to Infinity or
    underflow to a subnormal or zero, per IEEE 754-2019.
  - Non-canonical bit patterns render as something the encoder would never have
    emitted, so re-parsing yields the canonical pattern instead.

# Non-canonical bit patterns

Not every 128-bit pattern is canonical. Each encoding has its own non-canonical
forms -- patterns the encoder would never emit -- and what makes a pattern
non-canonical is a property of that encoding, documented where that encoding is
decoded.

Neither renderer can fail on one: every 128-bit pattern renders as the value
IEEE 754-2019 assigns it. Decoding is one-way for such a pattern, because the
parsers emit only canonical encodings, so re-parsing the rendered text yields
bits that differ from the original. Non-canonical patterns never arise from
this codec's own parsers; they appear only when encodings come from outside,
such as network or file data.

# Thread safety

All functions are pure and safe for concurrent use.

# This implementation

Nothing here allocates. The renderers write into a caller-owned [MaxLen]byte
and return a length; the parsers return the encoding as two words plus a
package-level sentinel error. The package has no dependencies outside the
standard library.
*/
package decimal128textcodec

/*
 * Everything exported is the four functions at the bottom of this file; the
 * rest is package-private, which is this language's spelling of the
 * reference's `private`.
 */

import (
	"errors"
	"math/bits"
	"strconv"
)

/* ---------------------------------------------------------------------------
 * Internal-state assertions
 *
 * Two mechanisms.
 *
 *   verify(what, cond)  internal-consistency claims -- unreachable for ANY
 *                       input, so a failure is a codec bug rather than bad
 *                       data. Compiled out unless the `verify` build tag is
 *                       set, and the default is OFF: a consumer links this
 *                       package into their own program and must not take a
 *                       panic inside a library on data they do not control.
 *                       `go test -tags verify` arms them, and CI runs that.
 *
 *   demand(what, cond)  claims that must survive into a shipping build: that
 *                       the caller's buffer is at least MaxLen, checked once
 *                       on entry to each decoder, and the coefficient-width
 *                       invariant standing in for the removed precision-
 *                       truncation step, where falling through would
 *                       silently encode an out-of-range coefficient.
 *
 * Go has no macros, so verify's condition is spelled as a thunk at the call
 * site: with the tag off the call is inlined to nothing and the never-called
 * closure -- and everything it would have computed -- is dead-code-eliminated.
 * A bare bool would evaluate the predicate even when the checks are off.
 * ------------------------------------------------------------------------- */
// impossible aborts unconditionally. Go spells "does not return" as panic.
func impossible(what string) {
	panic("decimal128-text-codec: impossible state: " + what)
}

func demand(what string, cond bool) {
	if !cond {
		impossible(what)
	}
}

func verify(what string, cond func() bool) {
	if verifyEnabled {
		if !cond() {
			impossible(what)
		}
	}
}

/* ------------------------------------------------------------------------
 * Public constants
 * ---------------------------------------------------------------------- */

/*
MaxLen is the exact bound on a rendered decimal128, in ASCII character bytes.

The widest scientific form -9.999...E-6143 is 1 sign, 1 digit, 1 radix point,
33 digits, 1 E, 1 sign and 4 exponent digits; the widest plain form
-0.00000999... is 1 sign, 1 zero, 1 radix point, 5 leading zeros and 34 digits.
Each form reaches exactly 42.

The plain form's five leading zeros are pinned by the GDAS rule that selects it
only when the adjusted exponent is at least -6, which puts five zeros between
the radix point and the first significant digit. The exponent field cannot
exceed four digits inside decimal128's range, so neither number can drift
without the format itself changing.

The renderers are checked against this bound once, on entry, rather than at
every write: the buffer is confirmed to be at least this long and the forms
above are what guarantee nothing longer is produced. Sizing it exactly rather
than generously is what keeps the two halves of that argument in contact; with
slack, an error in the arithmetic above would quietly consume the slack instead
of showing up.

A caller sizes their buffer with it, and *[MaxLen]byte puts it in the type, so
an undersized buffer is a compile error. No NUL is written, so no room is
reserved for one.
*/
// The canonical positive quiet NaN, which the parsers write on failure.
const qnanPatternHi = 0x7C00000000000000

const MaxLen = 42

/* ------------------------------------------------------------------------
 * Public API
 * ---------------------------------------------------------------------- */

/*
Bid128ToAscii renders a BID128 encoding into a caller-supplied buffer,
allocating nothing.

Writes the text into out and returns the number of bytes written; the text is
out[:n]. Nothing is allocated, and MaxLen is part of the buffer's type, so an
undersized buffer is a compile error rather than a runtime check.

The output is the GDAS to-scientific-string form: integer notation when the
quantum exponent is zero (e.g. "123"), plain notation when the adjusted
exponent is in the range -6 to 0 (e.g. "123.45"), normalized scientific
notation otherwise (e.g. "1.234E+5"). The exponent sign is always written, as
in "6.02E+23". Cohort information is preserved: "1.00" and "1.0" are distinct
outputs from distinct encodings of the same numerical value.

Special values render as "Infinity", "-Infinity", "NaN" and "sNaN", each with
an optional sign and an optional payload suffix such as "NaN123".

The two interchange encodings differ only in how the coefficient is stored:
BID128 as a binary integer, DPD128 as a leading digit plus eleven 10-bit
declets. The sign and the 14-bit biased exponent are the same in both, and peer
encodings of the same value render to identical text.

This cannot fail. Every 128-bit pattern renders: a non-canonical BID128
coefficient (>= 10^34) renders as zero at the encoded exponent, and a NaN
payload >= 10^33 renders as no payload, both per IEEE 754-2019 section 3.5.2.

	var buf [decimal128textcodec.MaxLen]byte
	n := decimal128textcodec.Bid128ToAscii(&buf, hi, lo)
	fmt.Printf("%s\n", buf[:n])
*/
func Bid128ToAscii(out *[MaxLen]byte, hi, lo uint64) int {
	// The length is returned rather than out[:n]: a slice header carries a
	// capacity this API has no use for, so out[:n] would hand back
	// cap == MaxLen and appending to the result would silently write into
	// the rest of the caller's buffer.
	return decodeBid128toAscii(out[:], hi, lo)
}

/*
Dpd128ToAscii renders a DPD128 encoding into a caller-supplied buffer,
allocating nothing.

Writes the text into out and returns the number of bytes written; the text is
out[:n]. Nothing is allocated, and MaxLen is part of the buffer's type, so an
undersized buffer is a compile error rather than a runtime check.

The output is the GDAS to-scientific-string form: integer notation when the
quantum exponent is zero (e.g. "123"), plain notation when the adjusted
exponent is in the range -6 to 0 (e.g. "123.45"), normalized scientific
notation otherwise (e.g. "1.234E+5"). The exponent sign is always written, as
in "6.02E+23". Cohort information is preserved: "1.00" and "1.0" are distinct
outputs from distinct encodings of the same numerical value.

Special values render as "Infinity", "-Infinity", "NaN" and "sNaN", each with
an optional sign and an optional payload suffix such as "NaN123".

The two interchange encodings differ only in how the coefficient is stored:
BID128 as a binary integer, DPD128 as a leading digit plus eleven 10-bit
declets. The sign and the 14-bit biased exponent are the same in both, and peer
encodings of the same value render to identical text.

This cannot fail. Every 128-bit pattern renders: a DPD128 coefficient is always
in range, since a leading digit of at most 9 followed by eleven declets cannot
reach 10^34; a NaN payload, carried in those same declets, is likewise always
in range; and non-canonical declets decode to their value, per IEEE 754-2019
section 3.5.2.

	var buf [decimal128textcodec.MaxLen]byte
	n := decimal128textcodec.Dpd128ToAscii(&buf, hi, lo)
	fmt.Printf("%s\n", buf[:n])
*/
func Dpd128ToAscii(out *[MaxLen]byte, hi, lo uint64) int {
	// The length is returned rather than out[:n]: a slice header carries a
	// capacity this API has no use for, so out[:n] would hand back
	// cap == MaxLen and appending to the result would silently write into
	// the rest of the caller's buffer.
	return decodeDpd128toAscii(out[:], hi, lo)
}

/*
Bid128Parse parses a decimal character sequence into a BID128 encoding.

Returns the encoding as hi and lo plus an error. On failure the error is
non-nil and the encoding is the canonical positive quiet NaN, so a caller who
discards the error still holds a legal IEEE 754-2019 datum rather than a zero
value that means something else. The error's text is a human-readable message;
its text is the engine's ParseStatus name for the failure -- EMPTY_STRING,
INVALID_FORMAT, UNEXPECTED_CHAR, DOUBLE_DOT, NO_COEFFICIENT_DIGIT,
NO_EXPONENT_DIGIT, INVALID_UNDERSCORE_LOCATION, FAILURE_PARSING_INFINITY or
FAILURE_PARSING_NAN -- and the conformance suite asserts it against the shared
corpus, so it is part of the contract.

Accepted input:

  - Plain notation: "0", "-1.5", "123.456", "0.0001".
  - Scientific notation: "1.5E10", "-2.3e-7", "6.02E+23". Both E and e are
    accepted as the exponent separator.
  - Digit separators: _ between two digits, in the coefficient or the exponent
    ("1_000", "1.234_567", "1e1_0"). An underscore must be both preceded and
    followed by a digit, so "1_", "_1", "1__2", "1_.5" and "1._5" are all
    rejected.
  - The case-insensitive words Infinity, Inf, NaN, qNaN and sNaN, each with an
    optional sign and an optional NaN payload of decimal digits ("NaN123",
    "-sNaN42"). A payload may also be parenthesised, as "NaN(123)".

The parenthesised payload form is positional and balanced: ( immediately after
the word, at least one digit, ) last. "NaN()", "NaN(1", "NaN(1)2(3)", "NaN[1]"
and "NaN{1}" are rejected. Rendering never emits parentheses, and the full
decimal128 ports accept the same spelling.

More than 34 significant digits round to 34, ties-to-even. Magnitudes above the
representable range become Infinity; magnitudes below it become a subnormal or
zero. None of those are failures.

Failures are structural: an empty sequence, unrecognized characters, missing
digits around the radix point, a malformed exponent field.

The two interchange encodings differ only in how the coefficient is stored:
BID128 as a binary integer, DPD128 as a leading digit plus eleven 10-bit
declets. The same text parses to peer encodings of the same value in either
format, and the canonical positive quiet NaN is the same bit pattern in both.
*/
func Bid128Parse(text string) (hi, lo uint64, err error) {
	return parseInto(true, text)
}

/*
Dpd128Parse parses a decimal character sequence into a DPD128 encoding.

Returns the encoding as hi and lo plus an error. On failure the error is
non-nil and the encoding is the canonical positive quiet NaN, so a caller who
discards the error still holds a legal IEEE 754-2019 datum rather than a zero
value that means something else. The error's text is a human-readable message;
its text is the engine's ParseStatus name for the failure -- EMPTY_STRING,
INVALID_FORMAT, UNEXPECTED_CHAR, DOUBLE_DOT, NO_COEFFICIENT_DIGIT,
NO_EXPONENT_DIGIT, INVALID_UNDERSCORE_LOCATION, FAILURE_PARSING_INFINITY or
FAILURE_PARSING_NAN -- and the conformance suite asserts it against the shared
corpus, so it is part of the contract.

Accepted input:

  - Plain notation: "0", "-1.5", "123.456", "0.0001".
  - Scientific notation: "1.5E10", "-2.3e-7", "6.02E+23". Both E and e are
    accepted as the exponent separator.
  - Digit separators: _ between two digits, in the coefficient or the exponent
    ("1_000", "1.234_567", "1e1_0"). An underscore must be both preceded and
    followed by a digit, so "1_", "_1", "1__2", "1_.5" and "1._5" are all
    rejected.
  - The case-insensitive words Infinity, Inf, NaN, qNaN and sNaN, each with an
    optional sign and an optional NaN payload of decimal digits ("NaN123",
    "-sNaN42"). A payload may also be parenthesised, as "NaN(123)".

The parenthesised payload form is positional and balanced: ( immediately after
the word, at least one digit, ) last. "NaN()", "NaN(1", "NaN(1)2(3)", "NaN[1]"
and "NaN{1}" are rejected. Rendering never emits parentheses, and the full
decimal128 ports accept the same spelling.

More than 34 significant digits round to 34, ties-to-even. Magnitudes above the
representable range become Infinity; magnitudes below it become a subnormal or
zero. None of those are failures.

Failures are structural: an empty sequence, unrecognized characters, missing
digits around the radix point, a malformed exponent field.

The two interchange encodings differ only in how the coefficient is stored:
BID128 as a binary integer, DPD128 as a leading digit plus eleven 10-bit
declets. The same text parses to peer encodings of the same value in either
format, and the canonical positive quiet NaN is the same bit pattern in both.
*/
func Dpd128Parse(text string) (hi, lo uint64, err error) {
	return parseInto(false, text)
}

func parseInto(formatIsBid bool, text string) (hi, lo uint64, err error) {
	hi, lo, err = encodeStringOrError(formatIsBid, text)
	if err != nil {
		// Always return an encoding, so a caller who discards the error still
		// observes a legal datum. The canonical positive quiet NaN is the same
		// bit pattern in both formats.
		return qnanPatternHi, 0, err
	}
	return hi, lo, nil
}

/* ------------------------------------------------------------------------
 * Format constants -- IEEE 754-2019 Table 3.6, decimal128
 * ---------------------------------------------------------------------- */

const (
	bias   = 6176
	qMin   = -6176 // the minimum quantum exponent
	qMax   = 6111  // the maximum quantum exponent
	tWidth = 110   // trailing significand field width in bits
	w5     = 17    // combination field width in bits
)

const (
	// 10^34, one past the largest coefficient; maxx = max eXclusive
	maxxCoeff34Hi = 0x0001ED09BEAD87C0
	maxxCoeff34Lo = 0x378D8E6400000000

	// 10^33, the smallest 34-digit coefficient
	minPrecision34CoeffHi = 0x0000314DC6448D93
	minPrecision34CoeffLo = 0x38C15B0A00000000

	// 33 nines -- the largest canonical NaN payload
	maxPayloadHi = minPrecision34CoeffHi
	maxPayloadLo = minPrecision34CoeffLo - 1

	signBit       = 0x8000000000000000
	infPatternHi  = 0x7800000000000000
	snanPatternHi = 0x7E00000000000000

	tenPow15 = 1000000000000000
	tenPow18 = 1000000000000000000
)

/*
 * Powers of 10 from 10^0 through 10^34 as 128-bit values in little-endian
 * order: pow10Table[2*i] is the low 64 bits of 10^i, pow10Table[2*i + 1] the
 * high 64.
 *
 * Sized to the coefficient bound and indexed directly -- no mask, no padding.
 * Every reachable index is <= 34: the binding case is calcDigitLen128, whose
 * input is always a coefficient or NaN payload below 10^34, so bitLen <= 113
 * and (bitLen * 1233) >> 12 <= 34. It doubles as the 5^n source for the
 * divide kernel, since 10^n >> n is exactly 5^n; there is no POW5 table.
 *
 * This carries the Table suffix because Go has no separate namespace for
 * constants, and `pow10` is a parameter name throughout the kernels below.
 */
var pow10Table = [(34 + 1) * 2]uint64{
	1, 0, // 10^0
	0x000000000000000A, 0, // 10^1
	0x0000000000000064, 0, // 10^2
	0x00000000000003E8, 0, // 10^3
	0x0000000000002710, 0, // 10^4
	0x00000000000186A0, 0, // 10^5
	0x00000000000F4240, 0, // 10^6
	0x0000000000989680, 0, // 10^7
	0x0000000005F5E100, 0, // 10^8
	0x000000003B9ACA00, 0, // 10^9
	0x00000002540BE400, 0, // 10^10
	0x000000174876E800, 0, // 10^11
	0x000000E8D4A51000, 0, // 10^12
	0x000009184E72A000, 0, // 10^13
	0x00005AF3107A4000, 0, // 10^14
	0x00038D7EA4C68000, 0, // 10^15
	0x002386F26FC10000, 0, // 10^16
	0x016345785D8A0000, 0, // 10^17
	0x0DE0B6B3A7640000, 0, // 10^18
	0x8AC7230489E80000, 0, // 10^19
	0x6BC75E2D63100000, 0x0000000000000005, // 10^20
	0x35C9ADC5DEA00000, 0x0000000000000036, // 10^21
	0x19E0C9BAB2400000, 0x000000000000021E, // 10^22
	0x02C7E14AF6800000, 0x000000000000152D, // 10^23
	0x1BCECCEDA1000000, 0x000000000000D3C2, // 10^24
	0x161401484A000000, 0x0000000000084595, // 10^25
	0xDCC80CD2E4000000, 0x000000000052B7D2, // 10^26
	0x9FD0803CE8000000, 0x00000000033B2E3C, // 10^27
	0x3E25026110000000, 0x00000000204FCE5E, // 10^28
	0x6D7217CAA0000000, 0x00000001431E0FAE, // 10^29
	0x4674EDEA40000000, 0x0000000C9F2C9CD0, // 10^30
	0xC0914B2680000000, 0x0000007E37BE2022, // 10^31
	0x85ACEF8100000000, 0x000004EE2D6D415B, // 10^32
	0x38C15B0A00000000, 0x0000314DC6448D93, // 10^33
	0x378D8E6400000000, 0x0001ED09BEAD87C0, // 10^34
}

const (
	exact   = 0
	ltHalf  = 1
	tieHalf = 2 // `half` alone would read as a quantity in Go
	gtHalf  = 3
)

// Two bits per digit 0..9: 0 -> exact, 1..4 -> ltHalf, 5 -> tieHalf, 6..9 -> gtHalf
const digitMap = 0xFF954

/*
 * Reciprocal-multiplication constants. The 64-bit ones fold in the 64-bit
 * shift that mulhi64 already performs, so the named shift is the remainder.
 */
const (
	mU64Div1e8 = 0x8000000000000000 | 0x2BCC77118461CEFD
	sU64Div1e8 = 26
	mU32Div1e1 = 0xCCCCCCCD
	sU32Div1e1 = 35
	mU32Div1e2 = 0x51EB851F
	sU32Div1e2 = 37
	mU64Div1e4 = 0x346DC5D63886594B
	sU64Div1e4 = 11
	mask32     = 0xFFFFFFFF
)

/*
 * None of the optional leading characters live in the string data. The sign,
 * and the 's' that marks a signaling NaN, are each written unconditionally and
 * then overwritten by whatever follows when they are not wanted -- the trick
 * fnzToIntegerAscii already uses for the sign. That holds only because every
 * optional character is a single byte at the front of the form: a spelling that
 * ever put one elsewhere would render wrong rather than fail to compile. The
 * one exception is written where it happens, in zerAscii's exponent form,
 * where the byte is a TRAILING '+' overwritten by AppendInt's own '-'.
 */
const infinityText = "Infinity"

const nanText = "NaN"

/*
 * Every plain-form zero is a prefix of this literal. k = -qExp is the count of
 * zeros after the radix point, so k = 0 renders "0" with no radix point at all
 * and k >= 1 renders "0." followed by k zeros -- lengths 1 and k + 2.
 *
 * The sign is not in the data. It is written into out[0] unconditionally and
 * overwritten by the '0' when the value is positive, the same trick the finite
 * renderers use. That works because the negative spelling of every form here is
 * the positive spelling with one byte in front; a form that ever put the sign
 * anywhere else would render wrong rather than fail to compile.
 */
const plainZero = "0.000000"

// The exponent form's base. The '+' is not here: see zerAscii below.
const zeroExpText = "0E"

/*
 * The parse failures, one value each. They are package-private on purpose: the
 * header's promise is that a caller can tell success from failure and read a
 * message, not that any particular wording is stable, and an exported sentinel
 * would turn every one of these strings into API. Sharing one value per failure
 * is also what lets the corpus assert that the two formats reject an input for
 * the same reason.
 */
var (
	errEmptyString        = errors.New("EMPTY_STRING")
	errInvalidFormat      = errors.New("INVALID_FORMAT")
	errUnexpectedChar     = errors.New("UNEXPECTED_CHAR")
	errDoubleRadixDot     = errors.New("DOUBLE_DOT")
	errNoCoefficientDigit = errors.New("NO_COEFFICIENT_DIGIT")
	errNoExponentDigit    = errors.New("NO_EXPONENT_DIGIT")
	errUnderscoreLocation = errors.New("INVALID_UNDERSCORE_LOCATION")
	errParsingInfinity    = errors.New("FAILURE_PARSING_INFINITY")
	errParsingNaN         = errors.New("FAILURE_PARSING_NAN")
)

/* ------------------------------------------------------------------------
 * Low-level unsigned arithmetic
 * ---------------------------------------------------------------------- */

// High 64 bits of the 128-bit product x * y.
func mulhi64(x, y uint64) uint64 {
	hi, _ := bits.Mul64(x, y)
	return hi
}

// Leading zero count, 64 for zero.
func clz64(v uint64) int {
	return bits.LeadingZeros64(v)
}

// Bit length of a 128-bit value: highest set bit plus one, 0 for zero.
func calcBitLen128(dw1, dw0 uint64) int {
	if dw1 != 0 {
		return 128 - clz64(dw1)
	}
	if dw0 != 0 {
		return 64 - clz64(dw0)
	}
	return 0
}

/*
 * Decimal digit count of a 128-bit value.
 *
 * (bitLen * 1233) >> 12 is floor(bitLen * log10(2)), which is either the exact
 * digit count or one too small; a single unsigned compare against the
 * next-larger power of ten resolves the off-by-one.
 */
func calcDigitLen128(dw1, dw0 uint64) int {
	bitLen := calcBitLen128(dw1, dw0)
	loDigitCount := (bitLen * 1233) >> 12
	hiDigitCount := loDigitCount + 1
	pow10Offset := loDigitCount << 1
	p1 := pow10Table[pow10Offset+1]
	p0 := pow10Table[pow10Offset]
	if dw1 != p1 {
		if dw1 < p1 {
			return hiDigitCount - 1
		}
		return hiDigitCount
	}
	if dw0 < p0 {
		return hiDigitCount - 1
	}
	return hiDigitCount
}

// Multiply a 64-bit value by 10^pow10 and add a, returning the 128-bit result.
func u128FmaPow10(x uint64, pow10 int, a uint64) (outHi, outLo uint64) {
	y := pow10Table[pow10<<1]
	prodHi, prodLo := bits.Mul64(x, y)
	sumLo, carry := bits.Add64(prodLo, a, 0)
	return prodHi + carry, sumLo
}

/* ------------------------------------------------------------------------
 * The divide kernel
 * ---------------------------------------------------------------------- */

/*
 * Divide a 113-bit value by 10^pow10 for pow10 in 1..9, returning the quotient
 * and the remainder.
 *
 * Table-free: 10^pow10 factors as 2^pow10 * 5^pow10, so the low pow10 bits
 * fall straight out as the low part of the remainder and the shifted value is
 * divided by 5^pow10 with two hardware 64-bit divides.
 */
func u113DivModPow10Max9(dwHi, dwLo uint64, pow10 int) (qHi, qLo, rem uint64) {
	verify("pow10 is in the kernel's 1..9 range", func() bool { return pow10 >= 1 && pow10 <= 9 })
	verify("the dividend is at most 113 bits", func() bool { return clz64(dwHi) >= 15 })

	mask := (uint64(1) << pow10) - 1
	b := dwLo & mask
	sHi := dwHi >> pow10
	sLo := (dwLo >> pow10) | (dwHi << (64 - pow10))

	k := 50 - pow10
	maskK := (uint64(1) << k) - 1
	hi63 := (sHi << (14 + pow10)) | (sLo >> k)
	loK := sLo & maskK

	// two slots per power in pow10Table, hence the doubled index
	d := pow10Table[pow10<<1] >> pow10 // = 5^pow10
	q1 := hi63 / d
	r1 := hi63 % d
	mid := (r1 << k) | loK
	q0 := mid / d
	r0 := mid % d

	return q1 >> (14 + pow10), (q1 << k) + q0, (r0 << pow10) | b
}

/* ------------------------------------------------------------------------
 * Residue -- how much was discarded, relative to half a ulp
 * ---------------------------------------------------------------------- */

func residueFromDecimalDigit(digit int) int {
	return int((digitMap >> (digit << 1)) & 0x03)
}

/*
 * Combine a primary residue with a sticky one from less-significant digits,
 * round-bit-and-sticky-bit style: the sticky residue collapses to a single
 * nonzero bit which is OR'd into the low bit of the round residue.
 */
func residueMerge(roundResidue, stickyResidue int) int {
	s := (stickyResidue & 1) | (stickyResidue >> 1)
	return (roundResidue | s) & 0x03
}

// Classify a 128-bit value relative to half of 10^pow10.
func residueFromValuePow10(dw1, dw0 uint64, pow10 int) int {
	if (dw1 | dw0) == 0 {
		return exact
	}
	pow10Index := pow10 << 1
	pow10Hi := pow10Table[pow10Index+1]
	pow10Lo := pow10Table[pow10Index]

	halfHi := pow10Hi >> 1
	halfLo := (pow10Hi << 63) | (pow10Lo >> 1)
	if dw1 != halfHi {
		if dw1 < halfHi {
			return ltHalf
		}
		return gtHalf
	}
	if dw0 != halfLo {
		if dw0 < halfLo {
			return ltHalf
		}
		return gtHalf
	}
	return tieHalf
}

/*
 * Divide a 128-bit coefficient by 10^truncationNeeded, in chunks of at most
 * nine digits, returning the quotient and the accumulated residue class.
 */
func u128ScaleDownPow10(dw1, dw0 uint64, truncationNeeded int) (outHi, outLo uint64, scaleResidue int) {
	verify("the coefficient being scaled down is nonzero", func() bool { return (dw1 | dw0) != 0 })
	verify("the truncation keeps at least one digit", func() bool {
		return truncationNeeded > 0 && truncationNeeded < calcDigitLen128(dw1, dw0)
	})
	scaleResidue = exact
	dw1T := dw1
	dw0T := dw0
	digitsRemaining := truncationNeeded
	for {
		stepPow10 := digitsRemaining
		if stepPow10 > 9 {
			stepPow10 = 9
		}
		var rem uint64
		dw1T, dw0T, rem = u113DivModPow10Max9(dw1T, dw0T, stepPow10)
		stepResidue := residueFromValuePow10(0, rem, stepPow10)
		scaleResidue = residueMerge(stepResidue, scaleResidue)
		digitsRemaining -= stepPow10
		if digitsRemaining <= 0 {
			break
		}
	}
	return dw1T, dw0T, scaleResidue
}

/* --------------------------------------------------------------------------
 * DPD declet conversion
 *
 * A declet is 10 bits holding three decimal digits, 0..999. This is the one
 * place BID and DPD genuinely differ: BID stores the 34-digit coefficient as a
 * binary integer, DPD as a leading digit plus eleven declets.
 *
 * Both directions are pure boolean arithmetic (Cowlishaw's dpd2bcd and
 * bcd2dpd) rather than lookup tables.
 * ------------------------------------------------------------------------ */

/*
 * Decode a 10-bit declet (bits p q r s t u v w x y) into 0..999. Accepts the
 * 24 non-canonical declets as well as the 1000 canonical ones.
 *
 * A complement of t is spelled &^ here: on operands already reduced to 0 or 1
 * it agrees with ^, and Go's and-not says what is meant without complementing
 * 63 bits that are then masked away.
 */
func dpdDecodeDeclet(declet uint64) uint64 {
	verify("the declet is 10 bits", func() bool { return declet <= 1023 })

	if declet == 0 {
		return 0
	}

	p := (declet >> 9) & 1
	q := (declet >> 8) & 1
	r := (declet >> 7) & 1
	s := (declet >> 6) & 1
	t := (declet >> 5) & 1
	u := (declet >> 4) & 1
	v := (declet >> 3) & 1
	w := (declet >> 2) & 1
	x := (declet >> 1) & 1
	y := declet & 1

	ns := s ^ 1
	nx := x ^ 1
	nv := v ^ 1
	nw := w ^ 1
	nt := t ^ 1

	a := (v & w) & (ns | t | nx)
	b := p & (nv | nw | (s &^ t & x))
	c := q & (nv | nw | (s &^ t & x))
	d := r

	e := v & ((nw & x) | (x &^ t) | (s & x))
	f := (s & (nv | nx)) | (p & ns & t & v & w & x)
	g := (t & (nv | nx)) | (q & ns & t & w)
	h := u

	i := v & ((nw & nx) | (w & x & (s | t)))
	j := (nv & w) | (s & v & nw & x) | (p & w & (nx | (ns &^ t)))
	k := (nv & x) | (t & nw & x) | (q & v & w & (nx | (ns & nt)))
	m := y

	d2 := (a << 3) | (b << 2) | (c << 1) | d
	d1 := (e << 3) | (f << 2) | (g << 1) | h
	d0 := (i << 3) | (j << 2) | (k << 1) | m

	return d2*100 + d1*10 + d0
}

// Encode 0..999 into a 10-bit declet. The inverse of dpdDecodeDeclet over the
// canonical declets.
func dpdEncodeDeclet(n uint64) uint64 {
	verify("the value fits three decimal digits", func() bool { return n <= 999 })

	if n == 0 {
		return 0
	}

	q10 := (n * 205) >> 11 // n / 10, exact for n <= 999
	q100 := (n * 41) >> 12 // n / 100, exact for n <= 999

	d2 := q100
	d1 := q10 - (q100 * 10)
	d0 := n - (q10 * 10)

	a := (d2 >> 3) & 1
	b := (d2 >> 2) & 1
	c := (d2 >> 1) & 1
	d := d2 & 1

	e := (d1 >> 3) & 1
	f := (d1 >> 2) & 1
	g := (d1 >> 1) & 1
	h := d1 & 1

	i := (d0 >> 3) & 1
	j := (d0 >> 2) & 1
	k := (d0 >> 1) & 1
	m := d0 & 1

	na := a ^ 1
	ne := e ^ 1
	ni := i ^ 1

	p := b | ((a & j) | ((a & f) & i))
	q := c | ((a & k) | ((a & g) & i))
	r := d
	s := (f & (na | ni)) | ((na & (e & j)) | (e & i))
	t := g | ((na & (e & k)) | (a & i))
	u := h
	v := a | (e | i)
	w := a | ((e & i) | (ne & j))
	x := e | ((a & i) | (na & k))
	y := m

	return (p << 9) | (q << 8) | (r << 7) | (s << 6) | (t << 5) |
		(u << 4) | (v << 3) | (w << 2) | (x << 1) | y
}

/*
 * Decode up to seven packed declets into a binary integer. Declets are ten
 * bits each, least significant first; the topmost may be narrower, which is
 * how the coefficient's leading digit rides along -- 0..9 encodes to itself.
 */
func binFromDeclets(declets uint64) uint64 {
	bin := uint64(0)
	rest := declets
	multiplier := uint64(1)
	for rest != 0 {
		bin += dpdDecodeDeclet(rest&0x3FF) * multiplier
		// The last iteration multiplies past 10^18 and the product is never
		// used, since rest is zero by then, so it is let wrap.
		multiplier *= 1000
		rest >>= 10
	}
	return bin
}

// Encode a value of at most 18 digits into six packed declets, least
// significant first. The inverse of binFromDeclets over that range.
func decletsFromBin(bin uint64) uint64 {
	verify("the value fits eighteen decimal digits", func() bool { return bin < tenPow18 })
	rest := bin
	declets := uint64(0)
	shift := 0
	for rest != 0 {
		q := mulhi64(rest, 0x020C49BA5E353F7D) >> 3 // rest / 1000
		declets |= dpdEncodeDeclet(rest-(q*1000)) << shift
		shift += 10
		rest = q
	}
	return declets
}

/* ------------------------------------------------------------------------
 * Rendering integers into ASCII
 * ---------------------------------------------------------------------- */

/*
 * Render exactly 8 decimal digits of dw, ending at offMaxx. Zero-pads. The
 * caller guarantees dw < 10^8 and that the 8 bytes are inside the buffer.
 *
 * A binary cascade: one divide by 10^4 to split into halves, two by 10^2 to
 * split those into pairs, four by 10 for the digits.
 */
func render8DigitsBeforeIndex(dw uint64, ascii []byte, offMaxx int) {
	abcd := mulhi64(dw, mU64Div1e4) >> sU64Div1e4
	efgh := dw - (abcd * 10000)

	ab := (abcd * mU32Div1e2) >> sU32Div1e2
	cd := abcd - (ab * 100)

	ef := (efgh * mU32Div1e2) >> sU32Div1e2
	gh := efgh - (ef * 100)

	a := (ab * mU32Div1e1) >> sU32Div1e1
	b := ab - (a * 10)

	c := (cd * mU32Div1e1) >> sU32Div1e1
	d := cd - (c * 10)

	e := (ef * mU32Div1e1) >> sU32Div1e1
	f := ef - (e * 10)

	g := (gh * mU32Div1e1) >> sU32Div1e1
	h := gh - (g * 10)

	ascii[offMaxx-8] = byte('0' + a)
	ascii[offMaxx-7] = byte('0' + b)
	ascii[offMaxx-6] = byte('0' + c)
	ascii[offMaxx-5] = byte('0' + d)
	ascii[offMaxx-4] = byte('0' + e)
	ascii[offMaxx-3] = byte('0' + f)
	ascii[offMaxx-2] = byte('0' + g)
	ascii[offMaxx-1] = byte('0' + h)
}

/*
 * Render exactly renderDigitCount digits of dw, ending at offMaxx, zero-padded.
 * Four digits per iteration while at least four remain, then one at a time.
 */
func renderTailDigitsBeforeIndex(renderDigitCount int, dw uint64, ascii []byte, offMaxx int) {
	t := dw
	remainingDigitCount := renderDigitCount
	ib := offMaxx
	for remainingDigitCount >= 4 {
		t0 := mulhi64(t, mU64Div1e4) >> sU64Div1e4
		abcd := t - (t0 * 10000)
		t = t0
		ab := (abcd * mU32Div1e2) >> sU32Div1e2
		cd := abcd - (ab * 100)
		a := (ab * mU32Div1e1) >> sU32Div1e1
		b := ab - (a * 10)
		c := (cd * mU32Div1e1) >> sU32Div1e1
		d := cd - (c * 10)
		ascii[ib-4] = byte('0' + a)
		ascii[ib-3] = byte('0' + b)
		ascii[ib-2] = byte('0' + c)
		ascii[ib-1] = byte('0' + d)
		ib -= 4
		remainingDigitCount -= 4
	}
	for remainingDigitCount > 0 {
		divTen := (t * mU32Div1e1) >> sU32Div1e1
		digit := t - (divTen * 10)
		ib--
		ascii[ib] = byte('0' + digit)
		t = divTen
		remainingDigitCount--
	}
	verify("exactly renderDigitCount bytes were written", func() bool { return offMaxx-ib == renderDigitCount })
}

/*
 * Render a 128-bit value as digitPrintCount decimal digits, ending at
 * off + digitPrintCount.
 *
 * Three phases: while the high word is live, peel 8 digits per iteration via
 * three divides by 10^8 across rotating 96-bit chunks; once it fits in 64
 * bits, one divide by 10^8 per 8 digits; then the 1..7 leftover digits.
 */
func u128ToAscii(digitPrintCount int, dw1, dw0 uint64, ascii []byte, off int) int {
	dw1T := dw1
	dw0T := dw0
	ich := off + digitPrintCount
	remainingDigitCount := digitPrintCount
	for dw1T != 0 {
		q2 := mulhi64(dw1T, mU64Div1e8) >> sU64Div1e8
		r2 := dw1T - (q2 * 100000000)
		s2 := (r2 << 32) | (dw0T >> 32)
		q1 := mulhi64(s2, mU64Div1e8) >> sU64Div1e8
		r1 := s2 - (q1 * 100000000)
		s1 := (r1 << 32) | (dw0T & mask32)
		q0 := mulhi64(s1, mU64Div1e8) >> sU64Div1e8
		r0 := s1 - (q0 * 100000000)
		dw0T = (q1 << 32) + q0
		dw1T = q2
		render8DigitsBeforeIndex(r0, ascii, ich)
		ich -= 8
		remainingDigitCount -= 8
	}
	for remainingDigitCount >= 8 {
		t0 := mulhi64(dw0T, mU64Div1e8) >> sU64Div1e8
		r0 := dw0T - (t0 * 100000000)
		dw0T = t0
		render8DigitsBeforeIndex(r0, ascii, ich)
		ich -= 8
		remainingDigitCount -= 8
	}
	if remainingDigitCount > 0 {
		renderTailDigitsBeforeIndex(remainingDigitCount, dw0T, ascii, ich)
	}
	return digitPrintCount
}

/* --------------------------------------------------------------------------
 * Rendering a decoded value
 *
 * These four know nothing about BID or DPD: both decoders reduce an encoding
 * to (sign, qExp, coefficient) and hand it here.
 * ------------------------------------------------------------------------ */

func infAscii(out []byte, sign bool) int {
	signLen := 0
	if sign {
		signLen = 1
	}
	totalLen := signLen + len(infinityText)
	out[0] = '-' // overwritten by the 'I' when unsigned
	copy(out[signLen:], infinityText)
	return totalLen
}

func nanAscii(out []byte, sign, isSignaling bool, payloadHi, payloadLo uint64) int {
	signLen := 0
	if sign {
		signLen = 1
	}
	sLen := 0
	if isSignaling {
		sLen = 1
	}
	wordOff := signLen + sLen
	baseLen := wordOff + len(nanText)
	out[0] = '-'       // overwritten by the 's' or the 'N' when unsigned
	out[signLen] = 's' // overwritten by the 'N' when quiet

	if (payloadHi|payloadLo) == 0 ||
		// non-canonical payload, IEEE 754-2019 3.5.2 c) 2) -- read as zero
		payloadHi > maxPayloadHi ||
		(payloadHi == maxPayloadHi && payloadLo > maxPayloadLo) {
		copy(out[wordOff:], nanText)
		return baseLen
	}
	digitLen := calcDigitLen128(payloadHi, payloadLo)
	totalLen := baseLen + digitLen
	copy(out[wordOff:], nanText)
	u128ToAscii(digitLen, payloadHi, payloadLo, out, baseLen)
	return totalLen
}

func zerAscii(out []byte, sign bool, qExp int) int {
	if qExp <= 0 && qExp >= -6 {
		k := -qExp
		zeroLen := 1
		if k != 0 {
			zeroLen = k + 2
		}
		signLen := 0
		if sign {
			signLen = 1
		}
		totalLen := signLen + zeroLen
		out[0] = '-' // overwritten by the '0' when unsigned
		copy(out[signLen:], plainZero[:zeroLen])
		return totalLen
	}
	signLen := 0
	if sign {
		signLen = 1
	}
	// A positive exponent needs an explicit '+'; a negative one prints its own
	// '-' from AppendInt. Write the '+' unconditionally and let AppendInt land
	// on top of it when qExp < 0 -- the same discipline as the leading sign,
	// pointed the other way. It is correct only because AppendInt emits a '-'
	// for a negative value and emits nothing for a positive one, and because the
	// plain form above has already taken every qExp in [-6, 0], so this branch
	// never sees zero.
	plusLen := 0
	if qExp > 0 {
		plusLen = 1
	}
	baseLen := signLen + len(zeroExpText) + plusLen
	out[0] = '-' // overwritten by the '0' when unsigned
	copy(out[signLen:], zeroExpText)
	out[signLen+len(zeroExpText)] = '+' // overwritten by AppendInt's '-' when qExp < 0
	// strconv.AppendInt writes in place: out[:baseLen] keeps the buffer's
	// capacity, so nothing is reallocated.
	return len(strconv.AppendInt(out[:baseLen], int64(qExp), 10))
}

// qExp == 0: a plain integer, no radix point and no exponent.
func fnzToIntegerAscii(out []byte, sign bool, digitLen int, dw1, dw0 uint64) int {
	verify("a finite nonzero value has at least one digit", func() bool { return digitLen > 0 })
	signLen := 0
	if sign {
		signLen = 1
	}
	totalLen := signLen + digitLen
	out[0] = '-' // overwritten by the first digit when unsigned
	u128ToAscii(digitLen, dw1, dw0, out, signLen)
	return totalLen
}

/*
 * Fixed-point form: leading zeros, then the coefficient digits shifted right
 * by one position to make room for the radix point.
 */
func fnzToDecimalPointAscii(out []byte, sign bool, qExp, digitLen int, dw1, dw0 uint64) int {
	digitsRightOfDecimal := -qExp
	leadingZeroCount := 1 + digitsRightOfDecimal - digitLen
	if leadingZeroCount < 0 {
		leadingZeroCount = 0
	}
	signLen := 0
	if sign {
		signLen = 1
	}
	decimalPointLen := 1
	totalLen := signLen + leadingZeroCount + decimalPointLen + digitLen
	out[0] = '-'
	for i := signLen; i <= leadingZeroCount; i++ { // one extra, overwritten below
		out[i] = '0'
	}
	u128ToAscii(digitLen, dw1, dw0, out, signLen+leadingZeroCount)
	for i := totalLen - 1; i >= totalLen-digitsRightOfDecimal; i-- {
		out[i] = out[i-1]
	}
	out[totalLen-digitsRightOfDecimal-1] = '.'
	return totalLen
}

/*
 * Normalized scientific form. The exponent sign is always written, per the
 * GDAS to-scientific-string form: 1.234E+5, never 1.234E5.
 */
func fnzToNormalizedScientificAscii(out []byte, sign bool, qExp, digitLen int, dw1, dw0 uint64) int {
	verify("a finite nonzero value has at least one digit", func() bool { return digitLen > 0 })
	eExp := qExp + (digitLen - 1)
	eExpAbs := eExp
	if eExp < 0 {
		eExpAbs = -eExp
	}
	signLen := 0
	if sign {
		signLen = 1
	}
	decimalPointLen := 0
	if digitLen > 1 {
		decimalPointLen = 1
	}
	expELen := 1
	expSignLen := 1
	expSignByte := byte('+')
	if eExp < 0 {
		expSignByte = '-'
	}
	expDigitLen := 1
	switch {
	case eExpAbs >= 1000:
		expDigitLen = 4
	case eExpAbs >= 100:
		expDigitLen = 3
	case eExpAbs >= 10:
		expDigitLen = 2
	}
	totalLen := signLen + decimalPointLen + digitLen + expELen + expSignLen + expDigitLen
	out[0] = '-'
	u128ToAscii(digitLen, dw1, dw0, out, signLen+decimalPointLen)
	if decimalPointLen > 0 {
		out[signLen] = out[signLen+1]
		out[signLen+1] = '.'
	}
	iE := signLen + decimalPointLen + digitLen
	out[iE] = 'E'
	out[iE+1] = expSignByte
	renderTailDigitsBeforeIndex(expDigitLen, uint64(eExpAbs), out, totalLen)
	return totalLen
}

/*
 * Finite non-zero: pick the form. Integer when the quantum exponent is zero,
 * fixed-point when the adjusted exponent is at least -6,
 * scientific otherwise -- the GDAS auto-select rule.
 */
func fnzAscii(out []byte, sign bool, qExp int, dw1, dw0 uint64) int {
	digitLen := calcDigitLen128(dw1, dw0)
	eExp := qExp + (digitLen - 1)
	if qExp == 0 {
		return fnzToIntegerAscii(out, sign, digitLen, dw1, dw0)
	}
	if qExp <= 0 && eExp >= -6 {
		return fnzToDecimalPointAscii(out, sign, qExp, digitLen, dw1, dw0)
	}
	return fnzToNormalizedScientificAscii(out, sign, qExp, digitLen, dw1, dw0)
}

/* ------------------------------------------------------------------------
 * bits -> text
 * ---------------------------------------------------------------------- */

// Unpack a BID128 encoding, per IEEE 754-2019 3.5.2, and render it.
func decodeBid128toAscii(out []byte, bid128Hi, bid128Lo uint64) int {
	verify("the field widths tile the encoding", func() bool { return 1+w5+tWidth == 128 })

	// The API's contract is a buffer of at least MaxLen, so that is what is
	// checked -- once, on every call, rather than per rendered form. The
	// public entry points take a *[MaxLen]byte, but that length is gone by
	// the time the slice reaches here, so it is re-established at this
	// boundary.
	demand("the buffer is at least MaxLen bytes", len(out) >= MaxLen)

	sign := (bid128Hi & signBit) != 0
	combination := int((bid128Hi >> (tWidth - 64)) & 0x1FFFF)
	coeffTHi := bid128Hi & ((uint64(1) << (tWidth - 64)) - 1)

	var coeffHi uint64
	var qExp int

	switch {
	case (combination >> (w5 - 2)) != 0x3:
		// G0 G1 is 00, 01 or 10: the exponent is the top of the field and the
		// coefficient's three high bits are the bottom of it.
		biasedExponentE := combination >> 3
		qExp = biasedExponentE - bias
		coeffHi = uint64(combination & 0x07)
	case (combination >> (w5 - 4)) != 0xF:
		// G0 G1 is 11 and G2 G3 is not: the coefficient's high bits are
		// 8 + Gw+4, and the exponent shifts down by two.
		biasedExponentE := (combination >> 1) & ((1 << (w5 - 5 + 2)) - 1)
		qExp = biasedExponentE - bias
		coeffHi = 8 + uint64(combination&1)
	case (combination >> (w5 - 5)) == 0x1E:
		return infAscii(out, sign)
	default:
		verify("the remaining combination field is the NaN pattern", func() bool {
			return (combination >> (w5 - 5)) == 0x1F
		})
		isSignaling := ((combination >> (w5 - 6)) & 1) != 0
		return nanAscii(out, sign, isSignaling, coeffTHi, bid128Lo)
	}

	dw1 := (coeffHi << (tWidth - 64)) | coeffTHi
	dw0 := bid128Lo

	// IEEE 754-2019 3.5.2: a coefficient at or above 10^34 is non-canonical
	// and the value used for it is zero, sign and exponent retained.
	if (dw1|dw0) == 0 ||
		dw1 > maxxCoeff34Hi ||
		(dw1 == maxxCoeff34Hi && dw0 >= maxxCoeff34Lo) {
		return zerAscii(out, sign, qExp)
	}
	return fnzAscii(out, sign, qExp, dw1, dw0)
}

// Unpack a DPD128 encoding and hand the pieces to the same renderers.
func decodeDpd128toAscii(out []byte, dpd128Hi, dpd128Lo uint64) int {
	const w = 12 // exponent continuation width

	// The API's contract is a buffer of at least MaxLen, so that is what is
	// checked -- once, on every call, rather than per rendered form. The
	// public entry points take a *[MaxLen]byte, but that length is gone by
	// the time the slice reaches here, so it is re-established at this
	// boundary.
	demand("the buffer is at least MaxLen bytes", len(out) >= MaxLen)

	sign := (dpd128Hi & signBit) != 0
	combination := int((dpd128Hi >> (tWidth - 64)) & 0x1FFFF)
	// in DPD the low w bits of the biased exponent always sit at the bottom of G
	expContinuation := combination & 0x0FFF

	var d0 uint64
	var qExp int
	isNaN := false
	isSignaling := false

	switch {
	case (combination >> (w5 - 2)) != 0x3:
		// leading digit 0..7, exponent MSBs in G0 G1
		expMSB := combination >> (w5 - 2)
		qExp = ((expMSB << w) | expContinuation) - bias
		d0 = uint64((combination >> w) & 0x7)
	case (combination >> (w5 - 4)) != 0xF:
		// leading digit 8 or 9, exponent MSBs displaced into G2 G3
		expMSB := (combination >> (w5 - 4)) & 0x3
		qExp = ((expMSB << w) | expContinuation) - bias
		d0 = 8 + uint64((combination>>w)&0x1)
	case (combination >> (w5 - 5)) == 0x1E:
		return infAscii(out, sign)
	default:
		verify("the remaining combination field is the NaN pattern", func() bool {
			return (combination >> (w5 - 5)) == 0x1F
		})
		isNaN = true
		isSignaling = ((combination >> (w5 - 6)) & 1) != 0
		d0 = 0
		qExp = 0
	}

	// Rebuild the binary coefficient from the declets. The leading digit rides
	// along as a sixth declet of the high group: 0..9 encodes to itself.
	decletsHi := (d0 << 50) | ((dpd128Hi & 0x3FFFFFFFFFFF) << 4) | (dpd128Lo >> 60)
	decletsLo := dpd128Lo & 0x0FFFFFFFFFFFFFFF // low 60 bits, six declets
	binHi := binFromDeclets(decletsHi)
	binLo := binFromDeclets(decletsLo)

	var dw1, dw0 uint64
	if binHi == 0 {
		dw1 = 0
		dw0 = binLo
	} else {
		prodHi, prodLo := bits.Mul64(binHi, tenPow18)
		sumLo, carry := bits.Add64(prodLo, binLo, 0)
		dw1 = prodHi + carry
		dw0 = sumLo
	}

	if isNaN {
		return nanAscii(out, sign, isSignaling, dw1, dw0)
	}
	if (dw1 | dw0) == 0 {
		return zerAscii(out, sign, qExp)
	}
	return fnzAscii(out, sign, qExp, dw1, dw0)
}

/* --------------------------------------------------------------------------
 * Packing a finalized value into an encoding
 *
 * Everything upstream of these -- parsing, rounding, clamping, the underflow
 * region -- is format-neutral; the two formats diverge only here.
 * ------------------------------------------------------------------------ */

func bid128SetFinite(sign bool, qExp int, dw1, dw0 uint64) (hi, lo uint64) {
	verify("the quantum exponent is in range", func() bool { return qExp >= qMin && qExp <= qMax })
	verify("the coefficient is at most 34 digits", func() bool { return calcDigitLen128(dw1, dw0) <= 34 })
	qBiased := uint64(qExp + bias)
	s := uint64(0)
	if sign {
		s = signBit
	}
	g := qBiased << (3 + 46)
	return s | g | dw1, dw0
}

/*
 * Pack a finite value into DPD128: the same sign and the same 14-bit biased
 * exponent as BID, and a coefficient of the same 34 digits laid out as a
 * leading digit in the combination field plus eleven declets.
 *
 * The leading digit cannot be packed independently of the exponent -- DPD puts
 * it in the same five-bit prefix as the exponent's top two bits -- so unlike
 * BID there is no shared sign-and-exponent step to factor out.
 */
func dpd128SetFinite(sign bool, qExp int, dw1, dw0 uint64) (hi, lo uint64) {
	verify("the quantum exponent is in range", func() bool { return qExp >= qMin && qExp <= qMax })
	verify("the coefficient is at most 34 digits", func() bool { return calcDigitLen128(dw1, dw0) <= 34 })

	// Split the coefficient into its low 18 digits and its high 16. Two
	// divisions by 10^9 rather than one by 10^18, so the existing 128-bit
	// kernel does the work and no wider machinery is needed.
	tHi, tLo, r1 := u113DivModPow10Max9(dw1, dw0, 9)
	tHi, tLo, r2 := u113DivModPow10Max9(tHi, tLo, 9)
	binLo := (r2 * 1000000000) + r1
	verify("the high 16 digits fit in one word", func() bool { return tHi == 0 })
	binHi := tLo

	// the top digit rides in the combination field; the other 15 are declets
	d0 := binHi / tenPow15
	declets5Hi := decletsFromBin(binHi - (d0 * tenPow15))
	declets6Lo := decletsFromBin(binLo)
	verify("the leading digit is a single digit", func() bool { return d0 <= 9 })

	biasedQExp := qExp + bias
	expMSB := (biasedQExp >> 12) & 0x3
	expLSB := biasedQExp & 0x0FFF
	var gPrefix int
	if d0 <= 7 {
		gPrefix = (expMSB << 3) | int(d0) // G0G1 = exponent MSBs
	} else {
		gPrefix = 0x18 | (expMSB << 1) | (int(d0) & 1) // G0G1 = 11, digit is 8 or 9
	}
	signValue := uint64(0)
	if sign {
		signValue = 1
	}
	signCombo := (signValue << 17) | (uint64(gPrefix) << 12) | uint64(expLSB)

	return (signCombo << 46) | (declets5Hi >> 4), (declets5Hi << 60) | declets6Lo
}

func setFinite(formatIsBid, sign bool, qExp int, dw1, dw0 uint64) (hi, lo uint64) {
	if formatIsBid {
		return bid128SetFinite(sign, qExp, dw1, dw0)
	}
	return dpd128SetFinite(sign, qExp, dw1, dw0)
}

func bid128SetNaN(sign, isSignaling bool, payloadDw1, payloadDw0 uint64) (hi, lo uint64) {
	withoutPayload := uint64(qnanPatternHi)
	if isSignaling {
		withoutPayload = snanPatternHi
	}
	if sign {
		withoutPayload |= signBit
	}
	return withoutPayload | payloadDw1, payloadDw0
}

/*
 * Pack a NaN into DPD128. The payload occupies the whole 110-bit trailing
 * field as eleven declets -- 33 digits, with no leading digit in the
 * combination field, since G0..G4 are fixed at 11111 and G5 carries the
 * signaling flag.
 */
func dpd128SetNaN(sign, isSignaling bool, payloadDw1, payloadDw0 uint64) (hi, lo uint64) {
	verify("the payload is at most 33 digits", func() bool {
		return calcDigitLen128(payloadDw1, payloadDw0) <= 33
	})

	tHi, tLo, r1 := u113DivModPow10Max9(payloadDw1, payloadDw0, 9)
	tHi, tLo, r2 := u113DivModPow10Max9(tHi, tLo, 9)
	binLo := (r2 * 1000000000) + r1
	verify("a 33-digit payload leaves 15 digits up top", func() bool { return tHi == 0 })
	declets5Hi := decletsFromBin(tLo)
	declets6Lo := decletsFromBin(binLo)

	gField := uint64(0x1F) << 12
	if isSignaling {
		gField |= 1 << 11
	}
	signValue := uint64(0)
	if sign {
		signValue = 1
	}
	signCombo := (signValue << 17) | gField
	return (signCombo << 46) | (declets5Hi >> 4), (declets5Hi << 60) | declets6Lo
}

func setNaN(formatIsBid, sign, isSignaling bool, payloadDw1, payloadDw0 uint64) (hi, lo uint64) {
	if formatIsBid {
		return bid128SetNaN(sign, isSignaling, payloadDw1, payloadDw0)
	}
	return dpd128SetNaN(sign, isSignaling, payloadDw1, payloadDw0)
}

/* ------------------------------------------------------------------------
 * Finalize: rounding, clamping, underflow
 * ---------------------------------------------------------------------- */

func finalizeOverflow(sign bool) (hi, lo uint64) {
	s := uint64(0)
	if sign {
		s = signBit
	}
	return s | infPatternHi, 0
}

// Does this coefficient/exponent pair fit as a canonical encoding?
func coeffQExpFit(dw1, dw0 uint64, qExp int) bool {
	if qExp < qMin || qExp > qMax {
		return false
	}
	// at most 113 bits is necessarily below 10^34, since 2^113 > 10^34
	if clz64(dw1) > 128-113 {
		return true
	}
	return dw1 < maxxCoeff34Hi || (dw1 == maxxCoeff34Hi && dw0 < maxxCoeff34Lo)
}

/*
 * A finite value whose quantum exponent exceeds qMax but whose coefficient has
 * leading capacity: absorb the excess into the coefficient, bringing the
 * exponent down to exactly qMax without changing the value.
 */
func finalizeClamping(formatIsBid, sign bool, qExp int, dw1, dw0 uint64) (hi, lo uint64) {
	qExcess := qExp - qMax
	verify("the excess exponent fits the coefficient's spare digits", func() bool {
		return qExcess > 0 && qExcess <= 34-calcDigitLen128(dw1, dw0)
	})
	pow10Offset := qExcess << 1
	y1 := pow10Table[pow10Offset+1]
	y0 := pow10Table[pow10Offset]
	pp00Hi, pp00Lo := bits.Mul64(dw0, y0)
	pp10Lo := dw1 * y0
	pp01Lo := dw0 * y1
	dw0Scaled := pp00Lo
	dw1Scaled := pp00Hi + pp10Lo + pp01Lo
	verify("the scaled coefficient is representable at qMax", func() bool {
		return coeffQExpFit(dw1Scaled, dw0Scaled, qMax)
	})
	return setFinite(formatIsBid, sign, qMax, dw1Scaled, dw0Scaled)
}

/*
 * The underflow boundary: the coefficient's most significant digit sits
 * exactly one position right of the representable range, so the coefficient
 * itself decides the rounding. Under ties-to-even the outcome is zero unless
 * the total residue is greater than half, zero being even.
 */
func finalizeUnderflowBoundary(formatIsBid, sign bool, dw1, dw0 uint64, residueIn int) (hi, lo uint64) {
	digitLen := calcDigitLen128(dw1, dw0)
	verify("the boundary coefficient is nonzero", func() bool { return digitLen != 0 })
	scaleResidue := residueFromValuePow10(dw1, dw0, digitLen)
	totalResidue := residueMerge(scaleResidue, residueIn)
	coeff := uint64(0)
	if totalResidue == gtHalf {
		coeff = 1
	}
	return setFinite(formatIsBid, sign, qMin, 0, coeff)
}

// Representable as a subnormal: truncate to the minimum exponent, keeping
// what precision survives, and round what is left.
func finalizeSubnormal(formatIsBid, sign bool, qExpIn int, dw1In, dw0In uint64, residueIn int) (hi, lo uint64) {
	truncationNeeded := qMin - qExpIn
	verify("the subnormal truncation keeps at least one digit", func() bool {
		return truncationNeeded > 0 && truncationNeeded < calcDigitLen128(dw1In, dw0In)
	})

	dw1T, dw0T, scaleResidue := u128ScaleDownPow10(dw1In, dw0In, truncationNeeded)
	totalResidue := residueMerge(scaleResidue, residueIn)
	qExpT := qMin

	roundUp := (totalResidue == gtHalf) || (totalResidue == tieHalf && (dw0T&1) != 0)
	if roundUp {
		dw0T++
		if dw0T == 0 {
			dw1T++
		}
		if dw1T == maxxCoeff34Hi && dw0T == maxxCoeff34Lo {
			dw1T = minPrecision34CoeffHi
			dw0T = minPrecision34CoeffLo
			qExpT++
		}
	}
	return setFinite(formatIsBid, sign, qExpT, dw1T, dw0T)
}

// Dispatch the three underflow cases: swamped to zero, the exact boundary,
// and the subnormal region.
func finalizeUnderflowRegion(formatIsBid, sign bool, qExp int, dw1, dw0 uint64, residue int) (hi, lo uint64) {
	// IEEE 754-2019 7.5 Underflow -- the subnormal region
	verify("the exponent is below the minimum", func() bool { return qExp < qMin })
	truncationNeeded := qMin - qExp
	digitLen := calcDigitLen128(dw1, dw0)
	verify("the coefficient is at most 34 digits", func() bool { return digitLen <= 34 })
	switch {
	case truncationNeeded > digitLen:
		// below half the smallest subnormal: zero, and always inexact
		return roundAndFinalizeFinite(formatIsBid, sign, qMin, 0, 0, ltHalf)
	case truncationNeeded == digitLen:
		return finalizeUnderflowBoundary(formatIsBid, sign, dw1, dw0, residue)
	default:
		return finalizeSubnormal(formatIsBid, sign, qExp, dw1, dw0, residue)
	}
}

/*
 * Round a parsed coefficient/exponent pair to a canonical encoding, under
 * ties-to-even, and pack it.
 *
 *  1. fast path -- already in range and no rounding to do
 *  2. a zero coefficient with an out-of-range exponent clamps
 *  3. (special values never arrive here)
 *  4. underflow, when the exponent is below the minimum
 *  5. truncate to 34 digits -- REMOVED as unreachable, see the note in the body
 *  6. round, and renormalize if the coefficient rolls over to 10^34
 *  7. overflow to Infinity, or clamp near Emax
 */
func roundAndFinalizeFinite(formatIsBid, sign bool, qExpIn int, dw1In, dw0In uint64, residueIn int) (hi, lo uint64) {
	// Step 1
	if residueIn <= ltHalf || (residueIn == tieHalf && (dw0In&1) == 0) {
		if coeffQExpFit(dw1In, dw0In, qExpIn) {
			return setFinite(formatIsBid, sign, qExpIn, dw1In, dw0In)
		}
		// Step 2
		if (dw1In | dw0In) == 0 {
			qClamped := qExpIn
			if qClamped > qMax {
				qClamped = qMax
			}
			if qClamped < qMin {
				qClamped = qMin
			}
			return setFinite(formatIsBid, sign, qClamped, 0, 0)
		}
	}

	/*
	 * Step 5 -- precision truncation -- IS NOT HERE, and this is the reason.
	 *
	 * The finalizer this descends from serves an arithmetic engine, whose
	 * callers hand it coefficients wider than the 34-digit precision limit:
	 * multiply and FMA produce a double-width product, and the engine
	 * truncates it here, accumulating the discarded digits into the residue
	 * before rounding. A codec has no such caller.
	 *
	 * The parser accumulates at most 19 + 15 = 34 significant digits and turns
	 * every digit past them into a residue instead, so the coefficient it
	 * hands over is never wider than the limit. The only other entry to this
	 * function is the swamped-underflow branch above, which passes a zero
	 * coefficient. So the truncation step is unreachable, and rather than ship
	 * a path that has never once executed -- and therefore has never been
	 * verified to work -- it is removed and the invariant is enforced.
	 *
	 * Measured: zero occurrences across the 87k-row corpus and
	 * 400,000 random numerals, and across 76,280 finalizer calls driven by
	 * inputs of up to 5,000 digits, where the widest coefficient ever seen was
	 * 34.
	 *
	 * This is `demand`, not `verify`: it must survive into shipping builds,
	 * because falling through would encode an out-of-range coefficient
	 * silently. It cannot fire on a consumer's data -- no input reaches it --
	 * only on a future caller that breaks the invariant, which is exactly when
	 * a loud failure is what you want. A caller that is not a parser, or one
	 * that parses differently, needs the truncation step back.
	 */
	demand("the coefficient is at most 34 digits", calcDigitLen128(dw1In, dw0In) <= 34)

	// Step 4 -- with no precision truncation to outrank it, the underflow test
	// is simply whether the exponent is below the minimum.
	if qMin-qExpIn > 0 {
		return finalizeUnderflowRegion(formatIsBid, sign, qExpIn, dw1In, dw0In, residueIn)
	}

	totalResidue := residueIn
	dw1 := dw1In
	dw0 := dw0In
	qExp := qExpIn

	// Step 6 -- ties-to-even, the only rounding this codec has
	applyRounding := (totalResidue == gtHalf) || (totalResidue == tieHalf && (dw0&1) != 0)
	if applyRounding {
		dw0++
		if dw0 == 0 {
			dw1++
		}
		if dw1 == maxxCoeff34Hi && dw0 == maxxCoeff34Lo {
			dw1 = minPrecision34CoeffHi
			dw0 = minPrecision34CoeffLo
			qExp++
		}
	}

	// Step 7
	verify("the exponent is at or above the minimum", func() bool { return qExp >= qMin })
	verify("the rounded coefficient is at most 34 digits", func() bool { return calcDigitLen128(dw1, dw0) <= 34 })
	if qExp > qMax {
		qExcess := qExp - qMax
		digitLen := calcDigitLen128(dw1, dw0)
		if digitLen+qExcess <= 34 {
			return finalizeClamping(formatIsBid, sign, qExp, dw1, dw0)
		}
		return finalizeOverflow(sign)
	}
	return setFinite(formatIsBid, sign, qExp, dw1, dw0)
}

/* ------------------------------------------------------------------------
 * text -> bits
 * ---------------------------------------------------------------------- */

/*
 * The code of the byte at index, or -1 past the end of the string.
 *
 * The parsers walk the input as byte codes, so end-of-input is one more code
 * value rather than a bound test at every read. -1 is not a byte, so a NUL in
 * the string stays an ordinary -- and invalid -- input byte rather than
 * doubling as the terminator: a Go string carries its own length and can hold
 * one, and it is rejected.
 *
 * In Go ++ is a statement rather than an expression, so the advance is its own
 * line at each call rather than riding in the index.
 */
func codeAt(str string, index int) int {
	if index >= 0 && index < len(str) {
		return int(str[index])
	}
	return -1
}

// The digit test, spelled out once. unicode.IsDigit takes a rune and is
// Unicode-aware, neither of which suits the int the reader yields.
func isDigit(ch int) bool {
	return ch >= '0' && ch <= '9'
}

/*
 * Parse a finite numeral: optional radix point, optional E exponent, any
 * number of digits.
 *
 * Coefficients with more than 34 significant digits are accepted; leading
 * zeros are flushed, and the digits past 34 are tracked as residue for the
 * ties-to-even rounding that follows. Exponent magnitudes clamp to 9999,
 * which is enough for finalize to detect overflow or underflow.
 *
 * Leading zeros can be flushed branchlessly by coercing a comparison to an
 * integer; Go has no bool-to-int conversion, so the two counters below say the
 * same thing with an if.
 */
func parseFiniteValueText(formatIsBid, sign bool, chFirst int, str string, startPos int) (hi, lo uint64, err error) {
	pos := startPos
	residue := exact

	hasCoefficientDigit := false // any digit at all, including zero
	significantDigitCount := 0   // does not count leading zeros
	hasDot := false
	expSign := false
	// an underscore is a digit separator: legal only between two digits, so it
	// must follow a digit and the next character must be a digit too
	prevWasDigit := false
	afterUnderscore := false

	verify("the first character was already read", func() bool { return chFirst != -1 })
	ch := chFirst
	fractionalDigitCount := 0
	accum19a := uint64(0)
	accum19b := uint64(0)
	exp := 0

	for isDigit(ch) || ch == '.' || ch == '_' {
		switch {
		case isDigit(ch):
			d := ch - '0'
			hasCoefficientDigit = true
			// count while flushing leading zeros
			if (significantDigitCount | d) != 0 {
				significantDigitCount++
			}
			switch {
			case significantDigitCount <= 19:
				accum19a = (accum19a * 10) + uint64(d)
			case significantDigitCount <= 34:
				accum19b = (accum19b * 10) + uint64(d)
			case significantDigitCount == 34+1:
				residue = residueFromDecimalDigit(d)
			default:
				residue = residueMerge(residue, residueFromDecimalDigit(d))
			}
			if hasDot {
				fractionalDigitCount++
			}
			prevWasDigit = true
			afterUnderscore = false
		case ch == '_':
			if !prevWasDigit {
				return 0, 0, errUnderscoreLocation
			}
			prevWasDigit = false
			afterUnderscore = true
		default: // '.'
			if afterUnderscore {
				return 0, 0, errUnderscoreLocation
			}
			if hasDot {
				return 0, 0, errDoubleRadixDot
			}
			hasDot = true
			prevWasDigit = false
		}
		ch = codeAt(str, pos)
		pos++
	}
	if afterUnderscore {
		return 0, 0, errUnderscoreLocation
	}
	if !hasCoefficientDigit {
		return 0, 0, errNoCoefficientDigit
	}
	// this path has at least one digit
	if ch == 'E' || ch == 'e' {
		ch = codeAt(str, pos)
		pos++
		if ch == '+' || ch == '-' {
			expSign = (ch == '-')
			ch = codeAt(str, pos)
			pos++
		}
		hasExpDigit := false
		expSignificantDigitCount := 0
		prevWasDigit = false
		afterUnderscore = false
		for isDigit(ch) || ch == '_' {
			if ch == '_' {
				if !prevWasDigit {
					return 0, 0, errUnderscoreLocation
				}
				prevWasDigit = false
				afterUnderscore = true
			} else {
				hasExpDigit = true
				eDigit := ch - '0'
				// count while flushing leading zeros
				if (expSignificantDigitCount | eDigit) != 0 {
					expSignificantDigitCount++
				}
				// A long exponent field runs this past the width of an int and
				// wraps, which Go defines and this code relies on: the value is
				// thrown away by the clamp below the moment the field is more
				// than four significant digits wide.
				exp = exp*10 + eDigit
				prevWasDigit = true
				afterUnderscore = false
			}
			ch = codeAt(str, pos)
			pos++
		}
		if afterUnderscore {
			return 0, 0, errUnderscoreLocation
		}
		if !hasExpDigit {
			return 0, 0, errNoExponentDigit
		}
		// clamp once after the loop
		if expSignificantDigitCount > 4 {
			exp = 9999
		}
	}
	if ch != -1 {
		return 0, 0, errUnexpectedChar
	}

	dw0 := accum19a
	dw1 := uint64(0)
	if significantDigitCount > 19 {
		capped := significantDigitCount
		if capped > 34 {
			capped = 34
		}
		dw1, dw0 = u128FmaPow10(accum19a, capped-19, accum19b)
	}

	/*
	 * Excess digits to the right of the radix point and excess digits beyond
	 * the precision limit pull the quantum exponent in opposite directions,
	 * so both corrections land here.
	 */
	signedExp := exp
	if expSign {
		signedExp = -exp
	}
	excessDigits := 0
	if significantDigitCount > 34 {
		excessDigits = significantDigitCount - 34
	}
	qExp := signedExp - fractionalDigitCount + excessDigits
	hi, lo = roundAndFinalizeFinite(formatIsBid, sign, qExp, dw1, dw0, residue)
	return hi, lo, nil
}

// Parse Infinity, or its three-letter abbreviation Inf, case-insensitively.
func parseInfinityText(sign bool, chFirst int, str string, startPos int) (hi, lo uint64, err error) {
	pos := startPos
	ch := chFirst
	chPrevCode := 0
	const target = "infinity"
	for i := 0; i < 8; i++ {
		targetCode := int(target[i])
		if (ch | 0x20) != targetCode {
			if ch == -1 {
				break
			}
			return 0, 0, errParsingInfinity
		}
		chPrevCode = ch | 0x20
		ch = codeAt(str, pos)
		pos++
	}
	if ch != -1 || (chPrevCode != 'f' && chPrevCode != 'y') {
		return 0, 0, errParsingInfinity
	}
	s := uint64(0)
	if sign {
		s = signBit
	}
	return infPatternHi | s, 0, nil
}

/*
 * Parse NaN, qNaN or sNaN, case-insensitively, with an optional payload of
 * decimal digits. A payload of 34 or more digits AS WRITTEN is non-canonical
 * and reads as zero, per IEEE 754-2019 3.5.2.
 *
 * The payload may be written bare (NaN123) or enclosed in parentheses
 * (NaN(123)). The parenthesised form is positional and balanced: the '(' must
 * follow the word immediately, at least one digit must appear, and the ')'
 * must be the last character of the input. So NaN(1)2(3), NaN(1, NaN(1] and
 * NaN() are all rejected, and NaN[1] and NaN{1} are not this form at all.
 *
 * This spelling exists because the platform's own strtod reads
 * NAN(n-char-sequence), so it reaches a caller from the platform
 * itself -- Go's strconv.ParseFloat does not read it, but it is part of the
 * grammar accepted here. The resemblance to
 * strtod stops at the parentheses: its sequence is letters, digits and
 * underscores read as an implementation-defined integer, while a decimal128
 * payload is decimal digits.
 */
func parseNanText(formatIsBid, sign bool, chFirst int, str string, startPos int) (hi, lo uint64, err error) {
	pos := startPos
	ch := chFirst
	hasS := (ch | 0x20) == 's'
	hasQ := (ch | 0x20) == 'q'
	if hasQ || hasS {
		ch = codeAt(str, pos)
		pos++
	}
	if ((ch | 0x20) != 'n') ||
		((codeAt(str, pos) | 0x20) != 'a') ||
		((codeAt(str, pos+1) | 0x20) != 'n') {
		return 0, 0, errParsingNaN
	}
	pos += 2

	payloadDw0 := uint64(0)
	payloadDw1 := uint64(0)
	ch = codeAt(str, pos)
	pos++
	hasParen := ch == '('
	if hasParen {
		ch = codeAt(str, pos)
		pos++
	}
	if hasParen || ch != -1 {
		accumDigitCount := 0
		accum19a := uint64(0)
		accum19b := uint64(0)
		for isDigit(ch) {
			d := ch - '0'
			// Count every digit written, leading zeros included. The 33-digit
			// limit is a limit on the payload as spelled, not on its
			// significant digits, so "NaN" followed by forty zeros and a one is
			// over the limit and reads as zero. Leading zeros contribute
			// nothing to the accumulators, so the value of a payload within the
			// limit is unaffected -- and an over-limit payload wraps these
			// accumulators, which is defined here and discarded below.
			accumDigitCount++
			if accumDigitCount <= 19 {
				accum19a = (accum19a * 10) + uint64(d)
			} else {
				accum19b = (accum19b * 10) + uint64(d)
			}
			ch = codeAt(str, pos)
			pos++
		}
		if accumDigitCount == 0 {
			return 0, 0, errParsingNaN
		}
		if hasParen {
			// the ')' must be present and must end the input
			if ch != ')' {
				return 0, 0, errParsingNaN
			}
			if codeAt(str, pos) != -1 {
				return 0, 0, errParsingNaN
			}
		} else if ch != -1 {
			return 0, 0, errParsingNaN
		}
		if accumDigitCount > 33 {
			payloadDw0 = 0
			payloadDw1 = 0
		} else {
			payloadDw0 = accum19a
			payloadDw1 = 0
			if accumDigitCount > 19 {
				payloadDw1, payloadDw0 = u128FmaPow10(accum19a, accumDigitCount-19, accum19b)
			}
		}
	}
	hi, lo = setNaN(formatIsBid, sign, hasS, payloadDw1, payloadDw0)
	return hi, lo, nil
}

// Read the optional sign, then dispatch on the leading character.
func encodeStringOrError(formatIsBid bool, str string) (hi, lo uint64, err error) {
	pos := 0
	chFirst := codeAt(str, pos)
	pos++
	ch := chFirst
	sign := ch == '-'
	if ch == '+' || ch == '-' {
		ch = codeAt(str, pos)
		pos++
	}
	if isDigit(ch) || ch == '.' {
		return parseFiniteValueText(formatIsBid, sign, ch, str, pos)
	}
	chLower := ch | 0x20
	if chLower == 'i' {
		return parseInfinityText(sign, ch, str, pos)
	}
	if chLower == 'n' || chLower == 's' || chLower == 'q' {
		return parseNanText(formatIsBid, sign, ch, str, pos)
	}
	if chFirst == -1 {
		return 0, 0, errEmptyString
	}
	return 0, 0, errInvalidFormat
}
