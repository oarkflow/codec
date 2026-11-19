package store

func (s *segment) mapReadOnly() {
	if s == nil || s.mapFailed || len(s.mapped) != 0 {
		return
	}
	b, err := mapReadOnly(s.f, s.size)
	if err == nil && len(b) > 0 {
		s.mapped = b
	} else {
		s.mapFailed = true
	}
}

func (s *segment) unmap() {
	if s == nil || len(s.mapped) == 0 {
		return
	}
	_ = unmapReadOnly(s.mapped)
	s.mapped = nil
}
