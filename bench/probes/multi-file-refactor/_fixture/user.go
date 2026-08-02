package userstore

// User is a stored user record.
type User struct {
	ID   UserID
	Name string
}

// NewUser builds a User from an id and name.
func NewUser(id UserID, name string) User {
	return User{ID: id, Name: name}
}
