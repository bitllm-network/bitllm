//go:build race

package members_test

// raceDetector is whether this test binary runs under the race detector, which
// makes timing bounds meaningless.
const raceDetector = true
