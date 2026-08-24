// SPDX-License-Identifier: MIT

//go:build verify

package decimal128textcodec

// verifyEnabled is true under `-tags verify`: every verify state assertion is
// evaluated, routing through impossible on failure. The test job in CI runs the
// whole conformance corpus this way, which is what makes the assertions worth
// writing at all.
const verifyEnabled = true
