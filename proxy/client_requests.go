package main

import (
	"mime"
	"net/http"
	"strings"
)

// acceptOnlyJSONFromClients lets through only requests of the form the proxy's
// own clients send. The TUI, the CLI and the editor plugins send no Origin
// header, and send each POST body as application/json. A request with an Origin
// header comes from a web page in a browser, and is refused unless the operator
// put that origin on ATLAS_ALLOWED_ORIGINS (comma-separated, empty by default).
func acceptOnlyJSONFromClients(next http.Handler) http.Handler {
	allowed := allowedOrigins()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" && !allowed[origin] {
			writeError(w, http.StatusForbidden, ErrUnauthorized,
				"a request with an Origin header is refused; put the origin on ATLAS_ALLOWED_ORIGINS to allow it")
			return
		}
		if r.Method == http.MethodPost && !isJSON(r.Header.Get("Content-Type")) {
			writeError(w, http.StatusUnsupportedMediaType, ErrUnsupported,
				"send the request body with Content-Type: application/json")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func allowedOrigins() map[string]bool {
	allowed := map[string]bool{}
	for _, origin := range strings.Split(envOr("ATLAS_ALLOWED_ORIGINS", ""), ",") {
		if origin = strings.TrimSpace(origin); origin != "" {
			allowed[origin] = true
		}
	}
	return allowed
}

func isJSON(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	return err == nil && mediaType == "application/json"
}
