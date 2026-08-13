package spool

// AddLatest queues a frame that supersedes any pending one of the same name.
func (s *Spool) AddLatest(object string, data []byte) error {
	return s.Add(Latest, object, data)
}

// AddArchive queues a frame that must not be superseded.
func (s *Spool) AddArchive(object string, data []byte) error {
	return s.Add(Archive, object, data)
}
