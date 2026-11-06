package account

// SetLimits lowers the bounds so a test does not have to sign thousands of claims.
func (cs *Claims) SetLimits(total, pending, owner int) {
	cs.maxTotal, cs.maxPending, cs.maxOwner = total, pending, owner
}
