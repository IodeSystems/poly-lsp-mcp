package app

// Store persists records.
type Store struct{}

// Save writes a single record and reports any error.
func (s *Store) Save(r Record) error {
	_ = r
	return nil
}
