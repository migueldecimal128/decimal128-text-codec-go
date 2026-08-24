// SPDX-License-Identifier: MIT

//go:build !verify

package decimal128textcodec

// verifyEnabled is false by default: verify compiles to nothing, the thunks at
// its call sites are never allocated, and the predicates inside them are dead
// code the optimizer drops. A consumer linking this package gets no assertion
// they did not ask for. Build with `-tags verify` (verify_on.go) to arm them.
const verifyEnabled = false
