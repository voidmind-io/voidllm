package proxy

// transformLine1 calls TransformStreamLine and returns the first output line,
// or nil when the result is empty/nil. It ignores errors so that existing tests
// that exercise the happy path remain unchanged. Tests that specifically verify
// abort behaviour must call TransformStreamLine directly and inspect the error.
func transformLine1(a Adapter, line []byte) []byte {
	lines, _ := a.TransformStreamLine(line)
	if len(lines) == 0 {
		return nil
	}
	return lines[0]
}

// transformLineIgnore calls TransformStreamLine and discards all return values.
// It is used for priming calls where only the adapter side-effects matter.
func transformLineIgnore(a Adapter, line []byte) {
	_, _ = a.TransformStreamLine(line)
}
