package server

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed webapp
var webappFiles embed.FS

const webappPolicy = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; manifest-src 'self'; worker-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"

// webappHandler serves the static phone client. It holds no secrets, so it
// is public; every API call it makes carries the user's bearer token.
func webappHandler() http.Handler {
	files, err := fs.Sub(webappFiles, "webapp")
	if err != nil {
		panic(err)
	}
	static := http.StripPrefix("/app/", http.FileServer(http.FS(files)))
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		headers := response.Header()
		headers.Set("Content-Security-Policy", webappPolicy)
		headers.Set("X-Content-Type-Options", "nosniff")
		headers.Set("Referrer-Policy", "no-referrer")
		headers.Set("Cache-Control", "no-cache")
		static.ServeHTTP(response, request)
	})
}
