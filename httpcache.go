// Package httpcache provides a Doer wrapper implementation that works as a
// mostly RFC-compliant cached client for http responses.
//
// It is only suitable for use as a 'private' cache (i.e. for a web-browser or an API-client
// and not for a shared proxy).
//
package httpcache

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httputil"
)

const (
	// XFromCache is the header added to responses that are returned from the cache
	XFromCache = "X-From-Cache"
)

// A Cache interface is used by the CachedClient to store and retrieve responses.
type Cache interface {
	// Get returns the []byte representation of a cached response and a bool
	// set to true if the value isn't empty
	Get(key string) (responseBytes []byte, ok bool)
	// Set stores the []byte representation of a response for a given key, with a TTL for supporting implementations
	Set(key string, responseBytes []byte, ttl int)
	// Delete removes the value associated with the key
	Delete(key string)
}

// A Doer interface abstracts the *http.Client Do method from the client implementation
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// cacheKey returns the cache key for req.
func cacheKey(req *http.Request) string {
	if req.Method == http.MethodGet {
		return req.URL.String()
	} else {
		return req.Method + " " + req.URL.String()
	}
}

type CacheOptions struct {
	TTL int
	// If true, responses returned from the cache will be given an extra header, X-From-Cache
	MarkCachedResponses bool
	Debug               bool
}

type ClientOptions struct {
}

// CachedClient is an implementation of Doer that will return values from a cache
// where possible (avoiding a network request) and will additionally add validators (etag/if-modified-since)
// to repeated requests allowing servers to return 304 / Not Modified
type CachedClient struct {
	Transport http.RoundTripper
	Cache     Cache
	Options   CacheOptions
}

// NewCachedClient returns a new Transport with the
// provided Cache implementation and MarkCachedResponses set to true
func NewCachedClient(client *http.Client, c Cache, options CacheOptions) Doer {
	return &CachedClient{Cache: c, Transport: client.Transport, Options: options}
}

func NewCachedClientRoundTripper(client *http.Client, c Cache, options CacheOptions) http.RoundTripper {
	return &CachedClient{Cache: c, Transport: client.Transport, Options: options}
}

// NewMemoryCachedClient returns a new Doer using a locking in-memory map cache implementation
// It is not optimized for real workloads so it should be used for testing only
func NewMapCachedClient(client *http.Client) Doer {
	c := NewMemoryCache()
	cc := NewCachedClient(client, c, CacheOptions{})
	return cc
}

// CachedResponse returns the cached http.Response for req if present, and nil
// otherwise.
func CachedResponse(c Cache, req *http.Request) (resp *http.Response, err error) {
	cachedVal, ok := c.Get(cacheKey(req))
	if !ok {
		return
	}

	b := bytes.NewBuffer(cachedVal)
	return http.ReadResponse(bufio.NewReader(b), req)
}

func (cc *CachedClient) log(message string) {
	if cc.Options.Debug {
		println(message)
	}
}

// Do takes a Request and returns a Response, error pair, following the *http.Client Do method
//
// If there is a fresh Response already in cache, then it will be returned without connecting to
// the server.
//
// If there is a stale Response, then any validators it contains will be set on the new request
// to give the server a chance to respond with NotModified. If this happens, then the cached Response
// will be returned.
func (cc *CachedClient) Do(req *http.Request) (resp *http.Response, err error) {
	return executeRequest(cc, req, resp)
}

func (cc *CachedClient) RoundTrip(req *http.Request) (resp *http.Response, err error) {
	return executeRequest(cc, req, resp)
}

func executeRequest(cc *CachedClient, req *http.Request, resp *http.Response) (*http.Response, error) {
	var err error
	cacheKey := cacheKey(req)
	cacheable := (req.Method == "GET" || req.Method == "HEAD") && req.Header.Get("range") == ""
	var cachedResp *http.Response

	// Cached response retrieval
	if cacheable {
		cachedResp, err = CachedResponse(cc.Cache, req)
		cc.log(fmt.Sprintf("\n[httpcache](%p) cached get key %v: (err:%v, nil:%v)",
			req,
			cacheKey,
			err,
			cachedResp == nil))
	} else {
		// Need to invalidate an existing value
		cc.log(fmt.Sprintf("\n[httpcache](%p) evicting entry (reason: cacheable == false) for key %v", req, cacheKey))
		cc.Cache.Delete(cacheKey)
	}

	// Response/request validation and remote request
	if cacheable && cachedResp != nil && err == nil {
		if cc.Options.MarkCachedResponses {
			cachedResp.Header.Set(XFromCache, "1")
		}

		if varyMatches(cachedResp, req) {
			// Can only use cached value if the new request doesn't Vary significantly
			freshness := cc.getFreshness(req, cachedResp.Header)
			cc.log(fmt.Sprintf("[httpcache](%p) varyMatches: true, freshness: %s, processing result", req, freshness))

			if freshness == fresh {
				return cachedResp, nil
			}

			if freshness == stale {
				var req2 *http.Request
				// Add validators if caller hasn't already done so
				etag := cachedResp.Header.Get("etag")
				if etag != "" && req.Header.Get("etag") == "" {
					req2 = cloneRequest(req)
					cc.log(fmt.Sprintf("[httpcache](%p) setting request if-none-match to %s from cached etag", req, etag))
					req2.Header.Set("if-none-match", etag)
				}
				lastModified := cachedResp.Header.Get("last-modified")
				if lastModified != "" && req.Header.Get("last-modified") == "" {
					if req2 == nil {
						req2 = cloneRequest(req)
					}
					cc.log(fmt.Sprintf("[httpcache](%p) setting request if-modified-since to %s from cached last-modified", req, lastModified))
					req2.Header.Set("if-modified-since", lastModified)
				}
				if req2 != nil {
					cc.log(fmt.Sprintf("[httpcache](%p) overriding request with updated validator headers", req))
					req = req2
				}
			}
		}

		cc.log(fmt.Sprintf("[httpcache](%p) cache miss or stale entry. executing remote request", req))
		resp, err = cc.Transport.RoundTrip(req)
		if err == nil && req.Method == "GET" && resp.StatusCode == http.StatusNotModified {
			// Replace the 304 response with the one from cache, but update with some new headers
			endToEndHeaders := getEndToEndHeaders(resp.Header)
			for _, header := range endToEndHeaders {
				cachedResp.Header[header] = resp.Header[header]
			}
			resp.Body.Close()
			resp = cachedResp
			cc.log(fmt.Sprintf("[httpcache](%p) 304 server response obtained. using local cache response", req))
		} else if (err != nil || (cachedResp != nil && resp.StatusCode >= 500)) &&
			req.Method == "GET" && canStaleOnError(cachedResp.Header, req.Header) {
			// In case of transport failure and stale-if-error activated, returns cached content
			// when available
			if resp != nil && resp.Body != nil {
				resp.Body.Close()
			}
			cc.log(fmt.Sprintf("[httpcache](%p) transport/upstream error with stale-if-error. using local cache response", req))
			return cachedResp, nil
		} else {
			if err != nil || resp.StatusCode != http.StatusOK {
				cc.log(fmt.Sprintf("[httpcache](%p) evicting entry (reason: request/upstream error) for key %v", req, cacheKey))
				cc.Cache.Delete(cacheKey)
			}
			if err != nil {
				cc.log(fmt.Sprintf("[httpcache](%p) transport/upstream error. returning nil response (%s)", req, err.Error()))
				return nil, err
			}
		}
	} else {
		reqCacheControl := parseCacheControl(req.Header)
		if _, ok := reqCacheControl["only-if-cached"]; ok {
			cc.log(fmt.Sprintf("[httpcache](%p) non-cacheable or entry error detected with only-if-cached request. returning timeout", req))
			resp, err = newGatewayTimeoutResponse(req)
			if err != nil {
				return nil, err
			}
		} else {
			cc.log(fmt.Sprintf("[httpcache](%p) non-cacheable or entry error detected. executing remote request", req))
			resp, err = cc.Transport.RoundTrip(req)
			if err != nil {
				return nil, err
			}
		}
	}

	// Prepare and store response if applicable
	if cacheable && canStore(parseCacheControl(req.Header), parseCacheControl(resp.Header)) {
		for _, varyKey := range headerAllCommaSepValues(resp.Header, "vary") {
			varyKey = http.CanonicalHeaderKey(varyKey)
			fakeHeader := "X-Varied-" + varyKey
			reqValue := req.Header.Get(varyKey)
			if reqValue != "" {
				resp.Header.Set(fakeHeader, reqValue)
			}
		}
		switch req.Method {
		case "GET":
			// Delay caching until EOF is reached.
			resp.Body = &cachingReadCloser{
				R: resp.Body,
				OnEOF: func(r io.Reader) {
					resp := *resp
					resp.Body = ioutil.NopCloser(r)
					respBytes, err := httputil.DumpResponse(&resp, true)
					if err == nil {
						cc.log(fmt.Sprintf("[httpcache](%p) insert entry (source: cachingReadCloser.OnEOF) for key %v", req, cacheKey))
						cc.Cache.Set(cacheKey, respBytes, cc.Options.TTL)
					}
				},
			}
		default:
			respBytes, err := httputil.DumpResponse(resp, true)
			if err == nil {
				cc.log(fmt.Sprintf("[httpcache](%p) insert entry (source: DumpResponse) for key %v", req, cacheKey))
				cc.Cache.Set(cacheKey, respBytes, cc.Options.TTL)
			}
		}
	} else {
		cc.log(fmt.Sprintf("[httpcache](%p) evicting entry (reason: (cacheable && canStore) == false) for key %v", req, cacheKey))
		cc.Cache.Delete(cacheKey)
	}

	return resp, nil
}

// varyMatches will return false unless all of the cached values for the headers listed in Vary
// match the new request
func varyMatches(cachedResp *http.Response, req *http.Request) bool {
	for _, header := range headerAllCommaSepValues(cachedResp.Header, "vary") {
		header = http.CanonicalHeaderKey(header)
		if header != "" && req.Header.Get(header) != cachedResp.Header.Get("X-Varied-"+header) {
			return false
		}
	}
	return true
}

func newGatewayTimeoutResponse(req *http.Request) (*http.Response, error) {
	var buf bytes.Buffer
	buf.WriteString("HTTP/1.1 504 Gateway Timeout\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(&buf), req)
	if err != nil {
		return nil, errors.New("httpcache: could not write timeout response")
	}
	return resp, nil
}

// cloneRequest returns a clone of the provided *http.Request.
// The clone is a shallow copy of the struct and its Header map.
// (This function copyright goauth2 authors: https://code.google.com/p/goauth2)
func cloneRequest(r *http.Request) *http.Request {
	// shallow copy of the struct
	r2 := new(http.Request)
	*r2 = *r
	// deep copy of the Header
	r2.Header = make(http.Header)
	for k, s := range r.Header {
		r2.Header[k] = s
	}
	return r2
}
