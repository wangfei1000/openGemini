package httpd

import (
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/openGemini/openGemini/open_src/influx/influxql"
)

// responseLogger is wrapper of http.ResponseWriter that keeps track of its HTTP status
// code and body size
type responseLogger struct {
	w      http.ResponseWriter
	status int
	size   int
}

func (l *responseLogger) CloseNotify() <-chan bool {
	if notifier, ok := l.w.(http.CloseNotifier); ok {
		return notifier.CloseNotify()
	}
	// needed for response recorder for testing
	return make(<-chan bool)
}

func (l *responseLogger) Header() http.Header {
	return l.w.Header()
}

func (l *responseLogger) Flush() {
	l.w.(http.Flusher).Flush()
}

func (l *responseLogger) Write(b []byte) (int, error) {
	if l.status == 0 {
		// Set status if WriteHeader has not been called
		l.status = http.StatusOK
	}

	size, err := l.w.Write(b)
	l.size += size
	return size, err
}

func (l *responseLogger) WriteHeader(s int) {
	l.w.WriteHeader(s)
	l.status = s
}

func (l *responseLogger) Status() int {
	if l.status == 0 {
		// This can happen if we never actually write data, but only set response headers.
		l.status = http.StatusOK
	}
	return l.status
}

func (l *responseLogger) Size() int {
	return l.size
}

// redact any occurrence of a password parameter, 'p'
func redactPassword(r *http.Request) {
	q := r.URL.Query()
	if p := q.Get("p"); p != "" {
		q.Set("p", "[REDACTED]")
		r.URL.RawQuery = q.Encode()
	}
}

func findSensitivePos(url string) int {
	sensitiveInfo1, sensitiveInfo2 := "+password+", "+passwd+"
	passwordIndex := strings.Index(url, sensitiveInfo1)
	if passwordIndex != -1 {
		return passwordIndex + len(sensitiveInfo1)
	}
	passwdIndex := strings.Index(url, sensitiveInfo2)
	if passwdIndex != -1 {
		return passwdIndex + len(sensitiveInfo2)
	}
	return -1
}

//in some situations, encode password still has sensitive info
func hideUrlPassword(url string) string {
	url = strings.ToLower(url)
	processIndex := findSensitivePos(url)
	if processIndex == -1 {
		return url
	}

	var buf strings.Builder
	buf.WriteString(url[:processIndex])
	buf.WriteString("[REDACTED]")
	plusIndex := strings.Index(url[processIndex:], "+")
	if plusIndex != -1 {
		buf.WriteString(url[processIndex+plusIndex:])
	}
	return buf.String()
}

// Common Log Format: http://en.wikipedia.org/wiki/Common_Log_Format

// buildLogLine creates a common log format
// in addition to the common fields, we also append referrer, user agent,
// request ID and response time (microseconds)
// ie, in apache mod_log_config terms:
//     %h %l %u %t \"%r\" %>s %b \"%{Referer}i\" \"%{User-agent}i\"" %L %D
func buildLogLine(l *responseLogger, r *http.Request, start time.Time) string {

	redactPassword(r)

	username := parseUsername(r)

	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}

	if xff := r.Header["X-Forwarded-For"]; xff != nil {
		addrs := append(xff, host)
		host = strings.Join(addrs, ",")
	}

	uri := hideUrlPassword(r.URL.RequestURI())

	referer := r.Referer()

	userAgent := r.UserAgent()

	return fmt.Sprintf(`%s - %s [%s] "%s %s %s" %s %s "%s" "%s" %s %d`,
		host,
		detect(username, "-"),
		start.Format("02/Jan/2006:15:04:05 -0700"),
		r.Method,
		uri,
		r.Proto,
		detect(strconv.Itoa(l.Status()), "-"),
		strconv.Itoa(l.Size()),
		detect(referer, "-"),
		detect(userAgent, "-"),
		r.Header.Get("Request-Id"),
		// response time, report in microseconds because this is consistent
		// with apache's %D parameter in mod_log_config
		int64(time.Since(start)/time.Microsecond))
}

// detect detects the first presence of a non blank string and returns it
func detect(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// parses the username either from the url or auth header
func parseUsername(r *http.Request) string {
	var (
		username = ""
		url      = r.URL
	)

	// get username from the url if passed there
	if url.User != nil {
		if name := url.User.Username(); name != "" {
			username = name
		}
	}

	// Try to get the username from the query param 'u'
	q := url.Query()
	if u := q.Get("u"); u != "" {
		username = u
	}

	// Try to get it from the authorization header if set there
	if username == "" {
		if u, _, ok := r.BasicAuth(); ok {
			username = u
		}
	}
	return username
}

// sanitize redacts passwords from query string for logging.
func sanitize(r *http.Request) {
	values := r.URL.Query()
	for i, q := range values["q"] {
		values["q"][i] = influxql.Sanitize(q)
	}
	r.URL.RawQuery = values.Encode()
}
