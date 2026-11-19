// http: Strata over HTTP request/response. The codec library knows nothing
// about HTTP; this example wires it to net/http with format detection and
// negotiation helpers, then puts a persistent, wildcard-searchable store
// behind it. The default demo uses an in-process transport; -serve starts a
// real listener for curl.
//
// Every request and response can be any of three formats, chosen per request:
//
//	application/x-strata           binary (programs)
//	text/x-strata                  readable text notation (people, curl)
//	application/x-strata+base64    the binary message as one opaque token
//
// Run the self-contained demo:      go run ./examples/http
// Or keep the server up for curl:   go run ./examples/http -serve :8080
//
//	curl -X PUT localhost:8080/users/ada -d 'Name Ada; Age 36; Tags admin dev'
//	curl localhost:8080/users/ada                          # answers in text
//	curl -H 'Accept: application/x-strata' localhost:8080/users/ada | xxd
//	curl -g 'localhost:8080/users?match=user:[ab]*'          # wildcard search (-g: no curl globbing)
//	curl 'localhost:8080/users/ada?format=base64'          # one opaque token
//	curl -X DELETE localhost:8080/users/ada
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"

	"github.com/oarkflow/codec"
	"github.com/oarkflow/codec/store"
)

type User struct {
	Name  string
	Age   uint32
	Tags  []string
	Email string
}

type Page struct {
	Count int
	Users []User
}

type Error struct {
	Message string `codec:"error"`
}

// inProcessTransport runs the demo client against the same handler without
// requiring a local TCP listener. The -serve mode below exercises real HTTP.
type inProcessTransport struct{ handler http.Handler }

func (t inProcessTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	w := httptest.NewRecorder()
	t.handler.ServeHTTP(w, r)
	return w.Result(), nil
}

// ---- the whole HTTP binding: negotiate, decode, encode ----

// requestFormat is the format the client sent. Anything that is not an
// explicit Strata media type (no header, curl's default form type, text/plain)
// counts as text, so people at a terminal get readable answers by default.
func requestFormat(r *http.Request) codec.Format {
	if f, ok := codec.FormatForMedia(r.Header.Get("Content-Type")); ok {
		return f
	}
	return codec.FormatText
}

// responseFormat: ?format=, then Accept, then the request's own format.
func responseFormat(r *http.Request) codec.Format {
	if f, ok := codec.ParseFormat(r.URL.Query().Get("format")); ok {
		return f
	}
	return codec.Negotiate(r.Header.Get("Accept"), requestFormat(r))
}

// decode reads the request body (any format) into v; ok is false if the
// request had no payload.
func decode(r *http.Request, v any) (ok bool, err error) {
	body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, 4<<20))
	if err != nil {
		return false, err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return false, nil
	}
	_, err = codec.DecodeAs(r.Header.Get("Content-Type"), body, v)
	return err == nil, err
}

func respond(w http.ResponseWriter, r *http.Request, status int, v any) {
	f := responseFormat(r)
	body, err := codec.EncodeAs(nil, f, v)
	if err != nil {
		status, f = http.StatusInternalServerError, codec.FormatText
		body, _ = codec.EncodeAs(nil, f, &Error{err.Error()})
	}
	if f != codec.FormatBinary {
		body = append(body, '\n') // friendly in a terminal
	}
	w.Header().Set("Content-Type", f.MediaType())
	w.WriteHeader(status)
	w.Write(body)
}

func fail(w http.ResponseWriter, r *http.Request, status int, err error) {
	respond(w, r, status, &Error{err.Error()})
}

// ---- the API ----

func newServer(db *store.DB[User]) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("PUT /users/{id}", func(w http.ResponseWriter, r *http.Request) {
		var u User
		if ok, err := decode(r, &u); err != nil || !ok {
			if err == nil {
				err = fmt.Errorf("send a user as the request body")
			}
			fail(w, r, http.StatusBadRequest, err)
			return
		}
		if err := db.Put("user:"+r.PathValue("id"), &u); err != nil {
			fail(w, r, http.StatusInternalServerError, err)
			return
		}
		respond(w, r, http.StatusOK, &u)
	})

	mux.HandleFunc("GET /users/{id}", func(w http.ResponseWriter, r *http.Request) {
		var u User
		switch err := db.Get("user:"+r.PathValue("id"), &u); err {
		case nil:
			respond(w, r, http.StatusOK, &u)
		case store.ErrNotFound:
			fail(w, r, http.StatusNotFound, err)
		default:
			fail(w, r, http.StatusInternalServerError, err)
		}
	})

	mux.HandleFunc("DELETE /users/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := db.Delete("user:" + r.PathValue("id")); err == store.ErrNotFound {
			fail(w, r, http.StatusNotFound, err)
		} else if err != nil {
			fail(w, r, http.StatusInternalServerError, err)
		} else {
			w.WriteHeader(http.StatusNoContent)
		}
	})

	// GET /users?match=user:a*   wildcard search over keys (default: all users)
	mux.HandleFunc("GET /users", func(w http.ResponseWriter, r *http.Request) {
		pattern := r.URL.Query().Get("match")
		if pattern == "" {
			pattern = "user:*"
		}
		var page Page
		err := db.Scan(pattern, func(_ string, u *User) bool {
			page.Users = append(page.Users, User{u.Name, u.Age, append([]string(nil), u.Tags...), u.Email})
			return true
		})
		if err != nil {
			fail(w, r, http.StatusBadRequest, err)
			return
		}
		page.Count = len(page.Users)
		respond(w, r, http.StatusOK, &page)
	})
	return mux
}

// ---- demo client: a few lines of net/http with codec.EncodeAs/DecodeAs ----

func call[Resp any](method, url string, f codec.Format, req any, resp *Resp) (int, error) {
	var body io.Reader
	if req != nil {
		b, err := codec.EncodeAs(nil, f, req)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(b)
	}
	hr, err := http.NewRequest(method, url, body)
	if err != nil {
		return 0, err
	}
	if req != nil {
		hr.Header.Set("Content-Type", f.MediaType())
	}
	hr.Header.Set("Accept", f.MediaType())
	res, err := http.DefaultClient.Do(hr)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		return res.StatusCode, err
	}
	if len(data) > 0 && resp != nil && res.StatusCode < 300 {
		_, err = codec.DecodeAs(res.Header.Get("Content-Type"), data, resp)
	}
	return res.StatusCode, err
}

// raw sends text exactly as a person would with curl -d.
func raw(method, url, body, accept string) (int, string) {
	hr, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		return 0, err.Error()
	}
	if accept != "" {
		hr.Header.Set("Accept", accept)
	}
	res, err := http.DefaultClient.Do(hr) // no Content-Type: the server sniffs it
	if err != nil {
		return 0, err.Error()
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		return res.StatusCode, err.Error()
	}
	return res.StatusCode, strings.TrimSpace(string(data))
}

func main() {
	serve := flag.String("serve", "", "listen address (e.g. :8080) to keep the server running for curl")
	flag.Parse()

	dir, err := os.MkdirTemp("", "strata-http")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)
	db, err := store.Open[User](dir, nil)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	srv := newServer(db)

	if *serve != "" {
		log.Printf("serving on %s (data in %s)", *serve, dir)
		log.Fatal(http.ListenAndServe(*serve, srv))
	}

	http.DefaultClient = &http.Client{Transport: inProcessTransport{handler: srv}}
	base := "http://example.local"

	// 1. a program sends and receives the compact binary form
	var u User
	st, err := call("PUT", base+"/users/ada", codec.FormatBinary, &User{Name: "Ada", Age: 36, Tags: []string{"admin", "dev"}, Email: "ada@example.com"}, &u)
	fmt.Printf("PUT  binary  -> %d %+v %v\n", st, u, err)

	// 2. a person reads the same record as text, and writes one by hand
	st, out := raw("GET", base+"/users/ada", "", "")
	fmt.Printf("GET  (curl)  -> %d %s\n", st, out)
	st, out = raw("PUT", base+"/users/grace", "Name Grace\nAge 45\nTags navy\n", "")
	fmt.Printf("PUT  text    -> %d %s\n", st, out)

	// 3. the same record as an opaque base64 token, and as binary again
	st, out = raw("GET", base+"/users/grace?format=base64", "", "")
	fmt.Printf("GET  base64  -> %d %s\n", st, out)
	var g User
	st, err = call("GET", base+"/users/grace", codec.FormatBinary, nil, &g)
	fmt.Printf("GET  binary  -> %d %+v %v\n", st, g, err)

	// 4. wildcard search over the persistent store
	st, out = raw("GET", base+"/users?match=user:[ag]*", "", "")
	fmt.Printf("GET  search  -> %d %s\n", st, out)

	// 5. errors come back in the negotiated format
	st, out = raw("PUT", base+"/users/bad", "Nmae typo", "")
	fmt.Printf("PUT  typo    -> %d %s\n", st, out)
	st, out = raw("DELETE", base+"/users/ada", "", "")
	fmt.Printf("DELETE       -> %d %q\n", st, out)
	st, out = raw("GET", base+"/users/ada", "", "")
	fmt.Printf("GET  deleted -> %d %s\n", st, out)
}
