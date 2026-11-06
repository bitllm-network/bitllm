package members

// Computes counts how many times the member set was derived.
func (m *Members) Computes() int64 { return m.computes.Load() }

// Writes counts how many times the file was written.
func (m *Members) Writes() int64 { return m.save.Writes() }
