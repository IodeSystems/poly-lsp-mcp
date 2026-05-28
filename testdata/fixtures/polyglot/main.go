package main

import "fmt"

// UserID identifies a user across services.
type UserID int64

func GreetUser(id UserID) string {
	return fmt.Sprintf("hello user %d", id)
}

func main() {
	fmt.Println(GreetUser(42))
}
