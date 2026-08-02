package app

import "net/http"

// Server dispatches HTTP requests.
type Server struct{}

// Handle dispatches req and returns the built Response.
func (srv *Server) Handle(req *http.Request) *Response {
	_ = req
	return &Response{Status: 200}
}

// Response is an HTTP response envelope.
type Response struct {
	Status int
}
