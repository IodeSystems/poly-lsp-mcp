package userstore

// Store holds users keyed by UserID.
type Store struct {
	users map[UserID]User
}

// NewStore returns an empty Store.
func NewStore() *Store {
	return &Store{users: make(map[UserID]User)}
}

// Get returns the user stored under id.
func (s *Store) Get(id UserID) (User, bool) {
	u, ok := s.users[id]
	return u, ok
}

// Put stores u keyed by its ID.
func (s *Store) Put(u User) {
	s.users[u.ID] = u
}
