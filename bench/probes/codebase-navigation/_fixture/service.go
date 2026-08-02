package app

// Service coordinates persistence for the app.
type Service struct {
	store *Store
}

// Register persists a single new record via Store.Save.
func (svc *Service) Register(r Record) error {
	return svc.store.Save(r)
}

// Import bulk-loads records, persisting each one through Store.Save.
func Import(s *Store, rs []Record) error {
	for _, r := range rs {
		if err := s.Save(r); err != nil {
			return err
		}
	}
	return nil
}
